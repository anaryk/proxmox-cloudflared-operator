package engine

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The nodes of the state carry what pco cluster status shows of each:
// whether it is covered and its replica ready, how its targets fare, the
// routes pinned to it, when its certificate ends, and the uses of its join
// token.
func TestTheNodeViewsCarryWhatTheClusterStatusShows(t *testing.T) {
	w := threeNodes(t)
	old := testCA(t)
	require.NoError(t, w.store.SaveEnrolment("pve2", store.Enrolment{CertPEM: certUnder(t, old)}))
	require.NoError(t, w.store.SaveJoinToken(store.JoinRecord{
		ID: "tok1", Node: "pve3", SecretSHA256: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", CreatedAt: t0, Expires: t0.Add(time.Hour),
		Uses: []store.JoinUse{{At: t0, Source: netip.MustParseAddr("10.0.0.4"), VMID: 9003}},
	}))
	w.acts["pve3"].fail = map[string]string{"db.example.com": "not reached"}
	for range 10 {
		w.tick(5 * time.Second)
	}
	st := w.tick(time.Second)

	byName := map[string]NodeView{}
	for _, n := range st.Nodes {
		byName[n.Name] = n
	}
	require.Len(t, byName, 3)
	two := byName["pve2"]
	require.True(t, two.Covered)
	require.True(t, two.ReplicaReady)
	require.Equal(t, t0.Add(90*24*time.Hour), two.CertExpires)
	require.Equal(t, 1, two.Pinned, "db.example.com is pinned to its node")
	require.Positive(t, two.Verified)
	three := byName["pve3"]
	require.Equal(t, 1, three.Failing)
	require.Equal(t, []JoinUseView{{At: t0, Source: netip.MustParseAddr("10.0.0.4"), VMID: 9003}}, three.JoinUses)
	require.Zero(t, three.CertExpires)

	cs := w.eng.ClusterStatus()
	require.Equal(t, st.Nodes, cs.Nodes)
	require.Equal(t, *st.Cluster, cs.Cluster)
	require.Equal(t, testNode, cs.Lease.Holder)
	require.Equal(t, cs.Nodes, w.eng.Nodes())
}

// A controller whose cycles hold before they take the agents' reports, as
// while a node is lost and the inventory cannot be read whole, still shows
// itself as the controller, the quorum and the members: what it knows of
// them does not wait for the reports.
func TestTheClusterViewOfACycleThatHeldShowsTheControllerAndTheQuorum(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.eng = w.newEngine()
	w.watchHere(t)
	snap := w.inv.snapshot()
	snap.Complete = false
	w.inv.set(snap)

	st := w.tick(time.Second)

	require.NotEmpty(t, st.Hold)
	require.NotNil(t, st.Cluster)
	cs := w.eng.ClusterStatus()
	require.Equal(t, testNode, cs.Cluster.Controller)
	require.True(t, cs.Cluster.Quorate)
	require.Equal(t, []string{"pve1", "pve2", "pve3"}, cs.Cluster.Members)
	require.Positive(t, cs.Cluster.Generation)

	// A node that does not hold the lease names the one that does.
	w.lease.set(func(st *lease.State) {
		st.Holder, st.Valid = false, false
		st.Record.Node, st.Record.Generation = "pve2", 9
	})
	w.tick(time.Second)
	cs = w.eng.ClusterStatus()
	require.Equal(t, "pve2", cs.Cluster.Controller)
	require.Equal(t, 9, cs.Cluster.Generation)
	require.True(t, cs.Cluster.Quorate)
}

// The cluster view says what corosync.conf says of the quorum.
func TestTheClusterViewShowsTheQuorumDeviceAndTheOverrides(t *testing.T) {
	w := threeNodes(t)
	no := false
	w.corosync = func() (members.Corosync, error) {
		return members.Corosync{Nodes: []string{"pve1", "pve2", "pve3"}, QuorumDevice: true, TwoNode: true, ExpectedVotes: 3, WaitForAll: &no}, nil
	}
	st := w.settle()
	require.True(t, st.Cluster.QuorumDevice)
	require.Equal(t, []string{"two_node", "expected_votes 3", "wait_for_all 0"}, st.Cluster.Overrides)
}

// Uninstalling the cluster removes every other node at once, so that their
// agents stand down; a node that is not online holds it unless forced.
func TestUninstallClusterRemovesEveryOtherNode(t *testing.T) {
	w := threeNodes(t)
	closed := []string{}
	w.closeNode = func(node string) { closed = append(closed, node) }
	w.settle()
	*w.listed = []members.Member{{Name: "pve1", ID: 1, Online: true}, {Name: "pve2", ID: 2, Online: true}, {Name: "pve3", ID: 3, Online: false}}

	_, err := w.eng.UninstallCluster(t.Context(), false)
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "node pve3 is not online")
	require.Equal(t, store.NodeActive, entryOf(t, w.env, "pve2").State)

	res, err := w.eng.UninstallCluster(t.Context(), true)
	require.NoError(t, err)
	require.Equal(t, []string{"pve2", "pve3"}, closed)
	require.Equal(t, UninstallResult{Clean: []string{}, Left: []string{
		"pve2: pco uninstall --yes --keep-cloudflare removes what pco keeps on the node",
		"pve3: pco uninstall --yes --keep-cloudflare removes what pco keeps on the node",
	}}, res)
	require.Equal(t, store.NodeRemoved, entryOf(t, w.env, "pve2").State)
	require.Equal(t, store.NodeRemoved, entryOf(t, w.env, "pve3").State)
	require.Equal(t, store.NodeActive, entryOf(t, w.env, testNode).State)
}

// The agent's own view is answered from the agent of this node, and its
// remembered epoch is forgotten on request.
func TestTheAgentViewAndTheResetOfItsEpoch(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	v, err := w.eng.AgentView()
	require.NoError(t, err)
	require.Equal(t, testNode, v.Node)
	require.Equal(t, testWriter.Generation, v.Epoch.Generation)
	ep, found, err := w.store.AgentEpoch()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, testWriter.Generation, ep.Generation)

	require.NoError(t, w.eng.ResetAgentEpoch(t.Context()))

	_, found, err = w.store.AgentEpoch()
	require.NoError(t, err)
	require.False(t, found)
	_, found, err = w.store.AgentSnapshot()
	require.NoError(t, err)
	require.False(t, found)
	require.Contains(t, adminEvents(w.env, time.Time{}), testNode+": the epoch the agent of this node remembers was forgotten; it follows the store's at the next start of pco")
}
