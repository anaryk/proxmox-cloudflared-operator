package engine

import (
	"context"
	"sync"
	"time"
)

const (
	// cycleDeadline is the longest a cycle of Run runs: a step still running
	// then is cut, and the cycle holds the rest, as when the daemon stops.
	cycleDeadline = 75 * time.Second
	// dueGrace is how long past its due time a loop that waits for the
	// cycle lock still counts as making progress.
	dueGrace = 15 * time.Second
)

// Progress is what the loop of Run is doing: running a cycle that started
// at StartedAt, or waiting for the next one, due at DueAt. The watchdog of
// the daemon and the heartbeat to the agents read it: an engine that makes
// no progress is restarted, and its lease is not called valid meanwhile.
type Progress struct {
	Running   bool
	StartedAt time.Time
	DueAt     time.Time
}

// progressBook keeps the progress of the loop, beside the cycle lock.
type progressBook struct {
	mu sync.Mutex
	p  Progress
}

func (b *progressBook) get() Progress {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.p
}

func (b *progressBook) started(at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.p.Running, b.p.StartedAt = true, at
}

func (b *progressBook) ended() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.p.Running = false
}

func (b *progressBook) due(at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.p.Running, b.p.DueAt = false, at
}

// Progress is what the loop of Run is doing now.
func (e *Engine) Progress() Progress { return e.progress.get() }

// Healthy says whether the engine makes progress at now: a cycle runs that
// started less than the cycle deadline ago, or the loop waits for a cycle
// that is due, or was due less than dueGrace ago. A loop that waits longer
// for the cycle lock, behind an admin action, is not; an admin action holds
// the lock for at most a little over half a minute, well inside what the
// watchdog allows after the last ping.
func (e *Engine) Healthy(now time.Time) bool { return e.Progress().Healthy(now) }

// Healthy says whether a loop in this state makes progress at now, by the
// rules of the engine's loop: the agent of an agent container, which proves
// the container every half minute, is judged by the same ones.
func (p Progress) Healthy(now time.Time) bool {
	if p.Running {
		return now.Sub(p.StartedAt) < cycleDeadline
	}
	return !p.DueAt.IsZero() && now.Sub(p.DueAt) < dueGrace
}

// LeaseValid is what the heartbeat to the agents says of the lease: valid,
// and for how much longer, only while this node holds it, is not behind,
// and the engine makes progress. An engine that hangs keeps its lease
// refreshed, as the keeper runs beside it; the agents must not take that for
// a live controller.
func (e *Engine) LeaseValid(now time.Time) (bool, time.Duration) {
	if e.d.Lease == nil {
		return false, 0
	}
	st := e.d.Lease.State()
	if !st.Holder || !st.Valid || st.Behind != "" || !e.Healthy(now) {
		return false, 0
	}
	return true, st.ValidFor
}

// timedCycle is the cycle of Run: one with the cycle deadline, which
// counts as progress from the moment it holds the cycle lock.
func (e *Engine) timedCycle(ctx context.Context) State {
	return e.cycle(ctx, cycleDeadline)
}

// beginCycle marks a cycle running, holds it while the test hook says so,
// and gives it its context: the deadline when there is one. The cycle lock
// is held.
func (e *Engine) beginCycle(ctx context.Context, deadline time.Duration) (context.Context, context.CancelFunc) {
	e.progress.started(e.d.Now())
	if hold := e.d.HoldCycle; hold != nil {
		hold(ctx)
	}
	if deadline <= 0 {
		return ctx, func() {}
	}
	return e.timeout(ctx, deadline)
}
