package setup

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// placedTunnel is a tunnel of an install, in the account it was found in.
type placedTunnel struct {
	account string
	tunnel  cfapi.Tunnel
}

// recover adopts the install whose tunnels the Cloudflare token sees, after
// its store was lost: its id becomes the id of this install, and the writer
// takes a generation above every one found in its tunnels, so that the
// daemon is not taken for a stale or a foreign writer. The install observes
// until pco apply. Whatever it cannot know it refuses rather than guess.
func (r *run) recover(ctx context.Context) error {
	if err := r.stopForRecovery(ctx); err != nil {
		return err
	}
	if err := r.recoveryToken(); err != nil {
		return err
	}
	api, err := r.newClient(r.token)
	if err != nil {
		return fmt.Errorf("the Cloudflare token cannot be used: %w", err)
	}
	found, err := findInstalls(ctx, api)
	if err != nil {
		return err
	}
	id, err := r.chooseInstall(found)
	if err != nil {
		return err
	}
	generation, err := highestGeneration(ctx, api, id, found[id])
	if err != nil {
		return err
	}
	return r.adopt(id, generation)
}

// stopForRecovery stops pco.service, if it runs, so that the store is not
// written under it; a failed run starts it again. A daemon run by hand is not
// stopped, and refuses the recovery.
func (r *run) stopForRecovery(ctx context.Context) error {
	if r.unitInstalled(serviceUnit) {
		active, err := r.serviceActive(ctx, serviceUnit)
		if err != nil {
			return err
		}
		r.looked = true
		if active {
			if _, err := r.run.Run(ctx, "systemctl", "stop", serviceUnit); err != nil {
				return fmt.Errorf("stopping %s: %w", serviceUnit, err)
			}
			r.stopped = true
		}
	}
	locked, err := r.daemonLocked()
	switch {
	case err != nil:
		return err
	case locked:
		return r.errRunsByHand()
	}
	if r.looked {
		running := false
		r.running = &running
	}
	return nil
}

// recoveryToken makes sure there is a Cloudflare token to find the install
// with, asking for it when it was not given.
func (r *run) recoveryToken() error {
	const missing = "--recover needs a Cloudflare token to find the install with: pass --cf-token-file or --cf-token-stdin"
	if r.token != "" {
		return nil
	}
	if r.o.Yes {
		return errors.New(missing)
	}
	token, err := r.ask.Secret("Cloudflare API token, to find the install with: ")
	if err != nil {
		return fmt.Errorf("reading the token: %w", err)
	}
	if r.token = strings.TrimSpace(token); r.token == "" {
		return errors.New(missing)
	}
	return nil
}

// findInstalls returns the tunnels of every install the token sees, by
// install id, in the accounts it lists and the accounts of its zones, as the
// purge looks.
func findInstalls(ctx context.Context, api cfapi.API) (map[string][]placedTunnel, error) {
	listed, err := api.Accounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the accounts the token sees: %w", err)
	}
	zones, err := api.Zones(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the zones the token sees: %w", err)
	}
	accounts := make(map[string]bool)
	for _, a := range listed {
		accounts[a.ID] = true
	}
	for _, z := range zones {
		accounts[z.AccountID] = true
	}
	found := make(map[string][]placedTunnel)
	for _, account := range slices.Sorted(maps.Keys(accounts)) {
		// The prefix of the name of every tunnel pco makes.
		tunnels, err := api.Tunnels(ctx, account, planner.TunnelName(""))
		if err != nil {
			return nil, fmt.Errorf("listing the tunnels of account %s: %w", account, err)
		}
		for _, t := range tunnels {
			if id, ok := installOf(t.Name); ok {
				found[id] = append(found[id], placedTunnel{account: account, tunnel: t})
			}
		}
	}
	return found, nil
}

// installOf returns the install whose tunnel has the name. A probe tunnel of
// the credential check is not the tunnel of an install.
func installOf(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, planner.TunnelName(""))
	if !ok {
		return "", false
	}
	if id, _, probe := strings.Cut(rest, "_"); probe && planner.IsProbeTunnel(id, name) {
		return "", false
	}
	return rest, validInstallID(rest) && planner.TunnelName(rest) == name
}

func (r *run) chooseInstall(found map[string][]placedTunnel) (string, error) {
	if id := r.o.InstallID; id != "" {
		if len(found[id]) == 0 {
			return "", fmt.Errorf("the token sees no tunnel of install %s, so the generation its writer used is unknown: "+
				"check the id, or recover with a token that sees the account of its tunnel", id)
		}
		return id, nil
	}
	ids := slices.Sorted(maps.Keys(found))
	switch len(ids) {
	case 0:
		return "", errors.New("the token sees no tunnel of an install of pco; pass --install-id when you know the id")
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("the token sees the tunnels of %d installs, %s: choose one with --install-id",
		len(ids), strings.Join(ids, ", "))
}

// highestGeneration returns the highest generation of a writer of the
// install in the sentinels of its tunnels, 0 when there is none. A
// configuration that cannot be read could hold a higher one, so it refuses
// the recovery rather than leave a guess.
func highestGeneration(ctx context.Context, api cfapi.API, id string, tunnels []placedTunnel) (int, error) {
	highest := 0
	for _, p := range tunnels {
		cfg, err := api.TunnelConfig(ctx, p.account, p.tunnel.ID)
		if err != nil {
			return 0, fmt.Errorf("reading the configuration of tunnel %s (%s) in account %s, which holds the generation of its writer: %w",
				p.tunnel.Name, p.tunnel.ID, p.account, err)
		}
		for _, rule := range cfg.Ingress {
			if w, ok := planner.ParseSentinel(rule.Hostname); ok && w.InstallID == id && w.Generation > highest {
				highest = w.Generation
			}
		}
	}
	return highest, nil
}

// adopt makes id the install of the store, with a writer of a generation
// above the highest one in use, and observe-only settings.
func (r *run) adopt(id string, highest int) error {
	inst, found, err := r.st.Install()
	if err != nil {
		return fmt.Errorf("reading the install: %w", err)
	}
	if found && inst.ID != id {
		return fmt.Errorf("the store holds install %s, not %s, and recovery never replaces an install: "+
			"remove it with pco uninstall first, or recover install %s with --install-id %s", inst.ID, id, inst.ID, inst.ID)
	}
	created := r.now()
	if found && inst.ID == id {
		created = inst.CreatedAt
	}
	w, wfound, err := r.st.Writer()
	if err != nil {
		return fmt.Errorf("reading the writer identity: %w", err)
	}
	if wfound && w.InstallID == id && w.Generation > highest {
		highest = w.Generation
	}
	if err := r.observeOnly(); err != nil {
		return err
	}
	if err := r.saveWriter(id, highest+1); err != nil {
		return err
	}
	r.install = store.Install{ID: id, CreatedAt: created, Profile: store.ProfileHost}
	if err := r.st.SaveInstall(r.install); err != nil {
		return fmt.Errorf("storing the install: %w", err)
	}
	r.ask.Info("store: recovered install %s with writer generation %d; it only observes until pco apply", id, highest+1)
	return nil
}
