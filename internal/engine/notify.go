package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"
)

// The kinds of a Notice.
const (
	NoticeState   = "state"
	NoticeEvent   = "event"
	NoticeGap     = "gap"
	NoticeTraffic = "traffic"
	NoticeReset   = "reset"
)

const (
	// maxSubscribers is how many streams may be open at once; the web
	// process holds one.
	maxSubscribers = 16
	// A subscriber with this many notices, or bytes of them, waiting is
	// full, and what waits collapses.
	queueNotices = 256
	queueBytes   = 1 << 20
	// A subscriber that has this many waiting even so is told to start over,
	// and its stream ends.
	maxNotices = 4096
	maxBytes   = 4 << 20
	// singleEvents is how many events of the high-volume kinds of one batch
	// go out one by one; the rest go out as one gap.
	singleEvents = 32

	resetBootChanged = "boot changed"
	resetBehind      = "too far behind"
)

// A Notice is one message of the stream; Kind says which of the others is
// set. Every subscriber gets the same notice, which must not be changed.
type Notice struct {
	Kind    string         // "state", "event", "gap", "traffic", "reset"
	State   *StateNotice   // Kind "state"
	Event   *Event         // Kind "event"
	Gap     *GapNotice     // Kind "gap"
	Traffic *TrafficNotice // Kind "traffic"
	Reason  string         // Kind "reset"
}

// StateNotice says that a state was published: the times of its cycle and
// its digest. A client fetches the state only when the digest is not the one
// it holds.
type StateNotice struct {
	At         time.Time `json:"at"`
	FinishedAt time.Time `json:"finishedAt"`
	Digest     string    `json:"digest"`
}

// GapNotice stands for events From to To of the boot that were not sent one
// by one: the events of the high-volume kinds (route, action, claim) of a
// batch beyond the first 32, or such events a slow subscriber missed. Level
// is the highest level among them.
type GapNotice struct {
	Boot  string `json:"boot"`
	From  uint64 `json:"from"`
	To    uint64 `json:"to"`
	Count int    `json:"count"`
	Level string `json:"level"`
}

// TrafficNotice is the newest sample of the traffic.
type TrafficNotice struct {
	At time.Time `json:"at"`
}

// Hello is what a stream begins with: the process of the daemon, the number
// of its last event, the digest of its state and the poll interval of the
// last settings read. Version is the daemon's, which the API fills in.
type Hello struct {
	Boot         string `json:"boot"`
	Version      string `json:"version"`
	Seq          uint64 `json:"seq"`
	Digest       string `json:"digest"`
	PollInterval string `json:"pollInterval"`
}

// streamsFull is why a subscription is refused when every stream is taken.
// It is an ErrBusy.
type streamsFull struct{}

func (streamsFull) Error() string {
	return fmt.Sprintf("%d streams are open already; try again later", maxSubscribers)
}

func (streamsFull) Is(target error) bool { return target == ErrBusy }

// Boot names this process of the daemon: 16 lower-case hex digits, random
// for each.
func (e *Engine) Boot() string { return e.boot }

// Subscribe returns the notices of the stream until ctx ends, when the
// channel is closed. With boot equal to Boot() and after > 0, the events of
// the ring after that seq come first, as the events of a cycle would: beyond
// the first 32 of the high-volume kinds as a gap. With another boot the first
// notice is a reset. What waits for a slow subscriber, from 256 notices or
// 1 MiB on, collapses; one that has 4096 notices or 4 MiB waiting even so is
// sent a reset, and then the channel is closed. It fails with ErrBusy while
// 16 subscribers exist.
func (e *Engine) Subscribe(ctx context.Context, boot string, after uint64) (<-chan Notice, Hello, error) {
	s := newSubscriber()
	hello := Hello{Boot: e.boot, PollInterval: e.PollInterval().String()}
	var err error
	e.events.locked(func(ring []Event, seq uint64) {
		hello.Seq = seq
		err = e.notify.join(s, func(last StateNotice) {
			hello.Digest = last.Digest
			switch {
			case boot != "" && boot != e.boot:
				s.push(queuedOf(Notice{Kind: NoticeReset, Reason: resetBootChanged})...)
			case boot == e.boot && after > 0:
				s.push(queuedOf(replay(ring, after)...)...)
			}
		})
	})
	if err != nil {
		return nil, Hello{}, err
	}
	out := make(chan Notice)
	go s.run(ctx, out, func() { e.notify.leave(s) })
	return out, hello, nil
}

// replay is what a subscriber that saw the events up to after is sent of the
// ring: the events after it as one batch, behind a gap for those the ring no
// longer holds. The levels of those are not known any more; the gap says
// warn, so that a client looks.
func replay(ring []Event, after uint64) []Notice {
	i := slices.IndexFunc(ring, func(ev Event) bool { return ev.Seq > after })
	if i < 0 {
		return nil
	}
	var out []Notice
	if first := ring[i].Seq; first > after+1 {
		out = append(out, Notice{Kind: NoticeGap, Gap: &GapNotice{
			Boot: ring[i].Boot, From: after + 1, To: first - 1, Count: int(first - 1 - after), Level: levelWarn,
		}})
	}
	return append(out, batchNotices(slices.Clone(ring[i:]))...)
}

// highVolume says whether the events of a kind come by the thousand in one
// cycle: one per route in the first cycle after a start, one per applied
// action in the first that enforces, one per claim of a new install.
func highVolume(kind string) bool {
	switch kind {
	case kindRoute, kindAction, kindClaim:
		return true
	}
	return false
}

// batchNotices are the notices of one batch of events: each event on its
// own, but for the events of the high-volume kinds beyond the first 32, which
// are one gap. The gap comes where its last event would, so that the ids of
// the stream only grow.
func batchNotices(batch []Event) []Notice {
	var gap *GapNotice
	high := 0
	for _, ev := range batch {
		if highVolume(ev.Kind) {
			high++
			if high > singleEvents {
				gap = joined(gap, gapOf(ev))
			}
		}
	}
	out := make([]Notice, 0, len(batch)-max(0, high-singleEvents)+1)
	high = 0
	for i := range batch {
		ev := &batch[i]
		if highVolume(ev.Kind) {
			high++
			if high > singleEvents {
				if ev.Seq == gap.To {
					out = append(out, Notice{Kind: NoticeGap, Gap: gap})
				}
				continue
			}
		}
		out = append(out, Notice{Kind: NoticeEvent, Event: ev})
	}
	return out
}

func gapOf(ev Event) GapNotice {
	return GapNotice{Boot: ev.Boot, From: ev.Seq, To: ev.Seq, Count: 1, Level: ev.Level}
}

// joined is one gap that stands for the events of both, a new one: a gap may
// be on its way to other subscribers.
func joined(a *GapNotice, b GapNotice) *GapNotice {
	if a == nil {
		return &b
	}
	g := *a
	g.From, g.To = min(g.From, b.From), max(g.To, b.To)
	g.Count += b.Count
	if levelRank(b.Level) > levelRank(g.Level) {
		g.Level = b.Level
	}
	return &g
}

func levelRank(level string) int {
	switch level {
	case levelError:
		return 2
	case levelWarn:
		return 1
	}
	return 0
}

// notifier hands the notices of the stream to its subscribers.
type notifier struct {
	mu   sync.Mutex
	subs map[*subscriber]bool
	last StateNotice // of the state published last
}

func newNotifier(first StateNotice) *notifier {
	return &notifier{subs: make(map[*subscriber]bool), last: first}
}

// join adds a subscriber, unless every stream is taken, and runs fn with the
// last state notice before any other notice reaches it.
func (n *notifier) join(s *subscriber, fn func(last StateNotice)) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.subs) >= maxSubscribers {
		return streamsFull{}
	}
	n.subs[s] = true
	fn(n.last)
	return nil
}

func (n *notifier) leave(s *subscriber) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.subs, s)
}

// state tells the subscribers of a state that was published.
func (n *notifier) state(sn StateNotice) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.last = sn
	n.deliver(Notice{Kind: NoticeState, State: &sn})
}

// events tells the subscribers of a batch of events, numbered.
func (n *notifier) events(batch []Event) { n.send(batchNotices(batch)...) }

func (n *notifier) send(notices ...Notice) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.deliver(notices...)
}

// deliver queues notices for every subscriber. The caller holds mu.
func (n *notifier) deliver(notices ...Notice) {
	if len(n.subs) == 0 {
		return
	}
	q := queuedOf(notices...)
	for s := range n.subs {
		s.push(q...)
	}
}

// subscriber is one stream: the notices that wait for it, oldest first.
type subscriber struct {
	mu    sync.Mutex
	queue []queued
	bytes int
	// slack and slackBytes are what the last collapse left when it could not
	// bring the queue back within its bounds: it collapses again only once
	// another 256 notices or 1 MiB have come, so that notices that all stay
	// are not walked on every push.
	slack, slackBytes int
	collapses         int  // how often what waits collapsed
	closed            bool // told to start over; nothing is queued after the reset
	wake              chan struct{}
}

func newSubscriber() *subscriber { return &subscriber{wake: make(chan struct{}, 1)} }

type queued struct {
	notice Notice
	size   int // of its JSON
}

func queuedOf(notices ...Notice) []queued {
	out := make([]queued, len(notices))
	for i, n := range notices {
		out[i] = queued{n, sizeOf(n)}
	}
	return out
}

func sizeOf(n Notice) int {
	var v any
	switch n.Kind {
	case NoticeState:
		v = n.State
	case NoticeEvent:
		v = n.Event
	case NoticeGap:
		v = n.Gap
	case NoticeTraffic:
		v = n.Traffic
	default:
		return len(n.Reason)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(data)
}

// push queues notices. When the subscriber is full, what waits collapses;
// when that leaves it past the hard bounds, it is told to start over.
func (s *subscriber) push(qs ...queued) {
	s.mu.Lock()
	for _, q := range qs {
		if s.closed {
			break
		}
		s.queue = append(s.queue, q)
		s.bytes += q.size
		if len(s.queue) > min(queueNotices+s.slack, maxNotices) || s.bytes > min(queueBytes+s.slackBytes, maxBytes) {
			s.collapse()
		}
		if len(s.queue) > maxNotices || s.bytes > maxBytes {
			s.giveUp()
		}
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// collapse makes room in a full queue: the events of the high-volume kinds
// and the gaps become one gap with their highest level, and of the states and
// of the traffic only the newest stays. The events of the other kinds stay,
// and so does the first notice, which may be on its way. The caller holds mu.
func (s *subscriber) collapse() {
	rest := s.queue[1:]
	newest := map[string]int{}
	for i, q := range rest {
		if k := q.notice.Kind; k == NoticeState || k == NoticeTraffic {
			newest[k] = i
		}
	}
	var gap *GapNotice
	kept := []queued{s.queue[0]}
	for i, q := range rest {
		n := q.notice
		switch {
		case n.Kind == NoticeGap:
			gap = joined(gap, *n.Gap)
		case n.Kind == NoticeEvent && highVolume(n.Event.Kind):
			gap = joined(gap, gapOf(*n.Event))
		case n.Kind == NoticeState || n.Kind == NoticeTraffic:
			if newest[n.Kind] == i {
				kept = append(kept, q)
			}
		default:
			kept = append(kept, q)
		}
	}
	if gap != nil {
		at := len(kept)
		if i := slices.IndexFunc(kept[1:], func(q queued) bool {
			return q.notice.Kind == NoticeEvent && q.notice.Event.Seq > gap.To
		}); i >= 0 {
			at = i + 1
		}
		kept = slices.Insert(kept, at, queuedOf(Notice{Kind: NoticeGap, Gap: gap})...)
	}
	s.queue, s.bytes = kept, 0
	for _, q := range kept {
		s.bytes += q.size
	}
	s.collapses++
	s.slack, s.slackBytes = 0, 0
	if len(kept) > queueNotices || s.bytes > queueBytes {
		s.slack, s.slackBytes = len(kept), s.bytes
	}
}

// giveUp drops what waits for a reset, after which the stream ends. The
// first notice stays, as it may be on its way. The caller holds mu.
func (s *subscriber) giveUp() {
	s.queue = append(s.queue[:1:1], queuedOf(Notice{Kind: NoticeReset, Reason: resetBehind})...)
	s.bytes = s.queue[0].size + s.queue[1].size
	s.closed = true
}

// run hands the notices to out, oldest first, until ctx ends or the reset
// of a subscriber told to start over was handed on; then the subscriber
// leaves and out is closed. A notice stays first in the queue until it is
// taken.
func (s *subscriber) run(ctx context.Context, out chan<- Notice, leave func()) {
	defer close(out)
	defer leave()
	for {
		n, ok := s.first()
		if !ok {
			select {
			case <-s.wake:
				continue
			case <-ctx.Done():
				return
			}
		}
		select {
		case out <- n:
			if s.drop() {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *subscriber) first() (Notice, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return Notice{}, false
	}
	return s.queue[0].notice, true
}

// drop takes away the first notice, once it was handed on, and says whether
// that was the last one of a subscriber told to start over.
func (s *subscriber) drop() (last bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bytes -= s.queue[0].size
	s.queue[0] = queued{}
	s.queue = s.queue[1:]
	return s.closed && len(s.queue) == 0
}
