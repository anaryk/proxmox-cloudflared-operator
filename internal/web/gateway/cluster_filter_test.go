package gateway

import (
	"encoding/json"
	"net/netip"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// withCluster is the populated state of the controller of three nodes: an
// appliance agent waiting for admission, a contested node, and two blocks,
// of qemu/101 and of qemu/140.
func withCluster(t *testing.T) engine.State {
	st := populated(t)
	zero, one := 0, 1
	st.Lease = &engine.LeaseView{Holder: "pve1", Address: netip.MustParseAddr("10.10.0.1"), Generation: 7, Nonce: "k3x9",
		AcquiredAt: t0.Add(-time.Hour), RefreshAge: 3 * time.Second, Valid: true}
	st.Nodes = []engine.NodeView{
		{Name: "pve1", State: "active", Kind: "host", Index: &zero, Controller: true, Build: "1.4.0", Protocol: "1",
			AgentAddress: netip.MustParseAddr("10.10.0.1"), WatchRunning: true, Covered: true, ReplicaReady: true, Verified: 4},
		{Name: "pve2", State: "contested", Kind: "appliance", VMID: 9302, Index: &one, AgentAddress: netip.MustParseAddr("10.0.0.92"),
			Contested: []engine.JoinUseView{
				{At: t0.Add(-3 * time.Minute), Source: netip.MustParseAddr("10.0.0.92"), VMID: 9302, MACs: []string{"bc:24:11:00:93:02"}},
				{At: t0.Add(-2 * time.Minute), Source: netip.MustParseAddr("10.0.0.66"), VMID: 9366, MACs: []string{"bc:24:11:00:93:66"}},
			}},
		{Name: "pve3", State: "joining", Kind: "appliance", VMID: 9303, AgentAddress: netip.MustParseAddr("10.0.0.93"),
			JoinUses: []engine.JoinUseView{{At: t0.Add(-time.Minute), Source: netip.MustParseAddr("10.0.0.93"), VMID: 9303, MACs: []string{"bc:24:11:00:93:03"}}}},
	}
	st.Cluster = &engine.ClusterView{
		Controller: "pve1", Generation: 7, Members: []string{"pve1", "pve2", "pve3"}, Quorate: true,
		Coverage: engine.CoverageView{Covered: true},
		Blocks: []engine.BlockView{
			{Address: netip.MustParseAddr("10.0.0.11"), Guest: "qemu/101", MAC: "bc:24:11:5e:7a:01", Node: "pve2", Port: "tap140i0", Since: t0},
			{Address: netip.MustParseAddr("10.0.0.40"), Guest: "qemu/140", MAC: "bc:24:11:5e:7a:40", Node: "pve3", Port: "tap150i0", Since: t0},
		},
	}
	st.Routes[0].Replicas = []engine.ReplicaView{{Node: "pve1", Level: "port", OK: true, InSet: true}, {Node: "pve2", Level: "port", Attested: true, OK: true, InSet: true}}
	st.Routes[0].Placement = &engine.PlacementView{Tunnel: "pco-abc123", Kind: engine.TunnelShared}
	return st
}

// A reader sees the nodes and the cluster, but no use of a join token, no
// address of an appliance agent's container, no block of a guest it may not
// audit and no MAC anywhere in them.
func TestAReaderSeesNoSecretOfTheCluster(t *testing.T) {
	st := withCluster(t)
	got := FilterState(st, sees(101))

	require.Equal(t, st.Lease, got.Lease, "the lease is the cluster's")
	require.Len(t, got.Nodes, 3)
	for _, n := range got.Nodes {
		require.Empty(t, n.JoinUses, n.Name)
		require.Empty(t, n.Contested, n.Name)
		if n.Kind == "appliance" {
			require.False(t, n.AgentAddress.IsValid(), "%s: the address of a container", n.Name)
		}
	}
	require.Equal(t, netip.MustParseAddr("10.10.0.1"), got.Nodes[0].AgentAddress, "a host's address is the node's")
	require.Equal(t, "contested", got.Nodes[1].State, "the state of a node stays")

	require.Len(t, got.Cluster.Blocks, 1)
	require.Equal(t, "qemu/101", got.Cluster.Blocks[0].Guest)
	require.Empty(t, got.Cluster.Blocks[0].MAC)
	require.Equal(t, st.Routes[0].Replicas, got.Routes[0].Replicas, "a route goes whole")

	for _, part := range []any{got.Nodes, got.Cluster, got.Lease} {
		data, err := json.Marshal(part)
		require.NoError(t, err)
		require.False(t, regexp.MustCompile(`(?i)\b[0-9a-f]{2}(:[0-9a-f]{2}){5}\b`).Match(data), "no MAC: %s", data)
		require.NotContains(t, string(data), "10.0.0.9", "no container's address")
		require.NotContains(t, string(data), "qemu/140")
		require.NotContains(t, string(data), "tap140", "the port where the MAC was seen is another guest's")
	}

	require.Len(t, st.Cluster.Blocks, 2, "the state given is left as it was")
	require.NotEmpty(t, st.Nodes[1].Contested)
}

// Without a cluster a reader gets the views as they are: none.
func TestAReaderOfASingleNodeGetsNoCluster(t *testing.T) {
	got := FilterState(populated(t), sees(101))
	require.Nil(t, got.Lease)
	require.Nil(t, got.Cluster)
	require.Equal(t, []engine.NodeView{}, got.Nodes)
}

// The events of the enrolment are an admin's: they name the sources and the
// containers of joins. Those of attestations and placements lose the MACs
// their text names; the others keep their node.
func TestAReaderGetsTheEventsOfTheClusterWithoutMACs(t *testing.T) {
	evs := []engine.Event{
		{Seq: 1, Level: "warn", Kind: "enrol", Subject: "pve3", Message: "join token of pve3 used from 10.0.0.66 by lxc/9366", Node: "pve1", Security: true},
		{Seq: 2, Level: "warn", Kind: "attestation", Subject: "www.example.com", Guest: "qemu/101",
			Message: "MAC bc:24:11:5e:7a:01 of qemu/101 seen on tap140i0 of pve2", Node: "pve2"},
		{Seq: 3, Level: "info", Kind: "placement", Subject: "www.example.com", Route: "www.example.com",
			Message: "pinned to pve2: bc:24:11:5e:7a:01 moved", Node: "pve1"},
		{Seq: 4, Level: "info", Kind: "lease", Subject: "pve1", Message: "acquired generation 7", Node: "pve1", Security: true},
	}
	got := FilterEvents(evs, sees(101), map[string]bool{"www.example.com": true})
	var seqs []uint64
	for _, ev := range got {
		seqs = append(seqs, ev.Seq)
		require.NotContains(t, ev.Message, "bc:24:11")
		require.NotEmpty(t, ev.Node)
	}
	require.Equal(t, []uint64{2, 3, 4}, seqs)
	require.Equal(t, "MAC a MAC of qemu/101 seen on tap140i0 of pve2", got[0].Message)
	require.True(t, strings.Contains(evs[1].Message, "bc:24:11"), "the input is as it was")
}
