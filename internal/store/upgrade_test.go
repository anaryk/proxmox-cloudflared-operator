package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTheUnfinishedUpgradeIsKept(t *testing.T) {
	s, p := openStore(t)
	_, found, err := s.UpgradeState()
	require.NoError(t, err)
	require.False(t, found)

	u := UpgradeState{From: "1.0.3", Since: t0}
	require.NoError(t, s.SaveUpgradeState(u))
	got, found, err := s.UpgradeState()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, u, got)
	require.Equal(t, []string{"meta/upgrade.json"}, stored(t, p.Cluster))

	require.ErrorContains(t, s.SaveUpgradeState(UpgradeState{Since: t0}), "release")
	require.ErrorContains(t, s.SaveUpgradeState(UpgradeState{From: "1.0.3"}), "since")

	require.NoError(t, s.DeleteUpgradeState())
	_, found, err = s.UpgradeState()
	require.NoError(t, err)
	require.False(t, found)
}

func TestBackupLocalCopiesTheSharedRoots(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.SaveInstall(Install{ID: "0123456789ab", CreatedAt: t0}))
	require.NoError(t, s.SaveSegment(Segment{Bridge: "vmbr0", AcknowledgedAt: t0}))
	require.NoError(t, s.SaveJoinToken(joinRecord("t1", "pve-b")))
	require.NoError(t, s.SaveNodeAddrs(nil))
	writeFile(t, filepath.Join(p.Cluster, "meta", ".settings.json.1"+tempExt), "{")
	dir := filepath.Join(t.TempDir(), "backup", "1-20261001T120000Z")

	require.NoError(t, s.BackupLocal(dir))

	require.Equal(t, []string{
		"cluster/meta/install.json",
		"cluster/segments/vmbr0.json",
		"private/jointokens/t1.json",
	}, stored(t, dir), "the shared roots without what is not an object; the local root stays where it is")
	for _, f := range []string{"meta/install.json", "segments/vmbr0.json"} {
		want, err := os.ReadFile(filepath.Join(p.Cluster, f))
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(dir, "cluster", f))
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	info, err := os.Stat(filepath.Join(dir, "private", "jointokens", "t1.json"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	require.ErrorContains(t, s.BackupLocal(dir), "exists", "a backup is never mixed into another")
}
