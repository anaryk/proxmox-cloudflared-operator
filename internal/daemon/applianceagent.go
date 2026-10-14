package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/access"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/api"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/appnet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/version"
)

// agentCheckEvery is how often an agent container proves itself again, and
// tries again to join while it has no certificate.
const agentCheckEvery = 30 * time.Second

// runApplianceAgent runs an agent container: the agent of its node, which
// follows the controller of its install over the network once it joined
// with the token root pushed onto its volume, and serves only while the
// container proves it is the one installed, on the node it was installed on,
// with no principal able to reach into it. It runs no controller: the API on
// its socket answers pco agent status and refuses the rest.
func runApplianceAgent(ctx context.Context, cfg Config, deps Deps, app ApplianceDeps, st *store.Store, state applianceState, lockCheck func() error) error {
	log := cfg.Log
	a := state.install.Appliance
	client, err := pve.New(pve.Config{
		BaseURL: cfg.PVEURL, TokenID: state.token.TokenID, Secret: state.token.Secret.Reveal(),
		CAFile: cfg.PVECAFile, ServerName: a.Endpoints[0].ServerName, NodeCertDir: deps.PVECertDir, Log: log,
	})
	if err != nil {
		return fmt.Errorf("building the Proxmox client: %w", err)
	}
	self := &appliance.Self{
		ID: appliance.Identity{VMID: a.VMID, Node: a.Node, MACs: a.MACs}, InstallID: state.install.ID,
		Incarnation: state.incarnation, Store: st,
		Facts:   func() (appliance.Facts, error) { return app.System.Facts(cfg.Paths.Local) },
		Uptimes: client.Uptimes, VerifyError: client.LastVerifyError, Endpoint: a.Endpoints[0],
		Flag: app.Flag, Rand: app.Rand, Now: deps.Now, Log: log, NoEpoch: true,
	}
	own := model.GuestRef{Kind: model.KindLXC, VMID: a.VMID}
	inv := inventory.New(client, inventory.Options{
		GateTags:   []string{store.DefaultSettings().GateTag},
		AlwaysRead: func(ref model.GuestRef, pool string) bool { return ref == own || pool == appliance.Pool },
	}, deps.Now, log)
	backend := deps.Gateway
	if backend == nil {
		backend = gateway.NewContainer(gateway.NewHostNft(), client, own, a.Node, self.Expect, log, gateway.WithNetTable(appnet.Script))
	}
	gw := newGatewayPart(backend, cfg.Paths.Local, deps.Now, log)
	prober := appliance.NewProber(deps.Prober, self.Segments)
	res := resolve.NewResolver(gateway.NewProber(prober, gw.UpLegs, backend), resolve.Settings{
		LocalNode: a.Node, NoForwardingTable: true, Ceiling: managedCeiling,
	}, deps.Now)
	conns := connector.NewManager(deps.Systemd, filepath.Join(cfg.Paths.Local, tunnelsDir), nil, log)
	filter := newEgressFilter(deps.Nft, cfg.Paths.Local, deps.ConnectorUID, deps.Resolvers)
	events := agent.NewEventLog(deps.Now)
	r := &agentRun{log: log, now: deps.Now, flag: self.SetFlag}
	checks := &agentDoctor{}
	ag, err := agent.New(agent.Deps{
		Node: a.Node, Kind: pki.KindAppliance, InstallID: state.install.ID, Store: st,
		Resolver: res, Connectors: conns, Egress: filter, Blocked: filter.ov.Blocked, Gateway: agentGateway{gw: gw},
		Now: deps.Now, Log: log, Event: events.Add, Changed: r.trigger, Doctor: checks.run,
	})
	if err != nil {
		return fmt.Errorf("making the agent of node %s: %w", a.Node, err)
	}
	dir := filepath.Dir(agent.TLSDir(cfg.Paths))
	files := agent.TLSFiles{Dir: agent.TLSDir(cfg.Paths)}
	macs, _ := app.System.Facts(cfg.Paths.Local)
	r.agent, r.files, r.events = ag, files, events
	r.joiner = agent.Joiner{
		TokenFile: agent.JoinFile(cfg.Paths), Files: files, Dir: dir,
		InstallID: state.install.ID, Node: a.Node, VMID: a.VMID, MACs: linkMACs(macs.Links), Build: version.Version,
		Now: deps.Now, Log: log,
	}
	r.discover = agent.ApplianceDiscovery{InstallID: state.install.ID, Endpoints: func() []string {
		eps := ag.ControllerEndpoints()
		if rec, found, err := agent.ReadController(dir); err == nil && found {
			eps = append(eps, rec.Endpoints...)
		}
		return eps
	}}
	r.guard = &agentGuard{
		vmid: a.VMID, node: a.Node, ownUser: tokenUser(state.token.TokenID),
		verdict: func(ctx context.Context) appliance.Verdict { return self.Check(ctx, inv.Refresh(ctx)) },
		placement: func(ctx context.Context) ([]pve.Resource, []pve.HAResource, error) {
			res, err := client.Resources(ctx)
			if err != nil {
				return nil, nil, err
			}
			ha, err := client.HAResources(ctx)
			return res, ha, err
		},
		access: func(ctx context.Context) (access.Data, error) { return engine.ReadAccess(ctx, client) },
		now:    deps.Now,
	}

	gw.start(ctx)
	env := &doctor.HostEnv{
		Systemd: deps.Systemd, Proxmox: client, Interval: func() time.Duration { return agentCheckEvery }, Clock: deps.Now,
		StoreCheck: applianceStore(app, cfg.Paths.Local, st), LockCheck: lockCheck, Binary: deps.Cloudflared, Dial: deps.Dial,
		Timeout: deps.HostTimeout, Enabled: deps.UnitEnabled, FileState: deps.UnitFileState,
		Cluster: agentContainerCluster{view: r.view},
	}
	user, _, _ := strings.Cut(state.token.TokenID, "!")
	env.App = &doctor.ApplianceHost{
		Host: env, Proxmox: client,
		Install: doctor.Self{
			VMID: a.VMID, Node: a.Node, Address: a.Endpoints[0].Address, ServerName: a.Endpoints[0].ServerName,
			User: user, Token: state.token.TokenID, MACs: a.MACs,
		},
		StateDir: cfg.Paths.Local, Volume: app.Volume, Netlink: app.Netlink, NetNft: app.NetNft,
		CloudflareURL: deps.CloudflareURL, Resolve: deps.Resolvers,
	}
	if app.Doctor != nil {
		app.Doctor(env.App.(*doctor.ApplianceHost))
	}
	agentState := func() engine.State {
		return engine.State{Profile: store.ProfileAppliance, Node: a.Node, At: deps.Now()}
	}
	doc := doctor.NewRunner(agentState, env, nil, deps.Now, log)
	checks.set(doc)
	gid, uids, webUID := socketAccess(deps.Accounts, log)
	srv := api.New(&agentOnlyEngine{noStateEngine: newNoStateEngine(agentOnlyLine(a.Node), deps.Now(), agentCheckEvery), node: a.Node, view: r.view, doc: doc},
		cfg.Version, uids, log)
	srv.SetWebUID(webUID)
	srv.SetShutdownTimeout(deps.ShutdownTimeout)
	k := &keeper{
		table: filter, off: filter.ov.Off, note: func(engine.EgressCheck) {}, now: deps.Now, log: log,
		net: serviceNet{nl: app.Netlink, nft: app.NetNft}, noteNet: func(engine.NetCheck) {},
		gw: gw, noteGw: func(engine.GatewayCheck) {}, watchNet: deps.WatchGateway,
		idle: func(_ context.Context, fn func()) error { fn(); return nil },
	}
	keep := func(ctx context.Context) { k.keep(ctx, deps.WatchRuleset, deps.EgressEvery, deps.Sleep) }
	main := func(ctx context.Context) { r.run(ctx, deps.Sleep) }
	return serve(ctx, srv, main, cfg.SocketPath, gid, deps.Notifier, nil, log, keep)
}

// tokenUser is the user of a token id.
func tokenUser(tokenID string) string {
	user, _, _ := strings.Cut(tokenID, "!")
	return user
}

// agentOnlyLine is what an agent container answers for everything but its
// agent.
func agentOnlyLine(node string) string {
	return "this container is the pco agent of node " + node + ": routes, settings and credentials are kept by the controller; " +
		"pco agent status shows this agent"
}

// linkMACs are the MACs of the container's links.
func linkMACs(links []appliance.NamedLink) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		out = append(out, l.MAC)
	}
	return out
}

// agentOnlyEngine is the API of an agent container: the agent's view, and a
// refusal of everything that belongs to the controller.
type agentOnlyEngine struct {
	*noStateEngine
	node string
	view func() agentproto.AgentView
	doc  *doctor.Runner
}

// Doctor makes the checks of the container and its agent.
func (e *agentOnlyEngine) Doctor(ctx context.Context) []doctor.Finding { return e.doc.AgentChecks(ctx) }

func (e *agentOnlyEngine) RunsAs() (profile, node string) { return store.ProfileAppliance, e.node }

func (e *agentOnlyEngine) AgentView() (agentproto.AgentView, error) { return e.view(), nil }

// agentRun joins, proves the container and has the client follow the
// controller while it serves.
type agentRun struct {
	agent    *agent.Agent
	files    agent.TLSFiles
	events   *agent.EventLog
	joiner   agent.Joiner
	discover agent.Discoverer
	guard    *agentGuard
	flag     func(bool) error
	log      zerolog.Logger
	now      func() time.Time

	mu      sync.Mutex
	client  *agent.Client
	stop    context.CancelFunc
	done    chan struct{}
	problem string
	stopped bool // the connectors were stopped since the agent last served
}

// run checks every agentCheckEvery until ctx ends.
func (r *agentRun) run(ctx context.Context, sleep func(context.Context, time.Duration) error) {
	defer r.stopClient()
	for {
		r.step(ctx)
		if sleep(ctx, agentCheckEvery) != nil {
			return
		}
	}
}

// step joins where the agent has no certificate, judges the container and
// starts or stops the client and the connectors by the verdict.
func (r *agentRun) step(ctx context.Context) {
	if _, err := r.files.Client(); errors.Is(err, agent.ErrNoCredentials) {
		_, jerr := r.joiner.Join(ctx)
		switch {
		case errors.Is(jerr, agent.ErrNoJoinToken):
			r.say("this agent has no certificate and no join token: pco cluster join-token --node " + r.joiner.Node +
				" on the controller, then install the agent again with it")
			return
		case jerr != nil:
			r.say(jerr.Error())
			return
		}
	}
	serve, why := r.guard.judge(ctx)
	if !serve {
		r.hold(ctx, why)
		return
	}
	r.say("")
	if err := r.flag(true); err != nil {
		r.say(fmt.Sprintf("writing the identity flag: %v; the connectors do not start until it is written", err))
		return
	}
	r.agent.Resume()
	r.mu.Lock()
	r.stopped = false
	r.mu.Unlock()
	r.startClient(ctx)
}

// hold stops the client, takes the flag away, empties the egress filter and
// stops the connectors, once for each time the agent stops serving.
func (r *agentRun) hold(ctx context.Context, why string) {
	r.say(why + "; the connectors are stopped and the egress filter is empty")
	r.stopClient()
	if err := r.flag(false); err != nil {
		r.log.Error().Err(err).Msg("removing the identity flag")
	}
	r.mu.Lock()
	done := r.stopped
	r.mu.Unlock()
	if done {
		return
	}
	if err := r.agent.Halt(ctx); err != nil {
		r.log.Error().Err(err).Msg("emptying the egress filter")
		return
	}
	if err := r.agent.StopConnectors(ctx); err != nil {
		r.log.Error().Err(err).Msg("stopping the connectors")
		return
	}
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
}

func (r *agentRun) say(problem string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if problem != "" && problem != r.problem {
		r.log.Warn().Msg(problem)
	}
	r.problem = problem
}

func (r *agentRun) startClient(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != nil {
		return
	}
	c, err := agent.NewClient(r.agent, agent.ClientDeps{Discover: r.discover, Files: r.files, Events: r.events})
	if err != nil {
		r.log.Error().Err(err).Msg("the agent cannot follow its controller")
		return
	}
	cctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(cctx)
	}()
	r.client, r.stop, r.done = c, stop, done
	r.log.Info().Msg("the agent follows its controller")
}

func (r *agentRun) stopClient() {
	r.mu.Lock()
	stop, done := r.stop, r.done
	r.client, r.stop, r.done = nil, nil, nil
	r.mu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
}

func (r *agentRun) trigger() {
	r.mu.Lock()
	c := r.client
	r.mu.Unlock()
	if c != nil {
		c.Trigger()
	}
}

// view is the agent as pco agent status shows it.
func (r *agentRun) view() agentproto.AgentView {
	r.mu.Lock()
	c, problem := r.client, r.problem
	r.mu.Unlock()
	var v agentproto.AgentView
	if c != nil {
		v = c.View()
	} else {
		v = r.agent.View()
		if creds, err := r.files.Client(); err == nil {
			v.CertFingerprint, v.CertExpires = pki.Fingerprint(creds.Cert), creds.Cert.NotAfter
		}
	}
	if problem != "" {
		v.Problems = append(v.Problems, problem)
	}
	return v
}

// agentGuard judges whether an agent container may serve: it is the one
// installed, by the facts self-identification reads; it runs on the node it
// was installed on and is no HA resource; and no principal other than an
// admin and pco's own can reach into it. What cannot be judged in a check
// keeps the verdict of the check before; before any, it does not serve.
type agentGuard struct {
	vmid      int
	node      string
	ownUser   string
	verdict   func(ctx context.Context) appliance.Verdict
	placement func(ctx context.Context) ([]pve.Resource, []pve.HAResource, error)
	access    func(ctx context.Context) (access.Data, error)
	now       func() time.Time

	identity, bound, reach judged
	// undecided is since when a part could not be judged in every check,
	// and why the last one could not; zero while each was judged.
	undecided    time.Time
	undecidedWhy string
}

// undecidedFor is how long the verdicts of the checks before may stand in
// for checks that cannot be made: an agent that cannot read Proxmox for
// longer cannot tell that it was moved, copied or reached into.
const undecidedFor = 5 * time.Minute

// judged is one part of the verdict, as last decided.
type judged struct {
	decided bool
	ok      bool
	why     string
}

func (j *judged) set(ok bool, why string) { *j = judged{decided: true, ok: ok, why: why} }

func (j judged) holds(waiting string) (bool, string) {
	switch {
	case !j.decided:
		return false, waiting
	case !j.ok:
		return false, j.why
	}
	return true, ""
}

func (g *agentGuard) judge(ctx context.Context) (bool, string) {
	why := ""
	switch v := g.verdict(ctx); {
	case v.Copy:
		g.identity.set(false, v.Why)
	case !v.OK && (v.Why == appliance.WhyIncomplete || v.Why == appliance.WhyPendingNIC):
		why = v.Why
	case !v.OK:
		g.identity.set(false, v.Why)
	default:
		g.identity.set(true, "")
	}
	if res, ha, err := g.placement(ctx); err != nil {
		why = "where the container runs cannot be read: " + err.Error()
	} else if b := appliance.BoundTo(g.vmid, g.node, res, ha); b.Pending {
		why = b.Why
	} else {
		g.bound.set(b.Bound, b.Why)
	}
	if d, err := g.access(ctx); err != nil {
		why = "the access control of Proxmox cannot be read: " + err.Error()
	} else {
		if exposed := access.Refused(d, g.vmid, appliance.Pool, g.ownUser); len(exposed) > 0 {
			ids := make([]string, len(exposed))
			for i, p := range exposed {
				ids[i] = p.ID
			}
			g.reach.set(false, fmt.Sprintf("%s can reach into the agent lxc/%d", strings.Join(ids, ", "), g.vmid))
		} else {
			g.reach.set(true, "")
		}
	}
	now := g.now()
	switch {
	case why == "":
		g.undecided, g.undecidedWhy = time.Time{}, ""
	case g.undecided.IsZero():
		g.undecided, g.undecidedWhy = now, why
	default:
		g.undecidedWhy = why
	}
	if !g.undecided.IsZero() && now.Sub(g.undecided) >= undecidedFor {
		return false, fmt.Sprintf("nothing proves this agent for %s: %s", now.Sub(g.undecided).Round(time.Second), g.undecidedWhy)
	}
	for _, part := range []struct {
		j       judged
		waiting string
	}{
		{g.identity, "self-identification has not passed yet"},
		{g.bound, fmt.Sprintf("where agent %d runs has not been read yet", g.vmid)},
		{g.reach, "the access control of Proxmox has not been read yet"},
	} {
		if ok, why := part.j.holds(part.waiting); !ok {
			return false, why
		}
	}
	return true, ""
}
