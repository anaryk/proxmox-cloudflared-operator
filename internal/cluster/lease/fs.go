package lease

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// FS is the lock directory on the cluster filesystem. Every error carries the
// errno pmxcfs answered, for errors.Is: EEXIST from Mkdir is a lock that is
// there; EACCES is a refresh of another node's lock or with an older mtime,
// any write without quorum, and every answer to an unlock request, also to
// one that has the lock removed, or one on a lock with a file inside, which
// never expires; ENOENT is a lock that is gone; EBUSY a refresh during a
// change of the membership. Rmdir removes any lock, whoever owns it.
type FS interface {
	// Mkdir makes the directory, never its parents.
	Mkdir(path string) error
	// Refresh sets the mtime of the lock to mtime, in whole seconds, and its
	// atime to zero.
	Refresh(path string, mtime time.Time) error
	// UnlockRequest asks pmxcfs to remove a lock that expired, by setting
	// both its times to zero. The removal comes a moment after the answer.
	UnlockRequest(path string) error
	// Rmdir removes the directory. pmxcfs does not check the owner of a
	// lock: only a holder that just refreshed it may call it.
	Rmdir(path string) error
	// Entries counts what is in the directory, which any node may list.
	Entries(path string) (int, error)
}

var (
	errStuck   = errors.New("a call to the cluster filesystem is still running")
	errTimeout = fmt.Errorf("the cluster filesystem did not answer within %s", callTimeout)
)

// bounded runs call within callTimeout. A call that does not return in time
// is left running, as a call into a hung filesystem cannot be taken back;
// until it returns, every other call fails at once instead of piling up
// behind it.
func (k *Keeper) bounded(ctx context.Context, call func() error) error {
	if !k.busy.CompareAndSwap(false, true) {
		return errStuck
	}
	done := make(chan error, 1)
	go func() {
		err := call()
		k.busy.Store(false)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-k.after(callTimeout):
		return errTimeout
	case <-ctx.Done():
		return ctx.Err()
	}
}
