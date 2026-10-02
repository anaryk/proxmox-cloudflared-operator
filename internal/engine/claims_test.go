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

	// Saved before the call returned, the holder last in line.
	claim := wwwClaim(t, e)
	require.Equal(t, "qemu/102", claim.Owner)
	require.Equal(t, "uuid:102", claim.Identity)
	require.True(t, at.Equal(claim.Since))
	require.Nil(t, claim.MissingSince)
	require.Len(t, claim.Waiting, 1)
	require.Equal(t, "qemu/101", claim.Waiting[0].Owner)
	require.True(t, at.Equal(claim.Waiting[0].FirstSeen))
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
		{"a guest that does not claim the hostname", "www.example.com", "qemu/103", ErrNotFound,
			"not found: qemu/103 does not claim www.example.com: it has no route for it and does not name it"},
		{"a guest Proxmox does not list", "www.example.com", "qemu/104", ErrNotFound,
			"not found: qemu/104 does not claim www.example.com: it has no route for it and does not name it"},
		{"a manual route that does not claim it", "www.example.com", "manual/www", ErrNotFound,
			"not found: manual/www does not claim www.example.com: it has no route for it and does not name it"},
		{"the holder", "www.example.com", "qemu/101", ErrInvalid, "invalid request: qemu/101 holds www.example.com already"},
		{"a hostname nobody holds", "nope.example.com", "qemu/102", ErrNotFound, "not found: nobody holds a claim on nope.example.com"},
		{"no owner", "www.example.com", "web-2", ErrInvalid,
			`invalid request: "web-2" is no owner: name a guest, as qemu/101 or lxc/200, or a manual route, as manual/<id>`},
		{"a guest written another way", "www.example.com", "qemu/0102", ErrInvalid,
			`invalid request: "qemu/0102" is no owner: name a guest, as qemu/101 or lxc/200, or a manual route, as manual/<id>`},
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
func TestResolveClaimNeedsACompleteListing(t *testing.T) {
	t.Run("before the first cycle", func(t *testing.T) {
		e := newEnv(t)
		require.NoError(t, e.store.SaveClaims(map[string]planner.Claim{
			"www.example.com": {Hostname: "www.example.com", Owner: "qemu/101", Since: t0},
		}))

		err := e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102")

		require.ErrorIs(t, err, ErrRefused)
		require.Equal(t, "qemu/101", wwwClaim(t, e).Owner)
	})
	t.Run("after a cycle that found the inventory incomplete", func(t *testing.T) {
		e := contested(t)
		e.inv.set(incomplete("cluster status: no quorum", webOne, webTwo))
		e.clock.advance(10 * time.Second)
		e.cycle()

		err := e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102")

		require.ErrorIs(t, err, ErrRefused)
		require.EqualError(t, err, "refused: the last cycle did not list every guest under a policy it could read, "+
			"so who claims www.example.com is not known; try again once it has")
		require.Equal(t, "qemu/101", wwwClaim(t, e).Owner)
	})
	t.Run("after a cycle that could not read the settings", func(t *testing.T) {
		e := contested(t)
		require.NoError(t, os.WriteFile(filepath.Join(e.paths.Cluster, "meta", "settings.json"),
			[]byte(`{"schemaVersion":1,"rev":9,"id":"settings","data":{"gateTag":"cf-tunnel","pollInterval":"10s","grace":"1m0s",`+
				`"admission":"tag","observeOnly":false,"denyHosts":["bad host"]}}`), 0o600))
		e.clock.advance(10 * time.Second)
		e.cycle()

		require.ErrorIs(t, e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102"), ErrRefused)
	})
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
