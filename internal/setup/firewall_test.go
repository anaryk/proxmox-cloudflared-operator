package setup

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The reads and writes of the datacenter firewall, as pvesh runs them.
const (
	ipsetList    = "pvesh get /cluster/firewall/ipset --output-format json"
	ipsetCreate  = "pvesh create /cluster/firewall/ipset --name pco-nodes --comment " + ipsetComment
	ipsetDelete  = "pvesh delete /cluster/firewall/ipset/pco-nodes"
	ipsetEntries = "pvesh get /cluster/firewall/ipset/pco-nodes --output-format json"
	rulesList    = "pvesh get /cluster/firewall/rules --output-format json"
	groupsList   = "pvesh get /cluster/firewall/groups --output-format json"
	nodesList    = "pvesh get /nodes --output-format json"
	hostRules    = "pvesh get /nodes/" + testNode + "/firewall/rules --output-format json"
	oneNode      = `[{"node":"` + testNode + `","status":"online"}]`
	ruleCreate   = "pvesh create /cluster/firewall/rules --type in --action ACCEPT --proto tcp --dport 8644 --source +pco-nodes --enable 1 --comment " + ruleComment

	// ownEntryDelete removes this node's address from the ipset, with the
	// digest of the listing.
	ownEntryDelete = "pvesh delete /cluster/firewall/ipset/pco-nodes/" + testAddr + " --digest d2"
)

func entryAdd(node, addr string) string {
	return "pvesh create /cluster/firewall/ipset/pco-nodes --cidr " + addr + " --comment " + entryComment(node)
}

func ruleDelete(pos int) string {
	return fmt.Sprintf("pvesh delete /cluster/firewall/rules/%d --digest d3", pos)
}

// ipsetJSON is the list of ipsets with pco-nodes, under comment, or without it.
func ipsetJSON(comment string) string {
	if comment == "" {
		return `[{"name":"management","comment":"admins","digest":"d1"}]`
	}
	return `[{"name":"management","comment":"admins","digest":"d1"},{"name":"pco-nodes","comment":"` + comment + `","digest":"d1"}]`
}

// entriesJSON lists the entries of pco-nodes: pco's for the nodes given, as
// node=addr, and any other as it is given.
func entriesJSON(entries ...string) string {
	var items []string
	for _, e := range entries {
		if node, addr, ok := strings.Cut(e, "="); ok {
			items = append(items, fmt.Sprintf(`{"cidr":"%s","comment":"%s","digest":"d2"}`, addr, entryComment(node)))
			continue
		}
		items = append(items, e)
	}
	return "[" + strings.Join(items, ",") + "]"
}

// ruleJSON is a rule of the agent channel at pos, as pvesh prints it.
func ruleJSON(pos, enable int, comment string) string {
	return fmt.Sprintf(`{"pos":%d,"type":"in","action":"ACCEPT","proto":"tcp","dport":"8644","source":"+pco-nodes","enable":%d,"comment":"%s","digest":"d3"}`,
		pos, enable, comment)
}

func rules(items ...string) string { return "[" + strings.Join(items, ",") + "]" }

const otherRule = `{"pos":0,"type":"in","action":"ACCEPT","proto":"tcp","dport":"8006","source":"+management","enable":1,"comment":"web","digest":"d3"}`

// firewallMade is the firewall step of the first node: the ipset, its entry
// and the rule are made, and no copy of the rule was added beside it.
func firewallMade() []call {
	return []call{
		{line: ipsetList, out: ipsetJSON("")},
		{line: ipsetCreate},
		{line: ipsetEntries, out: "[]"},
		{line: entryAdd(testNode, testAddr)},
		{line: rulesList, out: rules(otherRule)},
		{line: ruleCreate},
		{line: rulesList, out: rules(ruleJSON(0, 1, ruleComment), strings.Replace(otherRule, `"pos":0`, `"pos":1`, 1))},
	}
}

// firewallJoined is the firewall step of a node that joins: the ipset and the
// rule are there, and the node adds its address.
func firewallJoined() []call {
	return []call{
		{line: ipsetList, out: ipsetJSON(ipsetComment)},
		{line: ipsetEntries, out: entriesJSON(firstNode + "=" + firstAddr)},
		{line: entryAdd(testNode, testAddr)},
		{line: rulesList, out: rules(otherRule, ruleJSON(1, 1, ruleComment))},
	}
}

// firewallJoinedAt is firewallJoined for a node whose agent address is addr.
func firewallJoinedAt(addr string) []call {
	return []call{
		{line: ipsetList, out: ipsetJSON(ipsetComment)},
		{line: ipsetEntries, out: entriesJSON(firstNode + "=" + firstAddr)},
		{line: entryAdd(testNode, addr)},
		{line: rulesList, out: rules(otherRule, ruleJSON(1, 1, ruleComment))},
	}
}

// firewallRead is what uninstall reads of the firewall before it asks.
func firewallRead(entries, rules string) []call {
	return []call{
		{line: ipsetList, out: ipsetJSON(ipsetComment)},
		{line: ipsetEntries, out: entries},
		{line: rulesList, out: rules},
	}
}

// noFirewallSeen is a firewall without anything of pco.
func noFirewallSeen() []call {
	return []call{{line: ipsetList, out: ipsetJSON("")}, {line: rulesList, out: rules(otherRule)}}
}

func yes() *bool {
	b := true
	return &b
}

func TestTheFirewallRuleIsMadeWithTheFirstNode(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.script(preflightNew("9.0.10"), roleCreated(privs9), userCreated(), tokenCreated(),
		tagsAdded("", "cf-tunnel;cf-tunnel-managed"), firewallMade(), cloudflaredInstalled(), daemonIs("inactive"), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, firewallAlone: true, Node: testNode, FirewallRule: yes()}))
	e.done()

	m := e.manifest()
	require.True(t, m.CreatedIPSet)
	require.True(t, m.CreatedFirewallRule)
	require.True(t, m.FirewallRule)
	e.requireShown("firewall: added the datacenter rule IN ACCEPT -p tcp -dport 8644 -source +pco-nodes")
	e.requireShown("firewall: ipset pco-nodes holds " + testAddr + " (" + testNode + ")")
}

func TestTheFirewallRuleIsOfferedAndMayBeDeclined(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.ask.answers = []answer{{about: "8644", yes: false}}
	e.script(freshInstall()...)

	require.NoError(t, e.s.Run(t.Context(), Options{CloudflareToken: cfToken, firewallAlone: true, Node: testNode, RegisterTags: yes(), InstallCloudflared: yes()}))
	e.done()

	m := e.manifest()
	require.False(t, m.CreatedIPSet)
	require.False(t, m.CreatedFirewallRule)
	require.False(t, m.FirewallRule)
	e.requireShown("ask: Add the datacenter firewall rule")
	e.requireShown("firewall: no rule for the agent channel")
	require.NotContains(t, strings.Join(e.run.ran, "\n"), "firewall")
}

func TestTheFirewallRuleIsTakenWithYes(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.script(preflightNew("9.0.10"), roleCreated(privs9), userCreated(), tokenCreated(),
		tagsAdded("", "cf-tunnel;cf-tunnel-managed"), firewallMade(), cloudflaredInstalled(), daemonIs("inactive"), serviceStarted())

	require.NoError(t, e.s.Run(t.Context(), Options{Yes: true, CloudflareToken: cfToken, firewallAlone: true, Node: testNode}))
	e.done()

	require.True(t, e.manifest().CreatedFirewallRule)
}

func TestAnIPSetMadeMeanwhileByAnotherNodeIsNotThisNodes(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.script(preflightNew("9.0.10"), roleCreated(privs9), userCreated(), tokenCreated(),
		tagsAdded("", "cf-tunnel;cf-tunnel-managed"), []call{
			{line: ipsetList, out: ipsetJSON("")},
			{line: ipsetCreate, err: exitErr(255, "create ipset failed: ipset 'pco-nodes' already exists")},
			{line: ipsetList, out: ipsetJSON(ipsetComment)},
			{line: ipsetEntries, out: "[]"},
			{line: entryAdd(testNode, testAddr)},
			{line: rulesList, out: rules(ruleJSON(0, 1, ruleComment))},
		}, cloudflaredInstalled(), daemonIs("inactive"), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, firewallAlone: true, Node: testNode, FirewallRule: yes()}))
	e.done()

	m := e.manifest()
	require.False(t, m.CreatedIPSet, "the ipset is the other node's")
	require.True(t, m.FirewallRule)
	e.requireShown("the ipset pco-nodes was made meanwhile, by a setup on another node")
}

func TestASecondCopyOfTheRuleIsRemovedKeepingTheTopmost(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.script(preflightNew("9.0.10"), roleCreated(privs9), userCreated(), tokenCreated(),
		tagsAdded("", "cf-tunnel;cf-tunnel-managed"), []call{
			{line: ipsetList, out: ipsetJSON(ipsetComment)},
			{line: ipsetEntries, out: "[]"},
			{line: entryAdd(testNode, testAddr)},
			{line: rulesList, out: rules(otherRule)},
			{line: ruleCreate},
			// Another node added its copy at the same time.
			{line: rulesList, out: rules(ruleJSON(0, 1, ruleComment), ruleJSON(1, 1, ruleComment), strings.Replace(otherRule, `"pos":0`, `"pos":2`, 1))},
			{line: ruleDelete(1)},
			{line: rulesList, out: rules(ruleJSON(0, 1, ruleComment), strings.Replace(otherRule, `"pos":0`, `"pos":1`, 1))},
		}, cloudflaredInstalled(), daemonIs("inactive"), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, firewallAlone: true, Node: testNode, FirewallRule: yes()}))
	e.done()

	e.requireShown("removed a second copy of the datacenter rule")
}

// An IPv6 agent address goes into the ipset as it is: an address, no
// brackets, no prefix length.
func TestAJoinAddsAnIPv6AddressToTheIPSet(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	e.clusterAs(testNode, "fd00:4::10", firstNode)
	e.script(preflight("9.0.10"), roleKept(), userKept(), tokenKept(), tagsKept(), firewallJoinedAt("fd00:4::10"), cloudflaredKept(), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, firewallAlone: true, Node: testNode, FirewallRule: yes()}))
	e.done()

	require.Equal(t, netip.MustParseAddr("fd00:4::10"), e.node(testNode).AgentAddress)
	e.requireShown("firewall: ipset pco-nodes holds fd00:4::10 (" + testNode + ")")
}

func TestAJoinAddsItsAddressToTheIPSet(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	e.script(preflight("9.0.10"), roleKept(), userKept(), tokenKept(), tagsKept(), firewallJoined(), cloudflaredKept(), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, firewallAlone: true, Node: testNode, FirewallRule: yes()}))
	e.done()

	m := e.manifest()
	require.False(t, m.CreatedIPSet)
	require.False(t, m.CreatedFirewallRule)
	require.True(t, m.FirewallRule)
	e.requireShown("firewall: the datacenter rule IN ACCEPT -p tcp -dport 8644 -source +pco-nodes is there, nothing needed")
}

func TestAnAddressTheNodeNoLongerHasLeavesTheIPSet(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	e.script(preflight("9.0.10"), roleKept(), userKept(), tokenKept(), tagsKept(), []call{
		{line: ipsetList, out: ipsetJSON(ipsetComment)},
		{line: ipsetEntries, out: entriesJSON(firstNode+"="+firstAddr, testNode+"=192.0.2.99")},
		{line: "pvesh delete /cluster/firewall/ipset/pco-nodes/192.0.2.99 --digest d2"},
		{line: ipsetEntries, out: entriesJSON(firstNode + "=" + firstAddr)},
		{line: entryAdd(testNode, testAddr)},
		{line: rulesList, out: rules(ruleJSON(0, 1, ruleComment))},
	}, cloudflaredKept(), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, firewallAlone: true, Node: testNode, FirewallRule: yes()}))
	e.done()
}

func TestAJoinAddsTheAddressesOfTheOtherNodesTheIPSetLacks(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	e.script(preflight("9.0.10"), roleKept(), userKept(), tokenKept(), tagsKept(), []call{
		{line: ipsetList, out: ipsetJSON(ipsetComment)},
		{line: ipsetEntries, out: "[]"},
		{line: entryAdd(firstNode, firstAddr)},
		{line: entryAdd(testNode, testAddr)},
		{line: rulesList, out: rules(ruleJSON(0, 1, ruleComment))},
	}, cloudflaredKept(), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, firewallAlone: true, Node: testNode, FirewallRule: yes()}))
	e.done()
}

func TestADisabledRuleIsLeftAloneAndNamed(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	e.script(preflight("9.0.10"), roleKept(), userKept(), tokenKept(), tagsKept(), []call{
		{line: ipsetList, out: ipsetJSON(ipsetComment)},
		{line: ipsetEntries, out: entriesJSON(firstNode+"="+firstAddr, testNode+"="+testAddr)},
		{line: rulesList, out: rules(ruleJSON(0, 0, "paused by the admin"))},
	}, cloudflaredKept(), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, firewallAlone: true, Node: testNode, FirewallRule: yes()}))
	e.done()

	e.requireShown("warn: firewall: the datacenter rule IN ACCEPT -p tcp -dport 8644 -source +pco-nodes is there but disabled")
	require.False(t, e.manifest().CreatedFirewallRule)
}

func TestAnIPSetMadeByHandIsUsedAndKept(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.script(preflightNew("9.0.10"), roleCreated(privs9), userCreated(), tokenCreated(),
		tagsAdded("", "cf-tunnel;cf-tunnel-managed"), []call{
			{line: ipsetList, out: ipsetJSON("our nodes")},
			{line: ipsetEntries, out: entriesJSON(`{"cidr":"192.0.2.0/24","comment":"the lab","digest":"d2"}`)},
			{line: entryAdd(testNode, testAddr)},
			{line: rulesList, out: "[]"},
			{line: ruleCreate},
			{line: rulesList, out: rules(ruleJSON(0, 1, ruleComment))},
		}, cloudflaredInstalled(), daemonIs("inactive"), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, firewallAlone: true, Node: testNode, FirewallRule: yes()}))
	e.done()

	m := e.manifest()
	require.False(t, m.CreatedIPSet)
	require.True(t, m.CreatedFirewallRule)
}

func TestAFailedRuleIsNotedAndTheNoteTakenBackWhenNothingIsThere(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.script(preflightNew("9.0.10"), roleCreated(privs9), userCreated(), tokenCreated(),
		tagsAdded("", "cf-tunnel;cf-tunnel-managed"), []call{
			{line: ipsetList, out: ipsetJSON("")},
			{line: ipsetCreate},
			{line: ipsetEntries, out: "[]"},
			{line: entryAdd(testNode, testAddr)},
			{line: rulesList, out: "[]"},
			{line: ruleCreate, err: exitErr(255, "firewall is locked")},
			{line: rulesList, out: "[]"},
		})

	err := e.setup(Options{Yes: true, CloudflareToken: cfToken, firewallAlone: true, Node: testNode, FirewallRule: yes()})

	require.ErrorContains(t, err, "setup step firewall: adding the datacenter rule")
	m := e.manifest()
	require.True(t, m.CreatedIPSet)
	require.False(t, m.CreatedFirewallRule, "the note of a rule that was not made is taken back")
	e.done()
}

var withFirewall = Manifest{CreatedUser: true, CreatedToken: true, RegisteredTags: []string{"cf-tunnel", "cf-tunnel-managed"},
	FirewallRule: true, CreatedIPSet: true, CreatedFirewallRule: true}

// rulesRemoved removes pco's rule at pos from before, which leaves after.
func rulesRemoved(before string, pos int, after string) []call {
	return []call{{line: rulesList, out: before}, {line: ruleDelete(pos)}, {line: rulesList, out: after}}
}

// ipsetRemoved removes this node's entry and the ipset, once nothing refers
// to it.
func ipsetRemoved() []call {
	return []call{
		{line: groupsList, out: "[]"}, {line: nodesList, out: oneNode}, {line: hostRules, out: "[]"},
		{line: ipsetEntries, out: entriesJSON(testNode + "=" + testAddr)},
		{line: ownEntryDelete},
		{line: ipsetEntries, out: "[]"},
		{line: ipsetDelete},
	}
}

func TestTheLastUninstallRemovesTheRuleAndTheIPSet(t *testing.T) {
	e := newTestEnv(t)
	e.installed(withFirewall)
	e.atCloudflare()
	before := rules(otherRule, ruleJSON(1, 1, ruleComment))
	e.script(connectorsSeen(), egressSeen(), userRead(), firewallRead(entriesJSON(testNode+"="+testAddr), before),
		serviceStopped(), connectorsPruned(), egressDeleted(), rulesRemoved(before, 1, rules(otherRule)), ipsetRemoved(), userRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true}))
	e.done()

	e.requireShown("the firewall rule of the agent channel, IN ACCEPT -p tcp -dport 8644 -source +pco-nodes")
	e.requireShown("firewall: removed the ipset pco-nodes")
	e.requireStoreGone()
}

func TestTheLastUninstallRemovesEveryCopyOfTheRule(t *testing.T) {
	e := newTestEnv(t)
	e.installed(Manifest{FirewallRule: true})
	two := rules(ruleJSON(0, 1, ruleComment), ruleJSON(1, 1, ruleComment))
	e.script(connectorsSeen(), egressSeen(), firewallRead(entriesJSON(testNode+"="+testAddr), two),
		serviceStopped(), connectorsPruned(), egressDeleted(),
		[]call{{line: rulesList, out: two}, {line: ruleDelete(1)}}, rulesRemoved(rules(ruleJSON(0, 1, ruleComment)), 0, "[]"), ipsetRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}))
	e.done()
}

func TestARuleThatIsNotPcosIsKeptAndKeepsTheIPSet(t *testing.T) {
	e := newTestEnv(t)
	// The manifest says pco made the rule, but the admin has made it theirs.
	e.installed(Manifest{FirewallRule: true, CreatedIPSet: true, CreatedFirewallRule: true})
	theirs := rules(ruleJSON(0, 1, "by hand"))
	e.script(connectorsSeen(), egressSeen(), firewallRead(entriesJSON(testNode+"="+testAddr), theirs),
		serviceStopped(), connectorsPruned(), egressDeleted(), []call{{line: rulesList, out: theirs}, {line: groupsList, out: "[]"}, {line: nodesList, out: oneNode}, {line: hostRules, out: "[]"}})

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}))
	e.done()

	e.requireShown("(the firewall rule of the agent channel is kept: pco did not make it)")
	e.requireShown("firewall: the ipset pco-nodes is kept with its entries: the datacenter rule at position 0 refers to it")
}

func TestARuleOfASecurityGroupKeepsTheIPSet(t *testing.T) {
	e := newTestEnv(t)
	e.installed(withFirewall)
	before := rules(ruleJSON(0, 1, ruleComment))
	e.script(connectorsSeen(), egressSeen(), userRead(), firewallRead(entriesJSON(testNode+"="+testAddr), before),
		serviceStopped(), connectorsPruned(), egressDeleted(), rulesRemoved(before, 0, "[]"), []call{
			{line: groupsList, out: `[{"group":"ssh","comment":"","digest":"d4"}]`},
			{line: "pvesh get /cluster/firewall/groups/ssh --output-format json",
				out: `[{"pos":0,"type":"in","action":"ACCEPT","proto":"tcp","dport":"22","source":"+dc/pco-nodes","enable":1,"digest":"d4"}]`},
			{line: nodesList, out: oneNode}, {line: hostRules, out: "[]"},
		}, userRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}))
	e.done()

	e.requireShown("the ipset pco-nodes is kept with its entries: the rule at position 0 of the security group ssh refers to it")
}

func TestAHostRuleOfAnyNodeKeepsTheIPSet(t *testing.T) {
	e := newTestEnv(t)
	e.installed(withFirewall)
	before := rules(ruleJSON(0, 1, ruleComment))
	e.script(connectorsSeen(), egressSeen(), userRead(), firewallRead(entriesJSON(testNode+"="+testAddr), before),
		serviceStopped(), connectorsPruned(), egressDeleted(), rulesRemoved(before, 0, "[]"), []call{
			{line: groupsList, out: "[]"},
			{line: nodesList, out: `[{"node":"pve1","status":"online"},{"node":"pve2","status":"online"}]`},
			{line: hostRules, out: "[]"},
			{line: "pvesh get /nodes/pve2/firewall/rules --output-format json",
				out: `[{"pos":3,"type":"out","action":"ACCEPT","proto":"tcp","dport":"8644","dest":"+pco-nodes","enable":1,"digest":"d5"}]`},
		}, userRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}))
	e.done()

	e.requireShown("the ipset pco-nodes is kept with its entries: the host rule at position 3 of node pve2 refers to it")
}

func TestANodeWhoseHostRulesCannotBeReadKeepsTheIPSet(t *testing.T) {
	e := newTestEnv(t)
	e.installed(withFirewall)
	before := rules(ruleJSON(0, 1, ruleComment))
	e.script(connectorsSeen(), egressSeen(), userRead(), firewallRead(entriesJSON(testNode+"="+testAddr), before),
		serviceStopped(), connectorsPruned(), egressDeleted(), rulesRemoved(before, 0, "[]"), []call{
			{line: groupsList, out: "[]"},
			{line: nodesList, out: `[{"node":"pve1","status":"online"},{"node":"pve2","status":"offline"}]`},
			{line: hostRules, out: "[]"},
			{line: "pvesh get /nodes/pve2/firewall/rules --output-format json", err: exitErr(255, "no route to host")},
		}, userRemoved())

	err := e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true})

	require.ErrorContains(t, err, "the ipset pco-nodes is kept: listing the host firewall rules of node pve2")
	e.requireStoreKept()
	e.done()
}

func TestANodeAloneIsNotOfferedTheFirewallRule(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options Options
	}{
		{"with yes", Options{Yes: true, CloudflareToken: cfToken, Node: testNode}},
		{"asked", Options{CloudflareToken: cfToken, Node: testNode, RegisterTags: yes(), InstallCloudflared: yes()}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installUnit(serviceUnit)
			e.script(freshInstall()...)

			require.NoError(t, e.s.Run(t.Context(), tt.options))
			e.done()

			require.False(t, e.manifest().inFirewall())
			require.NotContains(t, e.ask.text(), "8644", "nothing is asked or said of the rule")
			require.NotContains(t, strings.Join(e.run.ran, "\n"), "firewall")
		})
	}
}

func TestAnIPSetWithEntriesOfOthersIsKept(t *testing.T) {
	e := newTestEnv(t)
	e.installed(withFirewall)
	other := `{"cidr":"192.0.2.0/24","comment":"the lab","digest":"d2"}`
	before := rules(ruleJSON(0, 1, ruleComment))
	e.script(connectorsSeen(), egressSeen(), userRead(), firewallRead(entriesJSON(testNode+"="+testAddr, other), before),
		serviceStopped(), connectorsPruned(), egressDeleted(), rulesRemoved(before, 0, "[]"), []call{
			{line: groupsList, out: "[]"}, {line: nodesList, out: oneNode}, {line: hostRules, out: "[]"},
			{line: ipsetEntries, out: entriesJSON(testNode+"="+testAddr, other)},
			{line: ownEntryDelete},
			{line: ipsetEntries, out: entriesJSON(other)},
		}, userRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}))
	e.done()

	e.requireShown("the ipset pco-nodes is kept: it holds 192.0.2.0/24, which pco did not add")
}

func TestAFailedFirewallRemovalKeepsTheStore(t *testing.T) {
	e := newTestEnv(t)
	e.installed(withFirewall)
	before := rules(ruleJSON(0, 1, ruleComment))
	e.script(connectorsSeen(), egressSeen(), userRead(), firewallRead(entriesJSON(testNode+"="+testAddr), before),
		serviceStopped(), connectorsPruned(), egressDeleted(),
		[]call{{line: rulesList, out: before}, {line: ruleDelete(0), err: exitErr(255, "digest mismatch")}}, userRemoved())

	err := e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true})

	require.ErrorContains(t, err, "removing the firewall rule of the agent channel")
	e.requireStoreKept()
	e.done()
}

func TestTheFirewallIsNotLookedAtWhenTheManifestSaysNothing(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.script(connectorsSeen(), egressSeen(), userRead(), serviceStopped(), connectorsPruned(), egressDeleted(), userRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}))
	e.done()

	require.NotContains(t, strings.Join(e.run.ran, "\n"), "firewall")
}

func TestTheFirewallSurveyThatFailsKeepsTheRule(t *testing.T) {
	e := newTestEnv(t)
	e.installed(Manifest{FirewallRule: true, CreatedIPSet: true, CreatedFirewallRule: true})
	e.script(connectorsSeen(), egressSeen(), []call{{line: ipsetList, err: exitErr(255, "no such API")}},
		serviceStopped(), connectorsPruned(), egressDeleted())

	err := e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true})

	require.ErrorContains(t, err, "the firewall rule of the agent channel and the ipset pco-nodes are kept")
	e.requireStoreKept()
	e.done()
}
