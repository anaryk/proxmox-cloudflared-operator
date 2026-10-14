package agent

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
)

func pinned(s agentproto.Snapshot) agentproto.Snapshot {
	s.Tunnels = append(s.Tunnels, agentproto.Tunnel{
		AccountID: "acc1", TunnelID: pinnedID, Name: "pco-" + testInstall + "-" + testNode, Kind: agentproto.TunnelPinned,
		Run: true, Token: agentproto.NewSecret(pinnedToken),
	})
	return s
}

func TestTheConnectorsOfTheSnapshotRunWithItsTokens(t *testing.T) {
	e := newEnv(t)

	st := e.apply(pinned(e.snapshot("www.example.com -> :8080")))

	require.Equal(t, []string{"Ensure " + sharedID + " " + sharedToken, "Ensure " + pinnedID + " " + pinnedToken}, e.conn.changes())
	require.Equal(t, []connector.Status{
		{TunnelID: sharedID, Active: true, Install: testInstall},
		{TunnelID: pinnedID, Active: true, Install: testInstall},
	}, st.Connectors)

	e.conn.reset()
	s := pinned(e.snapshot("www.example.com -> :8080"))
	s.Tunnels[0].Token = agentproto.NewSecret("rotated")
	e.apply(s)
	require.Equal(t, []string{"Ensure " + sharedID + " rotated", "Ensure " + pinnedID + " " + pinnedToken}, e.conn.changes(), "the manager restarts what changed")
}

// The controller keeps the connector of a tunnel it may not ask Cloudflare
// about, as one of a frozen account, running with the token on disk.
func TestATunnelWithoutATokenRunsWithTheOneOnDisk(t *testing.T) {
	e := newEnv(t)
	e.conn.put(sharedID, testInstall, "on-disk")
	s := e.snapshot()
	s.Tunnels[0].Token = agentproto.Secret{}

	st := e.apply(s)

	require.Equal(t, []string{"Ensure " + sharedID + " on-disk"}, e.conn.changes())
	require.Empty(t, st.Problems)

	t.Run("and none at all is a problem", func(t *testing.T) {
		e := newEnv(t)
		st := e.apply(s)

		require.Empty(t, e.conn.changes())
		require.Equal(t, []string{"tunnel pco-abc123 in account acc1: no run token in the desired state and none on this node; its connector is not started"}, st.Problems)
	})
}

// A tunnel the snapshot keeps without running it, as in observe mode or on a
// node not admitted yet, is left as it is: not started, not stopped.
func TestATunnelNotToRunIsLeftAsItIs(t *testing.T) {
	e := newEnv(t)
	e.conn.put(sharedID, testInstall, sharedToken)
	s := e.snapshot()
	s.Tunnels[0].Run = false
	s.Prune = true

	st := e.apply(s)

	require.Equal(t, []string{"PruneInstall " + sharedID}, e.conn.changes())
	require.True(t, e.conn.isRunning(sharedID))
	require.Len(t, st.Connectors, 1)
}

// A tunnel the controller retires, as a pinned one that serves nothing any
// more, has its connector stopped: Cloudflare deletes no tunnel that has
// connections. One stopped already is left alone.
func TestATunnelToStopHasItsConnectorStopped(t *testing.T) {
	e := newEnv(t)
	e.apply(pinned(e.snapshot()))
	require.True(t, e.conn.isRunning(pinnedID))
	e.conn.reset()
	s := pinned(e.snapshot())
	s.Tunnels[1].Run, s.Tunnels[1].Token, s.Tunnels[1].Stop = false, agentproto.Secret{}, true

	st := e.apply(s)

	require.Equal(t, []string{"Ensure " + sharedID + " " + sharedToken, "Stop " + pinnedID}, e.conn.changes())
	require.False(t, e.conn.isRunning(pinnedID))
	require.True(t, e.conn.isRunning(sharedID))
	require.Empty(t, st.Problems)

	e.conn.reset()
	s.Seq++
	e.apply(s)
	require.Equal(t, []string{"Ensure " + sharedID + " " + sharedToken}, e.conn.changes(), "a connector stopped is not stopped again")
}

func TestConnectorsArePrunedOnlyWhenTheSnapshotSaysSo(t *testing.T) {
	e := newEnv(t)
	e.conn.put(otherID, testInstall, "old")

	e.apply(e.snapshot())
	require.NotContains(t, e.conn.changes(), "PruneInstall "+sharedID)
	require.True(t, e.conn.isRunning(otherID), "a tunnel the controller may not know of is kept")

	e.conn.reset()
	s := pinned(e.snapshot())
	s.Prune = true
	e.apply(s)
	require.Equal(t, "PruneInstall "+sharedID+","+pinnedID, e.conn.changes()[2])
	require.False(t, e.conn.isRunning(otherID))
}

func TestATokenNeverShowsInAProblem(t *testing.T) {
	e := newEnv(t)
	e.conn.fail["Ensure"] = errBoom

	st := e.apply(e.snapshot())

	require.Equal(t, []string{"tunnel pco-abc123 in account acc1: starting its connector: starting " + sharedID + " with [redacted]: boom"}, st.Problems)
}

// Once a second node is registered, a node that boots starts its shared
// connector only when told to, after it verified what it serves: its unit is
// not enabled at boot. A pinned one is, as the only path to the node's own
// guests.
func TestSharedConnectorsAreNotEnabledAtBootOnceASecondNodeIsRegistered(t *testing.T) {
	e := newEnv(t)
	s := pinned(e.snapshot())
	e.apply(s)
	require.NotContains(t, e.conn.changes(), "Disable "+sharedID, "one node: the shared unit starts at boot")

	e.agent.Heartbeat(agentproto.Heartbeat{Epoch: epoch1, Nodes: map[string]agentproto.NodeFreshness{testNode: {}, "pve2": {}}})
	e.conn.reset()
	e.apply(s)
	require.Equal(t, []string{"Ensure " + sharedID + " " + sharedToken, "Disable " + sharedID, "Ensure " + pinnedID + " " + pinnedToken}, e.conn.changes())

	e.conn.reset()
	e.apply(s)
	require.False(t, slices.Contains(e.conn.changes(), "Disable "+sharedID), "once is enough while it runs")
}

func TestTheStatusReportsEveryConnectorOnTheNode(t *testing.T) {
	e := newEnv(t)
	e.conn.put(otherID, "zzz999", "theirs")
	e.conn.setReady(sharedID, true)

	st := e.apply(e.snapshot())

	require.Equal(t, []connector.Status{
		{TunnelID: sharedID, Active: true, Ready: true, Connections: 4, Install: testInstall},
		{TunnelID: otherID, Active: true, Install: "zzz999"},
	}, st.Connectors, "the controller tells its own from others' by their install")
	require.Equal(t, 1, e.agent.View().Connectors)
}
