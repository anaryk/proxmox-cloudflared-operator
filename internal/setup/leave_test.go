package setup

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// joinedNode is a node that joined the install of firstNode, was admitted
// with index 4 and runs the connector of tunnelID.
func (e *testEnv) joinedNode() {
	t := e.t
	t.Helper()
	e.installUnit(serviceUnit)
	e.joinable()
	e.script(joined()...)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()
	n := e.node(testNode)
	four := 4
	n.Index, n.State, n.StateSince = &four, string(agentproto.Active), t0.Add(time.Minute)
	require.NoError(t, e.st.SaveNode(n))
	tunnels := filepath.Join(e.paths.Local, "tunnels")
	require.NoError(t, os.MkdirAll(tunnels, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(tunnels, tunnelID+".token"), []byte("run token"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tunnels, tunnelID+".env"), []byte("METRICS_ADDR=127.0.0.1:20300\n"), 0o644))
}

// holder makes this node the one that holds the lease.
func (e *testEnv) holder() {
	e.t.Helper()
	require.NoError(e.t, e.st.SaveLeader(store.LeaderRecord{
		Writer: planner.Writer{InstallID: testInstall, Generation: 3, Nonce: "abcd1234"}, Node: testNode, AcquiredAt: t0.Add(-time.Hour),
	}))
}

// left is the script of a node that leaves the install: nothing of Proxmox
// or of the firewall is read, since the manifest of a joined node names
// nothing it created there.
func left() [][]call {
	return [][]call{connectorsSeen(), noEgressSeen(), noFirewallSeen(), serviceStopped(), connectorsPruned()}
}

func (e *testEnv) requireLeft() {
	t := e.t
	t.Helper()
	n := e.node(testNode)
	require.Equal(t, string(agentproto.Removed), n.State)
	require.Equal(t, t0, n.StateSince)
	require.Equal(t, 4, *n.Index, "the index stays with the name")
	_, found, err := e.st.Enrolment(testNode)
	require.NoError(t, err)
	require.False(t, found, "the certificate is refused from now on")
	require.Equal(t, string(agentproto.Active), e.node(firstNode).State)
	require.NoDirExists(t, e.paths.Local)
	require.DirExists(t, e.paths.Cluster)
	require.DirExists(t, e.paths.Private)
	require.Equal(t, testInstall, e.install().ID)
	require.Len(t, e.credentials(), 1)
	e.requireNoDelete()
	e.requireShown("pco is removed from node " + testNode + "; the install keeps running on " + firstNode)
}

func TestUninstallOfANodeThatIsNotTheLastLeavesTheInstall(t *testing.T) {
	e := newTestEnv(t)
	e.joinedNode()
	e.script(left()...)

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true}))
	e.done()

	e.requireLeft()
	require.Empty(t, e.daemon.calls, "a node that does not hold the lease hands nothing off")
	require.Empty(t, e.cf.Calls(), "Cloudflare is not even listed")
	require.NotContains(t, e.ask.text(), "Cloudflare")
	e.requireNoSecret()
}

func TestUninstallOfANodeThatIsNotTheLastKeepsWhatTheClusterShares(t *testing.T) {
	e := newTestEnv(t)
	e.joinedNode()
	m := e.manifest()
	m.CreatedUser, m.CreatedToken, m.RegisteredTags = true, true, []string{"cf-tunnel"}
	m.CreatedZone, m.FirewallRule, m.CreatedFirewallRule, m.CreatedIPSet = true, true, true, true
	require.NoError(t, writeManifest(filepath.Join(e.paths.Local, manifestName), m))
	e.script(connectorsSeen(), noEgressSeen(),
		firewallRead(entriesJSON(firstNode+"="+firstAddr, testNode+"="+testAddr), "["+ruleJSON(0, 1, ruleComment)+"]"),
		serviceStopped(), connectorsPruned(), []call{{line: ipsetEntries, out: entriesJSON(firstNode+"="+firstAddr, testNode+"="+testAddr)}, {line: ownEntryDelete}, {line: ipsetEntries, out: entriesJSON(firstNode + "=" + firstAddr)}})

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true}))
	e.done()

	e.requireLeft()
	ran := strings.Join(e.run.ran, "\n")
	require.NotContains(t, ran, "pveum", "the user and the token stay for the other nodes")
	require.NotContains(t, ran, "sdn")
	e.requireShown("the Proxmox user, token, tags, the SDN objects and the firewall rule stay: the nodes " + firstNode + " still use them")
	e.requireShown("  this node's address " + testAddr + " in the ipset pco-nodes")
}

func TestALeavingNodeTakesOutTheEntryAnotherNodeAddedForIt(t *testing.T) {
	e := newTestEnv(t)
	e.joinedNode()
	require.False(t, e.manifest().inFirewall(), "this node's setup put nothing into the firewall")
	both := entriesJSON(firstNode+"="+firstAddr, testNode+"="+testAddr)
	e.script(connectorsSeen(), noEgressSeen(), firewallRead(both, rules(ruleJSON(0, 1, ruleComment))),
		serviceStopped(), connectorsPruned(), []call{
			{line: ipsetEntries, out: both},
			{line: ownEntryDelete},
			{line: ipsetEntries, out: entriesJSON(firstNode + "=" + firstAddr)},
		})

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true}))
	e.done()

	e.requireShown("firewall: removed " + testAddr + " from the ipset pco-nodes")
	e.requireLeft()
}

func TestUninstallOfTheHolderHandsTheLeaseOffFirst(t *testing.T) {
	e := newTestEnv(t)
	e.joinedNode()
	e.holder()
	e.script(connectorsSeen(), noEgressSeen(), noFirewallSeen(), daemonIs("active"), serviceStopped(), connectorsPruned())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true}))
	e.done()

	require.Equal(t, []string{""}, e.daemon.calls, "any other node may take the lease")
	handoff := slices.Index(e.events, "handoff ")
	stop := slices.Index(e.events, "run systemctl disable --now pco.service")
	require.Less(t, handoff, stop, "the lease is handed off before the daemon stops")
	e.requireLeft()
	e.requireShown("lease: handed off")
}

func TestUninstallOfTheHolderWhoseDaemonCannotHandOffIsRefused(t *testing.T) {
	e := newTestEnv(t)
	e.joinedNode()
	e.holder()
	e.daemon.err = errors.New("the pco daemon at /run/pco/pco.sock does not know this request: is it a different version than this pco?")
	e.script(connectorsSeen(), noEgressSeen(), noFirewallSeen(), daemonIs("active"))

	err := e.uninstall(UninstallOptions{Yes: true})

	require.EqualError(t, err, "this node holds the lease and cannot hand it off (the pco daemon at /run/pco/pco.sock does not know "+
		"this request: is it a different version than this pco?); nothing was removed. pco uninstall --force removes the node "+
		"regardless, and another node takes the lease once it runs out")
	require.Equal(t, string(agentproto.Active), e.node(testNode).State)
	require.DirExists(t, e.paths.Local)
	e.done()
}

func TestForceRemovesTheHolderWithoutAHandOff(t *testing.T) {
	e := newTestEnv(t)
	e.joinedNode()
	e.holder()
	e.daemon.err = errors.New("no other node is eligible")
	e.script(connectorsSeen(), noEgressSeen(), noFirewallSeen(), daemonIs("active"), serviceStopped(), connectorsPruned())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, Force: true}))
	e.done()

	e.requireLeft()
	e.requireShown("warn: lease: not handed off (no other node is eligible); another node takes it once it runs out")
}

func TestAHolderWhoseDaemonIsStoppedHandsNothingOff(t *testing.T) {
	e := newTestEnv(t)
	e.joinedNode()
	e.holder()
	e.script(connectorsSeen(), noEgressSeen(), noFirewallSeen(), daemonIs("inactive"), serviceStopped(), connectorsPruned())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true}))
	e.done()

	require.Empty(t, e.daemon.calls)
	e.requireLeft()
}

func TestTheOnlyActiveNodeIsNotRemovedWithoutForce(t *testing.T) {
	e := newTestEnv(t)
	e.joinedNode()
	first := e.node(firstNode)
	first.State = string(agentproto.Joining)
	require.NoError(t, e.st.SaveNode(first))

	err := e.uninstall(UninstallOptions{Yes: true})

	require.EqualError(t, err, "no other node of the install is active ("+firstNode+" is joining), so the connectors of this node may "+
		"be the only ones serving the shared tunnel; nothing was removed. Admit or undrain another node first, or pass --force")
	require.Equal(t, string(agentproto.Active), e.node(testNode).State)
	e.done()

	e.script(connectorsSeen(), noEgressSeen(), noFirewallSeen(), serviceStopped(), connectorsPruned())
	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, Force: true}))
	e.done()
	require.Equal(t, string(agentproto.Removed), e.node(testNode).State)
}

func TestANodeThatIsNotTheLastRefusesWhatOnlyTheLastMayDo(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options UninstallOptions
		want    string
	}{
		{"purge", UninstallOptions{Yes: true, PurgeCloudflare: true}, "--purge-cloudflare goes with the last node of the install: " + firstNode + " still runs it"},
		{"detach", UninstallOptions{Yes: true, DetachManaged: true}, "--detach-managed goes with the last node of the install: " + firstNode + " still runs it"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.joinedNode()

			err := e.uninstall(tt.options)

			require.EqualError(t, err, tt.want)
			require.Equal(t, string(agentproto.Active), e.node(testNode).State)
			e.done()
		})
	}
}

func TestUninstallAsksOnceOnANodeThatIsNotTheLast(t *testing.T) {
	e := newTestEnv(t)
	e.joinedNode()
	e.ask.answers = []answer{{about: "Remove pco from this node", yes: true}}
	e.script(left()...)

	require.NoError(t, e.uninstall(UninstallOptions{}))
	e.done()

	e.requireLeft()
	e.requireShown("  node " + testNode + " in the registry, set to removed; the install stays on " + firstNode)
}

func TestTheLastNodeBesideRemovedOnesUninstallsTheInstall(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.atCloudflare()
	require.NoError(t, e.st.SaveNode(store.NodeEntry{Name: "pve9", Since: t0, State: string(agentproto.Removed), StateSince: t0}))
	e.script(connectorsSeen(), egressSeen(), userRead(), noFirewallSeen(), serviceStopped(), connectorsPruned(), egressDeleted(), userRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true}))
	e.done()

	e.requireStoreGone()
	require.Empty(t, e.daemon.calls)
}
