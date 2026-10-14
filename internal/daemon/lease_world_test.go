package daemon

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// dirFS is the lock of the lease in a plain directory, as a standalone node
// in local mode keeps it for a daemon of the tests: a mkdir, the mtime, a
// rmdir. It counts what it is asked.
type dirFS struct {
	mu    sync.Mutex
	calls map[string]int
}

func (f *dirFS) count(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[op]++
}

func (f *dirFS) asked(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

func (f *dirFS) Mkdir(path string) error {
	f.count("mkdir")
	return os.Mkdir(path, 0o755)
}

func (f *dirFS) Refresh(path string, mtime time.Time) error {
	f.count("refresh")
	return os.Chtimes(path, time.Unix(0, 0), mtime)
}

func (f *dirFS) UnlockRequest(path string) error {
	f.count("unlock")
	return os.Chtimes(path, time.Unix(0, 0), time.Unix(0, 0))
}

func (f *dirFS) Rmdir(path string) error {
	f.count("rmdir")
	return os.Remove(path)
}

// standaloneLease is the cluster filesystem of a node that stands alone, in
// dir: the members file without a cluster, the CA of the node, the
// directory of the locks. The lock of the daemon's lease goes there.
func (w *world) standaloneLease(t *testing.T, dir string) {
	t.Helper()
	pve := filepath.Join(dir, "pve")
	require.NoError(t, os.MkdirAll(filepath.Join(pve, "priv", "lock"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(pve, ".members"),
		[]byte(`{"nodename": "`+testNode+`", "version": 0}`), 0o600))
	ca, err := os.ReadFile(w.pve.caFile(t, dir))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(pve, "pve-root-ca.pem"), ca, 0o600))
	w.leaseFS = &dirFS{}
	w.deps.Lease = LeaseDeps{
		FS:          w.leaseFS,
		BootID:      func() (string, error) { return "boot-1", nil },
		PVEDir:      pve,
		ProcessLock: filepath.Join(dir, "lease.lock"),
	}
	w.lockDir = filepath.Join(pve, "priv", "lock", "pco-leader")
}
