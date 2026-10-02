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

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// survey is what uninstall finds on the node before it asks anything, read
// without changing anything: what is not there is neither listed nor removed.
type survey struct {
	unit, egressUnit bool     // the unit files of pco and of the egress filter
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
}

func (u *uninstall) survey(ctx context.Context) {
	f := &u.found
	f.unit = u.unitInstalled(serviceUnit)
	f.egressUnit = u.unitInstalled(egressUnit)
	f.connectors, f.connectorsErr = u.listConnectors(ctx)
	f.nft, f.table, f.nftErr = u.egressTableThere(ctx)
	if u.manifest.inProxmox() {
		f.proxmox, f.proxmoxErr = u.readProxmox(ctx)
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

// looksAtCloudflare reports whether Cloudflare is to be listed: for a purge,
// and for the question whether to purge, which is asked without Yes. Yes with
// an install and credentials comes with one of the two flags.
func (u *uninstall) looksAtCloudflare() bool {
	return u.installID != "" && len(u.creds) > 0 && !u.o.KeepCloudflare
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
	if f.egressUnit {
		u.ask.Info("  the unit of the egress filter, %s", egressUnit)
	}
	switch {
	case f.nftErr != nil:
		u.ask.Info("  the egress filter, which cannot be looked at: %v", f.nftErr)
	case f.table:
		u.ask.Info("  the egress filter: table inet %s", egressTable)
	}
	u.describeProxmox()
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
		u.ask.Info("  Proxmox user %s", userID)
	}
	if m.GrantedACL && slices.ContainsFunc(s.acl, isGrant) {
		u.ask.Info("  the grant of role %s on / to %s", roleID, userID)
	}
	if m.CreatedRole && s.role != nil {
		if goes, why := u.roleVerdict(); goes {
			u.ask.Info("  Proxmox role %s", roleID)
		} else {
			u.ask.Info("  (role %s is kept: %s)", roleID, why)
		}
	}
	if tags := without(m.RegisteredTags, without(m.RegisteredTags, s.tags)); len(tags) > 0 {
		u.ask.Info("  the registered tags %s", strings.Join(tags, ", "))
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
