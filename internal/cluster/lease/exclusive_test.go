package lease

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcessLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "lease.lock")
	lock := ProcessLock(path)

	release, err := lock()
	require.NoError(t, err)
	info, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	_, err = lock()
	require.ErrorIs(t, err, ErrRunning, "a second holder on the node")
	require.ErrorContains(t, err, path)

	release()
	release2, err := lock()
	require.NoError(t, err, "free again once the first let go")
	release2()
}
