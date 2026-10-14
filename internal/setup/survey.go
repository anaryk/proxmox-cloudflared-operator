package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// survey is what uninstall finds on the node before it asks anything, read
// without changing anything: what is not there is neither listed nor removed.
type survey struct {
	unit, egressUnit bool     // the unit files of pco and of the egress filter
	webUnit          bool     // the unit file of the web interface
	netnsUnit        bool     // the unit file of the gateway namespace
	gateway          bool     // the gateway namespace is pinned
	connectors       []string // the tunnel ids of the connectors
	connectorsErr    error
	nft              bool // nft runs
	table            bool // the table of the egress filter is there
	nftErr           error
	proxmox          proxmoxState
	proxmoxErr       error
	listed           bool // Cloudflare was looked at
	cloudflare       cfObjects
	cloudflareErr    error
	stores           []string // the roots of the store that are there
	registered       bool     // the node registry names this node
	// firewall is what the datacenter firewall holds of pco, read when the
	// manifest says setup put something there or another node may have.
	firewallRead bool
	firewall     fwState
	firewallErr  error
	// holder says that leader.json names this node as the holder of the
	// lease and its daemon runs, so the lease is handed off before it stops.
	holder bool
	// cards are the cards of the guests of the cluster on the managed vnet,
	// read when setup made the vnet or its zone.
	cards    []Card
	cardsErr error
	// attachments are the cards pco attached, read for --detach-managed.
	attachments    []store.Attachment
	attachmentsErr error
}

func (u *uninstall) survey(ctx context.Context) {
	f := &u.found
	f.unit = u.unitInstalled(serviceUnit)
	f.egressUnit = u.unitInstalled(egressUnit)
	f.webUnit = u.unitInstalled(webUnit)
	f.netnsUnit = u.unitInstalled(netnsUnit)
	if _, err := os.Stat(u.host.netnsPin); err == nil {
		f.gateway = true
	}
	f.connectors, f.connectorsErr = u.listConnectors(ctx)
	f.nft, f.table, f.nftErr = u.egressTableThere(ctx)
	if u.manifest.inProxmox() && u.last {
		f.proxmox, f.proxmoxErr = u.readProxmox(ctx)
	}
	if u.looksAtFirewall() {
		f.firewallRead = true
		f.firewall, f.firewallErr = readFirewall(ctx, u.run)
	}
	if !u.last && u.leaseHeldHere() {
		f.holder, _ = u.serviceActive(ctx)
	}
	if (u.manifest.CreatedZone || u.manifest.CreatedVnet) && u.last {
		f.cards, f.cardsErr = CardsOn(ctx, u.run, ManagedVnet)
	}
	if u.o.DetachManaged {
		f.attachments, f.attachmentsErr = u.attachments()
	}
	if u.looksAtCloudflare() {
		f.listed = true
		f.cloudflare, f.cloudflareErr = u.listCloudflare(ctx)
	}
	p := u.paths()
	for _, dir := range []string{p.Cluster, p.Private, p.Local} {
		if _, err := os.Stat(dir); err == nil {
			f.stores = append(f.stores, dir)
		}
	}
	if nodes, err := u.st.Nodes(); err == nil {
		f.registered = slices.ContainsFunc(nodes, func(n store.NodeEntry) bool { return n.Name == u.node })
	}
}

// attachments are the cards pco attached on the managed network, by owner;
// a store without a root has none.
func (u *uninstall) attachments() ([]store.Attachment, error) {
	have, err := u.st.Attachments()
	if errors.Is(err, store.ErrNoRoot) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]store.Attachment, 0, len(have))
	for _, owner := range slices.Sorted(maps.Keys(have)) {
		out = append(out, have[owner])
	}
	return out, nil
}

// looksAtCloudflare reports whether Cloudflare is to be listed: for a purge,
// and for the question whether to purge, which is asked without Yes. Yes with
// an install and credentials comes with one of the two flags. A node that
// leaves an install other nodes keep running has nothing to delete there.
func (u *uninstall) looksAtCloudflare() bool {
	return u.last && u.installID != "" && len(u.creds) > 0 && !u.o.KeepCloudflare
}

// leaseHeldHere reports whether leader.json names this node as the holder
// of the lease that has not released it.
func (u *uninstall) leaseHeldHere() bool {
	leader, _, found, err := u.st.Leader()
	return err == nil && found && leader.Node == u.node && leader.ReleasedAt.IsZero()
}

// listConnectors returns the tunnel ids of the connectors that have files or
// a unit.
func (u *uninstall) listConnectors(ctx context.Context) ([]string, error) {
	ids := make(map[string]bool)
	entries, err := os.ReadDir(filepath.Join(u.paths().Local, tunnelsDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		for _, ext := range []string{".token", ".env"} {
			if id, ok := strings.CutSuffix(e.Name(), ext); ok && !strings.HasPrefix(id, ".") {
				ids[id] = true
			}
		}
	}
	units, err := unitControl{u.run}.ListUnits(ctx, connectorUnits)
	for _, unit := range units {
		ids[strings.TrimSuffix(strings.TrimPrefix(unit, "pco-cloudflared@"), ".service")] = true
	}
	return slices.Sorted(maps.Keys(ids)), err
}

// egressTableThere reports whether nft runs and whether the table of the
// egress filter is there. A missing nft is no error, and neither is a missing
// table; any other failure is.
func (u *uninstall) egressTableThere(ctx context.Context) (nft, table bool, err error) {
	out, err := u.run.Run(ctx, "nft", "list", "tables")
	switch {
	case errors.Is(err, ErrCommandNotFound):
		return false, false, nil
	case err != nil:
		return true, false, fmt.Errorf("listing the nftables tables: %w", err)
	}
	for line := range strings.Lines(out) {
		if strings.Join(strings.Fields(line), " ") == "table inet "+egressTable {
			table = true
		}
	}
	return true, table, nil
}

// describe lists what the uninstall is about to remove, as the survey found
// it.
func (u *uninstall) describe() {
	f := u.found
	u.ask.Info("pco uninstall removes from this node:")
	if f.unit {
		u.ask.Info("  the daemon: %s is stopped and disabled", serviceUnit)
	}
	switch {
	case f.connectorsErr != nil:
		u.ask.Info("  the connectors, as far as they can be found: %v", f.connectorsErr)
	case len(f.connectors) > 0:
		u.ask.Info("  the connectors of %d tunnels: their pco-cloudflared@ units and files", len(f.connectors))
	}
	if f.netnsUnit {
		u.ask.Info("  the unit of the gateway namespace, %s", netnsUnit)
	}
	if f.gateway {
		u.ask.Info("  the gateway namespace %s with its legs, the routes of %s and table inet pco_host",
			gateway.Namespace, gateway.ServicePrefix)
	}
	if f.egressUnit {
		u.ask.Info("  the unit of the egress filter, %s", egressUnit)
	}
	switch {
	case f.nftErr != nil:
		u.ask.Info("  the egress filter, which cannot be looked at: %v", f.nftErr)
	case f.table:
		u.ask.Info("  the egress filter: table inet %s", egressTable)
	}
	u.describeWeb()
	u.describeFirewall()
	if !u.last {
		u.describeLeaving()
		return
	}
	u.describeProxmox()
	u.describeManaged()
	if len(f.stores) > 0 {
		u.ask.Info("  the store: %s", strings.Join(f.stores, ", "))
		if len(u.creds) > 0 {
			u.ask.Info("    with %d Cloudflare credentials", len(u.creds))
		}
		if f.registered {
			u.ask.Info("    and node %s in the registry", u.node)
		}
	}
	u.describeCloudflare()
}

// describeLeaving says what a node that leaves an install other nodes keep
// running takes with it, and what stays for them.
func (u *uninstall) describeLeaving() {
	f := u.found
	if f.holder {
		u.ask.Info("  the lease, handed off to another node first")
	}
	if slices.Contains(f.stores, u.paths().Local) {
		u.ask.Info("  the local state of this node: %s", u.paths().Local)
	}
	if f.registered {
		u.ask.Info("  node %s in the registry, set to removed; the install stays on %s", u.node, strings.Join(u.others, ", "))
	}
	if m := u.manifest; m.inProxmox() || m.CreatedZone || m.CreatedVnet || m.CreatedFirewallRule || m.CreatedIPSet {
		u.ask.Info("  (the Proxmox user, token, tags, the SDN objects and the firewall rule stay: the nodes %s still use them)",
			strings.Join(u.others, ", "))
	}
}

// describeProxmox lists what Proxmox holds of what setup created.
func (u *uninstall) describeProxmox() {
	m, f := u.manifest, u.found
	if !m.inProxmox() {
		return
	}
	if f.proxmoxErr != nil {
		u.ask.Info("  nothing in Proxmox, which cannot be read: %v", f.proxmoxErr)
		return
	}
	s := f.proxmox
	if m.CreatedToken && s.token {
		u.ask.Info("  Proxmox token %s", tokenID)
	}
	if m.CreatedUser && s.user {
		u.ask.Info("  Proxmox user %s", UserID)
	}
	if m.GrantedACL && slices.ContainsFunc(s.acl, isGrant) {
		u.ask.Info("  the grant of role %s on / to %s", RoleID, UserID)
	}
	if m.CreatedRole && s.role != nil {
		if goes, why := u.roleVerdict(); goes {
			u.ask.Info("  Proxmox role %s", RoleID)
		} else {
			u.ask.Info("  (role %s is kept: %s)", RoleID, why)
		}
	}
	if tags := without(m.RegisteredTags, without(m.RegisteredTags, s.tags)); len(tags) > 0 {
		u.ask.Info("  the registered tags %s", strings.Join(tags, ", "))
	}
	if m.CreatedDatacenterCfg && len(m.RegisteredTags) > 0 {
		u.ask.Info("  %s, which the tags made, if nothing else is left in it", u.host.datacenterCfg)
	}
}

// describeManaged lists the managed cards that go first and the objects of
// the managed network setup made, which go last when no card is left on the
// vnet.
func (u *uninstall) describeManaged() {
	f, m := u.found, u.manifest
	switch {
	case !u.o.DetachManaged:
	case f.attachmentsErr != nil:
		u.ask.Info("  the managed cards pco attached, which cannot be read: %v", f.attachmentsErr)
	case len(f.attachments) > 0:
		var owners []string
		for _, a := range f.attachments {
			owners = append(owners, a.Owner)
		}
		u.ask.Info("  the managed cards pco attached to %s, removed first", strings.Join(owners, ", "))
	}
	var objects []string
	if m.CreatedVnet {
		objects = append(objects, "vnet "+ManagedVnet)
	}
	if m.CreatedZone {
		objects = append(objects, "zone "+ManagedZone)
	}
	if len(objects) == 0 {
		return
	}
	what := "the SDN " + strings.Join(objects, " and ")
	switch {
	case f.cardsErr != nil:
		u.ask.Info("  %s, once the cards on %s can be read: %v", what, ManagedVnet, f.cardsErr)
	case len(f.cards) > 0 && !u.o.DetachManaged:
		u.ask.Info("  (%s are kept while guests %s have a card on %s; --detach-managed removes pco's cards first)",
			what, strings.Join(Owners(f.cards), ", "), ManagedVnet)
	default:
		u.ask.Info("  %s, which an SDN apply removes from every node, reloading the network of each", what)
	}
}

func (u *uninstall) describeCloudflare() {
	switch {
	case u.installID == "" || len(u.creds) == 0:
	case u.o.PurgeCloudflare:
		u.showCloudflare()
	case u.o.KeepCloudflare:
		u.ask.Info("  (what install %s has at Cloudflare stays, and nothing on this node can remove it later)", u.installID)
	default:
		u.ask.Info("  (what install %s has at Cloudflare is asked about next)", u.installID)
	}
}
