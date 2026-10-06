package pki

import (
	"crypto/ecdsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// NewCSR makes the certificate request of the holder of key for id, which
// IssueClient takes.
func NewCSR(key *ecdsa.PrivateKey, id Identity, rand io.Reader) ([]byte, error) {
	if err := id.check(); err != nil {
		return nil, err
	}
	der, err := x509.CreateCertificateRequest(rand, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: id.String()},
		URIs:    []*url.URL{id.URI()},
	}, key)
	if err != nil {
		return nil, fmt.Errorf("making the certificate request: %w", err)
	}
	return der, nil
}

// IssueClient issues the client certificate of want for the key of a
// certificate request, which must be signed by that key and name want and
// nothing else. The certificate carries want and only what the CA puts in it,
// whatever else the request asks for.
func (ca CA) IssueClient(csrDER []byte, want Identity, now time.Time, rand io.Reader) (*x509.Certificate, error) {
	if err := want.check(); err != nil {
		return nil, err
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("reading the certificate request: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("the certificate request is not signed by its key: %w", err)
	}
	pub, err := p256(csr.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("the certificate request: %w", err)
	}
	got, err := identityOfURIs("the certificate request", csr.URIs)
	if err != nil {
		return nil, err
	}
	if got != want {
		return nil, fmt.Errorf("the certificate request is for %s, not %s", got, want)
	}
	tmpl, err := ca.leaf(now, rand)
	if err != nil {
		return nil, err
	}
	tmpl.Subject = pkix.Name{CommonName: want.String()}
	tmpl.URIs = []*url.URL{want.URI()}
	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	return ca.sign(tmpl, pub, rand)
}

// IssueServer issues a server certificate for pub, naming names and addrs.
func (ca CA) IssueServer(pub *ecdsa.PublicKey, names []string, addrs []netip.Addr, now time.Time, rand io.Reader) (*x509.Certificate, error) {
	if pub == nil {
		return nil, errors.New("no public key to issue a server certificate for")
	}
	if _, err := p256(pub); err != nil {
		return nil, err
	}
	if len(names) == 0 && len(addrs) == 0 {
		return nil, errors.New("no names to issue a server certificate for")
	}
	tmpl, err := ca.leaf(now, rand)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if n == "" {
			return nil, errors.New("an empty name in a server certificate")
		}
		tmpl.DNSNames = append(tmpl.DNSNames, n)
	}
	for _, a := range addrs {
		if !a.IsValid() {
			return nil, errors.New("an invalid address in a server certificate")
		}
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.IP(a.Unmap().AsSlice()))
	}
	if len(names) > 0 {
		tmpl.Subject = pkix.Name{CommonName: names[0]}
	} else {
		tmpl.Subject = pkix.Name{CommonName: addrs[0].Unmap().String()}
	}
	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	return ca.sign(tmpl, pub, rand)
}

// ControllerServerName is the name an appliance agent verifies the
// controller's certificate under, whichever address it reaches it at.
func ControllerServerName(installID string) string {
	return "controller.pco-" + installID + ".invalid"
}

// NodeServerName is the name a host agent verifies the certificate of the
// node holding the lease under.
func NodeServerName(installID, node string) string {
	return "node-" + strings.ToLower(node) + ".pco-" + installID + ".invalid"
}

// leaf is the template of a leaf issued at now: a new serial, valid from
// Backdate before now for LeafValidity, or until the CA ends if that is
// sooner, signing nothing.
func (ca CA) leaf(now time.Time, rand io.Reader) (*x509.Certificate, error) {
	if ca.Cert == nil || ca.Key == nil {
		return nil, errors.New("no CA to issue a certificate with")
	}
	if !now.Before(ca.Cert.NotAfter) {
		return nil, fmt.Errorf("the CA expired at %s", ca.Cert.NotAfter.UTC().Format(time.RFC3339))
	}
	serial, err := randomSerial(rand)
	if err != nil {
		return nil, err
	}
	notAfter := now.Add(LeafValidity)
	if notAfter.After(ca.Cert.NotAfter) {
		notAfter = ca.Cert.NotAfter
	}
	return &x509.Certificate{
		SerialNumber:          serial,
		NotBefore:             now.Add(-Backdate),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}, nil
}

func (ca CA) sign(tmpl *x509.Certificate, pub *ecdsa.PublicKey, rand io.Reader) (*x509.Certificate, error) {
	der, err := x509.CreateCertificate(rand, tmpl, ca.Cert, pub, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("signing the certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("reading the certificate: %w", err)
	}
	return cert, nil
}
