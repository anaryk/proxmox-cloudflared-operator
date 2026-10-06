package store

import (
	"encoding/json"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func intp(v int) *int    { return &v }
func boolp(v bool) *bool { return &v }

func clusterNode(name string, index int) NodeEntry {
	return NodeEntry{
		Name:               name,
		Version:            "1.1.0",
		Since:              t0,
		Index:              intp(index),
		Kind:               "host",
		AgentAddress:       netip.MustParseAddr("10.99.0.2"),
		Protocol:           ProtocolRange{Min: 1, Max: 1},
		Schema:             1,
		CertSerial:         "1f",
		CertSHA256:         sha("cert of " + name),
		State:              "joining",
		StateSince:         t0.Add(time.Minute),
		ControllerEligible: boolp(true),
		ContestedUses: []JoinUse{
			{At: t0, Source: netip.MustParseAddr("10.0.0.9"), VMID: 9001, MACs: []string{"bc:24:11:00:00:01"}},
		},
	}
}

func TestANodeEntryOfTheClusterRoundTrips(t *testing.T) {
	s, p := openStore(t)
	a, b := clusterNode("pve-a", 0), clusterNode("pve-b-2", 3)
	b.Kind, b.VMID, b.AgentAddress = "appliance", 9101, netip.Addr{}
	require.NoError(t, s.SaveNode(a))
	require.NoError(t, s.SaveNode(b))

	got, err := s.Nodes()
	require.NoError(t, err)
	require.Equal(t, []NodeEntry{a, b}, got)
	require.Equal(t, []string{"nodes/pve-a.json", "nodes/pve-b-2.json"}, stored(t, p.Cluster))

	withRev, err := s.NodesRev()
	require.NoError(t, err)
	require.Equal(t, []NodeRev{{Node: a, Rev: 1}, {Node: b, Rev: 1}}, withRev)
}

// The previous release decodes a node leniently and writes one without the
// fields of the cluster: neither stops the other.
func TestANodeEntryOfThePreviousReleaseReadsAndIsRead(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "nodes", "pve1.json"),
		envelopeJSON("pve1", `{"name":"pve1","version":"0.9.0","since":"2026-10-01T12:00:00Z"}`))
	got, err := s.Nodes()
	require.NoError(t, err)
	require.Equal(t, []NodeEntry{{Name: "pve1", Version: "0.9.0", Since: t0}}, got)

	require.NoError(t, s.SaveNode(clusterNode("pve1", 0)))
	var previous struct {
		Name    string    `json:"name"`
		Version string    `json:"version"`
		Since   time.Time `json:"since"`
	}
	require.NoError(t, json.Unmarshal(readRaw(t, filepath.Join(p.Cluster, "nodes", "pve1.json")).Data, &previous))
	require.Equal(t, "pve1", previous.Name)

	b, err := json.Marshal(NodeEntry{Name: "pve1", Version: "0.9.0", Since: t0})
	require.NoError(t, err)
	require.JSONEq(t, `{"name":"pve1","version":"0.9.0","since":"2026-10-01T12:00:00Z"}`, string(b),
		"an entry without the new fields is what the previous release wrote")
}

func TestSaveNodeIfComparesAndSwaps(t *testing.T) {
	s, p := openStore(t)
	path := filepath.Join(p.Cluster, "nodes", "pve2.json")
	n := clusterNode("pve2", 1)

	rev, err := s.SaveNodeIf(n, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, rev)

	_, err = s.SaveNodeIf(n, 0)
	require.ErrorIs(t, err, ErrRevision, "two setups of one node: the second sees the first")

	n.State = "active"
	rev, err = s.SaveNodeIf(n, 1)
	require.NoError(t, err)
	require.EqualValues(t, 2, rev)

	stale := n
	stale.State = "draining"
	before := readRaw(t, path)
	rev, err = s.SaveNodeIf(stale, 1)
	require.ErrorIs(t, err, ErrRevision)
	require.EqualValues(t, 2, rev, "the revision stored is told")
	require.Equal(t, before, readRaw(t, path))

	rev, err = s.SaveNodeIf(n, 2)
	require.NoError(t, err)
	require.EqualValues(t, 2, rev, "an unchanged entry is not written")
}

func TestSaveNodeRefusesAnIndexOutOfRange(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(*NodeEntry)
		want string
	}{
		{"no name", func(n *NodeEntry) { n.Name = "" }, "name"},
		{"a negative index", func(n *NodeEntry) { n.Index = intp(-1) }, "index -1"},
		{"the managed leg's index", func(n *NodeEntry) { n.Index = intp(255) }, "index 255"},
		{"a protocol range upside down", func(n *NodeEntry) { n.Protocol = ProtocolRange{Min: 2, Max: 1} }, "protocol"},
		{"a negative VMID", func(n *NodeEntry) { n.VMID = -1 }, "VMID"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, p := openStore(t)
			n := clusterNode("pve1", 0)
			tt.edit(&n)
			require.ErrorContains(t, s.SaveNode(n), tt.want)
			_, err := s.SaveNodeIf(n, 0)
			require.ErrorContains(t, err, tt.want)
			require.Empty(t, stored(t, p.Cluster))
		})
	}
	s, _ := openStore(t)
	require.NoError(t, s.SaveNode(clusterNode("pve1", 254)), "254 is the last index of a node")
}

func TestTheInstallRecordsItsCluster(t *testing.T) {
	s, p := openStore(t)
	i := Install{ID: "0123456789ab", CreatedAt: t0, Cluster: &ClusterIdentity{CASHA256: sha("pve-root-ca"), Name: "pco-lab"}}
	require.NoError(t, s.SaveInstall(i))

	got, found, err := s.Install()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, i, got)

	var wire map[string]any
	require.NoError(t, json.Unmarshal(readRaw(t, filepath.Join(p.Cluster, "meta", "install.json")).Data, &wire))
	require.Equal(t, map[string]any{"caSHA256": sha("pve-root-ca"), "name": "pco-lab"}, wire["cluster"])

	standalone := Install{ID: "0123456789ab", CreatedAt: t0, Cluster: &ClusterIdentity{CASHA256: sha("pve-root-ca")}}
	require.NoError(t, s.SaveInstall(standalone), "a standalone node records no name")

	bad := Install{ID: "0123456789ab", CreatedAt: t0, Cluster: &ClusterIdentity{CASHA256: "AB"}}
	require.ErrorContains(t, s.SaveInstall(bad), "caSHA256")
}
