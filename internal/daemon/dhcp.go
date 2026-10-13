package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/vishvananda/netns"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet/dhcp"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// DHCPServer is the DHCP server of the managed network as the daemon runs
// it; *dhcp.Server is one.
type DHCPServer interface {
	Run(ctx context.Context) error
	Bound() bool
	Stats() dhcp.Stats
}

const (
	// defaultDHCPReadyWait is how long the start waits for the DHCP server
	// of a managed network to listen before it tells systemd that the
	// daemon is ready all the same: the guests that boot with the node ask
	// for their addresses then.
	defaultDHCPReadyWait = 10 * time.Second
	// dhcpCheckEvery is how often the daemon looks at the managed network
	// and its server, and runs a server that ended again; dhcpBoundEvery
	// how often the start looks whether it listens.
	dhcpCheckEvery = 5 * time.Second
	dhcpBoundEvery = 20 * time.Millisecond
)

// errDHCPNotListening is the problem of a server whose sockets are not open
// a while after it started, without an error to say why.
var errDHCPNotListening = errors.New("its sockets are not open")

// dhcpPart runs the DHCP server of the managed network while the network has
// a leg on this node: from the start of the daemon, again whenever it ends or
// its leg changes, until the network goes. It tells the engine how the server
// stands.
type dhcpPart struct {
	networks  func() (map[string]store.Network, error)
	node      string
	newServer func(cfg dhcp.Config) DHCPServer
	note      func(v *engine.DHCPView, problem error)
	now       func() time.Time
	after     func(d time.Duration) <-chan time.Time
	log       zerolog.Logger
	// wait is how long the start waits for the server, and how long a
	// server may take to listen before that is a problem.
	wait time.Duration
	// allowed says whether the server may run; nil is always. The
	// appliance's says no until it proved itself not to be a copy.
	allowed func() bool
	// nsID names the namespace the legs are in now, the one a server opens
	// its sockets in; empty when that cannot be told. A server that runs in
	// another one than the gateway holds now is started again in that.
	nsID func() string

	mu  sync.Mutex
	cur *dhcpRun
	// said is the last end of a server that was logged, so that a leg that
	// stays away is logged once and not every check; before counts what
	// the servers before cur saw.
	said   string
	before dhcp.Stats
}

// dhcpRun is one server with the leg it answers on.
type dhcpRun struct {
	cfg     dhcp.Config
	ns      string // the namespace it was started for
	srv     DHCPServer
	started time.Time
	since   time.Time // when it was first seen listening
	cancel  context.CancelFunc
	done    chan struct{}
	err     error // why Run ended; read once done is closed
	// before is why the server before this one ended, which stands until
	// this one listens.
	before error
}

func (r *dhcpRun) ended() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func (r *dhcpRun) stop() {
	r.cancel()
	<-r.done
}

// newDHCPPart returns the part of a daemon whose gateway is gw; own says
// that the legs live in a namespace of their own, the host's, which must be
// open before a socket is made in it.
func newDHCPPart(st *store.Store, node string, gw *gatewayPart, own bool, leases dhcp.Leases, deps Deps, note func(*engine.DHCPView, error), log zerolog.Logger) *dhcpPart {
	newServer := func(cfg dhcp.Config) DHCPServer {
		return dhcp.New(gw.namespace(own), cfg, leases, log)
	}
	if deps.NewDHCP != nil {
		newServer = func(cfg dhcp.Config) DHCPServer { return deps.NewDHCP(cfg, leases) }
	}
	nsID := func() string {
		var id string
		if err := gw.namespace(own)(func(ns netns.NsHandle) error { id = ns.UniqueId(); return nil }); err != nil {
			return ""
		}
		return id
	}
	return &dhcpPart{
		networks: st.Networks, node: node, newServer: newServer, note: note, now: deps.Now,
		after: time.After, log: log, wait: deps.DHCPReadyWait, nsID: nsID,
	}
}

// namespace holds the namespace of the legs for the DHCP server while it
// opens its sockets there: the gateway does not close or replace it
// meanwhile. In the host's namespace of its own, a handle that is not open
// is refused, never taken for the namespace of the process.
func (g *gatewayPart) namespace(own bool) dhcp.Namespace {
	return func(f func(ns netns.NsHandle) error) error {
		g.mu.Lock()
		defer g.mu.Unlock()
		ns := g.b.Netns()
		if own && !ns.IsOpen() {
			return errors.New("the gateway namespace is not open")
		}
		return f(ns)
	}
}

// wanted is the server the managed network needs on this node: its leg, its
// mark, its address and its subnet; false when it has no leg here.
func (p *dhcpPart) wanted() (dhcp.Config, bool, error) {
	nets, err := p.networks()
	if err != nil {
		return dhcp.Config{}, false, err
	}
	n, ok := nets[gateway.ManagedBridge]
	if !ok || n.Kind != store.NetworkManaged {
		return dhcp.Config{}, false, nil
	}
	addr, ok := n.Legs[p.node]
	if !ok {
		return dhcp.Config{}, false, nil
	}
	return dhcp.Config{
		Iface: gateway.LegIface(n.Mark), Mark: n.Mark, Server: addr, Subnet: n.Prefix.Masked(), LeaseTime: dhcp.DefaultLeaseTime,
	}, true, nil
}

// start runs the part until ctx ends or the function it returns is called,
// which waits for the server to stop.
func (p *dhcpPart) start(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

func (p *dhcpPart) run(ctx context.Context) {
	defer p.stopServer()
	for {
		p.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-p.after(dhcpCheckEvery):
		}
	}
}

// check starts the server the managed network needs, again when it ended or
// its leg changed, stops it when the network went, and tells the engine.
func (p *dhcpPart) check(ctx context.Context) {
	cfg, want, err := p.wanted()
	if err != nil {
		// The store comes back; the server that runs keeps running.
		p.log.Debug().Err(err).Msg("the networks could not be read for the DHCP server")
		p.report()
		return
	}
	p.mu.Lock()
	cur := p.cur
	p.mu.Unlock()
	ns := p.namespaceID()
	switch {
	case !want, p.allowed != nil && !p.allowed():
		p.stopServer()
		p.note(nil, nil)
		return
	case cur == nil:
	case cur.cfg != cfg:
		p.log.Info().Str("leg", cfg.Iface).Msg("the managed leg changed; the DHCP server starts again on it")
		p.stopServer()
		cur = nil
	case cur.ns != "" && ns != "" && cur.ns != ns:
		p.log.Info().Str("leg", cfg.Iface).Msg("the gateway namespace was made again; the DHCP server starts again in it")
		p.stopServer()
		cur = nil
	case cur.ended():
		p.mu.Lock()
		said := p.said
		p.said = cur.err.Error()
		p.mu.Unlock()
		if cur.err.Error() != said {
			p.log.Warn().Err(cur.err).Str("leg", cfg.Iface).Msg("the DHCP server of the managed network stopped; it starts again")
		}
	default:
		p.report()
		return
	}
	p.startServer(ctx, cfg, cur)
	p.report()
}

// startServer runs a new server for cfg; last is the one before it when it
// ended by itself, whose error stands until the new one listens.
func (p *dhcpPart) startServer(ctx context.Context, cfg dhcp.Config, last *dhcpRun) {
	ctx, cancel := context.WithCancel(ctx)
	r := &dhcpRun{cfg: cfg, ns: p.namespaceID(), srv: p.newServer(cfg), started: p.now(), cancel: cancel, done: make(chan struct{})}
	if last != nil && last.ended() {
		r.before = last.err
	}
	p.mu.Lock()
	if last != nil {
		p.before = addStats(p.before, last.srv.Stats())
	}
	p.mu.Unlock()
	go func() {
		err := r.srv.Run(ctx)
		if ctx.Err() == nil && err == nil {
			err = errors.New("it stopped")
		}
		r.err = err
		close(r.done)
	}()
	p.mu.Lock()
	p.cur = r
	p.mu.Unlock()
}

func (p *dhcpPart) stopServer() {
	p.mu.Lock()
	cur := p.cur
	p.cur = nil
	p.mu.Unlock()
	if cur != nil {
		cur.stop()
		p.mu.Lock()
		p.before = addStats(p.before, cur.srv.Stats())
		p.mu.Unlock()
	}
}

// addStats adds what counts across the servers of a part.
func addStats(a, b dhcp.Stats) dhcp.Stats {
	a.Unknown += b.Unknown
	a.Spoofed += b.Spoofed
	return a
}

// namespaceID names the namespace the legs are in now; empty when that
// cannot be told.
func (p *dhcpPart) namespaceID() string {
	if p.nsID == nil {
		return ""
	}
	return p.nsID()
}

// report tells the engine how the server stands: when it listens, since
// when, and the requests of MACs without a lease; or why it does not, once
// it failed or a while after it started.
func (p *dhcpPart) report() {
	p.mu.Lock()
	cur := p.cur
	var view *engine.DHCPView
	var problem error
	if cur != nil {
		bound := cur.srv.Bound() && !cur.ended()
		if bound && cur.since.IsZero() {
			cur.since = p.now()
		}
		st := addStats(p.before, cur.srv.Stats())
		view = &engine.DHCPView{Bound: bound, Unknown: int(st.Unknown), Forged: int(st.Spoofed)}
		if bound {
			view.Since = cur.since
		}
		switch {
		case bound:
			p.said = ""
		case cur.ended():
			problem = cur.err
		case cur.before != nil:
			problem = cur.before
		case p.now().Sub(cur.started) >= p.wait:
			problem = errDHCPNotListening
		}
	}
	p.mu.Unlock()
	if cur == nil {
		return
	}
	p.note(view, problem)
}

// bound says whether the server runs and listens.
func (p *dhcpPart) bound() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur != nil && p.cur.srv.Bound()
}

// ended says whether the server ran and stopped.
func (p *dhcpPart) ended() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur != nil && p.cur.ended()
}

// awaitBound waits, when the managed network has a leg on this node, until
// its server listens, at most p.wait; the guests that boot with the node ask
// for their addresses as soon as the daemon is ready. A server that does
// not listen in time, or that ended, is the engine's problem line, and the
// part keeps trying: an end does not hold the start for the rest of the
// wait, the check says why and starts the server again.
func (p *dhcpPart) awaitBound(ctx context.Context) {
	if _, want, err := p.wanted(); err != nil || !want {
		return
	}
	defer p.report()
	deadline := p.after(p.wait)
	for !p.bound() {
		if p.ended() {
			p.check(ctx)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			p.log.Warn().Dur("waited", p.wait).Msg("the DHCP server of the managed network does not listen yet; the daemon is ready without it")
			return
		case <-time.After(dhcpBoundEvery):
		}
	}
}
