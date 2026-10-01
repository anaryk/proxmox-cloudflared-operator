package api

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
)

func TestAStaleSocketIsReplaced(t *testing.T) {
	socket := filepath.Join(shortDir(t), "pco.sock")
	old, err := net.Listen("unix", socket)
	require.NoError(t, err)
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, old.Close())
	info, err := os.Lstat(socket)
	require.NoError(t, err, "the dead listener leaves its file behind")
	require.Equal(t, fs.ModeSocket, info.Mode().Type())

	serve(t, newServer(&fakeEngine{}), socket)

	v, err := apiclient.New(socket).Version(t.Context())
	require.NoError(t, err)
	require.Equal(t, "1.2.3", v)
}

func TestServeRefusesToRemoveWhatIsNotASocket(t *testing.T) {
	dir := shortDir(t)
	for name, create := range map[string]func(path string) error{
		"file":    func(path string) error { return os.WriteFile(path, []byte("keep me"), 0o600) },
		"symlink": func(path string) error { return os.Symlink(dir, path) },
		"dir":     func(path string) error { return os.Mkdir(path, 0o700) },
	} {
		t.Run(name, func(t *testing.T) {
			socket := filepath.Join(dir, name+".sock")
			require.NoError(t, create(socket))
			before, err := os.Lstat(socket)
			require.NoError(t, err)

			err = serveFails(t, newServer(&fakeEngine{}), socket)

			require.ErrorContains(t, err, "not a socket")
			after, err := os.Lstat(socket)
			require.NoError(t, err, "the path must still exist")
			require.True(t, os.SameFile(before, after))
		})
	}
}

func TestServeDoesNotStealASocketThatIsInUse(t *testing.T) {
	socket := filepath.Join(shortDir(t), "pco.sock")
	other, err := net.Listen("unix", socket) // not a pco daemon: it holds no lock
	require.NoError(t, err)
	defer func() { _ = other.Close() }()

	err = serveFails(t, newServer(&fakeEngine{}), socket)

	require.ErrorContains(t, err, "already listening")
	conn, err := net.Dial("unix", socket)
	require.NoError(t, err, "the other listener still owns the path")
	require.NoError(t, conn.Close())
}

func TestASecondInstanceIsRefused(t *testing.T) {
	socket := filepath.Join(shortDir(t), "pco.sock")
	stop := serve(t, newServer(&fakeEngine{}), socket)

	err := serveFails(t, newServer(&fakeEngine{}), socket)

	require.ErrorContains(t, err, "another pco instance is running")
	v, err := apiclient.New(socket).Version(t.Context())
	require.NoError(t, err, "the first instance still answers")
	require.Equal(t, "1.2.3", v)
	require.NoError(t, stop())
}

// lockHeldBy takes the lock of a socket path, as another instance would.
func lockHeldBy(t *testing.T, socket string) {
	t.Helper()
	f, err := os.OpenFile(socket+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
}

func TestAHeldLockKeepsServeFromTouchingTheSocket(t *testing.T) {
	dir := shortDir(t)
	socket := filepath.Join(dir, "pco.sock")
	// A socket the holder of the lock has just made: nobody listens on it yet
	// from where this process looks, but it must not be removed.
	stale, err := net.Listen("unix", socket)
	require.NoError(t, err)
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, stale.Close())
	before, err := os.Lstat(socket)
	require.NoError(t, err)
	lockHeldBy(t, socket)

	err = serveFails(t, newServer(&fakeEngine{}), socket)

	require.ErrorContains(t, err, "another pco instance is running")
	after, err := os.Lstat(socket)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "the socket must be left alone")
}

func TestTheLockIsReleasedWhenServeEnds(t *testing.T) {
	socket := filepath.Join(shortDir(t), "pco.sock")
	stop := serve(t, newServer(&fakeEngine{}), socket)
	require.NoError(t, stop())

	// The next instance can start, and so can anything else that takes the lock.
	serve(t, newServer(&fakeEngine{}), socket)
}

func TestShutdownRemovesOnlyItsOwnSocket(t *testing.T) {
	socket := filepath.Join(shortDir(t), "pco.sock")
	stop := serve(t, newServer(&fakeEngine{}), socket)
	// Someone takes the path over while the daemon runs.
	require.NoError(t, os.Remove(socket))
	other, err := net.Listen("unix", socket)
	require.NoError(t, err)
	defer func() { _ = other.Close() }()
	other.(*net.UnixListener).SetUnlinkOnClose(false)

	require.NoError(t, stop())

	info, err := os.Lstat(socket)
	require.NoError(t, err, "a socket that is not ours must stay")
	require.Equal(t, fs.ModeSocket, info.Mode().Type())
}

func TestAnExistingDirectoryIsBroughtTo0750(t *testing.T) {
	dir := shortDir(t)
	require.NoError(t, os.Chmod(dir, 0o777))

	serve(t, newServer(&fakeEngine{}), filepath.Join(dir, "pco.sock"))

	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o750), info.Mode().Perm())
}

func TestASymlinkInPlaceOfTheDirectoryIsRefused(t *testing.T) {
	target := shortDir(t)
	require.NoError(t, os.Chmod(target, 0o777))
	link := filepath.Join(shortDir(t), "link")
	require.NoError(t, os.Symlink(target, link))

	err := serveFails(t, newServer(&fakeEngine{}), filepath.Join(link, "pco.sock"))

	require.ErrorContains(t, err, "not a symlink")
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o777), info.Mode().Perm(), "the directory behind the link must not be touched")
	entries, err := os.ReadDir(target)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestARelativePathIsRefused(t *testing.T) {
	// Where a mistake would change the mode of the working directory, it is a
	// directory of the test's own.
	dir := shortDir(t)
	require.NoError(t, os.Chmod(dir, 0o777))
	t.Chdir(dir)

	err := serveFails(t, newServer(&fakeEngine{}), "pco.sock")

	require.ErrorContains(t, err, "must be absolute")
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o777), info.Mode().Perm(), "the working directory must not be touched")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestTheSocketIsPrivateUntilItIsReady(t *testing.T) {
	dir := shortDir(t)
	final := filepath.Join(dir, "pco.sock")
	s := newServer(&fakeEngine{})

	p, err := s.bindPrivate(dir, os.Getgid())
	require.NoError(t, err)
	t.Cleanup(p.abandon)

	// Bound and set up, in a directory only its owner can enter; nothing is
	// at the public path yet.
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

	info, err := p.publish(final)
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
	dir := shortDir(t)
	p, err := newServer(&fakeEngine{}).bindPrivate(dir, os.Getgid())
	require.NoError(t, err)

	p.abandon()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestTheDirectoryAndTheSocketGoToTheGroup(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("only root may hand files to another group")
	}
	const gid = 4242
	owner := func(t *testing.T, path string) (uid, group uint32) {
		t.Helper()
		info, err := os.Lstat(path)
		require.NoError(t, err)
		st, ok := info.Sys().(*syscall.Stat_t)
		require.True(t, ok)
		return st.Uid, st.Gid
	}

	t.Run("a new directory", func(t *testing.T) {
		dir := filepath.Join(shortDir(t), "run")
		serveAs(t, newServer(&fakeEngine{}), filepath.Join(dir, "pco.sock"), gid)

		for _, path := range []string{dir, filepath.Join(dir, "pco.sock")} {
			u, g := owner(t, path)
			require.Equal(t, uint32(0), u, path)
			require.Equal(t, uint32(gid), g, path)
		}
	})

	t.Run("an existing directory", func(t *testing.T) {
		dir := shortDir(t)
		serveAs(t, newServer(&fakeEngine{}), filepath.Join(dir, "pco.sock"), gid)

		u, g := owner(t, dir)
		require.Equal(t, uint32(0), u)
		require.Equal(t, uint32(gid), g)
	})
}
