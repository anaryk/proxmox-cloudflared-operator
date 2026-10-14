package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var thisCluster = store.ClusterIdentity{CASHA256: "e202df7ba95410b98a901e14d70a65fb868de5047c22f78d6f4d3c0d03a197b1", Name: "lab"}

// earlierStore is the store an earlier release made on one node: an install
// without the identity of its cluster, an entry without a state.
func earlierStore(t *testing.T) *store.Store {
	t.Helper()
	w := newWorld(t)
	require.NoError(t, w.store.SaveInstall(store.Install{ID: clusterInstall, CreatedAt: t0}))
	require.NoError(t, w.store.SaveNode(store.NodeEntry{Name: testNode, Since: t0, Version: "0.9.0"}))
	require.NoError(t, w.store.SaveWriter(planner.Writer{InstallID: clusterInstall, Generation: 4, Nonce: "n4"}))
	w.store.SetNode(testNode)
	return w.store
}

func firstStartOf(st *store.Store) *firstStart {
	return newFirstStart(st, testNode, "1.0.0", func() time.Time { return t0.Add(time.Hour) },
		func() (store.ClusterIdentity, error) { return thisCluster, nil }, zerolog.Nop())
}

func mayWrite() error { return nil }

// A build may be the controller only of nodes whose agents it can speak to:
// one that is neither down nor removed declared a range this build shares,
// or none.
func TestTheProtocolOfEveryNodeKeepsABuildFromTheLease(t *testing.T) {
	for _, tt := range []struct {
		name  string
		entry store.NodeEntry
		why   string
	}{
		{"shared", store.NodeEntry{Name: otherNode, Protocol: store.ProtocolRange{Min: 1, Max: 2}}, ""},
		{"none declared", store.NodeEntry{Name: otherNode}, ""},
		{"too new", store.NodeEntry{Name: otherNode, Protocol: store.ProtocolRange{Min: 3, Max: 4}},
			"node pve2 speaks the agent protocol 3 to 4, this build 1 to 1"},
		{"too new but down", store.NodeEntry{Name: otherNode, State: store.NodeDown, Protocol: store.ProtocolRange{Min: 3, Max: 4}}, ""},
		{"too new but removed", store.NodeEntry{Name: otherNode, State: store.NodeRemoved, Protocol: store.ProtocolRange{Min: 3, Max: 4}}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.why, protocolWhy([]store.NodeEntry{{Name: testNode}, tt.entry}))
		})
	}
}

// The first start of this release on a store of an earlier one records the
// unfinished upgrade from the release the node ran, the identity of the
// cluster, the CA named in leader.json, the cluster copies, and index 0 and
// the active state of the node, each once.
func TestTheFirstStartOfThisReleaseUpgradesTheStoreOnce(t *testing.T) {
	st := earlierStore(t)
	f := firstStartOf(st)

	f.try(mayWrite)

	u, found, err := st.UpgradeState()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "0.9.0", u.From)
	inst, _, err := st.Install()
	require.NoError(t, err)
	require.Equal(t, &thisCluster, inst.Cluster)
	ca, _, _, err := st.CA()
	require.NoError(t, err)
	require.NotEmpty(t, ca.CertPEM)
	rec, _, _, err := st.Leader()
	require.NoError(t, err)
	require.NotEmpty(t, rec.CASHA256)
	n := entryIn(t, st, testNode)
	require.Equal(t, store.NodeActive, n.State)
	require.Equal(t, 0, *n.Index)
	require.Equal(t, t0.Add(time.Hour), n.StateSince)
	require.True(t, f.done)

	again := firstStartOf(st)
	require.Empty(t, again.upgradeFrom, "the store is of this release now")
	again.try(mayWrite)
	ca2, _, _, err := st.CA()
	require.NoError(t, err)
	require.Equal(t, ca.CertPEM, ca2.CertPEM, "the CA is made once")
	require.Equal(t, n, entryIn(t, st, testNode))
}

// A new install of this release records no upgrade.
func TestTheFirstStartOfANewInstallRecordsNoUpgrade(t *testing.T) {
	st := earlierStore(t)
	inst, _, err := st.Install()
	require.NoError(t, err)
	inst.Cluster = &thisCluster
	require.NoError(t, st.SaveInstall(inst))

	firstStartOf(st).try(mayWrite)

	_, found, err := st.UpgradeState()
	require.NoError(t, err)
	require.False(t, found)
}

// Nothing is written while this node may not write, and it is done once it
// may.
func TestTheFirstStartWaitsUntilTheNodeMayWrite(t *testing.T) {
	st := earlierStore(t)
	f := firstStartOf(st)

	f.try(func() error { return errors.New("not the holder") })
	_, found, err := st.UpgradeState()
	require.NoError(t, err)
	require.False(t, found)
	require.Empty(t, entryIn(t, st, testNode).State)

	f.try(mayWrite)
	require.Equal(t, store.NodeActive, entryIn(t, st, testNode).State)
}

// A node that joined before the first start does not take index 0 from the
// node the earlier release registered.
func TestTheNodeOfTheEarlierReleaseKeepsIndexZero(t *testing.T) {
	st := earlierStore(t)
	one := 1
	require.NoError(t, st.SaveNode(store.NodeEntry{Name: otherNode, Since: t0, State: store.NodeJoining, StateSince: t0, Index: &one}))

	firstStartOf(st).try(mayWrite)

	require.Equal(t, 0, *entryIn(t, st, testNode).Index)
}
