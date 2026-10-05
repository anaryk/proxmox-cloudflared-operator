package webcert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io/fs"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// testCA is a cluster CA as Proxmox VE makes one: a root that may sign.
type testCA struct {
	cert *x509.Certificate
	key  crypto.Signer
}

func newTestCA(t *testing.T, name string) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return selfSignCA(t, name, key)
}

func selfSignCA(t *testing.T, name string, key crypto.Signer) testCA {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             t0.AddDate(-1, 0, 0),
		NotAfter:              t0.AddDate(9, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return testCA{cert: cert, key: key}
}

func (ca testCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

var testNames = Names{
	DNS:   []string{"pve1", "pve1.example.lan"},
	Addrs: []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::10")},
}

func issue(t *testing.T, ca testCA, names Names, now time.Time) (*x509.Certificate, []byte, []byte) {
	t.Helper()
	certPEM, keyPEM, err := Issue(ca.cert, ca.key, names, now, Lifetime, rand.Reader)
	require.NoError(t, err)
	leaf, err := ParseCert(certPEM)
	require.NoError(t, err)
	return leaf, certPEM, keyPEM
}

func TestIssueMakesALeafForTheNamesSignedByTheCA(t *testing.T) {
	ca := newTestCA(t, "Proxmox Virtual Environment")
	leaf, certPEM, keyPEM := issue(t, ca, testNames, t0)

	for _, name := range []string{"pve1", "pve1.example.lan", "192.0.2.10", "2001:db8::10"} {
		_, err := leaf.Verify(x509.VerifyOptions{
			DNSName: name, Roots: ca.pool(), CurrentTime: t0, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		require.NoError(t, err, name)
	}
	require.Equal(t, []string{"pve1", "pve1.example.lan"}, leaf.DNSNames, "exactly the names")
	require.Len(t, leaf.IPAddresses, 2)
	require.True(t, leaf.IPAddresses[0].Equal(net.ParseIP("192.0.2.10")))
	require.True(t, leaf.IPAddresses[1].Equal(net.ParseIP("2001:db8::10")))
	require.Empty(t, leaf.EmailAddresses)
	require.Empty(t, leaf.URIs)
	require.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, leaf.ExtKeyUsage, "serverAuth only")
	require.Empty(t, leaf.UnknownExtKeyUsage)
	require.Equal(t, t0.Add(90*24*time.Hour), leaf.NotAfter, "90 days")
	require.False(t, leaf.NotBefore.After(t0))
	require.Equal(t, ca.cert.Subject.String(), leaf.Issuer.String())

	key, err := ParseKey(keyPEM)
	require.NoError(t, err)
	ec, ok := key.(*ecdsa.PrivateKey)
	require.True(t, ok, "an ECDSA key")
	require.Equal(t, elliptic.P256(), ec.Curve)
	require.True(t, Matches(leaf, key))
	require.Contains(t, string(certPEM), "-----BEGIN CERTIFICATE-----")
}

// Proxmox's CA has no revocation, so the leaf must not be able to
// make certificates of its own that a browser trusting the CA accepts.
func TestTheLeafCannotSign(t *testing.T) {
	ca := newTestCA(t, "Proxmox Virtual Environment")
	leaf, _, keyPEM := issue(t, ca, testNames, t0)

	require.True(t, leaf.BasicConstraintsValid)
	require.False(t, leaf.IsCA)
	require.Equal(t, x509.KeyUsageDigitalSignature, leaf.KeyUsage, "DigitalSignature only")
	require.Zero(t, leaf.KeyUsage&x509.KeyUsageCertSign, "no CertSign")

	key, err := ParseKey(keyPEM)
	require.NoError(t, err)
	victimKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	victim := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		NotBefore:    t0.Add(-time.Hour),
		NotAfter:     t0.Add(time.Hour),
		DNSNames:     []string{"bank.example.com"},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, victim, leaf, victimKey.Public(), key)
	require.NoError(t, err, "anyone with the key can sign; what counts is that nobody takes it")
	forged, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	inter := x509.NewCertPool()
	inter.AddCert(leaf)
	_, err = forged.Verify(x509.VerifyOptions{DNSName: "bank.example.com", Roots: ca.pool(), Intermediates: inter, CurrentTime: t0})
	require.Error(t, err, "a chain through the leaf fails")
}

func TestIssueGivesEveryLeafAnotherRandomSerial(t *testing.T) {
	ca := newTestCA(t, "ca")
	a, _, _ := issue(t, ca, testNames, t0)
	b, _, _ := issue(t, ca, testNames, t0)

	require.NotEqual(t, a.SerialNumber, b.SerialNumber)
	for _, s := range []*big.Int{a.SerialNumber, b.SerialNumber} {
		require.Positive(t, s.Sign())
		require.LessOrEqual(t, s.BitLen(), 128)
	}
}

// The cluster CA of Proxmox VE is an RSA key, written by openssl as PKCS #1 or
// PKCS #8.
func TestIssueWithAnRSAClusterCA(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	ca := selfSignCA(t, "Proxmox Virtual Environment", rsaKey)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	require.NoError(t, err)
	for name, block := range map[string]*pem.Block{
		"pkcs1": {Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)},
		"pkcs8": {Type: "PRIVATE KEY", Bytes: pkcs8},
	} {
		t.Run(name, func(t *testing.T) {
			key, err := ParseKey(pem.EncodeToMemory(block))
			require.NoError(t, err)

			leaf, _, _ := issue(t, testCA{cert: ca.cert, key: key}, testNames, t0)

			_, err = leaf.Verify(x509.VerifyOptions{DNSName: "pve1", Roots: ca.pool(), CurrentTime: t0})
			require.NoError(t, err)
		})
	}
}

func TestIssueRefusesAKeyThatIsNotTheCAs(t *testing.T) {
	ca := newTestCA(t, "ca")
	other := newTestCA(t, "other")

	_, _, err := Issue(ca.cert, other.key, testNames, t0, Lifetime, rand.Reader)

	require.Error(t, err)
}

func TestIssueRefusesALeafWithoutNames(t *testing.T) {
	ca := newTestCA(t, "ca")

	_, _, err := Issue(ca.cert, ca.key, Names{}, t0, Lifetime, rand.Reader)

	require.EqualError(t, err, "no names to make a certificate for")
}

func TestSelfSigned(t *testing.T) {
	names := Names{DNS: []string{"pco-appliance"}, Addrs: []netip.Addr{netip.MustParseAddr("192.0.2.30")}}
	certPEM, keyPEM, err := SelfSigned(names, t0, SelfSignedLifetime, rand.Reader)
	require.NoError(t, err)
	leaf, err := ParseCert(certPEM)
	require.NoError(t, err)

	require.NoError(t, leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature), "signed by its own key")
	require.Equal(t, leaf.Subject.String(), leaf.Issuer.String())
	require.Equal(t, t0.Add(397*24*time.Hour), leaf.NotAfter)
	require.False(t, leaf.IsCA)
	require.Equal(t, x509.KeyUsageDigitalSignature, leaf.KeyUsage)
	require.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, leaf.ExtKeyUsage)
	require.Equal(t, []string{"pco-appliance"}, leaf.DNSNames)
	require.NoError(t, Check(certPEM, keyPEM, names, t0))
}

func TestDue(t *testing.T) {
	ca := newTestCA(t, "ca")
	other := newTestCA(t, "other")
	names := NodeNames("pve1", "pve1.example.lan", []netip.Addr{netip.MustParseAddr("192.0.2.10")}, "192.0.2.10:8643", nil)
	leaf, _, _ := issue(t, ca, names, t0)
	expires := t0.Add(Lifetime).Format(time.RFC3339)

	for _, tt := range []struct {
		name  string
		ca    *x509.Certificate
		names Names
		now   time.Time
		why   string
	}{
		{name: "fresh", ca: ca.cert, names: names, now: t0},
		{name: "31 days left", ca: ca.cert, names: names, now: t0.Add(Lifetime - 31*24*time.Hour)},
		{name: "29 days left", ca: ca.cert, names: names, now: t0.Add(Lifetime - 29*24*time.Hour),
			why: "it expires at " + expires + ", in less than 30 days"},
		{name: "expired", ca: ca.cert, names: names, now: t0.Add(Lifetime + time.Hour),
			why: "it expires at " + expires + ", in less than 30 days"},
		{name: "another CA", ca: other.cert, names: names, now: t0, why: "it is not signed by the cluster CA"},
		{name: "a new address", ca: ca.cert, now: t0, why: "it does not name 192.0.2.11",
			names: NodeNames("pve1", "pve1.example.lan", []netip.Addr{netip.MustParseAddr("192.0.2.11")}, "192.0.2.10:8643", nil)},
		{name: "a new listen address", ca: ca.cert, now: t0, why: "it does not name 198.51.100.4",
			names: NodeNames("pve1", "pve1.example.lan", []netip.Addr{netip.MustParseAddr("192.0.2.10")}, "198.51.100.4:8643", nil)},
		{name: "a new host name", ca: ca.cert, now: t0, why: "it does not name pve.example.org",
			names: NodeNames("pve1", "pve1.example.lan", []netip.Addr{netip.MustParseAddr("192.0.2.10")}, "192.0.2.10:8643", []string{"pve.example.org"})},
		{name: "a name in capitals", ca: ca.cert, now: t0,
			names: Names{DNS: []string{"PVE1.example.lan"}, Addrs: names.Addrs}},
		{name: "fewer names", ca: ca.cert, now: t0, names: Names{DNS: []string{"pve1"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			due, why := Due(leaf, tt.ca, tt.names, tt.now)

			require.Equal(t, tt.why != "", due)
			require.Equal(t, tt.why, why)
		})
	}
}

func TestCheck(t *testing.T) {
	ca := newTestCA(t, "ca")
	_, certPEM, keyPEM := issue(t, ca, testNames, t0)
	_, _, otherKey := issue(t, ca, testNames, t0)
	theNode := Names{DNS: []string{"pve1.example.lan"}}

	for _, tt := range []struct {
		name      string
		cert, key []byte
		names     Names
		now       time.Time
		err       string
	}{
		{name: "good", cert: certPEM, key: keyPEM, names: theNode, now: t0},
		{name: "by an address", cert: certPEM, key: keyPEM, names: Names{Addrs: []netip.Addr{netip.MustParseAddr("2001:db8::10")}}, now: t0},
		{name: "one of the names is enough", cert: certPEM, key: keyPEM, now: t0,
			names: Names{DNS: []string{"elsewhere.example.org", "pve1"}}},
		{name: "a chain after the leaf", cert: append(slicesClone(certPEM), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})...),
			key: keyPEM, names: theNode, now: t0},
		{name: "a key of another certificate", cert: certPEM, key: otherKey, names: theNode, now: t0,
			err: "the key is not the key of the certificate"},
		{name: "expired", cert: certPEM, key: keyPEM, names: theNode, now: t0.Add(Lifetime + time.Second),
			err: "the certificate expired at " + t0.Add(Lifetime).Format(time.RFC3339)},
		{name: "not yet valid", cert: certPEM, key: keyPEM, names: theNode, now: t0.Add(-48 * time.Hour),
			err: "the certificate is valid only from "},
		{name: "for other names", cert: certPEM, key: keyPEM, now: t0,
			names: Names{DNS: []string{"pve2.example.lan"}, Addrs: []netip.Addr{netip.MustParseAddr("192.0.2.99")}},
			err:   "the certificate names none of pve2.example.lan, 192.0.2.99"},
		{name: "no certificate", cert: []byte("hello"), key: keyPEM, names: theNode, now: t0, err: "no certificate in the PEM data"},
		{name: "no key", cert: certPEM, key: []byte("hello"), names: theNode, now: t0, err: "no private key in the PEM data"},
		{name: "the key in place of the certificate", cert: keyPEM, key: keyPEM, names: theNode, now: t0, err: "no certificate in the PEM data"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := Check(tt.cert, tt.key, tt.names, tt.now)

			if tt.err == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.err)
		})
	}
}

func slicesClone(b []byte) []byte { return append([]byte(nil), b...) }

func TestFingerprint(t *testing.T) {
	ca := newTestCA(t, "ca")
	leaf, _, _ := issue(t, ca, testNames, t0)
	sum := sha256.Sum256(leaf.Raw)

	fp := Fingerprint(leaf)

	require.Len(t, strings.Split(fp, ":"), 32)
	require.Equal(t, strings.ToUpper(hex.EncodeToString(sum[:])), strings.ReplaceAll(fp, ":", ""))
}

func TestNodeNames(t *testing.T) {
	addrs := []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.10")}

	for _, tt := range []struct {
		name         string
		node, fqdn   string
		listen       string
		hosts        []string
		dns          []string
		addrsWant    []string
		clusterAddrs []netip.Addr
	}{
		{name: "the node", node: "pve1", fqdn: "pve1.example.lan", listen: "192.0.2.10:8643", clusterAddrs: addrs,
			dns: []string{"pve1", "pve1.example.lan"}, addrsWant: []string{"192.0.2.10"}},
		{name: "another listen address", node: "pve1", fqdn: "pve1.example.lan", listen: "198.51.100.4:8643", clusterAddrs: addrs,
			dns: []string{"pve1", "pve1.example.lan"}, addrsWant: []string{"192.0.2.10", "198.51.100.4"}},
		{name: "an IPv6 listen address", node: "pve1", listen: "[2001:db8::10]:8643",
			dns: []string{"pve1"}, addrsWant: []string{"2001:db8::10"}},
		{name: "all addresses name nothing", node: "pve1", listen: "0.0.0.0:8643", clusterAddrs: addrs,
			dns: []string{"pve1"}, addrsWant: []string{"192.0.2.10"}},
		{name: "the hosts", node: "PVE1", fqdn: "pve1", listen: "192.0.2.10:8643",
			hosts: []string{"Pve.Example.org", " 203.0.113.9", "", "pve1"},
			dns:   []string{"pve1", "pve.example.org"}, addrsWant: []string{"192.0.2.10", "203.0.113.9"}},
		{name: "a name to listen on", node: "pve1", listen: "localhost:8643", dns: []string{"pve1", "localhost"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := NodeNames(tt.node, tt.fqdn, tt.clusterAddrs, tt.listen, tt.hosts)

			require.Equal(t, tt.dns, n.DNS)
			var got []string
			for _, a := range n.Addrs {
				got = append(got, a.String())
			}
			require.Equal(t, tt.addrsWant, got)
		})
	}
}

func TestFQDN(t *testing.T) {
	hosts := []byte("127.0.0.1 localhost.localdomain localhost\n" +
		"192.0.2.10 pve1.example.lan pve1 # written by Proxmox VE\n" +
		"# 192.0.2.99 pve2.old.lan pve2\n")

	require.Equal(t, "pve1.example.lan", FQDN("pve1", hosts))
	require.Equal(t, "pve1.example.org", FQDN("pve1.example.org", hosts), "a host name with a dot is one")
	require.Empty(t, FQDN("pve2", hosts), "a comment names nothing")
	require.Empty(t, FQDN("pve3", hosts))
}

func TestWriteAtomicWritesThePair(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, WriteAtomic(dir, []byte("cert\n"), []byte("key\n")))

	requireFile(t, filepath.Join(dir, CertName), "cert\n", 0o644)
	requireFile(t, filepath.Join(dir, KeyName), "key\n", 0o600)
	requireOnly(t, dir, CertName, KeyName)
}

// From --web-cert pveproxy to ca: the links go, what they led to stays.
func TestWriteAtomicReplacesLinksAndLeavesTheirTargets(t *testing.T) {
	dir := t.TempDir()
	pve := t.TempDir()
	for _, name := range []string{"pve-ssl.pem", "pve-ssl.key"} {
		require.NoError(t, os.WriteFile(filepath.Join(pve, name), []byte("pveproxy's\n"), 0o600))
	}
	require.NoError(t, os.Symlink(filepath.Join(pve, "pve-ssl.pem"), filepath.Join(dir, CertName)))
	require.NoError(t, os.Symlink(filepath.Join(pve, "pve-ssl.key"), filepath.Join(dir, KeyName)))

	require.NoError(t, WriteAtomic(dir, []byte("cert\n"), []byte("key\n")))

	requireFile(t, filepath.Join(dir, CertName), "cert\n", 0o644)
	requireFile(t, filepath.Join(dir, KeyName), "key\n", 0o600)
	for _, name := range []string{"pve-ssl.pem", "pve-ssl.key"} {
		requireFile(t, filepath.Join(pve, name), "pveproxy's\n", 0o600)
	}
}

func TestAWriteThatFailsLeavesTheOldPair(t *testing.T) {
	for _, tt := range []struct {
		name string
		fail string // the file whose rename fails
	}{
		{"the key", KeyName},
		{"the certificate", CertName},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, WriteAtomic(dir, []byte("old cert\n"), []byte("old key\n")))
			broken := errors.New("disk full")
			rename := func(from, to string) error {
				if filepath.Base(to) == tt.fail {
					return broken
				}
				return os.Rename(from, to)
			}

			err := writeAtomic(dir, []byte("new cert\n"), []byte("new key\n"), rename)

			require.ErrorIs(t, err, broken)
			requireFile(t, filepath.Join(dir, CertName), "old cert\n", 0o644)
			requireFile(t, filepath.Join(dir, KeyName), "old key\n", 0o600)
			requireOnly(t, dir, CertName, KeyName)
		})
	}
}

func TestAFailedWriteOfAFirstPairLeavesNoKey(t *testing.T) {
	dir := t.TempDir()
	rename := func(from, to string) error {
		if filepath.Base(to) == CertName {
			return fs.ErrPermission
		}
		return os.Rename(from, to)
	}

	require.Error(t, writeAtomic(dir, []byte("cert\n"), []byte("key\n"), rename))

	requireOnly(t, dir)
}

func TestLinkAtomic(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, PinName)

	require.NoError(t, LinkAtomic(link, "/etc/pve/local/pve-ssl.pem"))
	require.NoError(t, LinkAtomic(link, "/etc/pve/local/pveproxy-ssl.pem"))

	target, err := os.Readlink(link)
	require.NoError(t, err)
	require.Equal(t, "/etc/pve/local/pveproxy-ssl.pem", target)
	requireOnly(t, dir, PinName)
}

func TestReadEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pco-web")
	require.NoError(t, os.WriteFile(path, []byte("# written by pco setup\nPCO_WEB_LISTEN=192.0.2.10:8643\n"+
		"PCO_WEB_HOSTS=\"pve.example.org, 203.0.113.9\"\nPCO_WEB_HSTS=1\n"), 0o644))

	env, found, err := ReadEnv(path)

	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, Env{Listen: "192.0.2.10:8643", Hosts: []string{"pve.example.org", "203.0.113.9"}}, env)

	_, found, err = ReadEnv(filepath.Join(t.TempDir(), "none"))
	require.NoError(t, err)
	require.False(t, found)
}

func requireFile(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular(), "%s is a file", path)
	require.Equal(t, mode, info.Mode().Perm(), path)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, content, string(b))
}

func requireOnly(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	got := []string{}
	for _, e := range entries {
		got = append(got, e.Name())
	}
	require.ElementsMatch(t, append([]string{}, names...), got)
}
