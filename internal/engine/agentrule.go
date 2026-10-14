package engine

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// AgentChannelIPSet is the ipset of the datacenter firewall that the rule of
// the agent channel admits from, as pco setup makes them.
const AgentChannelIPSet = "pco-nodes"

const (
	agentChannelPort = "8644"
	agentRuleText    = "IN ACCEPT -p tcp -dport " + agentChannelPort + " -source +" + AgentChannelIPSet

	// problemAgentRule names the nodes whose agents send no status, and
	// what the datacenter firewall lacks of their channel.
	problemAgentRule = "the %s %s no status, and %s: %s"

	// agentRuleEvery is how often at most the firewall is read while an
	// agent is silent.
	agentRuleEvery = time.Minute
)

// AgentFirewall is what the firewall of Proxmox VE holds that the agent
// channel goes through: whether the datacenter firewall is on, its rules in
// their order, and the entries of the ipset pco-nodes, HasNodes false when
// there is no such ipset.
type AgentFirewall struct {
	On       bool
	Rules    []pve.FirewallRule
	HasNodes bool
	Nodes    []pve.IPSetEntry
}

// AgentRuleFacts is what the controller found of the firewall for the agents
// that send it no status: none while every agent reports. Gap is what the
// datacenter firewall lacks of their channel, empty when it lacks nothing or
// was not read, and Fix how to put it right; Err is why it could not be
// read.
type AgentRuleFacts struct {
	Silent []string
	Gap    string
	Fix    string
	Err    string
}

// agentRuleMemory is the last read of the firewall: when, for which silent
// agents, and what it found.
type agentRuleMemory struct {
	at    time.Time
	key   string
	facts AgentRuleFacts
}

// agentRule reads the firewall while an agent of a node the members file
// lists online sends no status, and names the gap of the agent channel it
// finds: a rule deleted or disabled cuts the agents off with nothing else to
// say so, as the node is then only unknown.
func (c *cycleRun) agentRule(reports map[string]NodeReport, listed map[string]members.Member) {
	read := c.e.d.AgentFirewall
	silent := c.silentAgents(reports, listed)
	if read == nil || len(silent) == 0 {
		c.e.agentRule = agentRuleMemory{}
		return
	}
	names := make([]string, len(silent))
	for i, n := range silent {
		names[i] = n.Name
	}
	key := strings.Join(names, ",")
	mem := &c.e.agentRule
	if mem.key != key || mem.at.IsZero() || c.now.Sub(mem.at) >= agentRuleEvery || c.now.Before(mem.at) {
		facts := AgentRuleFacts{Silent: names}
		fw, err := read(c.ctx)
		if err != nil {
			facts.Err = err.Error()
			c.e.d.Log.Debug().Err(err).Msg("reading the datacenter firewall for the silent agents failed")
		} else {
			facts.Gap, facts.Fix = agentRuleGap(fw, silent)
		}
		*mem = agentRuleMemory{at: c.now, key: key, facts: facts}
	}
	if gap := mem.facts.Gap; gap != "" {
		c.problem(problemAgentRule, oneOrMany(len(names), "agent of", "agents of")+" "+strings.Join(names, ", "),
			oneOrMany(len(names), "sends", "send"), gap, mem.facts.Fix)
	}
}

// oneOrMany is the word for one or for more, without the number.
func oneOrMany(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// silentAgents are the nodes the members file lists online whose agent has
// sent no status for silentAfter, or none since this controller began once
// the takeover hold is over, by name.
func (c *cycleRun) silentAgents(reports map[string]NodeReport, listed map[string]members.Member) []store.NodeEntry {
	held := c.now.Sub(c.since()) >= takeoverHold
	var out []store.NodeEntry
	for _, name := range slices.Sorted(maps.Keys(c.nodes)) {
		n := c.nodes[name].Node
		if name == c.e.d.Node || n.State == store.NodeRemoved || n.State == store.NodeDown {
			continue
		}
		if m, ok := listed[name]; !ok || !m.Online {
			continue
		}
		rep, heard := reports[name]
		if heard && c.now.Sub(rep.ReceivedAt) <= silentAfter || !heard && !held {
			continue
		}
		out = append(out, n)
	}
	return out
}

// agentRuleGap says what the datacenter firewall lacks of the channel of the
// silent agents, and how to put it right; empty when the firewall is off or
// lacks nothing that can be told. The rule is the one pco setup makes,
// exactly, under either spelling of its ipset.
func agentRuleGap(fw AgentFirewall, silent []store.NodeEntry) (gap, fix string) {
	if !fw.On {
		return "", ""
	}
	disabled := -1
	found := false
	for _, r := range fw.Rules {
		if !agentChannelRule(r) {
			continue
		}
		if r.Enable {
			found = true
			break
		}
		if disabled < 0 {
			disabled = r.Pos
		}
	}
	switch {
	case !found && disabled >= 0:
		return fmt.Sprintf("the rule %s of the datacenter firewall, at position %d, is disabled", agentRuleText, disabled),
			fmt.Sprintf("pvesh set /cluster/firewall/rules/%d --enable 1", disabled)
	case !found:
		return "the datacenter firewall is on without the rule " + agentRuleText,
			"pco setup --repair puts it back on a node that took it, or pvesh create /cluster/firewall/rules --type in " +
				"--action ACCEPT --proto tcp --dport " + agentChannelPort + " --source +" + AgentChannelIPSet + " --enable 1"
	case !fw.HasNodes:
		return "the datacenter firewall is on without the ipset " + AgentChannelIPSet + " its rule for TCP port " +
				agentChannelPort + " admits from",
			"pco setup --repair makes it on a node that took the rule"
	}
	var missing, fixes []string
	for _, n := range silent {
		a := n.AgentAddress.Unmap()
		if !a.IsValid() {
			continue
		}
		if held, known := ipsetHolds(fw.Nodes, a); known && !held {
			missing = append(missing, fmt.Sprintf("%s (%s)", a, n.Name))
			fixes = append(fixes, "pvesh create /cluster/firewall/ipset/"+AgentChannelIPSet+" --cidr "+a.String())
		}
	}
	if len(missing) == 0 {
		return "", ""
	}
	return "the ipset " + AgentChannelIPSet + " of the datacenter firewall does not hold " + strings.Join(missing, ", "),
		strings.Join(fixes, "; ")
}

// agentChannelRule says whether r is the rule of the agent channel, with
// nothing more in it.
func agentChannelRule(r pve.FirewallRule) bool {
	return r.Type == "in" && r.Action == "ACCEPT" && r.Proto == "tcp" && r.DPort == agentChannelPort &&
		(r.Source == "+"+AgentChannelIPSet || r.Source == "+dc/"+AgentChannelIPSet) &&
		r.Dest == "" && r.SPort == "" && r.Iface == "" && r.Macro == ""
}

// ipsetHolds says whether the entries hold a, and whether that can be told:
// an entry that names an alias may hold it, or not.
func ipsetHolds(entries []pve.IPSetEntry, a netip.Addr) (held, known bool) {
	excepted := false
	for _, e := range entries {
		p, ok := entryPrefix(e.CIDR)
		if !ok {
			return false, false
		}
		if !p.Contains(a) {
			continue
		}
		if e.NoMatch {
			excepted = true
		} else {
			held = true
		}
	}
	return held && !excepted, true
}

func entryPrefix(cidr string) (netip.Prefix, bool) {
	if p, err := netip.ParsePrefix(cidr); err == nil {
		return p.Masked(), true
	}
	if a, err := netip.ParseAddr(cidr); err == nil {
		return netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()), true
	}
	return netip.Prefix{}, false
}
