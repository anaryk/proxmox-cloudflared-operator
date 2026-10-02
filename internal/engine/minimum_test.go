package engine

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	belowPort   = "identity level observed is below the required port"
	oneHeldBack = "1 route is held back: its identity level is observed, below the required port; " +
		"guests on other nodes and trusted static addresses are proven at observed only: lower identityMinimum in the settings to serve them"
	threeHeldOut = "3 routes are held back: their identity level is filtered or observed, below the required port; " +
		"guests on other nodes and trusted static addresses are proven at observed only: lower identityMinimum in the settings to serve them"
)

// webAndAPI publishes www on guest 101 and api on guest 102, in enforce mode.
func webAndAPI(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot(
		guest(101, "web-1", "www.example.com -> :8080"),
		guest(102, "api", "api.example.com -> :8080"),
	))
	return e
}

func TestTheMinimumHoldsBackARouteProvenBelowIt(t *testing.T) {
	e := webAndAPI(t)
	e.res.setLevel("www.example.com", resolve.LevelObserved)

	st := e.cycle()

	www := route(st, "www.example.com")
	require.Equal(t, planner.StateUnreachable, www.State)
	require.Equal(t, "observed", www.Level)
	require.Equal(t, belowPort, www.Reason)
	require.Empty(t, www.Service)
	require.Equal(t, &planner.IngressRule{Hostname: "www.example.com", Service: "http_status:503"}, www.Rule)
	require.Equal(t, []resolve.CandidateResult{{Addr: guestAddr, Source: resolve.FromStatic, OK: true}}, www.Candidates,
		"the candidates say what resolution found")
	require.Equal(t, withSentinel(hostRule("api.example.com"), planner.IngressRule{Hostname: "www.example.com", Service: "http_status:503"}), e.rules())
	require.Equal(t, []string{"api.example.com"}, e.recordNames(), "no record is planned for it")
	require.Equal(t, []string{oneHeldBack}, st.Problems)

	api := route(st, "api.example.com")
	require.Equal(t, planner.StateActive, api.State)
	require.Equal(t, "port", api.Level)

	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Equal(t, resolve.LevelObserved, bindings["www.example.com"].Level, "the proof is kept as it was made")
}

func TestTheMinimumServesWhatIsProvenAtOrAboveIt(t *testing.T) {
	tests := []struct {
		name      string
		appliance bool
		minimum   string
		level     resolve.Level
		reason    string // empty when served
	}{
		{name: "port at port", minimum: "port", level: resolve.LevelPort},
		{name: "observed at port", minimum: "port", level: resolve.LevelObserved, reason: belowPort},
		{name: "observed at observed", minimum: "observed", level: resolve.LevelObserved},
		{name: "port at observed", minimum: "observed", level: resolve.LevelPort},
		{name: "port at filtered", minimum: "filtered", level: resolve.LevelPort},
		{name: "observed at filtered asks for port in the host profile", minimum: "filtered", level: resolve.LevelObserved, reason: belowPort},
		{name: "filtered at filtered asks for port in the host profile", minimum: "filtered", level: resolve.LevelFiltered,
			reason: "identity level filtered is below the required port"},
		{name: "filtered at filtered in the appliance profile", appliance: true, minimum: "filtered", level: resolve.LevelFiltered},
		{name: "observed at filtered in the appliance profile", appliance: true, minimum: "filtered", level: resolve.LevelObserved,
			reason: "identity level observed is below the required filtered"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.appliance {
				require.NoError(t, e.store.SaveInstall(store.Install{ID: testInstall, CreatedAt: t0, Profile: store.ProfileAppliance}))
			}
			e.settings(func(s *store.Settings) { s.IdentityMinimum = tt.minimum })
			e.res.setLevel("www.example.com", tt.level)

			st := e.cycle()

			www := route(st, "www.example.com")
			require.Equal(t, string(tt.level), www.Level)
			if tt.reason == "" {
				require.Equal(t, planner.StateActive, www.State)
				require.Empty(t, st.Problems)
				return
			}
			require.Equal(t, planner.StateUnreachable, www.State)
			require.Equal(t, tt.reason, www.Reason)
			require.Len(t, st.Problems, 1)
		})
	}
}

func TestLoweringTheMinimumServesTheRemoteGuest(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.res.setLevel("www.example.com", resolve.LevelObserved)
	st := e.cycle()
	require.Equal(t, planner.StateUnreachable, route(st, "www.example.com").State)
	require.Equal(t, []string{oneHeldBack}, st.Problems)
	require.Empty(t, e.records())

	e.settings(func(s *store.Settings) { s.IdentityMinimum = "observed" })
	e.clock.advance(10 * time.Second)
	st = e.cycle()

	www := route(st, "www.example.com")
	require.Equal(t, planner.StateActive, www.State)
	require.Equal(t, "observed", www.Level)
	require.Equal(t, "http://10.0.0.11:8080", www.Service)
	require.Empty(t, st.Problems, "nothing is held back any more")
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
	require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules())
}

func TestOneProblemLineForEveryRouteHeldBack(t *testing.T) {
	e := newEnv(t)
	e.inv.set(snapshot(
		guest(101, "web-1", "www.example.com -> :8080"),
		guest(102, "api", "api.example.com -> :8080"),
		guest(103, "db", "db.example.com -> :8080"),
		guest(104, "app", "app.example.com -> :8080"),
	))
	e.res.setLevel("www.example.com", resolve.LevelObserved)
	e.res.setLevel("api.example.com", resolve.LevelFiltered)
	e.res.setLevel("db.example.com", resolve.LevelObserved)

	st := e.cycle()

	require.Equal(t, []string{threeHeldOut}, st.Problems)
	require.Equal(t, planner.StateActive, route(st, "app.example.com").State)

	e.clock.advance(10 * time.Second)
	again := e.cycle()
	require.Equal(t, []string{threeHeldOut}, again.Problems, "once per cycle")
	require.Equal(t, 1, eventsContaining(e, "3 routes are held back"), "an event when it begins, not every cycle")
}

func TestAManualRouteIsNotHeldBackByTheMinimum(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot())
	require.NoError(t, e.store.SaveManualRoute(model.Route{
		Hostname: "nas.example.com", ManualID: "nas", Source: model.SourceManual,
		Target: model.Target{Scheme: model.SchemeHTTP, Addr: netip.MustParseAddr("10.0.0.50"), Port: 5000},
	}))

	// The second cycle reads what the first one stored.
	for range 2 {
		st := e.cycle()

		nas := route(st, "nas.example.com")
		require.Equal(t, planner.StateActive, nas.State)
		require.Equal(t, "manual", nas.Level)
		require.Equal(t, "http://10.0.0.50:5000", nas.Service)
		require.Empty(t, st.Problems)
		require.Equal(t, []string{"nas.example.com"}, e.recordNames())
		e.clock.advance(10 * time.Second)
	}
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Empty(t, bindings, "a route without a guest is not bound")
}

// A manual route that names a guest has its identity proven, so the minimum
// applies to it; only a route without a guest is exempt.
func TestAManualRouteThatNamesAGuestIsHeldBack(t *testing.T) {
	ref := model.GuestRef{Kind: model.KindQEMU, VMID: 101}
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot(guest(101, "web-1")))
	require.NoError(t, e.store.SaveManualRoute(model.Route{
		Hostname: "web.example.com", ManualID: "web", Source: model.SourceManual, Guest: &ref,
		Target: model.Target{Scheme: model.SchemeHTTP, Port: 8080},
	}))
	e.res.setLevel("web.example.com", resolve.LevelObserved)

	st := e.cycle()

	web := route(st, "web.example.com")
	require.Equal(t, "manual/web", web.Owner)
	require.Equal(t, planner.StateUnreachable, web.State)
	require.Equal(t, belowPort, web.Reason)
	require.Empty(t, e.recordNames())
	require.Equal(t, []string{oneHeldBack}, st.Problems)
}

// A route the minimum holds back was never served, so a withdrawal must not
// publish it either: the guest cannot be checked for a cycle, then answers
// again.
func TestAHeldBackRouteGetsNoRecordWhenItIsWithdrawn(t *testing.T) {
	for _, why := range []string{"guest is not running", "guest not found in inventory", "no ARP answer on vmbr0"} {
		t.Run(why, func(t *testing.T) {
			e := webAndAPI(t)
			e.res.setLevel("www.example.com", resolve.LevelObserved)
			blocked := withSentinel(hostRule("api.example.com"), planner.IngressRule{Hostname: "www.example.com", Service: planner.BlockedService})
			requireUnpublished := func(st State, step string) {
				t.Helper()
				require.Equal(t, []string{"api.example.com"}, e.recordNames(), step)
				for _, a := range actionKinds(st) {
					require.NotContains(t, a, "www.example.com", step)
				}
				require.Equal(t, blocked, e.rules(), step)
				require.Equal(t, []string{oneHeldBack}, st.Problems, "%s: counted once", step)
			}

			requireUnpublished(e.cycle(), "held back")
			bindings, err := e.store.Bindings()
			require.NoError(t, err)
			require.Equal(t, resolve.LevelObserved, bindings["www.example.com"].Level)

			e.res.stop("www.example.com", why)
			e.clock.advance(10 * time.Second)
			st := e.cycle()
			requireUnpublished(st, "withdrawn")
			www := route(st, "www.example.com")
			require.Equal(t, planner.StateUnreachable, www.State)
			require.Equal(t, why, www.Reason, "the reason of the resolver stays")

			e.res.start("www.example.com")
			e.clock.advance(10 * time.Second)
			st = e.cycle()
			requireUnpublished(st, "answering again")
			require.Equal(t, belowPort, route(st, "www.example.com").Reason)
		})
	}
}

func TestARouteServedAtPortKeepsItsRecordWhenWithdrawn(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	require.Equal(t, []string{"www.example.com"}, e.recordNames())

	e.res.stop("www.example.com", "guest is not running")
	e.clock.advance(10 * time.Second)
	st := e.cycle()

	www := route(st, "www.example.com")
	require.Equal(t, planner.StateWithdrawn, www.State)
	require.Equal(t, "guest is not running", www.Reason)
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
	require.Empty(t, st.Problems)
}

// A target that was never verified keeps its own reason and is not counted
// as held back.
func TestAnUnverifiedTargetIsNotHeldBack(t *testing.T) {
	e := webAndAPI(t)
	e.res.stop("www.example.com", "guest is not running")

	st := e.cycle()

	www := route(st, "www.example.com")
	require.Equal(t, planner.StateUnreachable, www.State)
	require.Equal(t, "guest is not running", www.Reason)
	require.Empty(t, www.Level)
	require.Empty(t, st.Problems)
}

func TestWhatTheMinimumHoldsBack(t *testing.T) {
	ref := model.GuestRef{Kind: model.KindQEMU, VMID: 101}
	annotated := model.Route{Hostname: "www.example.com", Source: model.SourceAnnotation, Guest: &ref, Target: model.Target{Scheme: model.SchemeHTTP, Port: 8080}}
	manualToGuest := model.Route{Hostname: "www.example.com", Source: model.SourceManual, ManualID: "www", Guest: &ref, Target: model.Target{Scheme: model.SchemeHTTP, Port: 8080}}
	manualToAddr := model.Route{Hostname: "www.example.com", Source: model.SourceManual, ManualID: "www", Target: model.Target{Scheme: model.SchemeHTTP, Addr: guestAddr, Port: 8080}}
	served := planner.ResolvedTarget{Addr: guestAddr, Reachable: true, Owner: "qemu/101"}
	withdrawn := planner.ResolvedTarget{Addr: guestAddr, Withdrawn: true, Reason: "guest is not running", Owner: "qemu/101"}
	unpublished := withdrawn
	unpublished.Addr = netip.Addr{}
	bound := func(level resolve.Level) *resolve.Binding {
		return &resolve.Binding{Owner: "qemu/101", Hostname: "www.example.com", Guest: "qemu/101", Addr: guestAddr, MAC: testMAC, VerifiedAt: t0, Level: level}
	}
	tests := []struct {
		name   string
		route  model.Route
		res    resolve.Result
		target planner.ResolvedTarget // what the plan is given
		held   bool
	}{
		{name: "proven at port", route: annotated, res: resolve.Result{Target: served, Binding: bound(resolve.LevelPort), Level: resolve.LevelPort}, target: served},
		{
			name: "proven at observed", route: annotated, res: resolve.Result{Target: served, Binding: bound(resolve.LevelObserved), Level: resolve.LevelObserved},
			target: planner.ResolvedTarget{Reason: belowPort}, held: true,
		},
		{
			name: "a guest's route that says manual", route: annotated, res: resolve.Result{Target: served, Level: resolve.LevelManual},
			target: planner.ResolvedTarget{Reason: "identity level manual is below the required port"}, held: true,
		},
		{
			name: "a manual route that names a guest", route: manualToGuest, res: resolve.Result{Target: served, Binding: bound(resolve.LevelObserved), Level: resolve.LevelObserved},
			target: planner.ResolvedTarget{Reason: belowPort}, held: true,
		},
		{name: "a route without a guest", route: manualToAddr, res: resolve.Result{Target: served, Level: resolve.LevelManual}, target: served},
		{name: "a route without a guest whatever its level", route: manualToAddr, res: resolve.Result{Target: served, Level: resolve.LevelObserved}, target: served},
		{name: "withdrawn after a proof at observed", route: annotated, res: resolve.Result{Target: withdrawn, Binding: bound(resolve.LevelObserved)}, target: unpublished, held: true},
		{name: "withdrawn, bound by an older version", route: annotated, res: resolve.Result{Target: withdrawn, Binding: bound("")}, target: unpublished, held: true},
		{name: "withdrawn after a proof at port", route: annotated, res: resolve.Result{Target: withdrawn, Binding: bound(resolve.LevelPort)}, target: withdrawn},
		{name: "withdrawn without a binding", route: manualToAddr, res: resolve.Result{Target: withdrawn}, target: withdrawn},
		{
			name: "never verified", route: annotated, res: resolve.Result{Target: planner.ResolvedTarget{Reason: "guest is not running"}},
			target: planner.ResolvedTarget{Reason: "guest is not running"},
		},
		{
			name: "rejected", route: annotated, res: resolve.Result{Target: planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"}},
			target: planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &cycleRun{
				settings: store.DefaultSettings(),
				install:  store.Install{ID: testInstall},
				claims:   planner.ClaimResult{Winners: []model.Route{tt.route}},
				results:  map[string]resolve.Result{"www.example.com": tt.res},
			}

			c.holdBelowMinimum()

			require.Equal(t, tt.target, c.results["www.example.com"].Target)
			if tt.held {
				require.Len(t, c.st.Problems, 1)
			} else {
				require.Empty(t, c.st.Problems)
			}
		})
	}
}

// A route held back by the minimum takes the path of a winner without a
// verified address: its rule answers 503, no record is planned for it, and a
// record it already has is kept for its claim, neither pointed elsewhere nor
// retired, as the record of a rejected target is.
func TestARouteHeldBackKeepsItsRecordLikeOneWithoutAVerifiedAddress(t *testing.T) {
	e := webAndAPI(t)
	e.cycle()
	require.Equal(t, []string{"api.example.com", "www.example.com"}, e.recordNames())
	recs := e.records()

	e.res.setLevel("www.example.com", resolve.LevelObserved)
	for range 4 {
		e.clock.advance(61 * time.Second)
		st := e.cycle()

		for _, a := range actionKinds(st) {
			require.NotContains(t, a, "www.example.com", "the record is not touched")
		}
		require.Equal(t, planner.StateUnreachable, route(st, "www.example.com").State)
	}

	require.Equal(t, recs, e.records(), "the record stays as it was, past the grace")
	require.Equal(t, withSentinel(hostRule("api.example.com"), planner.IngressRule{Hostname: "www.example.com", Service: "http_status:503"}), e.rules())
	claims, err := e.store.Claims()
	require.NoError(t, err)
	require.Equal(t, "qemu/101", claims["www.example.com"].Owner)
}

// The look at the inventory right before a delete takes only a rejected
// target's route for one that wants no record; a held back one wants its
// record as any other.
func TestAHeldBackTargetIsNotRejected(t *testing.T) {
	ref := model.GuestRef{Kind: model.KindQEMU, VMID: 101}
	www := model.Route{
		Hostname: "www.example.com", Source: model.SourceAnnotation, Guest: &ref,
		Target: model.Target{Scheme: model.SchemeHTTP, Port: 8080},
	}
	c := &cycleRun{
		settings: store.DefaultSettings(),
		install:  store.Install{ID: testInstall},
		claims:   planner.ClaimResult{Winners: []model.Route{www}},
		results: map[string]resolve.Result{"www.example.com": {
			Target: planner.ResolvedTarget{Addr: guestAddr, Reachable: true, Owner: "qemu/101"},
			Level:  resolve.LevelObserved,
		}},
	}

	c.holdBelowMinimum()

	require.Equal(t, planner.ResolvedTarget{Reason: belowPort}, c.results["www.example.com"].Target)
	require.Equal(t, map[string]planner.ResolvedTarget{"www.example.com": {Reason: belowPort, Level: "observed"}}, c.targets())
	require.Empty(t, c.rejectedRoutes())
}
