package engine

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestTheGapOfTheAgentChannelInTheDatacenterFirewall(t *testing.T) {
	rule := pve.FirewallRule{Pos: 2, Type: "in", Action: "ACCEPT", Proto: "tcp", DPort: "8644", Source: "+pco-nodes", Enable: true}
	off := rule
	off.Enable = false
	other := pve.FirewallRule{Pos: 0, Type: "in", Action: "ACCEPT", Proto: "tcp", DPort: "22", Source: "+management", Enable: true}
	silent := []store.NodeEntry{{Name: "pve2", AgentAddress: netip.MustParseAddr("10.0.0.12")}}
	held := []pve.IPSetEntry{{CIDR: "10.0.0.11"}, {CIDR: "10.0.0.12"}}
	for _, tt := range []struct {
		name string
		fw   AgentFirewall
		want string
	}{
		{"the firewall is off", AgentFirewall{Rules: []pve.FirewallRule{other}}, ""},
		{"the rule and the address are there", AgentFirewall{On: true, Rules: []pve.FirewallRule{other, rule}, HasNodes: true, Nodes: held}, ""},
		{"the rule in the other spelling", AgentFirewall{On: true, Rules: []pve.FirewallRule{withSource(rule, "+dc/pco-nodes")}, HasNodes: true, Nodes: held}, ""},
		{"no rule", AgentFirewall{On: true, Rules: []pve.FirewallRule{other}, HasNodes: true, Nodes: held},
			"the datacenter firewall is on without the rule IN ACCEPT -p tcp -dport 8644 -source +pco-nodes"},
		{"the rule is disabled", AgentFirewall{On: true, Rules: []pve.FirewallRule{other, off}, HasNodes: true, Nodes: held},
			"the rule IN ACCEPT -p tcp -dport 8644 -source +pco-nodes of the datacenter firewall, at position 2, is disabled: pvesh set /cluster/firewall/rules/2 --enable 1"},
		{"a rule for more than the channel", AgentFirewall{On: true, Rules: []pve.FirewallRule{withIface(rule, "vmbr1")}, HasNodes: true, Nodes: held},
			"the datacenter firewall is on without the rule"},
		{"no ipset", AgentFirewall{On: true, Rules: []pve.FirewallRule{rule}},
			"the datacenter firewall is on without the ipset pco-nodes"},
		{"the address is not in the ipset", AgentFirewall{On: true, Rules: []pve.FirewallRule{rule}, HasNodes: true, Nodes: held[:1]},
			"the ipset pco-nodes of the datacenter firewall does not hold 10.0.0.12 (pve2): pvesh create /cluster/firewall/ipset/pco-nodes --cidr 10.0.0.12"},
		{"the address is in a network of the ipset", AgentFirewall{On: true, Rules: []pve.FirewallRule{rule}, HasNodes: true, Nodes: []pve.IPSetEntry{{CIDR: "10.0.0.0/24"}}}, ""},
		{"the address is an exception of the ipset", AgentFirewall{On: true, Rules: []pve.FirewallRule{rule}, HasNodes: true,
			Nodes: []pve.IPSetEntry{{CIDR: "10.0.0.0/24"}, {CIDR: "10.0.0.12", NoMatch: true}}}, "does not hold 10.0.0.12 (pve2)"},
		{"an alias in the ipset may hold it", AgentFirewall{On: true, Rules: []pve.FirewallRule{rule}, HasNodes: true, Nodes: []pve.IPSetEntry{{CIDR: "dc/nodes"}}}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gap, fix := agentRuleGap(tt.fw, silent)
			if tt.want == "" {
				require.Empty(t, gap)
				require.Empty(t, fix)
				return
			}
			require.NotEmpty(t, fix)
			require.Contains(t, gap+": "+fix, tt.want)
		})
	}
}

func withSource(r pve.FirewallRule, source string) pve.FirewallRule { r.Source = source; return r }
func withIface(r pve.FirewallRule, iface string) pve.FirewallRule   { r.Iface = iface; return r }

// An agent that stopped reporting while the rule of its channel is gone is
// named, with the rule, in the problems and in the facts the doctor reads;
// the firewall is read only while an agent is silent, once a minute.
func TestASilentAgentWithoutTheRuleOfItsChannelIsNamed(t *testing.T) {
	one := 1
	e, r, listed := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one,
		AgentAddress: netip.MustParseAddr("10.0.0.12")})
	*listed = append(*listed, members.Member{Name: "pve2", ID: 2, Online: true})
	reads := 0
	fw := AgentFirewall{On: true, HasNodes: true, Nodes: []pve.IPSetEntry{{CIDR: "10.0.0.12"}}}
	e.agentFirewall = func(context.Context) (AgentFirewall, error) {
		reads++
		return fw, nil
	}
	e.lease = holding(testWriter)
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	e.eng = e.newEngine()
	e.enforce()

	r.report("pve2", e.clock.now(), t0)
	st := e.cycle()
	require.Zero(t, reads, "no agent is silent")
	require.Empty(t, e.eng.ClusterFacts().AgentRule.Silent)
	require.False(t, hasProblem(st, "sends no status"), st.Problems)

	e.clock.advance(takeoverHold + time.Second)
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	e.eng.d.Quorum = e.quorum
	st = e.cycle()
	require.Equal(t, 1, reads)
	require.True(t, hasProblem(st, "the agent of pve2 sends no status, and the datacenter firewall is on without the rule "+
		"IN ACCEPT -p tcp -dport 8644 -source +pco-nodes"), st.Problems)
	facts := e.eng.ClusterFacts().AgentRule
	require.Equal(t, []string{"pve2"}, facts.Silent)
	require.Contains(t, facts.Gap, "without the rule")

	e.clock.advance(10 * time.Second)
	e.eng.d.Quorum = func() (bool, time.Time) { return true, e.clock.now() }
	st = e.cycle()
	require.Equal(t, 1, reads, "read again only after a while")
	require.True(t, hasProblem(st, "the agent of pve2 sends no status"), st.Problems)

	fw.Rules = []pve.FirewallRule{{Type: "in", Action: "ACCEPT", Proto: "tcp", DPort: "8644", Source: "+pco-nodes", Enable: true}}
	e.clock.advance(agentRuleEvery)
	e.eng.d.Quorum = func() (bool, time.Time) { return true, e.clock.now() }
	st = e.cycle()
	require.Equal(t, 2, reads)
	require.False(t, hasProblem(st, "sends no status, and"), st.Problems)
	require.Empty(t, e.eng.ClusterFacts().AgentRule.Gap)
}

func TestAFirewallThatCannotBeReadIsNoProblemOfItsOwn(t *testing.T) {
	one := 1
	e, _, listed := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one})
	*listed = append(*listed, members.Member{Name: "pve2", ID: 2, Online: true})
	e.agentFirewall = func(context.Context) (AgentFirewall, error) {
		return AgentFirewall{}, errors.New("HTTP 403: Permission check failed")
	}
	e.lease = holding(testWriter)
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	e.eng = e.newEngine()
	e.enforce()
	e.cycle()
	e.clock.advance(takeoverHold + time.Second)
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	e.eng.d.Quorum = e.quorum

	st := e.cycle()
	require.False(t, hasProblem(st, "sends no status, and"), st.Problems)
	facts := e.eng.ClusterFacts().AgentRule
	require.Equal(t, []string{"pve2"}, facts.Silent)
	require.Contains(t, facts.Err, "Permission check failed")
}
