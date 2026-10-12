package clusterapi

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
)

const (
	testInstall = "0123456789ab"
	holder      = "pve1"
	testNode    = "pve2"
)

var (
	t0       = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	loopback = netip.MustParseAddr("127.0.0.1")
	epoch1   = agentproto.Epoch{InstallID: testInstall, Generation: 1, Nonce: "n1"}
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeAuth allows the loopback address and takes the node from the
// certificate, unless a test says otherwise.
type fakeAuth struct {
	mu      sync.Mutex
	denied  map[netip.Addr]bool
	joinOK  map[netip.Addr]bool
	refuse  *agentproto.Refusal
	sources []netip.Addr
}

func newFakeAuth() *fakeAuth {
	return &fakeAuth{denied: map[netip.Addr]bool{}, joinOK: map[netip.Addr]bool{loopback: true}}
}

func (f *fakeAuth) Check(cert *x509.Certificate, src netip.Addr) (string, *agentproto.Refusal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sources = append(f.sources, src)
	if f.refuse != nil {
		return "", f.refuse
	}
	id, err := pki.IdentityOf(cert)
	if err != nil {
		return "", &agentproto.Refusal{Code: agentproto.RefusalNotEnrolled, Message: err.Error()}
	}
	return id.Node, nil
}

func (f *fakeAuth) Allowed(src netip.Addr, forJoin bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.denied[src] {
		return false
	}
	if forJoin {
		return f.joinOK[src]
	}
	return true
}

func (f *fakeAuth) deny(src netip.Addr, denied bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denied[src] = denied
}

func (f *fakeAuth) allowJoin(src netip.Addr, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joinOK[src] = ok
}

func (f *fakeAuth) refuseWith(r *agentproto.Refusal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse = r
}

func (f *fakeAuth) firstSource() netip.Addr {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sources[0]
}

// fakeController serves one snapshot per node, which a test publishes, and
// records what it is told.
type fakeController struct {
	mu       sync.Mutex
	snap     agentproto.Snapshot
	seq      uint64
	changed  chan struct{}
	statuses []agentproto.Status
	hb       agentproto.Heartbeat
	join     func(tok pki.JoinToken, csr []byte, src netip.Addr, facts JoinFacts) ([]byte, error)
	renew    func(node string, csr []byte) ([]byte, error)
	renderAs string      // the node every snapshot is rendered for; the caller's when empty
	polled   chan string // the node of every snapshot call, as it begins
}

func (f *fakeController) setHeartbeat(hb agentproto.Heartbeat) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hb = hb
}

func (f *fakeController) setJoin(fn func(tok pki.JoinToken, csr []byte, src netip.Addr, facts JoinFacts) ([]byte, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.join = fn
}

func (f *fakeController) setRenew(fn func(node string, csr []byte) ([]byte, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renew = fn
}

func (f *fakeController) renderFor(node string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renderAs = node
}

func newFakeController() *fakeController {
	return &fakeController{changed: make(chan struct{}), polled: make(chan string, 64)}
}

func (f *fakeController) publish(s agentproto.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap, f.seq = s, s.Seq
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *fakeController) Snapshot(ctx context.Context, node string, epoch agentproto.Epoch, after uint64) (agentproto.Snapshot, error) {
	f.polled <- node
	for {
		f.mu.Lock()
		s, seq, ch, as := f.snap, f.seq, f.changed, f.renderAs
		f.mu.Unlock()
		if seq > 0 && (seq > after || epoch != s.Epoch) {
			s.Node = cmp.Or(as, node)
			return s, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return agentproto.Snapshot{}, ctx.Err()
		}
	}
}

func (f *fakeController) Report(_ context.Context, _ string, st agentproto.Status) (agentproto.Heartbeat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, st)
	hb := f.hb
	hb.EventAck = st.EventCursor
	return hb, nil
}

func (f *fakeController) Join(_ context.Context, tok pki.JoinToken, csr []byte, src netip.Addr, facts JoinFacts) ([]byte, error) {
	f.mu.Lock()
	join := f.join
	f.mu.Unlock()
	if join == nil {
		return nil, errors.New("no join here")
	}
	return join(tok, csr, src, facts)
}

func (f *fakeController) Renew(_ context.Context, node string, csr []byte) ([]byte, error) {
	f.mu.Lock()
	renew := f.renew
	f.mu.Unlock()
	if renew == nil {
		return nil, errors.New("no renewal here")
	}
	return renew(node, csr)
}

func (f *fakeController) reported() []agentproto.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agentproto.Status{}, f.statuses...)
}

type harness struct {
	t      *testing.T
	ca     pki.CA
	clock  *clock
	ctrl   *fakeController
	auth   *fakeAuth
	srv    *Server
	addr   netip.AddrPort
	done   chan error
	events *eventSink
}

// newHarness starts a server whose long-poll ends after 50 ms, unless
// longPoll says otherwise; its events go to h.events.
func newHarness(t *testing.T, longPoll ...time.Duration) *harness {
	t.Helper()
	h := &harness{t: t, clock: &clock{t: t0}, ctrl: newFakeController(), auth: newFakeAuth(), events: &eventSink{}}
	var err error
	h.ca, err = pki.NewCA(testInstall, t0, rand.Reader)
	require.NoError(t, err)
	h.srv = New(h.ctrl, h.auth, h.serverCert(h.ca), h.pool, zerolog.Nop())
	h.srv.now = h.clock.now
	h.srv.longPoll = 50 * time.Millisecond
	if len(longPoll) > 0 {
		h.srv.longPoll = longPoll[0]
	}
	h.srv.OnEvent(h.events.add)
	h.start()
	return h
}

func (h *harness) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(h.ca.Cert)
	return p
}

func (h *harness) serverCert(ca pki.CA) tls.Certificate {
	h.t.Helper()
	key, err := pki.NewKey(rand.Reader)
	require.NoError(h.t, err)
	cert, err := ca.IssueServer(&key.PublicKey, []string{pki.NodeServerName(testInstall, holder)}, h.clock.now(), rand.Reader)
	require.NoError(h.t, err)
	return pki.TLSCertificate(key, cert, ca.Cert)
}

func (h *harness) start() {
	h.t.Helper()
	listening := make(chan netip.AddrPort, 1)
	h.srv.OnListening(func(ap netip.AddrPort) { listening <- ap })
	ctx, cancel := context.WithCancel(context.Background())
	h.done = make(chan error, 1)
	go func() { h.done <- h.srv.Serve(ctx, netip.AddrPortFrom(loopback, 0)) }()
	select {
	case h.addr = <-listening:
	case err := <-h.done:
		h.t.Fatalf("the server ended before it listened: %v", err)
	}
	h.t.Cleanup(func() {
		cancel()
		require.NoError(h.t, <-h.done)
	})
}

// issue makes the client certificate of a node's agent under ca.
func (h *harness) issue(ca pki.CA, id pki.Identity) (*ecdsa.PrivateKey, *x509.Certificate) {
	h.t.Helper()
	key, err := pki.NewKey(rand.Reader)
	require.NoError(h.t, err)
	csr, err := pki.NewCSR(key, id, rand.Reader)
	require.NoError(h.t, err)
	cert, err := ca.IssueClient(csr, id, h.clock.now(), rand.Reader)
	require.NoError(h.t, err)
	return key, cert
}

func hostID(node string) pki.Identity {
	return pki.Identity{Kind: pki.KindHost, InstallID: testInstall, Node: node}
}

// client is an HTTP/2 client of the server with the certificate of node.
func (h *harness) client(ca pki.CA, id pki.Identity) *http.Client {
	key, cert := h.issue(ca, id)
	return h.clientWith(pki.TLSCertificate(key, cert))
}

func (h *harness) clientWith(cert tls.Certificate) *http.Client {
	tr := &http.Transport{
		TLSClientConfig:   pki.ClientTLS(cert, h.ca.SHA256(), pki.NodeServerName(testInstall, holder)),
		ForceAttemptHTTP2: true,
	}
	h.t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// bare is a client that sends no certificate.
func (h *harness) bare() *http.Client { return h.clientWith(tls.Certificate{}) }

func (h *harness) url(path string) string { return "https://" + h.addr.String() + path }

// answer is a response read to its end.
type answer struct {
	StatusCode int
	Header     http.Header
	Body       *bytes.Reader
}

func (h *harness) get(c *http.Client, path string) *answer {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodGet, h.url(path), nil)
	require.NoError(h.t, err)
	req.Header.Set(ProtocolHeader, "1-1")
	return h.do(c, req)
}

func (h *harness) post(c *http.Client, path string, body []byte, headers ...string) *answer {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodPost, h.url(path), bytes.NewReader(body))
	require.NoError(h.t, err)
	req.Header.Set(ProtocolHeader, "1-1")
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return h.do(c, req)
}

func (h *harness) do(c *http.Client, req *http.Request) *answer {
	h.t.Helper()
	resp, err := c.Do(req)
	require.NoError(h.t, err)
	defer func() { require.NoError(h.t, resp.Body.Close()) }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	return &answer{StatusCode: resp.StatusCode, Header: resp.Header, Body: bytes.NewReader(body)}
}

func refusalOf(t *testing.T, resp *answer) agentproto.Refusal {
	t.Helper()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	var r agentproto.Refusal
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&r))
	return r
}

func TestASourceOutsideTheAllowlistGetsNoTLSHandshake(t *testing.T) {
	h := newHarness(t)
	h.auth.deny(loopback, true)

	conn, err := net.Dial("tcp", h.addr.String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	// Nothing is sent: a server that read the ClientHello first would wait
	// for it, and this read would hang.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	n, err := conn.Read(make([]byte, 1))
	require.Equal(t, 0, n)
	require.ErrorIs(t, err, io.EOF)

	c := h.client(h.ca, hostID(testNode))
	_, err = c.Get(h.url("/v1/agent/desired")) //nolint:bodyclose // the dial fails
	require.Error(t, err)
	require.NotContains(t, err.Error(), "tls:", "the connection ends before any handshake")

	h.auth.deny(loopback, false)
	resp := h.get(h.client(h.ca, hostID(testNode)), "/v1/agent/desired?after=0")
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestTheListenerRefusesAWildcardAddress(t *testing.T) {
	s := New(newFakeController(), newFakeAuth(), tls.Certificate{}, x509.NewCertPool, zerolog.Nop())
	err := s.Serve(t.Context(), netip.MustParseAddrPort("0.0.0.0:8644"))
	require.ErrorContains(t, err, "one address")
	err = s.Serve(t.Context(), netip.MustParseAddrPort("[::]:8644"))
	require.ErrorContains(t, err, "one address")
	err = s.Serve(t.Context(), netip.AddrPort{})
	require.ErrorContains(t, err, "one address")
}

func TestARequestWithoutACertificateIsRefused(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/v1/agent/desired", "/v1/agent/status", "/v1/agent/renew"} {
		t.Run(path, func(t *testing.T) {
			var resp *answer
			if path == "/v1/agent/desired" {
				resp = h.get(h.bare(), path)
			} else {
				resp = h.post(h.bare(), path, []byte("{}"))
			}
			r := refusalOf(t, resp)
			require.Equal(t, agentproto.RefusalNotEnrolled, r.Code)
			require.Contains(t, r.Message, "no client certificate")
		})
	}
}

func TestTheRefusalOfTheCheckIsAnswered(t *testing.T) {
	h := newHarness(t)
	for _, code := range []string{agentproto.RefusalRemoved, agentproto.RefusalContested, agentproto.RefusalNotEnrolled, agentproto.RefusalOtherInstall, agentproto.RefusalOtherSource} {
		t.Run(code, func(t *testing.T) {
			h.auth.refuseWith(&agentproto.Refusal{Code: code, Message: "because"})
			resp := h.get(h.client(h.ca, hostID(testNode)), "/v1/agent/desired")
			r := refusalOf(t, resp)
			require.Equal(t, code, r.Code)
			require.Equal(t, "because", r.Message)
		})
	}
	require.Equal(t, loopback, h.auth.firstSource(), "the check is told the source")

	t.Run("a check that could not be made is no refusal", func(t *testing.T) {
		h.auth.refuseWith(&agentproto.Refusal{Message: "the registry could not be read"})
		resp := h.get(h.client(h.ca, hostID(testNode)), "/v1/agent/desired")
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	})
}

func TestACertificateOfAnotherCAFailsTheHandshake(t *testing.T) {
	h := newHarness(t)
	other, err := pki.NewCA(testInstall, t0, rand.Reader)
	require.NoError(t, err)
	_, err = h.client(other, hostID(testNode)).Get(h.url("/v1/agent/desired")) //nolint:bodyclose // the handshake fails
	require.Error(t, err)
}

func TestAnAgentOutsideTheProtocolRangeIsRefusedNamingBothRanges(t *testing.T) {
	h := newHarness(t)
	c := h.client(h.ca, hostID(testNode))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.url("/v1/agent/desired"), nil)
	require.NoError(t, err)
	req.Header.Set(ProtocolHeader, "2-3")
	r := refusalOf(t, h.do(c, req))
	require.Equal(t, agentproto.RefusalOutOfRange, r.Code)
	require.False(t, r.Definitive())
	require.Equal(t, "this agent speaks protocol versions 2 to 3 and this controller 1 to 1: one of the two must be upgraded", r.Message)

	t.Run("no header", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.url("/v1/agent/desired"), nil)
		require.NoError(t, err)
		r := refusalOf(t, h.do(c, req))
		require.Equal(t, agentproto.RefusalOutOfRange, r.Code)
		require.Contains(t, r.Message, "names no protocol version")
	})
	t.Run("a status body of another range", func(t *testing.T) {
		st := agentproto.Status{Protocol: agentproto.Range{Min: 7, Max: 9}, Node: testNode, Kind: pki.KindHost}
		body, err := json.Marshal(st)
		require.NoError(t, err)
		resp := h.post(c, "/v1/agent/status", body)
		r := refusalOf(t, resp)
		require.Equal(t, agentproto.RefusalOutOfRange, r.Code)
		require.Contains(t, r.Message, "7 to 9")
	})
}

func TestTheConnectionsOfANodeAreClosedAndItsLongPollCancelled(t *testing.T) {
	h := newHarness(t, time.Hour)
	c := h.client(h.ca, hostID(testNode))

	answered := make(chan error, 1)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.url("/v1/agent/desired?after=0"), nil)
	require.NoError(t, err)
	req.Header.Set(ProtocolHeader, "1-1")
	go func() {
		resp, err := c.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			err = errors.New("the poll was answered with " + resp.Status)
		}
		answered <- err
	}()
	<-h.ctrl.polled
	h.srv.Close(testNode)
	select {
	case err := <-answered:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the long-poll outlived Close")
	}
	require.Zero(t, h.srv.connections(testNode))

	t.Run("the node connects again", func(t *testing.T) {
		h.ctrl.publish(snapshotOf(1))
		resp := h.get(c, "/v1/agent/desired?after=0")
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})
	t.Run("closing an unknown node is nothing", func(t *testing.T) {
		h.srv.Close("nobody")
	})
}

func TestTheServerCertificateCanBeReplaced(t *testing.T) {
	h := newHarness(t)
	ca2, err := pki.NewCA(testInstall, t0, rand.Reader)
	require.NoError(t, err)
	h.srv.SetCertificate(h.serverCert(ca2))

	_, err = h.client(h.ca, hostID(testNode)).Get(h.url("/v1/agent/desired")) //nolint:bodyclose // pinned to the old CA
	require.Error(t, err)

	tr := &http.Transport{
		TLSClientConfig: pki.ClientTLS(func() tls.Certificate {
			k, c := h.issue(h.ca, hostID(testNode))
			return pki.TLSCertificate(k, c)
		}(), ca2.SHA256(), pki.NodeServerName(testInstall, holder)),
		ForceAttemptHTTP2: true,
	}
	defer tr.CloseIdleConnections()
	resp := h.get(&http.Client{Transport: tr}, "/v1/agent/desired?after=0")
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func snapshotOf(seq uint64) agentproto.Snapshot {
	return agentproto.Snapshot{
		Protocol: agentproto.ProtocolMax, Epoch: epoch1, Seq: seq, Node: testNode, State: agentproto.Active,
		Tunnels: []agentproto.Tunnel{{
			AccountID: "acc", TunnelID: "11111111-1111-4111-8111-111111111111", Name: "pco-" + testInstall,
			Kind: agentproto.TunnelShared, Run: true, Token: agentproto.NewSecret("run-token-of-the-shared-tunnel"),
		}},
		Nodes: []agentproto.Node{}, Targets: []agentproto.Target{}, Blocks: []agentproto.Block{}, Actions: []agentproto.Action{}, Endpoints: []string{},
	}
}
