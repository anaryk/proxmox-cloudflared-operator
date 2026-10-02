package daemon

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// ErrRunning is the error of a daemon that finds another one on this node.
var ErrRunning = errors.New("another pco daemon is running on this node")

// nodeLock is the lock of the node, held for as long as its file is open.
type nodeLock struct {
	f    *os.File
	path string
}

// lockNode takes an exclusive lock on <dir>/daemon.lock and keeps it until
// release is called or the process ends. The file stays where it is: removing
// it would let two daemons lock two different files.
//
// This is the lock of the node. The one the API server takes belongs to its
// socket and does not keep a second daemon with another socket from starting.
// (internal/api has a copy of this helper for that one; the packages do not
// share it.)
func lockNode(dir string) (*nodeLock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	path := store.Paths{Local: dir}.NodeLock()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrRunning
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return &nodeLock{f: f, path: path}, nil
}

func (l *nodeLock) release() { _ = l.f.Close() }

// check reports whether the lock still keeps a second daemon out: the file at
// its path must be the one this daemon locked. One that was removed or
// replaced lets another daemon lock a file of its own.
func (l *nodeLock) check() error {
	held, err := l.f.Stat()
	if err != nil {
		return fmt.Errorf("reading the lock this daemon holds: %w", err)
	}
	there, err := os.Lstat(l.path)
	if err != nil {
		return fmt.Errorf("the lock of the node %s is gone; a second daemon could start: %w", l.path, err)
	}
	if !os.SameFile(held, there) {
		return fmt.Errorf("the lock of the node %s is not the file this daemon locked; a second daemon could start", l.path)
	}
	return nil
}
