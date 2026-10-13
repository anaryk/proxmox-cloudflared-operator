package engine

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// clusterState is the populated state of a node of a cluster of three, with
// every view of the cluster filled.
func clusterState() State {
	st := populatedState()
	zero := 0
	st.Lease = &LeaseView{
		Holder: "pve1", Address: netip.MustParseAddr("10.10.0.1"), Previous: "pve2", Generation: 7, Nonce: "k3x9",
		AcquiredAt: t0.Add(-time.Hour), RefreshAge: 3 * time.Second, Valid: true,
	}
	st.Nodes = []NodeView{
		{Name: "pve3", State: "joining", Kind: "appliance", VMID: 9301, AgentAddress: netip.MustParseAddr("10.0.0.93"),
			StatusAge: 4 * time.Second, ClockSkew: 300 * time.Millisecond, StateSince: t0.Add(-time.Minute),
			JoinUses: []JoinUseView{{At: t0.Add(-2 * time.Minute), Source: netip.MustParseAddr("10.0.0.93"), VMID: 9301, MACs: []string{"bc:24:11:00:93:01"}}}},
		{Name: "pve1", State: "active", Kind: "host", Index: &zero, Controller: true, Build: "1.4.0", Protocol: "1",
			AgentAddress: netip.MustParseAddr("10.10.0.1"), StatusAge: time.Second, WatchRunning: true, Covered: true, ReplicaReady: true,
			Verified: 4, CertExpires: t0.Add(80 * 24 * time.Hour), StateSince: t0.Add(-time.Hour)},
	}
	st.Cluster = &ClusterView{
		Controller: "pve1", Generation: 7, Members: []string{"pve1", "pve2", "pve3"}, Quorate: true,
		Coverage: CoverageView{Covered: true, Since: t0.Add(-time.Hour)},
		Upgrade:  &UpgradeView{From: "1.3.2", Since: t0.Add(-48 * time.Hour)},
		Blocks: []BlockView{{Address: netip.MustParseAddr("10.0.0.11"), Guest: "qemu/101", MAC: "bc:24:11:5e:7a:01",
			Node: "pve2", Port: "tap140i0", Since: t0.Add(-time.Minute), LastReport: t0}},
	}
	st.Routes[0].Replicas = []ReplicaView{
		{Node: "pve2", Level: "port", Attested: true, OK: true, InSet: true},
		{Node: "pve1", Level: "port", OK: true, InSet: true},
	}
	st.Routes[0].Placement = &PlacementView{Tunnel: "pco-abc123", Kind: "shared",
		Move: &MoveView{From: "pco-abc123-pve2", To: "pco-abc123", Step: "switch", Since: t0}}
	return st
}

// A state without a cluster reads as before, but for the list of nodes and
// the replicas of each route, which are there and empty.
func TestTheStateWithoutAClusterAddsOnlyEmptyLists(t *testing.T) {
	for _, st := range []State{emptyState(), populatedState().normalized()} {
		data, err := json.Marshal(st)
		require.NoError(t, err)
		text := string(data)
		require.Contains(t, text, `"nodes":[]`)
		for _, absent := range []string{`"lease"`, `"cluster"`, `"placement"`} {
			require.NotContains(t, text, absent)
		}
		require.Equal(t, len(st.Routes), strings.Count(text, `"replicas":[]`), "every route has its replicas, empty")
		for _, tv := range st.Tunnels {
			data, err := json.Marshal(tv)
			require.NoError(t, err)
			require.NotContains(t, string(data), `"kind"`, "the shared tunnel says nothing new")
			require.NotContains(t, string(data), `"node"`)
		}
	}
	data, err := json.Marshal(Event{Seq: 1, Level: "info", Kind: "route", Subject: "www.example.com", Message: "served"})
	require.NoError(t, err)
	require.NotContains(t, string(data), `"node"`)
	require.NotContains(t, string(data), `"security"`)
}

// Ages and skews move on every cycle while what the cluster is stays: the
// digest leaves them out, and every other time of the views.
func TestTheDigestLeavesOutTheAgesOfTheCluster(t *testing.T) {
	st := clusterState().normalized()
	later := st.clone()
	later.Lease.RefreshAge, later.Lease.AcquiredAt = 9*time.Second, t0
	for i := range later.Nodes {
		n := &later.Nodes[i]
		n.StatusAge, n.ClockSkew, n.StateSince, n.CertExpires = 29*time.Second, 2*time.Second, t0, t0
		for j := range n.JoinUses {
			n.JoinUses[j].At = t0
		}
	}
	later.Cluster.Coverage.Since, later.Cluster.Upgrade.Since = t0, t0
	later.Cluster.Blocks[0].Since, later.Cluster.Blocks[0].LastReport = t0.Add(time.Hour), t0.Add(time.Hour)
	later.Routes[0].Placement.Move.Since = t0.Add(time.Hour)
	require.Equal(t, DigestOf(st), DigestOf(later))

	moved := st.clone()
	moved.Nodes[1].State = "active"
	require.NotEqual(t, DigestOf(st), DigestOf(moved), "a change of state is a change")
	replica := st.clone()
	replica.Routes[0].Replicas[0].InSet = false
	require.NotEqual(t, DigestOf(st), DigestOf(replica))
}

// A copy of the state shares nothing of the cluster with the state the
// engine keeps.
func TestACloneSharesNothingOfTheCluster(t *testing.T) {
	st := clusterState().normalized()
	before, err := json.Marshal(st)
	require.NoError(t, err)

	c := st.clone()
	c.Lease.Holder = "x"
	*c.Nodes[0].Index = 9
	c.Nodes[1].JoinUses[0].MACs[0] = "x"
	c.Nodes[0].Name = "x"
	c.Cluster.Members[0] = "x"
	c.Cluster.Upgrade.From = "x"
	c.Cluster.Blocks[0].Guest = "x"
	c.Routes[0].Replicas[0].Node = "x"
	c.Routes[0].Placement.Move.Step = "x"

	after, err := json.Marshal(st)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
}

func TestTheNodesAreByName(t *testing.T) {
	st := clusterState().normalized()
	require.Equal(t, "pve1", st.Nodes[0].Name)
	require.Equal(t, "pve3", st.Nodes[1].Name)
	require.Equal(t, "pve1", st.Routes[0].Replicas[0].Node, "the replicas of a route are by node")
	require.NotNil(t, st.Routes[1].Replicas)
}

// The views of the cluster and the answers of its routes are a contract of
// the API, as the state is.
func TestTheJSONOfTheCluster(t *testing.T) {
	st := clusterState().normalized()
	requireGolden(t, "cluster_status.json", ClusterStatus{Lease: st.Lease, Cluster: *st.Cluster, Nodes: st.Nodes})
	requireGolden(t, "cluster_route.json", st.Routes[0])
	requireGolden(t, "cluster_answers.json", map[string]any{
		"joinToken":     JoinTokenView{ID: "jt1", Node: "pve3", Expires: t0.Add(15 * time.Minute), Token: "pco1.eyJ2IjoxfQ"},
		"handoff":       HandoffResult{From: "pve1", To: "pve2", Until: t0.Add(30 * time.Second)},
		"finishUpgrade": FinishUpgradeResult{From: "1.3.2", Schema: 3, Backup: "/var/lib/pco/backup/3-20261001T120000Z"},
		"forget": ForgetResult{Node: "pve3", Compromised: true, Done: []string{"removed", "connections closed"},
			Pending: []string{"rotate the CA"}, NotRotated: []string{"the corosync authkey"}},
		"promote": PromoteResult{Generation: 8, Nonce: "p2q7", Drawn: true,
			Differences: []DifferenceView{{Subject: "node pve2", Store: "active", Reported: "absent"}}},
		"uninstall": UninstallResult{Clean: []string{"pve1", "pve2"}, Left: []string{"pve3: pco uninstall"}},
		"event":     Event{Seq: 3, Level: "info", Kind: "lease", Subject: "pve1", Message: "acquired generation 7", Node: "pve1", Security: true},
	})
}
