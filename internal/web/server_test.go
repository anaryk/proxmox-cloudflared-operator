package web

import (
	"bufio"
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

	"github.com/gin-gonic/gin"
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
		"level":    "debug",
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
		require.Equal(t, "info", entry["level"], line)
		require.Equal(t, want[i].status, entry["status"], line)
	}
}

// A request that went well is for debugging, one that failed is for the
// journal of every day.
func TestTheRequestLogLevels(t *testing.T) {
	cases := []struct {
		method, path string
		status       int
		logged       bool
	}{
		{http.MethodGet, "/", http.StatusOK, false},
		{http.MethodGet, "/api/moved", http.StatusFound, false},
		{http.MethodGet, "/nope", http.StatusNotFound, true},
		{http.MethodDelete, "/", http.StatusMethodNotAllowed, true},
		{http.MethodGet, "/api/fail", http.StatusBadGateway, true},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			var buf testutil.SyncBuffer
			s := newTestServer(t, Config{Assets: testAssets(), Log: zerolog.New(&buf).Level(zerolog.InfoLevel)}, testRoutes{})
			require.Equal(t, c.status, do(s, request(c.method, c.path)).Code)

			if !c.logged {
				require.Empty(t, buf.String())
				return
			}
			require.Contains(t, buf.String(), `"level":"info"`)
			require.Contains(t, buf.String(), `"message":"request"`)
		})
	}

	t.Run("at debug", func(t *testing.T) {
		var buf testutil.SyncBuffer
		s := newTestServer(t, Config{Assets: testAssets(), Log: zerolog.New(&buf).Level(zerolog.DebugLevel)})
		require.Equal(t, http.StatusOK, get(s, "/", "").Code)
		require.Contains(t, buf.String(), `"level":"debug"`)
	})
}

func TestTheClientAddressIsTheTCPPeer(t *testing.T) {
	s := newTestServer(t, Config{Assets: testAssets()}, stubMounter{})
	r := request(http.MethodGet, "/api/peer")
	r.Header.Set("X-Forwarded-For", "203.0.113.99")
	r.Header.Set("X-Real-Ip", "203.0.113.98")
	require.Equal(t, "192.0.2.7", do(s, r).Body.String())
}

// testRoutes are the routes of the tests that need a handler to fail, to move
// on or to hold a request open.
type testRoutes struct {
	// entered gets a value when a handler has begun; release lets the handler
	// that ignores its context go.
	entered chan struct{}
	release chan struct{}
}

func newTestRoutes() testRoutes {
	return testRoutes{entered: make(chan struct{}, 4), release: make(chan struct{})}
}

func (m testRoutes) Mount(r gin.IRouter) {
	r.GET("/api/moved", func(c *gin.Context) { c.Redirect(http.StatusFound, "/") })
	r.GET("/api/fail", func(c *gin.Context) { c.String(http.StatusBadGateway, "bad gateway") })
	r.GET("/api/partial", func(c *gin.Context) {
		_, _ = c.Writer.WriteString("partial")
		panic("ticket PVE:alice@pve:SECRET")
	})
	r.GET("/api/unwritten", func(c *gin.Context) {
		c.Header("Content-Encoding", "gzip")
		panic("ticket PVE:alice@pve:SECRET")
	})
	r.GET("/api/abort", func(*gin.Context) { panic(http.ErrAbortHandler) })
	// A stream: it sends an event and then waits for the end of its request.
	r.GET("/api/stream", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.WriteString("data: hello\n\n")
		c.Writer.Flush()
		m.entered <- struct{}{}
		<-c.Request.Context().Done()
	})
	// A handler that never looks at its context.
	r.GET("/api/stuck", func(*gin.Context) {
		m.entered <- struct{}{}
		<-m.release
	})
}

func TestARouteThatPanicsAfterItWroteTheAnswer(t *testing.T) {
	var buf testutil.SyncBuffer
	s := newTestServer(t, Config{Assets: testAssets(), Log: zerolog.New(&buf)}, testRoutes{})

	rec := do(s, request(http.MethodGet, "/api/partial"))

	// The status and the first bytes are on the wire; a 500 after them would
	// only add its text to the answer.
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "partial", rec.Body.String())
	require.Contains(t, buf.String(), "request handler panicked")
	require.NotContains(t, buf.String(), "SECRET")
}

func TestARouteThatPanicsBeforeItWroteAnything(t *testing.T) {
	s := newTestServer(t, Config{Assets: testAssets()}, testRoutes{})

	rec := do(s, request(http.MethodGet, "/api/unwritten"))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "internal error\n", rec.Body.String())
	require.Empty(t, rec.Header().Get("Content-Encoding"), "the error is plain text, whatever the handler had announced")
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

// net/http ends the connection of a handler that panics with ErrAbortHandler
// and logs nothing; the server has to let the panic through.
func TestAnAbortedRequestIsLeftToNetHTTP(t *testing.T) {
	var buf testutil.SyncBuffer
	s := newTestServer(t, Config{Assets: testAssets(), Log: zerolog.New(&buf)}, testRoutes{})

	require.PanicsWithValue(t, http.ErrAbortHandler, func() {
		do(s, request(http.MethodGet, "/api/abort"))
	})
	require.Empty(t, buf.String())
}

// running is a server that Run serves on a port of the machine.
type running struct {
	s      *Server
	addr   net.Addr
	pool   *x509.CertPool
	cancel context.CancelFunc
	done   chan error
}

func serve(t *testing.T, cfg Config, mounts ...Mounter) *running {
	t.Helper()
	certFile, keyFile, pool := writeCertificate(t, testutil.ShortDir(t))
	cfg.Listen, cfg.CertFile, cfg.KeyFile = "127.0.0.1:0", certFile, keyFile
	if cfg.Assets == nil {
		cfg.Assets = testAssets()
	}
	run := &running{s: newTestServer(t, cfg, mounts...), pool: pool, done: make(chan error, 1)}
	addrs := make(chan net.Addr, 1)
	run.s.listening = func(a net.Addr) { addrs <- a }
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	t.Cleanup(cancel)
	go func() { run.done <- run.s.Run(ctx) }()

	select {
	case run.addr = <-addrs:
	case err := <-run.done:
		t.Fatalf("Run returned before listening: %v", err)
	}
	return run
}

// stop ends Run and returns what it returned, failing the test when that takes
// longer than within.
func (r *running) stop(t *testing.T, within time.Duration) error {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		return err
	case <-time.After(within):
		t.Fatalf("Run did not return within %s of the stop", within)
		return nil
	}
}

// client reaches the server over HTTP/2, or over HTTP/1.1 without h2.
func (r *running) client(t *testing.T, h2 bool) *http.Client {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: r.pool, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: h2,
	}}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

func (r *running) url(path string) string { return "https://" + r.addr.String() + path }

func protocol(h2 bool) string {
	if h2 {
		return "HTTP/2"
	}
	return "HTTP/1.1"
}

func TestRun(t *testing.T) {
	notified := listenNotify(t, filepath.Join(testutil.ShortDir(t), "notify"))
	srv := serve(t, Config{Assets: testAssets()})
	require.Equal(t, "READY=1", <-notified)

	t.Run("over HTTP/2 with TLS", func(t *testing.T) {
		resp, err := srv.client(t, true).Get(srv.url("/"))
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
		_, err := tls.Dial("tcp", srv.addr.String(), &tls.Config{
			RootCAs: srv.pool, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11,
		})
		require.ErrorContains(t, err, "protocol version not supported")
	})

	t.Run("under TLS 1.2", func(t *testing.T) {
		conn, err := tls.Dial("tcp", srv.addr.String(), &tls.Config{
			RootCAs: srv.pool, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12,
		})
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		require.Equal(t, uint16(tls.VersionTLS12), conn.ConnectionState().Version)
	})

	t.Run("not in plain HTTP", func(t *testing.T) {
		resp, err := http.Get("http://" + srv.addr.String() + "/")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	// net/http would answer "OPTIONS *" itself, without the headers.
	t.Run("OPTIONS * is the router's", func(t *testing.T) {
		conn, err := tls.Dial("tcp", srv.addr.String(), &tls.Config{RootCAs: srv.pool, MinVersion: tls.VersionTLS12})
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		_, err = io.WriteString(conn, "OPTIONS * HTTP/1.1\r\nHost: "+srv.addr.String()+"\r\nConnection: close\r\n\r\n")
		require.NoError(t, err)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		for name, want := range everyAnswer {
			require.Equal(t, want, resp.Header.Values(name), name)
		}
		require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	})

	require.NoError(t, srv.stop(t, shutdownTimeout))
	require.Equal(t, "STOPPING=1", <-notified)
}

func TestTheSizeOfTheHeaders(t *testing.T) {
	srv := serve(t, Config{Assets: testAssets()})
	for _, h2 := range []bool{false, true} {
		t.Run(protocol(h2), func(t *testing.T) {
			client := srv.client(t, h2)
			pad := func(n int) *http.Request {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.url("/"), nil)
				require.NoError(t, err)
				req.Header.Set("X-Padding", strings.Repeat("a", n))
				return req
			}

			resp, err := client.Do(pad(32 << 10))
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)

			// An HTTP/2 client does not send what the server announced it does
			// not take.
			resp, err = client.Do(pad(100 << 10))
			if h2 {
				require.ErrorContains(t, err, "larger than peer's advertised limit")
				return
			}
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusRequestHeaderFieldsTooLarge, resp.StatusCode)
		})
	}
}

// A browser with a stream open must not make the stop slow or the unit fail:
// the requests are ended, the connections are drained, and Run returns nil.
func TestStopWithAStreamOpen(t *testing.T) {
	for _, h2 := range []bool{true, false} {
		t.Run(protocol(h2), func(t *testing.T) {
			var buf testutil.SyncBuffer
			m := newTestRoutes()
			srv := serve(t, Config{Assets: testAssets(), Log: zerolog.New(&buf)}, m)
			srv.s.shutdownAfter = 30 * time.Second

			resp, err := srv.client(t, h2).Get(srv.url("/api/stream"))
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			<-m.entered
			line, err := bufio.NewReader(resp.Body).ReadString('\n')
			require.NoError(t, err)
			require.Equal(t, "data: hello\n", line)

			require.NoError(t, srv.stop(t, 10*time.Second))
			require.NotContains(t, buf.String(), "closing")

			// The stream ends for the client as well.
			ended := make(chan struct{})
			go func() { _, _ = io.Copy(io.Discard, resp.Body); close(ended) }()
			select {
			case <-ended:
			case <-time.After(10 * time.Second):
				t.Fatal("the stream is still open at the client")
			}
		})
	}
}

// A handler that does not look at its context holds the stop until the grace
// runs out; then the connections are closed, and the stop is still a clean one.
func TestStopWithAHandlerThatDoesNotEnd(t *testing.T) {
	var buf testutil.SyncBuffer
	m := newTestRoutes()
	srv := serve(t, Config{Assets: testAssets(), Log: zerolog.New(&buf)}, m)
	srv.s.shutdownAfter = 200 * time.Millisecond
	t.Cleanup(func() { close(m.release) })

	failed := make(chan error, 1)
	go func() {
		resp, err := srv.client(t, true).Get(srv.url("/api/stuck"))
		if err == nil {
			_ = resp.Body.Close()
		}
		failed <- err
	}()
	<-m.entered

	require.NoError(t, srv.stop(t, 10*time.Second))
	require.Contains(t, buf.String(), "closing the connections")
	select {
	case err := <-failed:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the request is still open at the client")
	}
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
