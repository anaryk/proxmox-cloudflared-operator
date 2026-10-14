package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The upgrade is finished only once every registered node that is not
// removed runs a build that reads this release's store; a node that is
// down counts, and is forgotten first. The store is copied below the local
// root before the record of the upgrade goes.
func TestFinishUpgradeRefusesAnOlderBuildAndBacksTheStoreUp(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	require.NoError(t, w.store.SaveUpgradeState(store.UpgradeState{From: "0.9.0", Since: t0}))
	schema := func(name string, v int, state string) {
		n := entryOf(t, w.env, name)
		n.Schema, n.State = v, state
		require.NoError(t, w.store.SaveNode(n))
	}
	schema(testNode, store.SchemaVersion(), store.NodeActive)
	schema("pve2", store.SchemaVersion(), store.NodeActive)
	schema("pve3", 0, store.NodeDown)
	*w.listed = []members.Member{{Name: "pve1", ID: 1, Online: true}, {Name: "pve2", ID: 2, Online: true}, {Name: "pve3", ID: 3, Online: false}}
	w.acts["pve3"].silent = true

	st := w.tick(time.Second)
	require.Equal(t, &UpgradeView{From: "0.9.0", Since: t0, Older: []string{"pve3"}}, st.Cluster.Upgrade)

	_, err := w.eng.FinishUpgrade(t.Context())
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "1 node runs a build that does not read store schema")
	require.ErrorContains(t, err, "pve3 (an earlier release, without a store schema), which is down; a node that is down is forgotten first (pco node forget)")
	_, found, err := w.store.UpgradeState()
	require.NoError(t, err)
	require.True(t, found, "nothing changed")

	_, err = w.eng.ForgetNode(t.Context(), "pve3", false)
	require.NoError(t, err)
	res, err := w.eng.FinishUpgrade(t.Context())
	require.NoError(t, err)
	require.Equal(t, "0.9.0", res.From)
	require.Equal(t, store.SchemaVersion(), res.Schema)
	require.Equal(t, filepath.Join(w.paths.Local, "backup", "1-"+w.clock.now().UTC().Format("20060102T150405Z")), res.Backup)
	entries, err := os.ReadDir(filepath.Join(res.Backup, "cluster", "nodes"))
	require.NoError(t, err)
	require.Len(t, entries, 3, "the backup holds the registry")
	_, found, err = w.store.UpgradeState()
	require.NoError(t, err)
	require.False(t, found)
	st = w.tick(time.Second)
	require.Nil(t, st.Cluster.Upgrade)

	_, err = w.eng.FinishUpgrade(t.Context())
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "no upgrade from an earlier release is unfinished")
}
