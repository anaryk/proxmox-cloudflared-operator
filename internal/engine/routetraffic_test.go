package engine

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

const targetWWW = "10.0.0.11:8080"

// counting is an env whose egress filter is on, after a cycle that gave the
// filter the targets of the guests, or of web-1 publishing www.example.com.
func counting(t *testing.T, guests ...model.Guest) *env {
	t.Helper()
	e := newEnv(t)
	if len(guests) > 0 {
		e.inv.set(snapshot(guests...))
	}
	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressOn}})
	e.cycle()
	return e
}

// readFlows is the read of the counters in the n-th round, which finds these,
// by "addr:port", in a table of this generation.
func readFlows(e *env, n int, generation string, counts map[string]uint64) {
	e.egr.count(generation, counts)
	e.eng.SampleTargets(e.t.Context(), at(n))
}

func www(flowsPerSec float64) RouteTraffic {
	return RouteTraffic{Hostname: "www.example.com", Owner: "qemu/101", Target: targetWWW, FlowsPerSec: flowsPerSec}
}

func seriesOf(t *testing.T, e *Engine, hostname string) RouteSeries {
	t.Helper()
	s, err := e.RouteSeries(hostname)
	require.NoError(t, err)
	return s
}

// trafficNotices are the traffic notices among what came on the stream.
func trafficNotices(notices []Notice) []TrafficNotice {
	var out []TrafficNotice
	for _, n := range notices {
		if n.Kind == NoticeTraffic {
			out = append(out, *n.Traffic)
		}
	}
	return out
}

func TestFlowsPerSecondComeFromTheCountersOfTwoReads(t *testing.T) {
	e := counting(t)

	readFlows(e, 0, "3", map[string]uint64{targetWWW: 100})
	require.Equal(t, []RouteTraffic{www(0)}, e.eng.Traffic().Routes, "one read gives no rate")

	readFlows(e, 1, "3", map[string]uint64{targetWWW: 112})

	v := e.eng.Traffic()
	require.Equal(t, []RouteTraffic{www(2.4)}, v.Routes)
	require.Equal(t, 1, v.RoutesTotal)
	require.Empty(t, v.RoutesWhy)
	require.Equal(t, RouteSeries{
		Hostname: "www.example.com", Target: targetWWW,
		Samples: []RouteSample{{At: at(1), FlowsPerSec: 2.4}},
	}, seriesOf(t, e.eng, "www.example.com"))
}

// Filter.sync loads the table anew whenever a target is added, and every
// counter of it starts from zero then: one may already be above the value
// read before, so only the generation tells.
func TestAReadOfAnotherGenerationGivesNoSample(t *testing.T) {
	e := counting(t)
	readFlows(e, 0, "3", map[string]uint64{targetWWW: 100})
	readFlows(e, 1, "3", map[string]uint64{targetWWW: 110})

	readFlows(e, 2, "4", map[string]uint64{targetWWW: 200})

	require.Equal(t, []RouteSample{{At: at(1), FlowsPerSec: 2}}, seriesOf(t, e.eng, "www.example.com").Samples)
	require.Equal(t, []RouteTraffic{www(2)}, e.eng.Traffic().Routes, "the newest sample stands")

	readFlows(e, 3, "4", map[string]uint64{targetWWW: 205})

	require.Equal(t, []RouteSample{{At: at(1), FlowsPerSec: 2}, {At: at(3), FlowsPerSec: 1}},
		seriesOf(t, e.eng, "www.example.com").Samples)
}

// nft resets the counters of a set when asked to, and the table keeps its
// handle.
func TestACounterThatWentDownGivesNoSample(t *testing.T) {
	e := counting(t)
	readFlows(e, 0, "3", map[string]uint64{targetWWW: 100})
	readFlows(e, 1, "3", map[string]uint64{targetWWW: 110})

	readFlows(e, 2, "3", map[string]uint64{targetWWW: 4})
	readFlows(e, 3, "3", map[string]uint64{targetWWW: 9})

	require.Equal(t, []RouteSample{{At: at(1), FlowsPerSec: 2}, {At: at(3), FlowsPerSec: 1}},
		seriesOf(t, e.eng, "www.example.com").Samples)
}

func TestATargetGoneFromTheSetLosesItsSeries(t *testing.T) {
	e := counting(t, guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :9090"))
	readFlows(e, 0, "3", map[string]uint64{targetWWW: 0, "10.0.0.11:9090": 0})
	readFlows(e, 1, "3", map[string]uint64{targetWWW: 5, "10.0.0.11:9090": 10})
	require.Len(t, seriesOf(t, e.eng, "api.example.com").Samples, 1)

	readFlows(e, 2, "3", map[string]uint64{targetWWW: 10})

	v := e.eng.Traffic()
	require.Equal(t, []RouteTraffic{www(1)}, v.Routes)
	require.Equal(t, 1, v.RoutesTotal)
	require.Empty(t, seriesOf(t, e.eng, "api.example.com").Samples)

	readFlows(e, 3, "3", map[string]uint64{targetWWW: 15, "10.0.0.11:9090": 50})
	readFlows(e, 4, "3", map[string]uint64{targetWWW: 20, "10.0.0.11:9090": 60})
	require.Equal(t, []RouteSample{{At: at(4), FlowsPerSec: 2}}, seriesOf(t, e.eng, "api.example.com").Samples,
		"one that comes back starts anew")
}

// A target is an address and a port: two routes to the same port of a guest
// share its counter.
func TestTwoRoutesOnOneTargetShareItsFigure(t *testing.T) {
	e := counting(t, guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :8080"))
	readFlows(e, 0, "3", map[string]uint64{targetWWW: 0})
	readFlows(e, 1, "3", map[string]uint64{targetWWW: 20})

	v := e.eng.Traffic()

	require.Equal(t, []RouteTraffic{
		{Hostname: "api.example.com", Owner: "qemu/101", Target: targetWWW, FlowsPerSec: 4, Shared: 1},
		{Hostname: "www.example.com", Owner: "qemu/101", Target: targetWWW, FlowsPerSec: 4, Shared: 1},
	}, v.Routes)
	require.Equal(t, 2, v.RoutesTotal)
	s := seriesOf(t, e.eng, "api.example.com")
	require.Equal(t, 1, s.Shared)
	require.Equal(t, []RouteSample{{At: at(1), FlowsPerSec: 4}}, s.Samples)
}

func TestARouteOfAllowNodeIsCountedInTheAllownodeSets(t *testing.T) {
	e := newEnv(t)
	e.inv.set(snapshot())
	require.NoError(t, e.store.SaveManualRoute(model.Route{
		Hostname: "pve.example.com", ManualID: "pve", Source: model.SourceManual,
		Target:  model.Target{Scheme: model.SchemeHTTPS, Addr: nodeAddr, Port: 8006},
		Options: model.RouteOptions{AllowNode: true, NoTLSVerify: true},
	}))
	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressOn}})
	e.cycle()
	node := netip.AddrPortFrom(nodeAddr, 8006).String()

	readFlows(e, 0, "3", map[string]uint64{node: 1})
	readFlows(e, 1, "3", map[string]uint64{node: 6})

	require.Equal(t, []RouteTraffic{{Hostname: "pve.example.com", Owner: "manual/pve", Target: node, FlowsPerSec: 1}},
		e.eng.Traffic().Routes)
}

func TestARouteWithoutATargetHasNoFigure(t *testing.T) {
	e := newEnv(t)
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :9090")))
	e.res.rejected["api.example.com"] = "the address is not the guest's"
	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressOn}})
	e.cycle()

	readFlows(e, 0, "3", map[string]uint64{targetWWW: 0, "10.0.0.11:9090": 0})
	readFlows(e, 1, "3", map[string]uint64{targetWWW: 5, "10.0.0.11:9090": 5})

	v := e.eng.Traffic()
	require.Equal(t, []RouteTraffic{www(1)}, v.Routes)
	require.Equal(t, 1, v.RoutesTotal)
	for _, host := range []string{"api.example.com", "nobody.example.com"} {
		_, err := e.eng.RouteSeries(host)
		require.ErrorIs(t, err, ErrNotFound, host)
		require.ErrorContains(t, err, host+" has no target")
	}
}

// While the egress filter is not on, the counters are not read at all: there
// are none of pco's, and what nft holds instead is not asked.
func TestWithoutCountersTheTrafficSaysWhy(t *testing.T) {
	for _, tt := range []struct {
		name    string
		state   string
		readErr error
		reads   int
		why     string
	}{
		{"not checked yet", "", nil, 0, "the egress filter was not checked yet"},
		{"off", EgressOff, nil, 0, "the egress filter is off: pco has no per-guest counters"},
		{"not loaded", EgressNotLoaded, nil, 0, "the egress filter is not loaded"},
		{"changed", EgressChanged, nil, 0, "the egress filter is not the one pco loads"},
		{"a failed read", EgressOn, egress.ErrNoCounters, 1, "the counters could not be read: the egress table has no counting sets"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.state != "" {
				e.eng.NoteEgress(EgressCheck{View: EgressView{State: tt.state}})
			}
			e.cycle()
			e.egr.count("3", map[string]uint64{targetWWW: 100})
			e.egr.failReads(tt.readErr)
			ch := listen(t, e.eng)

			e.eng.SampleTargets(t.Context(), at(0))
			e.eng.RecordTraffic(at(0), nil)

			require.Equal(t, tt.reads, e.egr.readCount())
			v := e.eng.Traffic()
			require.Equal(t, []RouteTraffic{}, v.Routes)
			require.Zero(t, v.RoutesTotal)
			require.Equal(t, tt.why, v.RoutesWhy)
			got := trafficNotices(until(t, e.eng, ch))
			require.Equal(t, []TrafficNotice{{At: at(0), Tunnels: []TunnelNotice{}, RoutesWhy: tt.why}}, got)
		})
	}
}

// What a read found before the filter went off says nothing of the table pco
// loads when it is on again.
func TestAFilterThatIsNotOnForgetsTheCounters(t *testing.T) {
	e := counting(t)
	readFlows(e, 0, "3", map[string]uint64{targetWWW: 100})
	readFlows(e, 1, "3", map[string]uint64{targetWWW: 110})

	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressOff}})
	readFlows(e, 2, "3", map[string]uint64{targetWWW: 115})

	require.Empty(t, seriesOf(t, e.eng, "www.example.com").Samples)
	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressOn}})
	readFlows(e, 3, "3", map[string]uint64{targetWWW: 120})
	require.Equal(t, []RouteTraffic{www(0)}, e.eng.Traffic().Routes, "no rate against the read before")
}

// A read that fails leaves the series as they are; the next one that works
// gives the rate over the time since the last that did.
func TestAFailedReadKeepsTheSeries(t *testing.T) {
	e := counting(t)
	readFlows(e, 0, "3", map[string]uint64{targetWWW: 100})
	readFlows(e, 1, "3", map[string]uint64{targetWWW: 110})

	e.egr.failReads(errors.New("nft: signal: killed"))
	e.eng.SampleTargets(t.Context(), at(2))
	v := e.eng.Traffic()
	require.Equal(t, "the counters could not be read: nft: signal: killed", v.RoutesWhy)
	require.Empty(t, v.Routes, "no figures while the last read gave none")

	readFlows(e, 3, "3", map[string]uint64{targetWWW: 130})

	require.Equal(t, []RouteSample{{At: at(1), FlowsPerSec: 2}, {At: at(3), FlowsPerSec: 2}},
		seriesOf(t, e.eng, "www.example.com").Samples)
	require.Empty(t, e.eng.Traffic().RoutesWhy)
}

// The daemon gives a read the interval: one that nft does not answer by then
// is a read that failed, and says so. A read the stop cut off is not one.
func TestAReadThatRanOutOfTimeFailsAndOneCutOffByTheStopRecordsNothing(t *testing.T) {
	e := counting(t)
	readFlows(e, 0, "3", map[string]uint64{targetWWW: 100})
	readFlows(e, 1, "3", map[string]uint64{targetWWW: 110})
	e.egr.failReads(errors.New("nft -j list table inet pco: signal: killed"))

	stopped, stop := context.WithCancel(t.Context())
	stop()
	e.eng.SampleTargets(stopped, at(2))

	require.Empty(t, e.eng.Traffic().RoutesWhy)
	require.Equal(t, []RouteTraffic{www(2)}, e.eng.Traffic().Routes)

	late, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	e.eng.SampleTargets(late, at(3))

	require.Equal(t, "the counters could not be read: nft -j list table inet pco: signal: killed", e.eng.Traffic().RoutesWhy)
}

// Every browser is sent the reason: one line, and short.
func TestTheReasonOfAFailedReadIsOneShortLine(t *testing.T) {
	e := counting(t)
	e.egr.failReads(errors.New("nft -j list table inet pco: exit status 1: Error: Could not process rule:\n\tNo such file or directory\n" +
		strings.Repeat("list table inet pco ", 30)))

	e.eng.SampleTargets(t.Context(), at(0))

	why := e.eng.Traffic().RoutesWhy
	require.True(t, strings.HasPrefix(why, "the counters could not be read: nft -j list table inet pco: exit status 1: Error: "), why)
	require.NotContains(t, why, "\n")
	require.NotContains(t, why, "\t")
	require.Len(t, []rune(why), 200)
	require.True(t, strings.HasSuffix(why, "..."), why)
}

// The address of a route that lost its proof is out of the set at once, with
// every target on it, though another route proves the same address: none of
// them has a figure, and the rest are not counted with them.
func TestARouteWhoseTargetTheSetLostHasNoFigure(t *testing.T) {
	e := newEnv(t)
	e.inv.set(snapshot(
		guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :9090"),
		guest(102, "db-1", "db.example.com -> :5432"),
	))
	other := netip.MustParseAddr("10.0.0.12")
	e.res.moveTo("db.example.com", other)
	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressOn}})
	e.cycle()
	db := netip.AddrPortFrom(other, 5432).String()
	readFlows(e, 0, "3", map[string]uint64{targetWWW: 0, "10.0.0.11:9090": 0, db: 0})
	readFlows(e, 1, "3", map[string]uint64{targetWWW: 5, "10.0.0.11:9090": 5, db: 5})
	require.Equal(t, 3, e.eng.Traffic().RoutesTotal)
	_, err := e.eng.RouteSeries("api.example.com")
	require.NoError(t, err)

	e.res.stop("www.example.com", "identity check failed: 10.0.0.11 answered by bc:24:11:ff:ff:01")
	e.clock.advance(20 * time.Second)
	e.cycle()
	got, _ := e.egr.last()
	require.Equal(t, egressTargets(db), got, "10.0.0.11 left the set, with api.example.com's target on it")
	readFlows(e, 2, "4", map[string]uint64{db: 0})

	v := e.eng.Traffic()
	require.Equal(t, []RouteTraffic{{Hostname: "db.example.com", Owner: "qemu/102", Target: db, FlowsPerSec: 1}}, v.Routes)
	require.Equal(t, 1, v.RoutesTotal)
	for _, host := range []string{"www.example.com", "api.example.com"} {
		_, err := e.eng.RouteSeries(host)
		require.ErrorIs(t, err, ErrNotFound, host)
	}
	require.Zero(t, seriesOf(t, e.eng, "db.example.com").Shared)
}

func TestTheLast180SamplesOfATargetAreKept(t *testing.T) {
	e := counting(t)
	for n := range 182 {
		readFlows(e, n, "3", map[string]uint64{targetWWW: uint64(5 * n)})
	}

	got := seriesOf(t, e.eng, "www.example.com").Samples

	require.Len(t, got, 180)
	require.Equal(t, RouteSample{At: at(2), FlowsPerSec: 1}, got[0], "oldest first")
	require.Equal(t, RouteSample{At: at(181), FlowsPerSec: 1}, got[179])
}

func TestARouteIsStaleAfterThreeReadsWithoutASample(t *testing.T) {
	e := counting(t)
	readFlows(e, 0, "3", map[string]uint64{targetWWW: 100})
	readFlows(e, 1, "3", map[string]uint64{targetWWW: 110})

	for n, generation := range []string{"4", "5"} {
		readFlows(e, 2+n, generation, map[string]uint64{targetWWW: 0})
		require.False(t, e.eng.Traffic().Routes[0].Stale, "after %d", n+1)
	}
	readFlows(e, 4, "6", map[string]uint64{targetWWW: 0})
	require.True(t, e.eng.Traffic().Routes[0].Stale)

	readFlows(e, 5, "6", map[string]uint64{targetWWW: 5})
	require.Equal(t, []RouteTraffic{www(1)}, e.eng.Traffic().Routes)
}

// 1000 busy targets would be about 130 KB every 5 s to every browser: a
// notice carries the busiest hundred, and says how many there are.
func TestANoticeCarriesTheHundredBusiestRoutes(t *testing.T) {
	e := newEnv(t)
	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressOn}})
	served := map[netip.AddrPort][]servedRoute{}
	idle, busy := map[string]uint64{}, map[string]uint64{}
	for i := range 1000 {
		ap := netip.AddrPortFrom(netip.MustParseAddr("10.0.0.11"), uint16(10000+i))
		served[ap] = []servedRoute{{hostname: fmt.Sprintf("g%04d.example.com", i), owner: fmt.Sprintf("qemu/%d", 1000+i)}}
		idle[ap.String()] = 0
		busy[ap.String()] = uint64(5 * i)
	}
	e.eng.keepServed(served)
	readFlows(e, 0, "3", idle)
	readFlows(e, 1, "3", busy)
	ch := listen(t, e.eng)

	e.eng.RecordTraffic(at(1), nil)

	got := trafficNotices(until(t, e.eng, ch))
	require.Len(t, got, 1)
	routes := got[0].Routes
	require.Len(t, routes, 100)
	require.Equal(t, RouteTraffic{Hostname: "g0999.example.com", Owner: "qemu/1999", Target: "10.0.0.11:10999", FlowsPerSec: 999}, routes[0])
	require.Equal(t, RouteTraffic{Hostname: "g0900.example.com", Owner: "qemu/1900", Target: "10.0.0.11:10900", FlowsPerSec: 900}, routes[99])
	require.Equal(t, 1000, got[0].RoutesTotal)
	require.Len(t, e.eng.Traffic().Routes, 1000, "the view has all of them")
}

// The zero of a route that stopped is sent once, and the cap must not cut it
// for busier routes: the browser would go on showing the rate before.
func TestTheZeroOfARouteThatStoppedIsNotCutByTheCap(t *testing.T) {
	e := newEnv(t)
	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressOn}})
	served := map[netip.AddrPort][]servedRoute{}
	counts := func(n int, stuck string) map[string]uint64 {
		out := map[string]uint64{}
		for i := range 1000 {
			ap := netip.AddrPortFrom(netip.MustParseAddr("10.0.0.11"), uint16(10000+i))
			out[ap.String()] = uint64(5 * n * i)
			if ap.String() == stuck {
				out[ap.String()] = 5 * uint64(i)
			}
		}
		return out
	}
	for i := range 1000 {
		ap := netip.AddrPortFrom(netip.MustParseAddr("10.0.0.11"), uint16(10000+i))
		served[ap] = []servedRoute{{hostname: fmt.Sprintf("g%04d.example.com", i), owner: fmt.Sprintf("qemu/%d", 1000+i)}}
	}
	e.eng.keepServed(served)
	readFlows(e, 0, "3", counts(0, ""))
	readFlows(e, 1, "3", counts(1, ""))
	e.eng.RecordTraffic(at(1), nil)
	ch := listen(t, e.eng)

	readFlows(e, 2, "3", counts(2, "10.0.0.11:10001"))
	e.eng.RecordTraffic(at(2), nil)

	got := trafficNotices(until(t, e.eng, ch))
	require.Len(t, got, 1)
	routes := got[0].Routes
	require.Len(t, routes, 100)
	require.Equal(t, RouteTraffic{Hostname: "g0001.example.com", Owner: "qemu/1001", Target: "10.0.0.11:10001"}, routes[0])
	require.Equal(t, "g0999.example.com", routes[1].Hostname)
}

// A notice tells of a route that stopped once, with no flows, and then no
// more; the traffic never moves the digest of the state.
func TestARouteThatStoppedIsSentOnceWithNoFlows(t *testing.T) {
	e := counting(t)
	digest := e.eng.State().Digest
	ch := listen(t, e.eng)
	for n, flows := range []uint64{100, 110, 110, 110} {
		readFlows(e, n, "3", map[string]uint64{targetWWW: flows})
		e.eng.RecordTraffic(at(n), nil)
	}

	got := trafficNotices(until(t, e.eng, ch))

	var routes [][]RouteTraffic
	for _, n := range got {
		routes = append(routes, n.Routes)
		require.Equal(t, 1, n.RoutesTotal)
	}
	require.Equal(t, [][]RouteTraffic{nil, {www(2)}, {www(0)}, nil}, routes)
	require.Equal(t, digest, e.eng.State().Digest)
}

func TestARouteSeriesHandedOutIsACopy(t *testing.T) {
	e := counting(t)
	readFlows(e, 0, "3", map[string]uint64{targetWWW: 100})
	readFlows(e, 1, "3", map[string]uint64{targetWWW: 110})

	got := seriesOf(t, e.eng, "www.example.com")
	got.Samples[0].FlowsPerSec = 99

	require.Equal(t, []RouteSample{{At: at(1), FlowsPerSec: 2}}, seriesOf(t, e.eng, "www.example.com").Samples)
}
