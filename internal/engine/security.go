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

// kindAudit is the event kind of the audit trail itself: events of it that
// were lost.
const kindAudit = "audit"

// auditTrail writes the events of the audit trail this node made to its own
// file of the cluster root, beside the event log. The events wait in a
// queue, so that a slow cluster filesystem holds up no one who makes an
// event; the queue keeps the newest store.MaxSecurityEvents, as the file
// does, while the file cannot be written, and counts those it drops.
type auditTrail struct {
	node  string
	write func(node string, events []store.SecurityEvent) error
	// lost is told how many events were dropped, once the trail is written
	// again; it may be nil.
	lost func(n int)
	log  zerolog.Logger

	mu      sync.Mutex
	pending []store.SecurityEvent
	dropped int  // dropped from the queue since the last write that worked
	failing bool // the last write failed; said once until one succeeds
	wake    chan struct{}
}

func newAuditTrail(node string, write func(string, []store.SecurityEvent) error, log zerolog.Logger) *auditTrail {
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
	a.trim()
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// trim drops the oldest events beyond what the file keeps, and counts them.
// The caller holds the lock.
func (a *auditTrail) trim() {
	if n := len(a.pending) - store.MaxSecurityEvents; n > 0 {
		a.pending = slices.Delete(a.pending, 0, n)
		a.dropped += n
	}
}

// flush writes what waits in one write, and keeps it for the next try when
// the write fails. Once one works after events were dropped, that is said
// in the log and, through lost, in the trail.
func (a *auditTrail) flush() {
	a.mu.Lock()
	pending := a.pending
	a.pending = nil
	a.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	if err := a.write(a.node, pending); err != nil {
		a.mu.Lock()
		a.pending = append(pending, a.pending...)
		a.trim()
		said := a.failing
		a.failing = true
		a.mu.Unlock()
		if !said {
			a.log.Warn().Err(err).Int("waiting", len(pending)).Msg("writing the audit trail to the cluster root failed; it is tried again")
		}
		return
	}
	a.mu.Lock()
	a.failing = false
	dropped := a.dropped
	a.dropped = 0
	a.mu.Unlock()
	if dropped > 0 {
		a.log.Warn().Int("lost", dropped).Msg("events of the audit trail were lost while the cluster root could not be written")
		if a.lost != nil {
			a.lost(dropped)
		}
	}
}

// run writes the audit trail as events come, and what failed again after
// auditRetry, until ctx ends; then it writes what waits once more, and says
// in the log how many events that could not be written are lost.
func (a *auditTrail) run(ctx context.Context, after func(time.Duration) (<-chan time.Time, func() bool)) {
	defer func() {
		a.flush()
		a.mu.Lock()
		left := len(a.pending) + a.dropped
		a.mu.Unlock()
		if left > 0 {
			a.log.Warn().Int("lost", left).Msg("events of the audit trail are lost: the cluster root could not be written before the engine stopped")
		}
	}()
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
// those that pass every list of q but the kind, which asked for them. The
// trail has no numbers: a query that reads on from a position is refused. A
// file that cannot be read is left out, and named in the log.
func (e *Engine) auditEvents(q EventQuery, limit int) ([]Event, error) {
	if q.After != 0 || q.Boot != "" {
		return nil, fmt.Errorf("%w: the audit trail is read whole, by time: it has no position to read on from", ErrInvalid)
	}
	trail, skipped, err := e.d.Store.SecurityTrail()
	if err != nil {
		return nil, fmt.Errorf("reading the audit trail: %w", err)
	}
	for _, why := range skipped {
		e.d.Log.Warn().Str("why", why).Msg("a file of the audit trail is left out")
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

// noteAuditLost says in the event log, and so in the journal and the trail,
// that events of the audit trail were lost.
func (e *Engine) noteAuditLost(n int) {
	e.events.add(Event{At: e.d.Now(), Level: levelWarn, Kind: kindAudit, Subject: e.d.Node, Security: true,
		Message: fmt.Sprintf("%d audit events were lost: the cluster root could not be written while more than %d waited", n, store.MaxSecurityEvents)})
}

// reservedKinds are the kinds of the controller's audit trail and of its
// own findings about nodes, which an event of an agent must not pass for:
// such an event is kept as agent:<kind>.
var reservedKinds = map[string]bool{
	kindAdmin: true, kindLease: true, KindSecurity: true, kindAudit: true, kindEnrol: true, kindRotation: true,
	kindBlock: true, kindAttestation: true, kindPlacement: true,
}

// agentKind is the kind an event an agent sent is kept under.
func agentKind(kind string) string {
	if reservedKinds[kind] {
		return "agent:" + kind
	}
	return kind
}
