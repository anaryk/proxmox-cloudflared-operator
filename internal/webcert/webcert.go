// Package webcert makes and checks the certificate the web interface serves:
// a leaf for the node's names signed by the cluster CA, a self-signed one for
// the appliance, or the admin's own.
//
// The key of a leaf is its own, never pveproxy's: whoever holds pco-web holds
// that key and nothing more, and the leaf cannot sign another certificate.
package webcert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"
)

// The modes of the certificate, as setup and its manifest name them.
const (
	ModeCA       = "ca"       // a leaf signed by the cluster CA, renewed by the daemon
	ModeOwn      = "own"      // the admin's certificate and key, copied in
	ModePVEProxy = "pveproxy" // links to the certificate and key pveproxy serves
)

// Lifetime of the cluster-CA leaf: Proxmox's CA has no revocation, so a short
// life bounds a stolen leaf; renewed 30 days before the end.
const Lifetime = 90 * 24 * time.Hour

// SelfSignedLifetime of the appliance's certificate, which is trusted by its
// fingerprint and signs nothing for the node's names.
const SelfSignedLifetime = 397 * 24 * time.Hour

// RenewBefore is how long before its end a leaf is made anew.
const RenewBefore = 30 * 24 * time.Hour

// backdate lets a browser whose clock is a little behind the node's take a
// leaf made a moment ago.
const backdate = time.Hour

// Names are what a leaf is for: DNS names and addresses. The listen address
// of PCO_WEB_LISTEN is always among Addrs, so that the address is as good as
// a name in the browser.
type Names struct {
	DNS   []string
	Addrs []netip.Addr
}

func (n Names) String() string {
	all := slices.Clone(n.DNS)
	for _, a := range n.Addrs {
		all = append(all, a.String())
	}
	return strings.Join(all, ", ")
}

// NodeNames are the names of a node's leaf: its short name, its FQDN, the
// addresses cluster status lists for it, the address of the listen address
// and the names and addresses of PCO_WEB_HOSTS, each once, in that order. An
// unspecified listen address, such as 0.0.0.0, names nothing.
func NodeNames(node, fqdn string, addrs []netip.Addr, listen string, hosts []string) Names {
	var n Names
	add := func(name string) {
		name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
		if name == "" {
			return
		}
		if a, err := netip.ParseAddr(name); err == nil {
			n.addAddr(a)
			return
		}
		if !slices.Contains(n.DNS, name) {
			n.DNS = append(n.DNS, name)
		}
	}
	add(node)
	add(fqdn)
	for _, a := range addrs {
		n.addAddr(a)
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}
	add(host)
	for _, h := range hosts {
		add(h)
	}
	return n
}

// FQDN returns the fully qualified name of a host name as /etc/hosts has it,
// where Proxmox VE writes it and hostname --fqdn finds it; empty when it has
// none. No resolver is asked: the names are those pco web takes as its own,
// which reads them the same way.
func FQDN(hostname string, etcHosts []byte) string {
	short, _, dotted := strings.Cut(hostname, ".")
	if dotted {
		return hostname
	}
	for line := range strings.Lines(string(etcHosts)) {
		line, _, _ = strings.Cut(line, "#")
		fields := strings.Fields(line)
		if len(fields) < 2 || !slices.Contains(fields[1:], short) {
			continue
		}
		for _, name := range fields[1:] {
			if strings.HasPrefix(name, short+".") {
				return name
			}
		}
	}
	return ""
}

// NodeFQDN is the FQDN of this host, from its host name and /etc/hosts.
func NodeFQDN() string {
	hostname, err := os.Hostname()
	if err != nil {
		return ""
	}
	etcHosts, _ := os.ReadFile("/etc/hosts")
	return FQDN(hostname, etcHosts)
}

func (n *Names) addAddr(a netip.Addr) {
	a = a.Unmap()
	if a.IsValid() && !a.IsUnspecified() && !slices.Contains(n.Addrs, a) {
		n.Addrs = append(n.Addrs, a)
	}
}

// Issue makes a new ECDSA P-256 key and a leaf for names signed by ca, valid
// lifetime from now, with serverAuth only, BasicConstraints IsCA false, the
// key usage DigitalSignature only (no CertSign), and a random 128-bit serial:
// the leaf cannot sign another certificate.
func Issue(ca *x509.Certificate, caKey crypto.Signer, names Names, now time.Time, lifetime time.Duration, rand io.Reader) (certPEM, keyPEM []byte, err error) {
	if !Matches(ca, caKey) {
		return nil, nil, errors.New("the key of the cluster CA is not the key of its certificate")
	}
	return makeLeaf(names, now, lifetime, rand, ca, caKey)
}

// SelfSigned makes a key and a self-signed leaf, which the appliance serves
// and browsers trust by its fingerprint.
func SelfSigned(names Names, now time.Time, lifetime time.Duration, rand io.Reader) (certPEM, keyPEM []byte, err error) {
	return makeLeaf(names, now, lifetime, rand, nil, nil)
}

// makeLeaf makes a key and a leaf for it, signed by parent with parentKey, or
// by its own key when parent is nil.
func makeLeaf(names Names, now time.Time, lifetime time.Duration, rand io.Reader,
	parent *x509.Certificate, parentKey crypto.Signer,
) (certPEM, keyPEM []byte, err error) {
	if len(names.DNS) == 0 && len(names.Addrs) == 0 {
		return nil, nil, errors.New("no names to make a certificate for")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand)
	if err != nil {
		return nil, nil, fmt.Errorf("making a key: %w", err)
	}
	serial, err := randomSerial(rand)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName(names)},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(lifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              slices.Clone(names.DNS),
	}
	for _, a := range names.Addrs {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.IP(a.AsSlice()))
	}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand, tmpl, parent, key.Public(), parentKey)
	if err != nil {
		return nil, nil, fmt.Errorf("signing the certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding the key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// randomSerial draws a serial of 128 random bits, never zero.
func randomSerial(rand io.Reader) (*big.Int, error) {
	b := make([]byte, 16)
	for {
		if _, err := io.ReadFull(rand, b); err != nil {
			return nil, fmt.Errorf("reading random bytes: %w", err)
		}
		if s := new(big.Int).SetBytes(b); s.Sign() > 0 {
			return s, nil
		}
	}
}

func commonName(n Names) string {
	if len(n.DNS) > 0 {
		return n.DNS[0]
	}
	return n.Addrs[0].String()
}

// Due says whether the leaf must be issued again, and why: fewer than 30 days
// left, another CA, or names it does not cover (the listen address among them).
func Due(leaf, ca *x509.Certificate, names Names, now time.Time) (bool, string) {
	if leaf.NotAfter.Sub(now) < RenewBefore {
		return true, "it expires at " + leaf.NotAfter.UTC().Format(time.RFC3339) + ", in less than 30 days"
	}
	if ca == nil || leaf.CheckSignatureFrom(ca) != nil {
		return true, "it is not signed by the cluster CA"
	}
	for _, name := range names.DNS {
		if !slices.ContainsFunc(leaf.DNSNames, func(have string) bool { return strings.EqualFold(have, name) }) {
			return true, "it does not name " + name
		}
	}
	for _, a := range names.Addrs {
		if !slices.ContainsFunc(leaf.IPAddresses, func(have net.IP) bool { return have.Equal(net.IP(a.AsSlice())) }) {
			return true, "it does not name " + a.String()
		}
	}
	return false, ""
}

// Check validates a certificate and key of the admin's: both parse, the key is
// the certificate's, it is valid at now and covers one of names.
func Check(certPEM, keyPEM []byte, names Names, now time.Time) error {
	leaf, err := ParseCert(certPEM)
	if err != nil {
		return fmt.Errorf("reading the certificate: %w", err)
	}
	key, err := ParseKey(keyPEM)
	if err != nil {
		return fmt.Errorf("reading the key: %w", err)
	}
	switch {
	case !Matches(leaf, key):
		return errors.New("the key is not the key of the certificate")
	case now.Before(leaf.NotBefore):
		return fmt.Errorf("the certificate is valid only from %s", leaf.NotBefore.UTC().Format(time.RFC3339))
	case now.After(leaf.NotAfter):
		return fmt.Errorf("the certificate expired at %s", leaf.NotAfter.UTC().Format(time.RFC3339))
	case !covers(leaf, names):
		return fmt.Errorf("the certificate names none of %s", names)
	}
	return nil
}

// covers reports whether a browser takes leaf for one of names.
func covers(leaf *x509.Certificate, names Names) bool {
	for _, name := range names.DNS {
		if leaf.VerifyHostname(name) == nil {
			return true
		}
	}
	for _, a := range names.Addrs {
		if leaf.VerifyHostname(a.String()) == nil {
			return true
		}
	}
	return false
}

// ParseCert returns the first certificate of PEM data, which is the leaf when
// a chain follows it.
func ParseCert(b []byte) (*x509.Certificate, error) {
	for rest := b; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		switch {
		case block == nil:
			return nil, errors.New("no certificate in the PEM data")
		case block.Type == "CERTIFICATE":
			return x509.ParseCertificate(block.Bytes)
		}
	}
}

// ParseKey returns the first private key of PEM data, in PKCS #8, PKCS #1 or
// SEC 1, as openssl writes them. An encrypted key is refused.
func ParseKey(b []byte) (crypto.Signer, error) {
	for rest := b; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("no private key in the PEM data")
		}
		var key any
		var err error
		switch block.Type {
		case "PRIVATE KEY":
			key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		case "RSA PRIVATE KEY":
			key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		case "EC PRIVATE KEY":
			key, err = x509.ParseECPrivateKey(block.Bytes)
		case "ENCRYPTED PRIVATE KEY":
			return nil, errors.New("the private key is encrypted")
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		signer, ok := key.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("a private key of type %T cannot sign", key)
		}
		return signer, nil
	}
}

// Matches reports whether key is the key of cert.
func Matches(cert *x509.Certificate, key crypto.Signer) bool {
	pub, ok := cert.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	return ok && pub.Equal(key.Public())
}

// Fingerprint is the SHA-256 of the leaf, as "AB:CD:...".
func Fingerprint(leaf *x509.Certificate) string {
	sum := sha256.Sum256(leaf.Raw)
	pairs := make([]string, len(sum))
	for i, b := range sum {
		pairs[i] = strings.ToUpper(hex.EncodeToString([]byte{b}))
	}
	return strings.Join(pairs, ":")
}
