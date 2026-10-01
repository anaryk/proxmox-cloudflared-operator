package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	socketMode = 0o660
	dirMode    = 0o750
	// socketDirName is the one name the directory of the socket may have.
	socketDirName = "pco"
	lockSuffix    = ".lock"
	probeTimeout  = time.Second
)

// socket is a listening socket that is at its public path, together with the
// lock that keeps a second daemon from taking the path.
type socket struct {
	path string
	ln   net.Listener
	info fs.FileInfo // of the socket file this daemon made
	lock *os.File
}

// openSocket makes the directory, takes the lock of the path and binds the
// socket there. The lock comes before everything that looks at the path, so
// that two daemons that start together cannot both find a stale socket and
// replace each other's.
func (s *Server) openSocket(ctx context.Context, path string, gid int) (*socket, error) {
	dir := filepath.Dir(path)
	if err := s.prepareDir(dir, gid); err != nil {
		return nil, err
	}
	lock, err := lockFile(path + lockSuffix)
	if err != nil {
		return nil, err
	}
	if err := checkExisting(ctx, path); err != nil {
		_ = lock.Close()
		return nil, err
	}
	p, err := s.bindPrivate(path, gid)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	info, err := p.publish(path)
	if err != nil {
		p.abandon()
		_ = lock.Close()
		return nil, err
	}
	return &socket{path: path, ln: p.ln, info: info, lock: lock}, nil
}

// release removes the socket file, unless someone else has put another in its
// place, and gives up the lock. The listener is closed by then.
func (k *socket) release() {
	if cur, err := os.Lstat(k.path); err == nil && os.SameFile(cur, k.info) {
		_ = os.Remove(k.path)
	}
	_ = k.lock.Close()
}

// prepareDir makes the socket directory when it is missing, and gives it, new
// or old, the mode and the owner the socket directory has. That is done to a
// directory named pco and to no other, so that it can never be /tmp or /run; and
// only the directory itself is made, never its parent. A directory that is there
// already must be a real directory, not a symlink, and owned by the user the
// daemon runs as; anything else is refused before a thing is changed.
//
// The directory is opened without following a symlink, its owner is read from
// what is open, and what is changed is the open directory, so that a link put in
// its place cannot send a change elsewhere.
func (s *Server) prepareDir(dir string, gid int) error {
	if filepath.Base(dir) != socketDirName {
		return fmt.Errorf("the socket directory %s must be named %q: its mode and owner are changed, which no other directory may have done to it", dir, socketDirName)
	}
	if err := os.Mkdir(dir, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("creating the socket directory %s, whose parent must exist: %w", dir, err)
	}
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("the socket directory %s must be a real directory, not a symlink or a file: %w", dir, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("reading the socket directory %s: %w", dir, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("the owner of the socket directory %s cannot be read on this platform", dir)
	}
	if uid := uint32(os.Geteuid()); st.Uid != uid {
		return fmt.Errorf("the socket directory %s belongs to uid %d, not to the daemon, which runs as uid %d; not touching it", dir, st.Uid, uid)
	}
	// A daemon that is not root, as in a test, may not own what it is given.
	if err := f.Chmod(dirMode); err != nil && !errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("setting the mode of %s: %w", dir, err)
	}
	s.noteOwner(f.Chown(0, gid), dir, gid)
	return nil
}

// noteOwner reports a failure to hand a file to root and gid, unless it is the
// one of a daemon that is not allowed to.
func (s *Server) noteOwner(err error, path string, gid int) {
	if err != nil && !errors.Is(err, fs.ErrPermission) {
		s.log.Warn().Err(err).Str("path", path).Int("gid", gid).Msg("could not change the owner")
	}
}

// lockFile takes an exclusive lock on path and keeps the file open: the lock
// goes with it. The file stays when the daemon ends; removing it would let two
// daemons lock two different files.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another pco instance is running: %s is locked", path)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return f, nil
}

// checkExisting refuses a path that holds anything but a socket nobody listens
// on. A socket nobody listens on is left for publish to replace.
func checkExisting(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("checking %s: %w", path, err)
	case info.Mode().Type() != fs.ModeSocket:
		return fmt.Errorf("%s exists and is not a socket; not removing it", path)
	}
	d := net.Dialer{Timeout: probeTimeout}
	conn, err := d.DialContext(ctx, "unix", path)
	switch {
	case err == nil:
		_ = conn.Close()
		return fmt.Errorf("another process is already listening on %s", path)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
		return nil
	}
	return fmt.Errorf("checking whether %s is in use: %w", path, err)
}

// pending is a socket that is bound and set up, but not yet at its public path.
//
// A unix socket is created with the mode the umask leaves, and until it is
// changed another user may connect to it. The umask is a setting of the whole
// process, which the engine's goroutines write files under, so it is not
// changed. The socket is bound instead in a directory of its own that only the
// daemon can enter, given its mode and owner there, and then renamed to where
// it belongs, which makes it reachable all at once and in its final state. A
// rename also replaces a stale socket at the public path in one step.
type pending struct {
	ln   *net.UnixListener
	dir  string // the private directory
	path string // the socket in it
}

// bindPrivate binds a socket for path in a private directory beside it, named
// after it. The caller holds the lock of the path, which is what makes what an
// earlier daemon left of that directory, if it crashed, nobody's but ours to
// remove.
func (s *Server) bindPrivate(path string, gid int) (*pending, error) {
	dir := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".new")
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("removing what a crash left in %s: %w", dir, err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("making the private directory %s: %w", dir, err)
	}
	p := &pending{dir: dir, path: filepath.Join(dir, "s")}
	// Mkdir is subject to the umask.
	if err := os.Chmod(dir, 0o700); err != nil {
		p.abandonDir()
		return nil, fmt.Errorf("setting the mode of %s: %w", dir, err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: p.path, Net: "unix"})
	if err != nil {
		p.abandonDir()
		return nil, fmt.Errorf("listening on %s: %w", p.path, err)
	}
	// The file is renamed, so the listener must not unlink its old name.
	ln.SetUnlinkOnClose(false)
	p.ln = ln
	if err := os.Chmod(p.path, socketMode); err != nil {
		p.abandon()
		return nil, fmt.Errorf("setting the mode of the socket: %w", err)
	}
	s.noteOwner(os.Lchown(p.path, 0, gid), p.path, gid)
	return p, nil
}

// publish moves the socket to path and removes the private directory. It
// returns what it left at path.
func (p *pending) publish(path string) (fs.FileInfo, error) {
	if err := os.Rename(p.path, path); err != nil {
		return nil, fmt.Errorf("moving the socket to %s: %w", path, err)
	}
	// Whatever happens to the file now is not the listener's to undo.
	_ = os.Remove(p.dir)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("checking %s: %w", path, err)
	}
	return info, nil
}

// abandon closes the listener and removes what is left of the private directory.
func (p *pending) abandon() {
	_ = p.ln.Close()
	p.abandonDir()
}

func (p *pending) abandonDir() { _ = os.RemoveAll(p.dir) }
