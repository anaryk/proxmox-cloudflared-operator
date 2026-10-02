package daemon

import (
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
