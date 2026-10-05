package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
)

const (
	tunnelA = "00000000-0000-4000-8000-00000000000a"
	tunnelB = "00000000-0000-4000-8000-00000000000b"
)

// scraped is what a connector's metrics say with these counters, over four
// connections.
func scraped(requests, errors, concurrent float64) *connector.Metrics {
	return &connector.Metrics{
		Requests: requests, RequestErrors: errors, Concurrent: concurrent,
		HAConnections: 4, ConfigVersion: 14, Version: "2026.9.3",
		Edges:     []connector.Edge{{Connection: 0, Location: "fra08"}, {Connection: 1, Location: "prg01"}},
		RTTMillis: []float64{11.2, 18.9},
	}
}

// at is the time of the n-th round of the traffic.
func at(n int) time.Time { return t0.Add(time.Duration(n) * TrafficInterval) }

func round(e *Engine, n int, scrapes map[string]*connector.Metrics) { e.RecordTraffic(at(n), scrapes) }

func samplesOfA(t *testing.T, e *Engine) []TrafficSample {
	t.Helper()
	for _, tt := range e.Traffic().Tunnels {
		if tt.TunnelID == tunnelA {
			return tt.Samples
		}
	}
	t.Fatal("tunnel A has no traffic")
	return nil
}

func TestRatesComeFromTheCountersOfTwoScrapes(t *testing.T) {
	e := newEnv(t).eng

	round(e, 0, map[string]*connector.Metrics{tunnelA: scraped(100, 1, 0)})
	require.Empty(t, samplesOfA(t, e), "one scrape gives no rate")

	round(e, 1, map[string]*connector.Metrics{tunnelA: scraped(150, 3, 2)})

	require.Equal(t, []TrafficSample{{At: at(1), RPS: 10, ErrorsPerSec: 0.4, Concurrent: 2}}, samplesOfA(t, e))
}

func TestTheTrafficView(t *testing.T) {
	e := newEnv(t).eng
	require.Equal(t, TrafficView{Interval: "5s", Tunnels: []TunnelTraffic{}, Routes: []RouteTraffic{}}, e.Traffic(), "before the first round")

	round(e, 0, map[string]*connector.Metrics{tunnelB: scraped(0, 0, 0), tunnelA: nil})
	round(e, 1, map[string]*connector.Metrics{tunnelB: scraped(5, 0, 1), tunnelA: nil})
	http2 := scraped(10, 0, 0)
	http2.RTTMillis = nil
	round(e, 2, map[string]*connector.Metrics{tunnelB: http2, tunnelA: nil})

	got, err := json.Marshal(e.Traffic())
	require.NoError(t, err)
	require.JSONEq(t, `{
		"at": "2026-10-01T12:00:10Z",
		"interval": "5s",
		"tunnels": [
			{
				"tunnelId": "00000000-0000-4000-8000-00000000000a", "node": "pve1", "configVersion": 0, "haConnections": 0,
				"edges": [], "rttMs": [], "stale": true, "samples": []
			},
			{
				"tunnelId": "00000000-0000-4000-8000-00000000000b", "node": "pve1", "cloudflared": "2026.9.3",
				"configVersion": 14, "haConnections": 4,
				"edges": [{"connection": 0, "location": "fra08"}, {"connection": 1, "location": "prg01"}],
				"rttMs": [], "stale": false,
				"samples": [
					{"at": "2026-10-01T12:00:05Z", "rps": 1, "errorsPerSec": 0, "concurrent": 1},
					{"at": "2026-10-01T12:00:10Z", "rps": 1, "errorsPerSec": 0, "concurrent": 0}
				]
			}
		],
		"routes": [],
		"routesTotal": 0
	}`, string(got))
}

// A connector that restarted counts from zero: the interval it fell into says
// nothing of the traffic, and the next one does again.
func TestACounterThatWentDownDropsTheInterval(t *testing.T) {
	for _, tt := range []struct {
		name  string
		reset *connector.Metrics
	}{
		{"the requests", scraped(20, 3, 0)},
		{"the errors", scraped(200, 1, 0)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t).eng
			round(e, 0, map[string]*connector.Metrics{tunnelA: scraped(100, 2, 0)})
			round(e, 1, map[string]*connector.Metrics{tunnelA: tt.reset})
			require.Empty(t, samplesOfA(t, e))

			next := *tt.reset
			next.Requests += 50
			round(e, 2, map[string]*connector.Metrics{tunnelA: &next})

			require.Equal(t, []TrafficSample{{At: at(2), RPS: 10}}, samplesOfA(t, e))
		})
	}
}

func TestAnIntervalWithoutTimeHasNoRate(t *testing.T) {
	e := newEnv(t).eng
	e.RecordTraffic(t0, map[string]*connector.Metrics{tunnelA: scraped(100, 0, 0)})
	e.RecordTraffic(t0, map[string]*connector.Metrics{tunnelA: scraped(150, 0, 0)})
	e.RecordTraffic(t0.Add(-time.Second), map[string]*connector.Metrics{tunnelA: scraped(200, 0, 0)})

	require.Empty(t, samplesOfA(t, e))
}

func TestATunnelIsStaleAfterThreeMissedScrapes(t *testing.T) {
	e := newEnv(t).eng
	stale := func() bool { return e.Traffic().Tunnels[0].Stale }
	round(e, 0, map[string]*connector.Metrics{tunnelA: scraped(100, 0, 0)})

	for n := 1; n <= 2; n++ {
		round(e, n, map[string]*connector.Metrics{tunnelA: nil})
		require.False(t, stale(), "after %d missed", n)
	}
	round(e, 3, map[string]*connector.Metrics{tunnelA: nil})
	require.True(t, stale())

	round(e, 4, map[string]*connector.Metrics{tunnelA: scraped(300, 0, 0)})
	require.False(t, stale(), "one scrape that worked")
	require.Equal(t, []TrafficSample{{At: at(4), RPS: 10}}, samplesOfA(t, e),
		"the rate over the time since the last scrape that worked")
}

func TestTheLast180SamplesAreKept(t *testing.T) {
	e := newEnv(t).eng
	for n := range 182 {
		round(e, n, map[string]*connector.Metrics{tunnelA: scraped(float64(5*n), 0, 0)})
	}

	got := samplesOfA(t, e)

	require.Len(t, got, 180)
	require.Equal(t, at(2), got[0].At, "oldest first")
	require.Equal(t, at(181), got[179].At)
}

func TestATunnelLeftOutOfARoundIsForgotten(t *testing.T) {
	e := newEnv(t).eng
	round(e, 0, map[string]*connector.Metrics{tunnelA: scraped(0, 0, 0), tunnelB: scraped(0, 0, 0)})
	round(e, 1, map[string]*connector.Metrics{tunnelB: scraped(5, 0, 0)})
	round(e, 2, map[string]*connector.Metrics{tunnelA: scraped(500, 0, 0), tunnelB: scraped(10, 0, 0)})

	got := e.Traffic().Tunnels
	require.Equal(t, []string{tunnelA, tunnelB}, []string{got[0].TunnelID, got[1].TunnelID})
	require.Empty(t, got[0].Samples, "a tunnel that comes back starts anew")
}

func TestEveryRoundSendsOneTrafficNotice(t *testing.T) {
	env := newEnv(t)
	env.cycle()
	e := env.eng
	digest := e.State().Digest
	ch := listen(t, e)

	round(e, 0, map[string]*connector.Metrics{tunnelA: scraped(100, 0, 0), tunnelB: nil})
	round(e, 1, map[string]*connector.Metrics{tunnelA: scraped(150, 5, 3), tunnelB: nil})
	round(e, 2, map[string]*connector.Metrics{tunnelA: nil, tunnelB: nil})
	round(e, 3, map[string]*connector.Metrics{tunnelA: nil, tunnelB: nil})

	got := until(t, e, ch)
	require.Equal(t, []string{
		"traffic " + at(0).Format(time.TimeOnly), "traffic " + at(1).Format(time.TimeOnly),
		"traffic " + at(2).Format(time.TimeOnly), "traffic " + at(3).Format(time.TimeOnly),
	}, shown(got), "one notice a round, and no state")
	require.Equal(t, &TrafficNotice{At: at(1), Tunnels: []TunnelNotice{
		{TunnelID: tunnelA, RPS: 10, ErrorsPerSec: 1, Concurrent: 3, HAConnections: 4},
		{TunnelID: tunnelB},
	}}, got[1].Traffic)
	require.Equal(t, &TrafficNotice{At: at(3), Tunnels: []TunnelNotice{
		{TunnelID: tunnelA, RPS: 10, ErrorsPerSec: 1, Concurrent: 3, HAConnections: 4},
		{TunnelID: tunnelB, Stale: true},
	}}, got[3].Traffic, "the newest sample of each tunnel")
	require.Equal(t, digest, e.State().Digest, "the traffic is not part of the state")
}

func TestATrafficViewHandedOutIsACopy(t *testing.T) {
	e := newEnv(t).eng
	round(e, 0, map[string]*connector.Metrics{tunnelA: scraped(0, 0, 0)})
	round(e, 1, map[string]*connector.Metrics{tunnelA: scraped(5, 0, 0)})
	before, err := json.Marshal(e.Traffic())
	require.NoError(t, err)

	got := e.Traffic()
	got.Tunnels[0].Samples[0].RPS = 99
	got.Tunnels[0].Edges[0].Location = "changed"
	got.Tunnels[0].RTTMillis[0] = 99

	after, err := json.Marshal(e.Traffic())
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after))
}

func TestTheStatusesOfTheConnectorsAreThoseOfTheState(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	st := e.cycle()
	require.NotEmpty(t, st.Connectors)

	got := e.eng.ConnectorStatuses()
	require.Equal(t, st.Connectors, got)

	got[0].MetricsAddr = "changed"
	require.Equal(t, st.Connectors, e.eng.ConnectorStatuses(), "a copy")
}

// The state shows the rollout the engine confirmed last, with what its event
// says: the version, on how many connectors, and when.
func TestTheRolloutOfATunnelIsInTheState(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	st := e.cycle()
	require.Nil(t, st.Tunnels[0].Rollout, "nothing confirmed yet")
	id := e.tunnels()[0].ID
	rollout := func() Event {
		var last Event
		for _, ev := range unnumbered(e.eng.Events(time.Time{})) {
			if ev.Kind == kindRollout {
				last = ev
			}
		}
		return last
	}

	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{{ID: defaultConnectorID, ConfigVersion: 1}, {ID: "c2", ConfigVersion: 1}})
	e.clock.advance(rolloutAskEvery)
	st = e.cycle()

	ev := rollout()
	require.Equal(t, "configuration version 1 runs on 2 connectors in account acc1", ev.Message)
	require.Equal(t, &RolloutView{Version: 1, Connectors: 2, ConfirmedAt: ev.At}, st.Tunnels[0].Rollout)

	e.clock.advance(time.Minute)
	st = e.cycle()
	require.Equal(t, &RolloutView{Version: 1, Connectors: 2, ConfirmedAt: ev.At}, st.Tunnels[0].Rollout, "it stays until the next one")

	e.inv.set(snapshot(guest(101, "web-1", "www.example.com api.example.com -> :8080")))
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{{ID: defaultConnectorID, ConfigVersion: 2}})
	e.clock.advance(rolloutAskEvery)
	st = e.cycle()

	ev = rollout()
	require.Equal(t, fmt.Sprintf("configuration version %d runs on 1 connectors in account acc1", st.Tunnels[0].Version), ev.Message)
	require.Equal(t, &RolloutView{Version: st.Tunnels[0].Version, Connectors: 1, ConfirmedAt: ev.At}, st.Tunnels[0].Rollout)
}

// A rotation restarts the connectors: what they ran before says nothing of
// what they run after it, until they are seen running it again.
func TestARotationForgetsTheRollout(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	id := e.tunnels()[0].ID
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{{ID: defaultConnectorID, ConfigVersion: 1}})
	e.clock.advance(rolloutAskEvery)
	require.NotNil(t, e.cycle().Tunnels[0].Rollout)

	_, err := e.eng.RotateTunnel(t.Context(), "")
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(e.paths.Cluster, "meta", "leader.json")))
	e.clock.advance(rolloutAskEvery)
	st := e.cycle()
	require.True(t, st.Tunnels[0].Unchecked)
	require.Nil(t, st.Tunnels[0].Rollout, "also in a cycle that shows the tunnels as the last one found them")

	require.NoError(t, e.store.SaveWriter(testWriter))
	e.clock.advance(rolloutAskEvery)
	st = e.cycle()
	require.Nil(t, st.Tunnels[0].Rollout, "no connector is listed after the rotation")

	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{{ID: defaultConnectorID, ConfigVersion: 1}})
	e.clock.advance(rolloutAskEvery)
	st = e.cycle()
	require.Equal(t, &RolloutView{Version: 1, Connectors: 1, ConfirmedAt: e.clock.now()}, st.Tunnels[0].Rollout)
}

// A cycle that does not get to Cloudflare shows the tunnels as the last one
// found them, the rollout with them.
func TestAHeldCycleKeepsTheRollout(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	e.cf.SetConnectors(testAccount, e.tunnels()[0].ID, []cfapi.Connector{{ID: defaultConnectorID, ConfigVersion: 1}})
	e.clock.advance(rolloutAskEvery)
	want := e.cycle().Tunnels[0].Rollout
	require.NotNil(t, want)

	require.NoError(t, os.Remove(filepath.Join(e.paths.Cluster, "meta", "leader.json")))
	e.clock.advance(rolloutAskEvery)
	st := e.cycle()

	require.True(t, st.Tunnels[0].Unchecked)
	require.Equal(t, want, st.Tunnels[0].Rollout)
}
