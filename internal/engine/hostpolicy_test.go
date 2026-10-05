package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestTheApexIsRejectedUntilAllowHostsNamesIt(t *testing.T) {
	e := newEnv(t)
	e.inv.set(snapshot(guest(101, "web-1", "example.com *.example.com www.example.com -> :8080")))
	e.enforce()

	st := e.cycle()

	require.Equal(t, planner.StateRejected, route(st, "example.com").State)
	require.Equal(t, `the apex of zone example.com is published only when allowHosts names it: add "example.com" to allowHosts`,
		route(st, "example.com").Reason)
	require.Equal(t, planner.StateRejected, route(st, "*.example.com").State)
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
	require.Contains(t, st.Problems, `hostname example.com of qemu/101 is not published: the apex of zone example.com is published `+
		`only when allowHosts names it: add "example.com" to allowHosts`)
	claims, err := e.store.Claims()
	require.NoError(t, err)
	require.NotContains(t, claims, "example.com", "a refused route takes no claim")
	require.NotContains(t, claims, "*.example.com")
	require.Contains(t, claims, "www.example.com")

	e.settings(func(s *store.Settings) { s.AllowHosts = []string{"example.com", "*.example.com"} })
	e.clock.advance(time.Minute)
	st = e.cycle()

	require.Equal(t, planner.StateActive, route(st, "example.com").State)
	require.Equal(t, []string{"*.example.com", "example.com", "www.example.com"}, e.recordNames())
}

// Reproduced: a guest that named the apex of a zone no credential listed yet
// took a claim on it, and kept it over a guest before it in owner order once
// the zone was listed and allowHosts named the apex.
func TestANameInAZoneNeverServedTakesNoClaim(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot(guest(300, "web-3", "example.org -> :8080")))

	st := e.cycle()

	require.Equal(t, planner.StateNoZone, route(st, "example.org").State)
	require.NotContains(t, storedClaims(t, e), "example.org")

	e.cf.AddZone("zone2", "example.org", testAccount)
	e.settings(func(s *store.Settings) { s.AllowHosts = []string{"*", "example.org"} })
	e.inv.set(snapshot(guest(200, "web-2", "example.org -> :8080"), guest(300, "web-3", "example.org -> :8080")))
	e.clock.advance(zoneRefreshEvery)
	st = e.cycle()

	require.Equal(t, "qemu/200", storedClaims(t, e)["example.org"].Owner, "settled as any free hostname is")
	require.Equal(t, planner.StateActive, ownedRoute(st, "example.org", "qemu/200").State)
	require.Equal(t, planner.StateConflict, ownedRoute(st, "example.org", "qemu/300").State)
	require.Equal(t, []string{"example.org"}, recordNamesIn(e, "zone2"))
}

// A claim an earlier release let a guest take on a name in a zone never
// served goes at once.
func TestAClaimInAZoneNeverServedGoesAtOnce(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	require.NoError(t, e.store.SaveClaims(map[string]planner.Claim{
		"example.org": {Hostname: "example.org", Owner: "qemu/300", Identity: "uuid:300", Since: e.clock.now()},
	}))
	e.inv.set(snapshot(guest(300, "web-3", "example.org -> :8080")))

	e.cycle()

	require.NotContains(t, storedClaims(t, e), "example.org")
	events := claimEventsAfter(e, time.Time{})
	require.Len(t, events, 1)
	require.Equal(t, "released qemu/300: it may take no claim on it: no Cloudflare zone for this hostname in any credential", events[0].Message)
}

// The claims in a zone the install served are kept as they are while the
// zone is in doubt or gone from its listing: a guest before the holder in
// owner order that names the hostname meanwhile waits.
func TestTheClaimsInAServedZoneOutliveItsDoubt(t *testing.T) {
	e, view := servingTwoZones(t)
	e.inv.set(snapshot(guest(100, "web-0", "www.example.org -> :8080"), guest(101, "web-1", "www.example.com www.example.org -> :8080")))

	e.refuse()
	st := e.cycle()
	require.Equal(t, RouteFrozen, ownedRoute(st, "www.example.org", "qemu/101").State)
	require.Equal(t, "qemu/101", storedClaims(t, e)["www.example.org"].Owner, "in doubt")

	view.hide("zone2", true)
	e.clock.advance(zoneRefreshEvery)
	e.cycle()
	require.Equal(t, "qemu/101", storedClaims(t, e)["www.example.org"].Owner, "gone from its listing")
}

func TestTheClaimsInAZoneLetGoAreKept(t *testing.T) {
	e := letGo(t, false)
	e.inv.set(snapshot(guest(100, "web-0", "www.example.org -> :8080"), guest(101, "web-1", "www.example.com www.example.org -> :8080")))

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Equal(t, "qemu/101", storedClaims(t, e)["www.example.org"].Owner)
	require.Equal(t, planner.StateConflict, ownedRoute(st, "www.example.org", "qemu/100").State)
}

// Reproduced: a holder whose route the policy came to refuse kept its claim
// for the grace, and for as long as a broken entry still named the hostname.
func TestARefusedHolderLosesItsClaimAtOnce(t *testing.T) {
	for _, tt := range []struct{ name, entry string }{
		{"its route refused", "example.com -> :8080"},
		{"its entry broken as well", "example.com -> :80800"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.enforce()
			e.settings(func(s *store.Settings) { s.AllowHosts = []string{"*", "example.com"} })
			e.inv.set(snapshot(guest(101, "web-1", "example.com www.example.com -> :8080")))
			e.cycle()
			require.Equal(t, "qemu/101", storedClaims(t, e)["example.com"].Owner)
			since := e.clock.now()

			e.settings(func(s *store.Settings) { s.AllowHosts = []string{"*"} })
			e.inv.set(snapshot(guest(101, "web-1", tt.entry, "www.example.com -> :8080")))
			e.clock.advance(10 * time.Second)
			e.cycle()

			require.NotContains(t, storedClaims(t, e), "example.com")
			events := claimEventsAfter(e, since)
			require.Len(t, events, 1)
			require.Equal(t, `released qemu/101: it may take no claim on it: the apex of zone example.com is published only when `+
				`allowHosts names it: add "example.com" to allowHosts`, events[0].Message)
		})
	}
}

// Unread, the memory does not say which zones the install served: no claim
// goes for want of one.
func TestAMemoryThatCannotBeReadLetsNoClaimGo(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	require.NoError(t, e.store.SaveClaims(map[string]planner.Claim{
		"www.example.org": {Hostname: "www.example.org", Owner: "qemu/101", Identity: "uuid:101", Since: e.clock.now()},
	}))
	require.NoError(t, os.MkdirAll(filepath.Dir(e.memoryFile()), 0o700))
	require.NoError(t, os.WriteFile(e.memoryFile(), []byte("{"), 0o600))
	e.inv.set(snapshot(guest(101, "web-1", "www.example.org -> :8080")))

	st := e.cycle()

	require.True(t, hasProblem(st, "reading what the engine remembered"), "%v", st.Problems)
	require.Equal(t, "qemu/101", storedClaims(t, e)["www.example.org"].Owner)
}

func storedClaims(t *testing.T, e *env) map[string]planner.Claim {
	t.Helper()
	claims, err := e.store.Claims()
	require.NoError(t, err)
	return claims
}

func ownedRoute(st State, host, owner string) RouteView {
	for _, r := range st.Routes {
		if r.Hostname == host && r.Owner == owner {
			return r
		}
	}
	return RouteView{}
}

func recordNamesIn(e *env, zone string) []string {
	var out []string
	for _, r := range e.cf.RecordsIn(zone) {
		out = append(out, r.Name)
	}
	return out
}

func TestAGuestOverTheCapOfHostnamesPublishesNothing(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *store.Settings) { s.MaxHostnamesPerGuest = 1 })
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com api.example.com -> :8080")))
	e.enforce()

	st := e.cycle()

	require.Equal(t, []planner.Issue{{Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 101},
		Msg: "the Notes name 2 hostnames, more than maxHostnamesPerGuest allows (1); none of them is published until they name at most 1"}}, st.Issues)
	require.Empty(t, st.Routes)
	require.Empty(t, e.recordNames())
}

// Every guest with the gate tag counts, a template too: a clone of either is
// a tagged guest.
func TestTheStateCountsTheGuestsWithTheGateTag(t *testing.T) {
	e := newEnv(t)
	tmpl := guest(900, "base", "www.example.org -> :80")
	tmpl.Template = true
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), untagged(guest(102, "db", "")), tmpl))

	st := e.cycle()

	require.Equal(t, "tag", st.Admission)
	require.Equal(t, 2, st.GateTagged)

	e.settings(func(s *store.Settings) { s.Admission = store.AdmissionApprove })
	e.clock.advance(10 * time.Second)
	st = e.cycle()
	require.Equal(t, "approve", st.Admission)
}
