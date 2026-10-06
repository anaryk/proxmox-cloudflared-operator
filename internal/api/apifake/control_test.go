package apifake

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestTheControlsAnswerOnlyForTheirLoopbackAddress(t *testing.T) {
	e, _ := load(t, "empty")
	d := serve(t, e)
	host := strings.TrimPrefix(d.control, "http://")
	port := host[strings.LastIndex(host, ":")+1:]
	for _, c := range []struct {
		name   string
		header map[string]string
		ok     bool
	}{
		{"its address", nil, true},
		{"localhost", map[string]string{"Host": "localhost:" + port}, true},
		{"a page of its own origin", map[string]string{"Origin": d.control}, true},
		{"another name", map[string]string{"Host": "attacker.example:" + port}, false},
		{"another loopback address", map[string]string{"Host": "127.0.0.2:" + port}, false},
		{"another port", map[string]string{"Host": "127.0.0.1:1"}, false},
		{"a page of another origin", map[string]string{"Origin": "http://evil.example"}, false},
		{"a page of another port", map[string]string{"Origin": "http://127.0.0.1:1"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, d.control+"/refuse",
				strings.NewReader(`{"method": "Guests", "code": "refused", "message": "no"}`))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "text/plain")
			for k, v := range c.header {
				if k == "Host" {
					req.Host = v
				} else {
					req.Header.Set(k, v)
				}
			}
			res, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = res.Body.Close()
			if c.ok {
				require.Equal(t, http.StatusNoContent, res.StatusCode)
				_, err := e.Guests()
				require.Error(t, err, "the refusal was taken")
				return
			}
			require.Equal(t, http.StatusForbidden, res.StatusCode)
			_, err = e.Guests()
			require.NoError(t, err, "nothing was refused")
		})
	}
}

func TestAResetGoesBackToTheScenario(t *testing.T) {
	e, c := load(t, "populated")
	d := serve(t, e)
	ctx := context.Background()
	notices, hello, err := d.Stream(ctx, "", 0)
	require.NoError(t, err)
	loaded := e.State()
	view, err := e.SettingsView()
	require.NoError(t, err)

	_, err = e.Apply(ctx, true, loaded.Offer)
	require.NoError(t, err)
	longer := view.Settings
	longer.Grace = store.Duration(2 * time.Duration(longer.Grace))
	_, _, err = e.SaveSettings(ctx, view.Rev, longer)
	require.NoError(t, err)
	require.NoError(t, e.DeleteManualRoute(ctx, "status", 1))
	require.NoError(t, e.Refuse(Refusal{Method: "Guests", Code: "refused", Message: "no"}))
	e.Pause()
	c.add(time.Hour)

	status, _ := d.steer(t, http.MethodPost, "/reset", "")
	require.Equal(t, http.StatusNoContent, status)

	st := e.State()
	fresh, err := Scenario("populated", c.now)
	require.NoError(t, err)
	defer func() { require.NoError(t, fresh.Close()) }()
	require.Equal(t, fresh.State(), st, "as the scenario loads now")
	require.Equal(t, loaded.Offer, st.Offer)
	require.Equal(t, t0.Add(time.Hour), st.FinishedAt, "the scenario is as of now")
	require.Equal(t, hello.Boot, e.Boot(), "no restart")
	view, err = e.SettingsView()
	require.NoError(t, err)
	require.Equal(t, 1, view.Rev)
	manual, err := e.ManualRoutes()
	require.NoError(t, err)
	require.Len(t, manual, 1)
	_, err = e.Guests()
	require.NoError(t, err, "the refusal is forgotten")
	require.Equal(t, []string{"Guests", "ManualRoutes", "SettingsView", "State"}, calledMethods(e), "only the calls since")

	for {
		n := next(t, notices)
		if n.Kind == engine.NoticeState && n.State.Digest == st.Digest && n.State.FinishedAt.Equal(st.FinishedAt) {
			break
		}
	}
	events, err := e.QueryEvents(engine.EventQuery{})
	require.NoError(t, err)
	require.Len(t, events, 10, "the events of the scenario")
	require.Greater(t, events[0].Seq, hello.Seq, "numbered on in the boot")
}

func calledMethods(e *Engine) []string {
	set := map[string]bool{}
	for _, c := range e.Calls() {
		set[c.Method] = true
	}
	return slices.Sorted(maps.Keys(set))
}

func TestATrafficChangeKeepsWhatItLeavesOut(t *testing.T) {
	e, _ := load(t, "populated")
	tunnel := e.Traffic().Tunnels[0]
	before := tunnel.Samples[len(tunnel.Samples)-1]

	require.NoError(t, e.ChangeTraffic(TrafficChange{Tunnels: []TunnelFigures{{TunnelID: tunnel.TunnelID, RPS: ptr(500.0)}}}))
	after := e.Traffic().Tunnels[0]
	now := after.Samples[len(after.Samples)-1]
	require.Equal(t, 500.0, now.RPS)
	require.Equal(t, before.ErrorsPerSec, now.ErrorsPerSec)
	require.Equal(t, before.Concurrent, now.Concurrent)
	require.Equal(t, tunnel.HAConnections, after.HAConnections)

	routes := e.Traffic().Routes
	require.NoError(t, e.ChangeTraffic(TrafficChange{RoutesWhy: ptr(whyOff)}))
	require.Empty(t, e.Traffic().Routes)
	require.NoError(t, e.ChangeTraffic(TrafficChange{RoutesWhy: ptr("")}))
	require.Equal(t, routes, e.Traffic().Routes, "the figures come back")

	require.NoError(t, e.ChangeTraffic(TrafficChange{Routes: []RouteFigures{{Hostname: "www.example.com", Stale: ptr(true)}}}))
	www := e.Traffic().Routes[len(routes)-2]
	require.Equal(t, "www.example.com", www.Hostname)
	require.True(t, www.Stale)
	require.Equal(t, routes[len(routes)-2].FlowsPerSec, www.FlowsPerSec)
}

func ptr[T any](v T) *T { return &v }

func TestRunCyclesAndSamplesAsTheDaemonDoes(t *testing.T) {
	e, _ := load(t, "populated")
	d := serve(t, e)
	notices, _, err := d.Stream(context.Background(), "", 0)
	require.NoError(t, err)

	st := e.State()
	st.At = st.FinishedAt.Add(-40 * time.Second)
	e.SetState(st)
	require.Equal(t, engine.NoticeState, next(t, notices).Kind)
	require.Equal(t, 10*time.Second+40*time.Second, e.nextCycle(), "the poll interval after a cycle of 40 s")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.run(ctx, 20*time.Millisecond, func() time.Duration { return 50 * time.Millisecond })
	kinds := map[string]bool{}
	for !kinds[engine.NoticeState] || !kinds[engine.NoticeTraffic] {
		kinds[next(t, notices).Kind] = true
	}

	e.Pause()
	time.Sleep(150 * time.Millisecond)
	cancel()
	e.Resume()
	got := 0
	for {
		select {
		case <-notices:
			got++
			continue
		case <-time.After(200 * time.Millisecond):
		}
		break
	}
	require.LessOrEqual(t, got, 2, "Run neither cycled nor sampled while paused")
}

func TestARestartIsNotPaused(t *testing.T) {
	e, _ := load(t, "empty")
	e.Pause()
	e.Restart()
	require.Nil(t, e.pausedUntil())
}

func TestEveryTimeOfTheScenarioMovesWithTheClock(t *testing.T) {
	e, _ := load(t, "populated")
	st := e.State()
	g := golden[engine.State](t, engineGoldens, "state_populated.json")
	d := t0.Sub(g.FinishedAt)
	require.Equal(t, g.Routes[0].Path.VerifiedAt.Add(d), st.Routes[routeIndex(st, "www.example.com", "qemu/101")].Path.VerifiedAt)
	require.Equal(t, g.Credentials[0].Report.Token.ExpiresOn.Add(d), *st.Credentials[0].Report.Token.ExpiresOn)
	require.Equal(t, g.RogueConnectors[0].Since.Add(d), st.RogueConnectors[0].Since)
	claims, err := e.Claims()
	require.NoError(t, err)
	var missing time.Time
	for _, c := range claims {
		if c.Hostname == "old.example.com" {
			missing = *c.MissingSince
		}
	}
	held := st.Routes[routeIndex(st, "old.example.com", "qemu/101")]
	require.Equal(t, planner.StateHeld, held.State)
	require.Contains(t, held.Reason, missing.Format(time.RFC3339), "a time a text names moves with the time it names")
	events, err := e.QueryEvents(engine.EventQuery{})
	require.NoError(t, err)
	require.Equal(t, st.At, events[len(events)-1].At)
}

func routeIndex(st engine.State, host, owner string) int {
	for i, r := range st.Routes {
		if r.Hostname == host && r.Owner == owner {
			return i
		}
	}
	return -1
}

func TestACallTheAPINamesNoActorForIsOfAnUnknownActor(t *testing.T) {
	e, _ := load(t, "empty")
	_, err := e.Guests()
	require.NoError(t, err)
	_, err = e.ApproveGuest(engine.WithActor(context.Background(), "alice@pve (ticket)"), "qemu/1", "", nil, nil)
	require.Error(t, err)
	calls := e.Calls()
	require.Equal(t, "unknown", calls[0].Actor)
	require.Equal(t, "alice@pve (ticket)", calls[1].Actor)
	data, err := json.Marshal(calls[0])
	require.NoError(t, err)
	require.Contains(t, string(data), `"actor":"unknown"`)
}
