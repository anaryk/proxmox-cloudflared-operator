package lease

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// The errno of each call reaches the keeper as pmxcfs's answer would, on a
// filesystem in memory as pmxcfs is.
func TestSystemFSLinux(t *testing.T) {
	dir := scratch(t)
	lock := filepath.Join(dir, "pco-leader")
	f := SystemFS()

	require.ErrorIs(t, f.Mkdir(filepath.Join(dir, "missing", "pco-leader")), syscall.ENOENT, "never the parents")
	require.NoError(t, f.Mkdir(lock))
	err := f.Mkdir(lock)
	require.ErrorIs(t, err, syscall.EEXIST)
	require.ErrorContains(t, err, lock)

	require.NoError(t, f.Refresh(lock, time.Unix(1_800_000_000, 999)))
	atime, mtime := times(t, lock)
	require.Equal(t, unix.Timespec{}, atime)
	require.Equal(t, unix.Timespec{Sec: 1_800_000_000}, mtime, "whole seconds, which pmxcfs reads")

	require.NoError(t, f.UnlockRequest(lock))
	atime, mtime = times(t, lock)
	require.Equal(t, unix.Timespec{}, atime)
	require.Equal(t, unix.Timespec{}, mtime)

	file := filepath.Join(lock, "file")
	n, err := f.Entries(lock)
	require.NoError(t, err)
	require.Zero(t, n)
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	n, err = f.Entries(lock)
	require.NoError(t, err)
	require.Equal(t, 1, n, "a file in the lock is seen")
	require.ErrorIs(t, f.Rmdir(lock), syscall.ENOTEMPTY)
	require.NoError(t, os.Remove(file))
	require.NoError(t, f.Rmdir(lock))

	require.ErrorIs(t, f.Rmdir(lock), syscall.ENOENT)
	require.ErrorIs(t, f.Refresh(lock, time.Unix(1_800_000_000, 0)), syscall.ENOENT)
	require.ErrorIs(t, f.UnlockRequest(lock), syscall.ENOENT)

	if os.Geteuid() != 0 {
		require.NoError(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		require.ErrorIs(t, f.Mkdir(lock), syscall.EACCES)
	}
}

func TestSystemClocksLinux(t *testing.T) {
	c := SystemClocks()
	mono, boot := c.Monotonic(), c.Boottime()
	require.Positive(t, mono)
	require.GreaterOrEqual(t, boot, mono, "boot time also counts the suspends")
	require.GreaterOrEqual(t, c.Monotonic(), mono)
	require.WithinDuration(t, time.Now(), c.Now(), time.Minute)
}

// scratch is a directory on a tmpfs where there is one.
func scratch(t *testing.T) string {
	if dir, err := os.MkdirTemp("/dev/shm", "lease-"); err == nil {
		t.Cleanup(func() {
			_ = os.Chmod(dir, 0o700)
			_ = os.RemoveAll(dir)
		})
		return dir
	}
	return t.TempDir()
}

func times(t *testing.T, path string) (atime, mtime unix.Timespec) {
	t.Helper()
	var st unix.Stat_t
	require.NoError(t, unix.Stat(path, &st))
	return st.Atim, st.Mtim
}
