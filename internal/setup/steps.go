package setup

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/version"
)

func (r *run) preflight(ctx context.Context) error {
	if r.host.euid() != 0 {
		return errors.New("pco setup must run as root")
	}
	out, err := r.run.Run(ctx, "pveversion")
	if err != nil {
		return fmt.Errorf("reading the version of Proxmox VE, is this a Proxmox VE node? %w", err)
	}
	if r.version, err = parsePVEVersion(out); err != nil {
		return err
	}
	if !r.version.supported() {
		return fmt.Errorf("version %s of Proxmox VE is not supported: pco needs 8.4 or later, or 9", r.version.text)
	}
	if _, err := r.run.Run(ctx, "mountpoint", "-q", r.host.pveDir); err != nil {
		return fmt.Errorf("%s is not mounted, is pve-cluster running? %w", r.host.pveDir, err)
	}
	out, err = r.run.Run(ctx, "dpkg", "--print-architecture")
	if err != nil {
		return fmt.Errorf("reading the architecture: %w", err)
	}
	switch arch := strings.TrimSpace(out); arch {
	case "amd64", "arm64":
		r.ask.Info("preflight: Proxmox VE %s on %s", r.version.text, arch)
		return nil
	default:
		return fmt.Errorf("architecture %q is not supported: pco is built for amd64 and arm64", arch)
	}
}

// refuseOtherNode refuses a store whose node registry names another node:
// until the cluster support, pco runs on one node.
func (r *run) refuseOtherNode() error {
	nodes, err := r.st.Nodes()
	if err != nil {
		return fmt.Errorf("reading the node registry: %w", err)
	}
	for _, n := range nodes {
		if n.Name != r.o.Node {
			return fmt.Errorf("pco is already set up on node %s; cluster support arrives in a later release", n.Name)
		}
	}
	return nil
}

// prepareStore makes the store and the identity of the install, or keeps the
// ones there.
func (r *run) prepareStore(ctx context.Context) error {
	if err := r.st.Init(); err != nil {
		return fmt.Errorf("creating the store: %w", err)
	}
	if err := r.refuseOtherNode(); err != nil {
		return err
	}
	if r.o.Recover {
		return r.recover(ctx)
	}
	inst, found, err := r.st.Install()
	switch {
	case err != nil:
		return fmt.Errorf("reading the install: %w", err)
	case !found:
		return r.createInstall()
	}
	r.install = inst
	w, found, err := r.st.Writer()
	switch {
	case err != nil:
		return fmt.Errorf("reading the writer identity: %w", err)
	case !found:
		// Generation 1 is fenced: a writer above it is never taken for this
		// one. If the install wrote to Cloudflare before, its daemon stops
		// writing until a recovery takes a generation above the one in use.
		if err := r.saveWriter(inst.ID, 1); err != nil {
			return err
		}
		r.ask.Warn("store: install %s had no writer identity; wrote one of generation 1. If this install wrote to "+
			"Cloudflare before, run pco setup --recover to take a generation above the one in use", inst.ID)
	case w.InstallID != inst.ID:
		return fmt.Errorf("leader.json names install %s, but this is install %s; run pco setup --recover", w.InstallID, inst.ID)
	default:
		r.ask.Info("store: install %s kept, nothing needed", inst.ID)
	}
	return nil
}

func (r *run) createInstall() error {
	id, err := r.newInstallID()
	if err != nil {
		return err
	}
	if err := r.observeOnly(); err != nil {
		return err
	}
	if err := r.saveWriter(id, 1); err != nil {
		return err
	}
	// The install is stored last: a store that has one is set up.
	r.install = store.Install{ID: id, CreatedAt: r.now(), Profile: store.ProfileHost}
	if err := r.st.SaveInstall(r.install); err != nil {
		return fmt.Errorf("storing the install: %w", err)
	}
	r.ask.Info("store: created install %s; it only observes until pco apply", id)
	return nil
}

// observeOnly stores the settings, the ones there or the defaults, in
// observe-only mode.
func (r *run) observeOnly() error {
	settings, err := r.st.Settings()
	if err != nil {
		return fmt.Errorf("reading the settings: %w", err)
	}
	settings.ObserveOnly = true
	if err := r.st.SaveSettings(settings); err != nil {
		return fmt.Errorf("storing the settings: %w", err)
	}
	return nil
}

func (r *run) saveWriter(installID string, generation int) error {
	nonce, err := r.newNonce()
	if err != nil {
		return err
	}
	if err := r.st.SaveWriter(planner.Writer{InstallID: installID, Generation: generation, Nonce: nonce}); err != nil {
		return fmt.Errorf("storing the writer identity: %w", err)
	}
	return nil
}

// requireInstall is the store step of a repair, which only repairs what a
// setup made.
func (r *run) requireInstall(context.Context) error {
	inst, found, err := r.st.Install()
	switch {
	case errors.Is(err, store.ErrNoRoot), err == nil && !found:
		return errors.New("pco is not set up on this node; run pco setup")
	case err != nil:
		return fmt.Errorf("reading the install: %w", err)
	}
	if err := r.refuseOtherNode(); err != nil {
		return err
	}
	r.install = inst
	r.ask.Info("store: install %s found", inst.ID)
	return nil
}

func (r *run) register(context.Context) error {
	if err := r.refuseOtherNode(); err != nil {
		return err
	}
	nodes, err := r.st.Nodes()
	if err != nil {
		return fmt.Errorf("reading the node registry: %w", err)
	}
	entry := store.NodeEntry{Name: r.o.Node, Version: version.Version, Since: r.now()}
	switch {
	case len(nodes) == 0:
		if err := r.st.SaveNode(entry); err != nil {
			return fmt.Errorf("registering node %s: %w", r.o.Node, err)
		}
		r.ask.Info("node registry: registered %s", r.o.Node)
	case nodes[0].Version != version.Version:
		entry.Since = nodes[0].Since
		if err := r.st.SaveNode(entry); err != nil {
			return fmt.Errorf("registering node %s: %w", r.o.Node, err)
		}
		r.ask.Info("node registry: %s now runs version %s", r.o.Node, version.Version)
	default:
		r.ask.Info("node registry: %s is registered, nothing needed", r.o.Node)
	}
	return r.record(func(m *Manifest) { m.Node = r.o.Node })
}

func (r *run) startService(ctx context.Context) error {
	if !r.unitInstalled(serviceUnit) {
		r.ask.Warn("%s is not installed, as when pco runs from a build tree: start the daemon with pco daemon", serviceUnit)
		r.nextSteps()
		return nil
	}
	// A daemon run by hand holds the lock of the node; another one started
	// next to it would not start.
	byHand, err := r.runsByHand(ctx)
	if err != nil {
		return err
	}
	if byHand {
		if _, err := r.run.Run(ctx, "systemctl", "enable", serviceUnit); err != nil {
			return fmt.Errorf("enabling %s: %w", serviceUnit, err)
		}
		r.ask.Warn("a pco daemon runs on this node outside systemd (it holds %s): %s is enabled but not started; "+
			"stop the daemon run by hand, then start %s", r.lockPath(), serviceUnit, serviceUnit)
		if r.newPVEToken {
			r.ask.Warn("a new Proxmox token reaches a daemon only when it starts")
		}
		r.nextSteps()
		return nil
	}
	// The daemon reads the Proxmox token when it starts, so one that may run
	// with the old token is restarted, if it runs.
	if r.newPVEToken && (r.running == nil || *r.running) {
		if _, err := r.run.Run(ctx, "systemctl", "try-restart", serviceUnit); err != nil {
			return fmt.Errorf("restarting %s: %w", serviceUnit, err)
		}
	}
	if _, err := r.run.Run(ctx, "systemctl", "enable", "--now", serviceUnit); err != nil {
		return fmt.Errorf("starting %s: %w", serviceUnit, err)
	}
	r.ask.Info("%s: enabled and running", serviceUnit)
	r.nextSteps()
	return nil
}

func (r *run) nextSteps() {
	r.ask.Info("next: tag a guest with %s and write its routes into its notes, then run pco plan to see what "+
		"would change and pco apply to make it so", gateTags()[0])
}

// newInstallID returns 12 random lower-case hex characters.
func (s *Setup) newInstallID() (string, error) { return s.randomHex(6) }

// randomHex returns 2n random lower-case hex characters.
func (s *Setup) randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(s.rand, b); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

const nonceAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// newNonce returns 8 random lower-case letters and digits. A byte at or above
// the largest multiple of the alphabet's size is drawn again, so that every
// character is as likely.
func (s *Setup) newNonce() (string, error) {
	const size, limit = len(nonceAlphabet), 256 - 256%len(nonceAlphabet)
	nonce := make([]byte, 0, 8)
	b := make([]byte, 1)
	for len(nonce) < cap(nonce) {
		if _, err := io.ReadFull(s.rand, b); err != nil {
			return "", fmt.Errorf("reading random bytes: %w", err)
		}
		if int(b[0]) < limit {
			nonce = append(nonce, nonceAlphabet[int(b[0])%size])
		}
	}
	return string(nonce), nil
}

func validInstallID(id string) bool {
	return len(id) == 12 && strings.Trim(id, "0123456789abcdef") == ""
}
