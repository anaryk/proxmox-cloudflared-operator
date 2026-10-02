// Package daemon wires the parts of pco into the daemon that runs on a node:
// the Proxmox client, the inventory, the resolver, the connector manager, the
// engine and the API on its socket. It owns what a process owns and the parts
// do not: the lock of the node, the signals' context, the talk with systemd.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/api"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The defaults of the daemon's flags.
const (
	DefaultPVEURL = "https://127.0.0.1:8006"
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
	SocketPath string
	Paths      store.Paths
	Log        zerolog.Logger
}

// Deps are the parts of the daemon that a test replaces. The zero value is what
// the daemon uses on a node.
type Deps struct {
	Prober    resolve.Prober       // the host's network view; default: the host prober
	Systemd   connector.Systemd    // default: systemctl
	NewClient engine.ClientFactory // default: Cloudflare, one limiter per credential
	Notifier  Notifier             // default: sd_notify
	Accounts  Accounts             // default: the system's users and groups
	Now       func() time.Time     // default: time.Now
	// Sleep waits between two reads of the Proxmox token while the cluster
	// filesystem is not mounted; default: a timer that ctx ends.
	Sleep func(ctx context.Context, d time.Duration) error
}

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
	if d.NewClient == nil {
		d.NewClient = newCloudflareClients("", d.Now).New
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
	return d
}

func (c Config) check() error {
	switch {
	case c.Node == "":
		return errors.New("the node name is empty")
	case c.SocketPath == "":
		return errors.New("the socket path is empty")
	case c.PVEURL == "":
		return errors.New("the Proxmox URL is empty")
	}
	return nil
}

// Run runs the daemon until ctx ends, and returns nil when it stopped because
// of that. It fails to start when another daemon holds the lock of the node,
// and when there is no Proxmox API token to read the guests with; a store that
// is not set up or not mounted is the engine's to report.
func Run(ctx context.Context, cfg Config, deps Deps) error {
	if err := cfg.check(); err != nil {
		return err
	}
	deps = deps.withDefaults()
	log := cfg.Log

	release, err := lockNode(cfg.Paths.Local)
	if err != nil {
		return err
	}
	defer release()

	st, err := store.Open(cfg.Paths)
	if err != nil {
		return fmt.Errorf("opening the store: %w", err)
	}
	token, err := readPVEToken(ctx, st, deps.Sleep, log)
	if err != nil {
		return err
	}
	settings := startSettings(st, log)
	logStart(log, cfg, st)

	eng, err := build(cfg, deps, st, token, settings)
	if err != nil {
		return err
	}
	gid, uids := socketAccess(deps.Accounts, log)
	srv := api.New(eng, cfg.Version, uids, log)
	srv.OnListening(func() {
		if err := deps.Notifier.Ready(); err != nil {
			log.Warn().Err(err).Msg("telling systemd that the daemon is ready failed")
		}
		log.Info().Str("socket", cfg.SocketPath).Msg("listening")
	})
	return serve(ctx, srv, eng, cfg.SocketPath, gid, deps.Notifier, log)
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

func logStart(log zerolog.Logger, cfg Config, st *store.Store) {
	ev := log.Info().Str("version", cfg.Version).Str("node", cfg.Node)
	if inst, found, err := st.Install(); err == nil && found {
		ev = ev.Str("profile", inst.ProfileName())
	} else {
		ev = ev.Str("profile", "unknown")
	}
	ev.Msg("pco daemon starting")
}

// build makes the engine out of the parts. The Proxmox token goes into the
// client and nowhere else.
func build(cfg Config, deps Deps, st *store.Store, token store.PVEToken, settings store.Settings) (*engine.Engine, error) {
	client, err := pve.New(pve.Config{
		BaseURL: cfg.PVEURL,
		TokenID: token.TokenID,
		Secret:  token.Secret.Reveal(),
		CAFile:  cfg.PVECAFile,
	})
	if err != nil {
		return nil, fmt.Errorf("building the Proxmox client: %w", err)
	}
	log := cfg.Log
	inv := inventory.New(client, inventory.Options{GateTags: []string{settings.GateTag}}, deps.Now, log)
	res := resolve.NewResolver(deps.Prober, resolve.Settings{
		LocalNode:    cfg.Node,
		TrustStatic:  settings.TrustStatic,
		TrustedCIDRs: settings.TrustedCIDRs,
	}, deps.Now)
	conns := connector.NewManager(deps.Systemd, filepath.Join(cfg.Paths.Local, tunnelsDir), nil, log)

	eng, err := engine.New(engine.Deps{
		Store:      st,
		Inventory:  newSettingsWatch(inv, st, wiredFrom(settings), log),
		Resolver:   res,
		Connectors: conns,
		NewClient:  deps.NewClient,
		Node:       cfg.Node,
		Now:        deps.Now,
		Log:        log,
		LocalDir:   cfg.Paths.Local,
	})
	if err != nil {
		return nil, fmt.Errorf("building the engine: %w", err)
	}
	return eng, nil
}

// serve runs the engine and the API until ctx ends or the API fails, and then
// stops both.
func serve(ctx context.Context, srv *api.Server, eng *engine.Engine, socket string, gid int, n Notifier, log zerolog.Logger) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	served := make(chan error, 1)
	cycled := make(chan struct{})
	go func() { served <- srv.Serve(ctx, socket, gid) }()
	go func() {
		defer close(cycled)
		if err := eng.Run(ctx); err != nil {
			log.Error().Err(err).Msg("the reconcile loop ended")
		}
	}()

	var err error
	select {
	case err = <-served:
		served = nil
	case <-ctx.Done():
	}
	// The stop begins here; the API takes a moment to finish its requests.
	if nerr := n.Stopping(); nerr != nil {
		log.Warn().Err(nerr).Msg("telling systemd that the daemon is stopping failed")
	}
	log.Info().Msg("pco daemon stopping")
	stop()
	if served != nil {
		err = <-served
	}
	<-cycled
	return err
}
