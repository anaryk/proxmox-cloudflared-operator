package applianceinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/access"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// errKilled is what the fake node panics with where the installer is killed
// outright: right after a command, with nothing of the installer run after it.
var errKilled = errors.New("killed")

// exitError is a command that ran and exited with a status, as the runner of
// setup reports it.
type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return fmt.Sprintf("exit status %d: %s", e.code, e.msg) }
func (e exitError) ExitCode() int { return e.code }

type fakeFile struct {
	data  []byte
	perms string
}

type fakeCT struct {
	cfg     map[string]string
	running bool
	files   map[string]fakeFile
	state   string   // what systemctl is-system-running prints
	failed  []string // the failed units
	version string   // of the pco inside
	meta    bool     // /var/lib/pco/cluster/meta is there
	lease   string   // the address DHCP gives eth0, as a CIDR; empty: none
	web     bool     // pco-web.service is enabled
}

type fakeUser struct {
	ID      string
	Comment string
	Enabled bool
	Groups  []string
	Tokens  []fakeToken
}

type fakeToken struct {
	Name    string
	Comment string
	Privsep bool
}

type fakeGroup struct {
	ID      string
	Members []string
}

type fakeACL struct {
	Path, Type, UGID, Role string
	NoPropagate            bool
}

type fakeStorage struct {
	ID      string
	Content string
}

type fakeIface struct {
	Name      string
	Type      string
	VLANAware bool
	PVID      int // bridge-pvid in /etc/network/interfaces; 0: none, so 1
}

type fakeVNet struct {
	Name, Zone string
	VLANAware  bool
}

type fakePool struct {
	Comment string
	Members []int
}

// initRun is a pco appliance init the fake container ran.
type initRun struct {
	VMID      int
	Env       string
	Bootstrap map[string]any
}

// fakeNode is a Proxmox VE node that keeps what the installer's commands
// change, as Proxmox does: a destroyed container takes its ACL lines and its
// pool membership with it, a deleted pool its lines, a removed token its
// lines, a deleted user its lines and those of its tokens (leaving the
// tokens' secrets behind, as Proxmox does), and a deleted role its lines.
type fakeNode struct {
	t       *testing.T
	dir     string
	name    string
	version string
	arch    string
	cluster []map[string]any // the entries of /cluster/status

	storages []fakeStorage
	ifaces   []fakeIface
	addrs    []string // "dev cidr", as ip prints them
	vnets    []fakeVNet
	firewall bool

	cts       map[int]*fakeCT
	vms       map[int]bool
	elsewhere map[int]string // containers on other nodes of the cluster, by node
	creating  map[int]int    // how often a pct create of the VMID is still listed as running
	users     []*fakeUser
	groups    []fakeGroup
	roles     map[string][]string
	acl       []fakeACL
	pools     map[string]*fakePool
	tags      []string
	volumes   map[string]string // volid -> the file on the node
	urls      map[string][]byte // what a download of an URL fetches
	tasks     map[string]string // upid -> exit status
	polls     map[string]int

	pcoVersion string // of the template's pco
	secrets    int
	leaked     []string // token secrets a user delete left behind
	inits      []initRun
	purges     []string
	cfObjects  []string // what the appliance's install has at Cloudflare
	pushes     []string // "<vmid> <dst> <perms>"

	ran    []string
	before func(line string) // called before every command
	hooks  []hook
	killAt string // the command after which the installer is killed
}

type hook struct {
	prefix string
	fn     func(ctx context.Context, args []string) (string, error)
}

func newFakeNode(t *testing.T) *fakeNode {
	f := &fakeNode{
		t: t, dir: t.TempDir(), name: "pve1", version: "9.2.21", arch: "amd64",
		storages:  []fakeStorage{{"local", "iso,vztmpl,backup"}, {"local-zfs", "images,rootdir"}},
		ifaces:    []fakeIface{{Name: "vmbr0", Type: "bridge"}, {Name: "vmbr1", Type: "bridge", VLANAware: true}, {Name: "eno1", Type: "eth"}},
		addrs:     []string{"vmbr0 192.0.2.10/24"},
		cts:       map[int]*fakeCT{},
		vms:       map[int]bool{},
		elsewhere: map[int]string{},
		creating:  map[int]int{},
		users:     []*fakeUser{{ID: "root@pam", Enabled: true}},
		roles: map[string][]string{
			"Administrator": {"Sys.Modify", "Sys.Audit", "VM.Audit", "VM.Console", "VM.Config.Network", "Permissions.Modify", "Pool.Allocate"},
			"PVEVMUser":     {"VM.Audit", "VM.Console", "VM.PowerMgmt", "VM.Backup", "VM.Config.CDROM", "VM.Config.Cloudinit"},
			"PVEAuditor":    {"VM.Audit", "Sys.Audit", "Datastore.Audit"},
			"NoAccess":      {},
		},
		acl:        []fakeACL{{Path: "/", Type: "user", UGID: "root@pam", Role: "Administrator"}},
		pools:      map[string]*fakePool{},
		tags:       []string{"admin-only"},
		volumes:    map[string]string{},
		urls:       map[string][]byte{},
		tasks:      map[string]string{},
		polls:      map[string]int{},
		pcoVersion: testVersion,
	}
	return f
}

// on makes the next command that starts with prefix answer with fn.
func (f *fakeNode) on(prefix string, fn func(ctx context.Context, args []string) (string, error)) {
	f.hooks = append(f.hooks, hook{prefix: prefix, fn: fn})
}

func (f *fakeNode) Run(ctx context.Context, name string, args ...string) (string, error) {
	f.t.Helper()
	line := strings.Join(append([]string{name}, args...), " ")
	f.ran = append(f.ran, line)
	if f.before != nil {
		f.before(line)
	}
	out, err := f.dispatch(ctx, line, name, args)
	if f.killAt != "" && strings.HasPrefix(line, f.killAt) {
		f.killAt = ""
		panic(errKilled)
	}
	return out, err
}

func (f *fakeNode) dispatch(ctx context.Context, line, name string, args []string) (string, error) {
	for i, h := range f.hooks {
		if strings.HasPrefix(line, h.prefix) {
			f.hooks = slices.Delete(f.hooks, i, i+1)
			return h.fn(ctx, args)
		}
	}
	switch name {
	case "pveversion":
		return "pve-manager/" + f.version + "/0123456789abcdef (running kernel: 7.0.14-20-pve)\n", nil
	case "dpkg":
		return f.arch + "\n", nil
	case "mountpoint":
		return "", nil
	case "ip":
		return f.ipAddr(), nil
	case "pvesh":
		return f.pvesh(args)
	case "pveum":
		return f.pveum(args)
	case "pct":
		return f.pct(args)
	case "qm":
		if len(args) == 2 && args[0] == "status" {
			if id, _ := strconv.Atoi(args[1]); f.vms[id] {
				return "status: stopped\n", nil
			}
			return "", fmt.Errorf("Configuration file 'nodes/%s/qemu-server/%s.conf' does not exist", f.name, args[1])
		}
	case "pvesm":
		return f.pvesm(args)
	}
	f.t.Errorf("unexpected command %q", line)
	return "", errors.New("unexpected command")
}

func asJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// flag returns the value of --name in args.
func flag(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func (f *fakeNode) ipAddr() string {
	var b strings.Builder
	for i, a := range f.addrs {
		dev, cidr, _ := strings.Cut(a, " ")
		fmt.Fprintf(&b, "%d: %s    inet %s brd 192.0.2.255 scope global %s\\       valid_lft forever preferred_lft forever\n", i+2, dev, cidr, dev)
	}
	return b.String()
}

func (f *fakeNode) pvesh(args []string) (string, error) {
	if len(args) < 2 {
		return "", errors.New("pvesh: no path")
	}
	verb, path := args[0], args[1]
	switch {
	case verb == "get" && path == "/cluster/status":
		if f.cluster != nil {
			return asJSON(f.cluster), nil
		}
		return asJSON([]map[string]any{{"type": "node", "name": f.name, "ip": "192.0.2.10", "online": 1, "local": 1}}), nil
	case verb == "get" && path == "/nodes/"+f.name+"/storage":
		var out []map[string]any
		for _, s := range f.storages {
			out = append(out, map[string]any{"storage": s.ID, "content": s.Content, "active": 1, "enabled": 1, "type": "dir"})
		}
		return asJSON(out), nil
	case verb == "get" && path == "/nodes/"+f.name+"/network":
		var out []map[string]any
		for _, i := range f.ifaces {
			e := map[string]any{"iface": i.Name, "type": i.Type}
			if i.VLANAware {
				e["bridge_vlan_aware"] = 1
			}
			// What Proxmox does not parse of a stanza it lists under options,
			// as it does a hwaddress line.
			if i.PVID != 0 {
				e["options"] = []string{"bridge-pvid " + strconv.Itoa(i.PVID)}
			}
			out = append(out, e)
		}
		return asJSON(out), nil
	case verb == "get" && path == "/cluster/sdn/vnets":
		var out []map[string]any
		for _, v := range f.vnets {
			e := map[string]any{"vnet": v.Name, "zone": v.Zone, "type": "vnet"}
			if v.VLANAware {
				e["vlanaware"] = 1
			}
			out = append(out, e)
		}
		return asJSON(out), nil
	case verb == "get" && path == "/cluster/nextid":
		for id := 100; ; id++ {
			if f.cts[id] == nil && !f.vms[id] && f.elsewhere[id] == "" {
				return asJSON(strconv.Itoa(id)), nil
			}
		}
	case verb == "get" && path == "/cluster/firewall/options":
		if f.firewall {
			return `{"enable":1,"digest":"x"}`, nil
		}
		return `{"digest":"x"}`, nil
	case verb == "get" && path == "/cluster/options":
		return asJSON(map[string]any{"registered-tags": strings.Join(f.tags, ";")}), nil
	case verb == "set" && path == "/cluster/options":
		if slices.Contains(args, "--delete") {
			f.tags = nil
			return "", nil
		}
		f.tags = pve.SplitTags(flag(args, "--registered-tags"))
		return "", nil
	case verb == "get" && path == "/access/acl":
		return f.aclJSON(), nil
	case verb == "get" && path == "/access/users":
		var out []map[string]any
		for _, u := range f.users {
			var toks []map[string]any
			for _, t := range u.Tokens {
				toks = append(toks, map[string]any{"tokenid": t.Name, "privsep": b01(t.Privsep), "comment": t.Comment})
			}
			out = append(out, map[string]any{"userid": u.ID, "enable": b01(u.Enabled), "groups": strings.Join(u.Groups, ","), "tokens": toks})
		}
		return asJSON(out), nil
	case verb == "get" && path == "/access/groups":
		var out []map[string]any
		for _, g := range f.groups {
			out = append(out, map[string]any{"groupid": g.ID, "users": strings.Join(g.Members, ",")})
		}
		return asJSON(out), nil
	case verb == "get" && path == "/access/roles":
		var out []map[string]any
		for _, id := range slices.Sorted(maps.Keys(f.roles)) {
			out = append(out, map[string]any{"roleid": id, "privs": strings.Join(f.roles[id], ",")})
		}
		return asJSON(out), nil
	case verb == "get" && path == "/cluster/resources":
		var out []map[string]any
		for _, id := range slices.Sorted(maps.Keys(f.cts)) {
			out = append(out, map[string]any{"id": fmt.Sprintf("lxc/%d", id), "type": "lxc", "vmid": id, "node": f.name, "pool": f.poolOf(id)})
		}
		for _, id := range slices.Sorted(maps.Keys(f.vms)) {
			out = append(out, map[string]any{"id": fmt.Sprintf("qemu/%d", id), "type": "qemu", "vmid": id, "node": f.name})
		}
		for _, id := range slices.Sorted(maps.Keys(f.elsewhere)) {
			out = append(out, map[string]any{"id": fmt.Sprintf("lxc/%d", id), "type": "lxc", "vmid": id, "node": f.elsewhere[id], "pool": f.poolOf(id)})
		}
		return asJSON(out), nil
	case verb == "get" && path == "/access/permissions":
		who, on := flag(args, "--userid"), flag(args, "--path")
		privs := map[string]int{}
		for _, p := range access.Privileges(f.accessData(), who, on) {
			privs[p] = 1
		}
		return asJSON(map[string]any{on: privs}), nil
	case verb == "get" && path == "/pools":
		p := f.pools[flag(args, "--poolid")]
		if p == nil {
			return "", fmt.Errorf("pool '%s' does not exist", flag(args, "--poolid"))
		}
		var members []map[string]any
		for _, id := range p.Members {
			members = append(members, map[string]any{"id": "lxc/" + strconv.Itoa(id), "vmid": id, "type": "lxc"})
		}
		return asJSON([]map[string]any{{"poolid": flag(args, "--poolid"), "comment": p.Comment, "members": members}}), nil
	case verb == "get" && strings.HasSuffix(path, "/content"):
		storage := strings.TrimSuffix(strings.TrimPrefix(path, "/nodes/"+f.name+"/storage/"), "/content")
		var out []map[string]any
		for _, v := range slices.Sorted(maps.Keys(f.volumes)) {
			if strings.HasPrefix(v, storage+":vztmpl/") {
				out = append(out, map[string]any{"volid": v, "content": "vztmpl", "format": "tzst"})
			}
		}
		return asJSON(out), nil
	case verb == "create" && strings.HasSuffix(path, "/download-url"):
		return f.download(path, args)
	case verb == "get" && strings.Contains(path, "/tasks/"):
		upid := strings.TrimSuffix(strings.TrimPrefix(path, "/nodes/"+f.name+"/tasks/"), "/status")
		status, ok := f.tasks[upid]
		if !ok {
			return "", fmt.Errorf("no task %s", upid)
		}
		if f.polls[upid]++; f.polls[upid] == 1 {
			return `{"status":"running"}`, nil
		}
		return asJSON(map[string]string{"status": "stopped", "exitstatus": status}), nil
	case verb == "get" && path == "/nodes/"+f.name+"/tasks":
		id, _ := strconv.Atoi(flag(args, "--vmid"))
		if f.creating[id] > 0 {
			f.creating[id]--
			upid := fmt.Sprintf("UPID:%s:00001234:00000000:00000000:vzcreate:%d:root@pam:", f.name, id)
			return asJSON([]map[string]any{{"upid": upid, "type": "vzcreate", "id": strconv.Itoa(id), "node": f.name}}), nil
		}
		return "[]", nil
	case verb == "get" && strings.HasPrefix(path, "/nodes/"+f.name+"/lxc/") && strings.HasSuffix(path, "/config"):
		id, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, "/nodes/"+f.name+"/lxc/"), "/config"))
		ct := f.cts[id]
		if ct == nil {
			return "", fmt.Errorf("Configuration file 'nodes/%s/lxc/%d.conf' does not exist", f.name, id)
		}
		return asJSON(ct.cfg), nil
	}
	f.t.Errorf("unexpected pvesh %v", args)
	return "", errors.New("unexpected command")
}

func b01(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (f *fakeNode) download(path string, args []string) (string, error) {
	storage := strings.TrimSuffix(strings.TrimPrefix(path, "/nodes/"+f.name+"/storage/"), "/download-url")
	name, url, sum := flag(args, "--filename"), flag(args, "--url"), flag(args, "--checksum")
	upid := fmt.Sprintf("UPID:%s:0000%04X:00000000:00000000:download:%s:root@pam:", f.name, len(f.tasks)+1, name)
	data, ok := f.urls[url]
	got := sha256.Sum256(data)
	switch {
	case !ok:
		f.tasks[upid] = "404 Not Found"
	case hex.EncodeToString(got[:]) != sum:
		f.tasks[upid] = "checksum mismatch: got '" + hex.EncodeToString(got[:]) + "' != expected '" + sum + "'"
	default:
		file := filepath.Join(f.dir, "volumes", storage, name)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(file, data, 0o644); err != nil {
			return "", err
		}
		f.volumes[storage+":vztmpl/"+name] = file
		f.tasks[upid] = "OK"
	}
	// pvesh exits 0 also when the download fails: only the task says.
	return upid + "\n", nil
}

func (f *fakeNode) pvesm(args []string) (string, error) {
	switch {
	case len(args) == 2 && args[0] == "path":
		if file, ok := f.volumes[args[1]]; ok {
			return file + "\n", nil
		}
		return "", fmt.Errorf("no such volume '%s'", args[1])
	case len(args) == 2 && args[0] == "free":
		if _, ok := f.volumes[args[1]]; !ok {
			return "", fmt.Errorf("no such volume '%s'", args[1])
		}
		delete(f.volumes, args[1])
		return "", nil
	}
	f.t.Errorf("unexpected pvesm %v", args)
	return "", errors.New("unexpected command")
}

func (f *fakeNode) aclJSON() string {
	var out []map[string]any
	for _, a := range f.acl {
		out = append(out, map[string]any{"path": a.Path, "type": a.Type, "ugid": a.UGID, "roleid": a.Role, "propagate": b01(!a.NoPropagate)})
	}
	return asJSON(out)
}

func (f *fakeNode) user(id string) *fakeUser {
	for _, u := range f.users {
		if u.ID == id {
			return u
		}
	}
	return nil
}

func (f *fakeNode) token(full string) (*fakeUser, int) {
	user, name, _ := strings.Cut(full, "!")
	u := f.user(user)
	if u == nil {
		return nil, -1
	}
	return u, slices.IndexFunc(u.Tokens, func(t fakeToken) bool { return t.Name == name })
}

func (f *fakeNode) pveum(args []string) (string, error) {
	line := strings.Join(args, " ")
	switch {
	case line == "role list --output-format json":
		var out []map[string]any
		for _, id := range slices.Sorted(maps.Keys(f.roles)) {
			out = append(out, map[string]any{"roleid": id, "privs": strings.Join(f.roles[id], ","), "special": 0})
		}
		return asJSON(out), nil
	case args[0] == "role" && args[1] == "add":
		if f.roles[args[2]] != nil {
			return "", fmt.Errorf("role '%s' already exists", args[2])
		}
		f.roles[args[2]] = strings.Split(flag(args, "--privs"), ",")
		return "", nil
	case args[0] == "role" && args[1] == "modify":
		f.roles[args[2]] = append(f.roles[args[2]], strings.Split(flag(args, "--privs"), ",")...)
		return "", nil
	case args[0] == "role" && args[1] == "delete":
		if f.roles[args[2]] == nil {
			return "", fmt.Errorf("role '%s' does not exist", args[2])
		}
		delete(f.roles, args[2])
		f.acl = slices.DeleteFunc(f.acl, func(a fakeACL) bool { return a.Role == args[2] })
		return "", nil
	case line == "user list --output-format json":
		var out []map[string]any
		for _, u := range f.users {
			out = append(out, map[string]any{"userid": u.ID, "comment": u.Comment, "enable": b01(u.Enabled)})
		}
		return asJSON(out), nil
	case args[0] == "user" && args[1] == "add":
		if f.user(args[2]) != nil {
			return "", fmt.Errorf("user '%s' already exists", args[2])
		}
		f.users = append(f.users, &fakeUser{ID: args[2], Comment: flag(args, "--comment"), Enabled: true})
		return "", nil
	case args[0] == "user" && args[1] == "delete":
		u := f.user(args[2])
		if u == nil {
			return "", fmt.Errorf("user '%s' does not exist", args[2])
		}
		for _, t := range u.Tokens {
			f.leaked = append(f.leaked, u.ID+"!"+t.Name)
		}
		f.users = slices.DeleteFunc(f.users, func(o *fakeUser) bool { return o == u })
		f.acl = slices.DeleteFunc(f.acl, func(a fakeACL) bool { return a.UGID == u.ID || strings.HasPrefix(a.UGID, u.ID+"!") })
		return "", nil
	case len(args) >= 3 && args[0] == "user" && args[1] == "token" && args[2] == "list":
		u := f.user(args[3])
		if u == nil {
			return "", fmt.Errorf("no such user ('%s')", args[3])
		}
		var out []map[string]any
		for _, t := range u.Tokens {
			out = append(out, map[string]any{"tokenid": t.Name, "comment": t.Comment, "privsep": b01(t.Privsep), "expire": 0})
		}
		return asJSON(out), nil
	case len(args) >= 3 && args[0] == "user" && args[1] == "token" && args[2] == "add":
		u := f.user(args[3])
		if u == nil {
			return "", fmt.Errorf("no such user ('%s')", args[3])
		}
		if slices.ContainsFunc(u.Tokens, func(t fakeToken) bool { return t.Name == args[4] }) {
			return "", fmt.Errorf("Token already exists.")
		}
		u.Tokens = append(u.Tokens, fakeToken{Name: args[4], Comment: flag(args, "--comment"), Privsep: flag(args, "--privsep") != "0"})
		f.secrets++
		return asJSON(map[string]any{"full-tokenid": u.ID + "!" + args[4], "info": map[string]any{"privsep": flag(args, "--privsep")},
			"value": fmt.Sprintf("%s%d", pveSecret, f.secrets)}), nil
	case len(args) >= 3 && args[0] == "user" && args[1] == "token" && args[2] == "remove":
		u, i := f.token(args[3] + "!" + args[4])
		if i < 0 {
			return "", fmt.Errorf("no such token '%s' for user '%s'", args[4], args[3])
		}
		u.Tokens = slices.Delete(u.Tokens, i, i+1)
		full := args[3] + "!" + args[4]
		f.acl = slices.DeleteFunc(f.acl, func(a fakeACL) bool { return a.Type == "token" && a.UGID == full })
		return "", nil
	case line == "acl list --output-format json":
		return f.aclJSON(), nil
	case args[0] == "acl" && (args[1] == "modify" || args[1] == "delete"):
		return f.aclChange(args[1], args[2], args[3:])
	case line == "pool list --output-format json":
		var out []map[string]any
		for _, id := range slices.Sorted(maps.Keys(f.pools)) {
			out = append(out, map[string]any{"poolid": id, "comment": f.pools[id].Comment})
		}
		return asJSON(out), nil
	case args[0] == "pool" && args[1] == "add":
		if f.pools[args[2]] != nil {
			return "", fmt.Errorf("pool '%s' already exists", args[2])
		}
		f.pools[args[2]] = &fakePool{Comment: flag(args, "--comment")}
		return "", nil
	case args[0] == "pool" && args[1] == "delete":
		p := f.pools[args[2]]
		if p == nil {
			return "", fmt.Errorf("pool '%s' does not exist", args[2])
		}
		if len(p.Members) > 0 {
			return "", errors.New("pool is not empty")
		}
		delete(f.pools, args[2])
		f.acl = slices.DeleteFunc(f.acl, func(a fakeACL) bool { return a.Path == "/pool/"+args[2] })
		return "", nil
	}
	f.t.Errorf("unexpected pveum %v", args)
	return "", errors.New("unexpected command")
}

func (f *fakeNode) aclChange(verb, path string, args []string) (string, error) {
	role := flag(args, "--roles")
	if f.roles[role] == nil {
		return "", fmt.Errorf("role '%s' does not exist", role)
	}
	var lines []fakeACL
	if u := flag(args, "--users"); u != "" {
		if f.user(u) == nil {
			return "", fmt.Errorf("user '%s' does not exist", u)
		}
		lines = append(lines, fakeACL{Path: path, Type: "user", UGID: u, Role: role})
	}
	if t := flag(args, "--tokens"); t != "" {
		if _, i := f.token(t); i < 0 {
			return "", fmt.Errorf("token '%s' does not exist", t)
		}
		lines = append(lines, fakeACL{Path: path, Type: "token", UGID: t, Role: role})
	}
	for _, l := range lines {
		f.acl = slices.DeleteFunc(f.acl, func(a fakeACL) bool {
			a.NoPropagate = false
			return a == l
		})
		if verb == "modify" {
			f.acl = append(f.acl, l)
		}
	}
	return "", nil
}

func (f *fakeNode) poolOf(vmid int) string {
	for id, p := range f.pools {
		if slices.Contains(p.Members, vmid) {
			return id
		}
	}
	return ""
}

// accessData is the access control as the access package takes it.
func (f *fakeNode) accessData() access.Data {
	d := access.Data{Pools: map[model.GuestRef]string{}}
	acl, err := pve.DecodeACL([]byte(f.aclJSON()))
	if err != nil {
		f.t.Fatal(err)
	}
	d.ACL = acl
	for _, u := range f.users {
		user := pve.User{ID: u.ID, Enabled: u.Enabled, Groups: u.Groups}
		for _, t := range u.Tokens {
			if user.Tokens == nil {
				user.Tokens = map[string]bool{}
			}
			user.Tokens[t.Name] = t.Privsep
		}
		d.Users = append(d.Users, user)
	}
	for _, g := range f.groups {
		d.Groups = append(d.Groups, pve.Group{ID: g.ID, Members: g.Members})
	}
	for id, privs := range f.roles {
		d.Roles = append(d.Roles, pve.Role{ID: id, Privs: privs})
	}
	for id := range f.cts {
		if p := f.poolOf(id); p != "" {
			d.Pools[model.GuestRef{Kind: model.KindLXC, VMID: id}] = p
		}
	}
	return d
}

func (f *fakeNode) pct(args []string) (string, error) {
	if len(args) < 2 {
		return "", errors.New("pct: no command")
	}
	id, _ := strconv.Atoi(args[1])
	ct := f.cts[id]
	missing := fmt.Errorf("Configuration file 'nodes/%s/lxc/%d.conf' does not exist", f.name, id)
	if args[0] == "create" {
		return f.create(id, args)
	}
	if ct == nil {
		return "", missing
	}
	if lock := ct.cfg["lock"]; lock != "" && slices.Contains([]string{"start", "set", "destroy"}, args[0]) {
		return "", fmt.Errorf("CT is locked (%s)", lock)
	}
	switch args[0] {
	case "unlock":
		delete(ct.cfg, "lock")
		return "", nil
	case "status":
		if ct.running {
			return "status: running\n", nil
		}
		return "status: stopped\n", nil
	case "start":
		if ct.running {
			return "", fmt.Errorf("CT %d already running", id)
		}
		ct.running = true
		return "", nil
	case "stop":
		ct.running = false
		return "", nil
	case "set":
		return f.set(ct, id, args[2:])
	case "destroy":
		switch {
		case ct.running:
			return "", fmt.Errorf("CT %d is running - destroy failed", id)
		case ct.cfg["protection"] == "1":
			return "", errors.New("can't remove CT - protection mode enabled")
		}
		delete(f.cts, id)
		f.acl = slices.DeleteFunc(f.acl, func(a fakeACL) bool { return a.Path == "/vms/"+args[1] })
		for _, p := range f.pools {
			p.Members = slices.DeleteFunc(p.Members, func(m int) bool { return m == id })
		}
		return "", nil
	case "push":
		if !ct.running {
			return "", errors.New("can only push files to a running CT")
		}
		data, err := os.ReadFile(args[2])
		if err != nil {
			return "", err
		}
		if args[3] == bootstrapFile {
			f.checkBootstrapFile(args[2])
		}
		ct.files[args[3]] = fakeFile{data: data, perms: flag(args, "--perms")}
		f.pushes = append(f.pushes, fmt.Sprintf("%d %s %s", id, args[3], flag(args, "--perms")))
		return "", nil
	case "pull":
		if !ct.running {
			return "", errors.New("can only pull files from a running VM")
		}
		file, ok := ct.files[args[2]]
		if !ok {
			return "", fmt.Errorf("failed to open %s: No such file or directory", args[2])
		}
		return "", os.WriteFile(args[3], file.data, 0o600)
	case "exec":
		if !ct.running {
			return "", exitError{255, fmt.Sprintf("container '%d' not running!", id)}
		}
		if args[2] != "--keep-env" || args[3] != "0" || args[4] != "--" {
			f.t.Errorf("pct exec without --keep-env 0: %v", args)
		}
		return f.exec(ct, id, args[5:])
	}
	f.t.Errorf("unexpected pct %v", args)
	return "", errors.New("unexpected command")
}

// checkBootstrapFile requires the bootstrap to be readable by root alone, in
// a directory of its own under /run that only root may enter.
func (f *fakeNode) checkBootstrapFile(path string) {
	info, err := os.Stat(path)
	if err != nil {
		f.t.Errorf("the bootstrap: %v", err)
		return
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		f.t.Errorf("the directory of the bootstrap: %v", err)
		return
	}
	if info.Mode().Perm() != 0o600 || dir.Mode().Perm() != 0o700 {
		f.t.Errorf("the bootstrap has mode %04o in a directory of mode %04o: want 0600 in 0700", info.Mode().Perm(), dir.Mode().Perm())
	}
	if !strings.HasPrefix(filepath.Base(filepath.Dir(path)), "pco-appliance-install-") {
		f.t.Errorf("the bootstrap is written in %s, not in a directory of the run", filepath.Dir(path))
	}
}

func (f *fakeNode) create(id int, args []string) (string, error) {
	if f.cts[id] != nil || f.vms[id] {
		return "", fmt.Errorf("unable to create CT %d - CT %d already exists on node '%s'", id, id, f.name)
	}
	template := args[2]
	if _, ok := f.volumes[template]; !ok {
		if _, err := os.Stat(template); err != nil {
			return "", fmt.Errorf("volume '%s' does not exist", template)
		}
	}
	storage, size, _ := strings.Cut(flag(args, "--rootfs"), ":")
	mp0 := flag(args, "--mp0")
	mpStorage, rest, _ := strings.Cut(mp0, ":")
	mpSize, mpOpts, _ := strings.Cut(rest, ",")
	cfg := map[string]string{
		"arch": f.arch, "ostype": flag(args, "--ostype"), "hostname": flag(args, "--hostname"),
		"cores": flag(args, "--cores"), "memory": flag(args, "--memory"), "swap": flag(args, "--swap"),
		"unprivileged": flag(args, "--unprivileged"), "features": flag(args, "--features"),
		"onboot": flag(args, "--onboot"), "startup": flag(args, "--startup"), "tags": flag(args, "--tags"),
		"description": flag(args, "--description") + "\n",
		"rootfs":      fmt.Sprintf("%s:subvol-%d-disk-0,size=%sG", storage, id, size),
		"mp0":         fmt.Sprintf("%s:subvol-%d-disk-1,%s,size=%sG", mpStorage, id, mpOpts, mpSize),
		"net0":        strings.Replace(flag(args, "--net0"), ",", fmt.Sprintf(",hwaddr=BC:24:11:00:%02X:%02X,", id/256%256, id%256), 1) + ",type=veth",
	}
	f.cts[id] = &fakeCT{cfg: cfg, files: map[string]fakeFile{}, state: "degraded", failed: []string{"pco.service"}, version: f.pcoVersion,
		lease: "192.0.2.150/24"}
	if pool := flag(args, "--pool"); pool != "" {
		p := f.pools[pool]
		if p == nil {
			delete(f.cts, id)
			return "", fmt.Errorf("pool '%s' does not exist", pool)
		}
		p.Members = append(p.Members, id)
	}
	return "", nil
}

func (f *fakeNode) set(ct *fakeCT, id int, args []string) (string, error) {
	for i := 0; i+1 < len(args); i += 2 {
		switch key := strings.TrimPrefix(args[i], "--"); key {
		case "protection", "description":
			ct.cfg[key] = args[i+1]
		case "mp0":
			if ct.running {
				return "", errors.New("unable to hotplug mp0: the container runs")
			}
			storage, rest, _ := strings.Cut(args[i+1], ":")
			size, opts, _ := strings.Cut(rest, ",")
			ct.cfg["mp0"] = fmt.Sprintf("%s:subvol-%d-disk-2,%s,size=%sG", storage, id, opts, size)
			ct.files, ct.meta = map[string]fakeFile{}, false
		default:
			return "", fmt.Errorf("pct set: unexpected %s", key)
		}
	}
	return "", nil
}

func (f *fakeNode) exec(ct *fakeCT, id int, cmd []string) (string, error) {
	env := ""
	if len(cmd) > 2 && cmd[0] == "env" {
		env, cmd = cmd[1], cmd[2:]
	}
	line := strings.Join(cmd, " ")
	switch {
	case line == "timeout 90 systemctl is-system-running --wait":
		if ct.state != "running" {
			return ct.state + "\n", exitError{1, ""}
		}
		return "running\n", nil
	case line == "systemctl list-units --state=failed --plain --no-legend --no-pager":
		var b strings.Builder
		for _, u := range ct.failed {
			fmt.Fprintf(&b, "%s loaded failed failed %s\n", u, u)
		}
		return b.String(), nil
	case line == "pco version":
		return fmt.Sprintf("pco %s (none, unknown)\n", ct.version), nil
	case line == "test -f "+markerFile:
		if _, ok := ct.files[markerFile]; ok {
			return "", nil
		}
		return "", exitError{1, ""}
	case line == "test -d "+stateMeta:
		if ct.meta {
			return "", nil
		}
		return "", exitError{1, ""}
	case line == "pco appliance init --bootstrap "+bootstrapFile:
		return f.init(ct, id, env)
	case line == "ip -j -4 addr show dev eth0":
		return f.eth0(ct), nil
	case line == "pco web cert":
		// The daemon makes the certificate once init gave it a store.
		if !ct.meta {
			return "", exitError{1, "the web interface has no certificate yet"}
		}
		return "mode         self-signed: a key of its own and a self-signed certificate, renewed by pco\n" +
			"SHA-256      " + fakeFingerprint + "\n", nil
	case line == "systemctl enable --now pco-web.service":
		ct.web = true
		return "", nil
	case line == "pco appliance purge --list":
		return strings.Join(f.cfObjects, "\n"), nil
	case line == "pco appliance purge":
		f.purges = append(f.purges, fmt.Sprintf("%d %s", id, env))
		f.cfObjects = nil
		return "Cloudflare: deleted what the install had\n", nil
	case strings.HasPrefix(line, "pco appliance manifest "):
		return f.manifestCmd(ct, cmd[3:])
	}
	f.t.Errorf("unexpected command in lxc/%d: %q", id, line)
	return "", errors.New("unexpected command")
}

// fakeFingerprint is the fingerprint of the web certificate of every fake
// container.
const fakeFingerprint = "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89"

// eth0 is what ip -j -4 addr prints of eth0: the address of net0, or the
// lease DHCP gives it.
func (f *fakeNode) eth0(ct *fakeCT) string {
	cidr := option(ct.cfg["net0"], "ip")
	if cidr == "dhcp" {
		cidr = ct.lease
	}
	addr, bits, ok := strings.Cut(cidr, "/")
	if !ok {
		return `[{"ifindex":2,"ifname":"eth0","addr_info":[]}]`
	}
	return `[{"ifindex":2,"ifname":"eth0","addr_info":[{"family":"inet","local":"` + addr + `","prefixlen":` + bits +
		`,"scope":"global","label":"eth0"}]}]`
}

func (f *fakeNode) init(ct *fakeCT, id int, env string) (string, error) {
	file, ok := ct.files[bootstrapFile]
	if !ok {
		return "", errors.New("reading the bootstrap: no such file")
	}
	delete(ct.files, bootstrapFile)
	var b map[string]any
	if err := json.Unmarshal(file.data, &b); err != nil {
		f.t.Errorf("the bootstrap: %v", err)
	}
	if b["mode"] == "install" && ct.meta {
		return "", exitError{1, "init step store: the volume holds install 0123456789ab already, and mode install never replaces one"}
	}
	if file.perms != "0600" {
		f.t.Errorf("the bootstrap was pushed with mode %s", file.perms)
	}
	f.inits = append(f.inits, initRun{VMID: id, Env: env, Bootstrap: b})
	m, _ := json.Marshal(b["manifest"])
	ct.files[manifestFile] = fakeFile{data: m, perms: "0600"}
	ct.meta = true
	return fmt.Sprintf("init: mode %s done\n", b["mode"]), nil
}

func (f *fakeNode) manifestCmd(ct *fakeCT, args []string) (string, error) {
	var m setup.Manifest
	if err := json.Unmarshal(ct.files[manifestFile].data, &m); err != nil || m.Appliance == nil {
		return "", errors.New("not the manifest of an appliance")
	}
	g := setup.NetworkGrant{Zone: flag(args, "--zone"), VNet: flag(args, "--vnet")}
	g.VLAN, _ = strconv.Atoi(flag(args, "--vlan"))
	for i, a := range args {
		if a == "--created-role" {
			g.CreatedRoles = append(g.CreatedRoles, args[i+1])
		}
	}
	switch args[0] {
	case "add":
		m.Appliance.Grants = append(m.Appliance.Grants, g)
	case "remove":
		m.Appliance.Grants = slices.DeleteFunc(m.Appliance.Grants, g.Same)
	}
	data, _ := json.Marshal(m)
	ct.files[manifestFile] = fakeFile{data: data, perms: "0600"}
	return "", nil
}

// addToken gives a user a token, as an admin would.
func (f *fakeNode) addToken(user, name, comment string, privsep bool) {
	u := f.user(user)
	if u == nil {
		u = &fakeUser{ID: user, Enabled: true}
		f.users = append(f.users, u)
	}
	u.Tokens = append(u.Tokens, fakeToken{Name: name, Comment: comment, Privsep: privsep})
}

// grant adds a line to the access control list, as an admin would.
func (f *fakeNode) grant(path, kind, ugid, role string) {
	f.acl = append(f.acl, fakeACL{Path: path, Type: kind, UGID: ugid, Role: role})
}

// has reports whether a line of the access control list is there.
func (f *fakeNode) has(path, kind, ugid, role string) bool {
	return slices.Contains(f.acl, fakeACL{Path: path, Type: kind, UGID: ugid, Role: role})
}

func (f *fakeNode) userIDs() []string {
	var ids []string
	for _, u := range f.users {
		ids = append(ids, u.ID)
	}
	return ids
}

func (f *fakeNode) tokenNames(user string) []string {
	var names []string
	if u := f.user(user); u != nil {
		for _, t := range u.Tokens {
			names = append(names, t.Name)
		}
	}
	return names
}

func (f *fakeNode) count(prefix string) int {
	n := 0
	for _, line := range f.ran {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}
