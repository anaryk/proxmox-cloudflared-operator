package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// lockKept makes the lock of the lease as the daemon of a node that stands
// alone leaves it when it stops: an empty directory.
func (e *testEnv) lockKept() {
	e.t.Helper()
	require.NoError(e.t, os.MkdirAll(e.s.host.leaseLock, 0o755))
}

// heldBy makes leader.json name node as the holder of the lease.
func (e *testEnv) heldBy(node string) {
	e.t.Helper()
	require.NoError(e.t, e.st.SaveLeader(store.LeaderRecord{
		Writer: planner.Writer{InstallID: testInstall, Generation: 2, Nonce: "abcd1234", Incarnation: node + "/boot-1"},
		Node:   node, AcquiredAt: t0,
	}))
}

func lockScript() [][]call {
	return [][]call{connectorsSeen(), egressSeen(), serviceStopped(), connectorsPruned(), egressDeleted()}
}

// The daemon of a node that stands alone keeps the lock when it stops, so
// that its next start keeps the epoch; an uninstall is no such stop, and a
// setup after it would wait two minutes for the lock to expire.
func TestUninstallRemovesTheLockOfANodeThatStandsAlone(t *testing.T) {
	for _, tt := range []struct {
		name  string
		named string // the node leader.json names; empty for none, as setup writes it
	}{
		{"leader.json names this node", testNode},
		{"leader.json names no node", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installed(Manifest{})
			if tt.named != "" {
				e.heldBy(tt.named)
			}
			e.lockKept()
			e.script(lockScript()...)

			require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}))
			e.done()

			require.NoDirExists(t, e.s.host.leaseLock)
			e.requireShown("lease: removed the lock " + e.s.host.leaseLock)
			e.requireStoreGone()
		})
	}
}

// A lock that leader.json gives to another node is another node's, whatever
// the members file says, and one with a file in it never expires and is never
// removed by pco; a node of a cluster leaves what its daemon left.
func TestUninstallLeavesALockThatIsNotItsOwn(t *testing.T) {
	for _, tt := range []struct {
		name    string
		prepare func(e *testEnv)
		shown   string
	}{
		{"leader.json names another node", func(e *testEnv) { e.heldBy("pve2") }, ""},
		{"leader.json is of another install", func(e *testEnv) {
			require.NoError(e.t, e.st.SaveLeader(store.LeaderRecord{
				Writer: planner.Writer{InstallID: "ba9876543210", Generation: 2, Nonce: "abcd1234", Incarnation: testNode + "/boot-1"},
				Node:   testNode, AcquiredAt: t0,
			}))
		}, ""},
		{"leader.json cannot be read", func(e *testEnv) {
			require.NoError(e.t, os.WriteFile(filepath.Join(e.paths.Cluster, "meta", "leader.json"), []byte("{not json"), 0o600))
		}, "warn: lease: leader.json cannot be read"},
		{"a file in the lock", func(e *testEnv) {
			require.NoError(e.t, os.WriteFile(filepath.Join(e.s.host.leaseLock, "stray"), nil, 0o644))
		}, "warn: lease: the lock %s holds a file, so it is kept"},
		{"a node of a cluster", func(e *testEnv) { e.clusterOf("pve2") }, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installed(Manifest{})
			e.lockKept()
			tt.prepare(e)
			e.script(lockScript()...)

			require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}))
			e.done()

			require.DirExists(t, e.s.host.leaseLock)
			require.NotContains(t, e.ask.text(), "lease: removed")
			switch {
			case strings.Contains(tt.shown, "%s"):
				e.requireShown(fmt.Sprintf(tt.shown, e.s.host.leaseLock))
			case tt.shown != "":
				e.requireShown(tt.shown)
			}
			e.requireStoreGone()
		})
	}
}

// Without a lock nothing is said of it.
func TestUninstallSaysNothingOfALockThatIsNotThere(t *testing.T) {
	e := newTestEnv(t)
	e.installed(Manifest{})
	e.script(lockScript()...)

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}))
	e.done()

	require.NotContains(t, e.ask.text(), "lease:")
}
