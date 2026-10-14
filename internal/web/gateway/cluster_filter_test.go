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

	require.Equal(t, st.Lease.Holder, got.Lease.Holder, "the lease is the cluster's")
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
	require.Equal(t, st.Routes[0].Replicas, got.Routes[0].Replicas, "replicas without a MAC or a port go as they are")

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

// The events of the audit trail are an admin's, whatever their kind: they
// name the sources and the containers of joins, the lease and its holders.
// Those of attestations, placements and blocks are an admin's as well: a
// route's replicas and placement say what a reader needs of them. What a
// reader keeps loses the admin who acted, the account, and the MACs and
// ports its text names.
func TestAReaderGetsNoEventOfTheAuditTrail(t *testing.T) {
	evs := []engine.Event{
		{Seq: 1, Level: "warn", Kind: "enrol", Subject: "pve3", Message: "join token of pve3 used from 10.0.0.66 by lxc/9366", Node: "pve1", Security: true},
		{Seq: 2, Level: "warn", Kind: "attestation", Subject: "www.example.com", Guest: "qemu/101",
			Message: "MAC bc:24:11:5e:7a:01 of qemu/101 seen on tap140i0 of pve2", Node: "pve2"},
		{Seq: 3, Level: "info", Kind: "placement", Subject: "www.example.com", Route: "www.example.com",
			Message: "pinned to pve2: bc:24:11:5e:7a:01 moved", Node: "pve1"},
		{Seq: 4, Level: "info", Kind: "lease", Subject: "pve1", Message: "acquired generation 7", Node: "pve1", Security: true},
		{Seq: 5, Level: "warn", Kind: "block", Subject: "10.0.0.11", Guest: "qemu/101", Message: "10.0.0.11 blocked: seen on fwpr140p0", Node: "pve2"},
		{Seq: 6, Level: "info", Kind: "enrol", Subject: "qemu/101", Message: "an enrolment without the mark", Node: "pve1"},
		{Seq: 7, Level: "info", Kind: "route", Subject: "www.example.com", Route: "www.example.com", Guest: "qemu/101",
			Message: "qemu/101: active", Node: "pve1", Actor: "alice@pve (ticket)", Account: "acc1"},
		{Seq: 8, Level: "warn", Kind: "managed", Subject: "qemu/101", Guest: "qemu/101",
			Message: "foreign card net1 (bc:24:11:5e:7a:01) of qemu/101 seen on veth140i1 and fwln140i0", Node: "pve2"},
		{Seq: 9, Level: "warn", Kind: "agent:block", Subject: "10.0.0.11", Guest: "qemu/101", Message: "an agent's word", Node: "pve2"},
	}
	got := FilterEvents(evs, sees(101), map[string]bool{"www.example.com": true})
	var seqs []uint64
	for _, ev := range got {
		seqs = append(seqs, ev.Seq)
		require.NotEmpty(t, ev.Node, "the node stays")
		require.Empty(t, ev.Actor)
		require.Empty(t, ev.Account)
		require.False(t, ev.Security)
	}
	require.Equal(t, []uint64{7, 8}, seqs)
	require.Equal(t, "foreign card net1 (a MAC) of qemu/101 seen on a port and a port", got[1].Message)
	require.Equal(t, "alice@pve (ticket)", evs[6].Actor, "the input is as it was")
	require.True(t, strings.Contains(evs[7].Message, "bc:24:11"), "the input is as it was")
}

// An event of the audit trail is dropped by its mark, not by its kind: one of
// a kind no rule of the readers knows, about no guest, would otherwise reach
// every reader.
func TestAnEventOfTheAuditTrailOfAnyKindIsDropped(t *testing.T) {
	ev := engine.Event{Seq: 1, Level: "info", Kind: "rotation", Subject: "ca", Message: "the CA was rotated by alice@pve", Security: true}
	require.Empty(t, FilterEvents([]engine.Event{ev}, sees(101), map[string]bool{}))
	_, kept := readerEvent(ev, sees(101), map[string]bool{})
	require.False(t, kept, "nor does the stream send it")
	ev.Security = false
	require.Len(t, FilterEvents([]engine.Event{ev}, sees(101), map[string]bool{}), 1, "without the mark it is the cluster's")
}

// The free text of a route's replicas and placement may name the MAC a
// node's watch saw and the port of another guest it saw it on: a reader gets
// neither. The state given is left as it was.
func TestAReaderGetsTheReplicasAndPlacementWithoutMACsOrPorts(t *testing.T) {
	st := withCluster(t)
	st.Routes[0].Replicas[1].Reason = "bc:24:11:5e:7a:01 seen on tap140i0 and fwbr140i0"
	st.Routes[0].Placement = &engine.PlacementView{Tunnel: "pco-abc123-pve2", Kind: engine.TunnelPinned, Node: "pve2",
		Why:  "the MAC bc:24:11:5e:7a:01 is seen on veth150i0 only on pve2",
		Move: &engine.MoveView{From: "pco-abc123", To: "pco-abc123-pve2", Step: "waiting for fwpr150p0 on pve2", Since: t0}}

	got := FilterState(st, sees(101)).Routes[0]

	require.Equal(t, "a MAC seen on a port and a port", got.Replicas[1].Reason)
	require.Equal(t, "the MAC a MAC is seen on a port only on pve2", got.Placement.Why)
	require.Equal(t, "waiting for a port on pve2", got.Placement.Move.Step)
	require.Equal(t, "pco-abc123-pve2", got.Placement.Move.To)
	require.Equal(t, "pve2", got.Placement.Node)
	require.Equal(t, "bc:24:11:5e:7a:01 seen on tap140i0 and fwbr140i0", st.Routes[0].Replicas[1].Reason, "the state given is left as it was")
	require.Contains(t, st.Routes[0].Placement.Why, "veth150i0")
	require.Contains(t, st.Routes[0].Placement.Move.Step, "fwpr150p0")
}

// The nonce of the lease fences the writes of its holder: no reader's.
func TestAReaderGetsTheLeaseWithoutItsNonce(t *testing.T) {
	st := withCluster(t)
	got := FilterState(st, sees(101))
	require.Empty(t, got.Lease.Nonce)
	require.Equal(t, "pve1", got.Lease.Holder)
	require.Equal(t, "k3x9", st.Lease.Nonce, "the state given is left as it was")
}

// The redactor of free text takes out the MACs and the names of the ports of
// guests, and leaves the words that only begin as one does.
func TestRedactMACsAndPorts(t *testing.T) {
	for in, want := range map[string]string{
		"seen on tap101i0.":                "seen on a port.",
		"veth9302i0, fwpr101p0, fwln101i0": "a port, a port, a port",
		"fwbr101i1 of BC:24:11:5E:7A:01":   "a port of a MAC",
		"tapping the veth bridge vmbr0":    "tapping the veth bridge vmbr0",
		"on tap140i0.10 of pve2":           "on a port of pve2",
	} {
		require.Equal(t, want, redactMACsAndPorts(in), in)
	}
}
