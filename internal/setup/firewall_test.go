package setup

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The reads and writes of the datacenter firewall, as pvesh runs them.
const (
	ipsetList    = "pvesh get /cluster/firewall/ipset --output-format json"
	ipsetCreate  = "pvesh create /cluster/firewall/ipset --name pco-nodes --comment " + ipsetComment
	ipsetDelete  = "pvesh delete /cluster/firewall/ipset/pco-nodes"
	ipsetEntries = "pvesh get /cluster/firewall/ipset/pco-nodes --output-format json"
	rulesList    = "pvesh get /cluster/firewall/rules --output-format json"
	ruleCreate   = "pvesh create /cluster/firewall/rules --type in --action ACCEPT --proto tcp --dport 8644 --source +pco-nodes --enable 1 --comment " + ruleComment
)

func entryAdd(node, addr string) string {
	return "pvesh create /cluster/firewall/ipset/pco-nodes --cidr " + addr + " --comment " + entryComment(node)
}

// ownEntryDelete removes this node's address from the ipset.
const ownEntryDelete = "pvesh delete /cluster/firewall/ipset/pco-nodes/" + testAddr

// ipsetJSON is the list of ipsets with pco-nodes, under comment, or without it.
func ipsetJSON(comment string) string {
	if comment == "" {
		return `[{"name":"management","comment":"admins","digest":"d0"}]`
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

const otherRule = `{"pos":0,"type":"in","action":"ACCEPT","proto":"tcp","dport":"8006","source":"+management","enable":1,"comment":"web","digest":"d3"}`

// firewallMade is the firewall step of the first node: the ipset, its entry
// and the rule are made.
func firewallMade() []call {
	return []call{
		{line: ipsetList, out: ipsetJSON("")},
		{line: ipsetCreate},
		{line: ipsetEntries, out: "[]"},
		{line: entryAdd(testNode, testAddr)},
		{line: rulesList, out: "[" + otherRule + "]"},
		{line: ruleCreate},
	}
}

// firewallJoined is the firewall step of a node that joins: the ipset and the
// rule are there, and the node adds its address.
func firewallJoined() []call {
	return []call{
		{line: ipsetList, out: ipsetJSON(ipsetComment)},
		{line: ipsetEntries, out: entriesJSON(firstNode + "=" + firstAddr)},
		{line: entryAdd(testNode, testAddr)},
		{line: rulesList, out: "[" + otherRule + "," + ruleJSON(1, 1, ruleComment) + "]"},
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

func yes() *bool {
	b := true
	return &b
}

func TestTheFirewallRuleIsMadeWithTheFirstNode(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.script(preflightNew("9.0.10"), roleCreated(privs9), userCreated(), tokenCreated(),
		tagsAdded("", "cf-tunnel;cf-tunnel-managed"), firewallMade(), cloudflaredInstalled(), daemonIs("inactive"), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode, FirewallRule: yes()}))
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

	require.NoError(t, e.s.Run(t.Context(), Options{CloudflareToken: cfToken, Node: testNode, RegisterTags: yes(), InstallCloudflared: yes()}))
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

	require.NoError(t, e.s.Run(t.Context(), Options{Yes: true, CloudflareToken: cfToken, Node: testNode}))
	e.done()

	require.True(t, e.manifest().CreatedFirewallRule)
}

func TestAJoinAddsItsAddressToTheIPSet(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	e.script(preflight("9.0.10"), roleKept(), userKept(), tokenKept(), tagsKept(), firewallJoined(), cloudflaredKept(), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode, FirewallRule: yes()}))
	e.done()

	m := e.manifest()
	require.False(t, m.CreatedIPSet)
	require.False(t, m.CreatedFirewallRule)
	require.True(t, m.FirewallRule)
	e.requireShown("firewall: the datacenter rule IN ACCEPT -p tcp -dport 8644 -source +pco-nodes is there, nothing needed")
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
		{line: rulesList, out: "[" + ruleJSON(0, 1, ruleComment) + "]"},
	}, cloudflaredKept(), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode, FirewallRule: yes()}))
	e.done()
}

func TestADisabledRuleIsLeftAloneAndNamed(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	e.script(preflight("9.0.10"), roleKept(), userKept(), tokenKept(), tagsKept(), []call{
		{line: ipsetList, out: ipsetJSON(ipsetComment)},
		{line: ipsetEntries, out: entriesJSON(firstNode+"="+firstAddr, testNode+"="+testAddr)},
		{line: rulesList, out: "[" + ruleJSON(0, 0, "paused by the admin") + "]"},
	}, cloudflaredKept(), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode, FirewallRule: yes()}))
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
		}, cloudflaredInstalled(), daemonIs("inactive"), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode, FirewallRule: yes()}))
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

	err := e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode, FirewallRule: yes()})

	require.ErrorContains(t, err, "setup step firewall: adding the datacenter rule")
	m := e.manifest()
	require.True(t, m.CreatedIPSet)
	require.False(t, m.CreatedFirewallRule, "the note of a rule that was not made is taken back")
	e.done()
}

var withFirewall = Manifest{CreatedUser: true, CreatedToken: true, RegisteredTags: []string{"cf-tunnel", "cf-tunnel-managed"},
	FirewallRule: true, CreatedIPSet: true, CreatedFirewallRule: true}

// firewallRemoved is the removal of the rule and the ipset by the last node.
func firewallRemoved(pos int) []call {
	return []call{
		{line: fmt.Sprintf("pvesh delete /cluster/firewall/rules/%d --digest d3", pos)},
		{line: ownEntryDelete},
		{line: ipsetDelete},
	}
}

func TestTheLastUninstallRemovesTheRuleAndTheIPSet(t *testing.T) {
	e := newTestEnv(t)
	e.installed(withFirewall)
	e.atCloudflare()
	e.script(connectorsSeen(), egressSeen(), userRead(),
		firewallRead(entriesJSON(testNode+"="+testAddr), "["+otherRule+","+ruleJSON(1, 1, ruleComment)+"]"),
		serviceStopped(), connectorsPruned(), egressDeleted(), firewallRemoved(1), userRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true}))
	e.done()

	e.requireShown("the firewall rule of the agent channel, IN ACCEPT -p tcp -dport 8644 -source +pco-nodes, and the ipset pco-nodes")
	e.requireStoreGone()
}

func TestARuleTheManifestDoesNotClaimIsRemovedOnlyWithItsOwnWord(t *testing.T) {
	member := Manifest{FirewallRule: true}
	for _, tt := range []struct {
		name    string
		rules   string
		removes []call
		shown   string
	}{
		{
			name:    "the rule says pco made it",
			rules:   "[" + ruleJSON(0, 1, ruleComment) + "]",
			removes: firewallRemoved(0),
		},
		{
			name:    "the rule says nothing of pco",
			rules:   "[" + ruleJSON(0, 1, "by hand") + "]",
			removes: []call{{line: ownEntryDelete}, {line: ipsetDelete}},
			shown:   "(the firewall rule of the agent channel is kept: pco did not make it)",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installed(member)
			e.script(connectorsSeen(), egressSeen(), firewallRead(entriesJSON(testNode+"="+testAddr), tt.rules),
				serviceStopped(), connectorsPruned(), egressDeleted(), tt.removes)

			require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}))
			e.done()

			if tt.shown != "" {
				e.requireShown(tt.shown)
			}
		})
	}
}

func TestAnIPSetWithEntriesOfOthersIsKept(t *testing.T) {
	e := newTestEnv(t)
	e.installed(withFirewall)
	e.script(connectorsSeen(), egressSeen(), userRead(),
		firewallRead(entriesJSON(testNode+"="+testAddr, `{"cidr":"192.0.2.0/24","comment":"the lab","digest":"d2"}`),
			"["+ruleJSON(0, 1, ruleComment)+"]"),
		serviceStopped(), connectorsPruned(), egressDeleted(),
		[]call{{line: "pvesh delete /cluster/firewall/rules/0 --digest d3"}, {line: ownEntryDelete}}, userRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}))
	e.done()

	e.requireShown("the ipset pco-nodes is kept: it holds 192.0.2.0/24, which pco did not add")
}

func TestAFailedFirewallRemovalKeepsTheStore(t *testing.T) {
	e := newTestEnv(t)
	e.installed(withFirewall)
	e.script(connectorsSeen(), egressSeen(), userRead(),
		firewallRead(entriesJSON(testNode+"="+testAddr), "["+ruleJSON(0, 1, ruleComment)+"]"),
		serviceStopped(), connectorsPruned(), egressDeleted(),
		[]call{{line: "pvesh delete /cluster/firewall/rules/0 --digest d3", err: exitErr(255, "digest mismatch")}}, userRemoved())

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
	_ = store.ErrNoRoot
}
