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
// lock that keeps a second daemon from taking the path. It holds the socket
// directory open, and everything it does to the directory goes through that.
type socket struct {
	path string   // where the socket is
	name string   // its name in dir
	dir  *os.Root // the socket directory
	ln   net.Listener
	info fs.FileInfo // of the socket file this daemon made
	lock *os.File
}

// openSocket opens the directory, takes the lock of the path and binds the
// socket there. The lock comes before everything that looks at the path, so
// that two daemons that start together cannot both find a stale socket and
// replace each other's.
func (s *Server) openSocket(ctx context.Context, path string, gid int) (*socket, error) {
	dirPath, name := filepath.Split(path)
	dirPath = filepath.Clean(dirPath)
	dir, err := s.openDir(dirPath, gid)
	if err != nil {
		return nil, err
	}
	lock, err := lockFile(dir, name+lockSuffix)
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	giveUp := func(err error) (*socket, error) {
		_ = lock.Close()
		_ = dir.Close()
		return nil, err
	}
	if err := checkExisting(ctx, dir, name, path); err != nil {
		return giveUp(err)
	}
	p, err := s.bindPrivate(dir, dirPath, name, gid)
	if err != nil {
		return giveUp(err)
	}
	info, err := p.publish(name)
	if err != nil {
		p.abandon()
		return giveUp(err)
	}
	return &socket{path: path, name: name, dir: dir, ln: p.ln, info: info, lock: lock}, nil
}

// release removes the socket file, unless someone else has put another in its
// place, and gives up the lock and the directory. The listener is closed by
// then.
func (k *socket) release() {
	if cur, err := k.dir.Lstat(k.name); err == nil && os.SameFile(cur, k.info) {
		_ = k.dir.Remove(k.name)
	}
	_ = k.lock.Close()
	_ = k.dir.Close()
}

// openDir opens the socket directory, making it when it is missing, and gives it,
// new or old, the mode and the owner the socket directory has. That is done to
// a directory named pco and to no other, so that it can never be /tmp or /run;
// and only the directory itself is made, never its parent.
//
// The parent must be a directory that belongs to the user the daemon runs as and
// that nobody else can write to: whoever can write there can rename the socket
// directory away and put one of their own in its place. A directory that is
// there already must be a real directory, not a symlink, and owned by the user
// the daemon runs as. Anything else is refused before a thing is changed.
//
// What comes back is a handle on the directory. Everything done to it from now
// on is done through the handle, so that a path swapped for another cannot send
// it elsewhere. What the checks read, they read from what is open.
func (s *Server) openDir(dir string, gid int) (*os.Root, error) {
	if filepath.Base(dir) != socketDirName {
		return nil, fmt.Errorf("the socket directory %s must be named %q: its mode and owner are changed, which no other directory may have done to it", dir, socketDirName)
	}
	parent, err := openParent(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()

	if err := parent.Mkdir(socketDirName, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("creating the socket directory %s: %w", dir, err)
	}
	// A Root follows a symlink that leads to somewhere inside it, whatever the
	// flags say, so the entry is looked at first. Nobody else can change it
	// meanwhile, and what is opened is checked to be it.
	entry, err := parent.Lstat(socketDirName)
	if err != nil {
		return nil, fmt.Errorf("reading the socket directory %s: %w", dir, err)
	}
	if !entry.IsDir() {
		return nil, fmt.Errorf("the socket directory %s must be a real directory, not a symlink or a file", dir)
	}
	f, err := parent.OpenFile(socketDirName, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("the socket directory %s must be a real directory, not a symlink or a file: %w", dir, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("reading the socket directory %s: %w", dir, err)
	}
	if !os.SameFile(entry, info) {
		return nil, fmt.Errorf("the socket directory %s was replaced while it was opened", dir)
	}
	if err := checkOwner(info, "the socket directory", dir); err != nil {
		return nil, err
	}
	// A daemon that is not root, as in a test, may not own what it is given.
	if err := f.Chmod(dirMode); err != nil && !errors.Is(err, fs.ErrPermission) {
		return nil, fmt.Errorf("setting the mode of %s: %w", dir, err)
	}
	s.noteOwner(f.Chown(0, gid), dir, gid)

	root, err := parent.OpenRoot(socketDirName)
	if err != nil {
		return nil, fmt.Errorf("opening the socket directory %s: %w", dir, err)
	}
	held, err := root.Stat(".")
	if err != nil || !os.SameFile(info, held) {
		_ = root.Close()
		return nil, fmt.Errorf("the socket directory %s was replaced while it was opened", dir)
	}
	return root, nil
}

// openParent opens the directory that holds the socket directory dir, and
// refuses it unless it belongs to the user the daemon runs as and is writable
// by neither group nor others. A symlink to such a directory is followed, as
// /var/run, a link to /run, needs. The owner and the mode are those of the open
// directory, and not of the path, which may have changed since.
func openParent(dir string) (*os.Root, error) {
	path := filepath.Dir(dir)
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("the socket directory %s needs its parent %s to exist and to be a directory: %w", dir, path, err)
	}
	info, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("reading %s, the parent of the socket directory %s: %w", path, dir, err)
	}
	if err := checkOwner(info, "the parent of the socket directory", path); err != nil {
		_ = root.Close()
		return nil, err
	}
	if mode := info.Mode().Perm(); mode&0o022 != 0 {
		_ = root.Close()
		return nil, fmt.Errorf("the parent %s of the socket directory %s is writable by group or others (mode %04o): whoever can write there can replace the socket directory; not using it", path, dir, uint32(mode))
	}
	return root, nil
}

// checkOwner refuses what the daemon's own user does not own.
func checkOwner(info fs.FileInfo, what, path string) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("the owner of %s %s cannot be read on this platform", what, path)
	}
	if uid := uint32(os.Geteuid()); st.Uid != uid {
		return fmt.Errorf("%s %s belongs to uid %d, not to the daemon, which runs as uid %d; not touching it", what, path, st.Uid, uid)
	}
	return nil
}

// noteOwner reports a failure to hand a file to root and gid, unless it is the
// one of a daemon that is not allowed to.
func (s *Server) noteOwner(err error, path string, gid int) {
	if err != nil && !errors.Is(err, fs.ErrPermission) {
		s.log.Warn().Err(err).Str("path", path).Int("gid", gid).Msg("could not change the owner")
	}
}

// lockFile takes an exclusive lock on the file name in dir and keeps the file
// open: the lock goes with it. The file stays when the daemon ends; removing it
// would let two daemons lock two different files.
func lockFile(dir *os.Root, name string) (*os.File, error) {
	path := filepath.Join(dir.Name(), name)
	f, err := dir.OpenFile(name, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
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
// on. A socket nobody listens on is left for publish to replace. The probe
// connects by path, which is all a connection can do; it sends nothing, and the
// worst a path that was swapped for another can do is have it connect there.
func checkExisting(ctx context.Context, dir *os.Root, name, path string) error {
	info, err := dir.Lstat(name)
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
	root    *os.Root // the socket directory
	ln      *net.UnixListener
	dir     string // the private directory, as a path
	path    string // the socket in it, as a path
	relDir  string // the same, in the socket directory
	relSock string
}

// bindPrivate binds a socket for name in a private directory beside it, named
// after it, in the socket directory dir, which is at dirPath. The caller holds
// the lock of the path, which is what makes what an earlier daemon left of the
// private directory, if it crashed, nobody's but ours to remove.
//
// Everything is done through the handle on dir but one thing: binding the
// socket. bind(2) takes a path, and there is no form of it that takes a
// directory handle. The path leads through the parent, which nobody but the
// daemon's user can write to, through the socket directory, which was checked
// to be the daemon's own, and through a directory made a moment ago that only
// the daemon can enter, so that a swap on the way needs the rename of something
// above the parent. To make even that visible, the socket the path led to is
// compared with the one the handle finds, and the daemon gives up when they
// differ. The change of mode and of owner, and the rename, come after that and
// go through the handle.
func (s *Server) bindPrivate(dir *os.Root, dirPath, name string, gid int) (*pending, error) {
	relDir := "." + name + ".new"
	p := &pending{
		root:    dir,
		dir:     filepath.Join(dirPath, relDir),
		path:    filepath.Join(dirPath, relDir, "s"),
		relDir:  relDir,
		relSock: filepath.Join(relDir, "s"),
	}
	if err := dir.RemoveAll(relDir); err != nil {
		return nil, fmt.Errorf("removing what a crash left in %s: %w", p.dir, err)
	}
	if err := dir.Mkdir(relDir, 0o700); err != nil {
		return nil, fmt.Errorf("making the private directory %s: %w", p.dir, err)
	}
	// Mkdir is subject to the umask.
	if err := dir.Chmod(relDir, 0o700); err != nil {
		_ = dir.RemoveAll(relDir)
		return nil, fmt.Errorf("setting the mode of %s: %w", p.dir, err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: p.path, Net: "unix"})
	if err != nil {
		_ = dir.RemoveAll(relDir)
		return nil, fmt.Errorf("listening on %s: %w", p.path, err)
	}
	// The file is renamed, so the listener must not unlink its old name.
	ln.SetUnlinkOnClose(false)
	p.ln = ln
	if err := checkBound(dir, p.relSock, p.path); err != nil {
		p.abandon()
		return nil, err
	}
	if err := dir.Chmod(p.relSock, socketMode); err != nil {
		p.abandon()
		return nil, fmt.Errorf("setting the mode of the socket: %w", err)
	}
	s.noteOwner(dir.Lchown(p.relSock, 0, gid), p.path, gid)
	return p, nil
}

// checkBound makes sure that the socket the listener made at path is the one at
// rel in the directory the daemon holds.
func checkBound(root *os.Root, rel, path string) error {
	held, err := root.Lstat(rel)
	if err != nil {
		return fmt.Errorf("the socket is not in the directory this daemon opened: %w", err)
	}
	bound, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("checking %s: %w", path, err)
	}
	if !os.SameFile(held, bound) {
		return fmt.Errorf("the socket was bound at %s, which is not in the directory this daemon opened: a directory on the way was replaced while the daemon started", path)
	}
	return nil
}

// publish moves the socket to name in the socket directory and removes the
// private directory. It returns what it left at name.
func (p *pending) publish(name string) (fs.FileInfo, error) {
	if err := p.root.Rename(p.relSock, name); err != nil {
		return nil, fmt.Errorf("moving the socket to %s: %w", name, err)
	}
	// Whatever happens to the file now is not the listener's to undo.
	_ = p.root.Remove(p.relDir)
	info, err := p.root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("checking %s: %w", name, err)
	}
	return info, nil
}

// abandon closes the listener and removes what is left of the private directory.
func (p *pending) abandon() {
	_ = p.ln.Close()
	_ = p.root.RemoveAll(p.relDir)
}
