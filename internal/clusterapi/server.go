// Package clusterapi is the controller's endpoint for the agents of the other
// nodes: one HTTPS listener on one address, mutual TLS under the install's
// own CA, and four calls. An agent long-polls its desired state, reports its
// status and gets the controller's heartbeat back, renews its certificate,
// and an appliance agent joins with a token before it has a certificate.
//
// The listener drops a connection whose source is not a registered node or
// agent before any TLS handshake, every authenticated request is checked
// against the registry, and what the controller will not serve is refused
// with a code the agent reads: a definitive one stands it down, the others
// it waits out.
package clusterapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	stdlog "log"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
)

const (
	// Port is the one the controller listens on, on its agent address.
	Port = 8644
	// LongPoll is how long a poll for desired state waits for a new seq.
	LongPoll = 30 * time.Second
	// ProtocolHeader carries the versions of the agent protocol an agent
	// speaks, as "<min>-<max>", on every request but the join.
	ProtocolHeader = "Pco-Agent-Protocol"

	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 5 * time.Second
	writeSlack        = 30 * time.Second
	idleTimeout       = 2 * time.Minute
	maxHeaderBytes    = 64 << 10
	// bodyTimeout bounds the reading of a request body, so that a peer that
	// sends its headers and then dribbles holds no handler.
	bodyTimeout = 15 * time.Second
	// maxStreams is how many requests one connection may have open at once:
	// an agent needs a long-poll, a status and now and then a renewal.
	maxStreams = 8
	// maxPollsPerNode is how many long-polls of one node wait at once; a
	// newer one answers the oldest with 409.
	maxPollsPerNode = 2
	// sweepEvery is how often the certificates of the open connections are
	// verified again, so that one that expired, or whose CA left the pool
	// at the end of a rotation, does not keep its connection.
	sweepEvery = 10 * time.Second
)

// JoinFacts is what an appliance agent says of itself when it joins. The
// controller checks them against Proxmox before it enrols the node.
type JoinFacts struct {
	Node     string
	VMID     int
	MACs     []string
	Build    string
	Protocol agentproto.Range
}

// Controller is what the endpoint asks of the controller.
type Controller interface {
	// Snapshot returns node's current snapshot. It waits until the node's
	// seq in epoch is above after, or returns at once when the controller
	// serves another epoch, and ends with ctx. An agent that follows no epoch
	// yet passes a zero one.
	Snapshot(ctx context.Context, node string, epoch agentproto.Epoch, after uint64) (agentproto.Snapshot, error)
	// Report takes a status of node and answers with the heartbeat.
	Report(ctx context.Context, node string, st agentproto.Status) (agentproto.Heartbeat, error)
	// Join enrols an appliance agent that used tok from src and returns its
	// client certificate in PEM. The endpoint has checked the token's form,
	// install, CA, expiry and node, that csr asks for the identity of that
	// node and container, and recorded the use, which was the token's first;
	// the controller attests the facts and issues. Any error is answered as a
	// plain refusal, and so is a certificate for another key or identity.
	Join(ctx context.Context, tok pki.JoinToken, csr []byte, src netip.Addr, facts JoinFacts) ([]byte, error)
	// Renew issues node's next client certificate for csr, in PEM.
	Renew(ctx context.Context, node string, csr []byte) ([]byte, error)
}

// Auth decides who may connect and whom a certificate speaks for.
type Auth interface {
	// Check returns the node a client certificate speaks for, or a refusal.
	// A refusal without a code is a check that could not be made: the
	// request is answered as a temporary failure, not as a refusal.
	Check(cert *x509.Certificate, src netip.Addr) (node string, refusal *agentproto.Refusal)
	// Allowed reports whether a connection from src is taken at all: as a
	// registered node or agent, or, forJoin, as a container that may join.
	Allowed(src netip.Addr, forJoin bool) bool
}

// Server is the endpoint. One Serve runs at a time.
type Server struct {
	c    Controller
	a    Auth
	pool func() *x509.CertPool
	log  zerolog.Logger
	cert atomic.Pointer[tls.Certificate]
	now  func() time.Time

	longPoll    time.Duration
	bodyTimeout time.Duration
	sweepEvery  time.Duration
	perSource   int
	joinCfg     *Joins
	joinMu      sync.Mutex // one use of a join token at a time
	joins       *rateLimiter
	resend      *resendCounter
	onListening func(netip.AddrPort)
	event       func(node, msg string)

	mu        sync.Mutex
	conns     map[net.Conn]*connInfo
	nodePolls map[string][]*poll // oldest first
}

// New returns the endpoint of a controller. cert is its server certificate
// followed by the CA, and pool gives the CAs client certificates are verified
// under, read at every handshake so that a rotation takes at once.
func New(c Controller, a Auth, cert tls.Certificate, pool func() *x509.CertPool, log zerolog.Logger) *Server {
	s := &Server{
		c: c, a: a, pool: pool, log: log, now: time.Now, longPoll: LongPoll,
		bodyTimeout: bodyTimeout, sweepEvery: sweepEvery, perSource: maxPerSource,
		joins:     newRateLimiter(joinPerMinute, time.Minute),
		resend:    newResendCounter(),
		conns:     map[net.Conn]*connInfo{},
		nodePolls: map[string][]*poll{},
	}
	s.cert.Store(&cert)
	return s
}

// OnListening sets what Serve calls, once, with the address it listens on.
// It is set before Serve.
func (s *Server) OnListening(fn func(netip.AddrPort)) { s.onListening = fn }

// OnEvent sets what is told of a request refused for its size, with the node
// it came from. It is set before Serve.
func (s *Server) OnEvent(fn func(node, msg string)) { s.event = fn }

// SetCertificate replaces the server certificate for the connections that
// come after, as a CA rotation needs.
func (s *Server) SetCertificate(cert tls.Certificate) { s.cert.Store(&cert) }

// Serve listens on addr, one address and never a wildcard, until ctx ends. A
// clean shutdown returns nil.
func (s *Server) Serve(ctx context.Context, addr netip.AddrPort) error {
	if !addr.IsValid() || addr.Addr().IsUnspecified() {
		return fmt.Errorf("the agent endpoint listens on one address, never a wildcard: %s is none", addr)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr.String())
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	srv := s.httpServer()
	if s.onListening != nil {
		if ap, err := netip.ParseAddrPort(ln.Addr().String()); err == nil {
			s.onListening(ap)
		}
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(tls.NewListener(newAllowListener(ln, s), s.tlsConfig())) }()
	sweep := time.NewTicker(s.sweepEvery)
	defer sweep.Stop()
	for running := true; running; {
		select {
		case err := <-done:
			return fmt.Errorf("serving the agents on %s: %w", addr, err)
		case <-ctx.Done():
			running = false
		case <-sweep.C:
			s.sweep()
		}
	}
	stop, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(stop); err != nil {
		_ = srv.Close()
	}
	<-done
	return nil
}

// httpServer is the server of the endpoint, without its listener.
func (s *Server) httpServer() *http.Server {
	return &http.Server{
		Handler:           s.routes(),
		ConnContext:       s.connContext,
		ConnState:         s.connState,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      s.longPoll + writeSlack,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		HTTP2:             &http.HTTP2Config{MaxConcurrentStreams: maxStreams},
		ErrorLog:          stdlog.New(httpErrorLog{s.log}, "", 0),
	}
}

// tlsConfig is the listener's: the configuration of the connection is made
// at each handshake, with the pool and the certificate of that moment.
func (s *Server) tlsConfig() *tls.Config {
	cfg := s.connTLS()
	cfg.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) { return s.connTLS(), nil }
	return cfg
}

func (s *Server) connTLS() *tls.Config {
	cfg := pki.ServerTLS(*s.cert.Load(), s.pool())
	cfg.NextProtos = []string{"h2", "http/1.1"}
	return cfg
}

// routes is the whole endpoint.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agent/desired", s.desired)
	mux.HandleFunc("POST /v1/agent/status", s.status)
	mux.HandleFunc("POST /v1/agent/join", s.join)
	mux.HandleFunc("POST /v1/agent/renew", s.renew)
	return mux
}

// allowListener closes a connection whose source the allowlist does not
// hold, before anything of it is read: no TLS handshake begins for it. Each
// source is checked beside the accepting, since a miss may make the
// allowlist read its sources again, and at most maxChecks at once. One
// source holds at most perSource connections open.
type allowListener struct {
	net.Listener
	s      *Server
	conns  chan net.Conn
	errs   chan error
	done   chan struct{}
	once   sync.Once
	checks chan struct{}

	mu   sync.Mutex
	open map[netip.Addr]int
}

func newAllowListener(ln net.Listener, s *Server) *allowListener {
	l := &allowListener{
		Listener: ln, s: s, conns: make(chan net.Conn), errs: make(chan error),
		done: make(chan struct{}), checks: make(chan struct{}, maxChecks),
		open: map[netip.Addr]int{},
	}
	go l.run()
	return l
}

func (l *allowListener) run() {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			select {
			case l.errs <- err:
			case <-l.done:
				return
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		src := addrOf(c.RemoteAddr())
		if !l.take(src) {
			_ = c.Close()
			l.s.log.Debug().Str("source", src.String()).Msg("dropped a connection from a source that holds too many")
			continue
		}
		c = &countedConn{Conn: c, release: func() { l.give(src) }}
		select {
		case l.checks <- struct{}{}:
			go l.check(c)
		default:
			_ = c.Close()
		}
	}
}

// take counts a connection of src, unless it holds perSource already.
func (l *allowListener) take(src netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open[src] >= l.s.perSource {
		return false
	}
	l.open[src]++
	return true
}

func (l *allowListener) give(src netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open[src]--; l.open[src] <= 0 {
		delete(l.open, src)
	}
}

// countedConn gives its place back once, when it is closed.
type countedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *countedConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}

func (l *allowListener) check(c net.Conn) {
	defer func() { <-l.checks }()
	src := addrOf(c.RemoteAddr())
	if !l.s.a.Allowed(src, false) && !l.s.a.Allowed(src, true) {
		_ = c.Close()
		l.s.log.Debug().Str("source", src.String()).Msg("dropped a connection from a source that is no node or agent")
		return
	}
	select {
	case l.conns <- c:
	case <-l.done:
		_ = c.Close()
	}
}

func (l *allowListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case err := <-l.errs:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *allowListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

func addrOf(a net.Addr) netip.Addr {
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

// connInfo is what the server keeps of one connection: whose it is, once a
// request of it was authenticated, and the long-polls that run on it.
type connInfo struct {
	conn  net.Conn
	mu    sync.Mutex
	node  string
	cert  *x509.Certificate
	polls map[*poll]struct{}
}

// poll is a waiting long-poll. superseded says that a newer poll of its
// node cancelled it.
type poll struct {
	cancel     context.CancelFunc
	superseded atomic.Bool
}

type connKey struct{}

func (s *Server) connContext(ctx context.Context, c net.Conn) context.Context {
	info := &connInfo{conn: c, polls: map[*poll]struct{}{}}
	s.mu.Lock()
	s.conns[c] = info
	s.mu.Unlock()
	return context.WithValue(ctx, connKey{}, info)
}

func (s *Server) connState(c net.Conn, state http.ConnState) {
	if state != http.StateClosed {
		return
	}
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func connInfoOf(ctx context.Context) *connInfo {
	info, _ := ctx.Value(connKey{}).(*connInfo)
	return info
}

func (i *connInfo) bind(node string, cert *x509.Certificate) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.node, i.cert = node, cert
}

func (i *connInfo) addPoll(p *poll) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.polls[p] = struct{}{}
}

func (i *connInfo) removePoll(p *poll) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.polls, p)
}

// startPoll counts a long-poll of node, and supersedes the oldest of the
// node's when more than maxPollsPerNode would wait. The returned func ends
// it.
func (s *Server) startPoll(node string, p *poll) func() {
	s.mu.Lock()
	s.nodePolls[node] = append(s.nodePolls[node], p)
	polls := s.nodePolls[node]
	for len(polls) > maxPollsPerNode {
		polls[0].superseded.Store(true)
		polls[0].cancel()
		polls = polls[1:]
	}
	s.nodePolls[node] = polls
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		polls := slices.DeleteFunc(s.nodePolls[node], func(q *poll) bool { return q == p })
		if len(polls) == 0 {
			delete(s.nodePolls, node)
		} else {
			s.nodePolls[node] = polls
		}
	}
}

// Close closes the connections of a node and cancels its long-polls. A node
// that was forgotten is refused from its next request; this ends the ones
// that are open, before anything is rotated.
func (s *Server) Close(node string) {
	s.closeWhere(func(info *connInfo) bool { return info.node == node })
}

// sweep closes the connections whose certificate no longer verifies under
// the pool of now: it expired, or its CA left the pool when a rotation's
// window ended.
func (s *Server) sweep() {
	pool, now := s.pool(), s.now()
	s.closeWhere(func(info *connInfo) bool {
		if info.cert == nil {
			return false
		}
		if err := verifyClient(info.cert, pool, now); err != nil {
			s.log.Info().Str("node", info.node).Err(err).Msg("closed a connection whose certificate no longer verifies")
			return true
		}
		return false
	})
}

// closeWhere closes the connections that match, with their long-polls.
// match runs under the connection's lock.
func (s *Server) closeWhere(match func(*connInfo) bool) {
	s.mu.Lock()
	var infos []*connInfo
	for c, info := range s.conns {
		info.mu.Lock()
		if match(info) {
			infos = append(infos, info)
			delete(s.conns, c)
		}
		info.mu.Unlock()
	}
	s.mu.Unlock()
	for _, info := range infos {
		info.mu.Lock()
		for p := range info.polls {
			p.cancel()
		}
		info.mu.Unlock()
		_ = info.conn.Close()
	}
}

// peer is the authenticated agent of a request.
type peer struct {
	cert *x509.Certificate
	id   pki.Identity
	node string
	src  netip.Addr
}

// authenticate finds whom a request speaks for and that its protocol is one
// this build speaks; it answers the request itself when it does not.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (peer, bool) {
	src := sourceOf(r)
	if _, err := pki.PeerIdentity(r.TLS); err != nil {
		s.log.Warn().Str("source", src.String()).Str("path", r.URL.Path).Msg("refused a request without a client certificate")
		writeJSON(w, http.StatusForbidden, agentproto.Refusal{Code: agentproto.RefusalNotEnrolled, Message: "no client certificate was verified under the install's CA"})
		return peer{}, false
	}
	cert := r.TLS.VerifiedChains[0][0]
	// The handshake verified the certificate when the connection began;
	// the connection may be days old, and the CA it was verified under may
	// have left the pool since.
	if err := verifyClient(cert, s.pool(), s.now()); err != nil {
		s.log.Warn().Str("source", src.String()).Err(err).Msg("a client certificate no longer verifies")
		http.Error(w, "the client certificate does not verify under the install's CA now", http.StatusUnauthorized)
		return peer{}, false
	}
	node, ref := s.a.Check(cert, src)
	if ref != nil {
		s.refuse(w, r, node, *ref)
		return peer{}, false
	}
	id, err := pki.IdentityOf(cert)
	if err != nil {
		s.refuse(w, r, node, agentproto.Refusal{Code: agentproto.RefusalNotEnrolled, Message: err.Error()})
		return peer{}, false
	}
	if info := connInfoOf(r.Context()); info != nil {
		info.bind(node, cert)
	}
	if ref := protocolRefusal(r.Header.Get(ProtocolHeader)); ref != nil {
		s.refuse(w, r, node, *ref)
		return peer{}, false
	}
	return peer{cert: cert, id: id, node: node, src: src}, true
}

// verifyClient verifies a client certificate under pool at now, for client
// authentication.
func verifyClient(cert *x509.Certificate, pool *x509.CertPool, now time.Time) error {
	_, err := cert.Verify(x509.VerifyOptions{
		Roots: pool, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	return err
}

// readDeadline bounds the reading of the request's body from now.
func (s *Server) readDeadline(w http.ResponseWriter) {
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(s.bodyTimeout)); err != nil {
		s.log.Debug().Err(err).Msg("no read deadline for a request body")
	}
}

// protocolRefusal reads the range of the header and refuses one this build
// shares no version with.
func protocolRefusal(header string) *agentproto.Refusal {
	lo, hi, ok := strings.Cut(header, "-")
	var agent agentproto.Range
	if ok {
		_, errLo := fmt.Sscan(lo, &agent.Min)
		_, errHi := fmt.Sscan(hi, &agent.Max)
		ok = errLo == nil && errHi == nil
	}
	if !ok {
		return &agentproto.Refusal{Code: agentproto.RefusalOutOfRange, Message: fmt.Sprintf(
			"the request names no protocol version this controller can read (%s %q); it speaks %d to %d", ProtocolHeader, header, agentproto.ProtocolMin, agentproto.ProtocolMax)}
	}
	return rangeRefusal(agent)
}

func rangeRefusal(agent agentproto.Range) *agentproto.Refusal {
	ours := agentproto.Range{Min: agentproto.ProtocolMin, Max: agentproto.ProtocolMax}
	if _, ok := agentproto.Negotiate(agent, ours); ok {
		return nil
	}
	return &agentproto.Refusal{Code: agentproto.RefusalOutOfRange, Message: fmt.Sprintf(
		"this agent speaks protocol versions %d to %d and this controller %d to %d: one of the two must be upgraded", agent.Min, agent.Max, ours.Min, ours.Max)}
}

func sourceOf(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

// refusalMessages are what an agent is told of each refusal. The reason in
// detail, which names nodes, addresses and serials of the registry, goes to
// the controller's log only.
var refusalMessages = map[string]string{
	agentproto.RefusalRemoved:      "this node was removed from the install",
	agentproto.RefusalContested:    "this node is contested; an admin decides with pco node admit or pco node forget",
	agentproto.RefusalNotEnrolled:  "this certificate is not the one the install enrolled for this node",
	agentproto.RefusalOtherInstall: "this certificate is of another install",
	agentproto.RefusalOtherSource:  "this request came from an address that is not one of this node's",
}

// refuse answers a request the controller will not serve. A refusal without
// a code is a check that could not be made.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, node string, ref agentproto.Refusal) {
	src := sourceOf(r)
	if ref.Code == "" {
		s.log.Error().Str("node", node).Str("source", src.String()).Msg(ref.Message)
		http.Error(w, "the request could not be checked; try again", http.StatusServiceUnavailable)
		return
	}
	s.log.Warn().Str("node", node).Str("source", src.String()).Str("code", ref.Code).Str("path", r.URL.Path).Msg(ref.Message)
	// The protocol ranges are no secret, and the agent's admin needs both.
	if msg, ok := refusalMessages[ref.Code]; ok {
		ref.Message = msg
	} else if ref.Code != agentproto.RefusalOutOfRange {
		ref.Message = "refused"
	}
	writeJSON(w, http.StatusForbidden, ref)
}

// badRequest answers a request that is not what it should be, with the
// detail in the log only.
func (s *Server) badRequest(w http.ResponseWriter, node, what string, err error) {
	s.log.Warn().Str("node", node).Err(err).Msg(what)
	http.Error(w, what, http.StatusBadRequest)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fail answers a request the controller could not serve.
func (s *Server) fail(w http.ResponseWriter, node, what string, err error) {
	s.log.Error().Err(err).Str("node", node).Msg(what)
	http.Error(w, what, http.StatusServiceUnavailable)
}

// record tells of a request refused for its size, so that the event log
// shows an agent that sends more than it may.
func (s *Server) record(node, msg string) {
	s.log.Warn().Str("node", node).Msg(msg)
	if s.event != nil {
		s.event(node, msg)
	}
}

// httpErrorLog carries what net/http would print to the standard logger
// into the daemon's log.
type httpErrorLog struct{ log zerolog.Logger }

func (w httpErrorLog) Write(p []byte) (int, error) {
	w.log.Warn().Str("component", "clusterapi").Msg(strings.TrimSpace(string(p)))
	return len(p), nil
}
