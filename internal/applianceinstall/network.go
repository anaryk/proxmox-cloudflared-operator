package applianceinstall

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// localNetwork is the zone of every plain Linux bridge.
const localNetwork = "localnetwork"

// GrantNetwork grants pco@pve and the token of the appliance vmid what a card
// of the appliance on a network needs: role PCOManaged
// (VM.Config.Network) on /vms/<vmid> and role PCOSDN (SDN.Use and
// SDN.Audit, which reads the vnet) on /sdn/zones/<zone>/<vnet>[/<vlan>],
// never on a whole zone. A privilege-separated token holds only what its user
// holds as well, so both get each line. The roles are made when missing, and
// the token's privileges are asked of Proxmox afterwards. It returns the
// grant with the roles it made; on a failure it takes back what it did, and
// nothing that was there before.
func GrantNetwork(ctx context.Context, r setup.Runner, vmid int, zone, vnet string, vlan int) (setup.NetworkGrant, error) {
	g := setup.NetworkGrant{Zone: zone, VNet: vnet, VLAN: vlan}
	if err := g.Check(); err != nil {
		return g, err
	}
	gr, err := newGranting(ctx, r, vmid)
	if err != nil {
		return g, err
	}
	for _, id := range []string{setup.RoleManaged, setup.RoleSDN} {
		if err := gr.role(ctx, id, &g); err != nil {
			return g, err
		}
	}
	for _, line := range [][2]string{{"/vms/" + strconv.Itoa(vmid), setup.RoleManaged}, {g.Path(), setup.RoleSDN}} {
		if err := gr.line(ctx, line[0], line[1], g); err != nil {
			return g, err
		}
	}
	for _, check := range [][2]string{{"/vms/" + strconv.Itoa(vmid), "VM.Config.Network"}, {g.Path(), "SDN.Use"}, {g.Path(), "SDN.Audit"}} {
		if err := gr.holds(ctx, check[0], check[1], g); err != nil {
			return g, err
		}
	}
	return g, nil
}

// granting is one grant as it is made, with the access control list as it
// was before: a grant that fails takes back the lines and the roles it added
// and nothing that was there before it.
type granting struct {
	r      setup.Runner
	id     string // the token of the appliance
	before []aclLine
	roles  []roleEntry
	added  []aclLine
}

func newGranting(ctx context.Context, r setup.Runner, vmid int) (*granting, error) {
	before, err := aclLines(ctx, r)
	if err != nil {
		return nil, err
	}
	all, err := roles(ctx, r)
	if err != nil {
		return nil, err
	}
	return &granting{r: r, id: tokenID(vmid), before: before, roles: all}, nil
}

// role makes a role of a grant when it is missing, brings one of a release
// before up to date, and refuses one that is not pco's.
func (gr *granting) role(ctx context.Context, id string, g *setup.NetworkGrant) error {
	role, found := findRole(gr.roles, id)
	switch {
	case found && isEarlierGrantRole(id, role.Privs):
		if _, err := gr.r.Run(ctx, "pveum", "role", "modify", id, "--privs", grantRolePrivs[id]); err != nil {
			gr.takeBack(ctx, *g)
			return fmt.Errorf("giving role %s the privileges %s: %w", id, grantRolePrivs[id], err)
		}
		return nil
	case found && !isGrantRole(id, role.Privs):
		gr.takeBack(ctx, *g)
		return fmt.Errorf("role %s grants %s, not only %s: pco does not use it", id, strings.Join(role.Privs, ", "), grantRolePrivs[id])
	case found:
		return nil
	}
	if _, err := gr.r.Run(ctx, "pveum", "role", "add", id, "--privs", grantRolePrivs[id]); err != nil {
		gr.takeBack(ctx, *g)
		return fmt.Errorf("creating role %s: %w", id, err)
	}
	g.CreatedRoles = append(g.CreatedRoles, id)
	return nil
}

// line grants a role on a path to pco@pve and the token, and remembers the
// lines that were not there before.
func (gr *granting) line(ctx context.Context, path, role string, g setup.NetworkGrant) error {
	if _, err := gr.r.Run(ctx, "pveum", "acl", "modify", path, "--users", setup.UserID, "--tokens", gr.id, "--roles", role); err != nil {
		gr.takeBack(ctx, g)
		return fmt.Errorf("granting role %s on %s: %w", role, path, err)
	}
	for _, l := range []aclLine{{Path: path, Type: "user", UGID: setup.UserID, Role: role}, {Path: path, Type: "token", UGID: gr.id, Role: role}} {
		if !slices.Contains(gr.before, l) {
			gr.added = append(gr.added, l)
		}
	}
	return nil
}

// holds asks Proxmox whether the token holds a privilege on a path after the
// grant: its answer is what counts.
func (gr *granting) holds(ctx context.Context, path, priv string, g setup.NetworkGrant) error {
	held, err := permissions(ctx, gr.r, gr.id, path)
	if err == nil && !slices.Contains(held, priv) {
		err = fmt.Errorf("%s does not hold %s on %s after the grant", gr.id, priv, path)
	}
	if err != nil {
		gr.takeBack(ctx, g)
	}
	return err
}

// takeBack removes the lines this grant added and the roles it made.
func (gr *granting) takeBack(ctx context.Context, g setup.NetworkGrant) {
	ctx = context.WithoutCancel(ctx)
	for _, l := range gr.added {
		_, _ = gr.r.Run(ctx, "pveum", "acl", "delete", l.Path, "--"+l.Type+"s", l.UGID, "--roles", l.Role)
	}
	for _, id := range g.CreatedRoles {
		_, _ = gr.r.Run(ctx, "pveum", "role", "delete", id)
	}
}

// RevokeNetwork takes back the grant of a network: the lines of the token,
// those of pco@pve unless another token of it holds the same grant, and the
// roles once no line names them.
func RevokeNetwork(ctx context.Context, r setup.Runner, vmid int, zone, vnet string, vlan int) error {
	g := setup.NetworkGrant{Zone: zone, VNet: vnet, VLAN: vlan}
	if err := g.Check(); err != nil {
		return err
	}
	vm := "/vms/" + strconv.Itoa(vmid)
	return revokeLines(ctx, r, vmid, [][2]string{{vm, setup.RoleManaged}, {g.Path(), setup.RoleSDN}},
		[]string{setup.RoleManaged, setup.RoleSDN})
}

// NetworkOptions say what pco appliance grant-network and revoke-network
// grant or take back.
type NetworkOptions struct {
	VMID   int
	Bridge string // a Linux bridge of the node, or an SDN vnet
	VLAN   int
	Yes    bool
}

// GrantNetworkCommand is pco appliance grant-network: it finds the zone of
// the bridge, says what it grants and asks, grants it, and records the grant
// in the manifest of the appliance.
func (i *Installer) GrantNetworkCommand(ctx context.Context, o NetworkOptions) error {
	r, g, err := i.networkRun(ctx, o)
	if err != nil {
		return err
	}
	defer r.unlock()
	vm := "/vms/" + strconv.Itoa(o.VMID)
	r.ask.Info("pco appliance grant-network grants pco@pve and %s:", tokenID(o.VMID))
	r.ask.Info("  role %s (VM.Config.Network) on %s", setup.RoleManaged, vm)
	r.ask.Info("  role %s (SDN.Use, SDN.Audit) on %s", setup.RoleSDN, g.Path())
	r.ask.Info("  pveum acl modify %s --users %s --tokens '%s' --roles %s", vm, setup.UserID, tokenID(o.VMID), setup.RoleManaged)
	r.ask.Info("  pveum acl modify %s --users %s --tokens '%s' --roles %s", g.Path(), setup.UserID, tokenID(o.VMID), setup.RoleSDN)
	if !o.Yes {
		ok, err := r.ask.Confirm("Grant these?", false)
		if err != nil {
			return err
		}
		if !ok {
			return setup.ErrAborted
		}
	}
	if err := r.record(func(j *journal) { j.Grant = &g }); err != nil {
		return err
	}
	made, err := GrantNetwork(ctx, r.r, o.VMID, g.Zone, g.VNet, g.VLAN)
	if err != nil {
		_ = r.removeJournal()
		return err
	}
	r.ask.Info("granted: %s on %s and %s on %s", setup.RoleManaged, vm, setup.RoleSDN, g.Path())
	r.recordGrant(ctx, "add", made)
	return r.removeJournal()
}

// RevokeNetworkCommand is pco appliance revoke-network.
func (i *Installer) RevokeNetworkCommand(ctx context.Context, o NetworkOptions) error {
	r, g, err := i.networkRun(ctx, o)
	if err != nil {
		return err
	}
	defer r.unlock()
	r.ask.Info("pco appliance revoke-network takes back from %s, and from pco@pve unless another token needs it:", tokenID(o.VMID))
	r.ask.Info("  role %s on /vms/%d and role %s on %s, and the roles once nothing names them", setup.RoleManaged, o.VMID, setup.RoleSDN, g.Path())
	if !o.Yes {
		ok, err := r.ask.Confirm("Take these back?", false)
		if err != nil {
			return err
		}
		if !ok {
			return setup.ErrAborted
		}
	}
	if err := RevokeNetwork(ctx, r.r, o.VMID, g.Zone, g.VNet, g.VLAN); err != nil {
		return err
	}
	r.ask.Info("taken back: the grant of %s to lxc/%d", g.Path(), o.VMID)
	r.recordGrant(ctx, "remove", g)
	return nil
}

// networkRun checks the appliance and finds the zone of the bridge or vnet.
func (i *Installer) networkRun(ctx context.Context, o NetworkOptions) (*run, setup.NetworkGrant, error) {
	var g setup.NetworkGrant
	if o.VMID < 100 || o.VMID > 999999999 {
		return nil, g, fmt.Errorf("--vmid %d: want 100 to 999999999", o.VMID)
	}
	if o.VLAN < 0 || o.VLAN > 4094 {
		return nil, g, fmt.Errorf("--vlan %d: want 1 to 4094, or none for the whole bridge", o.VLAN)
	}
	r := i.newRun(Options{Yes: o.Yes, VMID: o.VMID}, kindGrant)
	r.j.VMID = o.VMID
	if err := r.whoami(); err != nil {
		return nil, g, err
	}
	unlock, err := i.lock()
	if err != nil {
		return nil, g, err
	}
	r.unlock = unlock
	g, err = r.network(ctx, o)
	if err != nil {
		unlock()
		return nil, g, err
	}
	return r, g, nil
}

// network finds the zone of a bridge or vnet: localnetwork for a Linux
// bridge of the node, the vnet's own zone for an SDN vnet. A VLAN needs a
// VLAN-aware one.
func (r *run) network(ctx context.Context, o NetworkOptions) (setup.NetworkGrant, error) {
	g := setup.NetworkGrant{VNet: o.Bridge, VLAN: o.VLAN}
	ifaces, err := nodeNetwork(ctx, r.r, r.node)
	if err != nil {
		return g, err
	}
	aware, pvid := false, 1
	if i := slices.IndexFunc(ifaces, func(i nodeIface) bool { return i.Name == o.Bridge && i.bridge() }); i >= 0 {
		g.Zone, aware, pvid = localNetwork, ifaces[i].vlanAware(), ifaces[i].pvid()
	} else {
		vs, err := vnets(ctx, r.r)
		if err != nil {
			return g, err
		}
		i := slices.IndexFunc(vs, func(v vnet) bool { return v.Name == o.Bridge })
		if i < 0 {
			return g, fmt.Errorf("%s is neither a bridge of node %s nor an SDN vnet", o.Bridge, r.node)
		}
		g.Zone, aware = vs[i].Zone, bool(vs[i].VLANAware)
	}
	if o.VLAN != 0 && !aware {
		return g, fmt.Errorf("%s is not VLAN-aware, so it carries no VLAN %d to grant", o.Bridge, o.VLAN)
	}
	if err := g.Check(); err != nil {
		return g, err
	}
	addrs, err := nodeAddrs(ctx, r.r)
	if err != nil {
		return g, err
	}
	for _, a := range addrs {
		// Without a VLAN the grant is of every VLAN of the bridge.
		on := a.Dev == vlanDevice(o.Bridge, o.VLAN, pvid)
		if o.VLAN == 0 {
			on = on || strings.HasPrefix(a.Dev, o.Bridge+".")
		}
		if on {
			r.ask.Warn("%s carries %s, an address of this node: a card of the appliance there reaches the node", a.Dev, a.Prefix)
		}
	}
	return g, nil
}

// recordGrant records a grant in the manifest of the appliance, or takes it
// out; the marks find it without the manifest, so a container that does not
// run is only a warning.
func (r *run) recordGrant(ctx context.Context, verb string, g setup.NetworkGrant) {
	args := []string{"appliance", "manifest", verb, "--zone", g.Zone, "--vnet", g.VNet}
	if g.Pool != "" {
		args = []string{"appliance", "manifest", verb, "--pool", g.Pool}
	}
	if g.VLAN != 0 {
		args = append(args, "--vlan", strconv.Itoa(g.VLAN))
	}
	for _, role := range g.CreatedRoles {
		args = append(args, "--created-role", role)
	}
	if _, err := r.exec(ctx, r.j.VMID, append([]string{"pco"}, args...)...); err != nil {
		r.ask.Warn("the manifest of lxc/%d is not changed (%v): uninstall finds the grant by the lines of its token", r.j.VMID, err)
	}
}

// rollbackGrant takes back a grant-network run that was cut short.
func (r *run) rollbackGrant(ctx context.Context) error {
	switch g := r.j.Grant; {
	case g == nil:
	case g.Pool != "":
		return RevokePool(ctx, r.r, r.j.VMID, g.Pool)
	default:
		return RevokeNetwork(ctx, r.r, r.j.VMID, g.Zone, g.VNet, g.VLAN)
	}
	return nil
}

// GrantPool grants pco@pve and the token of the appliance vmid role
// PCOManaged (VM.Config.Network) on /pool/<pool>, and nowhere else: the token
// may then change the network cards and the firewall of every guest in that
// pool, so that the managed network attaches its members without a grant per
// guest. The role is made when missing, and the token's privilege is asked of
// Proxmox afterwards. It returns the grant with the role it made; on a
// failure it takes back what it did, and nothing that was there before.
func GrantPool(ctx context.Context, r setup.Runner, vmid int, pool string) (setup.NetworkGrant, error) {
	g := setup.NetworkGrant{Pool: pool}
	if pool == "" {
		return g, fmt.Errorf("pool %q: want the name of a pool", pool)
	}
	if err := g.Check(); err != nil {
		return g, err
	}
	ps, err := pools(ctx, r)
	if err != nil {
		return g, err
	}
	if !slices.ContainsFunc(ps, func(p poolEntry) bool { return p.ID == pool }) {
		return g, fmt.Errorf("pool %s does not exist", pool)
	}
	gr, err := newGranting(ctx, r, vmid)
	if err != nil {
		return g, err
	}
	if err := gr.role(ctx, setup.RoleManaged, &g); err != nil {
		return g, err
	}
	if err := gr.line(ctx, g.Path(), setup.RoleManaged, g); err != nil {
		return g, err
	}
	if err := gr.holds(ctx, g.Path(), "VM.Config.Network", g); err != nil {
		return g, err
	}
	return g, nil
}

// RevokePool takes back the grant of a pool: the line of the token, that of
// pco@pve unless another token of it holds the same grant, and role
// PCOManaged once no line names it.
func RevokePool(ctx context.Context, r setup.Runner, vmid int, pool string) error {
	g := setup.NetworkGrant{Pool: pool}
	if err := g.Check(); err != nil {
		return err
	}
	return revokeLines(ctx, r, vmid, [][2]string{{g.Path(), setup.RoleManaged}}, []string{setup.RoleManaged})
}

// revokeLines deletes the lines of the appliance's token and of pco@pve that
// grant a role on a path, each {path, role}, but those of pco@pve another
// token of it has as well, and then the roles no line names any more.
func revokeLines(ctx context.Context, r setup.Runner, vmid int, lines [][2]string, roleIDs []string) error {
	acl, err := aclLines(ctx, r)
	if err != nil {
		return err
	}
	id := tokenID(vmid)
	var errs []error
	del := func(l aclLine) {
		if !slices.Contains(acl, l) {
			return
		}
		if _, err := r.Run(ctx, "pveum", "acl", "delete", l.Path, "--"+l.Type+"s", l.UGID, "--roles", l.Role); err != nil {
			errs = append(errs, fmt.Errorf("revoking role %s on %s from %s: %w", l.Role, l.Path, l.UGID, err))
			return
		}
		acl = slices.DeleteFunc(acl, func(o aclLine) bool { return o == l })
	}
	shared := func(path, role string) bool {
		return slices.ContainsFunc(acl, func(l aclLine) bool {
			return l.Path == path && l.Role == role && l.Type == "token" && l.UGID != id && strings.HasPrefix(l.UGID, setup.UserID+"!")
		})
	}
	for _, line := range lines {
		del(aclLine{Path: line[0], Type: "token", UGID: id, Role: line[1]})
		if !shared(line[0], line[1]) {
			del(aclLine{Path: line[0], Type: "user", UGID: setup.UserID, Role: line[1]})
		}
	}
	all, err := roles(ctx, r)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, rid := range roleIDs {
		role, found := findRole(all, rid)
		if found && isGrantRole(rid, role.Privs) && !slices.ContainsFunc(acl, func(l aclLine) bool { return l.Role == rid }) {
			if _, err := r.Run(ctx, "pveum", "role", "delete", rid); err != nil {
				errs = append(errs, fmt.Errorf("removing role %s: %w", rid, err))
			}
		}
	}
	return errors.Join(errs...)
}
