package pve

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// nodeCert is a self-signed certificate for 127.0.0.1, as a node has.
func nodeCert(t *testing.T, name string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func writeCert(t *testing.T, path string, cert tls.Certificate) {
	t.Helper()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	// pveproxy-ssl.pem and pve-ssl.pem carry the certificate alone; a key
	// before it is skipped all the same.
	require.NoError(t, os.WriteFile(path, append([]byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"), pemBytes...), 0o644))
}

// loopbackNode is the API of a node on 127.0.0.1 that presents the
// certificate the test sets, and counts the requests that reach it.
type loopbackNode struct {
	srv      *httptest.Server
	cert     atomic.Pointer[tls.Certificate]
	requests atomic.Int32
}

func newLoopbackNode(t *testing.T, cert tls.Certificate) *loopbackNode {
	t.Helper()
	n := &loopbackNode{}
	n.cert.Store(&cert)
	n.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.requests.Add(1)
		_, _ = io.WriteString(w, fixture(t, "version.json"))
	}))
	n.srv.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return &tls.Config{Certificates: []tls.Certificate{*n.cert.Load()}}, nil
	}}
	n.srv.StartTLS()
	t.Cleanup(n.srv.Close)
	return n
}

func (n *loopbackNode) client(t *testing.T, certDir string) *Client {
	t.Helper()
	cfg := testConfig(n.srv.URL)
	cfg.NodeCertDir = certDir
	c, err := New(cfg)
	require.NoError(t, err)
	return c
}

func TestALoopbackURLIsVerifiedAgainstTheCertificateOfTheNode(t *testing.T) {
	served, other := nodeCert(t, "pve1"), nodeCert(t, "someone else")
	for _, tt := range []struct {
		name    string
		files   map[string]tls.Certificate
		wantErr string // empty: the node is trusted
	}{
		{"the certificate of the cluster", map[string]tls.Certificate{"pve-ssl.pem": served}, ""},
		{"a certificate of the admin's own", map[string]tls.Certificate{"pveproxy-ssl.pem": served, "pve-ssl.pem": other}, ""},
		{"the admin's own comes first", map[string]tls.Certificate{"pveproxy-ssl.pem": other, "pve-ssl.pem": served}, "pveproxy-ssl.pem"},
		{"another certificate", map[string]tls.Certificate{"pve-ssl.pem": other}, "pve-ssl.pem"},
		{"no certificate of the node", nil, "pve-ssl.pem"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := newLoopbackNode(t, served)
			dir := t.TempDir()
			for name, cert := range tt.files {
				writeCert(t, filepath.Join(dir, name), cert)
			}

			v, err := node.client(t, dir).Version(context.Background())

			if tt.wantErr == "" {
				require.NoError(t, err)
				require.Equal(t, 9, v.Major)
				return
			}
			require.ErrorContains(t, err, filepath.Join(dir, tt.wantErr))
			require.Zero(t, node.requests.Load(), "the token is never sent")
			require.NotContains(t, err.Error(), testSecret)
		})
	}
}

// An admin may replace the certificate of the node while the daemon runs:
// the file is read again before a certificate that does not match is
// refused.
func TestAReplacedCertificateOfTheNodeIsReadAgain(t *testing.T) {
	first, second := nodeCert(t, "pve1"), nodeCert(t, "pve1 renewed")
	node := newLoopbackNode(t, first)
	dir := t.TempDir()
	file := filepath.Join(dir, "pve-ssl.pem")
	writeCert(t, file, first)
	c := node.client(t, dir)
	_, err := c.Version(context.Background())
	require.NoError(t, err)

	writeCert(t, file, second)
	node.cert.Store(&second)
	c.hc.CloseIdleConnections()
	_, err = c.Version(context.Background())
	require.NoError(t, err)

	node.cert.Store(&first)
	c.hc.CloseIdleConnections()
	_, err = c.Version(context.Background())
	require.ErrorContains(t, err, file)
}

func TestARemoteURLIsNotPinned(t *testing.T) {
	c, err := New(Config{BaseURL: "https://10.20.0.2:8006", TokenID: testTokenID, Secret: testSecret, CAFile: writeCA(t, unrelatedCA(t))})
	require.NoError(t, err)
	tlsCfg := c.hc.Transport.(*http.Transport).TLSClientConfig
	require.False(t, tlsCfg.InsecureSkipVerify)
	require.Nil(t, tlsCfg.VerifyConnection)
}
