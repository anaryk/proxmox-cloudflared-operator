package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

const testInstall = "0123456789ab"

func newCA(t *testing.T, now time.Time) CA {
	t.Helper()
	ca, err := NewCA(testInstall, now, rand.Reader)
	require.NoError(t, err)
	return ca
}

func TestNewCA(t *testing.T) {
	ca := newCA(t, t0)

	c := ca.Cert
	require.True(t, c.IsCA)
	require.True(t, c.BasicConstraintsValid)
	require.Zero(t, c.MaxPathLen)
	require.True(t, c.MaxPathLenZero, "the CA signs leaves only")
	require.NotZero(t, c.KeyUsage&x509.KeyUsageCertSign)
	require.Equal(t, t0.Add(-time.Hour), c.NotBefore)
	require.Equal(t, t0.Add(10*365*24*time.Hour), c.NotAfter)
	require.Contains(t, c.Subject.CommonName, testInstall)
	require.NoError(t, c.CheckSignatureFrom(c))

	pub, ok := c.PublicKey.(*ecdsa.PublicKey)
	require.True(t, ok)
	require.Equal(t, elliptic.P256(), pub.Curve)
	require.True(t, pub.Equal(ca.Key.Public()))

	other := newCA(t, t0)
	require.NotEqual(t, c.Subject.String(), other.Cert.Subject.String(),
		"two CAs of one install, as a rotation makes them, have their own names")
}

func TestNewCARefusesABadInstallID(t *testing.T) {
	_, err := NewCA("not an id", t0, rand.Reader)
	require.ErrorContains(t, err, "install id")
}

func TestCAThroughPEM(t *testing.T) {
	ca := newCA(t, t0)
	certPEM, keyPEM, err := ca.PEM()
	require.NoError(t, err)

	got, err := ParseCA(certPEM, keyPEM)
	require.NoError(t, err)
	require.Equal(t, ca.Cert.Raw, got.Cert.Raw)
	require.True(t, ca.Key.Equal(got.Key))
	require.Equal(t, ca.SHA256(), got.SHA256())
	issueServer(t, got, ControllerServerName(testInstall), t0)

	// A key in SEC 1, as openssl ec writes it, is read too.
	sec1, err := x509.MarshalECPrivateKey(ca.Key)
	require.NoError(t, err)
	got, err = ParseCA(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1}))
	require.NoError(t, err)
	require.True(t, ca.Key.Equal(got.Key))
}

func TestParseCARefuses(t *testing.T) {
	ca := newCA(t, t0)
	certPEM, keyPEM, err := ca.PEM()
	require.NoError(t, err)
	_, otherKeyPEM, err := newCA(t, t0).PEM()
	require.NoError(t, err)

	leaf, leafKey := issueClient(t, ca, hostID("node1"), t0)
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})
	leafKeyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	require.NoError(t, err)
	leafKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: leafKeyDER})

	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	p384Cert := selfSigned(t, p384)
	p384DER, err := x509.MarshalPKCS8PrivateKey(p384)
	require.NoError(t, err)

	cases := []struct {
		name          string
		certPEM, key  []byte
		wantSubstring string
	}{
		{"no certificate", nil, keyPEM, "no certificate"},
		{"no key", certPEM, nil, "no private key"},
		{"garbage", []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), keyPEM, "reading the CA certificate"},
		{"two certificates", append(append([]byte{}, certPEM...), certPEM...), keyPEM, "more than one"},
		{"the key of another CA", certPEM, otherKeyPEM, "not the key of the CA certificate"},
		{"a leaf", leafPEM, leafKeyPEM, "not a CA"},
		{"P-384", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p384Cert.Raw}),
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p384DER}), "P-256"},
		{"an encrypted key", certPEM, []byte("-----BEGIN ENCRYPTED PRIVATE KEY-----\nAAAA\n-----END ENCRYPTED PRIVATE KEY-----\n"), "encrypted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseCA(c.certPEM, c.key)
			require.ErrorContains(t, err, c.wantSubstring)
		})
	}
}

func TestCASHA256(t *testing.T) {
	ca := newCA(t, t0)
	sum := sha256.Sum256(ca.Cert.Raw)
	require.Equal(t, hex.EncodeToString(sum[:]), ca.SHA256())
	require.Equal(t, ca.SHA256(), Fingerprint(ca.Cert))
	require.Len(t, ca.SHA256(), 64)
}

// selfSigned is a CA certificate of key, made the way pco does not make them.
func selfSigned(t *testing.T, key *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(7),
		Subject:               pkix.Name{CommonName: "elsewhere"},
		NotBefore:             t0.Add(-time.Hour),
		NotAfter:              t0.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert
}
