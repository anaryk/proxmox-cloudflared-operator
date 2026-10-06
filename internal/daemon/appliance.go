package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/api"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/appnet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// stateRetryEvery is how often an appliance without a state on its volume
// looks for one again.
const stateRetryEvery = 30 * time.Second

// ApplianceDeps are the parts of the appliance's daemon that a test replaces.
// The zero value is what the daemon uses in the container.
type ApplianceDeps struct {
	// Volume checks the state volume as appliance.VolumeMounted does.
	Volume func(path, marker string) error
	// System is where the facts of the container are read; the zero value
	// is the running system.
	System appliance.System
	// Flag is the identity flag the connectors start behind; default
	// appliance.IdentityFlag. Rand is where a new epoch is drawn from;
	// default crypto/rand. StateRetry is how long the daemon waits between
	// two looks for a state on its volume; default 30 s.
	Flag       string
	Rand       io.Reader
	StateRetry time.Duration
	// Netlink and NetNft change and read what pco-net.service loads, which
	// the keeper checks beside the egress table; default: the container's
	// netlink and /usr/sbin/nft.
	Netlink appnet.Netlink
	NetNft  egress.Nft
}

func (a ApplianceDeps) withDefaults() ApplianceDeps {
	if a.Netlink == nil {
		a.Netlink = appnet.NewNetlink()
	}
	if a.NetNft == nil {
		a.NetNft = appnet.NewNft()
	}
	if a.Volume == nil {
		a.Volume = appliance.VolumeMounted
	}
	if a.Flag == "" {
		a.Flag = appliance.IdentityFlag
	}
	if a.Rand == nil {
		a.Rand = rand.Reader
	}
	if a.StateRetry <= 0 {
		a.StateRetry = stateRetryEvery
	}
	return a
}

// NoVolumeError is the failure of an appliance whose state volume is not
// there: the daemon exits with appliance.ExitNoVolume, which its unit does not
// restart on.
type NoVolumeError struct{ Line string }

func (e NoVolumeError) Error() string { return e.Line }

// ExitCode is the status the daemon exits with.
func (NoVolumeError) ExitCode() int { return appliance.ExitNoVolume }

// RunAppliance runs the daemon of the appliance until ctx ends. Its order is
// its own: the state volume first, before the lock and the store; then the
// lock and the store; then the install, whose appliance block names the node
// and the endpoint of the Proxmox API, so that the configuration is checked
// only once it is read. While the volume holds no state, the socket answers
// through a stub that refuses everything and the volume is looked at again
// every 30 s. Before the first cycle only a mount that names
// another VMID is acted on: the container is a copy. The rest is
// the first cycle's, whose snapshot self-identification needs.
func RunAppliance(ctx context.Context, cfg Config, deps Deps) error {
	deps = deps.withDefaults()
	app := deps.Appliance.withDefaults()
	log := cfg.Log
	local := cfg.Paths.Local
	if err := app.Volume(local, store.VolumeMarker); err != nil {
		line := appliance.VolumeLine(local, err, app.System.VMIDHint(local))
		log.Error().Err(err).Msg(line)
		return NoVolumeError{Line: line}
	}

	lock, err := lockNode(local)
	if err != nil {
		return err
	}
	defer lock.release()
	st, err := store.Open(cfg.Paths)
	if err != nil {
		return fmt.Errorf("opening the store: %w", err)
	}
	state, err := waitForState(ctx, cfg, deps, app, st)
	if err != nil {
		if ctx.Err() != nil {
			log.Info().Msg("told to stop while waiting for a state on the volume")
			return nil
		}
		return err
	}
	a := state.install.Appliance
	cfg.Node, cfg.Profile = a.Node, store.ProfileAppliance
	cfg.PVEURL, cfg.PVECAFile = "https://"+a.Endpoints[0].Address, a.CAFile
	if err := cfg.check(); err != nil {
		return err
	}
	settings := startSettings(st, log)
	logStart(log, cfg, st)

	// A restart the admin asks for stops the daemon as a signal does, and
	// systemd starts it again.
	ctx, restart := context.WithCancel(ctx)
	defer restart()
	parts, err := buildAppliance(cfg, deps, app, st, state, settings, restart)
	if err != nil {
		return err
	}
	if why, isCopy := parts.self.CopyBeforeCycle(); isCopy && stopCopy(ctx, why, parts.filter, parts.conns, state.install.ID, log) {
		parts.eng.StoppedServing()
	}
	eng, client, filter := parts.eng, parts.client, parts.filter
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
	}
	watch := func(ctx context.Context) { watchNetwork(ctx, eng, deps.WatchNetwork, deps.Sleep, log) }
	k := &keeper{
		table: filter, off: filter.ov.Off, note: eng.NoteEgress, now: deps.Now, log: log,
		net: serviceNet{nl: app.Netlink, nft: app.NetNft}, noteNet: eng.NoteNet,
	}
	keep := func(ctx context.Context) { k.keep(ctx, deps.WatchRuleset, deps.EgressEvery, deps.Sleep) }
	traffic := &sampler{
		statuses: eng.ConnectorStatuses, scrape: parts.conns.Metrics, record: eng.RecordTraffic, targets: eng.SampleTargets,
		now: deps.Now, log: log,
	}
	doc := doctor.NewRunner(eng.State, env, nil, deps.Now, log)
	gid, uids, web := socketAccess(deps.Accounts, log)
	srv := api.New(served{eng, doc}, cfg.Version, uids, log)
	srv.SetWebUID(web)
	srv.SetShutdownTimeout(deps.ShutdownTimeout)
	return serve(ctx, srv, eng, cfg.SocketPath, gid, deps.Notifier, log, watch, keep, traffic.run)
}

// stopCopy is what a copy does before its first cycle: the identity flag is
// gone already; the egress filter is emptied and the connectors of the
// install are stopped. It reports whether both went through; the first cycle
// does again what did not.
func stopCopy(ctx context.Context, why string, filter engine.Egress, conns engine.Connectors, installID string, log zerolog.Logger) bool {
	log.Error().Str("why", why).Msg("this container is a copy of the appliance; the connectors are stopped and the egress filter is empty")
	done := true
	if err := filter.Set(ctx, nil); err != nil && !errors.Is(err, egress.ErrOff) {
		log.Error().Err(err).Msg("emptying the egress filter of a copy failed")
		done = false
	}
	if err := conns.StopAll(ctx, installID); err != nil {
		log.Error().Err(err).Msg("stopping the connectors of a copy failed")
		done = false
	}
	return done
}

// applianceParts is what the appliance's daemon is built of.
type applianceParts struct {
	eng    *engine.Engine
	client *pve.Client
	filter *egressFilter
	conns  *connector.Manager
	self   *appliance.Self
}

// buildAppliance makes the engine of the appliance: the Proxmox client dials
// the endpoint of the install and verifies it under its server name, the
// inventory reads the appliance and the members of its pool every cycle, the
// prober puts the container's interfaces on the segments of their NICs and
// has no forwarding table to look at, and the engine runs with the
// self-identification, the quorum and the incarnation of the container.
// restart stops the daemon for systemd to start it again.
func buildAppliance(cfg Config, deps Deps, app ApplianceDeps, st *store.Store, state applianceState, settings store.Settings,
	restart func(),
) (applianceParts, error) {
	log := cfg.Log
	a := state.install.Appliance
	ep := a.Endpoints[0]
	client, err := pve.New(pve.Config{
		BaseURL:     cfg.PVEURL,
		TokenID:     state.token.TokenID,
		Secret:      state.token.Secret.Reveal(),
		CAFile:      cfg.PVECAFile,
		ServerName:  ep.ServerName,
		NodeCertDir: deps.PVECertDir,
		Log:         log,
	})
	if err != nil {
		return applianceParts{}, fmt.Errorf("building the Proxmox client: %w", err)
	}
	self := &appliance.Self{
		ID:          appliance.Identity{VMID: a.VMID, Node: a.Node, MACs: a.MACs},
		InstallID:   state.install.ID,
		Incarnation: state.incarnation,
		Store:       st,
		Facts:       func() (appliance.Facts, error) { return app.System.Facts(cfg.Paths.Local) },
		Uptimes:     client.Uptimes,
		VerifyError: client.LastVerifyError,
		Endpoint:    ep,
		Flag:        app.Flag,
		Rand:        app.Rand,
		Now:         deps.Now,
		Log:         log,
	}
	own := model.GuestRef{Kind: model.KindLXC, VMID: a.VMID}
	inv := inventory.New(client, inventory.Options{
		GateTags:   []string{settings.GateTag},
		AlwaysRead: func(ref model.GuestRef, pool string) bool { return ref == own || pool == appliance.Pool },
	}, deps.Now, log)
	res := resolve.NewResolver(appliance.NewProber(deps.Prober, self.Segments), resolve.Settings{
		LocalNode:         a.Node,
		TrustStatic:       settings.TrustStatic,
		TrustedCIDRs:      settings.TrustedCIDRs,
		NoForwardingTable: true,
	}, deps.Now)
	conns := connector.NewManager(deps.Systemd, filepath.Join(cfg.Paths.Local, tunnelsDir), nil, log)
	filter := newEgressFilter(deps.Nft, cfg.Paths.Local, deps.ConnectorUID, deps.Resolvers)
	start := wiredFrom(settings)
	ownUser, _, _ := strings.Cut(state.token.TokenID, "!")
	eng, err := engine.New(engine.Deps{
		Store:       st,
		Inventory:   inv,
		StartOnly:   func(s store.Settings) []string { return start.differences(wiredFrom(s)) },
		Restart:     restart,
		Resolver:    res,
		Connectors:  conns,
		Egress:      filter,
		NewClient:   deps.clients(settings.CloudflareBudget),
		Node:        a.Node,
		Now:         deps.Now,
		Log:         log,
		LocalDir:    cfg.Paths.Local,
		Problems:    cfg.Problems,
		Access:      client,
		OwnUser:     ownUser,
		OwnSoft:     ownSoft(app.System, deps.Resolvers),
		Identity:    self,
		Quorate:     quorate(client),
		Incarnation: state.incarnation,
		EpochDrawn:  self.EpochDrawn,

		StartOnlyFields: wiredFields(),
	})
	if err != nil {
		return applianceParts{}, fmt.Errorf("building the engine: %w", err)
	}
	return applianceParts{eng: eng, client: client, filter: filter, conns: conns, self: self}, nil
}

// quorate reads whether the cluster is quorate; a node that is not in a
// cluster is.
func quorate(client *pve.Client) func(ctx context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		status, err := client.ClusterStatus(ctx)
		if err != nil {
			return false, err
		}
		return status.Quorate, nil
	}
}

// ownSoft reads the gateways and the resolvers of the container, which join
// the soft deny.
func ownSoft(sys appliance.System, resolvers func() ([]netip.Addr, error)) func() (gateways, servers []netip.Addr, err error) {
	return func() ([]netip.Addr, []netip.Addr, error) {
		gateways, gerr := sys.Gateways()
		servers, rerr := resolvers()
		return gateways, servers, errors.Join(gerr, rerr)
	}
}
