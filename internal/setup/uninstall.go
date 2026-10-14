package setup

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
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
	// DetachManaged removes pco's cards on the managed network from the
	// guests first, so that the zone and the vnet can go.
	DetachManaged bool
	// Force removes a node of a cluster that holds the lease and cannot hand
	// it off, or whose connectors may be the only ones serving the shared
	// tunnel.
	Force bool
	// RemoveOthers, on the controller, takes every other node out of the
	// install, which then goes with this node as the last one. It runs once
	// the checks of this node passed and the questions were answered, and
	// before anything is removed here.
	RemoveOthers func(ctx context.Context) error
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
	// others are the registered nodes besides this one that were not
	// removed, and last says there are none: the install goes with this
	// node. Otherwise the node leaves the install, which keeps running on
	// the others, and takes only what is its own.
	others []string
	last   bool
	// entries counts the entries of the registry, removed ones too: with
	// more than one, another node may have put pco into the firewall.
	entries int
	found   survey   // what is on the node, read before anything is asked
	failed  []string // what could not be done
	// connectorsStay says that the connectors could not be removed.
	connectorsStay bool
}

// Uninstall removes pco from the node. It first looks at what is there, lists
// it and asks, and only then removes, in an order that keeps what a later part
// needs: what is at Cloudflare first, as it needs the stored credentials, with
// the connectors stopped before their tunnels are deleted, then the egress
// filter, the web interface and what Proxmox holds, and the store last. Only
// what the manifest lists is removed from Proxmox and of the web interface. A part that fails is reported and the others
// go on, but the store is then kept, so that a second run finishes the rest.
//
// On a node of a cluster that other nodes keep the install running on, the
// node hands the lease off when it holds it, stops and removes what runs on
// it, sets its entry of the registry to removed and deletes its local state;
// what the nodes share, at Cloudflare, in Proxmox and in the store, stays.
func (s *Setup) Uninstall(ctx context.Context, o UninstallOptions) error {
	if err := s.refuseAppliance(); err != nil {
		return err
	}
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
	others := u.others
	if o.RemoveOthers != nil {
		// The whole install goes: this node is the last one once the
		// others are removed, and is checked as such before they are.
		u.others, u.last = nil, true
	}
	if err := u.refuseToOrphan(); err != nil {
		return err
	}
	if err := u.refuseWhatOnlyTheLastMayDo(); err != nil {
		return err
	}
	if err := u.refuseToLeaveAlone(); err != nil {
		return err
	}
	u.survey(ctx)
	u.describe()
	if o.RemoveOthers != nil && len(others) > 0 {
		u.ask.Info("before that, every other node is taken out of the install: %s", strings.Join(others, ", "))
	}
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
	if o.RemoveOthers != nil {
		if err := o.RemoveOthers(ctx); err != nil {
			return err
		}
		if len(others) > 0 {
			u.waitForOthers(ctx)
		}
	}
	if err := u.handoff(ctx); err != nil {
		return err
	}

	if err := u.stopDaemon(ctx); err != nil {
		return err
	}
	u.removeLeaseLock()
	if o.DetachManaged {
		u.detachManaged(ctx)
	}
	if purge {
		u.deleteRecords(ctx)
	}
	u.pruneConnectors(ctx)
	if purge {
		u.deleteTunnels(ctx)
	}
	u.removeGateway(ctx)
	u.removeEgress(ctx)
	u.removeWeb(ctx)
	u.removeFirewall(ctx)
	if u.last {
		u.removeProxmox(ctx)
	}
	if removeCloudflared {
		u.removeCloudflared(ctx)
	}
	if u.last {
		u.removeManagedNetwork(ctx)
	}
	if len(u.failed) > 0 {
		u.ask.Warn("the store is kept, with the credentials and the manifest, so that pco uninstall can finish the rest")
		return fmt.Errorf("uninstall did not finish: %s; run pco uninstall again to finish the rest", strings.Join(u.failed, "; "))
	}
	if !u.last {
		return u.leaveStore()
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
	nodes, err := s.st.Nodes()
	if err != nil && !errors.Is(err, store.ErrNoRoot) {
		return nil, fmt.Errorf("reading the node registry: %w", err)
	}
	for _, n := range nodes {
		if n.Name != u.node && n.State != string(agentproto.Removed) {
			u.others = append(u.others, n.Name)
		}
	}
	slices.Sort(u.others)
	u.last = len(u.others) == 0
	u.entries = len(nodes)
	return u, nil
}

// refuseWhatOnlyTheLastMayDo refuses, on a node that leaves an install other
// nodes keep running, what reaches beyond the node: deleting at Cloudflare
// and taking pco's cards off the guests.
func (u *uninstall) refuseWhatOnlyTheLastMayDo() error {
	if u.last {
		return nil
	}
	still := strings.Join(u.others, ", ") + " still runs it"
	if len(u.others) > 1 {
		still = strings.Join(u.others, ", ") + " still run it"
	}
	switch {
	case u.o.PurgeCloudflare:
		return errors.New("--purge-cloudflare goes with the last node of the install: " + still)
	case u.o.DetachManaged:
		return errors.New("--detach-managed goes with the last node of the install: " + still)
	}
	return nil
}

// refuseToLeaveAlone refuses to remove the node while no other node is
// active, since its connectors may then be the only ones serving the shared
// tunnel; --force removes it regardless.
func (u *uninstall) refuseToLeaveAlone() error {
	if u.last || u.o.Force {
		return nil
	}
	nodes, err := u.st.Nodes()
	if err != nil {
		return fmt.Errorf("reading the node registry: %w", err)
	}
	var states []string
	for _, n := range nodes {
		if n.Name == u.node || n.State == string(agentproto.Removed) {
			continue
		}
		if n.State == string(agentproto.Active) {
			return nil
		}
		states = append(states, n.Name+" is "+cmp.Or(n.State, "not yet admitted"))
	}
	return fmt.Errorf("no other node of the install is active (%s), so the connectors of this node may be the only ones serving "+
		"the shared tunnel; nothing was removed. Admit or undrain another node first, or pass --force", strings.Join(states, ", "))
}

// othersStandDown is how long an uninstall of the whole install waits once
// the other nodes are removed: their agents read the registry every two
// seconds and are refused by the controller at their next request, and
// stand down then, stopping their connectors. The registry must still be
// there, and their connectors gone before the tunnels are deleted.
const othersStandDown = 10 * time.Second

// waitForOthers gives the agents of the nodes just removed the time to
// stand down.
func (u *uninstall) waitForOthers(ctx context.Context) {
	u.ask.Info("waiting %s for the agents of the other nodes to stand down", othersStandDown)
	if err := u.host.sleep(ctx, othersStandDown); err != nil {
		u.ask.Warn("the wait for the other nodes ended early: %v", err)
	}
}

// handoff asks the daemon to hand the lease off while this node holds it:
// another node then takes over at once rather than when the lease runs out.
// Without --force a hand-off that is not possible stops the uninstall.
func (u *uninstall) handoff(ctx context.Context) error {
	if !u.found.holder {
		return nil
	}
	var err error
	if u.daemon == nil {
		err = errors.New("no daemon to ask")
	} else {
		err = u.daemon.Handoff(ctx, "")
	}
	switch {
	case err == nil:
		u.ask.Info("lease: handed off")
		return nil
	case u.o.Force:
		u.ask.Warn("lease: not handed off (%v); another node takes it once it runs out", err)
		return nil
	}
	return fmt.Errorf("this node holds the lease and cannot hand it off (%v); nothing was removed. pco uninstall --force removes "+
		"the node regardless, and another node takes the lease once it runs out", err)
}

// leaveStore takes the node out of the install the other nodes keep: its
// entry becomes removed, under its index, so that its certificate is refused
// and its name keeps its leg MACs; its enrolment goes, and so does its local
// root, with the manifest. The shared roots stay.
func (u *uninstall) leaveStore() error {
	p := u.paths()
	if p.MountCheck != "" {
		if _, err := os.Stat(p.MountCheck); err != nil {
			return fmt.Errorf("leaving the install: %w; run pco uninstall again once it is", store.ErrNotMounted)
		}
	}
	if err := u.markRemoved(); err != nil {
		return fmt.Errorf("%w; run pco uninstall again to finish the rest", err)
	}
	if err := u.st.DeleteEnrolment(u.node); err != nil {
		return fmt.Errorf("removing the enrolment of node %s: %w; run pco uninstall again to finish the rest", u.node, err)
	}
	if err := os.RemoveAll(p.Local); err != nil {
		return fmt.Errorf("removing %s: %w; run pco uninstall again to finish the rest", p.Local, err)
	}
	u.ask.Info("store: removed %s; %s and %s stay with the install", p.Local, p.Cluster, p.Private)
	u.ask.Info("pco is removed from node %s; the install keeps running on %s. The package goes with apt-get purge pco",
		u.node, strings.Join(u.others, ", "))
	return nil
}

// markRemoved sets the node's entry to removed, at the revision it read, and
// reads again when the controller wrote it in between.
func (u *uninstall) markRemoved() error {
	for range registerTries {
		nodes, err := u.st.NodesRev()
		if err != nil {
			return fmt.Errorf("reading the node registry: %w", err)
		}
		i := slices.IndexFunc(nodes, func(n store.NodeRev) bool { return n.Node.Name == u.node })
		if i < 0 {
			return nil
		}
		entry := nodes[i].Node
		if entry.State == string(agentproto.Removed) {
			return nil
		}
		entry.State, entry.StateSince = string(agentproto.Removed), u.now()
		_, err = u.st.SaveNodeIf(entry, nodes[i].Rev)
		switch {
		case errors.Is(err, store.ErrRevision):
			continue
		case err != nil:
			return fmt.Errorf("setting node %s to removed: %w", u.node, err)
		}
		u.ask.Info("node registry: %s is removed", u.node)
		return nil
	}
	return fmt.Errorf("the entry of node %s kept changing while it was set to removed", u.node)
}

// refuseToOrphan refuses --yes without a word on Cloudflare while the store
// holds an install and the credentials that reach what it has there: the
// uninstall would take the credentials and leave the records and tunnels with
// nothing on this node to remove them. A node that leaves an install other
// nodes keep running takes no credential with it.
func (u *uninstall) refuseToOrphan() error {
	if !u.last || !u.o.Yes || u.o.PurgeCloudflare || u.o.KeepCloudflare || u.installID == "" || len(u.creds) == 0 {
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
		u.connectorsStay = true
		return
	}
	u.ask.Info("connectors: stopped and removed")
}

// removeGateway removes the gateway namespace of the attached networks once
// the connectors are gone: its unit is disabled, which stops nothing, and
// pco netns down takes the legs, the namespace, both routes of the service
// prefix and the host's table. While a connector may still run it stays:
// without the unreachable route a request to a service address would follow
// the default route.
func (u *uninstall) removeGateway(ctx context.Context) {
	f := u.found
	if f.netnsUnit {
		if _, err := u.run.Run(ctx, "systemctl", "disable", "--now", netnsUnit); err != nil {
			u.fail("disabling %s: %v", netnsUnit, err)
		}
	}
	switch {
	case !f.gateway:
		return
	case u.connectorsStay:
		u.fail("the gateway namespace %s is kept while connectors may still run", gateway.Namespace)
		return
	}
	if _, err := u.run.Run(ctx, "pco", "netns", "down", "--uninstall"); err != nil {
		u.fail("removing the gateway namespace %s: %v", gateway.Namespace, err)
		return
	}
	u.ask.Info("gateway: removed the namespace %s with its legs, the routes of %s and table inet pco_host",
		gateway.Namespace, gateway.ServicePrefix)
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

// detachManaged removes the cards pco attached on the managed network, right
// after the daemon stopped and can attach none again.
func (u *uninstall) detachManaged(ctx context.Context) {
	f := u.found
	if f.attachmentsErr != nil {
		u.fail("reading the attachments of the managed network: %v; no card is removed", f.attachmentsErr)
		return
	}
	d := Detacher{Run: u.run, Ask: u.ask, Sleep: u.host.sleep}
	if _, err := d.Detach(ctx, f.attachments); err != nil {
		u.fail("%v", err)
	}
}

// removeManagedNetwork removes the vnet and the zone of the managed network
// setup made, last and only once no guest of the cluster has a card on the
// vnet: the apply takes the bridge away from under such a card.
func (u *uninstall) removeManagedNetwork(ctx context.Context) {
	m := u.manifest
	if !m.CreatedZone && !m.CreatedVnet {
		return
	}
	cards, err := CardsOn(ctx, u.run, ManagedVnet)
	if err != nil {
		u.fail("the SDN zone %s is kept: %v", ManagedZone, err)
		return
	}
	if len(cards) > 0 {
		u.fail("%s", u.keptFor(cards))
		return
	}
	s := SDN{Run: u.run, Ask: u.ask, Ours: m}
	if err := s.Remove(ctx); err != nil {
		u.fail("%v", err)
	}
}

// keptFor says why the zone stays while cards are on the vnet, and what to do.
func (u *uninstall) keptFor(cards []Card) string {
	if !u.o.DetachManaged {
		return fmt.Sprintf("the SDN zone %s is kept: guests %s still have a card on %s; detach them first or pass --detach-managed",
			ManagedZone, strings.Join(Owners(cards), ", "), ManagedVnet)
	}
	var others []Card
	for _, c := range cards {
		if !slices.ContainsFunc(u.found.attachments, func(a store.Attachment) bool {
			return a.Owner == c.Owner && a.NIC == c.NIC && a.MAC == c.MAC
		}) {
			others = append(others, c)
		}
	}
	if len(others) > 0 {
		return fmt.Sprintf("the SDN zone %s is kept: guests %s still have a card on %s, which pco did not attach: remove it, "+
			"then run pco uninstall again", ManagedZone, strings.Join(Owners(others), ", "), ManagedVnet)
	}
	return fmt.Sprintf("the SDN zone %s is kept: guests %s still have a card on %s; a VM without network hotplug drops it "+
		"when it restarts, then run pco uninstall again", ManagedZone, strings.Join(Owners(cards), ", "), ManagedVnet)
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
