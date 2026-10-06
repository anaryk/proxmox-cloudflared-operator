package applianceinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/access"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// flexBool is a flag Proxmox prints as 0 or 1, a number or a string.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	switch string(bytes.Trim(bytes.TrimSpace(data), `"`)) {
	case "1", "true":
		*b = true
	case "0", "false", "", "null":
		*b = false
	default:
		return fmt.Errorf("%s is not a boolean", data)
	}
	return nil
}

// flexInt is a number Proxmox prints as a number or a string.
type flexInt int

func (n *flexInt) UnmarshalJSON(data []byte) error {
	s := string(bytes.Trim(bytes.TrimSpace(data), `"`))
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("%s is not a whole number", data)
	}
	*n = flexInt(v)
	return nil
}

// list is a list Proxmox prints as one string, separated by commas,
// semicolons or spaces.
type list []string

func (l *list) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		var parts []string
		if err := json.Unmarshal(data, &parts); err != nil {
			return errors.New("want a string or a list of strings")
		}
		*l = parts
		return nil
	}
	*l = strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || unicode.IsSpace(r) })
	return nil
}

// query runs a command that prints JSON and reads it into v.
func query(ctx context.Context, r setup.Runner, v any, name string, args ...string) error {
	out, err := r.Run(ctx, name, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(out), v); err != nil {
		return fmt.Errorf("%s %s printed what is not the JSON expected: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// pvesh reads path of the API on this node as root, args being its
// parameters as --name value.
func pvesh(ctx context.Context, r setup.Runner, v any, path string, args ...string) error {
	return query(ctx, r, v, "pvesh", append(append([]string{"get", path}, args...), "--output-format", "json")...)
}

// notThere reports whether the error of a command says that what it was
// asked about does not exist.
func notThere(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "does not exist") || strings.Contains(msg, "no such")
}

type storageEntry struct {
	ID      string   `json:"storage"`
	Type    string   `json:"type"`
	Content list     `json:"content"`
	Active  flexBool `json:"active"`
	Enabled *flexBool
}

func (s storageEntry) holds(content string) bool { return slices.Contains(s.Content, content) }

func storages(ctx context.Context, r setup.Runner, node string) ([]storageEntry, error) {
	var out []storageEntry
	if err := pvesh(ctx, r, &out, "/nodes/"+node+"/storage"); err != nil {
		return nil, fmt.Errorf("listing the storages of node %s: %w", node, err)
	}
	return out, nil
}

type nodeIface struct {
	Name      string   `json:"iface"`
	Type      string   `json:"type"`
	VLANAware flexBool `json:"bridge_vlan_aware"`
}

func (i nodeIface) bridge() bool { return i.Type == "bridge" || i.Type == "OVSBridge" }

// vlanAware reports whether the bridge carries VLANs: a Linux bridge says so,
// an Open vSwitch bridge always does.
func (i nodeIface) vlanAware() bool { return bool(i.VLANAware) || i.Type == "OVSBridge" }

func nodeNetwork(ctx context.Context, r setup.Runner, node string) ([]nodeIface, error) {
	var out []nodeIface
	if err := pvesh(ctx, r, &out, "/nodes/"+node+"/network"); err != nil {
		return nil, fmt.Errorf("listing the network of node %s: %w", node, err)
	}
	return out, nil
}

type vnet struct {
	Name      string   `json:"vnet"`
	Zone      string   `json:"zone"`
	VLANAware flexBool `json:"vlanaware"`
}

func vnets(ctx context.Context, r setup.Runner) ([]vnet, error) {
	var out []vnet
	if err := pvesh(ctx, r, &out, "/cluster/sdn/vnets"); err != nil {
		return nil, fmt.Errorf("listing the SDN vnets: %w", err)
	}
	return out, nil
}

// nodeAddr is a global IPv4 address of the node, as ip prints it.
type nodeAddr struct {
	Dev    string
	Prefix netip.Prefix
}

// nodeAddrs reads every global IPv4 address of the node, those it holds only
// at run time included, which the API knows nothing of.
func nodeAddrs(ctx context.Context, r setup.Runner) ([]nodeAddr, error) {
	out, err := r.Run(ctx, "ip", "-4", "-o", "addr", "show", "scope", "global")
	if err != nil {
		return nil, fmt.Errorf("listing the addresses of the node: %w", err)
	}
	var addrs []nodeAddr
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) < 4 || f[2] != "inet" {
			continue
		}
		p, err := netip.ParsePrefix(f[3])
		if err != nil || !p.Addr().Is4() {
			continue
		}
		addrs = append(addrs, nodeAddr{Dev: strings.TrimSuffix(f[1], ":"), Prefix: p})
	}
	return addrs, nil
}

type clusterEntry struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Quorate flexBool `json:"quorate"`
	Nodes   flexInt  `json:"nodes"`
}

// clusterState is what /cluster/status says of the cluster: a node that is
// not in one has no entry of type cluster, and is quorate.
type clusterState struct {
	name    string
	nodes   int
	quorate bool
}

func readCluster(ctx context.Context, r setup.Runner) (clusterState, error) {
	var entries []clusterEntry
	if err := pvesh(ctx, r, &entries, "/cluster/status"); err != nil {
		return clusterState{}, fmt.Errorf("reading the cluster status: %w", err)
	}
	c := clusterState{quorate: true, nodes: 1}
	for _, e := range entries {
		if e.Type == "cluster" {
			c = clusterState{name: e.Name, nodes: int(e.Nodes), quorate: bool(e.Quorate)}
		}
	}
	return c, nil
}

func nextID(ctx context.Context, r setup.Runner) (int, error) {
	var id flexInt
	if err := pvesh(ctx, r, &id, "/cluster/nextid"); err != nil {
		return 0, fmt.Errorf("asking for the next free VMID: %w", err)
	}
	if id < 100 {
		return 0, fmt.Errorf("the cluster offers VMID %d", id)
	}
	return int(id), nil
}

// guestExists reports whether a container or a virtual machine of vmid is on
// this node: either status command answers for it.
func guestExists(ctx context.Context, r setup.Runner, vmid int) bool {
	id := strconv.Itoa(vmid)
	if _, err := r.Run(ctx, "pct", "status", id); err == nil {
		return true
	}
	_, err := r.Run(ctx, "qm", "status", id)
	return err == nil
}

// ctRunning reports whether the container runs; false with no error for one
// that is stopped.
func ctRunning(ctx context.Context, r setup.Runner, vmid int) (bool, error) {
	out, err := r.Run(ctx, "pct", "status", strconv.Itoa(vmid))
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "status:")) {
	case "running":
		return true, nil
	case "stopped":
		return false, nil
	}
	return false, fmt.Errorf("pct status %d printed %q", vmid, strings.TrimSpace(out))
}

// ctConfig is the configuration of a container as it runs, every value as
// text.
type ctConfig map[string]string

func readCTConfig(ctx context.Context, r setup.Runner, node string, vmid int) (ctConfig, error) {
	var raw map[string]json.RawMessage
	if err := pvesh(ctx, r, &raw, fmt.Sprintf("/nodes/%s/lxc/%d/config", node, vmid), "--current", "1"); err != nil {
		return nil, err
	}
	cfg := ctConfig{}
	for k, v := range raw {
		var s string
		if json.Unmarshal(v, &s) != nil {
			s = string(v)
		}
		cfg[k] = s
	}
	return cfg, nil
}

// option returns the value of key in a property string such as
// "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:01".
func option(prop, key string) string {
	for part := range strings.SplitSeq(prop, ",") {
		if k, v, ok := strings.Cut(part, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// macs returns the MACs of every netN of the configuration, in normal form.
func (c ctConfig) macs() ([]string, error) {
	var out []string
	for _, k := range c.netKeys() {
		mac, err := model.NormalizeMAC(option(c[k], "hwaddr"))
		if err != nil {
			return nil, fmt.Errorf("%s has no MAC: %s", k, c[k])
		}
		out = append(out, mac)
	}
	if len(out) == 0 {
		return nil, errors.New("the container has no network card")
	}
	return out, nil
}

func (c ctConfig) netKeys() []string {
	var keys []string
	for k := range c {
		if n, ok := strings.CutPrefix(k, "net"); ok {
			if _, err := strconv.Atoi(n); err == nil {
				keys = append(keys, k)
			}
		}
	}
	slices.SortFunc(keys, func(a, b string) int {
		x, _ := strconv.Atoi(a[3:])
		y, _ := strconv.Atoi(b[3:])
		return x - y
	})
	return keys
}

// volumeStorage returns the storage of a volume option such as rootfs or mp0.
func (c ctConfig) volumeStorage(key string) string {
	vol, _, _ := strings.Cut(c[key], ",")
	storage, _, ok := strings.Cut(vol, ":")
	if !ok {
		return ""
	}
	return storage
}

type tokenEntry struct {
	Name    string    `json:"tokenid"`
	Comment string    `json:"comment"`
	Privsep *flexBool `json:"privsep"`
}

// tokens lists the tokens of pco@pve; none when the user is not there.
func tokens(ctx context.Context, r setup.Runner, users []userEntry) ([]tokenEntry, error) {
	if !slices.ContainsFunc(users, func(u userEntry) bool { return u.ID == setup.UserID }) {
		return nil, nil
	}
	var out []tokenEntry
	if err := query(ctx, r, &out, "pveum", "user", "token", "list", setup.UserID, "--output-format", "json"); err != nil {
		return nil, fmt.Errorf("listing the tokens of %s: %w", setup.UserID, err)
	}
	return out, nil
}

type userEntry struct {
	ID      string `json:"userid"`
	Comment string `json:"comment"`
}

func users(ctx context.Context, r setup.Runner) ([]userEntry, error) {
	var out []userEntry
	if err := query(ctx, r, &out, "pveum", "user", "list", "--output-format", "json"); err != nil {
		return nil, fmt.Errorf("listing the users: %w", err)
	}
	return out, nil
}

type roleEntry struct {
	ID    string `json:"roleid"`
	Privs list   `json:"privs"`
}

func roles(ctx context.Context, r setup.Runner) ([]roleEntry, error) {
	var out []roleEntry
	if err := query(ctx, r, &out, "pveum", "role", "list", "--output-format", "json"); err != nil {
		return nil, fmt.Errorf("listing the roles: %w", err)
	}
	return out, nil
}

func findRole(all []roleEntry, id string) (roleEntry, bool) {
	i := slices.IndexFunc(all, func(r roleEntry) bool { return r.ID == id })
	if i < 0 {
		return roleEntry{}, false
	}
	return all[i], true
}

type aclLine struct {
	Path string `json:"path"`
	Type string `json:"type"`
	UGID string `json:"ugid"`
	Role string `json:"roleid"`
}

// noAccessLine is the line of the access control list a NoAccess line of the
// manifest names: a token's id holds a "!", a user's never does.
func noAccessLine(n setup.NoAccessLine) aclLine {
	kind := "user"
	if strings.Contains(n.Principal, "!") {
		kind = "token"
	}
	return aclLine{Path: n.Path, Type: kind, UGID: n.Principal, Role: n.Role}
}

// aclEntry is a line as pveum lists it, with whether it counts below its
// path, as one pveum acl modify adds does.
type aclEntry struct {
	aclLine
	Propagate *flexBool `json:"propagate"`
}

func (e aclEntry) propagates() bool { return e.Propagate == nil || bool(*e.Propagate) }

func aclEntries(ctx context.Context, r setup.Runner) ([]aclEntry, error) {
	var out []aclEntry
	if err := query(ctx, r, &out, "pveum", "acl", "list", "--output-format", "json"); err != nil {
		return nil, fmt.Errorf("listing the access control list: %w", err)
	}
	return out, nil
}

func aclLines(ctx context.Context, r setup.Runner) ([]aclLine, error) {
	entries, err := aclEntries(ctx, r)
	if err != nil {
		return nil, err
	}
	lines := make([]aclLine, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, e.aclLine)
	}
	return lines, nil
}

type poolEntry struct {
	ID      string `json:"poolid"`
	Comment string `json:"comment"`
}

func pools(ctx context.Context, r setup.Runner) ([]poolEntry, error) {
	var out []poolEntry
	if err := query(ctx, r, &out, "pveum", "pool", "list", "--output-format", "json"); err != nil {
		return nil, fmt.Errorf("listing the pools: %w", err)
	}
	return out, nil
}

// poolMembers returns the members of a pool by their ids, as lxc/100.
func poolMembers(ctx context.Context, r setup.Runner, pool string) ([]string, error) {
	var out []struct {
		Members []struct {
			ID string `json:"id"`
		} `json:"members"`
	}
	if err := pvesh(ctx, r, &out, "/pools", "--poolid", pool); err != nil {
		return nil, fmt.Errorf("listing the members of pool %s: %w", pool, err)
	}
	var ids []string
	for _, p := range out {
		for _, m := range p.Members {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

type volume struct {
	ID string `json:"volid"`
}

// templates lists the container templates on a storage of the node.
func templates(ctx context.Context, r setup.Runner, node, storage string) ([]string, error) {
	var out []volume
	if err := pvesh(ctx, r, &out, fmt.Sprintf("/nodes/%s/storage/%s/content", node, storage), "--content", "vztmpl"); err != nil {
		return nil, fmt.Errorf("listing the templates on storage %s: %w", storage, err)
	}
	var ids []string
	for _, v := range out {
		ids = append(ids, v.ID)
	}
	return ids, nil
}

// clusterGuest is a guest as /cluster/resources lists it, on whichever node
// of the cluster it is.
type clusterGuest struct {
	Type string  `json:"type"`
	VMID flexInt `json:"vmid"`
	Node string  `json:"node"`
	Pool string  `json:"pool"`
}

func clusterGuests(ctx context.Context, r setup.Runner) ([]clusterGuest, error) {
	var out []clusterGuest
	if err := pvesh(ctx, r, &out, "/cluster/resources", "--type", "vm"); err != nil {
		return nil, fmt.Errorf("reading /cluster/resources: %w", err)
	}
	return out, nil
}

// containerNode returns the node of the cluster the container vmid is on;
// false when no node has it.
func containerNode(ctx context.Context, r setup.Runner, vmid int) (string, bool, error) {
	guests, err := clusterGuests(ctx, r)
	if err != nil {
		return "", false, err
	}
	for _, g := range guests {
		if g.Type == "lxc" && int(g.VMID) == vmid {
			return g.Node, true, nil
		}
	}
	return "", false, nil
}

// readAccess reads the access control of the cluster and the pool of every
// guest, which only /cluster/resources gives reliably.
func readAccess(ctx context.Context, r setup.Runner) (access.Data, error) {
	var d access.Data
	read := func(path string, decode func([]byte) error, args ...string) error {
		out, err := r.Run(ctx, "pvesh", append(append([]string{"get", path}, args...), "--output-format", "json")...)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		if err := decode([]byte(out)); err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		return nil
	}
	var err error
	if err := read("/access/acl", func(b []byte) error { d.ACL, err = pve.DecodeACL(b); return err }); err != nil {
		return d, err
	}
	if err := read("/access/users", func(b []byte) error { d.Users, err = pve.DecodeUsers(b); return err }, "--full", "1"); err != nil {
		return d, err
	}
	if err := read("/access/groups", func(b []byte) error {
		d.Groups, err = pve.DecodeGroups(b, func(id string) ([]string, error) {
			out, err := r.Run(ctx, "pvesh", "get", "/access/groups/"+id, "--output-format", "json")
			if err != nil {
				return nil, err
			}
			return pve.DecodeGroupMembers([]byte(out))
		})
		return err
	}); err != nil {
		return d, err
	}
	if err := read("/access/roles", func(b []byte) error { d.Roles, err = pve.DecodeRoles(b); return err }); err != nil {
		return d, err
	}
	guests, err := clusterGuests(ctx, r)
	if err != nil {
		return d, err
	}
	d.Pools = map[model.GuestRef]string{}
	for _, g := range guests {
		kind := model.KindQEMU
		if g.Type == "lxc" {
			kind = model.KindLXC
		}
		if g.Pool != "" {
			d.Pools[model.GuestRef{Kind: kind, VMID: int(g.VMID)}] = g.Pool
		}
	}
	return d, nil
}

// permissions asks Proxmox what principal holds on path.
func permissions(ctx context.Context, r setup.Runner, principal, path string) ([]string, error) {
	out, err := r.Run(ctx, "pvesh", "get", "/access/permissions", "--userid", principal, "--path", path, "--output-format", "json")
	if err != nil {
		return nil, fmt.Errorf("asking Proxmox what %s holds on %s: %w", principal, path, err)
	}
	perms, err := pve.DecodePermissions([]byte(out))
	if err != nil {
		return nil, fmt.Errorf("asking Proxmox what %s holds on %s: %w", principal, path, err)
	}
	return perms[path], nil
}
