package lease

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Gate is the write gate: nil while this process holds a valid lease whose
// last refresh started at most GateAge ago (a synchronous refresh otherwise,
// shared for ShareRefresh), quorum is true, no pause was seen, not behind.
// It returns the writer to write as. A write that waited more than a second
// after it, in a rate limiter say, asks again.
func (k *Keeper) Gate(ctx context.Context) (planner.Writer, error) {
	quorate, at := k.cfg.Quorum()
	k.mu.Lock()
	k.look()
	k.noteQuorum(quorate, at)
	w, refresh, err := k.admit()
	k.mu.Unlock()
	if !refresh {
		return w, err
	}

	if err := k.takeOp(ctx); err != nil {
		return planner.Writer{}, err
	}
	defer k.putOp()
	// Another gate may have refreshed while this one waited, and the node
	// may have been suspended meanwhile.
	quorate, at = k.cfg.Quorum()
	k.mu.Lock()
	k.look()
	k.noteQuorum(quorate, at)
	w, refresh, err = k.admit()
	if refresh {
		err = k.shared()
	}
	k.mu.Unlock()
	switch {
	case err != nil && refresh:
		return planner.Writer{}, fmt.Errorf("%w: %w", ErrFenced, err)
	case !refresh:
		return w, err
	}

	if err := k.refresh(ctx, true); err != nil {
		if errors.Is(err, ErrNotHolder) {
			return planner.Writer{}, err
		}
		return planner.Writer{}, fmt.Errorf("%w: %w", ErrFenced, err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.look()
	if w, refresh, err = k.admit(); refresh {
		return planner.Writer{}, fmt.Errorf("%w: a pause came during the refresh", ErrFenced)
	}
	return w, err
}

// admit decides a gate without a refresh: the writer, or why not, or that
// only a refresh can tell.
func (k *Keeper) admit() (w planner.Writer, refresh bool, err error) {
	if err := k.blocked(); err != nil {
		return planner.Writer{}, false, err
	}
	if !k.fresh(k.mono()) {
		return planner.Writer{}, true, nil
	}
	return k.rec.Writer, false, nil
}

// blocked says why no write may go out, whatever the age of the lease.
func (k *Keeper) blocked() error {
	switch {
	case !k.held:
		return fmt.Errorf("%w: the lease keeper is not running", ErrNotHolder)
	case !k.holder && k.waiting != "":
		return fmt.Errorf("%w: %s", ErrNotHolder, k.waiting)
	case !k.holder, k.stopping:
		return ErrNotHolder
	case k.behind != "":
		return fmt.Errorf("%w: %s", ErrBehind, k.behind)
	case k.alarm != "":
		return fmt.Errorf("%w: %s", ErrFenced, k.alarm)
	}
	return k.quorumErr()
}

// shared is the answer of a refresh the gate may not repeat yet: a failed
// synchronous one less than ShareRefresh ago, or any failed one less than
// retryAfter ago. nil means refresh.
func (k *Keeper) shared() error {
	mono := k.mono()
	switch {
	case k.synced && k.syncErr != nil && mono-k.syncAt < ShareRefresh:
		return k.syncErr
	case k.failErr != nil && mono-k.failAt < retryAfter:
		return k.failErr
	}
	return nil
}

// renewed makes sure the lease is fresh, with a refresh when it is not. The
// caller holds op.
func (k *Keeper) renewed(ctx context.Context) error {
	k.mu.Lock()
	fresh := k.fresh(k.mono())
	k.mu.Unlock()
	if fresh {
		return nil
	}
	if err := k.refresh(ctx, true); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.fresh(k.mono()) {
		return fmt.Errorf("%w: a pause came during the refresh", ErrFenced)
	}
	return nil
}

// tenureOver is a re-read of leader.json that names another epoch: someone
// acquired since, or the store was put back.
type tenureOver struct{ why string }

func (e *tenureOver) Error() string { return e.why }

// refused is pmxcfs's answer to the refresh itself, whose errno decides.
type refused struct{ err error }

func (e *refused) Error() string { return e.err.Error() }
func (e *refused) Unwrap() error { return e.err }

// refresh renews the lease: leader.json must still name the epoch of the
// tenure, then the mtime of the lock is set. The caller holds op.
//
// The errno of a refusal decides only with a quorum: pmxcfs refuses every
// write without one. ENOENT, the lock gone, ends the tenure while the last
// quorum read is true; EACCES, the lock another node's, when the first read
// begun after it is. Anything else keeps the tenure, refreshing on the normal
// period, and a refresh that succeeds again resumes the same epoch: nobody can
// have acquired without rewriting leader.json.
func (k *Keeper) refresh(ctx context.Context, sync bool) error {
	k.mu.Lock()
	if !k.holder {
		k.mu.Unlock()
		return ErrNotHolder
	}
	rec := k.rec
	start := k.stamp()
	mtime := k.nextMtime(start.wall)
	k.attemptAt = start.mono
	k.mu.Unlock()

	err := k.bounded(ctx, func() error {
		if _, _, err := k.ours(rec); err != nil {
			return err
		}
		if err := k.cfg.FS.Refresh(k.cfg.Lock, mtime); err != nil {
			return &refused{err}
		}
		return nil
	})
	var quorate bool
	var at time.Time
	if errors.Is(err, syscall.EACCES) {
		quorate, at = k.cfg.Quorum()
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	if sync {
		k.synced, k.syncAt, k.syncErr = true, start.mono, err
	}
	if !k.holder || !sameEpoch(k.rec.Writer, rec.Writer) {
		return ErrNotHolder
	}
	if err == nil {
		k.renewedAt(start)
		return nil
	}
	if k.failErr == nil || k.failErr.Error() != err.Error() {
		k.log.Warn().Err(err).Str("epoch", epochOf(rec.Writer)).Msg("refreshing the lease failed")
	}
	k.failErr, k.failAt = err, start.mono

	var over *tenureOver
	var pmxcfs *refused
	switch {
	case errors.As(err, &over):
		return k.lost(over.why)
	case !errors.As(err, &pmxcfs):
	case errors.Is(err, syscall.ENOENT) && k.quorumErr() == nil:
		return k.lost("the lock is gone: it expired and was removed")
	case errors.Is(err, syscall.EACCES):
		k.noteQuorum(quorate, at)
		k.refused, k.refusedAt = true, start.wall
		k.judgeRefusal()
		if !k.holder {
			return fmt.Errorf("%w: %s", ErrNotHolder, k.waiting)
		}
	}
	return err
}

// renewedAt takes a good refresh that began at start.
func (k *Keeper) renewedAt(start stamp) {
	k.failErr, k.refused = nil, false
	if start.mono < k.pauseAt {
		return
	}
	if !k.valid(start.mono) {
		k.log.Info().Str("epoch", epochOf(k.rec.Writer)).Msg("the lease is valid again; the same epoch resumes")
	}
	k.good, k.goodMono, k.goodWall, k.pause = true, start.mono, start.wall, ""
}

// lost ends the tenure for why and says so.
func (k *Keeper) lost(why string) error {
	k.log.Warn().Str("epoch", epochOf(k.rec.Writer)).Str("why", why).Msg("the tenure ended")
	k.endTenure(why)
	return fmt.Errorf("%w: %s", ErrNotHolder, why)
}

// judgeRefusal decides a refresh pmxcfs refused with EACCES, by the first
// quorum read begun after it: quorate, the lock is another node's and the
// tenure ends; not quorate, the errno says nothing.
func (k *Keeper) judgeRefusal() {
	if !k.refused || !k.holder || !k.quorumAt.After(k.refusedAt) {
		return
	}
	k.refused = false
	if k.quorate {
		_ = k.lost("pmxcfs refused the refresh while the cluster is quorate: the lock is another node's")
	}
}

// ours re-reads leader.json and checks that it names the epoch of rec. Every
// refresh asks it first, so a process that is not the tenure's never renews
// the lock, which pmxcfs owns per node and not per process.
func (k *Keeper) ours(rec store.LeaderRecord) (store.LeaderRecord, int64, error) {
	stored, rev, found, err := k.cfg.Store.Leader()
	switch {
	case err != nil:
		return store.LeaderRecord{}, 0, fmt.Errorf("reading leader.json: %w", err)
	case !found:
		return store.LeaderRecord{}, 0, errors.New("leader.json is missing")
	case !sameEpoch(stored.Writer, rec.Writer):
		return store.LeaderRecord{}, 0, &tenureOver{fmt.Sprintf("leader.json names epoch %s, not this tenure's %s",
			epochOf(stored.Writer), epochOf(rec.Writer))}
	}
	return stored, rev, nil
}

// look looks for a pause since the last look: a gap between ticks, or a
// suspend, which moves boot time and not monotonic time. A pause invalidates
// the last refresh: the process cannot know what happened meanwhile. A step
// of the wall clock asks for a refresh at once.
func (k *Keeper) look() {
	mono, boot := k.cfg.Clocks.Monotonic(), k.cfg.Clocks.Boottime()
	offset := boot - mono
	wallOffset := time.Duration(k.cfg.Clocks.Now().UnixNano()) - mono
	if k.looked {
		// A suspend moves the wall clock as much as boot time; a step moves
		// the wall clock alone.
		if step := wallOffset - k.wallOffset - (offset - k.offset); k.holder && (step > SuspendJump || step < -SuspendJump) {
			k.log.Warn().Dur("step", step).Msg("the wall clock stepped; refreshing the lease at once")
			k.stepped = true
		}
		gap, jump := mono-k.lookedAt, offset-k.offset
		switch {
		case gap > TickGap:
			k.paused(mono, fmt.Sprintf("a pause of %s without a tick", gap))
		case jump > SuspendJump || jump < -SuspendJump:
			k.paused(mono, fmt.Sprintf("a pause of %s in a suspend", jump.Abs()))
		}
	}
	k.looked, k.lookedAt, k.offset, k.wallOffset = true, mono, offset, wallOffset
}

func (k *Keeper) paused(at time.Duration, why string) {
	if k.holder && k.good {
		k.log.Warn().Str("why", why).Msg("the last refresh of the lease no longer counts")
	}
	k.good, k.pause, k.pauseAt, k.synced = false, why, at, false
}

func (k *Keeper) noteQuorum(quorate bool, at time.Time) {
	k.quorate, k.quorumAt = quorate, at
}

// quorumErr says why the last quorum read does not answer yes.
func (k *Keeper) quorumErr() error {
	age := k.cfg.Clocks.Now().Sub(k.quorumAt)
	switch {
	case k.quorumAt.IsZero():
		return fmt.Errorf("%w: the members file was not read yet", ErrNotQuorate)
	case age > quorumAge || age < -quorumAge:
		return fmt.Errorf("%w: the last read of the members file is %s old", ErrNotQuorate, age)
	case !k.quorate:
		return ErrNotQuorate
	}
	return nil
}

// valid is the lease valid at mono: a good refresh less than Validity ago.
func (k *Keeper) valid(mono time.Duration) bool {
	return k.holder && k.good && mono-k.goodMono < Validity
}

// fresh is the lease valid and refreshed at most GateAge ago.
func (k *Keeper) fresh(mono time.Duration) bool {
	return k.valid(mono) && mono-k.goodMono <= GateAge
}

// fencedWhy says why the holder may not write, whatever a refresh would say.
func (k *Keeper) fencedWhy(mono time.Duration) string {
	switch {
	case k.quorumErr() != nil:
		return k.quorumErr().Error()
	case k.alarm != "":
		return k.alarm
	case !k.good && k.pause != "":
		return k.pause + "; the next write refreshes first"
	case !k.valid(mono):
		why := fmt.Sprintf("the lease was not refreshed for %s", mono-k.goodMono)
		if k.failErr != nil {
			why += ": " + k.failErr.Error()
		}
		return why
	case k.failErr != nil:
		return "the last refresh failed: " + k.failErr.Error()
	}
	return ""
}
