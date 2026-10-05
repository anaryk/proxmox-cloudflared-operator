package pve

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

const (
	testTokenID = "pco@pve!pco"
	testSecret  = "4f6b2a1e-0c9d-4e57-9a3b-secret"
	wantAuth    = "PVEAPIToken=pco@pve!pco=4f6b2a1e-0c9d-4e57-9a3b-secret"
)

type reply struct {
	status int
	body   string
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

func okReply(t *testing.T, name string) reply {
	t.Helper()
	return reply{http.StatusOK, fixture(t, name)}
}

type seenRequest struct {
	method string
	uri    string
	auth   string
	accept string
}

// recorder remembers the requests a test server received.
type recorder struct {
	mu   sync.Mutex
	seen []seenRequest
}

func (r *recorder) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, seenRequest{
		method: req.Method,
		uri:    req.URL.RequestURI(),
		auth:   req.Header.Get("Authorization"),
		accept: req.Header.Get("Accept"),
	})
}

func (r *recorder) requests() []seenRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]seenRequest(nil), r.seen...)
}

func testConfig(baseURL string) Config {
	return Config{BaseURL: baseURL, TokenID: testTokenID, Secret: testSecret}
}

// startServer runs h behind TLS and returns a client that trusts it.
func startServer(t *testing.T, cfg Config, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	cfg.BaseURL = srv.URL
	c, err := newWithHTTPClient(cfg, srv.Client())
	require.NoError(t, err)
	return c
}

// newTestClient serves routes, keyed by the request URI below /api2/json
// (for example "/nodes/pve1/network"); anything else answers 404.
func newTestClient(t *testing.T, routes map[string]reply) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		rep, ok := routes[strings.TrimPrefix(r.URL.RequestURI(), "/api2/json")]
		if !ok {
			rep = reply{http.StatusNotFound, fmt.Sprintf(`{"data":null,"message":"unexpected request %s"}`, r.URL.RequestURI())}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	})
	return startServer(t, testConfig(""), h), rec
}

// newErrorClient answers every request with the same reply.
func newErrorClient(t *testing.T, status int, body string) *Client {
	t.Helper()
	return startServer(t, testConfig(""), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

func writeCA(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	require.NoError(t, os.WriteFile(path, pemBytes, 0o600))
	return path
}

// unrelatedCA returns a fresh self-signed certificate that signed nothing the
// test servers present.
func unrelatedCA(t *testing.T) *x509.Certificate {
	t.Helper()
	return namedCA(t, "unrelated test ca")
}

// namedCA returns a fresh self-signed CA certificate with the given name.
func namedCA(t *testing.T, name string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert
}

func TestNewValidatesConfig(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	caFile := writeCA(t, srv.Certificate())
	emptyFile := filepath.Join(t.TempDir(), "empty.pem")
	require.NoError(t, os.WriteFile(emptyFile, []byte("not a certificate\n"), 0o600))

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // empty: the config is accepted
	}{
		{"loopback ipv4 without ca", func(c *Config) {}, ""},
		{"loopback ipv6 without ca", func(c *Config) { c.BaseURL = "https://[::1]:8006" }, ""},
		{"loopback with default port", func(c *Config) { c.BaseURL = "https://127.0.0.1" }, ""},
		{"loopback with ca file", func(c *Config) { c.CAFile = caFile }, ""},
		{"base url with a path", func(c *Config) { c.BaseURL = "https://127.0.0.1:8006/proxy" }, ""},
		{"remote host with ca", func(c *Config) {
			c.BaseURL = "https://pve1.lab.invalid:8006"
			c.CAFile = caFile
		}, ""},
		{"http scheme", func(c *Config) { c.BaseURL = "http://127.0.0.1:8006" }, "https"},
		{"no scheme", func(c *Config) { c.BaseURL = "127.0.0.1" }, "https"},
		{"scheme is a host name", func(c *Config) { c.BaseURL = "pve1.lab.invalid:8006" }, "https"},
		{"empty base url", func(c *Config) { c.BaseURL = "" }, "https"},
		{"base url without host", func(c *Config) { c.BaseURL = "https://" }, "host"},
		{"base url with user name", func(c *Config) { c.BaseURL = "https://pco@127.0.0.1:8006" }, "credentials"},
		{"base url with credentials", func(c *Config) { c.BaseURL = "https://pco:" + testSecret + "@127.0.0.1:8006" }, "credentials"},
		{"base url with query", func(c *Config) { c.BaseURL = "https://127.0.0.1:8006?x=1" }, "query"},
		{"base url with empty query", func(c *Config) { c.BaseURL = "https://127.0.0.1:8006/?" }, "query"},
		{"base url with fragment", func(c *Config) { c.BaseURL = "https://127.0.0.1:8006#top" }, "fragment"},
		{"unparsable base url", func(c *Config) { c.BaseURL = "https://127.0.0.1:port" }, "base url"},
		{"token id without bang", func(c *Config) { c.TokenID = "pco@pve" }, "token id"},
		{"token id without name", func(c *Config) { c.TokenID = "pco@pve!" }, "token id"},
		{"token id without user", func(c *Config) { c.TokenID = "!pco" }, "token id"},
		{"empty secret", func(c *Config) { c.Secret = "" }, "secret"},
		{"remote host without ca", func(c *Config) { c.BaseURL = "https://pve1.lab.invalid:8006" }, "CA file"},
		{"remote ip without ca", func(c *Config) { c.BaseURL = "https://10.20.0.2:8006" }, "CA file"},
		{"localhost name is not an address", func(c *Config) { c.BaseURL = "https://localhost:8006" }, "CA file"},
		{"loopback with missing ca file", func(c *Config) {
			c.CAFile = filepath.Join(t.TempDir(), "missing.pem")
		}, "CA file"},
		{"loopback with ca file holding no certificate", func(c *Config) { c.CAFile = emptyFile }, "no certificates"},
		{"remote host with missing ca file", func(c *Config) {
			c.BaseURL = "https://pve1.lab.invalid:8006"
			c.CAFile = filepath.Join(t.TempDir(), "missing.pem")
		}, "CA file"},
		{"remote host with ca file holding no certificate", func(c *Config) {
			c.BaseURL = "https://pve1.lab.invalid:8006"
			c.CAFile = emptyFile
		}, "no certificates"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig("https://127.0.0.1:8006")
			tt.mutate(&cfg)
			c, err := New(cfg)
			if tt.wantErr == "" {
				require.NoError(t, err)
				require.NotNil(t, c)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
			require.NotContains(t, err.Error(), testSecret)
		})
	}
}

func TestNewDefaultsTimeout(t *testing.T) {
	c, err := New(testConfig("https://127.0.0.1:8006"))
	require.NoError(t, err)
	require.Equal(t, 10*time.Second, c.timeout)

	cfg := testConfig("https://127.0.0.1:8006")
	cfg.Timeout = 3 * time.Second
	c, err = New(cfg)
	require.NoError(t, err)
	require.Equal(t, 3*time.Second, c.timeout)
}

// remoteServer serves the version fixture over TLS. Its certificate is valid
// for example.com; the clients of remote reach it whatever host they are
// pointed at.
func remoteServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, fixture(t, "version.json"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// remote is a client for cfg, with host the host of its base URL, whose
// connections go to srv.
func remote(t *testing.T, srv *httptest.Server, host string, cfg Config) *Client {
	t.Helper()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	cfg.BaseURL = "https://" + host + ":" + port
	base, err := parseConfig(cfg)
	require.NoError(t, err)
	hc, err := newHTTPClient(base, cfg, x509.SystemCertPool)
	require.NoError(t, err)
	hc.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	return newClient(cfg, base, hc)
}

func TestCAFileIsUsedForRemoteHosts(t *testing.T) {
	srv := remoteServer(t)
	withCA := func(caFile string) Config {
		cfg := testConfig("")
		cfg.CAFile = caFile
		return cfg
	}

	v, err := remote(t, srv, "example.com", withCA(writeCA(t, srv.Certificate()))).Version(context.Background())
	require.NoError(t, err)
	require.Equal(t, "9.1", v.Release)

	_, err = remote(t, srv, "example.com", withCA(writeCA(t, unrelatedCA(t)))).Version(context.Background())
	var unknown x509.UnknownAuthorityError
	require.ErrorAs(t, err, &unknown)
}

func TestServerNameIsWhatTheCertificateIsVerifiedUnder(t *testing.T) {
	srv := remoteServer(t)
	cfg := testConfig("")
	cfg.CAFile = writeCA(t, srv.Certificate())

	// The address is not in the certificate; the name is.
	_, err := remote(t, srv, "10.20.0.2", cfg).Version(context.Background())
	var wrongName x509.HostnameError
	require.ErrorAs(t, err, &wrongName)

	cfg.ServerName = "example.com"
	c := remote(t, srv, "10.20.0.2", cfg)
	require.Equal(t, "example.com", c.hc.Transport.(*http.Transport).TLSClientConfig.ServerName)
	v, err := c.Version(context.Background())
	require.NoError(t, err)
	require.Equal(t, "9.1", v.Release)

	cfg.ServerName = "pve1.lab.invalid"
	_, err = remote(t, srv, "10.20.0.2", cfg).Version(context.Background())
	require.ErrorAs(t, err, &wrongName)
}

func TestTheClientKeepsWhyTheCertificateStoppedVerifying(t *testing.T) {
	srv := remoteServer(t)
	cfg := testConfig("")
	cfg.CAFile = writeCA(t, unrelatedCA(t))
	cfg.ServerName = "example.com"
	c := remote(t, srv, "10.20.0.2", cfg)
	require.NoError(t, c.LastVerifyError(), "nothing asked yet")

	_, err := c.Version(t.Context())
	require.Error(t, err)
	var unknown x509.UnknownAuthorityError
	require.ErrorAs(t, c.LastVerifyError(), &unknown, "another CA signed the certificate")

	// The certificate is put back in order: the next request verifies.
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	c.hc.Transport.(*http.Transport).TLSClientConfig.RootCAs = pool
	_, err = c.Version(t.Context())
	require.NoError(t, err)
	require.NoError(t, c.LastVerifyError())

	// Under a name it is not valid for, it does not.
	c.hc.Transport.(*http.Transport).TLSClientConfig.ServerName = "pve1.lab.invalid"
	c.hc.Transport.(*http.Transport).CloseIdleConnections()
	_, err = c.Version(t.Context())
	require.Error(t, err)
	var wrongName x509.HostnameError
	require.ErrorAs(t, c.LastVerifyError(), &wrongName)
}

func TestAFailureBeforeTheHandshakeLeavesTheVerifyErrorAlone(t *testing.T) {
	srv := remoteServer(t)
	cfg := testConfig("")
	cfg.CAFile = writeCA(t, unrelatedCA(t))
	cfg.ServerName = "example.com"
	c := remote(t, srv, "10.20.0.2", cfg)
	_, err := c.Version(t.Context())
	require.Error(t, err)
	before := c.LastVerifyError()
	require.Error(t, before)

	srv.Close()
	_, err = c.Version(t.Context())
	require.Error(t, err)
	require.Equal(t, before, c.LastVerifyError(), "a server that is gone says nothing of its certificate")
}

func TestNewTrustsTheSystemRootsBesidesTheCAFile(t *testing.T) {
	system, err := x509.SystemCertPool()
	if err != nil {
		t.Skipf("no system roots here: %v", err)
	}
	ca := namedCA(t, "cluster ca")
	caFile := writeCA(t, ca)
	c, err := New(Config{BaseURL: "https://10.20.0.2:8006", TokenID: testTokenID, Secret: testSecret, CAFile: caFile})
	require.NoError(t, err)

	system.AddCert(ca)
	require.True(t, system.Equal(c.hc.Transport.(*http.Transport).TLSClientConfig.RootCAs))
}

func TestCAPoolHoldsTheSystemRootsAndTheFile(t *testing.T) {
	system, file := namedCA(t, "a public root"), namedCA(t, "cluster ca")
	roots := func() (*x509.CertPool, error) {
		pool := x509.NewCertPool()
		pool.AddCert(system)
		return pool, nil
	}
	pool, err := loadCAPool(writeCA(t, file), roots, zerolog.Nop())
	require.NoError(t, err)

	for name, cert := range map[string]*x509.Certificate{"system root": system, "file": file} {
		_, err := cert.Verify(x509.VerifyOptions{Roots: pool})
		require.NoError(t, err, name)
	}
	_, err = unrelatedCA(t).Verify(x509.VerifyOptions{Roots: pool})
	require.Error(t, err)
}

func TestCAPoolWithoutSystemRootsIsTheFileAlone(t *testing.T) {
	var logged bytes.Buffer
	file := namedCA(t, "cluster ca")
	noRoots := func() (*x509.CertPool, error) { return nil, errors.New("no root store here") }

	pool, err := loadCAPool(writeCA(t, file), noRoots, zerolog.New(&logged))
	require.NoError(t, err)
	_, err = file.Verify(x509.VerifyOptions{Roots: pool})
	require.NoError(t, err)
	require.Contains(t, logged.String(), "system roots")
	require.Contains(t, logged.String(), "no root store here")

	_, err = loadCAPool(filepath.Join(t.TempDir(), "missing.pem"), noRoots, zerolog.Nop())
	require.ErrorContains(t, err, "CA file")
}

func TestRequestCarriesAuthAndAccept(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/version": {http.StatusOK, `{"data":{"release":"9.1"}}`},
	})
	_, err := c.Version(context.Background())
	require.NoError(t, err)

	require.Equal(t, []seenRequest{{
		method: http.MethodGet,
		uri:    "/api2/json/version",
		auth:   wantAuth,
		accept: "application/json",
	}}, rec.requests())
}

func TestBaseURLPathIsKept(t *testing.T) {
	rec := &recorder{}
	c := startServer(t, testConfig(""), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		_, _ = io.WriteString(w, `{"data":{"release":"9.1"}}`)
	}))
	c.base = c.base.JoinPath("proxy")

	_, err := c.Version(context.Background())
	require.NoError(t, err)
	require.Equal(t, "/proxy/api2/json/version", rec.requests()[0].uri)
}

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         string
		wantMessage  string
		wantNotFound bool
		wantForbid   bool
	}{
		{"unauthorized", 401, `{"data":null,"message":"no ticket\n"}`, "no ticket", false, true},
		{"forbidden", 403, `{"data":null,"message":"Permission check failed (/vms/101, VM.Audit)"}`, "Permission check failed (/vms/101, VM.Audit)", false, true},
		{"not found", 404, `{"data":null,"message":"no such node\n"}`, "no such node", true, false},
		{"config does not exist", 500, fixture(t, "config_missing.json"), "Configuration file 'nodes/pve1/qemu-server/999.conf' does not exist", true, false},
		{"other server error", 500, `{"data":null,"message":"got timeout\n"}`, "got timeout", false, false},
		{"bad request", 400, `{"data":null,"errors":{"vmid":"value is not valid"},"message":"Parameter verification failed.\n"}`, "Parameter verification failed.", false, false},
		{"body is not json", 502, "<html>bad gateway</html>", "Bad Gateway", false, false},
		{"empty message", 404, `{"data":null,"message":"  \n"}`, "Not Found", true, false},
		{"empty body", 503, "", "Service Unavailable", false, false},
		{"unknown status", 599, "", "unknown error", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newErrorClient(t, tt.status, tt.body)
			_, err := c.Version(context.Background())
			require.Error(t, err)

			var apiErr *APIError
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, tt.status, apiErr.Status)
			require.Equal(t, tt.wantMessage, apiErr.Message)
			require.Equal(t, tt.wantNotFound, IsNotFound(err))
			require.Equal(t, tt.wantForbid, IsForbidden(err))
			require.False(t, errors.Is(err, ErrAgentUnavailable))
		})
	}
}

func TestErrorPredicatesIgnoreOtherErrors(t *testing.T) {
	for _, err := range []error{nil, errors.New("boom"), context.Canceled} {
		require.False(t, IsNotFound(err))
		require.False(t, IsForbidden(err))
	}

	wrapped := fmt.Errorf("fetching: %w", &APIError{Status: 404, Message: "gone"})
	require.True(t, IsNotFound(wrapped))
	require.False(t, IsForbidden(wrapped))
}

func TestAPIErrorString(t *testing.T) {
	err := &APIError{Status: 500, Message: "got timeout"}
	require.Equal(t, "proxmox api: HTTP 500: got timeout", err.Error())
}

func TestAgentUnavailableMessages(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"agent missing", fixture(t, "agent_missing.json")},
		{"agent not running", fixture(t, "agent_not_running.json")},
		{"guest stopped", fixture(t, "guest_stopped.json")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newErrorClient(t, 500, tt.body)
			_, err := c.AgentInterfaces(context.Background(), "pve1", 101)
			require.ErrorIs(t, err, ErrAgentUnavailable)

			var apiErr *APIError
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, 500, apiErr.Status)
			require.Contains(t, err.Error(), apiErr.Message)
		})
	}
}

func TestAgentOtherFailuresAreNotUnavailable(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantMessage string
	}{
		{"other 500", 500, `{"data":null,"message":"got timeout\n"}`, "got timeout"},
		{"guest agent text on a 403", 403, `{"data":null,"message":"guest agent audit denied\n"}`, "guest agent audit denied"},
		{"guest agent text on a 404", 404, `{"data":null,"message":"guest agent missing\n"}`, "guest agent missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newErrorClient(t, tt.status, tt.body)
			_, err := c.AgentInterfaces(context.Background(), "pve1", 101)
			require.False(t, errors.Is(err, ErrAgentUnavailable))

			var apiErr *APIError
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, tt.status, apiErr.Status)
			require.Equal(t, tt.wantMessage, apiErr.Message)
		})
	}
}

func TestAgentMessagesOnOtherEndpointsStayPlain(t *testing.T) {
	c := newErrorClient(t, 500, fixture(t, "agent_missing.json"))
	_, err := c.LXCInterfaces(context.Background(), "pve1", 200)
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrAgentUnavailable))
}

func TestContextCancellationReturnsPromptly(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	c := startServer(t, testConfig(""), http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-r.Context().Done()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		_, err := c.Version(ctx)
		done <- err
	}()

	<-started
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("request did not return after the context was cancelled")
	}
}

func TestRequestTimeout(t *testing.T) {
	cfg := testConfig("")
	cfg.Timeout = 20 * time.Millisecond
	c := startServer(t, cfg, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))

	_, err := c.Version(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestResponseBodyLimit(t *testing.T) {
	huge := `{"data":{"release":"` + strings.Repeat("9", maxBodyBytes) + `"}}`
	c := newErrorClient(t, 200, huge)
	_, err := c.Version(context.Background())
	require.ErrorContains(t, err, "exceeds")

	c = newErrorClient(t, 500, strings.Repeat("x", maxBodyBytes+1))
	_, err = c.Version(context.Background())
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, 500, apiErr.Status)
	require.Equal(t, "Internal Server Error", apiErr.Message)
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	cfg := testConfig(srv.URL)
	cfg.NodeCertDir = t.TempDir()
	writeCert(t, filepath.Join(cfg.NodeCertDir, "pve-ssl.pem"), tls.Certificate{Certificate: [][]byte{srv.Certificate().Raw}})

	c, err := New(cfg)
	require.NoError(t, err)
	_, err = c.Version(context.Background())

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusFound, apiErr.Status)
	require.Len(t, rec.requests(), 1)
}

func TestMalformedResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not json", "pong"},
		{"truncated", `{"data":{"release":"9.`},
		{"wrong shape", `{"data":["9.1"]}`},
		{"no data member", `{}`},
		{"null data", `{"data":null}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newErrorClient(t, 200, tt.body)
			_, err := c.Version(context.Background())
			require.Error(t, err)
			var apiErr *APIError
			require.False(t, errors.As(err, &apiErr))
		})
	}
}

func TestErrorsNeverContainTheSecret(t *testing.T) {
	ctx := context.Background()
	var errs []error
	collect := func(c *Client) {
		_, err := c.Version(ctx)
		errs = append(errs, err)
		_, err = c.Resources(ctx)
		errs = append(errs, err)
		_, err = c.AgentInterfaces(ctx, "pve1", 101)
		errs = append(errs, err)
	}

	for _, status := range []int{400, 401, 403, 404, 500} {
		collect(newErrorClient(t, status, `{"data":null,"message":"denied\n"}`))
	}
	collect(newErrorClient(t, 200, "not json"))

	closed := httptest.NewTLSServer(http.NotFoundHandler())
	cfg := testConfig(closed.URL)
	c, err := newWithHTTPClient(cfg, closed.Client())
	require.NoError(t, err)
	closed.Close()
	collect(c)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = c.Version(cancelled)
	errs = append(errs, err)

	for _, bad := range []Config{
		{BaseURL: "http://127.0.0.1:8006", TokenID: testTokenID, Secret: testSecret},
		{BaseURL: "https://127.0.0.1:8006", TokenID: "pco", Secret: testSecret},
		{BaseURL: "https://pve1.lab.invalid:8006", TokenID: testTokenID, Secret: testSecret},
		{BaseURL: "https://pve1.lab.invalid:8006", TokenID: testTokenID, Secret: testSecret, CAFile: "/nonexistent/ca.pem"},
	} {
		_, err := New(bad)
		errs = append(errs, err)
	}

	for _, err := range errs {
		require.Error(t, err)
		require.NotContains(t, err.Error(), testSecret)
	}
}
