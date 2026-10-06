package lease

import (
	"context"
	"errors"
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Release gives the lease back on a clean stop: no write passes the gate
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
	if err := k.takeOp(ctx); err != nil {
		return err
	}
	defer k.putOp()
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
		// pmxcfs lets any node remove any lock. The refresh right before the
		// rmdir proves the lock still the tenure's, and a keeper that is
		// fenced after it leaves the lock alone.
		err = k.refresh(ctx, false)
	}
	if err == nil {
		err = k.unfenced()
	}
	if err == nil {
		err = k.bounded(ctx, func() error { return k.cfg.FS.Rmdir(k.cfg.Lock) })
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
