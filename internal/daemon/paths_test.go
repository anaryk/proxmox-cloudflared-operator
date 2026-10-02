package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

func TestStorePathsAreTheNodesOrAStoreOfItsOwn(t *testing.T) {
	defaults := store.DefaultPaths()
	require.NotEmpty(t, defaults.MountCheck)

	got, err := StorePaths("", "", "")
	require.NoError(t, err)
	require.Equal(t, defaults, got)

	got, err = StorePaths("/tmp/c", "/tmp/p", "/tmp/l")
	require.NoError(t, err)
	require.Equal(t, store.Paths{Cluster: "/tmp/c", Private: "/tmp/p", Local: "/tmp/l"}, got,
		"nothing is mounted behind the directories of a test")
}

// The lock of a node is in its local directory, the tokens in the private one
// and the connectors are kept by what the cluster one says. Moving some of
// them would let a daemon reconcile a store a second time, or mix a store of
// a test with the tokens and the connector directory of the node.
func TestTheDirectoriesMoveAllThreeOrNone(t *testing.T) {
	for _, tt := range []struct {
		name                    string
		cluster, private, local string
	}{
		{"the local one alone", "", "", "/tmp/l"},
		{"the cluster one alone", "/tmp/c", "", ""},
		{"the private one alone", "", "/tmp/p", ""},
		{"the cluster and the private ones", "/tmp/c", "/tmp/p", ""},
		{"the cluster and the local ones", "/tmp/c", "", "/tmp/l"},
		{"the private and the local ones", "", "/tmp/p", "/tmp/l"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := StorePaths(tt.cluster, tt.private, tt.local)

			require.EqualError(t, err, "--cluster-dir, --private-dir and --local-dir are given all three or none: "+
				"a daemon that kept some of the directories of the node would reconcile its store a second time, "+
				"or mix a store of its own with the tokens and the connectors of the node")
		})
	}
}

func TestTheNodeLockIsExclusiveAndGoesWithItsHolder(t *testing.T) {
	dir := filepath.Join(testutil.ShortDir(t), "local") // made by the lock

	lock, err := lockNode(dir)
	require.NoError(t, err)
	_, err = lockNode(dir)
	require.ErrorIs(t, err, ErrRunning)

	lock.release()
	again, err := lockNode(dir)
	require.NoError(t, err)
	again.release()
	require.FileExists(t, store.Paths{Local: dir}.NodeLock(), "the lock file stays")
}

// The lock holds only while its file is the one at its path: once the file
// is removed or replaced, a second daemon would get a lock of its own.
func TestTheNodeLockSaysWhetherItStillHolds(t *testing.T) {
	t.Run("held", func(t *testing.T) {
		lock, err := lockNode(testutil.ShortDir(t))
		require.NoError(t, err)
		t.Cleanup(lock.release)

		require.NoError(t, lock.check())
	})
	t.Run("removed", func(t *testing.T) {
		dir := testutil.ShortDir(t)
		lock, err := lockNode(dir)
		require.NoError(t, err)
		t.Cleanup(lock.release)
		require.NoError(t, os.Remove(store.Paths{Local: dir}.NodeLock()))

		require.ErrorContains(t, lock.check(), "is gone; a second daemon could start")
	})
	t.Run("replaced", func(t *testing.T) {
		dir := testutil.ShortDir(t)
		lock, err := lockNode(dir)
		require.NoError(t, err)
		t.Cleanup(lock.release)
		require.NoError(t, os.Remove(store.Paths{Local: dir}.NodeLock()))
		other, err := lockNode(dir)
		require.NoError(t, err, "a second daemon gets a lock of its own")
		t.Cleanup(other.release)

		require.EqualError(t, lock.check(), "the lock of the node "+store.Paths{Local: dir}.NodeLock()+
			" is not the file this daemon locked; a second daemon could start")
		require.NoError(t, other.check())
	})
}

func TestTheStoreIsReadyWhenItIsMountedAndSetUp(t *testing.T) {
	dir := testutil.ShortDir(t)
	mount := filepath.Join(dir, "mounted")
	require.NoError(t, os.WriteFile(mount, nil, 0o600))
	paths := store.Paths{
		Cluster: filepath.Join(dir, "cluster"), Private: filepath.Join(dir, "private"), Local: filepath.Join(dir, "local"), MountCheck: mount,
	}
	s, err := store.Open(paths)
	require.NoError(t, err)
	ready := storeReady(s)

	require.ErrorIs(t, ready(), store.ErrNoRoot, "no roots yet")
	require.NoError(t, s.Init())
	require.EqualError(t, ready(), "pco is not set up on this node; run pco setup")
	require.NoError(t, s.SaveInstall(store.Install{ID: "abc123"}))
	require.NoError(t, ready())
	require.NoError(t, os.Remove(mount))
	require.ErrorIs(t, ready(), store.ErrNotMounted)
}
