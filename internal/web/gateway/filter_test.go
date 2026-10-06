package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/auth"
)

// caseRow tests one case of the readers' filter: a state with guests the
// reader may not see, and the golden of the fields the case touches.
type caseRow struct {
	state   func(t *testing.T) engine.State
	visible auth.Visible
	fields  []string // the JSON names the golden holds
}

// caseRows has one row per case of stateCases: a case added without its row
// fails TestEveryStateCaseHasARow.
var caseRows = map[string]caseRow{
	"routes":     {state: populated, visible: sees(102), fields: []string{"routes"}},
	"issues":     {state: populated, visible: sees(101), fields: []string{"issues"}},
	"actions":    {state: populated, visible: sees(101), fields: []string{"actions"}},
	"conflicts":  {state: manualOnly, visible: sees(), fields: []string{"conflicts"}},
	"lost":       {state: manualOnly, visible: sees(), fields: []string{"lost"}},
	"waiting":    {state: populated, visible: sees(101, 104), fields: []string{"waiting"}},
	"offer":      {state: populated, visible: sees(101, 104), fields: []string{"offer"}},
	"unapproved": {state: populated, visible: sees(201), fields: []string{"unapproved"}},
	"identity":   {state: withIdentity, visible: sees(140), fields: []string{"identity"}},
}

// manualOnly is a state of manual routes only, with a conflict and a lost
// marker on their names and on a name no route has.
func manualOnly(*testing.T) engine.State {
	st := engine.State{Node: "pve1", Digest: "aa00bb11cc22dd33", Mode: engine.ModeEnforce, Complete: true}
	st.Routes = []engine.RouteView{manualRoute("api.example.com", "api"), manualRoute("status.example.com", "status")}
	st.Actions = []reconcile.Action{
		{Kind: reconcile.CreateRecord, Credential: "cred1", Target: "status.example.com", Detail: "in zone example.com", Applied: true},
		{Kind: reconcile.DeleteRecord, Credential: "cred1", Target: "old.example.com", Detail: "in zone example.com", Destructive: true},
	}
	st.Conflicts = []reconcile.Conflict{
		{Zone: "example.com", Name: "api.example.com", Type: "A", Content: "192.0.2.10"},
		{Zone: "example.com", Name: "shop.example.com", Type: "A", Content: "192.0.2.11"},
	}
	st.Lost = []string{"gone.example.com", "status.example.com"}
	return st
}

// withIdentity is the populated state of an appliance whose MAC two guests
// carry.
func withIdentity(t *testing.T) engine.State {
	st := populated(t)
	st.Profile = "appliance"
	st.Identity = &engine.IdentityView{
		VMID: 9250, Node: "pve1", Why: "lxc/9295 in pool pco carries a MAC of lxc/9250: a copy of the appliance runs",
		Copies: []string{"lxc/9295"}, Tenants: []string{"qemu/140", "qemu/141"}, Exposed: []string{"alice@pve"}, CheckedAt: t0,
	}
	return st
}

// waitingHidden is a state where what waits names a guest the reader does
// not see, beside a route the reader sees.
func waitingHidden(*testing.T) engine.State {
	st := engine.State{Node: "pve1", Digest: "5150ab0cd0e0f001", Mode: engine.ModeEnforce, Complete: true}
	st.Routes = []engine.RouteView{guestRoute("www.example.com", 101, planner.StateActive)}
	st.Waiting = []engine.Waiting{{
		Kind: "vanished-guests", Detail: "1 guest that holds a hostname is no longer listed by Proxmox; a confirmation takes it as removed",
		Items: []string{"qemu/104 db-1"},
	}}
	st.Offer = "0f0f0f0f0f0f0f0f"
	return st
}

func jsonFields(t *testing.T, v any, names []string) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	var all map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &all))
	out := map[string]json.RawMessage{}
	for _, n := range names {
		out[n] = all[n]
	}
	data, err = json.Marshal(out)
	require.NoError(t, err)
	return data
}

func TestEveryStateCaseHasARow(t *testing.T) {
	names := map[string]bool{}
	for _, c := range stateCases {
		require.False(t, names[c.Name], "the case %s is there twice", c.Name)
		names[c.Name] = true
		_, ok := caseRows[c.Name]
		require.True(t, ok, "the case %s has no row in caseRows: add one with a golden of hidden guests", c.Name)
	}
	for name := range caseRows {
		require.True(t, names[name], "caseRows has %s, which is no case", name)
	}
}

func TestEveryStateCaseHidesWhatIsHidden(t *testing.T) {
	for _, c := range stateCases {
		t.Run(c.Name, func(t *testing.T) {
			row := caseRows[c.Name]
			in := row.state(t)
			before := jsonFields(t, in, row.fields)
			st := cloneState(t, in)
			c.Apply(&st, row.visible, visibleHosts(st, row.visible))
			got := jsonFields(t, st, row.fields)
			require.NotEqual(t, string(before), string(got), "the row of %s hides nothing: give it a state with guests the reader does not see", c.Name)
			requireGoldenJSON(t, "filter/case_"+c.Name+".json", got)
			require.Equal(t, string(before), string(jsonFields(t, in, row.fields)), "a case leaves its input as it was")
		})
	}
}

func cloneState(t *testing.T, st engine.State) engine.State {
	t.Helper()
	data, err := json.Marshal(st)
	require.NoError(t, err)
	var out engine.State
	require.NoError(t, json.Unmarshal(data, &out))
	return out
}

func TestFilterStateGoldens(t *testing.T) {
	cases := []struct {
		name    string
		state   func(t *testing.T) engine.State
		visible auth.Visible
	}{
		// qemu/101 holds www.example.com, lxc/201 waits for approval; the
		// loser qemu/102, the guest with a broken entry qemu/103 and lxc/202
		// are hidden.
		{"state_half", populated, sees(101, 201)},
		{"state_none", populated, sees()},
		{"state_manual", manualOnly, sees()},
		{"state_waiting_hidden", waitingHidden, sees(101)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.state(t)
			before, err := json.Marshal(in)
			require.NoError(t, err)
			got, err := json.Marshal(FilterState(in, c.visible))
			require.NoError(t, err)
			requireGoldenJSON(t, "filter/"+c.name+".json", got)
			after, err := json.Marshal(in)
			require.NoError(t, err)
			require.Equal(t, string(before), string(after), "FilterState leaves the state it was given as it was")
		})
	}
}

// The state an admin gets is the daemon's, byte for byte.
func TestAdminsGetTheStateAsItIs(t *testing.T) {
	s := newTestServer(t)
	st := populated(t)
	s.daemon.serveState(st)
	b := s.signIn(admin)
	rec := b.do(http.MethodGet, "/api/v1/state", "")
	require.Equal(t, http.StatusOK, rec.Code)
	want, err := json.Marshal(st)
	require.NoError(t, err)
	require.Equal(t, string(want), rec.Body.String())
	require.Equal(t, `"5e0c1f7a92b4d3e8"`, rec.Header().Get("ETag"))
}

func TestReadersGetTheFilteredState(t *testing.T) {
	s := newTestServer(t)
	s.daemon.serveState(populated(t))
	s.pve.sees(reader, 101, 201)
	b := s.signIn(reader)
	rec := b.do(http.MethodGet, "/api/v1/state", "")
	require.Equal(t, http.StatusOK, rec.Code)
	requireGoldenJSON(t, "filter/state_half.json", rec.Body.Bytes())
}

func TestFilterTrafficOfLargeWithAThirdVisible(t *testing.T) {
	st := largeState()
	tv := largeTraffic(st)
	visible := func(g model.GuestRef) bool { return g.VMID%3 == 0 }
	got := FilterTraffic(tv, visible)

	require.NotEmpty(t, got.Routes)
	require.Less(t, len(got.Routes), len(tv.Routes))
	require.Equal(t, len(got.Routes), got.RoutesTotal, "a reader's routesTotal counts the routes it sees with a figure")
	shared := map[string]int{}
	for _, r := range got.Routes {
		ref, err := model.ParseGuestRef(r.Owner)
		require.NoError(t, err)
		require.True(t, visible(ref), "%s is not the reader's", r.Owner)
		shared[r.Target]++
	}
	for _, r := range got.Routes {
		require.Equal(t, shared[r.Target]-1, r.Shared, "shared counts only the routes the reader sees")
	}
	require.Equal(t, tv.Tunnels, got.Tunnels)
	require.Equal(t, 960, tv.RoutesTotal, "the input is as it was")
	data, err := json.Marshal(got)
	require.NoError(t, err)
	requireGoldenJSON(t, "filter/traffic_large.json", data)
}

func TestFilterTrafficKeepsWhyAndManualRoutes(t *testing.T) {
	tv := engine.TrafficView{At: t0, Interval: "5s", Tunnels: []engine.TunnelTraffic{}, Routes: []engine.RouteTraffic{
		{Hostname: "status.example.com", Owner: "manual/status", Target: "10.0.5.20:9000", FlowsPerSec: 1},
		{Hostname: "www.example.com", Owner: "qemu/101", Target: "10.0.0.11:8080", FlowsPerSec: 1},
		{Hostname: "odd.example.com", Owner: "not-an-owner", Target: "10.0.0.12:8080", FlowsPerSec: 1},
	}, RoutesTotal: 3}
	got := FilterTraffic(tv, sees())
	require.Len(t, got.Routes, 1)
	require.Equal(t, "manual/status", got.Routes[0].Owner)
	require.Equal(t, 1, got.RoutesTotal)

	off := engine.TrafficView{At: t0, Interval: "5s", Tunnels: []engine.TunnelTraffic{}, Routes: []engine.RouteTraffic{}, RoutesWhy: "the egress filter is off: pco has no per-guest counters"}
	got = FilterTraffic(off, sees())
	require.Equal(t, off.RoutesWhy, got.RoutesWhy)
	require.NotNil(t, got.Routes)
	require.Zero(t, got.RoutesTotal)
}

func TestDoctorCountsCountTheFindings(t *testing.T) {
	got := DoctorCounts(doctorAnswer, t0)
	require.Equal(t, 2, got.OK)
	require.Equal(t, 1, got.Warn)
	require.Equal(t, 1, got.Fail)
	require.Equal(t, t0, got.At)
}

// A reader never gets a finding: they name guests waiting for approval and
// hostnames of conflicts it may not see.
func TestReadersGetTheDoctorsCountsOnly(t *testing.T) {
	s := newTestServer(t)
	s.daemon.answer("GET /v1/doctor", http.StatusOK, doctorAnswer)
	b := s.signIn(reader)
	rec := b.do(http.MethodPost, "/api/v1/doctor", "{}")
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"ok":2,"warn":1,"fail":1,"at":"2026-10-01T12:00:00Z"}`, rec.Body.String())
	for _, f := range doctorAnswer {
		require.NotContains(t, rec.Body.String(), f.Check)
	}

	rec = s.signIn(admin).do(http.MethodPost, "/api/v1/doctor", "{}")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "credential cred1")
}

func TestHiddenGuestsAndHostnamesAnswer404(t *testing.T) {
	s := newTestServer(t)
	s.daemon.serveState(populated(t))
	s.pve.sees(reader, 101)
	s.daemon.answer("GET /v1/traffic/route", http.StatusOK, engine.RouteSeries{Hostname: "x", Samples: []engine.RouteSample{}})
	s.daemon.answer("GET /v1/traffic", http.StatusOK, trafficAnswer)
	b := s.signIn(reader)

	requireError(t, b.do(http.MethodGet, "/api/v1/guests/qemu/102/annotation", ""), http.StatusNotFound, "not_found")
	require.Zero(t, s.daemon.count("GET /v1/guests/qemu/102/annotation"))
	rec := b.do(http.MethodGet, "/api/v1/guests/qemu/101/annotation", "")
	require.Equal(t, http.StatusOK, rec.Code)

	// A hostname the reader's state lacks, and one whose route is another's,
	// answer alike.
	for _, host := range []string{"nowhere.example.com", "shop.example.com"} {
		requireError(t, b.do(http.MethodGet, "/api/v1/traffic/route?hostname="+host, ""), http.StatusNotFound, "not_found")
	}
	require.Zero(t, s.daemon.count("GET /v1/traffic/route"))
	rec = b.do(http.MethodGet, "/api/v1/traffic/route?hostname=www.example.com", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The losing route of a hostname is not its route: its figures are the
	// holder's.
	s.pve.sees(reader2, 102)
	requireError(t, s.signIn(reader2).do(http.MethodGet, "/api/v1/traffic/route?hostname=www.example.com", ""), http.StatusNotFound, "not_found")
}

// diagnoseState is the populated state with a manual route and a hostname
// whose routes are all in conflict.
func diagnoseState(t *testing.T) engine.State {
	st := populated(t)
	st.Routes = append(st.Routes, manualRoute("api.example.com", "api"))
	a := guestRoute("dup.example.com", 110, planner.StateConflict)
	a.Reason = "hostname is claimed by qemu/111 as well"
	b := guestRoute("dup.example.com", 111, planner.StateConflict)
	b.Reason = "hostname is claimed by qemu/110 as well"
	st.Routes = append(st.Routes, a, b)
	sortRoutes(st.Routes)
	return st
}

func TestReadersDiagnoseOnlyWhatTheHolderOfSees(t *testing.T) {
	s := newTestServer(t)
	s.daemon.serveState(diagnoseState(t))
	steps := []doctor.Step{
		{Name: "route", Level: doctor.LevelOK, Detail: "qemu/101 (web-1) holds it; state active"},
		{Name: "zone", Level: doctor.LevelOK, Detail: "zone example.com is active"},
		{Name: "dns", Level: doctor.LevelFail, Detail: "no record points at the tunnel"},
		{Name: "ingress", Level: doctor.LevelWarn, Detail: "skipped", Skipped: true},
	}
	s.daemon.answer("GET /v1/diagnose", http.StatusOK, steps)

	// bob sees the loser of www.example.com and one of the two claimants of
	// dup.example.com, the one that is not first.
	s.pve.sees(reader, 102, 111)
	bob := s.signIn(reader)
	for _, host := range []string{"www.example.com", "WWW.example.com.", "dup.example.com", "nowhere.example.com", "not a name"} {
		requireError(t, bob.do(http.MethodPost, "/api/v1/diagnose", `{"hostname":"`+host+`"}`), http.StatusNotFound, "not_found")
	}
	require.Zero(t, s.daemon.count("GET /v1/diagnose"), "a refused diagnosis never reaches the socket")
	rec := bob.do(http.MethodPost, "/api/v1/diagnose", `{"hostname":"api.example.com"}`)
	require.Equal(t, http.StatusOK, rec.Code, "a manual route's chain is everyone's")

	s.pve.sees(reader2, 101, 110)
	carol := s.signIn(reader2)
	for _, host := range []string{"www.example.com", "dup.example.com"} {
		rec = carol.do(http.MethodPost, "/api/v1/diagnose", `{"hostname":"`+host+`"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	requireGoldenJSON(t, "filter/diagnose.json", rec.Body.Bytes())
}

// The gateway decides by the route the daemon diagnoses: for every hostname
// of the fixtures, the holder the gateway's doctor.HolderOf picks is the
// route DiagnoseRoute walks.
func TestTheHolderIsTheRouteTheDiagnosisWalks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // nothing is probed
	states := map[string]engine.State{
		"populated": diagnoseState(t),
		"large":     largeState(),
		"outage":    outageState(),
	}
	for name, st := range states {
		seen := map[string]bool{}
		for _, r := range st.Routes {
			if seen[r.Hostname] {
				continue
			}
			seen[r.Hostname] = true
			holder, ok := doctor.HolderOf(st, r.Hostname)
			require.True(t, ok)
			steps, err := doctor.DiagnoseRoute(ctx, st, r.Hostname, nil)
			require.NoError(t, err)
			require.Equal(t, "route", steps[0].Name)
			if holder.State == planner.StateConflict {
				require.Equal(t, holder.Reason, steps[0].Detail, "%s %s", name, r.Hostname)
				continue
			}
			who := engine.OwnerName(holder.Owner, holder.Guest)
			require.True(t, strings.HasPrefix(steps[0].Detail, who+" holds it"), "%s %s: %s", name, r.Hostname, steps[0].Detail)
			for _, other := range st.Routes {
				if other.Hostname == r.Hostname && other.Owner != holder.Owner {
					require.NotContains(t, steps[0].Detail, other.Owner)
				}
			}
		}
	}
	dup, ok := doctor.HolderOf(states["populated"], "dup.example.com")
	require.True(t, ok)
	require.Equal(t, "qemu/110", dup.Owner)
	require.True(t, slices.ContainsFunc(states["large"].Routes, func(r engine.RouteView) bool { return r.State == planner.StateConflict }))
}

func TestFilterEventsByGuestRouteAndSubject(t *testing.T) {
	hosts := map[string]bool{"www.example.com": true}
	got := FilterEvents(eventsAnswer, sees(101, 201), hosts)
	var seqs []uint64
	for _, ev := range got {
		seqs = append(seqs, ev.Seq)
	}
	require.Equal(t, []uint64{4, 7, 9, 10, 11}, seqs)
	require.NotNil(t, FilterEvents(nil, sees(), nil))
	require.Len(t, eventsAnswer, 8, "the input is as it was")
}
