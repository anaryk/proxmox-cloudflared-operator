package lease

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Draw decides the epoch of an acquisition from leader.json as stored and the
// epoch the agent of this node remembers.
//
// leader.json is written before any agent can learn an epoch, so an agent
// that remembers more than leader.json means the store went back, a restore,
// or a second controller: then nothing is drawn and behind says why, and the
// controller observes until pco promote. A record of this node and this
// boot that was not released is kept: the daemon restarted while the lock
// was still its own. Otherwise the generation is one above leader.json's,
// never above agentproto.MaxGeneration, with a new nonce. An automatic draw
// never reads Cloudflare, where whoever holds the API token can forge a
// sentinel.
func Draw(stored store.LeaderRecord, remembered agentproto.Epoch, haveRemembered bool, node, incarnation string, now time.Time, rand io.Reader) (next store.LeaderRecord, behind string, err error) {
	return draw(stored, remembered, haveRemembered, node, incarnation, now, rand, true)
}

// draw is Draw, which keeps a record only when mayKeep: a lock this node made
// again had lapsed, and its tenure ended with it.
func draw(stored store.LeaderRecord, remembered agentproto.Epoch, haveRemembered bool, node, incarnation string,
	now time.Time, rand io.Reader, mayKeep bool) (store.LeaderRecord, string, error) {
	if err := stored.Validate(); err != nil {
		return store.LeaderRecord{}, "", fmt.Errorf("leader.json holds no writer to draw from: %w", err)
	}
	if haveRemembered {
		switch {
		case remembered.InstallID != "" && remembered.InstallID != stored.InstallID:
			return stored, fmt.Sprintf("the agent of this node follows install %s, but leader.json names install %s",
				remembered.InstallID, stored.InstallID), nil
		case remembered.Generation > stored.Generation:
			return stored, fmt.Sprintf("leader.json is at generation %d, below %s that the agent of this node follows: the store went back",
				stored.Generation, remembered), nil
		}
	}
	if mayKeep && stored.Node == node && stored.Incarnation == incarnation && stored.ReleasedAt.IsZero() {
		return stored, "", nil
	}
	next, err := drawAbove(stored, stored.Generation, node, incarnation, now, rand)
	return next, "", err
}

// drawAbove draws an epoch above generation for node, keeping what of stored
// belongs to the install.
func drawAbove(stored store.LeaderRecord, generation int, node, incarnation string, now time.Time, rand io.Reader) (store.LeaderRecord, error) {
	if generation >= agentproto.MaxGeneration {
		return store.LeaderRecord{}, fmt.Errorf("the next generation %d is above %d, the highest an agent accepts",
			generation+1, agentproto.MaxGeneration)
	}
	nonce, err := newNonce(rand)
	if err != nil {
		return store.LeaderRecord{}, err
	}
	return store.LeaderRecord{
		Writer:     planner.Writer{InstallID: stored.InstallID, Generation: generation + 1, Nonce: nonce, Incarnation: incarnation},
		Node:       node,
		AcquiredAt: now,
		CASHA256:   stored.CASHA256,
	}, nil
}

const nonceAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// newNonce returns 8 random lower-case letters and digits, as setup draws
// them: a byte at or above the largest multiple of the alphabet's size is
// drawn again, so that every character is as likely.
func newNonce(rand io.Reader) (string, error) {
	const size, limit = len(nonceAlphabet), 256 - 256%len(nonceAlphabet)
	nonce := make([]byte, 0, 8)
	b := make([]byte, 1)
	for len(nonce) < cap(nonce) {
		if _, err := io.ReadFull(rand, b); err != nil {
			return "", fmt.Errorf("reading random bytes: %w", err)
		}
		if int(b[0]) < limit {
			nonce = append(nonce, nonceAlphabet[int(b[0])%size])
		}
	}
	return string(nonce), nil
}

// ReportedEpoch takes the epoch an agent reported. One that leader.json does
// not explain, of a higher generation than the tenure's or of its generation
// under another nonce, makes the controller behind before the first write of
// the tenure: the store went back. After it, a second controller writes; the
// writes stop and an alarm says so, until the tenure ends or pco promote.
func (k *Keeper) ReportedEpoch(e agentproto.Epoch) {
	k.mu.Lock()
	defer k.mu.Unlock()
	ours := k.rec.Writer
	if !k.holder || e.Generation < ours.Generation || e.Generation == ours.Generation && e.Nonce == ours.Nonce {
		return
	}
	k.highest = max(k.highest, e.Generation)
	why := fmt.Sprintf("an agent follows epoch %s, which leader.json at %s does not explain", e, epochOf(ours))
	switch {
	case !k.firstWrite && k.behind == "":
		k.behind = why + ": the store went back"
		k.log.Error().Str("behind", k.behind).Msg("this controller only observes until pco promote")
	case k.firstWrite && k.alarm == "":
		k.alarm = why + ": another controller writes"
		k.log.Error().Str("why", k.alarm).Msg("writes stopped")
	}
}

// Promote draws an epoch explicitly, above seed, which the caller takes from
// the sentinels at Cloudflare and the agents' reports, above every report the
// keeper took and above what the local agent remembers. It clears behind and
// the alarm. A seed above agentproto.MaxGeneration was forged.
func (k *Keeper) Promote(ctx context.Context, seed int) error {
	if seed > agentproto.MaxGeneration {
		return fmt.Errorf("generation %d is above %d, which no draw makes: it was forged", seed, agentproto.MaxGeneration)
	}
	if err := k.takeOp(ctx); err != nil {
		return err
	}
	defer k.putOp()
	quorate, at := k.cfg.Quorum()
	k.mu.Lock()
	k.look()
	k.noteQuorum(quorate, at)
	err := k.quorumErr()
	if !k.holder || k.stopping {
		err = ErrNotHolder
	}
	rec, highest := k.rec, k.highest
	k.mu.Unlock()
	if err != nil {
		return err
	}
	if err := k.renewed(ctx); err != nil {
		return err
	}

	remembered, have := k.cfg.Remembered()
	if !have {
		remembered = agentproto.Epoch{}
	}
	now := k.cfg.Clocks.Now().UTC()
	var next store.LeaderRecord
	err = k.bounded(ctx, func() error {
		stored, rev, err := k.ours(rec)
		if err != nil {
			return err
		}
		next, err = drawAbove(stored, max(stored.Generation, seed, highest, remembered.Generation), k.cfg.Node, k.cfg.Incarnation, now, k.cfg.Rand)
		if err != nil {
			return err
		}
		return k.save(next, rev)
	})

	k.mu.Lock()
	defer k.mu.Unlock()
	var over *tenureOver
	switch {
	case errors.As(err, &over):
		return k.lost(over.why)
	case err != nil:
		return err
	case !k.holder || !sameEpoch(k.rec.Writer, rec.Writer):
		return ErrNotHolder
	}
	k.rec, k.behind, k.alarm, k.firstWrite, k.highest = next, "", "", false, 0
	k.log.Info().Str("epoch", epochOf(next.Writer)).Msg("promoted: a new epoch")
	return nil
}
