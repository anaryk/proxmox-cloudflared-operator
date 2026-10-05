package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// UninstallOptions are the answers to the questions of uninstall given in
// advance. Yes answers the question whether to remove pco, and nothing else:
// deleting at Cloudflare and removing cloudflared reach beyond this node. With
// Yes, cloudflared stays unless RemoveCloudflared says otherwise, and an
// install with credentials needs PurgeCloudflare or KeepCloudflare: once the
// store is gone, nothing on the node can remove what the install has at
// Cloudflare. Without Yes, what no flag answers is asked.
type UninstallOptions struct {
	Yes               bool // remove pco without asking
	PurgeCloudflare   bool // delete the records and tunnels of the install at Cloudflare
	KeepCloudflare    bool // leave Cloudflare as it is, without looking at it
	RemoveCloudflared bool // remove the cloudflared package and source setup installed
}

func (o UninstallOptions) check() error {
	if o.PurgeCloudflare && o.KeepCloudflare {
		return errors.New("--purge-cloudflare and --keep-cloudflare do not go together")
	}
	return nil
}

const (
	// tunnelsDir is where the connectors' files are kept, below the local root.
	tunnelsDir = "tunnels"
	// connectorUnits matches the units of the connectors.
	connectorUnits = "pco-cloudflared@*.service"
	// egressTable is the nftables table of the egress filter.
	egressTable = "pco_egress"
)

// uninstall is the state of one Uninstall.
type uninstall struct {
	*Setup
	o         UninstallOptions
	manifest  Manifest
	installID string // empty when the store holds no install
	creds     []store.Credential
	// served are the zones the install ever served, by id; nil when that is
	// not known, as without a memory of the install, and every zone counts.
	served map[string]bool
	node   string
	found  survey   // what is on the node, read before anything is asked
	failed []string // what could not be done
}

// Uninstall removes pco from the node. It first looks at what is there, lists
// it and asks, and only then removes, in an order that keeps what a later part
// needs: what is at Cloudflare first, as it needs the stored credentials, with
// the connectors stopped before their tunnels are deleted, then the egress
// filter and what Proxmox holds, and the store last. Only what the manifest
// lists is removed from Proxmox. A part that fails is reported and the others
// go on, but the store is then kept, so that a second run finishes the rest.
func (s *Setup) Uninstall(ctx context.Context, o UninstallOptions) error {
	if err := o.check(); err != nil {
		return err
	}
	if s.host.euid() != 0 {
		return errors.New("pco uninstall must run as root")
	}
	u, err := s.newUninstall(o)
	if err != nil {
		return err
	}
	if err := u.refuseToOrphan(); err != nil {
		return err
	}
	u.survey(ctx)
	u.describe()
	if !o.Yes {
		ok, err := u.ask.Confirm("Remove pco from this node?", false)
		if err != nil {
			return err
		}
		if !ok {
			return ErrAborted
		}
	}
	purge, err := u.decidePurge()
	if err != nil {
		return err
	}
	removeCloudflared, err := u.decideCloudflared()
	if err != nil {
		return err
	}

	if err := u.stopDaemon(ctx); err != nil {
		return err
	}
	if purge {
		u.deleteRecords(ctx)
	}
	u.pruneConnectors(ctx)
	if purge {
		u.deleteTunnels(ctx)
	}
	u.removeEgress(ctx)
	u.removeProxmox(ctx)
	if removeCloudflared {
		u.removeCloudflared(ctx)
	}
	if len(u.failed) > 0 {
		u.ask.Warn("the store is kept, with the credentials and the manifest, so that pco uninstall can finish the rest")
		return fmt.Errorf("uninstall did not finish: %s; run pco uninstall again to finish the rest", strings.Join(u.failed, "; "))
	}
	return u.removeStore()
}

func (s *Setup) newUninstall(o UninstallOptions) (*uninstall, error) {
	u := &uninstall{Setup: s, o: o}
	m, _, err := readManifest(s.manifestPath())
	if err != nil {
		return nil, fmt.Errorf("reading the manifest, which says what setup created: %w", err)
	}
	u.manifest = m
	inst, found, err := s.st.Install()
	switch {
	case errors.Is(err, store.ErrNoRoot):
	case err != nil:
		return nil, fmt.Errorf("reading the install: %w", err)
	case found:
		u.installID = inst.ID
		if m, err := s.st.EngineMemory(); err == nil && m.InstallID == inst.ID {
			u.served = servedZones(s.st, inst.ID)
		}
	}
	u.creds, err = s.st.Credentials()
	if err != nil && !errors.Is(err, store.ErrNoRoot) {
		return nil, fmt.Errorf("reading the credentials: %w", err)
	}
	u.node = m.Node
	if u.node == "" {
		name, err := s.host.hostname()
		if err != nil {
			return nil, fmt.Errorf("reading the host name: %w", err)
		}
		u.node, _, _ = strings.Cut(name, ".")
	}
	return u, nil
}

// refuseToOrphan refuses --yes without a word on Cloudflare while the store
// holds an install and the credentials that reach what it has there: the
// uninstall would take the credentials and leave the records and tunnels with
// nothing on this node to remove them.
func (u *uninstall) refuseToOrphan() error {
	if !u.o.Yes || u.o.PurgeCloudflare || u.o.KeepCloudflare || u.installID == "" || len(u.creds) == 0 {
		return nil
	}
	return fmt.Errorf("install %s may have DNS records and a tunnel at Cloudflare, and the uninstall removes the "+
		"credentials that reach them: say what becomes of them with --purge-cloudflare, which deletes them, or "+
		"--keep-cloudflare, which leaves them, and then nothing on this node can remove them later", u.installID)
}

// fail notes something that could not be done, and says so.
func (u *uninstall) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	u.failed = append(u.failed, msg)
	u.ask.Warn("%s", msg)
}

// decidePurge reports whether to delete what the install has at Cloudflare.
// What cannot be listed cannot be deleted, and the store with the credentials
// is kept until it is, or until the operator says to leave it.
func (u *uninstall) decidePurge() (bool, error) {
	f := u.found
	if f.listed && f.cloudflareErr != nil {
		u.fail("what install %s has at Cloudflare cannot be listed (%v): to finish, run pco uninstall --purge-cloudflare "+
			"again once Cloudflare answers, or pco uninstall --keep-cloudflare to leave Cloudflare as it is",
			u.installID, f.cloudflareErr)
	}
	switch {
	case u.o.PurgeCloudflare:
		return true, nil
	case !f.listed || f.cloudflareErr != nil || f.cloudflare.empty():
		return false, nil
	}
	u.showCloudflare()
	return u.ask.Confirm("Delete these at Cloudflare as well?", false)
}

// decideCloudflared reports whether to remove what setup installed of
// cloudflared, which other software of the node may use.
func (u *uninstall) decideCloudflared() (bool, error) {
	m := u.manifest
	switch {
	case !m.InstalledCloudflared && !m.AddedAptSource && !m.AddedKeyring:
		return false, nil
	case u.o.RemoveCloudflared:
		return true, nil
	case u.o.Yes:
		u.ask.Info("cloudflared: kept; --remove-cloudflared removes what setup installed of it")
		return false, nil
	}
	return u.ask.Confirm("Remove the cloudflared package and its apt source, which setup installed?", false)
}

// stopDaemon stops the daemon, which would otherwise make again what is
// removed. One that does not stop, and one run by hand, which systemd cannot
// stop, stop the uninstall before anything goes.
func (u *uninstall) stopDaemon(ctx context.Context) error {
	byHand, err := u.runsByHand(ctx)
	switch {
	case err != nil:
		return fmt.Errorf("%w; nothing was removed", err)
	case byHand:
		return fmt.Errorf("%w, then run pco uninstall again; nothing was removed", u.errRunsByHand())
	}
	if !u.found.unit {
		return nil
	}
	if _, err := u.run.Run(ctx, "systemctl", "disable", "--now", serviceUnit); err != nil {
		return fmt.Errorf("stopping %s: %w; nothing was removed: stop the daemon, then run pco uninstall again", serviceUnit, err)
	}
	u.ask.Info("%s: stopped and disabled", serviceUnit)
	// The lock was its, or one more daemon started meanwhile.
	locked, err := u.daemonLocked()
	switch {
	case err != nil:
		return fmt.Errorf("%s was stopped and disabled, but %w; nothing else was removed", serviceUnit, err)
	case locked:
		return fmt.Errorf("%s was stopped and disabled, but %w, then run pco uninstall again; nothing else was removed",
			serviceUnit, u.errRunsByHand())
	}
	return nil
}

func (u *uninstall) pruneConnectors(ctx context.Context) {
	m := connector.NewManager(unitControl{u.run}, filepath.Join(u.paths().Local, tunnelsDir), nil, zerolog.Nop())
	if err := m.Prune(ctx, nil); err != nil {
		u.fail("removing the connectors: %v", err)
		return
	}
	u.ask.Info("connectors: stopped and removed")
}

func (u *uninstall) removeEgress(ctx context.Context) {
	f := u.found
	table := f.table
	if f.egressUnit {
		if _, err := u.run.Run(ctx, "systemctl", "disable", "--now", egressUnit); err != nil {
			u.fail("stopping %s: %v", egressUnit, err)
		}
		if f.nft && f.nftErr == nil {
			// The unit may have taken its table with it.
			_, table, f.nftErr = u.egressTableThere(ctx)
		}
	}
	switch {
	case f.nftErr != nil:
		u.fail("%v", f.nftErr)
		return
	case !f.nft:
		u.ask.Info("egress filter: nothing needed, nft is not installed")
		return
	case !table:
		u.ask.Info("egress filter: nothing needed")
		return
	}
	if _, err := u.run.Run(ctx, "nft", "delete", "table", "inet", egressTable); err != nil {
		u.fail("removing table inet %s: %v", egressTable, err)
		return
	}
	u.ask.Info("egress filter: removed table inet %s", egressTable)
}

// removeStore removes the node from the registry and the three roots of the
// store. The shared ones are only touched while the cluster filesystem is
// mounted: what lies under its mount point otherwise is not the store.
func (u *uninstall) removeStore() error {
	p := u.paths()
	if p.MountCheck != "" {
		if _, err := os.Stat(p.MountCheck); err != nil {
			return fmt.Errorf("removing the store: %w; run pco uninstall again once it is", store.ErrNotMounted)
		}
	}
	if err := u.st.DeleteNode(u.node); err != nil && !errors.Is(err, store.ErrNoRoot) {
		return fmt.Errorf("removing node %s from the registry: %w; run pco uninstall again to finish the rest", u.node, err)
	}
	for _, dir := range []string{p.Cluster, p.Private, p.Local} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("removing %s: %w; run pco uninstall again to finish the rest", dir, err)
		}
	}
	u.ask.Info("store: removed %s, %s and %s", p.Cluster, p.Private, p.Local)
	u.ask.Info("pco is removed from this node; the package goes with apt-get purge pco")
	return nil
}
