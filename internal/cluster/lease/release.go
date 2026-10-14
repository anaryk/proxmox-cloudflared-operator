package lease

import (
	"context"
	"errors"
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Release gives the lease back on a clean stop, except on a node that stands
// alone, which keeps the lock (keepLock): no write passes the gate
// from its start, leader.json gets releasedAt, the lock is refreshed once and
// removed right after that refresh found the tenure's nonce. A keeper that
// cannot write, fenced or without quorum, leaves the lock to expire: a fenced
// keeper never removes it. A keeper that is behind writes nothing and gives
// the lock back. The keeper acquires no more, and Run returns.
func (k *Keeper) Release(ctx context.Context) error {
	return k.letGo(ctx, "", true)
}

// Handoff releases the lease for to, which alone may take it for
// HandoffBackoff; to empty hands it to whichever node comes first, and this
// one holds back as long. A hand-off that cannot be written is refused and
// the lease kept.
func (k *Keeper) Handoff(ctx context.Context, to string) error {
	if to == k.cfg.Node {
		return errors.New("the lease cannot be handed off to this node itself")
	}
	return k.letGo(ctx, to, false)
}

func (k *Keeper) letGo(ctx context.Context, to string, closing bool) error {
	alone := closing && to == "" && k.standalone()
	if err := k.takeOp(ctx); err != nil {
		return err
	}
	defer k.putOp()
	if alone {
		return k.keepLock()
	}
	quorate, at := k.cfg.Quorum()
	k.mu.Lock()
	if closing {
		k.closed = true
	}
	if !k.holder {
		k.mu.Unlock()
		if closing {
			return nil
		}
		return ErrNotHolder
	}
	k.look()
	k.noteQuorum(quorate, at)
	// A controller that is behind, or that another one writes beside, writes
	// nothing, its release included; a hand-off has to be written.
	quiet := k.behind != "" || k.alarm != ""
	err := k.quorumErr()
	if !closing && err == nil && quiet {
		err = k.blocked()
	}
	k.stopping = true
	k.mu.Unlock()

	if err == nil {
		err = k.renewed(ctx)
	}
	if err == nil && !quiet {
		err = k.writeRelease(ctx, to)
	}
	if err != nil && !closing {
		// Nothing was written: the hand-off is refused and the lease kept.
		k.mu.Lock()
		k.stopping = false
		k.mu.Unlock()
		return err
	}
	if err == nil {
		err = k.unfenced()
	}
	if err == nil {
		k.mu.Lock()
		rec := k.rec
		k.mu.Unlock()
		err = k.removeLock(ctx, func() error {
			_, _, err := k.ours(rec)
			return err
		})
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.holder {
		if err == nil {
			err = ErrNotHolder
		}
		return fmt.Errorf("the lease is left to expire: %w", err)
	}
	epoch := epochOf(k.rec.Writer)
	if err != nil {
		k.endTenure("released; the lock is left to expire")
		k.log.Warn().Err(err).Str("epoch", epoch).Msg("released the lease; the lock is left to expire")
		return fmt.Errorf("the lease is left to expire: %w", err)
	}
	why := "released"
	if to != "" {
		why = "handed off to " + to
	}
	k.endTenure(why)
	if to == "" && !closing {
		k.holdBack = k.mono() + HandoffBackoff
	}
	k.log.Info().Str("epoch", epoch).Msg("the lease is " + why)
	return nil
}

// keepLock ends the tenure on a clean stop of a node that stands alone
// without touching the lock or leader.json: nobody else can take it, and the
// next start in the same boot keeps the epoch. The caller holds op.
func (k *Keeper) keepLock() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.closed = true
	if !k.holder {
		return nil
	}
	epoch := epochOf(k.rec.Writer)
	k.endTenure("stopped; a node that stands alone keeps its lock for its next start")
	k.log.Info().Str("epoch", epoch).Msg("stopped; the lock is kept, as this node stands alone")
	return nil
}

// unfenced says why the holder is fenced now, nil when it is not.
func (k *Keeper) unfenced() error {
	quorate, at := k.cfg.Quorum()
	k.mu.Lock()
	defer k.mu.Unlock()
	k.look()
	k.noteQuorum(quorate, at)
	switch {
	case !k.holder:
		return ErrNotHolder
	case k.fencedWhy(k.mono()) != "":
		return fmt.Errorf("%w: %s", ErrFenced, k.fencedWhy(k.mono()))
	}
	return nil
}

// removeLock removes the lock in one step with what proves it this node's:
// check (leader.json naming the tenure's epoch, say), then a refresh, which
// pmxcfs takes only from the node that made the lock, then the rmdir. pmxcfs
// has no check-and-remove and lets any node remove any lock, so the rmdir is
// sent only when the refresh began at most removeWithin before it and no
// suspend came between: the lock cannot have expired and gone to another
// node meanwhile. Anything else leaves the lock to expire. What no check
// can see is a stop of the process between the last look at the clocks and
// the rmdir itself; the node that took the lock meanwhile then loses it at
// its next refresh. The caller holds op.
func (k *Keeper) removeLock(ctx context.Context, check func() error) error {
	quorate, at := k.cfg.Quorum()
	k.mu.Lock()
	k.look()
	k.noteQuorum(quorate, at)
	err := k.quorumErr()
	mtime := k.nextMtime(k.cfg.Clocks.Now())
	k.mu.Unlock()
	if err != nil {
		return err
	}
	return k.bounded(ctx, func() error {
		if err := check(); err != nil {
			return err
		}
		mono, boot := k.cfg.Clocks.Monotonic(), k.cfg.Clocks.Boottime()
		if err := k.cfg.FS.Refresh(k.cfg.Lock, mtime); err != nil {
			return fmt.Errorf("refreshing it first: %w", err)
		}
		took := k.cfg.Clocks.Monotonic() - mono
		suspended := (k.cfg.Clocks.Boottime() - boot - took).Abs()
		if took > removeWithin || suspended > SuspendJump {
			return fmt.Errorf("%s passed since the refresh that confirmed it", took+suspended)
		}
		return k.cfg.FS.Rmdir(k.cfg.Lock)
	})
}

// writeRelease writes releasedAt, and the hand-off to to, into leader.json.
// The caller holds op.
func (k *Keeper) writeRelease(ctx context.Context, to string) error {
	k.mu.Lock()
	rec := k.rec
	k.mu.Unlock()
	now := k.cfg.Clocks.Now().UTC()
	next := rec
	next.ReleasedAt = now
	if to != "" {
		next.Handoff = &store.Handoff{To: to, Until: now.Add(HandoffBackoff)}
	}
	err := k.bounded(ctx, func() error {
		_, rev, err := k.ours(rec)
		if err != nil {
			return err
		}
		return k.save(next, rev)
	})
	var over *tenureOver
	k.mu.Lock()
	defer k.mu.Unlock()
	switch {
	case errors.As(err, &over):
		return k.lost(over.why)
	case err != nil:
		return err
	case !k.holder:
		return ErrNotHolder
	}
	k.rec = next
	return nil
}
