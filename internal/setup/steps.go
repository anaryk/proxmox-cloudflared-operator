package setup

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
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
	if r.version, err = ParsePVEVersion(out); err != nil {
		return err
	}
	if !r.version.Supported() {
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

// prepareStore makes the store and the identity of the install, or keeps the
// ones there. A store that other nodes run the install from, which the
// cluster filesystem shares, is the one this node joins.
func (r *run) prepareStore(ctx context.Context) error {
	if !r.o.Recover && !r.o.NewInstall {
		if err := r.refuseBesideConnectors(ctx); err != nil {
			return err
		}
	}
	if err := r.st.Init(); err != nil {
		return fmt.Errorf("creating the store: %w", err)
	}
	if err := r.readOthers(); err != nil {
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
	if inst.Restored != nil && r.o.NewInstall {
		return fmt.Errorf("install %s was restored from a bundle, and --new-install would start another install beside it "+
			"and leave the restore half done: run pco setup, or pco setup --recover; nothing was changed", inst.ID)
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
		if err := saveWriter(r.st, r.rand, inst.ID, 1); err != nil {
			return err
		}
		r.ask.Warn("store: install %s had no writer identity; wrote one of generation 1. If this install wrote to "+
			"Cloudflare before, run pco setup --recover to take a generation above the one in use", inst.ID)
	case w.InstallID != inst.ID:
		return fmt.Errorf("leader.json names install %s, but this is install %s; run pco setup --recover", w.InstallID, inst.ID)
	case inst.Restored != nil && inst.Restored.TakenOverBy == "":
		// Whether it joins the nodes of the bundle or takes the store into
		// another cluster, the cluster step says.
		r.ask.Info("store: install %s kept, restored from a bundle made on %s", inst.ID, dateOf(inst.Restored.BundleMadeAt))
	case len(r.others) > 0:
		r.ask.Info("store: install %s kept; joining the nodes %s", inst.ID, strings.Join(r.others, ", "))
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
	if err := observeOnly(r.st); err != nil {
		return err
	}
	if err := saveWriter(r.st, r.rand, id, 1); err != nil {
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
func observeOnly(st *store.Store) error {
	settings, err := st.Settings()
	if err != nil {
		return fmt.Errorf("reading the settings: %w", err)
	}
	settings.ObserveOnly = true
	if err := st.SaveSettings(settings); err != nil {
		return fmt.Errorf("storing the settings: %w", err)
	}
	return nil
}

// saveWriter stores a writer of the install at the generation, with a nonce
// drawn from rand.
func saveWriter(st *store.Store, rand io.Reader, installID string, generation int) error {
	nonce, err := newNonce(rand)
	if err != nil {
		return err
	}
	if err := st.SaveWriter(planner.Writer{InstallID: installID, Generation: generation, Nonce: nonce}); err != nil {
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
	if err := r.readOthers(); err != nil {
		return err
	}
	r.install = inst
	r.ask.Info("store: install %s found", inst.ID)
	return nil
}

// registerTries is how often register reads the entry again when another
// writer changed it in between.
const registerTries = 3

// register writes the node's entry of the registry: a new node joining
// others is joining, without an index, which the controller gives it; the
// first node of an install gets its state at the first start of the daemon.
// An entry that is there has its fields refreshed and its state, index and
// what else the controller decided left alone; a removed node joins again
// under the index its name had, so its leg MACs stay.
func (r *run) register(context.Context) error {
	if err := r.writeEntry(); err != nil {
		return err
	}
	return r.record(func(m *Manifest) { m.Node = r.o.Node })
}

// writeEntry writes the entry nextEntry makes at the revision it read, and
// reads again when another writer came between.
func (r *run) writeEntry() error {
	for range registerTries {
		entry, rev, err := r.nextEntry()
		switch {
		case err != nil:
			return err
		case entry == nil:
			return nil
		}
		_, err = r.st.SaveNodeIf(*entry, rev)
		switch {
		case errors.Is(err, store.ErrRevision):
			continue
		case err != nil:
			return fmt.Errorf("registering node %s: %w", r.o.Node, err)
		}
		return nil
	}
	return fmt.Errorf("the entry of node %s kept changing while it was registered; run pco setup again", r.o.Node)
}

// nextEntry is the entry to write and the revision it was read at, or nil
// when the stored one is current.
func (r *run) nextEntry() (*store.NodeEntry, int64, error) {
	nodes, err := r.st.NodesRev()
	if err != nil {
		return nil, 0, fmt.Errorf("reading the node registry: %w", err)
	}
	entry := store.NodeEntry{
		Name: r.o.Node, Version: version.Version, Since: r.now(), Kind: string(pki.KindHost), AgentAddress: r.agentAddress,
		Protocol: store.ProtocolRange{Min: agentproto.ProtocolMin, Max: agentproto.ProtocolMax}, Schema: store.SchemaVersion(),
		CertSerial: r.serial, CertSHA256: r.fingerprint,
	}
	i := slices.IndexFunc(nodes, func(n store.NodeRev) bool { return n.Node.Name == r.o.Node })
	if i < 0 {
		if len(r.others) > 0 {
			entry.State, entry.StateSince = string(agentproto.Joining), r.now()
			r.ask.Info("node registry: registered %s as joining; the controller admits it", r.o.Node)
		} else {
			r.ask.Info("node registry: registered %s", r.o.Node)
		}
		return &entry, 0, nil
	}
	have := nodes[i].Node
	entry.Since, entry.Index, entry.State, entry.StateSince = have.Since, have.Index, have.State, have.StateSince
	entry.ControllerEligible, entry.ContestedUses, entry.Drained = have.ControllerEligible, have.ContestedUses, have.Drained
	switch {
	case have.State == string(agentproto.Removed):
		entry.State, entry.StateSince, entry.ContestedUses = string(agentproto.Joining), r.now(), nil
		if have.Index != nil {
			r.ask.Info("node registry: %s joins again as joining, under its old index %d", r.o.Node, *have.Index)
		} else {
			r.ask.Info("node registry: %s joins again as joining", r.o.Node)
		}
	case reflect.DeepEqual(entry, have):
		r.ask.Info("node registry: %s is registered, nothing needed", r.o.Node)
		return nil, 0, nil
	default:
		r.ask.Info("node registry: refreshed %s", r.o.Node)
	}
	return &entry, nodes[i].Rev, nil
}

func (r *run) startService(ctx context.Context) error {
	if !r.unitInstalled(serviceUnit) {
		r.ask.Warn("%s is not installed, as when pco runs from a build tree: start the daemon with pco daemon", serviceUnit)
		r.nextSteps()
		return nil
	}
	if err := r.enableEgress(ctx); err != nil {
		return err
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

// enableEgress enables the unit that loads the egress table and starts it, so
// that the table is there at every boot, also before a connector exists to
// start it, and before the daemon, which it is ordered ahead of.
func (r *run) enableEgress(ctx context.Context) error {
	if !r.unitInstalled(egressUnit) {
		return nil
	}
	if _, err := r.run.Run(ctx, "systemctl", "enable", "--now", egressUnit); err != nil {
		return fmt.Errorf("enabling %s: %w", egressUnit, err)
	}
	r.ask.Info("%s: enabled and running", egressUnit)
	return nil
}

func (r *run) nextSteps() {
	r.ask.Info("next: tag a guest with %s and write its routes into its notes, then run pco plan to see what "+
		"would change and pco apply to make it so", cmp.Or(r.gate, defaultTags()[0]))
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
func newNonce(rand io.Reader) (string, error) {
	const size, limit = len(nonceAlphabet), 256 - 256%len(nonceAlphabet)
	nonce := make([]byte, 0, 8)
	b := make([]byte, 1)
	for len(nonce) < cap(nonce) {
		if _, err := io.ReadFull(rand, b); err != nil {
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
