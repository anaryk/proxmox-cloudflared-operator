package apifake

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const (
	// maxSubscribers is how many streams may be open at once, as at the
	// daemon.
	maxSubscribers = 16
	// singleEvents is how many events of the high-volume kinds of a batch
	// go out one by one; the rest go out as one gap, as at the daemon.
	singleEvents = 32
	// maxQueued is how many notices may wait for a stream before it is told
	// to start over and ends.
	maxQueued = 4096
	// noticeRoutes is the most routes a traffic notice carries.
	noticeRoutes = 100
	// keepEvents is how many events of all boots the engine keeps.
	keepEvents = 10 * ringEvents
)

// failure is an error the engine answers with by its own choice or as a
// control asked: its message, and the sentinel of the engine the API answers
// it by; nil for an internal error.
type failure struct {
	sentinel error
	msg      string
}

func (f *failure) Error() string { return f.msg }

func (f *failure) Is(target error) bool { return f.sentinel != nil && target == f.sentinel }

// Subscribe opens a stream as the daemon does: a client that comes back
// with the boot and the seq of the last event it saw is sent the events
// since, one of another boot is told to start over.
func (e *Engine) Subscribe(ctx context.Context, boot string, after uint64) (<-chan engine.Notice, engine.Hello, error) {
	if err := e.begin(ctx, "Subscribe", struct {
		Boot  string `json:"boot"`
		After uint64 `json:"after"`
	}{boot, after}); err != nil {
		return nil, engine.Hello{}, err
	}
	poll := e.PollInterval()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.subs) >= maxSubscribers {
		return nil, engine.Hello{}, &failure{engine.ErrBusy, fmt.Sprintf("%d streams are open already; try again later", maxSubscribers)}
	}
	s := &subscriber{wake: make(chan struct{}, 1), gone: make(chan struct{})}
	switch {
	case boot != "" && boot != e.boot:
		s.push(engine.Notice{Kind: engine.NoticeReset, Reason: "boot changed"})
	case boot == e.boot && after > 0:
		s.push(replay(e.ring(), after)...)
	}
	e.subs[s] = true
	out := make(chan engine.Notice)
	go e.serve(ctx, s, out)
	return out, engine.Hello{Boot: e.boot, Seq: e.seq, Digest: e.state.Digest, PollInterval: poll.String()}, nil
}

// replay is what a client that saw the events up to after is sent: the
// events since, behind a gap for those that are no longer kept.
func replay(ring []engine.Event, after uint64) []engine.Notice {
	i := slices.IndexFunc(ring, func(ev engine.Event) bool { return ev.Seq > after })
	if i < 0 {
		return nil
	}
	var out []engine.Notice
	if first := ring[i].Seq; first > after+1 {
		out = append(out, engine.Notice{Kind: engine.NoticeGap, Gap: &engine.GapNotice{
			Boot: ring[i].Boot, From: after + 1, To: first - 1, Count: int(first - 1 - after), Level: "warn",
		}})
	}
	return append(out, batchNotices(slices.Clone(ring[i:]))...)
}

// highVolume says whether the events of a kind come by the thousand in one
// cycle at the daemon.
func highVolume(kind string) bool { return kind == "route" || kind == "action" || kind == "claim" }

// batchNotices are the notices of one batch of events: each on its own, but
// the events of the high-volume kinds beyond the first 32, which are one gap
// where the last of them would be.
func batchNotices(batch []engine.Event) []engine.Notice {
	var gap *engine.GapNotice
	high := 0
	for _, ev := range batch {
		if !highVolume(ev.Kind) {
			continue
		}
		if high++; high <= singleEvents {
			continue
		}
		if gap == nil {
			gap = &engine.GapNotice{Boot: ev.Boot, From: ev.Seq, Level: ev.Level}
		}
		gap.To, gap.Count = ev.Seq, gap.Count+1
		if levelRank(ev.Level) > levelRank(gap.Level) {
			gap.Level = ev.Level
		}
	}
	out := make([]engine.Notice, 0, len(batch))
	high = 0
	for i := range batch {
		ev := &batch[i]
		if highVolume(ev.Kind) {
			if high++; high > singleEvents {
				if ev.Seq == gap.To {
					out = append(out, engine.Notice{Kind: engine.NoticeGap, Gap: gap})
				}
				continue
			}
		}
		out = append(out, engine.Notice{Kind: engine.NoticeEvent, Event: ev})
	}
	return out
}

func levelRank(level string) int {
	switch level {
	case "error":
		return 2
	case "warn":
		return 1
	}
	return 0
}

// emit numbers events as one batch of the boot, keeps them and tells the
// streams. An event without a time happened now. The caller holds mu.
func (e *Engine) emit(events ...engine.Event) []engine.Event {
	batch := slices.Clone(events)
	for i := range batch {
		if batch[i].At.IsZero() {
			batch[i].At = e.now()
		}
		e.seq++
		batch[i].Seq, batch[i].Boot = e.seq, e.boot
	}
	e.events = append(e.events, batch...)
	if n := len(e.events) - keepEvents; n > 0 {
		e.events = slices.Delete(e.events, 0, n)
	}
	e.deliver(batchNotices(slices.Clone(batch))...)
	return batch
}

// deliver queues notices for every stream. The caller holds mu.
func (e *Engine) deliver(notices ...engine.Notice) {
	for s := range e.subs {
		s.push(notices...)
	}
}

// subscriber is one stream: the notices that wait for it, oldest first.
type subscriber struct {
	mu     sync.Mutex
	queue  []engine.Notice
	ending bool          // told to start over: nothing is queued after the reset
	wake   chan struct{} // a notice was queued
	gone   chan struct{} // closed when the stream is dropped
}

func (s *subscriber) push(notices ...engine.Notice) {
	s.mu.Lock()
	for _, n := range notices {
		if s.ending {
			break
		}
		s.queue = append(s.queue, n)
		if len(s.queue) > maxQueued {
			// The first notice may be on its way.
			s.queue = append(s.queue[:1:1], engine.Notice{Kind: engine.NoticeReset, Reason: "too far behind"})
			s.ending = true
		}
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *subscriber) first() (engine.Notice, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return engine.Notice{}, false
	}
	return s.queue[0], true
}

// pop takes the first notice away once it was handed on, and says whether
// it was the last of a stream told to start over.
func (s *subscriber) pop() (last bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = s.queue[1:]
	return s.ending && len(s.queue) == 0
}

// serve hands the notices of a stream on, but not while the streams are
// paused, until the client goes or the stream is dropped; then out is
// closed.
func (e *Engine) serve(ctx context.Context, s *subscriber, out chan<- engine.Notice) {
	defer close(out)
	defer e.leave(s)
	for {
		if resumed := e.pausedUntil(); resumed != nil {
			select {
			case <-resumed:
				continue
			case <-s.gone:
				return
			case <-ctx.Done():
				return
			}
		}
		n, ok := s.first()
		if !ok {
			select {
			case <-s.wake:
				continue
			case <-s.gone:
				return
			case <-ctx.Done():
				return
			}
		}
		select {
		case out <- n:
			if s.pop() {
				return
			}
		case <-s.gone:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (e *Engine) pausedUntil() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.resumed == nil {
		return nil
	}
	return e.resumed
}

func (e *Engine) leave(s *subscriber) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.subs, s)
}

// Subscribers is how many streams are open.
func (e *Engine) Subscribers() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.subs)
}

// Pause stops the notices of every stream until Resume, as a daemon whose
// cycles hang would: the streams stay open and keep their pings, what
// happens meanwhile waits, and no cycle runs and no traffic is sampled.
func (e *Engine) Pause() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.resumed == nil {
		e.resumed = make(chan struct{})
	}
}

// Resume sends the notices again, those that waited first.
func (e *Engine) Resume() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.resumed != nil {
		close(e.resumed)
		e.resumed = nil
	}
}

// DropStreams ends every stream, as a restart of the daemon would, but the
// boot stays: a client that comes back resumes where it was.
func (e *Engine) DropStreams() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.drop()
}

func (e *Engine) drop() {
	for s := range e.subs {
		close(s.gone)
	}
	clear(e.subs)
}

// Restart restarts the daemon: a new boot, whose events are numbered from 1,
// and every stream ends. A client that comes back with the old boot is told
// to start over.
func (e *Engine) Restart() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.restart()
}

func (e *Engine) restart() {
	e.boot, e.seq = newBoot(), 0
	e.drop()
}

// SetState serves st from now on, named by its digest, which it returns, and
// tells the streams as a cycle would. The times of its cycle are those of the
// next cycles too. st is the engine's then: the caller must not change it.
func (e *Engine) SetState(st engine.State) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	sortState(&st)
	st.Digest = digestOf(st)
	e.cycle = 0
	if !st.At.IsZero() && st.FinishedAt.After(st.At) {
		e.cycle = st.FinishedAt.Sub(st.At)
	}
	e.state = st
	e.deliver(stateNotice(st))
	return st.Digest
}

// AddEvents adds events as one batch, as a cycle would, and returns them as
// they were numbered.
func (e *Engine) AddEvents(events ...engine.Event) []engine.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.emit(events...)
}

// Cycle publishes the state again as a cycle that ended now would: the same
// digest, with the times of the new cycle. A state without a cycle, and a
// daemon whose streams are paused, have none.
func (e *Engine) Cycle() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.resumed != nil || e.state.At.IsZero() {
		return
	}
	now := e.now()
	e.state.At, e.state.FinishedAt = now.Add(-e.cycle), now
	e.deliver(stateNotice(e.state))
}

// Sample takes a sample of the traffic now, with the figures of the last
// one, and tells the streams; not while they are paused.
func (e *Engine) Sample() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.resumed != nil {
		return
	}
	now := e.now()
	tunnels := slices.Clone(e.traffic.Tunnels)
	for i := range tunnels {
		if n := len(tunnels[i].Samples); n > 0 {
			s := tunnels[i].Samples[n-1]
			s.At = now
			tunnels[i].Samples = push(tunnels[i].Samples, s)
		}
	}
	e.traffic.At, e.traffic.Tunnels = now, tunnels
	for _, r := range e.traffic.Routes {
		e.series[r.Hostname] = push(e.series[r.Hostname], engine.RouteSample{At: now, FlowsPerSec: r.FlowsPerSec})
	}
	e.deliver(e.trafficNotice())
}

// push is s with v at its end, the oldest dropped beyond keepSamples, in a
// new slice: a copy handed out keeps what it had.
func push[T any](s []T, v T) []T {
	out := make([]T, 0, keepSamples)
	out = append(out, s[max(0, len(s)-keepSamples+1):]...)
	return append(out, v)
}

// trafficNotice is the newest figures of every tunnel and of the routes, as
// the daemon sends them: first the routes that stopped since the last
// notice, then those with the highest rate, at most noticeRoutes. The caller
// holds mu.
func (e *Engine) trafficNotice() engine.Notice {
	tv := e.traffic
	n := &engine.TrafficNotice{At: tv.At, Tunnels: make([]engine.TunnelNotice, 0, len(tv.Tunnels)), RoutesTotal: len(tv.Routes), RoutesWhy: tv.RoutesWhy}
	for _, t := range tv.Tunnels {
		tn := engine.TunnelNotice{TunnelID: t.TunnelID, HAConnections: t.HAConnections, Stale: t.Stale}
		if k := len(t.Samples); k > 0 {
			s := t.Samples[k-1]
			tn.RPS, tn.ErrorsPerSec, tn.Concurrent, tn.Sampled = s.RPS, s.ErrorsPerSec, s.Concurrent, true
		}
		n.Tunnels = append(n.Tunnels, tn)
	}
	var busy, stopped []engine.RouteTraffic
	moving := map[string]bool{}
	for _, r := range tv.Routes {
		switch {
		case r.FlowsPerSec > 0:
			busy = append(busy, r)
			moving[r.Hostname] = true
		case e.moving[r.Hostname]:
			stopped = append(stopped, r)
		}
	}
	e.moving = moving
	slices.SortStableFunc(busy, func(a, b engine.RouteTraffic) int { return cmp.Compare(b.FlowsPerSec, a.FlowsPerSec) })
	routes := slices.Concat(stopped, busy)
	n.Routes = routes[:min(len(routes), noticeRoutes)]
	return engine.Notice{Kind: engine.NoticeTraffic, Traffic: n}
}

// TrafficChange is what a control changes of the traffic: the figures of
// tunnels, by tunnel id, and of routes, by hostname, and why the routes have
// none. A route the traffic does not have yet is added with its owner and
// target.
type TrafficChange struct {
	Tunnels   []engine.TunnelNotice `json:"tunnels"`
	Routes    []engine.RouteTraffic `json:"routes"`
	RoutesWhy *string               `json:"routesWhy"`
}

// ChangeTraffic takes a sample of the traffic now with the figures of c, and
// tells the streams.
func (e *Engine) ChangeTraffic(c TrafficChange) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	tunnels := slices.Clone(e.traffic.Tunnels)
	for _, tn := range c.Tunnels {
		i := slices.IndexFunc(tunnels, func(t engine.TunnelTraffic) bool { return t.TunnelID == tn.TunnelID })
		if i < 0 {
			return fmt.Errorf("the traffic has no tunnel %s", tn.TunnelID)
		}
		t := &tunnels[i]
		t.Samples = push(t.Samples, engine.TrafficSample{At: now, RPS: tn.RPS, ErrorsPerSec: tn.ErrorsPerSec, Concurrent: tn.Concurrent})
		t.HAConnections, t.Stale = tn.HAConnections, tn.Stale
	}
	routes := slices.Clone(e.traffic.Routes)
	for _, r := range c.Routes {
		i := slices.IndexFunc(routes, func(x engine.RouteTraffic) bool { return x.Hostname == r.Hostname })
		switch {
		case i >= 0:
			routes[i].FlowsPerSec, routes[i].Stale = r.FlowsPerSec, r.Stale
		case r.Owner == "" || r.Target == "":
			return fmt.Errorf("the traffic has no route %s; a new one needs its owner and target", r.Hostname)
		default:
			routes = append(routes, r)
		}
		e.series[r.Hostname] = push(e.series[r.Hostname], engine.RouteSample{At: now, FlowsPerSec: r.FlowsPerSec})
	}
	slices.SortStableFunc(routes, func(a, b engine.RouteTraffic) int { return cmp.Compare(a.Hostname, b.Hostname) })
	if c.RoutesWhy != nil {
		e.traffic.RoutesWhy = *c.RoutesWhy
	}
	if e.traffic.RoutesWhy != "" {
		routes = []engine.RouteTraffic{}
		clear(e.series)
	}
	e.traffic.At, e.traffic.Tunnels, e.traffic.Routes = now, tunnels, routes
	e.deliver(e.trafficNotice())
	return nil
}

// Run runs the cycles and the samples of the traffic as the daemon does,
// until ctx ends: a cycle a poll interval of the settings after the last one
// ended, and a sample every 5 s.
func (e *Engine) Run(ctx context.Context) {
	sample := time.NewTicker(engine.TrafficInterval)
	defer sample.Stop()
	cycle := time.NewTimer(e.nextCycle())
	defer cycle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sample.C:
			e.Sample()
		case <-cycle.C:
			e.Cycle()
			cycle.Reset(e.nextCycle())
		}
	}
}

// nextCycle is how long the next cycle takes to come: the poll interval,
// and then the cycle itself.
func (e *Engine) nextCycle() time.Duration {
	poll := e.PollInterval()
	e.mu.Lock()
	defer e.mu.Unlock()
	return poll + e.cycle
}
