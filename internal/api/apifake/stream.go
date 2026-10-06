package apifake

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const (
	// maxSubscribers is how many streams may be open at once, as at the
	// daemon.
	maxSubscribers = 16
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

// subscriber is one stream: what waits for it, queued as the daemon queues
// it, and whether it was dropped.
type subscriber struct {
	queue *engine.Queue
	gone  chan struct{} // closed when the stream is dropped
}

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
	s := &subscriber{queue: engine.NewQueue(), gone: make(chan struct{})}
	switch {
	case boot != "" && boot != e.boot:
		s.queue.Push(engine.Notice{Kind: engine.NoticeReset, Reason: "boot changed"})
	case boot == e.boot && after > 0:
		s.queue.Push(engine.Replay(e.ring(), after)...)
	}
	e.subs[s] = true
	out := make(chan engine.Notice)
	go e.serve(ctx, s, out)
	return out, engine.Hello{Boot: e.boot, Seq: e.seq, Digest: e.state.Digest, PollInterval: poll.String()}, nil
}

// emit numbers events as one batch of the boot, keeps them and tells the
// streams, as the daemon does. An event without a time happened now. The
// caller holds mu.
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
	e.deliver(engine.BatchNotices(slices.Clone(batch))...)
	return batch
}

// deliver queues notices for every stream. The caller holds mu.
func (e *Engine) deliver(notices ...engine.Notice) {
	for s := range e.subs {
		s.queue.Push(notices...)
	}
}

// serve hands the notices of a stream on, but not while the streams are
// paused, when they wait in the queue as for a slow client, until the client
// goes, the stream is dropped or its queue ends it; then out is closed.
func (e *Engine) serve(ctx context.Context, s *subscriber, out chan<- engine.Notice) {
	defer close(out)
	defer e.leave(s)
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	queued := make(chan engine.Notice)
	go s.queue.Run(ctx, queued)
	for {
		if !e.wait(ctx, s) {
			return
		}
		var n engine.Notice
		select {
		case got, ok := <-queued:
			if !ok {
				return
			}
			n = got
		case <-s.gone:
			return
		case <-ctx.Done():
			return
		}
		// The streams may have been paused while it waited.
		if !e.wait(ctx, s) {
			return
		}
		select {
		case out <- n:
		case <-s.gone:
			return
		case <-ctx.Done():
			return
		}
	}
}

// wait waits while the streams are paused, and says whether the stream goes
// on.
func (e *Engine) wait(ctx context.Context, s *subscriber) bool {
	for {
		resumed := e.pausedUntil()
		if resumed == nil {
			return true
		}
		select {
		case <-resumed:
		case <-s.gone:
			return false
		case <-ctx.Done():
			return false
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
// happens meanwhile waits in their queues, and Run neither cycles nor
// samples the traffic.
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
	e.resume()
}

func (e *Engine) resume() {
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
// every stream ends, and the new process is not paused. A client that comes
// back with the old boot is told to start over.
func (e *Engine) Restart() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.restart()
}

func (e *Engine) restart() {
	e.boot, e.seq = newBoot(), 0
	e.drop()
	e.resume()
}

// SetState serves st from now on, named by its digest, which it returns, and
// tells the streams as a cycle would. The times of its cycle are those of the
// next cycles too. st is the engine's then: the caller must not change it.
func (e *Engine) SetState(st engine.State) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	sortState(&st)
	st.Digest = engine.DigestOf(st)
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
// digest, with the times of the new cycle. It runs when it is asked for, as
// by Trigger or a control, also while the streams are paused, whose clients
// hear of it when they resume, and also for a state without a cycle yet,
// which has its first then.
func (e *Engine) Cycle() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cycleNow()
}

func (e *Engine) cycleNow() {
	now := e.now()
	e.state.At, e.state.FinishedAt = now.Add(-e.cycle), now
	e.deliver(stateNotice(e.state))
}

// tick is a cycle of Run: none while the streams are paused, and none for a
// state without a cycle yet, which waits for one to be asked for.
func (e *Engine) tick() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.resumed == nil && !e.state.At.IsZero() {
		e.cycleNow()
	}
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
	for _, r := range e.routeFigures() {
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

// trafficNotice is the newest figures of every tunnel and the routes the
// daemon would send of them. The caller holds mu.
func (e *Engine) trafficNotice() engine.Notice {
	tv := e.traffic
	routes := e.routeFigures()
	n := &engine.TrafficNotice{At: tv.At, Tunnels: make([]engine.TunnelNotice, 0, len(tv.Tunnels)), RoutesTotal: len(routes), RoutesWhy: tv.RoutesWhy}
	for _, t := range tv.Tunnels {
		tn := engine.TunnelNotice{TunnelID: t.TunnelID, HAConnections: t.HAConnections, Stale: t.Stale}
		if k := len(t.Samples); k > 0 {
			s := t.Samples[k-1]
			tn.RPS, tn.ErrorsPerSec, tn.Concurrent, tn.Sampled = s.RPS, s.ErrorsPerSec, s.Concurrent, true
		}
		n.Tunnels = append(n.Tunnels, tn)
	}
	n.Routes, e.moving = engine.NoticeRoutes(routes, e.moving)
	return engine.Notice{Kind: engine.NoticeTraffic, Traffic: n}
}

// TrafficChange is what a control changes of the traffic: figures of
// tunnels, by tunnel id, and of routes, by hostname, and why the routes have
// none. A figure left out keeps what it was. A route the traffic does not
// have yet is added with its owner and target. While routesWhy is not
// empty no route has a figure, as at the daemon; the figures are kept, and
// an empty routesWhy shows them again.
type TrafficChange struct {
	Tunnels   []TunnelFigures `json:"tunnels"`
	Routes    []RouteFigures  `json:"routes"`
	RoutesWhy *string         `json:"routesWhy"`
}

// TunnelFigures are the figures of a tunnel a TrafficChange sets.
type TunnelFigures struct {
	TunnelID      string   `json:"tunnelId"`
	RPS           *float64 `json:"rps"`
	ErrorsPerSec  *float64 `json:"errorsPerSec"`
	Concurrent    *float64 `json:"concurrent"`
	HAConnections *int     `json:"haConnections"`
	Stale         *bool    `json:"stale"`
}

// RouteFigures are the figures of a route a TrafficChange sets.
type RouteFigures struct {
	Hostname    string   `json:"hostname"`
	Owner       string   `json:"owner"`
	Target      string   `json:"target"`
	FlowsPerSec *float64 `json:"flowsPerSec"`
	Shared      *int     `json:"shared"`
	Stale       *bool    `json:"stale"`
}

// set is v, or else what is.
func set[T any](is *T, v *T) {
	if v != nil {
		*is = *v
	}
}

// ChangeTraffic takes a sample of the traffic now with the figures of c, and
// tells the streams.
func (e *Engine) ChangeTraffic(c TrafficChange) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	tunnels := slices.Clone(e.traffic.Tunnels)
	for _, f := range c.Tunnels {
		i := slices.IndexFunc(tunnels, func(t engine.TunnelTraffic) bool { return t.TunnelID == f.TunnelID })
		if i < 0 {
			return fmt.Errorf("the traffic has no tunnel %s", f.TunnelID)
		}
		t := &tunnels[i]
		var s engine.TrafficSample
		if k := len(t.Samples); k > 0 {
			s = t.Samples[k-1]
		}
		s.At = now
		set(&s.RPS, f.RPS)
		set(&s.ErrorsPerSec, f.ErrorsPerSec)
		set(&s.Concurrent, f.Concurrent)
		t.Samples = push(t.Samples, s)
		set(&t.HAConnections, f.HAConnections)
		set(&t.Stale, f.Stale)
	}
	routes := slices.Clone(e.traffic.Routes)
	for _, f := range c.Routes {
		i := slices.IndexFunc(routes, func(r engine.RouteTraffic) bool { return r.Hostname == f.Hostname })
		if i < 0 {
			if f.Owner == "" || f.Target == "" {
				return fmt.Errorf("the traffic has no route %s; a new one needs its owner and target", f.Hostname)
			}
			routes = append(routes, engine.RouteTraffic{Hostname: f.Hostname, Owner: f.Owner, Target: f.Target})
			i = len(routes) - 1
		}
		r := &routes[i]
		set(&r.FlowsPerSec, f.FlowsPerSec)
		set(&r.Shared, f.Shared)
		set(&r.Stale, f.Stale)
		e.series[r.Hostname] = push(e.series[r.Hostname], engine.RouteSample{At: now, FlowsPerSec: r.FlowsPerSec})
	}
	slices.SortStableFunc(routes, func(a, b engine.RouteTraffic) int { return cmp.Compare(a.Hostname, b.Hostname) })
	set(&e.traffic.RoutesWhy, c.RoutesWhy)
	e.traffic.At, e.traffic.Tunnels, e.traffic.Routes = now, tunnels, routes
	e.deliver(e.trafficNotice())
	return nil
}

// Run runs the cycles and the samples of the traffic as the daemon does,
// until ctx ends: a cycle a poll interval of the settings after the last one
// ended, and a sample every 5 s.
func (e *Engine) Run(ctx context.Context) {
	e.run(ctx, engine.TrafficInterval, e.nextCycle)
}

func (e *Engine) run(ctx context.Context, sampleEvery time.Duration, nextCycle func() time.Duration) {
	sample := time.NewTicker(sampleEvery)
	defer sample.Stop()
	cycle := time.NewTimer(nextCycle())
	defer cycle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sample.C:
			e.Sample()
		case <-cycle.C:
			e.tick()
			cycle.Reset(nextCycle())
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
