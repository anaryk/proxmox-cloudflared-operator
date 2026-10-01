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
	socket := filepath.Join(pcoDir(t), "pco.sock")
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
	dir := pcoDir(t)
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
	socket := filepath.Join(pcoDir(t), "pco.sock")
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
	socket := socketPath(t)
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
	dir := pcoDir(t)
	socket := filepath.Join(dir, "pco.sock")
	// A socket the holder of the lock has just made: nobody listens on it yet
	// from where this process looks, but it must not be removed. Nor must the
	// private directory it is making the next one in.
	stale, err := net.Listen("unix", socket)
	require.NoError(t, err)
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, stale.Close())
	before, err := os.Lstat(socket)
	require.NoError(t, err)
	private := filepath.Join(dir, ".pco.sock.new")
	require.NoError(t, os.Mkdir(private, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(private, "s"), nil, 0o600))
	lockHeldBy(t, socket)

	err = serveFails(t, newServer(&fakeEngine{}), socket)

	require.ErrorContains(t, err, "another pco instance is running")
	after, err := os.Lstat(socket)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "the socket must be left alone")
	require.FileExists(t, filepath.Join(private, "s"), "so must the private directory")
}

func TestTheLockIsReleasedWhenServeEnds(t *testing.T) {
	socket := socketPath(t)
	stop := serve(t, newServer(&fakeEngine{}), socket)
	require.NoError(t, stop())

	// The next instance can start, and so can anything else that takes the lock.
	serve(t, newServer(&fakeEngine{}), socket)
}

func TestShutdownRemovesOnlyItsOwnSocket(t *testing.T) {
	socket := socketPath(t)
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

func TestALeftoverPrivateDirectoryDoesNotStopServe(t *testing.T) {
	for _, tt := range []struct {
		name   string
		create func(t *testing.T, path string)
	}{
		{"a directory with files in it", func(t *testing.T, path string) {
			require.NoError(t, os.Mkdir(path, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(path, "s"), []byte("left by a crash"), 0o600))
			require.NoError(t, os.MkdirAll(filepath.Join(path, "deeper", "still"), 0o700))
		}},
		{"a file", func(t *testing.T, path string) {
			require.NoError(t, os.WriteFile(path, []byte("left by a crash"), 0o600))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := pcoDir(t)
			leftover := filepath.Join(dir, ".pco.sock.new")
			tt.create(t, leftover)

			serve(t, newServer(&fakeEngine{}), filepath.Join(dir, "pco.sock"))

			_, err := os.Lstat(leftover)
			require.ErrorIs(t, err, fs.ErrNotExist, "the leftover is removed")
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			require.ElementsMatch(t, []string{"pco.sock", "pco.sock.lock"}, names)
		})
	}
}

func TestAnExistingDirectoryIsBroughtTo0750(t *testing.T) {
	dir := pcoDir(t)
	require.NoError(t, os.Chmod(dir, 0o777))

	serve(t, newServer(&fakeEngine{}), filepath.Join(dir, "pco.sock"))

	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o750), info.Mode().Perm())
}

func TestOnlyADirectoryNamedPcoIsTouched(t *testing.T) {
	// /tmp and /run, and every other directory the system shares, must come out
	// of Serve as they went in.
	for _, name := range []string{"tmp", "run", "var", "PCO", "pco2", "pco.d", ".pco"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(shortDir(t), name)
			require.NoError(t, os.Mkdir(dir, 0o700))
			require.NoError(t, os.Chmod(dir, os.ModeSticky|0o777))
			before, err := os.Stat(dir)
			require.NoError(t, err)
			require.Equal(t, os.ModeSticky|0o777, before.Mode()&(os.ModeSticky|os.ModePerm), "set up as /tmp is")

			err = serveFails(t, newServer(&fakeEngine{}), filepath.Join(dir, "pco.sock"))

			require.ErrorContains(t, err, dir)
			require.ErrorContains(t, err, `must be named "pco"`)
			after, err := os.Stat(dir)
			require.NoError(t, err)
			require.Equal(t, before.Mode(), after.Mode(), "the mode must not change")
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, entries, "nothing may be put there")
		})
	}
}

func TestOnlyThePcoDirectoryIsEverMade(t *testing.T) {
	root := shortDir(t)

	err := serveFails(t, newServer(&fakeEngine{}), filepath.Join(root, "missing", "pco", "pco.sock"))

	require.ErrorContains(t, err, filepath.Join(root, "missing", "pco"))
	require.ErrorContains(t, err, "parent")
	_, statErr := os.Lstat(filepath.Join(root, "missing"))
	require.ErrorIs(t, statErr, fs.ErrNotExist, "no parent may be made")
}

func TestWhatIsNotARealDirectoryIsRefused(t *testing.T) {
	target := shortDir(t)
	require.NoError(t, os.Chmod(target, 0o777))
	for name, create := range map[string]func(path string) error{
		"a symlink to a directory": func(path string) error { return os.Symlink(target, path) },
		"a symlink to a sibling": func(path string) error {
			sibling := filepath.Join(filepath.Dir(path), "sibling")
			if err := os.Mkdir(sibling, 0o777); err != nil {
				return err
			}
			if err := os.Chmod(sibling, 0o777); err != nil {
				return err
			}
			return os.Symlink("sibling", path)
		},
		"a dangling symlink": func(path string) error { return os.Symlink(filepath.Join(target, "nothing"), path) },
		"a file":             func(path string) error { return os.WriteFile(path, []byte("keep me"), 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(shortDir(t), "pco")
			require.NoError(t, create(path))

			err := serveFails(t, newServer(&fakeEngine{}), filepath.Join(path, "pco.sock"))

			require.ErrorContains(t, err, path)
			require.ErrorContains(t, err, "real directory")
			info, err := os.Stat(target)
			require.NoError(t, err)
			require.Equal(t, fs.FileMode(0o777), info.Mode().Perm(), "what the link points to must not be touched")
			entries, err := os.ReadDir(target)
			require.NoError(t, err)
			require.Empty(t, entries)
			sibling := filepath.Join(filepath.Dir(path), "sibling")
			if _, err := os.Lstat(sibling); err == nil {
				info, err := os.Stat(sibling)
				require.NoError(t, err)
				require.Equal(t, fs.FileMode(0o777), info.Mode().Perm(), "nor the sibling")
				entries, err := os.ReadDir(sibling)
				require.NoError(t, err)
				require.Empty(t, entries)
			}
		})
	}
}

func TestADirectoryOfSomeoneElseIsRefused(t *testing.T) {
	needRoot(t, "only root may give a directory to another user")
	const other = 1234
	dir := pcoDir(t)
	require.NoError(t, os.Chown(dir, other, other))
	require.NoError(t, os.Chmod(dir, 0o777))

	err := serveFails(t, newServer(&fakeEngine{}), filepath.Join(dir, "pco.sock"))

	require.ErrorContains(t, err, dir)
	require.ErrorContains(t, err, "belongs to uid 1234")
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o777), info.Mode().Perm(), "the mode must not change")
	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	require.Equal(t, uint32(other), st.Uid, "the owner must not change")
	require.Equal(t, uint32(other), st.Gid)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "nothing may be put there")
}

func TestARelativePathIsRefused(t *testing.T) {
	// Where a mistake would change the mode of the working directory, it is a
	// directory of the test's own.
	dir := pcoDir(t)
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

func TestTheDirectoryAndTheSocketGoToTheGroup(t *testing.T) {
	needRoot(t, "only root may hand files to another group")
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
		socket := socketPath(t)
		serveAs(t, newServer(&fakeEngine{}), socket, gid)

		for _, path := range []string{filepath.Dir(socket), socket} {
			u, g := owner(t, path)
			require.Equal(t, uint32(0), u, path)
			require.Equal(t, uint32(gid), g, path)
		}
	})

	t.Run("an existing directory", func(t *testing.T) {
		dir := pcoDir(t)
		serveAs(t, newServer(&fakeEngine{}), filepath.Join(dir, "pco.sock"), gid)

		u, g := owner(t, dir)
		require.Equal(t, uint32(0), u)
		require.Equal(t, uint32(gid), g)
	})
}

// parentWith makes a directory of the test's own with a pco directory in it, and
// gives the parent the mode. The pco directory is 0700 and empty, so that a
// change to it shows.
func parentWith(t *testing.T, mode fs.FileMode) (parent, pco string) {
	t.Helper()
	parent = shortDir(t)
	pco = filepath.Join(parent, "pco")
	require.NoError(t, os.Mkdir(pco, 0o700))
	require.NoError(t, os.Chmod(parent, mode))
	return parent, pco
}

func TestAParentThatOthersCanWriteIsRefused(t *testing.T) {
	// Whoever can write to the parent can rename pco away and put one of their
	// own in its place, and the daemon would make its socket and its lock there.
	for _, tt := range []struct {
		name string
		mode fs.FileMode
	}{
		{"group can write", 0o775},
		{"only the group can write", 0o770},
		{"others can write", 0o757},
		{"only others can write", 0o702},
		{"everybody can write", 0o777},
		{"everybody can write, sticky as /tmp is", os.ModeSticky | 0o777},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parent, pco := parentWith(t, tt.mode)

			err := serveFails(t, newServer(&fakeEngine{}), filepath.Join(pco, "pco.sock"))

			require.ErrorContains(t, err, parent)
			require.ErrorContains(t, err, "writable")
			info, err := os.Stat(parent)
			require.NoError(t, err)
			require.Equal(t, tt.mode, info.Mode()&(os.ModeSticky|os.ModePerm), "the parent must not change")
			info, err = os.Stat(pco)
			require.NoError(t, err)
			require.Equal(t, fs.FileMode(0o700), info.Mode().Perm(), "pco must not be changed")
			entries, err := os.ReadDir(pco)
			require.NoError(t, err)
			require.Empty(t, entries, "nothing may be made in pco")
		})
	}
}

func TestAMissingPcoUnderAParentThatOthersCanWriteIsNotMade(t *testing.T) {
	parent := shortDir(t)
	require.NoError(t, os.Chmod(parent, os.ModeSticky|0o777))

	err := serveFails(t, newServer(&fakeEngine{}), filepath.Join(parent, "pco", "pco.sock"))

	require.ErrorContains(t, err, "writable")
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Empty(t, entries, "pco must not be made there")
}

func TestAParentThatOthersCannotWriteIsFine(t *testing.T) {
	for _, mode := range []fs.FileMode{0o755, 0o750, 0o700, 0o705} {
		t.Run(mode.String(), func(t *testing.T) {
			parent := shortDir(t)
			require.NoError(t, os.Chmod(parent, mode))
			socket := filepath.Join(parent, "pco", "pco.sock")

			serve(t, newServer(&fakeEngine{}), socket)

			v, err := apiclient.New(socket).Version(t.Context())
			require.NoError(t, err)
			require.Equal(t, "1.2.3", v)
		})
	}
}

func TestAParentThatIsASymlinkIsFollowed(t *testing.T) {
	// /var/run is a link to /run.
	target := shortDir(t)
	require.NoError(t, os.Chmod(target, 0o755))
	link := filepath.Join(shortDir(t), "run")
	require.NoError(t, os.Symlink(target, link))

	serve(t, newServer(&fakeEngine{}), filepath.Join(link, "pco", "pco.sock"))

	info, err := os.Lstat(filepath.Join(target, "pco", "pco.sock"))
	require.NoError(t, err, "the socket is in the directory the link leads to")
	require.Equal(t, fs.ModeSocket, info.Mode().Type())
}

func TestAParentThatIsNotADirectoryIsRefused(t *testing.T) {
	file := filepath.Join(shortDir(t), "file")
	require.NoError(t, os.WriteFile(file, []byte("keep me"), 0o600))

	err := serveFails(t, newServer(&fakeEngine{}), filepath.Join(file, "pco", "pco.sock"))

	require.ErrorContains(t, err, file)
	require.ErrorContains(t, err, "parent")
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, "keep me", string(data))
}

func TestAParentOfSomeoneElseIsRefused(t *testing.T) {
	needRoot(t, "only root may give a directory to another user")
	const other = 1234
	parent, pco := parentWith(t, 0o755)
	require.NoError(t, os.Chown(parent, other, other))

	err := serveFails(t, newServer(&fakeEngine{}), filepath.Join(pco, "pco.sock"))

	require.ErrorContains(t, err, parent)
	require.ErrorContains(t, err, "belongs to uid 1234")
	info, err := os.Stat(pco)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o700), info.Mode().Perm(), "pco must not be changed")
	entries, err := os.ReadDir(pco)
	require.NoError(t, err)
	require.Empty(t, entries, "nothing may be made in pco")
}
