package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func nodeSegment(node, bridge string, vlan int) NodeSegment {
	return NodeSegment{Node: node, Segment: Segment{Bridge: bridge, VLAN: vlan, AcknowledgedAt: t0, By: "cli"}}
}

func TestNodeSegmentsRoundTrip(t *testing.T) {
	s, p := openStore(t)
	got, err := s.NodeSegments()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)

	a := nodeSegment("pve-a", "vmbr0", 0)
	b := nodeSegment("pve-b-2", "vmbr1", 20)
	require.NoError(t, s.SaveNodeSegment(a))
	require.NoError(t, s.SaveNodeSegment(b))
	require.Equal(t, "pve-a-vmbr0", a.ID())
	require.Equal(t, "pve-b-2-vmbr1.20", NodeSegmentID("pve-b-2", "vmbr1", 20))

	got, err = s.NodeSegments()
	require.NoError(t, err)
	require.Equal(t, map[string]NodeSegment{"pve-a-vmbr0": a, "pve-b-2-vmbr1.20": b}, got)
	require.Equal(t, []string{"nodesegments/pve-a-vmbr0.json", "nodesegments/pve-b-2-vmbr1.20.json"}, stored(t, p.Cluster),
		"beside the old kind, which the previous release lists")

	require.NoError(t, s.DeleteNodeSegment("pve-a-vmbr0"))
	got, err = s.NodeSegments()
	require.NoError(t, err)
	require.Equal(t, map[string]NodeSegment{"pve-b-2-vmbr1.20": b}, got)
}

// A node name may hold a dash, and a bridge too: the id of one segment can be
// the id of another, and the second is refused rather than taking its file.
func TestANodeSegmentWhoseIDIsTakenByAnotherIsRefused(t *testing.T) {
	s, _ := openStore(t)
	require.NoError(t, s.SaveNodeSegment(nodeSegment("pve-a", "vmbr0", 0)))

	err := s.SaveNodeSegment(nodeSegment("pve", "a-vmbr0", 0))

	require.ErrorContains(t, err, "pve-a-vmbr0")
	got, err := s.NodeSegments()
	require.NoError(t, err)
	require.Equal(t, "pve-a", got["pve-a-vmbr0"].Node)
}

func TestSaveNodeSegmentRefusesWhatIsNoSegmentOfANode(t *testing.T) {
	for _, tt := range []struct {
		name string
		seg  NodeSegment
		want string
	}{
		{"no node", nodeSegment("", "vmbr0", 0), "node"},
		{"a node with a dot", nodeSegment("pve.lan", "vmbr0", 0), `"pve.lan"`},
		{"no bridge", nodeSegment("pve1", "", 0), "bridge"},
		{"VLAN 4095", nodeSegment("pve1", "vmbr0", 4095), "VLAN 4095"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, p := openStore(t)
			require.ErrorContains(t, s.SaveNodeSegment(tt.seg), tt.want)
			require.Empty(t, stored(t, p.Cluster))
		})
	}
}

// Until the upgrade is finished, an acknowledgement the previous release
// made counts on every node.
func TestAnOldAcknowledgementCountsOnEveryNode(t *testing.T) {
	s, _ := openStore(t)
	old := Segment{Bridge: "vmbr0", AcknowledgedAt: t0, By: "cli"}
	require.NoError(t, s.SaveSegment(old))
	own := nodeSegment("pve-a", "vmbr1", 20)
	require.NoError(t, s.SaveNodeSegment(own))

	onA, err := s.SegmentsOn("pve-a")
	require.NoError(t, err)
	require.Equal(t, map[string]Segment{"vmbr0": old, "vmbr1.20": own.Segment}, onA)
	onB, err := s.SegmentsOn("pve-b")
	require.NoError(t, err)
	require.Equal(t, map[string]Segment{"vmbr0": old}, onB)
}

func TestRevokingOnOneNodeTakesTheOldAcknowledgementAwayButKeepsTheOthers(t *testing.T) {
	s, p := openStore(t)
	for _, n := range []string{"pve-a", "pve-b", "pve-c"} {
		require.NoError(t, s.SaveNode(NodeEntry{Name: n, Version: "1.1.0", Since: t0}))
	}
	gone := NodeEntry{Name: "pve-d", Version: "1.1.0", Since: t0, State: "removed"}
	require.NoError(t, s.SaveNode(gone))
	old := Segment{Bridge: "vmbr0", AcknowledgedAt: t0, By: "cli"}
	require.NoError(t, s.SaveSegment(old))
	ownC := nodeSegment("pve-c", "vmbr0", 0)
	ownC.AcknowledgedAt = t0.Add(time.Hour)
	require.NoError(t, s.SaveNodeSegment(ownC))

	require.NoError(t, s.RevokeNodeSegment("pve-a", "vmbr0", 0))

	segs, err := s.Segments()
	require.NoError(t, err)
	require.Empty(t, segs, "the old object would keep vmbr0 acknowledged on pve-a")
	onA, err := s.SegmentsOn("pve-a")
	require.NoError(t, err)
	require.Empty(t, onA)
	onB, err := s.SegmentsOn("pve-b")
	require.NoError(t, err)
	require.Equal(t, map[string]Segment{"vmbr0": old}, onB, "pve-b keeps what the old object gave it")
	onC, err := s.SegmentsOn("pve-c")
	require.NoError(t, err)
	require.Equal(t, map[string]Segment{"vmbr0": ownC.Segment}, onC, "pve-c had its own")
	require.NoFileExists(t, filepath.Join(p.Cluster, "nodesegments", "pve-d-vmbr0.json"), "a removed node gets none")

	require.NoError(t, s.RevokeNodeSegment("pve-a", "vmbr0", 0), "revoking again changes nothing")
	require.NoError(t, s.RevokeNodeSegment("pve-b", "vmbr0", 0))
	onB, err = s.SegmentsOn("pve-b")
	require.NoError(t, err)
	require.Empty(t, onB)
}
