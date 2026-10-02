package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// lockName is the lock a daemon holds on the node for as long as it runs,
// below the local root, also when it was not started by systemd.
const lockName = "daemon.lock"

func (s *Setup) lockPath() string { return filepath.Join(s.host.paths.Local, lockName) }

// daemonLocked reports whether a daemon holds the lock of the node. It tries
// the lock without waiting and lets it go at once; a missing file is no
// daemon, and is not made.
func (s *Setup) daemonLocked() (bool, error) {
	f, err := os.Open(s.lockPath())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("opening %s: %w", s.lockPath(), err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, fmt.Errorf("trying %s: %w", s.lockPath(), err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}

// errRunsByHand is the error of a daemon that holds the lock of the node while
// systemd does not run it.
func (s *Setup) errRunsByHand() error {
	return fmt.Errorf("a pco daemon runs on this node outside systemd (it holds %s): stop it first", s.lockPath())
}
