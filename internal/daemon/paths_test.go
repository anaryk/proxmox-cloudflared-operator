package daemon

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

func TestStorePathsClearTheMountCheckWhenTheClusterOrPrivateRootMoves(t *testing.T) {
	defaults := store.DefaultPaths()
	require.NotEmpty(t, defaults.MountCheck)

	for _, tt := range []struct {
		name                    string
		cluster, private, local string
		want                    store.Paths
	}{
		{"defaults", "", "", "", defaults},
		{
			"the cluster root moves", "/tmp/c", "", "",
			store.Paths{Cluster: "/tmp/c", Private: defaults.Private, Local: defaults.Local},
		},
		{
			"the private root moves", "", "/tmp/p", "",
			store.Paths{Cluster: defaults.Cluster, Private: "/tmp/p", Local: defaults.Local},
		},
		{
			"both move, the local root stays", "/tmp/c", "/tmp/p", "",
			store.Paths{Cluster: "/tmp/c", Private: "/tmp/p", Local: defaults.Local},
		},
		{
			"all move: a store of its own", "/tmp/c", "/tmp/p", "/tmp/l",
			store.Paths{Cluster: "/tmp/c", Private: "/tmp/p", Local: "/tmp/l"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := StorePaths(tt.cluster, tt.private, tt.local)

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// The lock of a node is in its local directory. A daemon with another local
// directory and the same cluster store would reconcile it a second time, so the
// local directory moves only with a store of its own.
func TestTheLocalDirectoryMovesOnlyWithAStoreOfItsOwn(t *testing.T) {
	for _, tt := range []struct {
		name                    string
		cluster, private, local string
	}{
		{"alone", "", "", "/tmp/l"},
		{"with the cluster directory only", "/tmp/c", "", "/tmp/l"},
		{"with the private directory only", "", "/tmp/p", "/tmp/l"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := StorePaths(tt.cluster, tt.private, tt.local)

			require.EqualError(t, err, "--local-dir is refused without --cluster-dir and --private-dir: "+
				"the lock of the node lives in the local directory, and a daemon with another one but the same "+
				"cluster store would reconcile it a second time; a separate store needs all three")
		})
	}
}

func TestTheNodeLockIsExclusiveAndGoesWithItsHolder(t *testing.T) {
	dir := filepath.Join(testutil.ShortDir(t), "local") // made by the lock

	release, err := lockNode(dir)
	require.NoError(t, err)
	_, err = lockNode(dir)
	require.ErrorIs(t, err, ErrRunning)

	release()
	again, err := lockNode(dir)
	require.NoError(t, err)
	again()
	require.FileExists(t, filepath.Join(dir, lockName), "the lock file stays")
}
