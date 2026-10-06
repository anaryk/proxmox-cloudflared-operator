package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func issueClient(t *testing.T, ca CA, id Identity, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := NewKey(rand.Reader)
	require.NoError(t, err)
	csr, err := NewCSR(key, id, rand.Reader)
	require.NoError(t, err)
	cert, err := ca.IssueClient(csr, id, now, rand.Reader)
	require.NoError(t, err)
	return cert, key
}

func issueServer(t *testing.T, ca CA, name string, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := NewKey(rand.Reader)
	require.NoError(t, err)
	cert, err := ca.IssueServer(&key.PublicKey, []string{name}, nil, now, rand.Reader)
	require.NoError(t, err)
	return cert, key
}

func poolOf(cas ...CA) *x509.CertPool {
	p := x509.NewCertPool()
	for _, ca := range cas {
		p.AddCert(ca.Cert)
	}
	return p
}

func TestNewKey(t *testing.T) {
	key, err := NewKey(rand.Reader)
	require.NoError(t, err)
	require.Equal(t, elliptic.P256(), key.Curve)
}

func TestIssueClient(t *testing.T) {
	ca := newCA(t, t0)
	for _, id := range []Identity{hostID("node1"), applianceID("node2", 9100)} {
		t.Run(string(id.Kind), func(t *testing.T) {
			cert, key := issueClient(t, ca, id, t0)

			_, err := cert.Verify(x509.VerifyOptions{
				Roots: poolOf(ca), CurrentTime: t0, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			})
			require.NoError(t, err)
			_, err = cert.Verify(x509.VerifyOptions{
				Roots: poolOf(ca), CurrentTime: t0, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			require.Error(t, err, "a client certificate does not serve")
			_, err = cert.Verify(x509.VerifyOptions{
				Roots: poolOf(newCA(t, t0)), CurrentTime: t0, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			})
			require.Error(t, err)

			require.Len(t, cert.URIs, 1)
			require.Equal(t, id.URI().String(), cert.URIs[0].String())
			got, err := IdentityOf(cert)
			require.NoError(t, err)
			require.Equal(t, id, got)

			require.Equal(t, t0.Add(-time.Hour), cert.NotBefore)
			require.Equal(t, t0.Add(90*24*time.Hour), cert.NotAfter)
			require.False(t, cert.IsCA)
			require.Zero(t, cert.KeyUsage&x509.KeyUsageCertSign)
			require.Empty(t, cert.DNSNames)
			require.True(t, key.PublicKey.Equal(cert.PublicKey))
			require.Positive(t, cert.SerialNumber.Sign())
		})
	}
}

func TestIssueClientDrawsSerials(t *testing.T) {
	ca := newCA(t, t0)
	a, _ := issueClient(t, ca, hostID("node1"), t0)
	b, _ := issueClient(t, ca, hostID("node1"), t0)
	require.NotEqual(t, a.SerialNumber, b.SerialNumber)
}

func TestIssueClientRefuses(t *testing.T) {
	ca := newCA(t, t0)
	key, err := NewKey(rand.Reader)
	require.NoError(t, err)
	csrFor := func(id Identity) []byte {
		csr, err := NewCSR(key, id, rand.Reader)
		require.NoError(t, err)
		return csr
	}

	noURI, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "node1"}}, key)
	require.NoError(t, err)
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	p384CSR, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{hostID("node1").URI()}}, p384)
	require.NoError(t, err)
	tampered := csrFor(hostID("node1"))
	tampered[len(tampered)-1] ^= 0x01

	cases := []struct {
		name          string
		csr           []byte
		want          Identity
		wantSubstring string
	}{
		{"another node", csrFor(hostID("node2")), hostID("node1"), "is for pco:host:0123456789ab:node2, not pco:host:0123456789ab:node1"},
		{"another kind", csrFor(applianceID("node1", 9100)), hostID("node1"), "is for pco:appliance"},
		{"another install", csrFor(Identity{Kind: KindHost, InstallID: "ba9876543210", Node: "node1"}), hostID("node1"), "is for pco:host:ba9876543210"},
		{"another vmid", csrFor(applianceID("node2", 9101)), applianceID("node2", 9100), "is for pco:appliance:0123456789ab:node2:9101"},
		{"no identity", noURI, hostID("node1"), "carries no pco identity"},
		{"a P-384 key", p384CSR, hostID("node1"), "P-256"},
		{"a signature that does not match", tampered, hostID("node1"), "not signed by its key"},
		{"not a request", []byte("nonsense"), hostID("node1"), "reading the certificate request"},
		{"an identity that is not one", csrFor(hostID("node1")), Identity{Kind: KindHost, InstallID: testInstall, Node: "node1", VMID: 9100}, "only an appliance"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ca.IssueClient(c.csr, c.want, t0, rand.Reader)
			require.ErrorContains(t, err, c.wantSubstring)
		})
	}

	t.Run("an expired CA", func(t *testing.T) {
		_, err := ca.IssueClient(csrFor(hostID("node1")), hostID("node1"), ca.Cert.NotAfter.Add(time.Second), rand.Reader)
		require.ErrorContains(t, err, "the CA expired")
	})
	t.Run("no CA", func(t *testing.T) {
		_, err := CA{}.IssueClient(csrFor(hostID("node1")), hostID("node1"), t0, rand.Reader)
		require.ErrorContains(t, err, "no CA")
	})
}

func TestALeafEndsWithItsCA(t *testing.T) {
	ca := newCA(t, t0)
	now := ca.Cert.NotAfter.Add(-10 * 24 * time.Hour)
	cert, _ := issueClient(t, ca, hostID("node1"), now)
	require.Equal(t, ca.Cert.NotAfter, cert.NotAfter)
}

func TestIssueServer(t *testing.T) {
	ca := newCA(t, t0)
	key, err := NewKey(rand.Reader)
	require.NoError(t, err)
	name := NodeServerName(testInstall, "node1")
	addrs := []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("::ffff:192.0.2.11"), netip.MustParseAddr("2001:db8::10")}

	cert, err := ca.IssueServer(&key.PublicKey, []string{name}, addrs, t0, rand.Reader)
	require.NoError(t, err)

	_, err = cert.Verify(x509.VerifyOptions{
		Roots: poolOf(ca), CurrentTime: t0, DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	require.NoError(t, err)
	_, err = cert.Verify(x509.VerifyOptions{
		Roots: poolOf(ca), CurrentTime: t0, DNSName: NodeServerName(testInstall, "node2"), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	require.Error(t, err)
	_, err = cert.Verify(x509.VerifyOptions{
		Roots: poolOf(ca), CurrentTime: t0, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	require.Error(t, err, "a server certificate does not authenticate a client")

	require.Equal(t, []string{name}, cert.DNSNames)
	require.Len(t, cert.IPAddresses, 3)
	require.True(t, cert.IPAddresses[1].Equal(net.ParseIP("192.0.2.11")))
	require.Len(t, cert.IPAddresses[1], net.IPv4len, "a mapped address is named as the IPv4 one")
	require.Empty(t, cert.URIs)
	require.Equal(t, t0.Add(-time.Hour), cert.NotBefore)
	require.Equal(t, t0.Add(90*24*time.Hour), cert.NotAfter)
	require.False(t, cert.IsCA)
	require.True(t, key.PublicKey.Equal(cert.PublicKey))
}

func TestIssueServerRefuses(t *testing.T) {
	ca := newCA(t, t0)
	key, err := NewKey(rand.Reader)
	require.NoError(t, err)
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	name := ControllerServerName(testInstall)

	cases := []struct {
		name          string
		pub           *ecdsa.PublicKey
		names         []string
		addrs         []netip.Addr
		wantSubstring string
	}{
		{"nothing to name", &key.PublicKey, nil, nil, "no names"},
		{"an empty name", &key.PublicKey, []string{""}, nil, "empty name"},
		{"an invalid address", &key.PublicKey, []string{name}, []netip.Addr{{}}, "invalid address"},
		{"a P-384 key", &p384.PublicKey, []string{name}, nil, "P-256"},
		{"no key", nil, []string{name}, nil, "no public key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ca.IssueServer(c.pub, c.names, c.addrs, t0, rand.Reader)
			require.ErrorContains(t, err, c.wantSubstring)
		})
	}
}

func TestServerNames(t *testing.T) {
	require.Equal(t, "controller.pco-0123456789ab.invalid", ControllerServerName(testInstall))
	require.Equal(t, "node-node1.pco-0123456789ab.invalid", NodeServerName(testInstall, "node1"))
	require.Equal(t, "node-node1.pco-0123456789ab.invalid", NodeServerName(testInstall, "Node1"))
}
