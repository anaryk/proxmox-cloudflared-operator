package daemon

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/clusterapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// clusterEvery is how often the daemon looks whether it runs in a cluster
// and whether this node holds the lease.
const clusterEvery = 2 * time.Second

// ClusterDeps are the parts of the cluster mode that a test replaces. The
// zero value is what the daemon uses on a node.
type ClusterDeps struct {
	// Port is the port of the agent endpoint; default 8644. Every is how
	// often the daemon looks again; default 2 s.
	Port  uint16
	Every time.Duration
}

// leaseView is what the cluster mode asks of the lease keeper.
type leaseView interface {
	State() lease.State
	Gate(ctx context.Context) (planner.Writer, error)
}

// clusterPart runs what a node of a cluster runs besides the engine: on
// every node the agent's client, which follows the controller where the
// lease is and serves this node's agent its desired state; on the node that
// holds the lease the agent endpoint, which serves the others. Neither runs
// while the registry names this node alone: a single node runs as before.
// It also runs the first start of this release once this node may write.
type clusterPart struct {
	node   string
	st     *store.Store
	paths  store.Paths
	pveDir string
	keeper leaseView
	eng    *engine.Engine
	remote *RemoteFleet
	quorum func() members.Members
	first  *firstStart
	events *agent.EventLog
	port   uint16
	every  time.Duration
	now    func() time.Time
	log    zerolog.Logger

	mu        sync.Mutex
	listening *clusterRun
	following *clusterRun
	client    *agent.Client
	server    *clusterapi.Server
	allow     *clusterapi.Allowlist
	observe   observeWindow
	// multiPVE is the client of the Proxmox API, whose fallbacks follow the
	// members file.
	multiPVE *pve.Multi
	// doctor makes the checks of this node that the controller asks its
	// agent for.
	doctor agentDoctor
}

// clusterRun is a part that runs until stopped.
type clusterRun struct {
	stop context.CancelFunc
	done chan struct{}
}

func start(ctx context.Context, run func(ctx context.Context)) *clusterRun {
	ctx, stop := context.WithCancel(ctx)
	r := &clusterRun{stop: stop, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		run(ctx)
	}()
	return r
}

func (r *clusterRun) end() {
	if r != nil {
		r.stop()
		<-r.done
	}
}

// run looks every few seconds until ctx ends, and stops what it started.
func (c *clusterPart) run(ctx context.Context) {
	defer c.stopAll()
	t := time.NewTicker(c.every)
	defer t.Stop()
	for {
		c.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *clusterPart) stopAll() {
	c.mu.Lock()
	l, f := c.listening, c.following
	c.listening, c.following, c.client, c.server = nil, nil, nil, nil
	c.mu.Unlock()
	l.end()
	f.end()
}

// tick does the first start where it is due, and starts or stops the client
// and the endpoint as the registry and the lease say.
func (c *clusterPart) tick(ctx context.Context) {
	c.first.try(func() error {
		if !c.keeper.State().Holder {
			return errors.New("this node does not hold the lease")
		}
		_, err := c.keeper.Gate(ctx)
		return err
	})
	// What this node is, and whether it serves the others, is read anew at
	// every tick: a registry that cannot be read serves nobody, and the
	// endpoint's checks read the registry and the enrolments again at every
	// request.
	multi, err := c.clustered()
	if c.multiPVE != nil && c.quorum != nil {
		followMembers(c.multiPVE, c.node, c.quorum(), &c.log)
	}
	st := c.keeper.State()
	holder := st.Holder && st.Valid && err == nil
	if multi {
		c.observe.note(c.st, c.eng.State().Mode == engine.ModeObserve, holder, c.now(), c.log)
	}

	c.mu.Lock()
	following, listening := c.following, c.listening
	c.mu.Unlock()

	switch {
	case err != nil:
		c.log.Warn().Err(err).Msg("the node registry cannot be read; this node serves no agent until it can")
	case multi && following == nil:
		c.startClient(ctx)
	case !multi && following != nil:
		c.stopPart(&c.following)
	}
	switch {
	case multi && holder && listening == nil:
		c.startListener(ctx)
	case (!multi || !holder) && listening != nil:
		c.stopPart(&c.listening)
	case listening != nil:
		c.refreshListener(ctx)
	}
}

func (c *clusterPart) stopPart(r **clusterRun) {
	c.mu.Lock()
	old := *r
	*r = nil
	if r == &c.listening {
		c.server, c.allow = nil, nil
		// What a closed endpoint served is not served again by a later one
		// until the engine renders it anew.
		c.remote.Serve(nil)
	} else {
		c.client = nil
	}
	c.mu.Unlock()
	old.end()
}

// clustered says whether the registry names a node besides this one that
// was not removed.
func (c *clusterPart) clustered() (bool, error) {
	nodes, err := c.st.Nodes()
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(nodes, func(n store.NodeEntry) bool {
		return n.Name != c.node && n.State != store.NodeRemoved
	}), nil
}

// startClient starts the client of this node's agent, once the agent is
// made: it follows the controller where leader.json names it, and waits
// while that is this node.
func (c *clusterPart) startClient(ctx context.Context) {
	a := c.eng.Agent()
	inst, found, err := c.st.Install()
	if a == nil || err != nil || !found {
		return
	}
	files := agent.TLSFiles{Dir: agent.TLSDir(c.paths)}
	cl, err := agent.NewClient(a, agent.ClientDeps{
		Discover: agent.HostDiscovery{Node: c.node, InstallID: inst.ID, Store: c.st, Port: c.port},
		Files:    files,
		Reissue:  &agent.Reissuer{Node: c.node, InstallID: inst.ID, Store: c.st, Files: files, Now: c.now, Log: c.log},
		Events:   c.events,
	})
	if err != nil {
		c.log.Warn().Err(err).Msg("the agent of this node cannot follow the controller")
		return
	}
	r := start(ctx, func(ctx context.Context) { _ = cl.Run(ctx) })
	c.mu.Lock()
	c.following, c.client = r, cl
	c.mu.Unlock()
	c.log.Info().Msg("the agent of this node follows the controller of the cluster")
}

// startListener opens the agent endpoint on this node's agent address.
func (c *clusterPart) startListener(ctx context.Context) {
	inst, found, err := c.st.Install()
	if err != nil || !found {
		return
	}
	addr, err := c.agentAddress()
	if err != nil {
		c.log.Warn().Err(err).Msg("the agent endpoint cannot listen")
		return
	}
	creds, err := agent.TLSFiles{Dir: agent.TLSDir(c.paths)}.Server()
	if err != nil {
		c.log.Warn().Err(err).Msg("the agent endpoint has no server certificate yet")
		return
	}
	allow := clusterapi.NewAllowlist(c.sources, c.now, c.log)
	if err := allow.Refresh(ctx); err != nil {
		c.log.Warn().Err(err).Msg("reading who may reach the agent endpoint failed")
	}
	chk := &clusterapi.Checker{
		InstallID: inst.ID, Store: c.st, Allow: allow, Now: c.now,
		ObserveOnly: c.observe.window,
		// The engine's audit trail writes it to this node's file of the
		// cluster root and to the journal.
		Record: func(e store.SecurityEvent) { c.eng.NoteEnrol(e.Subject, e.Message) },
		Log:    c.log,
	}
	srv := clusterapi.New(c.remote, chk, creds.ServerTLS(), chk.Pool, c.log)
	srv.OnEvent(c.eng.NoteNode)
	ap := netip.AddrPortFrom(addr, c.port)
	r := start(ctx, func(ctx context.Context) {
		if err := srv.Serve(ctx, ap); err != nil {
			c.log.Error().Err(err).Msg("the agent endpoint stopped")
		}
	})
	c.mu.Lock()
	c.listening, c.server, c.allow = r, srv, allow
	c.mu.Unlock()
	c.log.Info().Stringer("address", ap).Msg("serving the agents of the other nodes")
}

// refreshListener reads again who may connect and the server certificate,
// which the client of this node issues again when it is due.
func (c *clusterPart) refreshListener(ctx context.Context) {
	c.mu.Lock()
	srv, allow := c.server, c.allow
	c.mu.Unlock()
	if allow != nil {
		if err := allow.Refresh(ctx); err != nil {
			c.log.Debug().Err(err).Msg("reading who may reach the agent endpoint failed")
		}
	}
	if creds, err := (agent.TLSFiles{Dir: agent.TLSDir(c.paths)}).Server(); err == nil && srv != nil {
		srv.SetCertificate(creds.ServerTLS())
	}
}

// agentAddress is this node's agent address in the registry.
func (c *clusterPart) agentAddress() (netip.Addr, error) {
	nodes, err := c.st.Nodes()
	if err != nil {
		return netip.Addr{}, err
	}
	for _, n := range nodes {
		if n.Name == c.node && n.AgentAddress.IsValid() {
			return n.AgentAddress, nil
		}
	}
	return netip.Addr{}, errors.New("this node has no agent address in the registry: pco setup gives it one")
}

// sources are the addresses the endpoint takes a connection from: of every
// registered node that was not removed, its agent address and the address
// the cluster lists for it.
func (c *clusterPart) sources(context.Context) (clusterapi.Sources, error) {
	nodes, err := c.st.Nodes()
	if err != nil {
		return clusterapi.Sources{}, err
	}
	listed := map[string]netip.Addr{}
	if c.quorum != nil {
		for _, m := range c.quorum().Nodes {
			listed[m.Name] = m.IP
		}
	}
	src := clusterapi.Sources{Nodes: map[string][]netip.Addr{}}
	for _, n := range nodes {
		if n.Name == c.node || n.State == store.NodeRemoved {
			continue
		}
		var addrs []netip.Addr
		if n.AgentAddress.IsValid() {
			addrs = append(addrs, n.AgentAddress)
		}
		if ip := listed[n.Name]; ip.IsValid() && ip != n.AgentAddress {
			addrs = append(addrs, ip)
		}
		src.Nodes[n.Name] = addrs
	}
	return src, nil
}

// observeWindow is since when the controller is in observe-only mode, which
// the settings keep as a flag alone: the endpoint takes the certificate of
// a node the store holds none for within a day of it. The start is kept in
// the store, so that a restart of the daemon, or a takeover, does not begin
// the day again.
type observeWindow struct {
	mu    sync.Mutex
	since time.Time
}

// note reads the start of the window, records it as observe-only mode
// begins and removes it as it ends; only the holder of the lease writes. A
// start that cannot be read closes the window.
func (w *observeWindow) note(st *store.Store, on, holder bool, now time.Time, log zerolog.Logger) {
	rec, found, err := st.ObserveSince()
	since := time.Time{}
	switch {
	case err != nil:
		log.Warn().Err(err).Msg("since when observe-only mode runs cannot be read; no unknown certificate is taken")
	case !on:
		if found && holder {
			if err := st.DeleteObserveSince(); err != nil {
				log.Warn().Err(err).Msg("removing the start of observe-only mode failed")
			}
		}
	case found:
		since = rec.Since
	case holder:
		if err := st.SaveObserveSince(store.ObserveSince{Since: now}); err != nil {
			log.Warn().Err(err).Msg("recording the start of observe-only mode failed; no unknown certificate is taken")
			break
		}
		since = now
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.since = since
}

func (w *observeWindow) window() (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.since, !w.since.IsZero()
}
