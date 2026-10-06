package applianceinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/access"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// commands are what the installer runs on the node, and dpkg-deb, which
// install.sh unpacks the installer with.
var commands = []string{"pct", "pveum", "pvesh", "pvesm", "dpkg-deb"}

// preflight looks at the node and decides everything the install needs,
// before anything is made.
func (r *run) preflight(ctx context.Context) error {
	if err := r.node0(ctx); err != nil {
		return err
	}
	if err := r.cluster(ctx); err != nil {
		return err
	}
	if err := r.chooseStorages(ctx); err != nil {
		return err
	}
	if err := r.chooseBridge(ctx); err != nil {
		return err
	}
	data, err := readAccess(ctx, r.r)
	if err != nil {
		return err
	}
	r.data = data
	if err := r.chooseVMID(ctx); err != nil {
		return err
	}
	if err := r.otherInstalls(); err != nil {
		return err
	}
	if err := r.firewall(ctx); err != nil {
		return err
	}
	host := r.o.APIHost
	if host == "" {
		addr, ok := r.bridgeAddr()
		if !ok {
			return fmt.Errorf("the node has no address on %s, which the appliance would reach the API at: "+
				"name one with --api-host", r.netDevice())
		}
		host = addr
	}
	ep, err := r.chooseEndpoint(ctx, host)
	if err != nil {
		return err
	}
	r.j.Endpoint = ep
	return r.confirmDenials()
}

// node0 checks what the installer needs of the node itself.
func (r *run) node0(ctx context.Context) error {
	if r.h.euid() != 0 {
		return errors.New("pco appliance must run as root on the node")
	}
	out, err := r.r.Run(ctx, "pveversion")
	if err != nil {
		return fmt.Errorf("reading the version of Proxmox VE, is this a Proxmox VE node? %w", err)
	}
	if r.version, err = setup.ParsePVEVersion(out); err != nil {
		return err
	}
	if !r.version.Supported() {
		return fmt.Errorf("version %s of Proxmox VE is not supported: pco needs 8.4 or later, or 9", r.version)
	}
	out, err = r.r.Run(ctx, "dpkg", "--print-architecture")
	if err != nil {
		return fmt.Errorf("reading the architecture: %w", err)
	}
	switch r.arch = strings.TrimSpace(out); r.arch {
	case "amd64", "arm64":
	default:
		return fmt.Errorf("architecture %q is not supported: pco is built for amd64 and arm64", r.arch)
	}
	var missing []string
	for _, c := range commands {
		if err := r.h.findCommand(c); err != nil {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s not found: is this a Proxmox VE node?", strings.Join(missing, ", "))
	}
	if _, err := r.r.Run(ctx, "mountpoint", "-q", r.h.pveDir); err != nil {
		return fmt.Errorf("%s is not mounted, is pve-cluster running? %w", r.h.pveDir, err)
	}
	name, err := r.h.hostname()
	if err != nil {
		return fmt.Errorf("reading the host name: %w", err)
	}
	r.node, _, _ = strings.Cut(name, ".")
	if r.addrs, err = nodeAddrs(ctx, r.r); err != nil {
		return err
	}
	if len(r.addrs) == 0 {
		return errors.New("the node has no global IPv4 address")
	}
	r.ask.Info("preflight: Proxmox VE %s on %s, node %s", r.version, r.arch, r.node)
	return nil
}

func (r *run) cluster(ctx context.Context) error {
	c, err := readCluster(ctx, r.r)
	if err != nil {
		return err
	}
	switch {
	case c.name == "":
		r.ask.Info("preflight: %s is a node of its own, in no cluster", r.node)
	case c.quorate:
		r.ask.Info("preflight: %s is a member of cluster %s with %d nodes, quorate; one appliance serves the whole cluster", r.node, c.name, c.nodes)
	default:
		return fmt.Errorf("cluster %s has no quorum, so nothing can be written to /etc/pve: install once it has", c.name)
	}
	return nil
}

// chooseStorages chooses the storage of the container and the one the
// template is downloaded to.
func (r *run) chooseStorages(ctx context.Context) error {
	all, err := storages(ctx, r.r, r.node)
	if err != nil {
		return err
	}
	usable := func(content string) []string {
		var ids []string
		for _, s := range all {
			if s.holds(content) && bool(s.Active) {
				ids = append(ids, s.ID)
			}
		}
		return ids
	}
	rootdir := usable("rootdir")
	switch {
	case r.o.Storage != "" && !slices.Contains(rootdir, r.o.Storage):
		return fmt.Errorf("storage %s does not hold containers on node %s (those that do: %s)", r.o.Storage, r.node, orNone(rootdir))
	case r.o.Storage == "" && len(rootdir) == 1:
		r.o.Storage = rootdir[0]
	case r.o.Storage == "":
		return fmt.Errorf("choose the storage of the appliance with --storage: %s", orNone(rootdir))
	}
	r.j.Options.Storage = r.o.Storage
	if r.o.Template != "" {
		r.ask.Info("preflight: the container goes on storage %s", r.o.Storage)
		return nil
	}
	vztmpl := usable("vztmpl")
	switch {
	case r.o.TemplateStorage != "" && !slices.Contains(vztmpl, r.o.TemplateStorage):
		return fmt.Errorf("storage %s does not hold templates on node %s (those that do: %s)", r.o.TemplateStorage, r.node, orNone(vztmpl))
	case r.o.TemplateStorage != "":
	case slices.Contains(vztmpl, r.o.Storage):
		r.o.TemplateStorage = r.o.Storage
	case len(vztmpl) == 1:
		r.o.TemplateStorage = vztmpl[0]
	default:
		return fmt.Errorf("choose the storage the template is downloaded to with --template-storage: %s", orNone(vztmpl))
	}
	r.j.Options.TemplateStorage = r.o.TemplateStorage
	r.ask.Info("preflight: the container goes on storage %s, the template on %s", r.o.Storage, r.o.TemplateStorage)
	return nil
}

// chooseBridge checks the bridge and its VLANs: a card without a tag on a
// VLAN-aware bridge would be a member of every VLAN.
func (r *run) chooseBridge(ctx context.Context) error {
	b, err := r.readBridge(ctx)
	if err != nil {
		return err
	}
	switch {
	case r.o.VLAN != 0 && !b.vlanAware():
		return fmt.Errorf("bridge %s is not VLAN-aware, so --vlan %d cannot be given to the appliance's card", b.Name, r.o.VLAN)
	case r.o.VLAN == 0 && b.vlanAware():
		return fmt.Errorf("bridge %s is VLAN-aware: name the VLAN of the appliance with --vlan (--vlan %d for the "+
			"bridge's untagged VLAN), as a card without a tag is a member of every VLAN", b.Name, b.pvid())
	}
	return nil
}

// readBridge reads the bridge of the appliance's card, and its PVID.
func (r *run) readBridge(ctx context.Context) (nodeIface, error) {
	ifaces, err := nodeNetwork(ctx, r.r, r.node)
	if err != nil {
		return nodeIface{}, err
	}
	i := slices.IndexFunc(ifaces, func(i nodeIface) bool { return i.Name == r.o.Bridge && i.bridge() })
	if i < 0 {
		return nodeIface{}, fmt.Errorf("node %s has no bridge %s", r.node, r.o.Bridge)
	}
	r.pvid = ifaces[i].pvid()
	return ifaces[i], nil
}

// netDevice is the device of the node the appliance's card is on the
// network of: the bridge, or its VLAN device.
func (r *run) netDevice() string { return vlanDevice(r.o.Bridge, r.o.VLAN, r.pvid) }

// bridgeAddr is the node's first address on the network of the appliance.
func (r *run) bridgeAddr() (string, bool) {
	dev := r.netDevice()
	for _, a := range r.addrs {
		if a.Dev == dev {
			return a.Prefix.Addr().String(), true
		}
	}
	return "", false
}

// chooseVMID takes the VMID given or the next free one, and decides which
// principals that could reach into it the admin lets the installer deny.
func (r *run) chooseVMID(ctx context.Context) error {
	vmid := r.o.VMID
	if vmid == 0 {
		var err error
		if vmid, err = nextID(ctx, r.r); err != nil {
			return err
		}
		r.chosen = true
	}
	if guestExists(ctx, r.r, vmid) {
		return fmt.Errorf("VMID %d is taken: choose another with --vmid, or leave it out for the next free one", vmid)
	}
	r.j.VMID = vmid
	r.j.Denials = denials(r.data, vmid)
	r.ask.Info("preflight: the appliance will be lxc/%d", vmid)
	return nil
}

// otherInstalls warns of another install of pco: both would claim every guest
// with the gate tag. It goes on with --yes, and at a terminal only when the
// admin says so.
func (r *run) otherInstalls() error {
	var found []string
	switch b, err := r.h.readFile(filepath.Join(r.h.pveDir, hostInstall)); {
	case err == nil:
		var inst struct {
			ID string `json:"id"`
		}
		id := "of unknown id"
		if json.Unmarshal(b, &inst) == nil && inst.ID != "" {
			id = inst.ID
		}
		found = append(found, fmt.Sprintf("pco is set up on this node as the host profile (install %s): both would claim "+
			"every guest with the gate tag; uninstall one, or give this one another gate tag with --gate-tag", id))
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("looking for a host install of pco: %w", err)
	}
	for _, u := range r.data.Users {
		if u.ID != setup.UserID {
			continue
		}
		for _, name := range sortedKeys(u.Tokens) {
			if other, ok := applianceOf(name); ok && other != r.j.VMID {
				found = append(found, fmt.Sprintf("another appliance lxc/%d exists (its token %s): one appliance serves a cluster", other, setup.UserID+"!"+name))
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	for _, f := range found {
		r.ask.Warn("%s", f)
	}
	if r.o.Yes {
		r.ask.Warn("going on, as --yes says")
		return nil
	}
	ok, err := r.ask.Confirm("Install the appliance beside it?", false)
	switch {
	case err != nil:
		return err
	case !ok:
		return errors.New("aborted")
	}
	return nil
}

func (r *run) firewall(ctx context.Context) error {
	var opts struct {
		Enable flexBool `json:"enable"`
	}
	if err := pvesh(ctx, r.r, &opts, "/cluster/firewall/options"); err != nil {
		return fmt.Errorf("reading the firewall options of the datacenter: %w", err)
	}
	state := "off"
	if opts.Enable {
		state = "on"
	}
	r.ask.Info("preflight: the datacenter firewall is %s; the appliance reaches the API on port %s of the node", state, apiPort)
	return nil
}

// denial is a NoAccess line for a principal that can reach into the
// appliance.
type denial struct {
	Principal string   `json:"principal"`
	Privs     []string `json:"privs"`
	Path      string   `json:"path"`
	Flag      string   `json:"flag"` // --users or --tokens
	Who       string   `json:"who"`  // the user or the token the line names
	Why       string   `json:"why,omitempty"`
}

// line is the command an admin runs, with a token quoted for the shell.
func (d denial) line() string {
	who := d.Who
	if strings.Contains(who, "!") {
		who = "'" + who + "'"
	}
	return fmt.Sprintf("pveum acl modify %s %s %s --roles %s", d.Path, d.Flag, who, roleNoAccess)
}

// effect says what the line takes from the principal it names: on a path
// above the appliance, far more than the appliance.
func (d denial) effect(vmid int) []string {
	var what string
	switch d.Path {
	case "/":
		what = "every privilege in the whole cluster"
	case "/vms":
		what = "every privilege on every guest of the cluster"
	case "/pool":
		what = "every privilege on every pool of the cluster and on the guests in them"
	case "/pool/" + poolID:
		return []string{fmt.Sprintf("NoAccess on %s takes from %s what it holds on pool %s and the guests in it", d.Path, d.Who, poolID)}
	default:
		return []string{fmt.Sprintf("NoAccess on %s takes from %s what it holds on lxc/%d alone", d.Path, d.Who, vmid)}
	}
	return []string{
		fmt.Sprintf("NoAccess on %s takes from %s %s that no line further down grants it, not only what it holds on lxc/%d",
			d.Path, d.Who, what, vmid),
		fmt.Sprintf("the other way out: take back the grant that gives %s %s, then run the installer again", d.Who, strings.Join(d.Privs, ", ")),
	}
}

// denials are the NoAccess lines that keep every principal but the admins and
// pco's own out of the appliance vmid in pool pco, each of which could read
// its secrets: one on each path access.Refused names, where NoAccess takes
// away what the principal holds. A token without privilege separation holds
// the roles of its user, so its line names the user.
func denials(d access.Data, vmid int) []denial {
	var out []denial
	add := func(n denial) {
		i := slices.IndexFunc(out, func(o denial) bool { return o.Path == n.Path && o.Flag == n.Flag && o.Who == n.Who })
		if i < 0 {
			out = append(out, n)
			return
		}
		for _, p := range n.Privs {
			if !slices.Contains(out[i].Privs, p) {
				out[i].Privs = append(out[i].Privs, p)
			}
		}
		slices.Sort(out[i].Privs)
		if n.Why != "" && !strings.Contains(out[i].Why, n.Why) {
			out[i].Why = strings.TrimPrefix(out[i].Why+"; "+n.Why, "; ")
		}
	}
	for _, p := range access.Refused(d, vmid, poolID, setup.UserID) {
		flag, who, why := "--users", p.ID, ""
		if user, name, isToken := strings.Cut(p.ID, "!"); isToken {
			if privsep(d, user, name) {
				flag = "--tokens"
			} else {
				who, why = user, fmt.Sprintf("%s is not privilege-separated: it holds the roles of %s", p.ID, user)
			}
		}
		for _, path := range p.At {
			add(denial{Principal: p.ID, Privs: slices.Clone(p.Privs), Path: path, Flag: flag, Who: who, Why: why})
		}
	}
	return out
}

// privsep says whether a token separates its privileges; one not listed is
// taken to, as Proxmox makes them.
func privsep(d access.Data, user, name string) bool {
	for _, u := range d.Users {
		if u.ID == user {
			sep, known := u.Tokens[name]
			return sep || !known
		}
	}
	return true
}

// confirmDenials shows each NoAccess line with what it takes and asks once
// for all: only an explicit yes, at the question or with --deny-access, adds
// them; --yes never does, as pco never changes an ACL the admin did not ask
// for.
func (r *run) confirmDenials() error {
	if len(r.j.Denials) == 0 {
		r.ask.Info("preflight: no principal but the admins can reach into lxc/%d", r.j.VMID)
		return nil
	}
	for _, d := range r.j.Denials {
		r.ask.Warn("%s holds %s on lxc/%d, with which it can read the appliance's secrets", d.Principal, strings.Join(d.Privs, ", "), r.j.VMID)
		if d.Why != "" {
			r.ask.Warn("  %s", d.Why)
		}
		r.ask.Warn("  %s", d.line())
		for _, e := range d.effect(r.j.VMID) {
			r.ask.Warn("  %s", e)
		}
	}
	if r.o.DenyAccess {
		r.ask.Info("preflight: the NoAccess lines above are added once the container exists (--deny-access)")
		return nil
	}
	if !r.o.Yes {
		question := "Add the NoAccess line above?"
		if n := len(r.j.Denials); n > 1 {
			question = fmt.Sprintf("Add the %d NoAccess lines above?", n)
		}
		ok, err := r.ask.Confirm(question, false)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return fmt.Errorf("principals other than the admins can reach into lxc/%d: run the lines above, or let the installer "+
		"add them with --deny-access (--yes never does)", r.j.VMID)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
