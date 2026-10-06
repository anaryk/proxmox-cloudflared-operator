package appliance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// ErrNoCredential is the refusal of a recovery without a stored Cloudflare
// credential to read the sentinels with.
var ErrNoCredential = errors.New("no Cloudflare credential is stored, and without one the generation at Cloudflare " +
	"cannot be read: add one with pco credential add first")

// RecoverDeps are what Recover works with besides the store.
type RecoverDeps struct {
	NewClient func(store.Credential) (cfapi.API, error)
	// RecoverInstall is setup.RecoverInstall.
	RecoverInstall RecoverInstallFunc
	Incarnation    func() (string, error)
	Now            func() time.Time
	Rand           io.Reader
}

// Recover is pco appliance recover: with the stored credentials, seeds a new
// epoch above the highest sentinel of this install at Cloudflare (and above
// the stored one), writes it durably with the current incarnation and sets
// observe-only. It is what a rollback or a restore with state is followed by.
// The tunnels are looked for through every stored credential together, and a
// credential that cannot list what it sees refuses the recovery, as it could
// hide a higher generation.
func Recover(ctx context.Context, st *store.Store, deps RecoverDeps) (planner.Writer, error) {
	if err := requireDurable(st); err != nil {
		return planner.Writer{}, err
	}
	inst, found, err := st.Install()
	switch {
	case err != nil:
		return planner.Writer{}, fmt.Errorf("reading the install: %w", err)
	case !found || inst.Appliance == nil:
		return planner.Writer{}, errors.New("the volume holds no install of an appliance: " +
			"pco appliance repair --vmid <vmid> --recover on the node adopts one")
	}
	creds, err := st.Credentials()
	switch {
	case err != nil:
		return planner.Writer{}, fmt.Errorf("reading the credentials: %w", err)
	case len(creds) == 0:
		return planner.Writer{}, ErrNoCredential
	}
	incarnation, err := deps.Incarnation()
	if err != nil {
		return planner.Writer{}, fmt.Errorf("reading the incarnation of this container: %w", err)
	}
	api, err := seeTogether(ctx, creds, deps.NewClient)
	if err != nil {
		return planner.Writer{}, err
	}
	if _, _, err := deps.RecoverInstall(ctx, api, st, inst.ID, deps.Now, deps.Rand, inst); err != nil {
		var none *planner.NoTunnelsError
		if errors.As(err, &none) {
			return planner.Writer{}, fmt.Errorf("the stored credentials see no tunnel of install %s, the install of this volume, "+
				"so the generation its writer used is unknown: add a credential that sees the account of its tunnel "+
				"(pco credential add), then run pco appliance recover again", none.InstallID)
		}
		return planner.Writer{}, err
	}
	if err := withIncarnation(st, incarnation); err != nil {
		return planner.Writer{}, err
	}
	w, _, err := st.Writer()
	if err != nil {
		return planner.Writer{}, fmt.Errorf("reading leader.json: %w", err)
	}
	return w, nil
}

// together is the Cloudflare the stored credentials see together, for the
// listing a recovery makes: the accounts and the zones of all of them, and
// the tunnels of an account, with their configuration, through the first
// credential that sees the account. The rest of the API is the first
// credential's; a recovery only lists.
type together struct {
	cfapi.API
	accounts []cfapi.Account
	zones    []cfapi.Zone
	by       map[string]cfapi.API // account id
}

func seeTogether(ctx context.Context, creds []store.Credential, newClient func(store.Credential) (cfapi.API, error)) (cfapi.API, error) {
	t := &together{by: make(map[string]cfapi.API)}
	for _, c := range creds {
		api, err := newClient(c)
		if err != nil {
			return nil, fmt.Errorf("credential %s cannot be used: %w%s", c.ID, err, wayOut(c))
		}
		if t.API == nil {
			t.API = api
		}
		accounts, err := api.Accounts(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing the accounts credential %s sees: %w%s", c.ID, err, wayOut(c))
		}
		zones, err := api.Zones(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing the zones credential %s sees: %w%s", c.ID, err, wayOut(c))
		}
		for _, a := range accounts {
			if t.by[a.ID] == nil {
				t.by[a.ID] = api
				t.accounts = append(t.accounts, a)
			}
		}
		for _, z := range zones {
			if !slices.ContainsFunc(t.zones, func(o cfapi.Zone) bool { return o.ID == z.ID }) {
				t.zones = append(t.zones, z)
			}
			if t.by[z.AccountID] == nil {
				t.by[z.AccountID] = api
			}
		}
	}
	return t, nil
}

// wayOut is what follows the refusal of a credential that cannot answer:
// the way out.
func wayOut(c store.Credential) string {
	return "; recovery needs every stored credential to answer, as one that does not could hide a higher generation: " +
		"try again, or remove it with pco credential remove " + c.ID + " if its token is gone"
}

func (t *together) Accounts(context.Context) ([]cfapi.Account, error) {
	return slices.Clone(t.accounts), nil
}

func (t *together) Zones(context.Context) ([]cfapi.Zone, error) { return slices.Clone(t.zones), nil }

func (t *together) Tunnels(ctx context.Context, accountID, namePrefix string) ([]cfapi.Tunnel, error) {
	api, err := t.of(accountID)
	if err != nil {
		return nil, err
	}
	return api.Tunnels(ctx, accountID, namePrefix)
}

func (t *together) TunnelConfig(ctx context.Context, accountID, tunnelID string) (cfapi.TunnelConfig, error) {
	api, err := t.of(accountID)
	if err != nil {
		return cfapi.TunnelConfig{}, err
	}
	return api.TunnelConfig(ctx, accountID, tunnelID)
}

func (t *together) of(accountID string) (cfapi.API, error) {
	if api := t.by[accountID]; api != nil {
		return api, nil
	}
	return nil, fmt.Errorf("no stored credential sees account %s", accountID)
}

// Daemon is the daemon of the container as init and recover find it: its
// unit, run through systemctl, and the lock of the node it holds while it
// runs, also when it was started by hand.
type Daemon struct {
	Systemctl func(ctx context.Context, args ...string) (string, error)
	Lock      string
}

// Running reports whether a daemon holds the lock of the node. A missing
// lock file is no daemon, and is not made.
func (d Daemon) Running() (bool, error) {
	f, err := os.Open(d.Lock)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("opening %s: %w", d.Lock, err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, fmt.Errorf("trying %s: %w", d.Lock, err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}

// Stop stops pco.service and reports whether it was active. A daemon that
// still holds the lock after, as one started by hand, is an error: it would
// write the store under the caller.
func (d Daemon) Stop(ctx context.Context) (wasActive bool, err error) {
	out, err := d.Systemctl(ctx, "is-active", Unit)
	switch state := strings.TrimSpace(out); state {
	case "active", "activating", "deactivating", "reloading", "refreshing":
		wasActive = true
	case "inactive", "failed":
	default:
		if err != nil {
			return false, fmt.Errorf("asking systemd about %s: %w", Unit, err)
		}
		return false, fmt.Errorf("systemd says %s is %q", Unit, state)
	}
	if _, err := d.Systemctl(ctx, "stop", Unit); err != nil {
		return wasActive, fmt.Errorf("stopping %s: %w", Unit, err)
	}
	running, err := d.Running()
	switch {
	case err != nil:
		return wasActive, err
	case running:
		return wasActive, fmt.Errorf("a pco daemon runs outside systemd (it holds %s): stop it first", d.Lock)
	}
	return wasActive, nil
}

// Start starts pco.service.
func (d Daemon) Start(ctx context.Context) error {
	if _, err := d.Systemctl(ctx, "start", Unit); err != nil {
		return fmt.Errorf("starting %s: %w", Unit, err)
	}
	return nil
}
