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

	// fwTries bounds the loops that delete one object at a time and list
	// again for the next one and its digest.
	fwTries = 32
)

// entryComment marks an entry of the ipset as one pco added for a node.
func entryComment(node string) string { return "pco: node " + node }

// fwIPSet, fwEntry, fwGroup and fwRule are what pvesh prints of the
// datacenter firewall. Every item of a listing carries the digest of the
// configuration, which a delete passes on, so that it is refused when
// another writer changed the configuration since it was listed.
type fwIPSet struct {
	Name    string `json:"name"`
	Comment string `json:"comment"`
}

type fwEntry struct {
	CIDR    string `json:"cidr"`
	Comment string `json:"comment"`
	Digest  string `json:"digest"`
}

type fwGroup struct {
	Group string `json:"group"`
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

// pcos reports whether the rule is the agent channel's and says pco made it.
func (f fwRule) pcos() bool { return f.agentChannel() && f.Comment == ruleComment }

// refersToNodes reports whether the rule names the ipset as a source or a
// destination, in either form Proxmox VE takes.
func (f fwRule) refersToNodes() bool {
	for _, field := range []string{f.Source, f.Dest} {
		for ref := range strings.SplitSeq(field, ",") {
			if ref = strings.TrimSpace(ref); ref == "+"+nodesIPSet || ref == "+dc/"+nodesIPSet {
				return true
			}
		}
	}
	return false
}

// pcoRules are the rules pco made, topmost first.
func pcoRules(rules []fwRule) []fwRule {
	var out []fwRule
	for _, r := range rules {
		if r.pcos() {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b fwRule) int { return a.Pos - b.Pos })
	return out
}

// fwState is what the datacenter firewall holds of pco, read in one go.
type fwState struct {
	ipset   *fwIPSet // nil without the ipset
	entries []fwEntry
	rules   []fwRule
}

// rule is the first rule of the agent channel, whoever made it, or nil.
func (s fwState) rule() *fwRule {
	if i := slices.IndexFunc(s.rules, fwRule.agentChannel); i >= 0 {
		return &s.rules[i]
	}
	return nil
}

func (s fwState) hasEntry(cidr string) bool {
	return slices.ContainsFunc(s.entries, func(e fwEntry) bool { return e.CIDR == cidr })
}

// ownEntries are the entries pco added for node.
func (s fwState) ownEntries(node string) []string {
	var out []string
	for _, e := range s.entries {
		if e.Comment == entryComment(node) {
			out = append(out, e.CIDR)
		}
	}
	return out
}

func listIPSets(ctx context.Context, r Runner) ([]fwIPSet, error) {
	var sets []fwIPSet
	if err := query(ctx, r, &sets, "pvesh", "get", "/cluster/firewall/ipset", "--output-format", "json"); err != nil {
		return nil, fmt.Errorf("listing the ipsets of the datacenter firewall: %w", err)
	}
	return sets, nil
}

func hasIPSet(sets []fwIPSet) bool {
	return slices.ContainsFunc(sets, func(s fwIPSet) bool { return s.Name == nodesIPSet })
}

func listEntries(ctx context.Context, r Runner) ([]fwEntry, error) {
	var entries []fwEntry
	if err := query(ctx, r, &entries, "pvesh", "get", "/cluster/firewall/ipset/"+nodesIPSet, "--output-format", "json"); err != nil {
		return nil, fmt.Errorf("listing the ipset %s: %w", nodesIPSet, err)
	}
	return entries, nil
}

func listRules(ctx context.Context, r Runner) ([]fwRule, error) {
	var rules []fwRule
	if err := query(ctx, r, &rules, "pvesh", "get", "/cluster/firewall/rules", "--output-format", "json"); err != nil {
		return nil, fmt.Errorf("listing the rules of the datacenter firewall: %w", err)
	}
	return rules, nil
}

// readFirewall reads the ipset, its entries and the rules.
func readFirewall(ctx context.Context, r Runner) (fwState, error) {
	var s fwState
	sets, err := listIPSets(ctx, r)
	if err != nil {
		return s, err
	}
	if i := slices.IndexFunc(sets, func(s fwIPSet) bool { return s.Name == nodesIPSet }); i >= 0 {
		s.ipset = &sets[i]
		if s.entries, err = listEntries(ctx, r); err != nil {
			return s, err
		}
	}
	s.rules, err = listRules(ctx, r)
	return s, err
}

// deleteEntries removes the entries of the ipset that match, one at a time,
// each with the digest of the listing it was found in.
func deleteEntries(ctx context.Context, r Runner, match func(fwEntry) bool) ([]fwEntry, error) {
	for range fwTries {
		entries, err := listEntries(ctx, r)
		if err != nil {
			return nil, err
		}
		i := slices.IndexFunc(entries, match)
		if i < 0 {
			return entries, nil
		}
		e := entries[i]
		if _, err := r.Run(ctx, "pvesh", "delete", "/cluster/firewall/ipset/"+nodesIPSet+"/"+e.CIDR, "--digest", e.Digest); err != nil {
			return nil, fmt.Errorf("removing %s from the ipset %s: %w", e.CIDR, nodesIPSet, err)
		}
	}
	return nil, fmt.Errorf("the ipset %s kept changing while entries were removed", nodesIPSet)
}

// deleteRule removes a rule of the datacenter firewall by its position, with
// the digest of the listing it was found in.
func deleteRule(ctx context.Context, r Runner, rule fwRule) error {
	_, err := r.Run(ctx, "pvesh", "delete", "/cluster/firewall/rules/"+strconv.Itoa(rule.Pos), "--digest", rule.Digest)
	return err
}

// ensureFirewall offers the datacenter rule that admits the agent channel
// from the nodes, and makes it: the ipset with every registered node's agent
// address, then the rule. --repair only keeps the rule current on a node that
// took it before. It is offered because it is needed only when the firewall
// of Proxmox VE is on, and never forced, since it changes a firewall the
// admin runs. A node alone needs no agent channel, so the rule is offered
// only with the join of further nodes.
func (r *run) ensureFirewall(ctx context.Context) error {
	if !r.o.ClusterPreview || r.o.Repair && !r.manifest.FirewallRule {
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
	if err := r.ensureIPSet(ctx); err != nil {
		return err
	}
	return r.ensureRule(ctx)
}

// ensureIPSet makes the ipset when it is missing, takes out an address it
// held for this node that is no longer the node's, and puts the agent
// address of every registered node that lacks one into it, this node's
// last.
func (r *run) ensureIPSet(ctx context.Context) error {
	if err := r.ensureIPSetThere(ctx); err != nil {
		return err
	}
	own := entryComment(r.o.Node)
	addr := r.agentAddress.String()
	entries, err := deleteEntries(ctx, r.run, func(e fwEntry) bool { return e.Comment == own && e.CIDR != addr })
	if err != nil {
		return err
	}
	fw := fwState{entries: entries}
	nodes, err := r.st.Nodes()
	if err != nil {
		return fmt.Errorf("reading the node registry: %w", err)
	}
	for _, n := range nodes {
		if n.Name != r.o.Node && n.State != string(agentproto.Removed) && n.AgentAddress.IsValid() {
			if err := r.addEntry(ctx, &fw, n.Name, n.AgentAddress); err != nil {
				return err
			}
		}
	}
	if err := r.addEntry(ctx, &fw, r.o.Node, r.agentAddress); err != nil {
		return err
	}
	if err := r.record(func(m *Manifest) { m.FirewallRule = true }); err != nil {
		return err
	}
	r.ask.Info("firewall: ipset %s holds %s (%s)", nodesIPSet, r.agentAddress, r.o.Node)
	return nil
}

// ensureIPSetThere makes the ipset when it is missing. One that another
// node's setup made in the meantime is taken as it is, and is not this
// node's.
func (r *run) ensureIPSetThere(ctx context.Context) error {
	sets, err := listIPSets(ctx, r.run)
	switch {
	case err != nil:
		return err
	case hasIPSet(sets):
		return nil
	}
	if err := r.record(func(m *Manifest) { m.CreatedIPSet = true }); err != nil {
		return err
	}
	_, err = r.run.Run(ctx, "pvesh", "create", "/cluster/firewall/ipset", "--name", nodesIPSet, "--comment", ipsetComment)
	if err == nil {
		r.ask.Info("firewall: made the ipset %s", nodesIPSet)
		return nil
	}
	sets, lerr := listIPSets(ctx, r.run)
	if lerr != nil {
		return fmt.Errorf("making the ipset %s: %w", nodesIPSet, err)
	}
	if rerr := r.record(func(m *Manifest) { m.CreatedIPSet = false }); rerr != nil {
		return rerr
	}
	if !hasIPSet(sets) {
		return fmt.Errorf("making the ipset %s: %w", nodesIPSet, err)
	}
	r.ask.Info("firewall: the ipset %s was made meanwhile, by a setup on another node", nodesIPSet)
	return nil
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
// is there, enabled or not, is left as the admin has it. After adding it, it
// lists the rules again and removes the copies a setup on another node added
// at the same time, keeping the topmost.
func (r *run) ensureRule(ctx context.Context) error {
	rules, err := listRules(ctx, r.run)
	if err != nil {
		return err
	}
	fw := fwState{rules: rules}
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
			rules, err := listRules(ctx, r.run)
			return slices.ContainsFunc(rules, fwRule.agentChannel), err
		}, func(m *Manifest) { m.CreatedFirewallRule = false })
		return fmt.Errorf("adding the datacenter rule %s: %w", ruleText, err)
	}
	r.ask.Info("firewall: added the datacenter rule %s", ruleText)
	return r.dropCopies(ctx)
}

// dropCopies removes every rule pco made but the topmost, the lowest one
// first.
func (r *run) dropCopies(ctx context.Context) error {
	for range fwTries {
		rules, err := listRules(ctx, r.run)
		if err != nil {
			return err
		}
		ours := pcoRules(rules)
		if len(ours) <= 1 {
			return nil
		}
		if err := deleteRule(ctx, r.run, ours[len(ours)-1]); err != nil {
			return fmt.Errorf("removing a second copy of the datacenter rule %s: %w", ruleText, err)
		}
		r.ask.Info("firewall: removed a second copy of the datacenter rule %s, which a setup on another node added at the same time", ruleText)
	}
	return fmt.Errorf("the rules of the datacenter firewall kept changing while copies of %s were removed", ruleText)
}

// inFirewall reports whether setup put anything into the datacenter firewall.
func (m Manifest) inFirewall() bool { return m.FirewallRule || m.CreatedIPSet || m.CreatedFirewallRule }

// looksAtFirewall reports whether uninstall reads the datacenter firewall:
// when this node's setup put something there, or when the registry has
// another entry, of a node that may have, removed ones too.
func (u *uninstall) looksAtFirewall() bool { return u.manifest.inFirewall() || u.entries > 1 }

// ipsetIsPcos reports whether the ipset is pco's: this node's setup made it,
// or it says so itself, since the node that made it may have left before.
func (u *uninstall) ipsetIsPcos() bool {
	set := u.found.firewall.ipset
	return set != nil && (u.manifest.CreatedIPSet || set.Comment == ipsetComment)
}

// describeFirewall lists what of the datacenter firewall goes.
func (u *uninstall) describeFirewall() {
	f := u.found
	if !f.firewallRead {
		return
	}
	if f.firewallErr != nil {
		if u.manifest.inFirewall() {
			u.ask.Info("  the firewall rule of the agent channel and the ipset %s, which cannot be read: %v", nodesIPSet, f.firewallErr)
		}
		return
	}
	own := f.firewall.ownEntries(u.node)
	if !u.last {
		if len(own) > 0 {
			u.ask.Info("  this node's address %s in the ipset %s", strings.Join(own, ", "), nodesIPSet)
		}
		return
	}
	if n := len(pcoRules(f.firewall.rules)); n > 0 {
		u.ask.Info("  the firewall rule of the agent channel, %s", ruleText)
	}
	if rule := f.firewall.rule(); rule != nil && !rule.pcos() {
		u.ask.Info("  (the firewall rule of the agent channel is kept: pco did not make it)")
	}
	switch {
	case u.ipsetIsPcos():
		u.ask.Info("  the ipset %s, unless a rule pco did not make refers to it", nodesIPSet)
	case len(own) > 0:
		u.ask.Info("  this node's address %s in the ipset %s, which is kept: pco did not make it", strings.Join(own, ", "), nodesIPSet)
	}
}

// removeFirewall takes this node's addresses out of the ipset and, with the
// last node, every rule pco made and the ipset, unless another rule refers
// to it.
func (u *uninstall) removeFirewall(ctx context.Context) {
	f := u.found
	switch {
	case !f.firewallRead:
		return
	case f.firewallErr != nil && u.manifest.inFirewall():
		u.fail("the firewall rule of the agent channel and the ipset %s are kept: %v", nodesIPSet, f.firewallErr)
		return
	case f.firewallErr != nil:
		u.ask.Warn("the datacenter firewall could not be read (%v); what other nodes put there for pco is left as it is", f.firewallErr)
		return
	}
	if !u.last {
		u.removeOwnEntries(ctx)
		return
	}
	fw := f.firewall
	if len(pcoRules(fw.rules)) == 0 && !u.ipsetIsPcos() && len(fw.ownEntries(u.node)) == 0 {
		return
	}
	rules, ok := u.removeRules(ctx)
	if !ok {
		return
	}
	if !u.ipsetIsPcos() {
		u.removeOwnEntries(ctx)
		return
	}
	refs, err := u.referencesToNodes(ctx, rules)
	switch {
	case err != nil:
		u.fail("the ipset %s is kept: %v", nodesIPSet, err)
		return
	case len(refs) > 0:
		u.ask.Info("firewall: the ipset %s is kept with its entries: %s refers to it, and emptying it would change what that rule admits",
			nodesIPSet, strings.Join(refs, " and "))
		return
	}
	u.removeIPSet(ctx)
}

// removeRules removes every rule pco made, the lowest first, each with the
// digest of the listing it was found in, and returns the rules left.
func (u *uninstall) removeRules(ctx context.Context) ([]fwRule, bool) {
	for range fwTries {
		rules, err := listRules(ctx, u.run)
		if err != nil {
			u.fail("%v", err)
			return nil, false
		}
		ours := pcoRules(rules)
		if len(ours) == 0 {
			return rules, true
		}
		if err := deleteRule(ctx, u.run, ours[len(ours)-1]); err != nil {
			u.fail("removing the firewall rule of the agent channel: %v", err)
			return nil, false
		}
		u.ask.Info("firewall: removed the datacenter rule %s", ruleText)
	}
	u.fail("the rules of the datacenter firewall kept changing while pco's were removed")
	return nil, false
}

// referencesToNodes names the rules of the datacenter and of its security
// groups that refer to the ipset; rules are those of the datacenter, read
// after pco's went.
func (u *uninstall) referencesToNodes(ctx context.Context, rules []fwRule) ([]string, error) {
	var refs []string
	for _, r := range rules {
		if r.refersToNodes() {
			refs = append(refs, fmt.Sprintf("the datacenter rule at position %d", r.Pos))
		}
	}
	var groups []fwGroup
	if err := query(ctx, u.run, &groups, "pvesh", "get", "/cluster/firewall/groups", "--output-format", "json"); err != nil {
		return nil, fmt.Errorf("listing the security groups: %w", err)
	}
	for _, g := range groups {
		var rules []fwRule
		if err := query(ctx, u.run, &rules, "pvesh", "get", "/cluster/firewall/groups/"+g.Group, "--output-format", "json"); err != nil {
			return nil, fmt.Errorf("listing the rules of the security group %s: %w", g.Group, err)
		}
		for _, r := range rules {
			if r.refersToNodes() {
				refs = append(refs, fmt.Sprintf("the rule at position %d of the security group %s", r.Pos, g.Group))
			}
		}
	}
	return refs, nil
}

// removeIPSet removes pco's entries and then the ipset, which entries pco
// did not add keep.
func (u *uninstall) removeIPSet(ctx context.Context) {
	left, err := deleteEntries(ctx, u.run, func(e fwEntry) bool { return strings.HasPrefix(e.Comment, entryComment("")) })
	if err != nil {
		u.fail("%v", err)
		return
	}
	if len(left) > 0 {
		var others []string
		for _, e := range left {
			others = append(others, e.CIDR)
		}
		u.ask.Info("the ipset %s is kept: it holds %s, which pco did not add", nodesIPSet, strings.Join(others, ", "))
		return
	}
	if _, err := u.run.Run(ctx, "pvesh", "delete", "/cluster/firewall/ipset/"+nodesIPSet); err != nil {
		u.fail("removing the ipset %s: %v", nodesIPSet, err)
		return
	}
	u.ask.Info("firewall: removed the ipset %s", nodesIPSet)
}

// removeOwnEntries takes every address pco added for this node out of the
// ipset.
func (u *uninstall) removeOwnEntries(ctx context.Context) {
	own := u.found.firewall.ownEntries(u.node)
	if len(own) == 0 {
		return
	}
	if _, err := deleteEntries(ctx, u.run, func(e fwEntry) bool { return e.Comment == entryComment(u.node) }); err != nil {
		u.fail("%v", err)
		return
	}
	u.ask.Info("firewall: removed %s from the ipset %s", strings.Join(own, ", "), nodesIPSet)
}
