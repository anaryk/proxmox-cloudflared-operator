package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// A node that stands alone keeps the lease as a node of a cluster does: the
// lock in the cluster filesystem, a new epoch at the first start, the lock
// given back as the daemon stops.
func TestTheDaemonOfAStandaloneNodeHoldsTheLease(t *testing.T) {
	w := newWorld(t)
	r := w.start()

	st := r.await(func(st engine.State) bool { return st.Lease != nil && st.Lease.Valid })

	require.Equal(t, testNode, st.Lease.Holder)
	require.Equal(t, 2, st.Lease.Generation, "one generation above the record setup wrote")
	require.DirExists(t, w.lockDir)
	rec, _, found, err := w.store.Leader()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, testNode, rec.Node)
	require.Equal(t, testNode+"/boot-1", rec.Incarnation)
	require.True(t, rec.ReleasedAt.IsZero())

	require.NoError(t, r.stop())

	require.NoDirExists(t, w.lockDir, "given back as the daemon stopped")
	rec, _, _, err = w.store.Leader()
	require.NoError(t, err)
	require.False(t, rec.ReleasedAt.IsZero())
	require.Equal(t, 2, rec.Generation)
}

// A daemon that ended without giving the lease back, as after kill -9, finds
// the lock of its node and boot still its own and keeps the epoch.
func TestADaemonThatDiedKeepsTheEpochInTheSameBoot(t *testing.T) {
	w := newWorld(t)
	rec, _, _, err := w.store.Leader()
	require.NoError(t, err)
	rec.Generation, rec.Nonce = 7, "kept0001"
	rec.Node, rec.Incarnation, rec.AcquiredAt = testNode, testNode+"/boot-1", time.Now().Add(-time.Hour)
	require.NoError(t, w.store.SaveLeader(rec))
	require.NoError(t, os.Mkdir(w.lockDir, 0o755))

	r := w.start()
	st := r.await(func(st engine.State) bool { return st.Lease != nil && st.Lease.Valid })

	require.Equal(t, 7, st.Lease.Generation)
	require.Equal(t, "kept0001", st.Lease.Nonce)
	require.Zero(t, w.leaseFS.asked("unlock"), "no unlock request for a lock of its own")
}

// The Cloudflare clients are made only while the lease is held and valid,
// so that a node that lost it, or is fenced, holds no token.
func TestTheCloudflareClientsComeOnlyWithAValidLease(t *testing.T) {
	made := 0
	client := &cffake.Fake{}
	factory := func(c store.Credential) (cfapi.API, error) {
		made++
		if c.ID == "" {
			return nil, errors.New("a credential without an id")
		}
		return client, nil
	}
	for name, tc := range map[string]struct {
		st   lease.State
		made bool
	}{
		"holder":     {lease.State{Holder: true, Valid: true}, true},
		"fenced":     {lease.State{Holder: true}, false},
		"not holder": {lease.State{Waiting: "the lock is held"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			made = 0
			_, err := holdingClients(factory, stateOf(tc.st))(store.Credential{ID: "c1"})
			require.Equal(t, tc.made, err == nil)
			require.Equal(t, tc.made, made == 1)
		})
	}
}

type stateOf lease.State

func (s stateOf) State() lease.State { return lease.State(s) }

func TestTheNodeIsEligibleAsTheRegistryAndTheStoreSay(t *testing.T) {
	no := false
	for name, tc := range map[string]struct {
		entry   *store.NodeEntry
		cluster *store.ClusterIdentity
		why     string
	}{
		"registered":         {entry: &store.NodeEntry{Name: testNode, Since: t0}},
		"not registered":     {why: "node " + testNode + " is not registered"},
		"draining":           {entry: &store.NodeEntry{Name: testNode, Since: t0, State: store.NodeDraining}, why: "node " + testNode + " is draining"},
		"set not to be":      {entry: &store.NodeEntry{Name: testNode, Since: t0, ControllerEligible: &no}, why: "node " + testNode + " was set not to be the controller"},
		"of another cluster": {entry: &store.NodeEntry{Name: testNode, Since: t0}, cluster: &store.ClusterIdentity{CASHA256: fingerprint("aa")}, why: "the store was made in another cluster"},
		"of this cluster":    {entry: &store.NodeEntry{Name: testNode, Since: t0}, cluster: &store.ClusterIdentity{CASHA256: fingerprint("bb")}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := store.Paths{Cluster: filepath.Join(dir, "c"), Private: filepath.Join(dir, "p"), Local: filepath.Join(dir, "l")}
			st, err := store.Open(p)
			require.NoError(t, err)
			require.NoError(t, st.Init())
			require.NoError(t, st.SaveInstall(store.Install{ID: testInstall, CreatedAt: t0, Cluster: tc.cluster}))
			if tc.entry != nil {
				require.NoError(t, st.SaveNode(*tc.entry))
			}
			here := func() (store.ClusterIdentity, error) { return store.ClusterIdentity{CASHA256: fingerprint("bb")}, nil }

			ok, why := eligibility(st, testNode, p, here, time.Now)()

			require.Equal(t, tc.why == "", ok, why)
			require.Contains(t, why, tc.why)
		})
	}
}

func fingerprint(b string) string {
	out := ""
	for range 32 {
		out += b[:2]
	}
	return out
}
