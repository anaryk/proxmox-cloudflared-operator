package lease

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// DefaultProcessLock is the file whose lock keeps a second keeper of the node
// from starting.
const DefaultProcessLock = "/run/pco/lease.lock"

// ErrRunning is the error of a keeper that finds another one on its node.
var ErrRunning = errors.New("another lease keeper is running on this node")

// ProcessLock takes an exclusive lock on the file at path, made with its
// directory when missing, and gives it back with release. pmxcfs owns a lock
// per node, not per process: a second process of the node would have its
// refreshes taken as the holder's. The lock is let go when the process ends,
// so a keeper that starts after a crash takes it again. The file stays where
// it is: removing it would let two keepers lock two different files.
func ProcessLock(path string) func() (release func(), err error) {
	return func() (func(), error) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("creating the directory of %s: %w", path, err)
		}
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return nil, fmt.Errorf("opening %s: %w", path, err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, fmt.Errorf("%w: %s is locked", ErrRunning, path)
			}
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		return func() { _ = f.Close() }, nil
	}
}
