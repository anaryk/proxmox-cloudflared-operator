package clusterapi

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
)

// rotatingPool is a pool a test changes, as a rotation's window does.
type rotatingPool struct {
	mu  sync.Mutex
	cas []*x509.Certificate
}

func (p *rotatingPool) get() *x509.CertPool {
	p.mu.Lock()
	defer p.mu.Unlock()
	pool := x509.NewCertPool()
	for _, ca := range p.cas {
		pool.AddCert(ca)
	}
	return pool
}

func (p *rotatingPool) set(cas ...*x509.Certificate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cas = cas
}

// longPoll starts a long-poll on c and returns where its end is told.
func (h *harness) longPoll(c *http.Client) <-chan error {
	answered := make(chan error, 1)
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodGet, h.url("/v1/agent/desired?after=0"), nil)
	require.NoError(h.t, err)
	req.Header.Set(ProtocolHeader, "1-1")
	go func() {
		resp, err := c.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			err = &statusErr{code: resp.StatusCode}
		}
		answered <- err
	}()
	return answered
}

type statusErr struct{ code int }

func (e *statusErr) Error() string { return http.StatusText(e.code) }

func TestAConnectionUnderThePreviousCAEndsWithItsWindow(t *testing.T) {
	previous, err := pki.NewCA(testInstall, t0, rand.Reader)
	require.NoError(t, err)
	pool := &rotatingPool{}
	h := newHarnessWith(t, func(s *Server) {
		s.longPoll = time.Hour
		s.pool = pool.get
	})
	pool.set(h.ca.Cert, previous.Cert)
	c := h.client(previous, hostID(testNode))
	require.Equal(t, http.StatusOK, h.post(c, "/v1/agent/status", encodeStatus(t, statusOf(0))).StatusCode)

	poll := h.longPoll(c)
	<-h.ctrl.polled
	pool.set(h.ca.Cert)

	resp := h.post(c, "/v1/agent/status", encodeStatus(t, statusOf(0)))
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a request on the open connection is verified again")

	h.srv.sweep()
	select {
	case err := <-poll:
		var answered *statusErr
		require.False(t, errors.As(err, &answered), "the long-poll ends with its connection")
	case <-time.After(10 * time.Second):
		t.Fatal("the long-poll outlived the window of its CA")
	}
	require.Zero(t, h.srv.connections(testNode))

	t.Run("a certificate of the current CA keeps its connection", func(t *testing.T) {
		c := h.client(h.ca, hostID(testNode))
		require.Equal(t, http.StatusOK, h.post(c, "/v1/agent/status", encodeStatus(t, statusOf(0))).StatusCode)
		h.srv.sweep()
		require.Equal(t, 1, h.srv.connections(testNode))
	})
}

func TestAnExpiredCertificateIsRefusedOnItsOpenConnection(t *testing.T) {
	h := newHarness(t)
	key, cert := h.issue(h.ca, hostID(testNode))
	c := h.clientWith(pki.TLSCertificate(key, cert))
	require.Equal(t, http.StatusOK, h.post(c, "/v1/agent/status", encodeStatus(t, statusOf(0))).StatusCode)

	h.clock.advance(cert.NotAfter.Sub(h.clock.now()) + time.Second)
	require.Equal(t, http.StatusUnauthorized, h.post(c, "/v1/agent/status", encodeStatus(t, statusOf(0))).StatusCode)
	h.srv.sweep()
	require.Zero(t, h.srv.connections(testNode))
}

func TestASlowBodyIsCutOff(t *testing.T) {
	h := newHarnessWith(t, func(s *Server) { s.bodyTimeout = 200 * time.Millisecond })
	for path, c := range map[string]*http.Client{
		"/v1/agent/status": h.client(h.ca, hostID(testNode)),
		"/v1/agent/renew":  h.client(h.ca, hostID(testNode)),
		"/v1/agent/join":   h.bare(),
	} {
		t.Run(path, func(t *testing.T) {
			pr, pw := io.Pipe()
			t.Cleanup(func() { _ = pw.Close() })
			go func() { _, _ = pw.Write([]byte(`{"node":`)) }()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.url(path), pr)
			require.NoError(t, err)
			req.Header.Set(ProtocolHeader, "1-1")
			start := time.Now()
			resp, err := c.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.NotEqual(t, http.StatusOK, resp.StatusCode)
			require.Less(t, time.Since(start), 5*time.Second, "the handler gave up on the body")
		})
	}
}

func TestOneSourceHoldsABoundedNumberOfConnections(t *testing.T) {
	h := newHarnessWith(t, func(s *Server) { s.perSource = 2 })
	dial := func() net.Conn {
		c, err := net.Dial("tcp", h.addr.String())
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	closedAtOnce := func(c net.Conn) bool {
		require.NoError(t, c.SetReadDeadline(time.Now().Add(500*time.Millisecond)))
		_, err := c.Read(make([]byte, 1))
		var ne net.Error
		return !errors.As(err, &ne) || !ne.Timeout()
	}
	first, second := dial(), dial()
	_ = second
	require.True(t, closedAtOnce(dial()), "the third connection of one source is closed")

	require.NoError(t, first.Close())
	require.Eventually(t, func() bool { return !closedAtOnce(dial()) }, 10*time.Second, 50*time.Millisecond,
		"a closed connection gives its place back")
}

func TestANodeHoldsAtMostTwoLongPolls(t *testing.T) {
	h := newHarness(t, time.Hour)
	c := h.client(h.ca, hostID(testNode))
	oldest := h.longPoll(c)
	<-h.ctrl.polled
	second := h.longPoll(c)
	<-h.ctrl.polled
	third := h.longPoll(c)
	<-h.ctrl.polled

	select {
	case err := <-oldest:
		var answered *statusErr
		require.ErrorAs(t, err, &answered)
		require.Equal(t, http.StatusConflict, answered.code)
	case <-time.After(10 * time.Second):
		t.Fatal("the oldest long-poll was not answered")
	}
	h.ctrl.publish(snapshotOf(1))
	for _, poll := range []<-chan error{second, third} {
		err := <-poll
		var answered *statusErr
		require.ErrorAs(t, err, &answered)
		require.Equal(t, http.StatusOK, answered.code)
	}
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	require.Empty(t, h.srv.nodePolls, "every poll that ended is forgotten")
}

func TestAConnectionHoldsAtMostEightStreams(t *testing.T) {
	h := newHarness(t)
	require.Equal(t, uint32(8), uint32(h.srv.httpServer().HTTP2.MaxConcurrentStreams))
}

func TestARenewalNeedsAP256Key(t *testing.T) {
	h := newHarness(t)
	h.ctrl.setRenew(func(string, []byte) ([]byte, error) {
		t.Fatal("the controller was asked")
		return nil, nil
	})
	c := h.client(h.ca, hostID(testNode))
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{hostID(testNode).URI()}}, key)
	require.NoError(t, err)
	body, err := json.Marshal(RenewRequest{CSR: csr})
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, h.post(c, "/v1/agent/renew", body).StatusCode)
}
