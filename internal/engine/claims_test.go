package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

var (
	webOne = guest(101, "web-1", "www.example.com -> :8080")
	webTwo = guest(102, "web-2", "www.example.com -> :9090")
)

// contested is an engine in enforce mode that serves www.example.com for
// qemu/101 on port 8080, while qemu/102 asks for it on port 9090.
func contested(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot(webOne, webTwo))
	st := e.cycle()
	require.Equal(t, planner.StateActive, wwwRoute(st, "qemu/101").State)
	require.Equal(t, planner.StateConflict, wwwRoute(st, "qemu/102").State)
	require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules())
	return e
}

// wwwRoute is the route of an owner for www.example.com.
func wwwRoute(st State, owner string) RouteView {
	for _, r := range st.Routes {
		if r.Hostname == "www.example.com" && r.Owner == owner {
			return r
		}
	}
	return RouteView{}
}

// wwwClaim is the claim the store keeps on www.example.com.
func wwwClaim(t *testing.T, e *env) planner.Claim {
	t.Helper()
	claims, err := e.store.Claims()
	require.NoError(t, err)
	return claims["www.example.com"]
}

// otherPort is the rule that serves www.example.com on port 9090, as qemu/102
// asks for it.
var otherPort = planner.IngressRule{Hostname: "www.example.com", Service: "http://10.0.0.11:9090"}

func TestResolveClaimHandsTheHostnameToTheOwnerThatWaits(t *testing.T) {
	e := contested(t)
	e.clock.advance(5 * time.Second)
	at := e.clock.now()

	require.NoError(t, e.eng.ResolveClaim(t.Context(), "WWW.Example.com.", "qemu/102"))

	// Saved before the call returned; the holder keeps its place in line,
	// which is where its claim began.
	claim := wwwClaim(t, e)
	require.Equal(t, "qemu/102", claim.Owner)
	require.Equal(t, "uuid:102", claim.Identity)
	require.True(t, at.Equal(claim.Since))
	require.Nil(t, claim.MissingSince)
	require.Len(t, claim.Waiting, 1)
	require.Equal(t, "qemu/101", claim.Waiting[0].Owner)
	require.True(t, t0.Equal(claim.Waiting[0].FirstSeen))
	require.Equal(t, []string{"www.example.com: the claim on www.example.com was moved from qemu/101 to qemu/102 by the admin; " +
		"qemu/101 waits for it from now on"}, adminEvents(e, at.Add(-time.Nanosecond)))

	e.clock.advance(15 * time.Second)
	st := e.cycle()

	require.Equal(t, planner.StateActive, wwwRoute(st, "qemu/102").State)
	old := wwwRoute(st, "qemu/101")
	require.Equal(t, planner.StateConflict, old.State)
	require.Equal(t, "hostname is held by qemu/102", old.Reason)
	require.Equal(t, withSentinel(otherPort), e.rules())
	require.Empty(t, claimEventsAfter(e, at), "the claim rules report no change of their own")
}

// Neither the holder the admin moved the hostname away from nor a clone of
// it takes it back while the new holder asks for it, however long it waits.
func TestTheOldHolderDoesNotTakeAResolvedHostnameBack(t *testing.T) {
	e := contested(t)
	require.NoError(t, e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102"))
	clone := guest(103, "web-1-clone", "www.example.com -> :8080")
	clone.Identity = webOne.Identity
	e.inv.set(snapshot(webOne, webTwo, clone))

	var st State
	for range 5 {
		e.clock.advance(61 * time.Second)
		st = e.cycle()
	}

	require.Equal(t, "qemu/102", wwwClaim(t, e).Owner)
	require.Equal(t, planner.StateActive, wwwRoute(st, "qemu/102").State)
	require.Equal(t, planner.StateConflict, wwwRoute(st, "qemu/101").State)
	require.Equal(t, planner.StateConflict, wwwRoute(st, "qemu/103").State)
	require.Equal(t, withSentinel(otherPort), e.rules())
}

func TestAResolvedClaimOutlivesARestart(t *testing.T) {
	e := contested(t)
	require.NoError(t, e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102"))

	e.restart()
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Equal(t, planner.StateActive, wwwRoute(st, "qemu/102").State)
	require.Equal(t, withSentinel(otherPort), e.rules())
}

// An owner that names the hostname without a route for it claims it too: the
// hostname is then held for it, and served by nobody.
func TestAClaimCanGoToAnOwnerThatOnlyNamesTheHostname(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	namer := guest(102, "web-2")
	namer.Description = "www.example.com moves here soon\n```cf-tunnel\nwww.example.com -> \n```"
	e.inv.set(snapshot(webOne, namer))
	st := e.cycle()
	require.Equal(t, planner.StateActive, wwwRoute(st, "qemu/101").State)

	require.NoError(t, e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102"))
	e.clock.advance(20 * time.Second)
	st = e.cycle()

	require.Equal(t, planner.StateHeld, wwwRoute(st, "qemu/102").State)
	require.Equal(t, planner.StateConflict, wwwRoute(st, "qemu/101").State)
	require.Equal(t, withSentinel(), e.rules(), "nobody serves it")
}

func TestResolveClaimRefuses(t *testing.T) {
	stranger := guest(103, "other", "api.example.com -> :8080")
	for _, tt := range []struct {
		name, host, owner string
		want              error
		message           string
	}{
		{"a guest that does not claim the hostname", "www.example.com", "qemu/103", ErrRefused,
			"refused: qemu/103 no longer claims www.example.com: it has no route for it and does not name it"},
		{"a guest Proxmox does not list", "www.example.com", "qemu/104", ErrRefused,
			"refused: qemu/104 no longer claims www.example.com: it has no route for it and does not name it"},
		{"a manual route that does not claim it", "www.example.com", "manual/www", ErrRefused,
			"refused: manual/www no longer claims www.example.com: it has no route for it and does not name it"},
		{"the holder", "www.example.com", "qemu/101", ErrInvalid, "invalid request: qemu/101 holds www.example.com already"},
		{"a hostname nobody holds", "nope.example.com", "qemu/102", ErrNotFound, "not found: nobody holds a claim on nope.example.com"},
		{"no owner", "www.example.com", "web-2", ErrInvalid,
			`invalid request: "web-2" is no owner: name a guest, as qemu/101 or lxc/200, or a manual route, as manual/<id>`},
		{"a guest written another way", "www.example.com", "qemu/0102", ErrInvalid,
			`invalid request: "qemu/0102" is no owner: name a guest, as qemu/101 or lxc/200, or a manual route, as manual/<id>`},
		{"a manual route with white space", "www.example.com", "manual/ www", ErrInvalid,
			`invalid request: "manual/ www" is no owner: name a guest, as qemu/101 or lxc/200, or a manual route, as manual/<id>`},
		{"a manual route without an id", "www.example.com", "manual/", ErrInvalid,
			`invalid request: "manual/" is no owner: name a guest, as qemu/101 or lxc/200, or a manual route, as manual/<id>`},
		{"no hostname", "not a host", "qemu/102", ErrInvalid, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.enforce()
			e.inv.set(snapshot(webOne, webTwo, stranger))
			e.cycle()
			before, since := e.files(), e.clock.now()

			err := e.eng.ResolveClaim(t.Context(), tt.host, tt.owner)

			require.ErrorIs(t, err, tt.want)
			if tt.message != "" {
				require.EqualError(t, err, tt.message)
			}
			require.Equal(t, before, e.files(), "nothing is saved")
			require.Empty(t, adminEvents(e, since.Add(-time.Nanosecond)))
		})
	}
}

// Whether an owner claims a hostname is known only from a listing of every
// guest under a policy that could be read.
// Whether an owner claims a hostname is asked of the inventory afresh, when
// the hostname is about to move: the last cycle may be a poll interval old.
func TestResolveClaimLooksAtTheInventoryAgain(t *testing.T) {
	t.Run("a fresh look is taken", func(t *testing.T) {
		e := contested(t)
		refreshes := e.inv.refreshes()

		require.NoError(t, e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102"))

		require.Equal(t, refreshes+1, e.inv.refreshes())
	})
	t.Run("before the first cycle of this process", func(t *testing.T) {
		e := newEnv(t)
		e.inv.set(snapshot(webOne, webTwo))
		require.NoError(t, e.store.SaveClaims(map[string]planner.Claim{
			"www.example.com": {Hostname: "www.example.com", Owner: "qemu/101", Since: t0},
		}))

		require.NoError(t, e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102"))

		require.Equal(t, "qemu/102", wwwClaim(t, e).Owner)
	})
	t.Run("a fresh look that is incomplete", func(t *testing.T) {
		e := contested(t)
		e.inv.enqueue(incomplete("cluster status: no quorum", webOne, webTwo))
		before := e.files()

		err := e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102")

		require.ErrorIs(t, err, ErrRefused)
		require.EqualError(t, err, "refused: the inventory is incomplete (cluster status: no quorum), "+
			"so who claims www.example.com now is not known; try again once it is complete")
		require.Equal(t, before, e.files())
	})
	t.Run("settings that cannot be read", func(t *testing.T) {
		e := contested(t)
		require.NoError(t, os.WriteFile(filepath.Join(e.paths.Cluster, "meta", "settings.json"),
			[]byte(`{"schemaVersion":1,"rev":9,"id":"settings","data":{"gateTag":"cf-tunnel","pollInterval":"10s","grace":"1m0s",`+
				`"admission":"tag","observeOnly":false,"denyHosts":["bad host"]}}`), 0o600))

		err := e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102")

		require.ErrorIs(t, err, ErrRefused, "what cannot be read tells nothing of who claims it")
		require.ErrorContains(t, err, "refused: reading the settings failed (")
		require.ErrorContains(t, err, "), so who claims www.example.com now is not known")
		require.Equal(t, "qemu/101", wwwClaim(t, e).Owner)
	})
	t.Run("a policy that cannot be used", func(t *testing.T) {
		// The store refuses such settings, so the look is given them.
		e := contested(t)
		s, err := e.store.Settings()
		require.NoError(t, err)
		s.DenyHosts = []string{"bad host"}

		_, err = e.eng.claimantsUnder(t.Context(), "www.example.com", s)

		require.ErrorIs(t, err, ErrRefused)
		require.EqualError(t, err, "refused: the settings contain an invalid allow or deny pattern, so who claims www.example.com is not known")
	})
	t.Run("the identity is the one of the fresh look", func(t *testing.T) {
		e := contested(t)
		recreated := webTwo
		recreated.Identity = "uuid:102-recreated"
		e.inv.set(snapshot(webOne, recreated))

		require.NoError(t, e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102"))

		require.Equal(t, "uuid:102-recreated", wwwClaim(t, e).Identity)
	})
	t.Run("the look has a deadline of its own", func(t *testing.T) {
		e := contested(t)
		deadlines := &timeouts{}
		e.eng.timeout = deadlines.withTimeout

		require.NoError(t, e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102"))

		require.Equal(t, 1, deadlines.count(20*time.Second))
		require.Zero(t, deadlines.count(refreshTimeout))
	})
}

// The fresh look is taken before the cycle lock: a resolve that waits for
// the inventory holds no cycle up.
func TestAResolveLooksWithoutHoldingTheCycles(t *testing.T) {
	e := contested(t)
	cycled := false
	e.inv.hook(func() {
		e.inv.hook(nil)
		select {
		case e.eng.sem <- struct{}{}:
			<-e.eng.sem
		default:
			return // the lock is held: no cycle could run now
		}
		e.clock.advance(20 * time.Second)
		e.cycle()
		cycled = true
	})

	require.NoError(t, e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102"))

	require.True(t, cycled, "a cycle ran while the resolve looked")
	require.Equal(t, "qemu/102", wwwClaim(t, e).Owner)
}

// 101 holds, its clone 103 waits since t0, 102 since t0+10s. 102 drops its
// route, and before a cycle notices, the admin resolves to 102: nothing moves,
// and 101 goes on serving.
func TestAResolveToAnOwnerThatJustDroppedItsRouteMovesNothing(t *testing.T) {
	e, clone := threeClaimants(t)
	e.inv.set(snapshot(webOne, untagged(webTwo), clone))
	before := e.files()

	err := e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102")

	require.ErrorIs(t, err, ErrRefused)
	require.EqualError(t, err, "refused: qemu/102 no longer claims www.example.com: it has no route for it and does not name it")
	require.Equal(t, before, e.files())
	var st State
	for range 3 {
		e.clock.advance(61 * time.Second)
		st = e.cycle()
	}
	require.Equal(t, planner.StateActive, wwwRoute(st, "qemu/101").State)
	require.Equal(t, "qemu/101", wwwClaim(t, e).Owner)
	require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules())
}

// The holder the admin moved the hostname away from keeps its place in line:
// when the new holder stops asking, the hostname comes back to it, not to a
// clone that waited behind it.
func TestTheOldHolderGetsTheHostnameBackWhenTheNewHolderLetsGo(t *testing.T) {
	e, clone := threeClaimants(t)
	require.NoError(t, e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102"))
	e.clock.advance(20 * time.Second)
	require.Equal(t, planner.StateActive, wwwRoute(e.cycle(), "qemu/102").State)

	e.inv.set(snapshot(webOne, untagged(webTwo), clone))
	var st State
	for range 3 {
		e.clock.advance(61 * time.Second)
		st = e.cycle()
	}

	require.Equal(t, "qemu/101", wwwClaim(t, e).Owner)
	require.Equal(t, planner.StateActive, wwwRoute(st, "qemu/101").State)
	require.Equal(t, planner.StateConflict, wwwRoute(st, "qemu/103").State)
}

// threeClaimants is an engine in enforce mode where qemu/101 holds
// www.example.com, its clone qemu/103 waits since t0 and qemu/102 since
// t0+10s.
func threeClaimants(t *testing.T) (*env, model.Guest) {
	t.Helper()
	e := newEnv(t)
	e.enforce()
	clone := guest(103, "web-1-clone", "www.example.com -> :8080")
	clone.Identity = webOne.Identity
	e.inv.set(snapshot(webOne, clone))
	e.cycle()
	e.inv.set(snapshot(webOne, webTwo, clone))
	e.clock.advance(10 * time.Second)
	st := e.cycle()
	require.Equal(t, planner.StateActive, wwwRoute(st, "qemu/101").State)
	waiting := wwwClaim(t, e).Waiting
	require.Len(t, waiting, 2)
	require.Equal(t, "qemu/103", waiting[0].Owner)
	require.True(t, t0.Equal(waiting[0].FirstSeen))
	require.Equal(t, "qemu/102", waiting[1].Owner)
	return e, clone
}

func TestResolveClaimAsksForACycle(t *testing.T) {
	e := contested(t)
	drain(e)

	require.NoError(t, e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102"))

	requireTriggered(t, e)
}

// drain empties the trigger of the engine.
func drain(e *env) {
	select {
	case <-e.eng.trigger:
	default:
	}
}

func requireTriggered(t *testing.T, e *env) {
	t.Helper()
	select {
	case <-e.eng.trigger:
	default:
		t.Fatal("no cycle was asked for")
	}
}

func TestAClaimThatCannotBeSavedIsNotMoved(t *testing.T) {
	skipAsRoot(t)
	e := contested(t)
	dir := filepath.Join(e.paths.Cluster, "claims")
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	before, since := e.files(), e.clock.now()

	err := e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102")

	require.ErrorContains(t, err, "saving the claims")
	require.NotErrorIs(t, err, ErrRefused)
	require.Equal(t, before, e.files())
	require.Empty(t, adminEvents(e, since.Add(-time.Nanosecond)))
	require.NoError(t, os.Chmod(dir, 0o700))
	e.clock.advance(10 * time.Second)
	st := e.cycle()
	require.Equal(t, planner.StateActive, wwwRoute(st, "qemu/101").State)
}

// skipAsRoot skips a test that relies on file permissions, which root ignores.
func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
}

// What a claim's state is not known, or about to change, it says so.
func TestAClaimSaysWhatIsNotKnownAndWhatIsPending(t *testing.T) {
	e := contested(t)

	e.restart()
	views, err := e.eng.Claims()
	require.NoError(t, err)
	require.Len(t, views, 1)
	require.Equal(t, ClaimUnknown, views[0].State, "no cycle of this process settled the claims")

	e.clock.advance(20 * time.Second)
	e.cycle()
	require.NoError(t, e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102"))
	views, err = e.eng.Claims()
	require.NoError(t, err)
	require.Equal(t, "qemu/102", views[0].Holder)
	require.Equal(t, ClaimPending, views[0].State, "the last cycle served it from qemu/101")

	e.clock.advance(20 * time.Second)
	e.cycle()
	views, err = e.eng.Claims()
	require.NoError(t, err)
	require.Equal(t, ClaimConflict, views[0].State)
}

// The states are those of the last cycle that settled the claims: a cycle that
// holds before it settles them changes none, and when no cycle of this
// process has settled them, they are not known.
func TestClaimStatesComeFromTheLastCycleThatSettledThem(t *testing.T) {
	e := contested(t)
	e.inv.set(incomplete("cluster status: no quorum", webOne, webTwo))
	states := func() []string {
		t.Helper()
		views, err := e.eng.Claims()
		require.NoError(t, err)
		out := make([]string, len(views))
		for i, v := range views {
			out[i] = v.State
		}
		return out
	}

	e.clock.advance(20 * time.Second)
	e.cycle()
	require.Equal(t, []string{ClaimConflict}, states(), "kept from the cycle that settled them")

	e.restart()
	e.clock.advance(20 * time.Second)
	e.cycle()
	require.Equal(t, []string{ClaimUnknown}, states(), "no cycle of this process settled them")
}

func TestClaimsAreListedWithTheirState(t *testing.T) {
	e := contested(t)
	e.inv.set(snapshot(webOne, webTwo, guest(103, "api", "api.example.com -> :8080"), guest(104, "old", "old.example.com -> :8080")))
	e.clock.advance(10 * time.Second)
	e.cycle()
	e.inv.set(snapshot(webOne, webTwo, guest(103, "api", "api.example.com -> :8080"), untagged(guest(104, "old"))))
	e.clock.advance(10 * time.Second)
	gone := e.clock.now()
	e.cycle()

	views, err := e.eng.Claims()

	require.NoError(t, err)
	at := t0.Add(10 * time.Second)
	ref := func(vmid int) model.GuestRef { return model.GuestRef{Kind: model.KindQEMU, VMID: vmid} }
	require.Equal(t, []ClaimView{
		{
			Hostname: "api.example.com", Holder: "qemu/103", Guest: &GuestView{GuestRef: ref(103), Name: "api"},
			Since: at, State: ClaimServing, Waiting: []ClaimantView{},
		},
		{
			Hostname: "old.example.com", Holder: "qemu/104", Guest: &GuestView{GuestRef: ref(104), Name: "old"},
			Since: at, MissingSince: &gone, State: ClaimHeld, Waiting: []ClaimantView{},
		},
		{
			Hostname: "www.example.com", Holder: "qemu/101", Guest: &GuestView{GuestRef: ref(101), Name: "web-1"},
			Since: t0, State: ClaimConflict,
			Waiting: []ClaimantView{{Owner: "qemu/102", Guest: &GuestView{GuestRef: ref(102), Name: "web-2"}, Since: t0}},
		},
	}, normalizedTimes(views))
}

// normalizedTimes gives the times of the views the location of the test's
// own, as a round trip through the store may change it.
func normalizedTimes(views []ClaimView) []ClaimView {
	for i := range views {
		views[i].Since = views[i].Since.UTC()
		if m := views[i].MissingSince; m != nil {
			utc := m.UTC()
			views[i].MissingSince = &utc
		}
		for j := range views[i].Waiting {
			views[i].Waiting[j].Since = views[i].Waiting[j].Since.UTC()
		}
	}
	return views
}
