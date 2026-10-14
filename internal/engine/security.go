package engine

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// KindSecurity is the kind a query of the events asks for to get the audit
// trail of every node, as the cluster root keeps it.
const KindSecurity = "security"

// auditRetry is how long a write of the audit trail that failed waits
// before it is tried again, when no new event comes first.
const auditRetry = time.Minute

// audited says whether an event is of the audit trail: one marked so, what
// an admin did, and what happened to the lease, which says who wrote when.
func audited(ev Event) bool {
	return ev.Security || ev.Kind == kindAdmin || ev.Kind == kindLease
}

// auditTrail writes the events of the audit trail this node made to its own
// file of the cluster root, beside the event log. The events wait in a
// queue, so that a slow cluster filesystem holds up no one who makes an
// event; the queue keeps the newest store.MaxSecurityEvents, as the file
// does, while the file cannot be written.
type auditTrail struct {
	node  string
	write func(node string, e store.SecurityEvent) error
	log   zerolog.Logger

	mu      sync.Mutex
	pending []store.SecurityEvent
	failing bool // the last write failed; said once until one succeeds
	wake    chan struct{}
}

func newAuditTrail(node string, write func(string, store.SecurityEvent) error, log zerolog.Logger) *auditTrail {
	return &auditTrail{node: node, write: write, log: log, wake: make(chan struct{}, 1)}
}

// add queues the events of the audit trail among events.
func (a *auditTrail) add(events []Event) {
	var mine []store.SecurityEvent
	for _, ev := range events {
		if audited(ev) {
			mine = append(mine, store.SecurityEvent{At: ev.At, Level: ev.Level, Kind: ev.Kind, Subject: ev.Subject, Message: ev.Message, Actor: ev.Actor})
		}
	}
	if len(mine) == 0 {
		return
	}
	a.mu.Lock()
	a.pending = append(a.pending, mine...)
	if n := len(a.pending) - store.MaxSecurityEvents; n > 0 {
		a.pending = slices.Delete(a.pending, 0, n)
	}
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// flush writes what waits, oldest first, and keeps what could not be
// written for the next try.
func (a *auditTrail) flush() {
	a.mu.Lock()
	pending := a.pending
	a.pending = nil
	a.mu.Unlock()
	for i, e := range pending {
		if err := a.write(a.node, e); err != nil {
			a.mu.Lock()
			a.pending = append(slices.Clone(pending[i:]), a.pending...)
			if n := len(a.pending) - store.MaxSecurityEvents; n > 0 {
				a.pending = slices.Delete(a.pending, 0, n)
			}
			said := a.failing
			a.failing = true
			a.mu.Unlock()
			if !said {
				a.log.Warn().Err(err).Int("waiting", len(pending)-i).Msg("writing the audit trail to the cluster root failed; it is tried again")
			}
			return
		}
	}
	a.mu.Lock()
	a.failing = false
	a.mu.Unlock()
}

// run writes the audit trail as events come, and what failed again after
// auditRetry, until ctx ends; then it writes what waits once more.
func (a *auditTrail) run(ctx context.Context, after func(time.Duration) (<-chan time.Time, func() bool)) {
	defer a.flush()
	for {
		a.flush()
		a.mu.Lock()
		retry := len(a.pending) > 0
		a.mu.Unlock()
		var tick <-chan time.Time
		stop := func() bool { return false }
		if retry {
			tick, stop = after(auditRetry)
		}
		select {
		case <-ctx.Done():
			stop()
			return
		case <-a.wake:
			stop()
		case <-tick:
		}
	}
}

// auditEvents are the events of the audit trail of every node, as their
// files in the cluster root keep them, merged by time: the newest limit of
// those that pass every list of q but the kind, which asked for them.
func (e *Engine) auditEvents(q EventQuery, limit int) ([]Event, error) {
	trail, err := e.d.Store.SecurityEvents()
	if err != nil {
		return nil, fmt.Errorf("reading the audit trail: %w", err)
	}
	q.Kind = nil
	var out []Event
	for _, s := range trail {
		ev := Event{
			At: s.At, Level: cmp.Or(s.Level, levelInfo), Kind: s.Kind, Subject: s.Subject, Message: s.Message,
			Actor: s.Actor, Node: s.Node, Security: true,
		}
		if q.Match(ev) {
			out = append(out, ev)
		}
	}
	return nonNil(newest(out, limit)), nil
}
