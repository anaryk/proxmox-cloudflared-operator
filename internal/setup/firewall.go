package setup

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

// The datacenter firewall rule that admits the agent channel of the
// controller from the nodes of the install, and the ipset it reads the nodes
// from. The rule is exactly this one and nothing else is touched: a wrong
// rule in the datacenter firewall can cut the cluster's traffic.
const (
	agentPort    = "8644"
	nodesIPSet   = "pco-nodes"
	ruleText     = "IN ACCEPT -p tcp -dport " + agentPort + " -source +" + nodesIPSet
	ruleComment  = "pco: the agent channel of the nodes, made by pco setup"
	ipsetComment = "pco: the nodes of the install, made by pco setup"
)

// entryComment marks an entry of the ipset as one pco added for a node.
func entryComment(node string) string { return "pco: node " + node }

// fwIPSet, fwEntry and fwRule are what pvesh prints of the datacenter
// firewall. Every entry of a listing carries the digest of the whole list,
// which a delete may insist on.
type fwIPSet struct {
	Name    string `json:"name"`
	Comment string `json:"comment"`
}

type fwEntry struct {
	CIDR    string `json:"cidr"`
	Comment string `json:"comment"`
}

type fwRule struct {
	Pos     int     `json:"pos"`
	Type    string  `json:"type"`
	Action  string  `json:"action"`
	Proto   string  `json:"proto"`
	DPort   string  `json:"dport"`
	Source  string  `json:"source"`
	Dest    string  `json:"dest"`
	SPort   string  `json:"sport"`
	Iface   string  `json:"iface"`
	Macro   string  `json:"macro"`
	Comment string  `json:"comment"`
	Enable  pveBool `json:"enable"`
	Digest  string  `json:"digest"`
}

// agentChannel reports whether the rule is the one of the agent channel,
// exactly, with nothing more in it.
func (f fwRule) agentChannel() bool {
	return f.Type == "in" && f.Action == "ACCEPT" && f.Proto == "tcp" && f.DPort == agentPort && f.Source == "+"+nodesIPSet &&
		f.Dest == "" && f.SPort == "" && f.Iface == "" && f.Macro == ""
}

// fwState is what the datacenter firewall holds of pco, read in one go.
type fwState struct {
	ipset   *fwIPSet // nil without the ipset
	entries []fwEntry
	rules   []fwRule
}

// rule is the rule of the agent channel, or nil.
func (s fwState) rule() *fwRule {
	if i := slices.IndexFunc(s.rules, fwRule.agentChannel); i >= 0 {
		return &s.rules[i]
	}
	return nil
}

func (s fwState) hasEntry(cidr string) bool {
	return slices.ContainsFunc(s.entries, func(e fwEntry) bool { return e.CIDR == cidr })
}

// readFirewall reads the ipset, its entries and the rules.
func readFirewall(ctx context.Context, r Runner) (fwState, error) {
	var s fwState
	var sets []fwIPSet
	if err := query(ctx, r, &sets, "pvesh", "get", "/cluster/firewall/ipset", "--output-format", "json"); err != nil {
		return s, fmt.Errorf("listing the ipsets of the datacenter firewall: %w", err)
	}
	if i := slices.IndexFunc(sets, func(s fwIPSet) bool { return s.Name == nodesIPSet }); i >= 0 {
		s.ipset = &sets[i]
		if err := query(ctx, r, &s.entries, "pvesh", "get", "/cluster/firewall/ipset/"+nodesIPSet, "--output-format", "json"); err != nil {
			return s, fmt.Errorf("listing the ipset %s: %w", nodesIPSet, err)
		}
	}
	if err := query(ctx, r, &s.rules, "pvesh", "get", "/cluster/firewall/rules", "--output-format", "json"); err != nil {
		return s, fmt.Errorf("listing the rules of the datacenter firewall: %w", err)
	}
	return s, nil
}

// ensureFirewall offers the datacenter rule that admits the agent channel
// from the nodes, and makes it: the ipset with every registered node's agent
// address, then the rule. --repair only keeps the rule current on a node that
// took it before. It is offered because it is needed only when the firewall
// of Proxmox VE is on, and never forced, since it changes a firewall the
// admin runs.
func (r *run) ensureFirewall(ctx context.Context) error {
	if r.o.Repair && !r.manifest.FirewallRule {
		return nil
	}
	want := true
	if !r.o.Repair {
		var err error
		want, err = r.choose(r.o.FirewallRule, true, "Add the datacenter firewall rule that admits the agent channel of pco, "+
			"TCP port "+agentPort+", from the nodes of the install (ipset "+nodesIPSet+")?")
		if err != nil {
			return err
		}
	}
	if !want {
		r.ask.Info("firewall: no rule for the agent channel; when the Proxmox VE firewall is on, the nodes need TCP port %s "+
			"to the controller, and pco setup adds the rule when run again", agentPort)
		return nil
	}
	fw, err := r.ensureIPSet(ctx)
	if err != nil {
		return err
	}
	return r.ensureRule(ctx, fw)
}

// ensureIPSet makes the ipset when it is missing and puts the agent address
// of every registered node that lacks one into it, this node's last.
func (r *run) ensureIPSet(ctx context.Context) (fwState, error) {
	var sets []fwIPSet
	if err := query(ctx, r.run, &sets, "pvesh", "get", "/cluster/firewall/ipset", "--output-format", "json"); err != nil {
		return fwState{}, fmt.Errorf("listing the ipsets of the datacenter firewall: %w", err)
	}
	if !slices.ContainsFunc(sets, func(s fwIPSet) bool { return s.Name == nodesIPSet }) {
		if err := r.record(func(m *Manifest) { m.CreatedIPSet = true }); err != nil {
			return fwState{}, err
		}
		if _, err := r.run.Run(ctx, "pvesh", "create", "/cluster/firewall/ipset", "--name", nodesIPSet, "--comment", ipsetComment); err != nil {
			r.takeBack(func() (bool, error) {
				fw, err := readFirewall(ctx, r.run)
				return fw.ipset != nil, err
			}, func(m *Manifest) { m.CreatedIPSet = false })
			return fwState{}, fmt.Errorf("making the ipset %s: %w", nodesIPSet, err)
		}
		r.ask.Info("firewall: made the ipset %s", nodesIPSet)
	}
	var fw fwState
	if err := query(ctx, r.run, &fw.entries, "pvesh", "get", "/cluster/firewall/ipset/"+nodesIPSet, "--output-format", "json"); err != nil {
		return fwState{}, fmt.Errorf("listing the ipset %s: %w", nodesIPSet, err)
	}
	nodes, err := r.st.Nodes()
	if err != nil {
		return fwState{}, fmt.Errorf("reading the node registry: %w", err)
	}
	for _, n := range nodes {
		if n.Name != r.o.Node && n.State != string(agentproto.Removed) && n.AgentAddress.IsValid() {
			if err := r.addEntry(ctx, &fw, n.Name, n.AgentAddress); err != nil {
				return fwState{}, err
			}
		}
	}
	if err := r.addEntry(ctx, &fw, r.o.Node, r.agentAddress); err != nil {
		return fwState{}, err
	}
	if err := r.record(func(m *Manifest) { m.FirewallRule = true }); err != nil {
		return fwState{}, err
	}
	r.ask.Info("firewall: ipset %s holds %s (%s)", nodesIPSet, r.agentAddress, r.o.Node)
	return fw, nil
}

func (r *run) addEntry(ctx context.Context, fw *fwState, node string, addr netip.Addr) error {
	cidr := addr.String()
	if fw.hasEntry(cidr) {
		return nil
	}
	if _, err := r.run.Run(ctx, "pvesh", "create", "/cluster/firewall/ipset/"+nodesIPSet, "--cidr", cidr, "--comment", entryComment(node)); err != nil {
		return fmt.Errorf("adding %s (%s) to the ipset %s: %w", cidr, node, nodesIPSet, err)
	}
	fw.entries = append(fw.entries, fwEntry{CIDR: cidr, Comment: entryComment(node)})
	return nil
}

// ensureRule adds the rule of the agent channel when no rule is it; one that
// is there, enabled or not, is left as the admin has it.
func (r *run) ensureRule(ctx context.Context, fw fwState) error {
	if err := query(ctx, r.run, &fw.rules, "pvesh", "get", "/cluster/firewall/rules", "--output-format", "json"); err != nil {
		return fmt.Errorf("listing the rules of the datacenter firewall: %w", err)
	}
	rule := fw.rule()
	switch {
	case rule == nil:
	case !bool(rule.Enable):
		r.ask.Warn("firewall: the datacenter rule %s is there but disabled; enable it when the Proxmox VE firewall is on, "+
			"or the nodes cannot reach the controller", ruleText)
		return nil
	default:
		r.ask.Info("firewall: the datacenter rule %s is there, nothing needed", ruleText)
		return nil
	}
	if err := r.record(func(m *Manifest) { m.CreatedFirewallRule = true }); err != nil {
		return err
	}
	if _, err := r.run.Run(ctx, "pvesh", "create", "/cluster/firewall/rules", "--type", "in", "--action", "ACCEPT",
		"--proto", "tcp", "--dport", agentPort, "--source", "+"+nodesIPSet, "--enable", "1", "--comment", ruleComment); err != nil {
		r.takeBack(func() (bool, error) {
			var rules []fwRule
			err := query(ctx, r.run, &rules, "pvesh", "get", "/cluster/firewall/rules", "--output-format", "json")
			return slices.ContainsFunc(rules, fwRule.agentChannel), err
		}, func(m *Manifest) { m.CreatedFirewallRule = false })
		return fmt.Errorf("adding the datacenter rule %s: %w", ruleText, err)
	}
	r.ask.Info("firewall: added the datacenter rule %s", ruleText)
	return nil
}

// inFirewall reports whether setup put anything into the datacenter firewall.
func (m Manifest) inFirewall() bool { return m.FirewallRule || m.CreatedIPSet || m.CreatedFirewallRule }

// firewallVerdict says what of the firewall goes with the last node: the rule
// and the ipset, when the manifest says pco made them or they say so
// themselves, since the node that made them may have left before.
func (u *uninstall) firewallVerdict() (ruleGoes, ipsetGoes bool) {
	m, fw := u.manifest, u.found.firewall
	rule := fw.rule()
	ruleGoes = rule != nil && (m.CreatedFirewallRule || rule.Comment == ruleComment)
	ipsetGoes = fw.ipset != nil && (m.CreatedIPSet || fw.ipset.Comment == ipsetComment)
	return ruleGoes, ipsetGoes
}

// ownEntry is the address pco put into the ipset for this node, "" when
// there is none.
func (u *uninstall) ownEntry() string {
	for _, e := range u.found.firewall.entries {
		if e.Comment == entryComment(u.node) {
			return e.CIDR
		}
	}
	return ""
}

// describeFirewall lists what of the datacenter firewall goes.
func (u *uninstall) describeFirewall() {
	if !u.manifest.inFirewall() {
		return
	}
	if err := u.found.firewallErr; err != nil {
		u.ask.Info("  the firewall rule of the agent channel and the ipset %s, which cannot be read: %v", nodesIPSet, err)
		return
	}
	if !u.last {
		if cidr := u.ownEntry(); cidr != "" {
			u.ask.Info("  this node's address %s in the ipset %s", cidr, nodesIPSet)
		}
		return
	}
	ruleGoes, ipsetGoes := u.firewallVerdict()
	fw := u.found.firewall
	switch {
	case ruleGoes && ipsetGoes:
		u.ask.Info("  the firewall rule of the agent channel, %s, and the ipset %s", ruleText, nodesIPSet)
	case ruleGoes:
		u.ask.Info("  the firewall rule of the agent channel, %s", ruleText)
	case ipsetGoes:
		u.ask.Info("  the ipset %s of the agent channel", nodesIPSet)
	}
	if fw.rule() != nil && !ruleGoes {
		u.ask.Info("  (the firewall rule of the agent channel is kept: pco did not make it)")
	}
	if fw.ipset != nil && !ipsetGoes {
		if cidr := u.ownEntry(); cidr != "" {
			u.ask.Info("  this node's address %s in the ipset %s, which is kept: pco did not make it", cidr, nodesIPSet)
		}
	}
}

// removeFirewall takes this node's address out of the ipset and, with the
// last node, the rule and the ipset when they are pco's. Entries pco did not
// add keep the ipset.
func (u *uninstall) removeFirewall(ctx context.Context) {
	if !u.manifest.inFirewall() {
		return
	}
	if err := u.found.firewallErr; err != nil {
		u.fail("the firewall rule of the agent channel and the ipset %s are kept: %v", nodesIPSet, err)
		return
	}
	fw := u.found.firewall
	ruleGoes, ipsetGoes := u.firewallVerdict()
	if !u.last {
		u.removeOwnEntry(ctx)
		return
	}
	if ruleGoes {
		rule := fw.rule()
		if _, err := u.run.Run(ctx, "pvesh", "delete", "/cluster/firewall/rules/"+strconv.Itoa(rule.Pos), "--digest", rule.Digest); err != nil {
			u.fail("removing the firewall rule of the agent channel: %v", err)
			return
		}
		u.ask.Info("firewall: removed the datacenter rule %s", ruleText)
	}
	if !ipsetGoes {
		u.removeOwnEntry(ctx)
		return
	}
	var others []string
	for _, e := range fw.entries {
		if !strings.HasPrefix(e.Comment, entryComment("")) {
			others = append(others, e.CIDR)
			continue
		}
		if _, err := u.run.Run(ctx, "pvesh", "delete", "/cluster/firewall/ipset/"+nodesIPSet+"/"+e.CIDR); err != nil {
			u.fail("removing %s from the ipset %s: %v", e.CIDR, nodesIPSet, err)
			return
		}
	}
	if len(others) > 0 {
		u.ask.Info("the ipset %s is kept: it holds %s, which pco did not add", nodesIPSet, strings.Join(others, ", "))
		return
	}
	if _, err := u.run.Run(ctx, "pvesh", "delete", "/cluster/firewall/ipset/"+nodesIPSet); err != nil {
		u.fail("removing the ipset %s: %v", nodesIPSet, err)
		return
	}
	u.ask.Info("firewall: removed the ipset %s", nodesIPSet)
}

func (u *uninstall) removeOwnEntry(ctx context.Context) {
	cidr := u.ownEntry()
	if cidr == "" {
		return
	}
	if _, err := u.run.Run(ctx, "pvesh", "delete", "/cluster/firewall/ipset/"+nodesIPSet+"/"+cidr); err != nil {
		u.fail("removing %s from the ipset %s: %v", cidr, nodesIPSet, err)
		return
	}
	u.ask.Info("firewall: removed %s from the ipset %s", cidr, nodesIPSet)
}
