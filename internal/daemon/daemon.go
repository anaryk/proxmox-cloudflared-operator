// Package daemon wires the parts of pco into the daemon that runs on a node:
// the Proxmox client, the inventory, the resolver, the connector manager, the
// engine and the API on its socket. It owns what a process owns and the parts
// do not: the lock of the node, the signals' context, the talk with systemd.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/vishvananda/netns"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/api"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet/dhcp"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The defaults of the daemon's flags.
const (
	DefaultPVEURL = pve.DefaultURL
	DefaultSocket = "/run/pco/pco.sock"
)

// tunnelsDir is where the connectors' token and env files are kept, below the
// local root.
const tunnelsDir = "tunnels"

// Config says where the daemon works.
type Config struct {
	Version    string // reported by the API
	PVEURL     string // base URL of the Proxmox API
	PVECAFile  string // CA bundle for the Proxmox API; not needed for a loopback URL
	Node       string // the name of this node in Proxmox
	Profile    string // what the daemon runs as: store.ProfileHost or store.ProfileAppliance; empty is the host
	SocketPath string
	Paths      store.Paths
	Log        zerolog.Logger
	// Problems are problem lines that every cycle reports, about how the
	// daemon was started.
	Problems []string
	// TestHooks switches on what the test suites drive the daemon with: a
	// cycle holds as it begins for as long as the file HoldCycle names
	// exists, /run/pco/hold-cycle when empty. Never on in production.
	TestHooks bool
	HoldCycle string
}

// Deps are the parts of the daemon that a test replaces. The zero value is what
// the daemon uses on a node.
type Deps struct {
	Prober    resolve.Prober       // the host's network view; default: the host prober
	Systemd   connector.Systemd    // default: systemctl
	NewClient engine.ClientFactory // default: Cloudflare, one limiter per credential with the budget of the settings
	Notifier  Notifier             // default: sd_notify
	Accounts  Accounts             // default: the system's users and groups
	Now       func() time.Time     // default: time.Now
	// Sleep waits between two reads of the Proxmox token while the cluster
	// filesystem is not mounted; default: a timer that ctx ends.
	Sleep func(ctx context.Context, d time.Duration) error
	// CloudflareURL is the base URL of the Cloudflare API that the default
	// clients talk to; empty is Cloudflare. A test points it at a fake.
	CloudflareURL string
	// ShutdownTimeout is how long a stop waits for the requests that are
	// running; default 30 s. What is still running then is cut off.
	ShutdownTimeout time.Duration
	// Dial is how the doctor tries the way out to Cloudflare; default: a
	// net.Dialer. Cloudflared is the binary whose version it reads; default:
	// the one the connectors run.
	Dial        func(ctx context.Context, network, addr string) (net.Conn, error)
	Cloudflared string
	// HostTimeout bounds every question the doctor asks of the host;
	// default: the doctor's own, 5 s. UnitEnabled is how the doctor asks
	// whether a unit starts at boot; default: systemctl is-enabled.
	HostTimeout time.Duration
	UnitEnabled func(ctx context.Context, unit string) (bool, error)
	// UnitFileState is how the doctor of the appliance asks for the state of
	// a unit's file, as systemctl is-enabled prints it; default: systemctl.
	UnitFileState func(ctx context.Context, unit string) (string, error)
	// PVECertDir is where the certificates of this node are, which the
	// Proxmox API on a loopback URL must present; default /etc/pve/local.
	PVECertDir string
	// Nft runs nft for the egress filter, ConnectorUID looks up the user the
	// connectors run as and Resolvers reads the name servers of the node;
	// default: /usr/sbin/nft, the user pco-connector and /etc/resolv.conf.
	Nft          egress.Nft
	ConnectorUID func() (uint32, error)
	Resolvers    func() ([]netip.Addr, error)
	// WatchNetwork calls onMove for every bound address whose MAC moves,
	// until ctx ends; default: egress.Watch. WatchRuleset calls changed
	// whenever the nftables ruleset changes; default: egress.WatchRuleset.
	// EgressEvery is how often the egress table is checked besides; default
	// 30 s. ObserveForwarding reads the forwarding tables of the bridges
	// for the agent of a node of a cluster; default: egress.ObserveForwarding.
	WatchNetwork      func(ctx context.Context, bound func() map[netip.Addr]egress.Pin, onMove func(netip.Addr)) error
	ObserveForwarding agent.ObserveFunc
	WatchRuleset      func(ctx context.Context, changed func()) error
	EgressEvery       time.Duration
	// Gateway is the backend of the networks reached through a leg; default:
	// on the host the namespace pco, in the appliance the container's own
	// network cards, which it adds through the Proxmox API. WatchGateway
	// calls changed whenever a link or a route of the node, or of the
	// container, changes, which has the gateway checked, and skipped of the
	// first message it skips and goes on past; default: netlink.
	Gateway      gateway.Backend
	WatchGateway func(ctx context.Context, changed func(), skipped func(error)) error
	// WatchPorts calls joined with every port of a guest that joins the
	// managed bridge, on the host; WatchSources listens on the managed leg
	// for frames of cards no lease knows. Defaults: netlink and a packet
	// socket.
	WatchPorts   func(ctx context.Context, ns netns.NsHandle, bridge string, joined func(port string), skipped func(error)) error
	WatchSources func(ctx context.Context, w gateway.SourceWatch, ns netns.NsHandle, dev string) error
	// Namespace runs pco-netns.service, which the daemon starts for a probe,
	// enables with the first network attached and disables when the last is
	// detached; default: systemctl.
	Namespace Netns
	// The web interface: WebDir holds its certificate, WebEnv is the
	// environment file of its unit, WebLoaded is where systemd puts what
	// pco-web.service loaded, ClusterCA and ClusterCAKey are the cluster CA;
	// defaults: the node's. RestartWeb restarts pco-web.service if it runs;
	// default: systemctl try-restart. WebEvery is how often its certificates
	// are looked at; default a minute.
	WebDir, WebEnv, WebLoaded string
	ClusterCA, ClusterCAKey   string
	RestartWeb                func(ctx context.Context) error
	WebEvery                  time.Duration
	// NewDHCP makes the DHCP server of the managed network for its leg;
	// default: the server of package dhcp, with its sockets in the
	// namespace of the legs. DHCPReadyWait is how long the start waits for
	// it to listen before the daemon tells systemd that it is ready all the
	// same; default 10 s.
	NewDHCP       func(cfg dhcp.Config, leases dhcp.Leases) DHCPServer
	DHCPReadyWait time.Duration
	// Appliance is what only the daemon of an appliance uses.
	Appliance ApplianceDeps
	// Lease is what the lease of the host is kept with, and Cluster what
	// a node of a cluster serves and follows the controller with.
	Lease   LeaseDeps
	Cluster ClusterDeps
}

// defaultShutdownTimeout lets a credential check or an apply, which may take a
// minute at Cloudflare, mostly finish.
const defaultShutdownTimeout = 30 * time.Second

func (d Deps) withDefaults() Deps {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Prober == nil {
		d.Prober = resolve.NewHostProber(0, 0)
	}
	if d.Systemd == nil {
		d.Systemd = connector.NewSystemctl()
	}
	if d.Notifier == nil {
		d.Notifier = systemdNotifier{}
	}
	if d.Accounts == nil {
		d.Accounts = systemAccounts{}
	}
	if d.Sleep == nil {
		d.Sleep = sleepContext
	}
	if d.ShutdownTimeout <= 0 {
		d.ShutdownTimeout = defaultShutdownTimeout
	}
	if d.Nft == nil {
		d.Nft = egress.NewNft()
	}
	if d.ConnectorUID == nil {
		d.ConnectorUID = egress.ConnectorUID
	}
	if d.Resolvers == nil {
		d.Resolvers = egress.SystemResolvers
	}
	if d.WatchNetwork == nil {
		d.WatchNetwork = egress.Watch
	}
	if d.ObserveForwarding == nil {
		d.ObserveForwarding = egress.ObserveForwarding
	}
	if d.WatchRuleset == nil {
		d.WatchRuleset = egress.WatchRuleset
	}
	if d.EgressEvery <= 0 {
		d.EgressEvery = checkEvery
	}
	if d.WatchGateway == nil {
		d.WatchGateway = watchLinksAndRoutes
	}
	if d.WatchPorts == nil {
		d.WatchPorts = gateway.WatchGuestPorts
	}
	if d.WatchSources == nil {
		d.WatchSources = runSourceWatch
	}
	if d.Namespace == nil {
		d.Namespace = setup.NewNamespaceUnit(setup.NewHostRunner())
	}
	if d.RestartWeb == nil {
		d.RestartWeb = tryRestartWeb
	}
	if d.WebEvery <= 0 {
		d.WebEvery = webCheckEvery
	}
	if d.DHCPReadyWait <= 0 {
		d.DHCPReadyWait = defaultDHCPReadyWait
	}
	return d
}

// clients returns the factory of the Cloudflare clients: the one the deps
// were given, or the daemon's own, which spends budget requests of each
// credential in 5 minutes. regate, when set, is asked again before a write
// the rate limit held for a while.
func (d Deps) clients(budget int, regate func(ctx context.Context) error) engine.ClientFactory {
	if d.NewClient != nil {
		return d.NewClient
	}
	f := newCloudflareClients(d.CloudflareURL, d.Now, budget)
	f.regate = regate
	return f.New
}

func (c Config) check() error {
	switch {
	case c.Node == "":
		return errors.New("the node name is empty")
	case c.SocketPath == "":
		return errors.New("the socket path is empty")
	case c.PVEURL == "":
		return errors.New("the Proxmox URL is empty")
	case c.Profile != "" && c.Profile != store.ProfileHost && c.Profile != store.ProfileAppliance:
		return fmt.Errorf("profile %q: want %q or %q", c.Profile, store.ProfileHost, store.ProfileAppliance)
	}
	return nil
}

// profile is the profile the daemon runs in, never an empty one.
func (c Config) profile() string {
	if c.Profile == "" {
		return store.ProfileHost
	}
	return c.Profile
}

// Run runs the daemon until ctx ends, and returns nil when it stopped because
// of that, also when it was told to stop while it waited for the cluster
// filesystem, and when a request would not finish in time and was cut off.
//
// It fails to start when another daemon holds the lock of the node, and when it
// cannot read the Proxmox API token to read the guests with: a store that is
// not set up fails there, and one that is not mounted after the wait for it.
// Once the token is read, a store that is gone is the engine's to report in its
// state, and the daemon keeps running.
//
// An appliance runs as RunAppliance says.
func Run(ctx context.Context, cfg Config, deps Deps) error {
	if cfg.Profile == store.ProfileAppliance {
		return RunAppliance(ctx, cfg, deps)
	}
	if err := cfg.check(); err != nil {
		return err
	}
	deps = deps.withDefaults()
	log := cfg.Log

	lock, err := lockNode(cfg.Paths.Local)
	if err != nil {
		return err
	}
	defer lock.release()

	st, err := store.Open(cfg.Paths)
	if err != nil {
		return fmt.Errorf("opening the store: %w", err)
	}
	// The store keeps the cluster copies of what a controller on another
	// node inherits once it knows the node.
	st.SetNode(cfg.Node)
	token, err := readPVEToken(ctx, st, deps.Sleep, log)
	if err != nil {
		if ctx.Err() != nil {
			log.Info().Msg("told to stop while waiting for the cluster filesystem")
			return nil
		}
		return err
	}
	settings := startSettings(st, log)
	logStart(log, cfg, st)

	// A restart the admin asks for stops the daemon as a signal does, and
	// systemd starts it again.
	ctx, restart := context.WithCancel(ctx)
	defer restart()
	if deps.Gateway == nil {
		deps.Gateway = gateway.NewHost(gateway.NewNamespaceNft(gateway.Namespace), gateway.NewHostNft(), log)
	}
	hl, err := newHostLease(cfg.Node, st, cfg.Paths, deps.Lease, log)
	if err != nil {
		return fmt.Errorf("preparing the lease: %w", err)
	}
	gw := newGatewayPart(deps.Gateway, cfg.Paths.Local, deps.Now, log)
	gw.start(ctx)
	cp := newClusterPart(cfg, deps, st, hl, log)
	eng, client, filter, conns, err := build(cfg, deps, st, token, settings, restart, gw, hl, cp)
	if err != nil {
		return err
	}
	cp.attach(eng)
	// The keeper runs from before the engine to after it: the engine writes
	// only under the lease, and the lease is given back once it stopped.
	hl.start(eng)
	defer hl.end(eng)
	hl.awaitFirstTry(ctx)
	dh := newDHCPPart(st, cfg.Node, gw, true, eng.DHCPLeases(), deps, eng.NoteDHCP, log)
	defer dh.start(ctx)()
	env := &doctor.HostEnv{
		Systemd:    deps.Systemd,
		Proxmox:    client,
		Interval:   eng.PollInterval,
		Clock:      deps.Now,
		StoreCheck: StoreReady(st),
		LockCheck:  lock.check,
		Binary:     deps.Cloudflared,
		Dial:       deps.Dial,
		Timeout:    deps.HostTimeout,
		Enabled:    deps.UnitEnabled,
		FileState:  deps.UnitFileState,
		Pool:       servicePool(st),
		PinsOf:     st.Pins,
		Cluster:    doctorCluster{cp: cp, eng: eng},
	}
	watch := func(ctx context.Context) { watchNetwork(ctx, eng, deps.WatchNetwork, deps.Sleep, log) }
	k := &keeper{
		table: filter, off: filter.ov.Off, note: eng.NoteEgress, now: deps.Now, log: log,
		gw: gw, noteGw: eng.NoteGateway, watchNet: deps.WatchGateway, idle: eng.Idle,
	}
	keep := func(ctx context.Context) { k.keep(ctx, deps.WatchRuleset, deps.EgressEvery, deps.Sleep) }
	traffic := &sampler{
		statuses: eng.ConnectorStatuses, scrape: conns.Metrics, record: eng.RecordTraffic, targets: eng.SampleTargets,
		now: deps.Now, log: log,
	}
	ports := func(ctx context.Context) { gw.watchPorts(ctx, deps.WatchPorts, deps.Sleep) }
	sources := newSourceWatch(gw, st, eng, deps, log)
	wd := newWatchdog(deps.Notifier, eng.Healthy, deps.Now, log)
	beside := []func(context.Context){watch, keep, traffic.run, ports, sources.keep, wd.run, cp.run}
	if cfg.profile() == store.ProfileHost {
		web := newWebKeeper(cfg, deps, client, hl.d.PVEDir, eng.NoteWeb, log)
		env.Web = web.facts
		beside = append(beside, func(ctx context.Context) { web.keep(ctx, deps.WebEvery) })
	}
	doc := doctor.NewRunner(eng.State, env, nil, deps.Now, log)
	cp.doctor.set(doc)
	gid, uids, web := socketAccess(deps.Accounts, log)
	srv := api.New(served{eng, doc, cp.agentView}, cfg.Version, uids, log)
	srv.SetRole(api.LeaseRole(eng.State, hl.alone))
	srv.SetWebUID(web)
	srv.SetShutdownTimeout(deps.ShutdownTimeout)
	return serve(ctx, srv, runEngine(eng), cfg.SocketPath, gid, deps.Notifier, dh.awaitBound, log, beside...)
}

// servicePool is what the doctor reads of the pool of service addresses:
// how many were given out, and its size.
func servicePool(st *store.Store) func() (used, size int, err error) {
	return func() (used, size int, err error) {
		_, used, size, err = st.ServicePool()
		return used, size, err
	}
}

// startSettings reads the settings that are wired at start. Settings that
// cannot be read are the defaults for now: the engine reports them every cycle.
func startSettings(st *store.Store, log zerolog.Logger) store.Settings {
	s, err := st.Settings()
	if err != nil {
		log.Warn().Err(err).Msg("reading the settings failed; starting with the defaults for what is read only at start")
		return store.DefaultSettings()
	}
	return s
}

// logStart logs the profile the daemon runs in next to the one the install
// recorded, so that a marker that disagrees with the install shows.
func logStart(log zerolog.Logger, cfg Config, st *store.Store) {
	ev := log.Info().Str("version", cfg.Version).Str("node", cfg.Node).Str("profile", cfg.profile())
	if inst, found, err := st.Install(); err == nil && found {
		ev = ev.Str("installProfile", inst.ProfileName())
	} else {
		ev = ev.Str("installProfile", "unknown")
	}
	ev.Msg("pco daemon starting")
}

// served is what the API answers for: the engine, and the doctor that reads
// its state.
type served struct {
	*engine.Engine
	doc *doctor.Runner
	// agent is the agent of this node as its client knows it, which the
	// engine alone does not.
	agent func() (agentproto.AgentView, error)
}

func (s served) Diagnose(ctx context.Context, hostname string) ([]doctor.Step, error) {
	return s.doc.Diagnose(ctx, hostname)
}

func (s served) AgentView() (agentproto.AgentView, error) {
	if s.agent != nil {
		return s.agent()
	}
	return s.Engine.AgentView()
}

func (s served) Doctor(ctx context.Context) []doctor.Finding { return s.doc.Doctor(ctx) }

// StoreReady returns the check of whether the store is mounted and set up on
// this node.
func StoreReady(st *store.Store) func() error {
	return func() error {
		_, found, err := st.Install()
		switch {
		case err != nil:
			return err
		case !found:
			return errors.New("pco is not set up on this node; run pco setup")
		}
		return nil
	}
}

// routedGuests names the guests of the manual routes for the inventory, which
// reads them as it reads a guest with the gate tag. Routes it cannot read name
// none: the cycle holds on the same error when it reads them.
func routedGuests(st *store.Store, log zerolog.Logger) func() []model.GuestRef {
	return func() []model.GuestRef {
		routes, err := st.ManualRoutes()
		if err != nil {
			log.Debug().Err(err).Msg("the manual routes could not be read for the inventory")
			return nil
		}
		var refs []model.GuestRef
		for _, r := range routes {
			if r.Guest != nil {
				refs = append(refs, *r.Guest)
			}
		}
		return refs
	}
}

// build makes the engine out of the parts, and returns the Proxmox client it
// reads through, the egress filter it feeds and the connector manager it
// keeps the connectors with too. The Proxmox token goes into the client and
// nowhere else. restart stops the daemon for systemd to start it again. The
// resolver probes through the legs of gw that are up, and the engine syncs
// gw.
func build(cfg Config, deps Deps, st *store.Store, token store.PVEToken, settings store.Settings, restart func(), gw *gatewayPart,
	hl *hostLease, cp *clusterPart) (*engine.Engine, *pve.Client, *egressFilter, *connector.Manager, error) {
	client, multi, err := proxmoxClient(cfg, deps, token, hl)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("building the Proxmox client: %w", err)
	}
	log := cfg.Log
	// The guests and the network of this node are read with pvesh while its
	// pveproxy does not answer: no other node's can then.
	onNode, err := pve.NewLocal(pve.ExecPvesh(), cfg.Node)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("building the reader of this node: %w", err)
	}
	src := inventory.OnNode(client, cfg.Node, onNode)
	inv := inventory.New(src, inventory.Options{GateTags: []string{settings.GateTag}, Routed: routedGuests(st, log)}, deps.Now, log)
	res := resolve.NewResolver(gateway.NewProber(deps.Prober, gw.UpLegs, deps.Gateway), resolve.Settings{
		LocalNode:    cfg.Node,
		TrustStatic:  settings.TrustStatic,
		TrustedCIDRs: settings.TrustedCIDRs,
	}, deps.Now)
	conns := connector.NewManager(deps.Systemd, filepath.Join(cfg.Paths.Local, tunnelsDir), nil, log)
	filter := newEgressFilter(deps.Nft, cfg.Paths.Local, deps.ConnectorUID, deps.Resolvers)

	start := wiredFrom(settings)
	// The access control is read through the same client; pco's own user,
	// whose tokens are not delegates, is the user of the token. The host has
	// no gateway or resolver of its own to soft-deny beyond the node's.
	ownUser, _, _ := strings.Cut(token.TokenID, "!")
	host := managednet.NewHostConfigurer(setup.NewHostRunner(), cfg.Node)
	ed := engine.Deps{
		Store:     st,
		Inventory: inv,
		StartOnly: func(s store.Settings) []string { return start.differences(wiredFrom(s)) },
		Restart:   restart,
		Fleet:     engine.NewLocalFleet(agentDeps(res, conns, filter, gw, nodeProbe{gw: gw, prober: deps.Prober, unit: deps.Namespace}, cp.doctor.run)),
		NewClient: holdingClients(deps.clients(settings.CloudflareBudget, regate(hl)), hl.keeper),
		Node:      cfg.Node,
		Now:       deps.Now,
		Log:       log,
		LocalDir:  cfg.Paths.Local,
		Problems:  cfg.Problems,
		Access:    client,
		OwnUser:   ownUser,
		Gateway:   gw,
		Networks:  nodeNetworks{proxmox: client, prober: deps.Prober, unit: deps.Namespace},
		// The host's daemon runs as root: the cards of the guests of this
		// node are changed with qm and pct, those of the other nodes by
		// their agents.
		Guests: host,

		StartOnlyFields: wiredFields(),

		Lease:     hl.keeper,
		Quorum:    hl.quorate,
		Cluster:   hl.Cluster,
		HoldCycle: holdHook(cfg, deps.Sleep, log),
	}
	managedReaders(&ed, client, cfg.Node)
	cp.wire(&ed, client, multi)
	eng, err := engine.New(ed)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("building the engine: %w", err)
	}
	host.SetRemote(eng.PVEWriter(), client)
	return eng, client, filter, conns, nil
}

// serve runs the API, starts the engine once the socket is listening, and runs
// both until ctx ends or the API fails; then it stops both. The engine waits
// for the socket so that a socket that cannot be made does not leave a first
// cycle that is cut off at a point nobody chose. What runs beside the engine
// starts and stops with it. ready, when given, is waited for before systemd
// is told that the daemon is ready: the DHCP server of a managed network.
//
// A request that does not finish within the shutdown time is cut off. That is
// the end of a stop that was asked for, and not a failure of the daemon. main
// is the engine's run, or what an agent container runs in its place.
func serve(ctx context.Context, srv *api.Server, main func(context.Context), socket string, gid int, n Notifier, ready func(context.Context), log zerolog.Logger, beside ...func(context.Context)) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	listening := make(chan struct{})
	srv.OnListening(func() {
		if ready != nil {
			ready(ctx)
		}
		if err := n.Ready(); err != nil {
			log.Warn().Err(err).Msg("telling systemd that the daemon is ready failed")
		}
		log.Info().Str("socket", socket).Msg("listening")
		close(listening)
	})
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, socket, gid) }()

	var err error
	cycled := make(chan struct{})
	var others sync.WaitGroup
	started := false
	select {
	case <-listening:
		started = true
		go func() {
			defer close(cycled)
			main(ctx)
		}()
		for _, run := range beside {
			others.Go(func() { run(ctx) })
		}
		select {
		case err = <-served:
			served = nil
		case <-ctx.Done():
		}
	case err = <-served:
		served = nil
	case <-ctx.Done():
	}
	asked := ctx.Err() != nil

	// The stop begins here; the API takes a moment to finish its requests.
	if nerr := n.Stopping(); nerr != nil {
		log.Warn().Err(nerr).Msg("telling systemd that the daemon is stopping failed")
	}
	log.Info().Msg("pco daemon stopping")
	stop()
	if served != nil {
		err = <-served
	}
	if started {
		<-cycled
		others.Wait()
	}
	if asked && errors.Is(err, context.DeadlineExceeded) {
		log.Warn().Msg("requests were still running when the time to finish them ended; they were cut off")
		return nil
	}
	return err
}

// runEngine is the run of eng as serve takes it: it ends with ctx and has no
// error to tell.
func runEngine(eng *engine.Engine) func(context.Context) {
	return func(ctx context.Context) { _ = eng.Run(ctx) }
}
