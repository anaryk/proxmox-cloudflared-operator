// Package access computes what a Proxmox VE principal may do on a path, the
// way Proxmox does, from the access control data the API lists. It follows
// PVE::AccessControl::roles and the permission computation of
// PVE::RPCEnvironment (pve-access-control); where that source can be read
// more than one way, the answers of /access/permissions saved in
// testdata/matrix.json decide.
package access

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

const (
	rootUser      = "root@pam"
	administrator = "Administrator"
	noAccess      = "NoAccess"
)

// Data is the access control of the cluster as read at one time.
type Data struct {
	ACL    []pve.ACLEntry
	Users  []pve.User
	Groups []pve.Group
	Roles  []pve.Role
	Pools  map[model.GuestRef]string // guest -> pool, from the snapshot
}

// Principal is a user or a token with the privileges it holds on a path.
type Principal struct {
	ID    string // user@realm or user@realm!token
	Privs []string
}

// Roles returns the roles principal holds on path as PVE::AccessControl::roles
// does: walking the path from "/" down, the most specific node with an entry
// for the principal (its own, or one of its groups) replaces what it
// inherited, an entry with propagate=0 counts on its own node only, and a
// privilege-separated token is looked up by its own id. root@pam is
// Administrator everywhere.
//
// At one node the principal's own entries replace those of its groups; the
// entries of several groups add up. Pools are not considered here.
func Roles(d Data, principal, path string) []string {
	return sorted(newView(d).roles(principal, path))
}

// Privileges returns the privileges of principal on path as PVE's permission
// does: the union of the privileges of Roles, intersected with the user's for
// a privilege-separated token; NoAccess yields none. For /vms/<id> the roles
// on /pool/<p> of the guest's pool are added.
//
// The pool roles are what the principal holds on /pool/<p>, inherited ones
// included, and they count past a narrower role on the guest; only NoAccess
// on the guest keeps them out, and NoAccess on the pool takes everything away.
// A disabled user is answered as any other, as Proxmox does.
func Privileges(d Data, principal, path string) []string {
	return sorted(newView(d).privileges(principal, path))
}

// Effective returns every principal with any of privs on path, with those of
// privs it holds there. Disabled users and their tokens are left out.
func Effective(d Data, path string, privs []string) []Principal {
	return newView(d).effective(path, is(privs...))
}

// Admins returns the principals holding Sys.Modify on "/".
func Admins(d Data) []string {
	return ids(newView(d).admins())
}

// Delegated returns, for guest ref, the non-admin principals that hold
// VM.Config.Network on it, pco's own user and tokens (ownUser) excluded.
func Delegated(d Data, ref model.GuestRef, ownUser string) []Principal {
	v := newView(d)
	held := v.effective(vmPath(ref.VMID), is("VM.Config.Network"))
	return v.others(held, ownUser)
}

// Refused returns the non-admin principals that can reach into a guest, the
// appliance: those holding on /vms/<vmid> a privilege that opens its console,
// changes it, copies it or its data, moves it or starts and stops it
// (VM.Console, VM.Config.*, VM.Clone, VM.Backup, VM.Snapshot*, VM.Migrate,
// VM.PowerMgmt, VM.Allocate), Pool.Allocate on the pool of the guest, or
// Permissions.Modify, with which they can grant themselves any of these, on
// either path or a node above one. ownUser and its tokens are left out.
//
// The guest is taken as a member of pool, as the appliance is once installed.
// When the snapshot has it in another pool, it is taken as a member of each in
// turn and what either gives counts.
func Refused(d Data, vmid int, pool string, ownUser string) []Principal {
	v := newView(d)
	var pools []string
	for _, p := range []string{v.pools[vmid], pool} {
		if p != "" && !slices.Contains(pools, p) {
			pools = append(pools, p)
		}
	}

	var held []Principal
	check := func(path string, match func(priv string) bool) {
		held = merge(held, v.effective(path, match))
	}
	vm := vmPath(vmid)
	above := []string{"/", "/vms"}
	if len(pools) == 0 {
		check(vm, reachesIn)
	} else {
		above = append(above, "/pool")
	}
	for _, p := range pools {
		v.pools[vmid] = p
		check(vm, reachesIn)
		check("/pool/"+p, is("Pool.Allocate", "Permissions.Modify"))
	}
	for _, path := range above {
		check(path, is("Permissions.Modify"))
	}
	return v.others(held, ownUser)
}

// reachesIn reports whether a privilege on the guest is one of those Refused
// looks for.
func reachesIn(priv string) bool {
	switch priv {
	case "VM.Console", "VM.Clone", "VM.Backup", "VM.Migrate", "VM.PowerMgmt", "VM.Allocate", "Permissions.Modify":
		return true
	}
	return strings.HasPrefix(priv, "VM.Config.") || strings.HasPrefix(priv, "VM.Snapshot")
}

// is returns a match for the privileges privs.
func is(privs ...string) func(priv string) bool {
	return func(priv string) bool { return slices.Contains(privs, priv) }
}

func vmPath(vmid int) string { return "/vms/" + strconv.Itoa(vmid) }

// grants maps a user, group or token to its roles at one node of the ACL,
// each with its propagate flag.
type grants map[string]map[string]bool

// node is what the ACL grants at one path.
type node struct {
	users, groups, tokens grants
}

// view is Data indexed for the walks of roles and privileges.
type view struct {
	acl     map[string]*node
	users   []pve.User                 // sorted by id
	tokens  map[string]map[string]bool // user -> token name -> privsep
	members map[string]map[string]bool // group -> its users
	privs   map[string][]string        // role -> its privileges
	pools   map[int]string             // vmid -> pool
}

func newView(d Data) *view {
	v := &view{
		acl:     map[string]*node{},
		users:   slices.SortedFunc(slices.Values(d.Users), func(a, b pve.User) int { return cmp.Compare(a.ID, b.ID) }),
		tokens:  map[string]map[string]bool{},
		members: map[string]map[string]bool{},
		privs:   map[string][]string{},
		pools:   map[int]string{},
	}
	for _, e := range d.ACL {
		nodes := nodePaths(e.Path)
		at := nodes[len(nodes)-1]
		n := v.acl[at]
		if n == nil {
			n = &node{users: grants{}, groups: grants{}, tokens: grants{}}
			v.acl[at] = n
		}
		var g grants
		switch e.Type {
		case "user":
			g = n.users
		case "group":
			g = n.groups
		case "token":
			g = n.tokens
		default:
			continue
		}
		if g[e.UGID] == nil {
			g[e.UGID] = map[string]bool{}
		}
		g[e.UGID][e.RoleID] = e.Propagate
	}
	for _, u := range d.Users {
		v.tokens[u.ID] = u.Tokens
		for _, g := range u.Groups {
			v.member(g, u.ID)
		}
	}
	for _, g := range d.Groups {
		for _, m := range g.Members {
			v.member(g.ID, m)
		}
	}
	for _, r := range d.Roles {
		v.privs[r.ID] = r.Privs
	}
	for ref, pool := range d.Pools {
		v.pools[ref.VMID] = pool
	}
	return v
}

func (v *view) member(group, user string) {
	if v.members[group] == nil {
		v.members[group] = map[string]bool{}
	}
	v.members[group][user] = true
}

// nodePaths returns the nodes of the ACL tree from "/" down to path, split
// as Perl's split("/", $path) splits it: "/vms/100" is "/", "/vms",
// "/vms/100".
func nodePaths(path string) []string {
	out := []string{"/"}
	cur := ""
	for _, part := range strings.Split(path, "/") {
		if part == "" {
			continue
		}
		cur += "/" + part
		out = append(out, cur)
	}
	return out
}

// token splits a token id into its user and its name, and looks up whether
// it separates its privileges.
func (v *view) token(principal string) (user string, privsep, isToken, known bool) {
	user, name, isToken := strings.Cut(principal, "!")
	if !isToken {
		return principal, false, false, false
	}
	privsep, known = v.tokens[user][name]
	return user, privsep, true, known
}

func (v *view) roles(principal, path string) map[string]bool {
	if principal == rootUser {
		return map[string]bool{administrator: true}
	}
	user, privsep, isToken, known := v.token(principal)
	switch {
	case isToken && !known:
		return nil
	case isToken && !privsep:
		return v.roles(user, path)
	}

	var roles map[string]bool
	nodes := nodePaths(path)
	for i, p := range nodes {
		n := v.acl[p]
		if n == nil {
			continue
		}
		final := i == len(nodes)-1
		if r := applying(final, n.tokens[principal]); r != nil {
			roles = r
			continue
		}
		if r := applying(final, n.users[principal]); r != nil {
			roles = r
			continue
		}
		var fromGroups []map[string]bool
		for g, r := range n.groups {
			if v.members[g][principal] {
				fromGroups = append(fromGroups, r)
			}
		}
		if r := applying(final, fromGroups...); r != nil {
			roles = r
		}
	}
	if roles[noAccess] {
		return map[string]bool{noAccess: true}
	}
	return roles
}

// applying returns the roles of the entries that apply at a node: all of them
// on the path's own node, the propagated ones above it. It returns nil when
// none does, which leaves what was inherited in place.
func applying(final bool, entries ...map[string]bool) map[string]bool {
	var out map[string]bool
	for _, e := range entries {
		for role, propagate := range e {
			if final || propagate {
				if out == nil {
					out = map[string]bool{}
				}
				out[role] = true
			}
		}
	}
	return out
}

func (v *view) privileges(principal, path string) map[string]bool {
	if principal == rootUser {
		return v.privsOf(map[string]bool{administrator: true})
	}
	user, privsep, isToken, known := v.token(principal)
	var userPrivs map[string]bool
	switch {
	case isToken && !known:
		return nil
	case isToken:
		userPrivs = v.privileges(user, path)
		if !privsep {
			return userPrivs
		}
	}

	roles := v.withPoolRoles(principal, path, v.roles(principal, path))
	if roles[noAccess] {
		return nil
	}
	privs := v.privsOf(roles)
	if isToken && user != rootUser {
		maps.DeleteFunc(privs, func(p string, _ bool) bool { return !userPrivs[p] })
	}
	return privs
}

// withPoolRoles adds to the roles on /vms/<id> those the principal holds on
// the pool of the guest.
func (v *view) withPoolRoles(principal, path string, roles map[string]bool) map[string]bool {
	id, ok := strings.CutPrefix(path, "/vms/")
	vmid, err := strconv.Atoi(id)
	if !ok || err != nil || vmPath(vmid) != path {
		return roles
	}
	pool, ok := v.pools[vmid]
	if !ok || roles[noAccess] {
		return roles
	}
	fromPool := v.roles(principal, "/pool/"+pool)
	switch {
	case len(fromPool) == 0:
		return roles
	case fromPool[noAccess]:
		return map[string]bool{noAccess: true}
	}
	out := maps.Clone(roles)
	if out == nil {
		out = map[string]bool{}
	}
	maps.Copy(out, fromPool)
	return out
}

func (v *view) privsOf(roles map[string]bool) map[string]bool {
	out := map[string]bool{}
	for role := range roles {
		for _, p := range v.privs[role] {
			out[p] = true
		}
	}
	return out
}

// principals lists every enabled user and the tokens of each.
func (v *view) principals() []string {
	var out []string
	for _, u := range v.users {
		if !u.Enabled {
			continue
		}
		out = append(out, u.ID)
		for _, name := range slices.Sorted(maps.Keys(u.Tokens)) {
			out = append(out, u.ID+"!"+name)
		}
	}
	slices.Sort(out)
	return out
}

func (v *view) effective(path string, match func(priv string) bool) []Principal {
	var out []Principal
	for _, id := range v.principals() {
		var held []string
		for p := range v.privileges(id, path) {
			if match(p) {
				held = append(held, p)
			}
		}
		if len(held) > 0 {
			slices.Sort(held)
			out = append(out, Principal{ID: id, Privs: held})
		}
	}
	return out
}

func (v *view) admins() []Principal {
	return v.effective("/", is("Sys.Modify"))
}

// others leaves out of held the admins and ownUser with its tokens.
func (v *view) others(held []Principal, ownUser string) []Principal {
	admins := ids(v.admins())
	held = slices.DeleteFunc(held, func(p Principal) bool {
		own := ownUser != "" && (p.ID == ownUser || strings.HasPrefix(p.ID, ownUser+"!"))
		return own || slices.Contains(admins, p.ID)
	})
	if len(held) == 0 {
		return nil
	}
	return held
}

// merge joins two lists sorted by id, uniting the privileges of a principal
// in both.
func merge(a, b []Principal) []Principal {
	byID := map[string][]string{}
	for _, p := range slices.Concat(a, b) {
		byID[p.ID] = append(byID[p.ID], p.Privs...)
	}
	out := make([]Principal, 0, len(byID))
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		privs := byID[id]
		slices.Sort(privs)
		out = append(out, Principal{ID: id, Privs: slices.Compact(privs)})
	}
	return out
}

func ids(ps []Principal) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

func sorted(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(set))
}
