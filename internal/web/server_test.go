package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

const peer = "192.0.2.7:51234"

func newTestServer(t *testing.T, cfg Config, mounts ...Mounter) *Server {
	t.Helper()
	s, err := New(cfg, mounts...)
	require.NoError(t, err)
	return s
}

func request(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = peer
	return r
}

func do(s *Server, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func get(s *Server, path, acceptEncoding string) *httptest.ResponseRecorder {
	r := request(http.MethodGet, path)
	if acceptEncoding != "" {
		r.Header.Set("Accept-Encoding", acceptEncoding)
	}
	return do(s, r)
}

// steppingClock is a clock that moves on by step every time it is read.
func steppingClock(start time.Time, step time.Duration) func() time.Time {
	now := start
	return func() time.Time {
		t := now
		now = now.Add(step)
		return t
	}
}

func TestTheRequestLog(t *testing.T) {
	var buf testutil.SyncBuffer
	s := newTestServer(t, Config{
		Assets: testAssets(),
		Now:    steppingClock(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), 1500*time.Microsecond),
		Log:    zerolog.New(&buf),
	}, stubMounter{})

	r := request(http.MethodGet, "/routes/app.example.com?owner=qemu/101&token=SECRET-QUERY")
	r.Header.Set("X-Forwarded-For", "203.0.113.99")
	r.Header.Set("X-Real-Ip", "203.0.113.98")
	r.Header.Set("Cookie", "__Host-pco-session=SECRET-COOKIE")
	r.Header.Set("Authorization", "PVEAPIToken=root@pam!pco=SECRET-TOKEN")
	r.Body = io.NopCloser(strings.NewReader("SECRET-BODY"))
	require.Equal(t, http.StatusOK, do(s, r).Code)

	line := buf.String()
	for _, secret := range []string{"203.0.113", "SECRET", "owner", "Cookie", "Authorization"} {
		require.NotContains(t, line, secret)
	}
	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &entry))
	require.Equal(t, map[string]any{
		"level":    "info",
		"method":   "GET",
		"path":     "/routes/app.example.com",
		"status":   float64(200),
		"duration": 1.5,
		"peer":     peer,
		"message":  "request",
	}, entry)
}

func TestTheRequestLogOfAFailure(t *testing.T) {
	var buf testutil.SyncBuffer
	s := newTestServer(t, Config{Assets: testAssets(), Log: zerolog.New(&buf)}, stubMounter{})

	require.Equal(t, http.StatusNotFound, get(s, "/nope?x=1", "").Code)
	require.Equal(t, http.StatusMethodNotAllowed, do(s, request(http.MethodDelete, "/")).Code)
	require.Equal(t, http.StatusInternalServerError, do(s, request(http.MethodPost, "/api/panic")).Code)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 4)
	want := []struct {
		method, path string
		status       float64
	}{
		{"GET", "/nope", 404},
		{"DELETE", "/", 405},
		{"POST", "/api/panic", 500},
		{"POST", "/api/panic", 500},
	}
	// The panic is logged once with its stack, and the request once as every
	// request is; what it panicked with may carry a secret and is left out.
	require.NotContains(t, buf.String(), "SECRET")
	for i, line := range lines {
		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &entry))
		require.Equal(t, want[i].method, entry["method"], line)
		require.Equal(t, want[i].path, entry["path"], line)
		if i == 2 {
			require.Equal(t, "error", entry["level"])
			require.Contains(t, entry["stack"], "stubMounter")
			continue
		}
		require.Equal(t, want[i].status, entry["status"], line)
	}
}

func TestTheClientAddressIsTheTCPPeer(t *testing.T) {
	s := newTestServer(t, Config{Assets: testAssets()}, stubMounter{})
	r := request(http.MethodGet, "/api/peer")
	r.Header.Set("X-Forwarded-For", "203.0.113.99")
	r.Header.Set("X-Real-Ip", "203.0.113.98")
	require.Equal(t, "192.0.2.7", do(s, r).Body.String())
}

func TestRun(t *testing.T) {
	dir := testutil.ShortDir(t)
	notified := listenNotify(t, filepath.Join(dir, "notify"))
	certFile, keyFile, pool := writeCertificate(t, dir)

	s := newTestServer(t, Config{
		Listen:   "127.0.0.1:0",
		CertFile: certFile,
		KeyFile:  keyFile,
		Assets:   testAssets(),
	})
	addrs := make(chan net.Addr, 1)
	s.listening = func(a net.Addr) { addrs <- a }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	var addr net.Addr
	select {
	case addr = <-addrs:
	case err := <-done:
		t.Fatalf("Run returned before listening: %v", err)
	}
	require.Equal(t, "READY=1", <-notified)

	t.Run("over HTTP/2 with TLS", func(t *testing.T) {
		client := &http.Client{Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2: true,
		}}
		defer client.CloseIdleConnections()
		resp, err := client.Get("https://" + addr.String() + "/")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, 2, resp.ProtoMajor)
		require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, indexPage, string(body))
	})

	t.Run("not under TLS 1.2", func(t *testing.T) {
		conn, err := tls.Dial("tcp", addr.String(), &tls.Config{RootCAs: pool, MaxVersion: tls.VersionTLS11})
		if err == nil {
			_ = conn.Close()
		}
		require.Error(t, err)
	})

	t.Run("not in plain HTTP", func(t *testing.T) {
		resp, err := http.Get("http://" + addr.String() + "/")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	cancel()
	require.NoError(t, <-done)
	require.Equal(t, "STOPPING=1", <-notified)
}

func TestRunWithoutItsCertificate(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, Config{
		Listen:   "127.0.0.1:0",
		CertFile: filepath.Join(dir, "tls.crt"),
		KeyFile:  filepath.Join(dir, "tls.key"),
		Assets:   testAssets(),
	})
	err := s.Run(context.Background())
	require.ErrorContains(t, err, "tls.crt")
}

func TestRunOnAnAddressInUse(t *testing.T) {
	dir := testutil.ShortDir(t)
	certFile, keyFile, _ := writeCertificate(t, dir)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = taken.Close() }()

	s := newTestServer(t, Config{Listen: taken.Addr().String(), CertFile: certFile, KeyFile: keyFile, Assets: testAssets()})
	require.ErrorContains(t, s.Run(context.Background()), taken.Addr().String())
}

// listenNotify points NOTIFY_SOCKET at a socket of the test and returns what
// is sent to it.
func listenNotify(t *testing.T, path string) <-chan string {
	t.Helper()
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	t.Setenv("NOTIFY_SOCKET", path)
	out := make(chan string, 4)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			out <- string(buf[:n])
		}
	}()
	return out
}

// writeCertificate writes a self-signed certificate for 127.0.0.1 and its key
// into dir, and returns the pool that trusts it.
func writeCertificate(t *testing.T, dir string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "pco test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}
