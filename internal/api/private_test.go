package api

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// openDirOf opens the directory of a test as the daemon holds its own.
func openDirOf(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func TestTheSocketIsPrivateUntilItIsReady(t *testing.T) {
	dir := pcoDir(t)
	final := filepath.Join(dir, "pco.sock")
	s := newServer(&fakeEngine{})

	p, err := s.bindPrivate(openDirOf(t, dir), dir, "pco.sock", os.Getgid())
	require.NoError(t, err)
	t.Cleanup(p.abandon)

	// Bound and set up, in a directory only its owner can enter and that is
	// named after the socket; nothing is at the public path yet.
	require.Equal(t, filepath.Join(dir, ".pco.sock.new"), p.dir)
	dirInfo, err := os.Lstat(p.dir)
	require.NoError(t, err)
	require.True(t, dirInfo.IsDir())
	require.Equal(t, fs.FileMode(0o700), dirInfo.Mode().Perm())
	sockInfo, err := os.Lstat(p.path)
	require.NoError(t, err)
	require.Equal(t, fs.ModeSocket, sockInfo.Mode().Type())
	require.Equal(t, fs.FileMode(0o660), sockInfo.Mode().Perm(), "the mode is final before the socket can be reached")
	_, err = os.Lstat(final)
	require.ErrorIs(t, err, fs.ErrNotExist)

	info, err := p.publish("pco.sock")
	require.NoError(t, err)

	pub, err := os.Lstat(final)
	require.NoError(t, err)
	require.True(t, os.SameFile(info, pub))
	require.Equal(t, fs.ModeSocket, pub.Mode().Type())
	require.Equal(t, fs.FileMode(0o660), pub.Mode().Perm())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the private directory is gone")
	require.Equal(t, "pco.sock", entries[0].Name())
}

func TestANeverPublishedSocketLeavesNothingBehind(t *testing.T) {
	dir := pcoDir(t)
	p, err := newServer(&fakeEngine{}).bindPrivate(openDirOf(t, dir), dir, "pco.sock", os.Getgid())
	require.NoError(t, err)

	p.abandon()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestASocketBoundElsewhereIsNoticed(t *testing.T) {
	// The listener binds by path. If a directory on the way to it was swapped
	// for another, the socket is made where the daemon did not look.
	ours, theirs := shortDir(t), shortDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(ours, "s"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(theirs, "s"), nil, 0o600))
	root := openDirOf(t, ours)

	require.NoError(t, checkBound(root, "s", filepath.Join(ours, "s")))
	require.ErrorContains(t, checkBound(root, "s", filepath.Join(theirs, "s")), "not in the directory")
	require.Error(t, checkBound(root, "missing", filepath.Join(ours, "s")))
	require.Error(t, checkBound(root, "s", filepath.Join(ours, "missing")))
}
