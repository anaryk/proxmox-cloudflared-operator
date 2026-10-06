package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
)

// noticeRoutes is the most routes a traffic notice carries: 1000 busy targets
// would be about 130 KB every 5 s to every browser.
const noticeRoutes = 100

// maxWhy is the most characters of why there are no figures: every browser is
// sent it, and what nft said of a failed read may be long.
const maxWhy = 200

// Why there are no per-route figures, in the words of the legend of the map.
const (
	whyNotChecked = "the egress filter was not checked yet"
	whyOff        = "the egress filter is off: pco has no per-guest counters"
	whyNotLoaded  = "the egress filter is not loaded"
	whyChanged    = "the egress filter is not the one pco loads"
	whyUnread     = "the counters could not be read: "
)

// RouteTraffic is the rate of the connections the connectors open to the
// target of a route. A target is an address and a port: the routes on one
// share its figure.
type RouteTraffic struct {
	Hostname    string  `json:"hostname"`
	Owner       string  `json:"owner"`
	Target      string  `json:"target"`           // "10.0.0.11:8080"
	FlowsPerSec float64 `json:"flowsPerSec"`      // connections opened per second
	Shared      int     `json:"shared,omitempty"` // other routes on the same target
	Stale       bool    `json:"stale"`            // three reads in a row gave no sample
}

// RouteSample is the rate of new connections to a target over one interval.
type RouteSample struct {
	At          time.Time `json:"at"`
	FlowsPerSec float64   `json:"flowsPerSec"`
}

// RouteSeries is what the counters gave of the target of one route.
type RouteSeries struct {
	Hostname string        `json:"hostname"`
	Target   string        `json:"target"`
	Shared   int           `json:"shared"`
	Samples  []RouteSample `json:"samples"` // oldest first, at most 180
}

// servedRoute is a route a target of the egress filter serves.
type servedRoute struct{ hostname, owner string }

// routeTarget is the target of a route, and the owner of the route.
type routeTarget struct {
	target netip.AddrPort
	owner  string
}

// flowTraffic is what the reads of the counters of the egress filter gave,
// and which routes each target serves. It is guarded by the mutex of traffic.
type flowTraffic struct {
	routes map[string]routeTarget // by hostname
	hosts  []string               // of routes, sorted
	shared map[netip.AddrPort]int // by target: how many routes it serves

	// why says why the last read gave no figures. read says that a read
	// worked since the filter was last seen not on; at and generation are of
	// the last that did.
	why        string
	read       bool
	at         time.Time
	generation string
	// times are those of the reads that gave samples, oldest first: the
	// samples of a series are of the newest of them.
	times  []time.Time
	series map[netip.AddrPort]*flowSeries
	// moving are the routes whose figure was not zero at the last notice.
	moving map[string]bool
}

// flowSeries is what the reads gave of the counter of one target.
type flowSeries struct {
	flows   uint64    // as last read
	misses  int       // reads in a row that gave no sample
	samples []float32 // one per time, NaN where its read gave none
}

// keepServed takes, for each target of the set given to the egress filter,
// the routes it serves.
func (e *Engine) keepServed(served map[netip.AddrPort][]servedRoute) {
	routes := make(map[string]routeTarget)
	shared := make(map[netip.AddrPort]int, len(served))
	for ap, rs := range served {
		shared[ap] = len(rs)
		for _, r := range rs {
			routes[r.hostname] = routeTarget{target: ap, owner: r.owner}
		}
	}
	tr := &e.traffic
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.flows.routes, tr.flows.shared = routes, shared
	tr.flows.hosts = slices.Sorted(maps.Keys(routes))
}

// servedTargets maps each target of the set given to the egress filter to the
// winners it serves: those whose verified address and rule port it is.
func (c *cycleRun) servedTargets() map[netip.AddrPort][]servedRoute {
	given := make(map[netip.AddrPort]bool, len(c.e.egress))
	for _, t := range c.e.egress {
		given[endpoint(t)] = true
	}
	out := map[netip.AddrPort][]servedRoute{}
	for _, rt := range c.claims.Winners {
		t, ok := c.verifiedTarget(rt)
		if ap := endpoint(t); ok && given[ap] {
			out[ap] = append(out[ap], servedRoute{hostname: rt.Hostname, owner: rt.Owner()})
		}
	}
	return out
}

// endpoint is a target as the counters name it.
func endpoint(t egress.Target) netip.AddrPort {
	return netip.AddrPortFrom(t.Addr.WithZone("").Unmap(), t.Port)
}

// SampleTargets reads the counters of the egress filter and records them as
// of t, while the last state says that the filter is on; otherwise it records
// why there are none and asks the filter nothing. A read that fails is no
// problem of the cycle: the figures say why they are missing, also when ctx
// ran out of time; a read the stop cut off records nothing. The stream hears
// of the read with the RecordTraffic of the same round.
func (e *Engine) SampleTargets(ctx context.Context, t time.Time) {
	state := e.egressState()
	var (
		generation string
		counts     map[netip.AddrPort]uint64
		err        error
	)
	if state == EgressOn {
		generation, counts, err = e.d.Egress.FlowCounts(ctx)
		if errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		if err != nil {
			e.d.Log.Debug().Err(err).Msg("reading the counters of the egress filter failed")
		}
	}
	tr := &e.traffic
	tr.mu.Lock()
	defer tr.mu.Unlock()
	switch {
	case state != EgressOn:
		tr.flows.forget(notOn(state))
	case err != nil:
		tr.flows.why = unread(err)
	default:
		tr.flows.add(t, generation, counts)
	}
}

func (e *Engine) egressState() string {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.state.Egress.State
}

// notOn says why there are no counters while the egress filter is in state.
func notOn(state string) string {
	switch state {
	case EgressOff:
		return whyOff
	case EgressNotLoaded:
		return whyNotLoaded
	case EgressChanged:
		return whyChanged
	}
	return whyNotChecked
}

// unread is why a read failed, on one line of at most maxWhy characters.
func unread(err error) string {
	why := []rune(whyUnread + strings.Join(strings.Fields(err.Error()), " "))
	if len(why) > maxWhy {
		return string(why[:maxWhy-3]) + "..."
	}
	return string(why)
}

// forget drops what the reads gave: the counters of a table pco did not load,
// or of none, say nothing of the one it loads next.
func (ft *flowTraffic) forget(why string) {
	ft.why, ft.read, ft.times, ft.series = why, false, nil, nil
}

// add takes a read of the counters at t. The rate of a target is the growth of
// its counter over the time since the last read, on the monotonic clock when
// t has it. A read of another generation gives none, whatever the values: the
// table was loaded anew and every counter of it started again. Neither does a
// counter that went down, as nft resets one when asked. A target the read
// does not have is forgotten.
func (ft *flowTraffic) add(t time.Time, generation string, counts map[netip.AddrPort]uint64) {
	elapsed := t.Sub(ft.at).Seconds()
	sampled := ft.read && generation == ft.generation && elapsed > 0
	if sampled {
		if ft.times == nil {
			ft.times = make([]time.Time, 0, trafficSamples)
		}
		ft.times = push(ft.times, t)
	}
	if ft.series == nil {
		ft.series = make(map[netip.AddrPort]*flowSeries, len(counts))
	}
	maps.DeleteFunc(ft.series, func(ap netip.AddrPort, _ *flowSeries) bool {
		_, counted := counts[ap]
		return !counted
	})
	for ap, n := range counts {
		s := ft.series[ap]
		if s == nil {
			ft.series[ap] = &flowSeries{flows: n, misses: 1, samples: make([]float32, 0, trafficSamples)}
			continue
		}
		switch {
		case sampled && n >= s.flows:
			s.samples, s.misses = push(s.samples, float32(float64(n-s.flows)/elapsed)), 0
		case sampled:
			s.samples, s.misses = push(s.samples, float32(math.NaN())), s.misses+1
		default:
			s.misses++
		}
		s.flows = n
	}
	ft.why, ft.read, ft.at, ft.generation = "", true, t, generation
}

// push appends v and drops the oldest beyond trafficSamples, in place.
func push[T any](s []T, v T) []T {
	if len(s) >= trafficSamples {
		s = slices.Delete(s, 0, len(s)-trafficSamples+1)
	}
	return append(s, v)
}

// now is the newest rate of the series, 0 before the first.
func (s *flowSeries) now() float64 {
	for _, v := range slices.Backward(s.samples) {
		if !math.IsNaN(float64(v)) {
			return widened(v)
		}
	}
	return 0
}

// widened is a rate kept in 32 bits as the float64 that prints the same: 2.4,
// not 2.4000000953674316.
func widened(v float32) float64 {
	f, _ := strconv.ParseFloat(strconv.FormatFloat(float64(v), 'g', -1, 32), 64)
	return f
}

// all is the figure of every route whose target the last read counted, by
// hostname; none while the last read gave none.
func (ft *flowTraffic) all() []RouteTraffic {
	out := []RouteTraffic{}
	if ft.why != "" {
		return out
	}
	for _, host := range ft.hosts {
		rt := ft.routes[host]
		s, ok := ft.series[rt.target]
		if !ok {
			continue
		}
		out = append(out, RouteTraffic{
			Hostname: host, Owner: rt.owner, Target: rt.target.String(), FlowsPerSec: s.now(),
			Shared: ft.shared[rt.target] - 1, Stale: s.misses >= staleAfter,
		})
	}
	return out
}

// notice is what a traffic notice carries of the routes, as NoticeRoutes
// picks them; how many routes have a figure; and why none has.
func (ft *flowTraffic) notice() (routes []RouteTraffic, total int, why string) {
	all := ft.all()
	routes, ft.moving = NoticeRoutes(all, ft.moving)
	return routes, len(all), ft.why
}

// NoticeRoutes are the routes of all a traffic notice carries, at most 100 of
// them: first those whose rate was not zero at the last notice, moving, and
// is zero now, which are told once, so that the cap does not cut them, then
// those with the highest rate now. It returns the routes moving now, for the
// next notice.
func NoticeRoutes(all []RouteTraffic, moving map[string]bool) (routes []RouteTraffic, now map[string]bool) {
	var busy, stopped []RouteTraffic
	now = make(map[string]bool)
	for _, r := range all {
		switch {
		case r.FlowsPerSec > 0:
			busy = append(busy, r)
			now[r.Hostname] = true
		case moving[r.Hostname]:
			stopped = append(stopped, r)
		}
	}
	slices.SortStableFunc(busy, func(a, b RouteTraffic) int { return cmp.Compare(b.FlowsPerSec, a.FlowsPerSec) })
	routes = slices.Concat(stopped, busy)
	return routes[:min(len(routes), noticeRoutes)], now
}

// RouteSeries returns what the counters gave of the target of a route, a
// copy; ErrNotFound for a route without a target.
func (e *Engine) RouteSeries(hostname string) (RouteSeries, error) {
	tr := &e.traffic
	tr.mu.Lock()
	defer tr.mu.Unlock()
	ft := &tr.flows
	rt, ok := ft.routes[hostname]
	if !ok {
		return RouteSeries{}, fmt.Errorf("%w: %s has no target", ErrNotFound, hostname)
	}
	out := RouteSeries{Hostname: hostname, Target: rt.target.String(), Shared: ft.shared[rt.target] - 1, Samples: []RouteSample{}}
	if s, ok := ft.series[rt.target]; ok {
		times := ft.times[len(ft.times)-len(s.samples):]
		for i, v := range s.samples {
			if !math.IsNaN(float64(v)) {
				out.Samples = append(out.Samples, RouteSample{At: times[i], FlowsPerSec: widened(v)})
			}
		}
	}
	return out, nil
}
