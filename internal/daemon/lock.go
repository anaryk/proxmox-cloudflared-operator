package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const lockName = "daemon.lock"

// ErrRunning is the error of a daemon that finds another one on this node.
var ErrRunning = errors.New("another pco daemon is running on this node")

// lockNode takes an exclusive lock on <dir>/daemon.lock and keeps it until the
// returned function is called or the process ends. The file stays where it is:
// removing it would let two daemons lock two different files.
//
// This is the lock of the node. The one the API server takes belongs to its
// socket and does not keep a second daemon with another socket from starting.
func lockNode(dir string) (release func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, lockName)
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
	return func() { _ = f.Close() }, nil
}
