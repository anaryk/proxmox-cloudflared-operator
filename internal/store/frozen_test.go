package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// raiseSchema makes this build write and read the given global schema
// version for the rest of the test, as a later release would.
func raiseSchema(t *testing.T, version int) {
	t.Helper()
	was := schemaVersion
	schemaVersion = version
	t.Cleanup(func() { schemaVersion = was })
}

func someLeader() LeaderRecord {
	return LeaderRecord{
		Writer:     planner.Writer{InstallID: "0123456789ab", Generation: 4, Nonce: "n0nce", Incarnation: "pve1/boot-1"},
		Node:       "pve1",
		AcquiredAt: t0,
		CASHA256:   sha("ca"),
	}
}

func TestTheFrozenObjectsKeepSchemaOneWhateverTheGlobalVersion(t *testing.T) {
	s, p := openStore(t)
	raiseSchema(t, 2)
	leader := someLeader()
	node := NodeEntry{Name: "pve-a", Version: "1.0.0", Since: t0}

	require.NoError(t, s.SaveLeader(leader))
	require.NoError(t, s.SaveNode(node))
	require.NoError(t, s.SaveSettings(DefaultSettings()))

	require.Equal(t, 1, readRaw(t, filepath.Join(p.Cluster, "meta", "leader.json")).SchemaVersion)
	require.Equal(t, 1, readRaw(t, filepath.Join(p.Cluster, "nodes", "pve-a.json")).SchemaVersion)
	require.Equal(t, 2, readRaw(t, filepath.Join(p.Cluster, "meta", "settings.json")).SchemaVersion,
		"every other object has the global version")

	got, rev, found, err := s.Leader()
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 1, rev)
	require.Equal(t, leader, got)
	w, found, err := s.Writer()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, leader.Writer, w)
	nodes, err := s.Nodes()
	require.NoError(t, err)
	require.Equal(t, []NodeEntry{node}, nodes)

	version, path, err := NewestSchema(p)
	require.NoError(t, err)
	require.Equal(t, 2, version)
	require.Equal(t, filepath.Join(p.Cluster, "meta", "settings.json"), path)
}

func TestFrozenObjectsAnEarlierReleaseWroteAreReadUnderAHigherGlobalVersion(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "meta", "leader.json"),
		envelopeJSON("leader", `{"installId":"0123456789ab","generation":2,"nonce":"abc"}`))
	writeFile(t, filepath.Join(p.Cluster, "nodes", "pve1.json"),
		envelopeJSON("pve1", `{"name":"pve1","version":"0.9.0","since":"2026-10-01T12:00:00Z"}`))
	raiseSchema(t, 3)

	got, _, found, err := s.Leader()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, LeaderRecord{Writer: planner.Writer{InstallID: "0123456789ab", Generation: 2, Nonce: "abc"}}, got)
	nodes, err := s.Nodes()
	require.NoError(t, err)
	require.Equal(t, []NodeEntry{{Name: "pve1", Version: "0.9.0", Since: t0}}, nodes)

	require.NoError(t, s.SaveNode(NodeEntry{Name: "pve1", Version: "1.0.0", Since: t0}))
	f := readRaw(t, filepath.Join(p.Cluster, "nodes", "pve1.json"))
	require.Equal(t, 1, f.SchemaVersion)
	require.EqualValues(t, 2, f.Rev)

	version, path, err := NewestSchema(p)
	require.NoError(t, err)
	require.Zero(t, version, "the frozen objects say nothing about the schema of the store")
	require.Empty(t, path)
}

func TestAFrozenObjectOfAHigherSchemaIsStillRefused(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "nodes", "pve1.json"),
		`{"schemaVersion":2,"rev":1,"id":"pve1","data":{"name":"pve1","version":"9","since":"2026-10-01T12:00:00Z"}}`)

	_, err := s.Nodes()

	require.ErrorContains(t, err, "schema version 2")
}

func TestOnlyTheLeaderIsFrozenInMeta(t *testing.T) {
	require.True(t, frozen(kindMeta, "leader"))
	require.True(t, frozen(kindNodes, "pve1"))
	require.True(t, frozen(kindNodes, "pve-a"))
	require.False(t, frozen(kindMeta, "settings"))
	require.False(t, frozen(kindMeta, "install"))
	require.False(t, frozen(kindRoutes, "leader"))
}
