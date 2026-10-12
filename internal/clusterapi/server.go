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
	// expiry and node; the controller records the use, judges a second one,
	// and attests the facts. Any error is answered as a plain refusal.
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
	joins       *rateLimiter
	resend      *resendCounter
	onListening func(netip.AddrPort)
	event       func(node, msg string)

	mu    sync.Mutex
	conns map[net.Conn]*connInfo
}

// New returns the endpoint of a controller. cert is its server certificate
// followed by the CA, and pool gives the CAs client certificates are verified
// under, read at every handshake so that a rotation takes at once.
func New(c Controller, a Auth, cert tls.Certificate, pool func() *x509.CertPool, log zerolog.Logger) *Server {
	s := &Server{
		c: c, a: a, pool: pool, log: log, now: time.Now, longPoll: LongPoll,
		joins:  newRateLimiter(joinPerMinute, time.Minute),
		resend: newResendCounter(),
		conns:  map[net.Conn]*connInfo{},
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
	srv := &http.Server{
		Handler:           s.routes(),
		ConnContext:       s.connContext,
		ConnState:         s.connState,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      s.longPoll + writeSlack,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          stdlog.New(httpErrorLog{s.log}, "", 0),
	}
	if s.onListening != nil {
		if ap, err := netip.ParseAddrPort(ln.Addr().String()); err == nil {
			s.onListening(ap)
		}
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(tls.NewListener(newAllowListener(ln, s), s.tlsConfig())) }()
	select {
	case err := <-done:
		return fmt.Errorf("serving the agents on %s: %w", addr, err)
	case <-ctx.Done():
	}
	stop, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(stop); err != nil {
		_ = srv.Close()
	}
	<-done
	return nil
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
// allowlist read its sources again, and at most maxChecks at once.
type allowListener struct {
	net.Listener
	s      *Server
	conns  chan net.Conn
	errs   chan error
	done   chan struct{}
	once   sync.Once
	checks chan struct{}
}

func newAllowListener(ln net.Listener, s *Server) *allowListener {
	l := &allowListener{
		Listener: ln, s: s, conns: make(chan net.Conn), errs: make(chan error),
		done: make(chan struct{}), checks: make(chan struct{}, maxChecks),
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
		select {
		case l.checks <- struct{}{}:
			go l.check(c)
		default:
			_ = c.Close()
		}
	}
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
	polls map[*poll]struct{}
}

type poll struct{ cancel context.CancelFunc }

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

func (i *connInfo) bind(node string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.node = node
}

func (i *connInfo) addPoll(cancel context.CancelFunc) *poll {
	p := &poll{cancel: cancel}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.polls[p] = struct{}{}
	return p
}

func (i *connInfo) removePoll(p *poll) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.polls, p)
}

// Close closes the connections of a node and cancels its long-polls. A node
// that was forgotten is refused from its next request; this ends the ones
// that are open, before anything is rotated.
func (s *Server) Close(node string) {
	s.mu.Lock()
	var infos []*connInfo
	for c, info := range s.conns {
		info.mu.Lock()
		if info.node == node {
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

// connections is how many open connections a node has.
func (s *Server) connections(node string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, info := range s.conns {
		info.mu.Lock()
		if info.node == node {
			n++
		}
		info.mu.Unlock()
	}
	return n
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
		s.refuse(w, r, "", agentproto.Refusal{Code: agentproto.RefusalNotEnrolled, Message: "no client certificate was verified under the install's CA"})
		return peer{}, false
	}
	cert := r.TLS.VerifiedChains[0][0]
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
		info.bind(node)
	}
	if ref := protocolRefusal(r.Header.Get(ProtocolHeader)); ref != nil {
		s.refuse(w, r, node, *ref)
		return peer{}, false
	}
	return peer{cert: cert, id: id, node: node, src: src}, true
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

// refuse answers a request the controller will not serve. A refusal without
// a code is a check that could not be made.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, node string, ref agentproto.Refusal) {
	src := sourceOf(r)
	if ref.Code == "" {
		s.log.Error().Str("node", node).Str("source", src.String()).Msg(ref.Message)
		http.Error(w, "the request could not be checked: "+ref.Message, http.StatusServiceUnavailable)
		return
	}
	s.log.Warn().Str("node", node).Str("source", src.String()).Str("code", ref.Code).Str("path", r.URL.Path).Msg(ref.Message)
	writeJSON(w, http.StatusForbidden, ref)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fail answers a request the controller could not serve.
func (s *Server) fail(w http.ResponseWriter, node, what string, err error) {
	s.log.Error().Err(err).Str("node", node).Msg(what)
	http.Error(w, what+": "+err.Error(), http.StatusServiceUnavailable)
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
