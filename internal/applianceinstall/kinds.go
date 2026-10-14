package applianceinstall

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/clusterapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The kinds of an appliance the installer makes: KindCombined, the default,
// runs everything on a node of its own; in a cluster one KindController
// serves the agent containers, one KindAgent on each other node.
const (
	KindCombined   = store.ApplianceCombined
	KindController = store.ApplianceController
	KindAgent      = store.ApplianceAgent
)

const (
	// agentsIPSet is the ipset of the controller container's firewall that
	// holds the addresses of the agent containers, and agentsRule the rule
	// that lets them reach the agent endpoint and nobody else.
	agentsIPSet   = "pco-agents"
	agentsComment = "pco: the agent containers of the other nodes"
	joinFile      = stateDir + "/join.token"
	// roleDatastoreAudit is Proxmox's role that holds Datastore.Audit, which
	// a controller's token gets on its own storage only.
	roleDatastoreAudit = "PVEDatastoreAuditor"
)

// kindName is the kind of the options, never an empty one.
func (o Options) kindName() string {
	if o.Kind == "" {
		return KindCombined
	}
	return o.Kind
}

// checkKind refuses what does not go with the kind, before anything is
// looked at on the node.
func (o Options) checkKind() error {
	switch o.kindName() {
	case KindCombined, KindController, KindAgent:
	default:
		return fmt.Errorf("--kind %q: want combined, controller or agent", o.Kind)
	}
	agentKind := o.kindName() == KindAgent
	switch {
	case agentKind && o.JoinToken == "":
		return errors.New("an agent joins its controller with a join token: give it with --join-file <path> or --join -, " +
			"as pco cluster join-token --node <node> on the controller made it")
	case !agentKind && o.JoinToken != "":
		return errors.New("--join-file and --join go with --kind agent")
	case agentKind && o.CloudflareToken != "":
		return errors.New("an agent holds no Cloudflare token: the controller hands it what it runs")
	case o.HA && o.kindName() != KindController:
		return errors.New("--ha is for a controller: an agent serves the node it is installed on, and is never an HA resource; " +
			"a combined appliance is converted into a controller first")
	case len(o.HANodes) > 0 && !o.HA:
		return errors.New("--ha-nodes goes with --ha")
	}
	if agentKind {
		if _, err := pki.ParseJoinToken(o.JoinToken); err != nil {
			return fmt.Errorf("the join token: %w", err)
		}
	}
	for _, n := range o.HANodes {
		if !nodeName.MatchString(n) {
			return fmt.Errorf("--ha-nodes: %q is not the name of a node", n)
		}
	}
	return nil
}

// ctTags are the tags of the container of the kind, by which the installer
// of another kind finds it.
func ctTags(kind string) string { return "pco-appliance;pco-" + kind }

// kindOf is the kind a container's tags give it; empty for none.
func kindOf(tags string) string {
	for t := range strings.SplitSeq(tags, ";") {
		switch t {
		case "pco-" + KindCombined, "pco-" + KindController, "pco-" + KindAgent:
			return strings.TrimPrefix(t, "pco-")
		}
	}
	return ""
}

// appliancesByKind are the containers of pool pco by the kind their tags
// give them.
func appliancesByKind(ctx context.Context, r setup.Runner) (map[string][]clusterGuest, error) {
	guests, err := clusterGuests(ctx, r)
	if err != nil {
		return nil, err
	}
	out := map[string][]clusterGuest{}
	for _, g := range guests {
		if g.Type != "lxc" || g.Pool != poolID {
			continue
		}
		if k := kindOf(g.Tags); k != "" {
			out[k] = append(out[k], g)
		}
	}
	return out, nil
}

// checkCluster refuses a kind beside the appliances it does not go with: a
// combined appliance serves a node of its own and is converted, never joined;
// one controller serves an install; one agent serves a node, and the
// controller serves the node it runs on. An agent's token must be for this
// node and not expired.
func (r *run) checkCluster(ctx context.Context) error {
	kind := r.o.kindName()
	if kind == KindCombined {
		return nil
	}
	by, err := appliancesByKind(ctx, r.r)
	if err != nil {
		return err
	}
	if c := by[KindCombined]; len(c) > 0 {
		return fmt.Errorf("the combined appliance lxc/%d serves this cluster: pco appliance convert --vmid %d on its node "+
			"makes it the controller, and agents join it after that", int(c[0].VMID), int(c[0].VMID))
	}
	switch kind {
	case KindController:
		if c := by[KindController]; len(c) > 0 {
			return fmt.Errorf("the controller lxc/%d serves this cluster already: one controller serves an install", int(c[0].VMID))
		}
	case KindAgent:
		tok, err := pki.ParseJoinToken(r.o.JoinToken)
		if err != nil {
			return fmt.Errorf("the join token: %w", err)
		}
		switch {
		case tok.Node != r.node:
			return fmt.Errorf("the join token is for node %s, and this is node %s: make one for this node with "+
				"pco cluster join-token --node %s on the controller", tok.Node, r.node, r.node)
		case !r.now().Before(tok.Expires):
			return fmt.Errorf("the join token expired at %s: make a new one on the controller", tok.Expires.UTC().Format("15:04:05 MST"))
		}
		for _, k := range []string{KindController, KindAgent} {
			if i := slices.IndexFunc(by[k], func(g clusterGuest) bool { return g.Node == r.node }); i >= 0 {
				return fmt.Errorf("lxc/%d, the %s on this node, serves it already: one agent serves a node, and the controller "+
					"serves the node it runs on", int(by[k][i].VMID), k)
			}
		}
		if c := by[KindController]; len(c) > 0 {
			r.controller = c[0]
		} else {
			r.ask.Warn("no controller container is in pool %s: the agent is granted to pco@pve only, and its address goes into no ipset", poolID)
		}
		r.ask.Info("preflight: an agent of install %s for node %s; its token expires at %s", tok.InstallID, tok.Node,
			tok.Expires.UTC().Format("15:04:05 MST"))
	}
	return nil
}

// pushJoin pushes the join token onto the state volume, where the agent
// reads it at its start and removes it once the controller answered. It is
// written under /run, readable by root alone, and removed right after its
// push: the token is never in an argument, the environment or the journal.
func (r *run) pushJoin(ctx context.Context) error {
	if r.o.JoinToken == "" {
		return errors.New("a resumed run of an agent needs its join token again: give it with --join-file or --join -")
	}
	if err := r.makeTemp(); err != nil {
		return err
	}
	defer r.removeTemp()
	path := filepath.Join(r.tmp, "join.token")
	if err := writeNew(path, []byte(r.o.JoinToken+"\n")); err != nil {
		return err
	}
	_, err := r.r.Run(ctx, "pct", "push", strconv.Itoa(r.j.VMID), path, joinFile, "--perms", "0600")
	if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
		r.ask.Warn("removing %s: %v", path, rerr)
	}
	if err != nil {
		return fmt.Errorf("pushing the join token into lxc/%d: %w", r.j.VMID, err)
	}
	r.ask.Info("join token: pushed into lxc/%d, which joins its controller with it once", r.j.VMID)
	return nil
}

// grantAgent lets pco@pve and the controller's token put the legs of the
// networks the controller attaches on the agent's container: no token of
// pco may grant itself anything.
func (r *run) grantAgent(ctx context.Context) error {
	path := "/vms/" + strconv.Itoa(r.j.VMID)
	if err := r.managedRole(ctx); err != nil {
		return err
	}
	if _, err := r.r.Run(ctx, "pveum", "acl", "modify", path, "--users", setup.UserID, "--roles", setup.RoleManaged); err != nil {
		return fmt.Errorf("granting %s on %s to %s: %w", setup.RoleManaged, path, setup.UserID, err)
	}
	if r.controller.VMID == 0 {
		return nil
	}
	tok := tokenID(int(r.controller.VMID))
	if _, err := r.r.Run(ctx, "pveum", "acl", "modify", path, "--tokens", tok, "--roles", setup.RoleManaged); err != nil {
		return fmt.Errorf("granting %s on %s to %s: %w", setup.RoleManaged, path, tok, err)
	}
	r.ask.Info("acl %s: %s for %s and %s, for the legs the controller puts on the agent", path, setup.RoleManaged, setup.UserID, tok)
	return nil
}

// managedRole makes the role of a card's grant when it is missing; one of
// that name that grants more is not pco's and is refused.
func (r *run) managedRole(ctx context.Context) error {
	all, err := roles(ctx, r.r)
	if err != nil {
		return err
	}
	switch role, found := findRole(all, setup.RoleManaged); {
	case found && isGrantRole(setup.RoleManaged, role.Privs):
		return nil
	case found:
		return fmt.Errorf("role %s grants %s, not only %s: pco does not use it", setup.RoleManaged,
			strings.Join(role.Privs, ", "), grantRolePrivs[setup.RoleManaged])
	}
	if _, err := r.r.Run(ctx, "pveum", "role", "add", setup.RoleManaged, "--privs", grantRolePrivs[setup.RoleManaged]); err != nil {
		return fmt.Errorf("creating role %s: %w", setup.RoleManaged, err)
	}
	return nil
}

// enlist puts the agent's address into the ipset of the controller's
// firewall, which lets it reach the agent endpoint, and records it for
// uninstall.
func (r *run) enlist(ctx context.Context) error {
	c := r.controller
	if c.VMID == 0 {
		return nil
	}
	addr, err := r.net0Addr(ctx, r.j.VMID)
	if err != nil {
		return err
	}
	if err := r.record(func(*journal) {
		a := r.appliance()
		a.Enlisted, a.Controller, a.ControllerNode = addr.String(), int(c.VMID), c.Node
	}); err != nil {
		return err
	}
	set := fmt.Sprintf("/nodes/%s/lxc/%d/firewall/ipset/%s", c.Node, int(c.VMID), agentsIPSet)
	if _, err := r.r.Run(ctx, "pvesh", "create", set, "--cidr", addr.String(), "--comment", marker(r.j.VMID)); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("adding %s to ipset %s of the controller lxc/%d: %w", addr, agentsIPSet, int(c.VMID), err)
	}
	r.ask.Info("firewall: %s of lxc/%d is in ipset %s of the controller lxc/%d", addr, r.j.VMID, agentsIPSet, int(c.VMID))
	return nil
}

// endpointRule gives the controller's firewall the ipset of the agents and
// the one rule that lets them, and only them, reach the agent endpoint. The
// guest firewall of the container is left as the admin set it: the rule
// takes effect where it is on.
func (r *run) endpointRule(ctx context.Context) error {
	base := fmt.Sprintf("/nodes/%s/lxc/%d/firewall", r.node, r.j.VMID)
	if _, err := r.r.Run(ctx, "pvesh", "create", base+"/ipset", "--name", agentsIPSet, "--comment", agentsComment); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("making ipset %s of lxc/%d: %w", agentsIPSet, r.j.VMID, err)
	}
	var rules []firewallRule
	if err := pvesh(ctx, r.r, &rules, base+"/rules"); err != nil {
		return fmt.Errorf("reading the firewall rules of lxc/%d: %w", r.j.VMID, err)
	}
	if slices.ContainsFunc(rules, func(rule firewallRule) bool { return rule.Comment == agentsComment }) {
		return nil
	}
	if _, err := r.r.Run(ctx, "pvesh", "create", base+"/rules", "--type", "in", "--action", "ACCEPT", "--proto", "tcp",
		"--dport", strconv.Itoa(clusterapi.Port), "--source", "+"+agentsIPSet, "--enable", "1", "--comment", agentsComment); err != nil {
		return fmt.Errorf("adding the rule of the agent endpoint to lxc/%d: %w", r.j.VMID, err)
	}
	r.ask.Info("firewall: lxc/%d takes tcp/%d from ipset %s alone, which each agent's install fills", r.j.VMID, clusterapi.Port, agentsIPSet)
	return nil
}

// firewallRule is a rule of a guest's firewall, known by its comment.
type firewallRule struct {
	Comment string `json:"comment"`
}

// storageFacts records where the controller's state volume is, and lets its
// token read that storage again, so that doctor can say whether a relocation
// may bring back an older volume.
func (r *run) storageFacts(ctx context.Context) (*store.ApplianceStorage, error) {
	var cfg struct {
		Shared flexBool `json:"shared"`
	}
	if err := pvesh(ctx, r.r, &cfg, "/storage/"+r.o.Storage); err != nil {
		return nil, fmt.Errorf("reading storage %s: %w", r.o.Storage, err)
	}
	var jobs []struct {
		ID    string  `json:"id"`
		Guest flexInt `json:"guest"`
	}
	if err := pvesh(ctx, r.r, &jobs, "/cluster/replication"); err != nil {
		return nil, fmt.Errorf("reading the replication jobs: %w", err)
	}
	facts := &store.ApplianceStorage{ID: r.o.Storage, Shared: bool(cfg.Shared), HA: r.o.HA}
	for _, j := range jobs {
		if int(j.Guest) == r.j.VMID {
			facts.Replication = append(facts.Replication, j.ID)
		}
	}
	return facts, nil
}

// auditStorage gives the controller's token Datastore.Audit on its own
// storage, and nowhere else.
func (r *run) auditStorage(ctx context.Context) error {
	path, tok := "/storage/"+r.o.Storage, tokenID(r.j.VMID)
	if _, err := r.r.Run(ctx, "pveum", "acl", "modify", path, "--tokens", tok, "--roles", roleDatastoreAudit); err != nil {
		return fmt.Errorf("granting %s on %s to %s: %w", roleDatastoreAudit, path, tok, err)
	}
	r.ask.Info("acl %s: %s for %s, so that doctor reads where its volume is", path, roleDatastoreAudit, tok)
	return nil
}

// joinInstall is the install an agent's token names; empty for the other
// kinds.
func (o Options) joinInstall() string {
	if o.kindName() != KindAgent {
		return ""
	}
	tok, err := pki.ParseJoinToken(o.JoinToken)
	if err != nil {
		return ""
	}
	return tok.InstallID
}

// delist takes an agent's address out of the controller's ipset, where its
// install put it. The manifest comes from the container and may name
// anything: only an entry that carries this container's mark goes.
func (r *run) delist(ctx context.Context, vmid int, m *setup.Manifest) error {
	if m == nil || m.Appliance == nil || m.Appliance.Enlisted == "" || m.Appliance.Controller < 100 || !nodeName.MatchString(m.Appliance.ControllerNode) {
		return nil
	}
	a := m.Appliance
	addr, err := netip.ParseAddr(a.Enlisted)
	if err != nil {
		return nil
	}
	set := fmt.Sprintf("/nodes/%s/lxc/%d/firewall/ipset/%s", a.ControllerNode, a.Controller, agentsIPSet)
	var entries []struct {
		CIDR    string `json:"cidr"`
		Comment string `json:"comment"`
	}
	if err := pvesh(ctx, r.r, &entries, set); err != nil {
		return fmt.Errorf("reading ipset %s of the controller lxc/%d: %w; take %s out of it by hand", agentsIPSet, a.Controller, err, addr)
	}
	for _, e := range entries {
		if (e.CIDR == addr.String() || e.CIDR == addr.String()+"/32") && e.Comment == marker(vmid) {
			if _, err := r.r.Run(ctx, "pvesh", "delete", set+"/"+e.CIDR); err != nil {
				return fmt.Errorf("taking %s out of ipset %s of the controller lxc/%d: %w", addr, agentsIPSet, a.Controller, err)
			}
			r.ask.Info("firewall: %s is out of ipset %s of the controller lxc/%d", addr, agentsIPSet, a.Controller)
		}
	}
	return nil
}
