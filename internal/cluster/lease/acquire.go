package lease

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"syscall"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// acquire tries once to take the lease. recheck is the mkdir after an unlock
// request: a lock still there waits for the next poll. The caller holds op.
func (k *Keeper) acquire(ctx context.Context, recheck bool) {
	stored, found, why := k.mayAcquire(ctx)
	if why != "" {
		k.wait(why)
		return
	}
	start := k.stamp()
	err := k.mkdir(ctx)
	switch {
	case err == nil:
		k.claim(ctx, true, start)
	case errors.Is(err, syscall.EEXIST) && recheck:
		// Still there: the next poll asks again.
	case errors.Is(err, syscall.EEXIST):
		k.contend(ctx, stored, found)
	default:
		k.wait(fmt.Sprintf("cannot make the lock: %v", err))
	}
}

// mayAcquire reads leader.json and says why this node may not try now.
func (k *Keeper) mayAcquire(ctx context.Context) (stored store.LeaderRecord, found bool, why string) {
	if ok, why := k.cfg.Eligible(); !ok {
		return stored, false, "this node may not be the controller: " + why
	}
	k.mu.Lock()
	quorum, holdBack := k.quorumErr(), k.holdBack
	k.mu.Unlock()
	switch {
	case quorum != nil:
		return stored, false, quorum.Error()
	case k.mono() < holdBack:
		return stored, false, "this node handed the lease off and holds back for a while"
	}
	err := k.bounded(ctx, func() (err error) {
		stored, _, found, err = k.cfg.Store.Leader()
		return err
	})
	if err != nil {
		return stored, false, fmt.Sprintf("reading leader.json: %v", err)
	}
	return stored, found, mayTry(stored, found, k.cfg.Node, k.cfg.Guard(), k.cfg.Clocks.Now())
}

// mkdir makes the lock with a plain mkdir. The directory of the locks is made
// when it is missing, as the HA manager does, but never what is above it: a
// cluster filesystem that is not mounted has none of it.
func (k *Keeper) mkdir(ctx context.Context) error {
	err := k.bounded(ctx, func() error { return k.cfg.FS.Mkdir(k.cfg.Lock) })
	if !errors.Is(err, syscall.ENOENT) {
		return err
	}
	parent := k.bounded(ctx, func() error { return k.cfg.FS.Mkdir(filepath.Dir(k.cfg.Lock)) })
	if parent != nil && !errors.Is(parent, syscall.EEXIST) {
		return err
	}
	return k.bounded(ctx, func() error { return k.cfg.FS.Mkdir(k.cfg.Lock) })
}

// contend handles a lock that is there. When leader.json names this node and
// this boot, the daemon restarted and the lock may still be its own: pmxcfs
// accepting the refresh says so. Otherwise an unlock request asks pmxcfs to
// remove the lock, which it does only once the lock expired, a lock this node
// made before a reboot included.
//
// pmxcfs answers the request with EACCES whether or not it removes the lock,
// which it does a moment later, so a mkdir follows it after removalWait.
// Expiry is judged by the wall clock of the node with the lowest node id, not
// by this node's: a step forward of that clock lets a request remove a lock
// that is fresh. That is why a node sends one only while its agent hears no
// live controller (mayTry).
func (k *Keeper) contend(ctx context.Context, stored store.LeaderRecord, found bool) {
	if found && stored.Node == k.cfg.Node && stored.Incarnation == k.cfg.Incarnation {
		start := k.stamp()
		k.mu.Lock()
		mtime := k.nextMtime(start.wall)
		k.mu.Unlock()
		if k.bounded(ctx, func() error { return k.cfg.FS.Refresh(k.cfg.Lock, mtime) }) == nil {
			k.claim(ctx, false, start)
			return
		}
	}
	err := k.bounded(ctx, func() error { return k.cfg.FS.UnlockRequest(k.cfg.Lock) })
	switch {
	case err == nil, errors.Is(err, syscall.ENOENT):
		// Gone already.
		start := k.stamp()
		if err := k.bounded(ctx, func() error { return k.cfg.FS.Mkdir(k.cfg.Lock) }); err != nil {
			k.wait(fmt.Sprintf("the lock expired, but making it again failed: %v", err))
			return
		}
		k.claim(ctx, true, start)
	case errors.Is(err, syscall.EACCES):
		k.mu.Lock()
		k.recheck = k.mono() + removalWait
		k.mu.Unlock()
		k.wait(fmt.Sprintf("the lock is held; leader.json names node %q", stored.Node))
	case errors.Is(err, syscall.ENOTEMPTY):
		k.waitAlarm(fmt.Sprintf("something put a file into the lock %s, so it never expires; pco never removes it", k.cfg.Lock))
	default:
		k.wait(fmt.Sprintf("the unlock request failed: %v", err))
	}
}

// claim makes the lock this node's tenure: it decides the epoch, writes it
// into leader.json and reads it back, and only then holds the lease. made is
// a lock this attempt made, which goes again at once if that fails and it is
// still this node's: nothing else was written. A lock that was there already,
// still this node's, is left to expire.
func (k *Keeper) claim(ctx context.Context, made bool, start stamp) {
	var stored store.LeaderRecord
	var rev int64
	err := k.bounded(ctx, func() (err error) {
		stored, rev, _, err = k.cfg.Store.Leader()
		return err
	})
	if err != nil {
		k.abandon(ctx, made, start, fmt.Sprintf("reading leader.json: %v", err))
		return
	}
	remembered, have := k.cfg.Remembered()
	now := k.cfg.Clocks.Now().UTC()
	var next store.LeaderRecord
	var behind string
	if made {
		next, behind, err = draw(stored, remembered, have, k.cfg.Node, k.cfg.Incarnation, now, k.cfg.Rand, false)
	} else {
		next, behind, err = Draw(stored, remembered, have, k.cfg.Node, k.cfg.Incarnation, now, k.cfg.Rand)
	}
	if err != nil {
		k.abandon(ctx, made, start, err.Error())
		return
	}
	if behind == "" && !sameEpoch(next.Writer, stored.Writer) {
		if err := k.bounded(ctx, func() error { return k.save(next, rev) }); err != nil {
			k.abandon(ctx, made, start, err.Error())
			return
		}
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	k.begin(next, behind, start)
	ev := k.log.Info()
	switch {
	case behind != "":
		ev = k.log.Error().Str("behind", behind)
	case sameEpoch(next.Writer, stored.Writer):
		ev = ev.Bool("kept", true)
	}
	ev.Str("epoch", epochOf(next.Writer)).Msg("acquired the lease")
}

// save stores leader.json over revision rev and reads it back, inside a
// bounded call.
func (k *Keeper) save(next store.LeaderRecord, rev int64) error {
	if err := k.cfg.Store.SaveLeaderIf(next, rev); err != nil {
		return fmt.Errorf("writing leader.json: %w", err)
	}
	got, _, found, err := k.cfg.Store.Leader()
	switch {
	case err != nil:
		return fmt.Errorf("reading leader.json back: %w", err)
	case !found || !sameRecord(got, next):
		return errors.New("leader.json does not read back as written")
	}
	return nil
}

// sameRecord compares two records as they survive a write.
func sameRecord(a, b store.LeaderRecord) bool {
	return a.Writer == b.Writer && a.Node == b.Node && a.AcquiredAt.Equal(b.AcquiredAt) &&
		a.ReleasedAt.Equal(b.ReleasedAt) && a.CASHA256 == b.CASHA256 &&
		(a.Handoff == nil) == (b.Handoff == nil) &&
		(a.Handoff == nil || a.Handoff.To == b.Handoff.To && a.Handoff.Until.Equal(b.Handoff.Until))
}

// abandon gives up a claim that began at start.
func (k *Keeper) abandon(ctx context.Context, made bool, start stamp, why string) {
	if made {
		if err := k.giveBack(ctx, start); err != nil {
			why += fmt.Sprintf("; the lock is left to expire: %v", err)
		}
	}
	k.waitAt(zerolog.WarnLevel, "could not take the lease", why)
}

// giveBack removes the lock a claim that began at start made. By now the lock
// may have expired and gone to another node, so it goes only as removeLock
// says, and not at all after a pause since the claim began.
func (k *Keeper) giveBack(ctx context.Context, start stamp) error {
	k.mu.Lock()
	k.look()
	paused, why := k.pauseAt >= start.mono, k.pause
	k.mu.Unlock()
	if paused {
		return errors.New(why)
	}
	return k.removeLock(ctx, func() error { return nil })
}

// wait records why this node does not hold the lease. A change is logged.
func (k *Keeper) wait(why string) { k.waitAt(zerolog.InfoLevel, "waiting for the lease", why) }

// waitAlarm is wait for what an administrator must look at.
func (k *Keeper) waitAlarm(why string) {
	k.waitAt(zerolog.ErrorLevel, "the lease cannot be taken", why)
}

func (k *Keeper) waitAt(level zerolog.Level, msg, why string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if why != k.waiting {
		k.log.WithLevel(level).Str("why", why).Msg(msg)
	}
	k.waiting = why
}
