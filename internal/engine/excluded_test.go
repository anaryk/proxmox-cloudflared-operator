package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const leftOutReason = "credential main can list example.org but not read its DNS: grant Zone > DNS > Edit to serve it"

// unreadableEnv is an engine in enforce mode whose credential lists
// example.com and example.org but may read the DNS of example.com only, as
// the common least-privilege token does, with a guest that publishes a name
// in each zone.
func unreadableEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.cf.AddZone("zone2", "example.org", testAccount)
	e.cf.Deny("dns.read", "zone2")
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com www.example.org -> :8080")))
	e.enforce()
	return e
}

func (e *env) check(id string) {
	e.t.Helper()
	_, err := e.eng.CheckCredential(e.t.Context(), id, false)
	require.NoError(e.t, err)
}

// unreadable refuses to show the records of some zones, as Cloudflare does
// for a token without DNS permission there.
type unreadable struct {
	cfapi.API
	zones []string
}

func (u unreadable) Records(ctx context.Context, zoneID string, f cfapi.RecordFilter) ([]cfapi.Record, error) {
	if slices.Contains(u.zones, zoneID) {
		return nil, &cfapi.Error{Status: http.StatusForbidden, Codes: []int{10000}, Message: "Authentication error"}
	}
	return u.API.Records(ctx, zoneID, f)
}

func TestATokenThatCannotReadTheDNSOfEveryZoneIsAdded(t *testing.T) {
	e := unreadableEnv(t)
	require.NoError(t, e.store.DeleteCredential(testCred))

	view, err := e.eng.AddCredential(t.Context(), "main", "another-token-0123456789")

	require.NoError(t, err)
	require.True(t, view.Report.Usable)
	require.Equal(t, []credentials.Exclusion{{
		Zone: "example.org", ZoneID: "zone2", Reason: "no DNS read", Detail: "grant Zone > DNS > Edit on example.org",
	}}, view.Report.Excluded)
	creds, err := e.store.Credentials()
	require.NoError(t, err)
	require.Len(t, creds, 1)
}

func TestATokenWhoseZoneReadGotNoAnswerIsNotAdded(t *testing.T) {
	e := unreadableEnv(t)
	require.NoError(t, e.store.DeleteCredential(testCred))
	// example.com is read first.
	e.cf.FailNext("dns.read", 1, &cfapi.Error{Status: http.StatusServiceUnavailable, Message: "unavailable"})

	view, err := e.eng.AddCredential(t.Context(), "main", "another-token-0123456789")

	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, "the token could not be checked: dns.read on example.com: Cloudflare did not answer")
	require.Len(t, view.Report.Excluded, 1, "only the refusal leaves a zone out")
	creds, err := e.store.Credentials()
	require.NoError(t, err)
	require.Empty(t, creds)
}

func TestAZoneLeftOutIsNotServed(t *testing.T) {
	e := unreadableEnv(t)
	e.check(testCred)
	n := len(e.cf.Calls())

	st := e.cycle()

	require.Empty(t, st.Problems, "a zone left out is no problem")
	require.Empty(t, st.Hold)
	require.Equal(t, planner.StateActive, route(st, "www.example.com").State)
	org := route(st, "www.example.org")
	require.Equal(t, planner.StateNoZone, org.State)
	require.Equal(t, leftOutReason, org.Reason)
	require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules())
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
	noDNSIn(t, e.callsSince(n), "zone2")
	require.Equal(t, "example.org left out: no DNS read", credentialView(st, testCred).Report.LeftOut())
}

func TestARouteThatLostItsNameInAZoneLeftOutKeepsItsReason(t *testing.T) {
	e := unreadableEnv(t)
	e.inv.set(snapshot(
		guest(101, "web-1", "www.example.org -> :8080"),
		guest(102, "web-2", "www.example.org -> :8080"),
	))
	e.check(testCred)

	st := e.cycle()

	var reasons []string
	for _, r := range st.Routes {
		reasons = append(reasons, string(r.State)+": "+r.Reason)
	}
	require.Equal(t, []string{"no-zone: " + leftOutReason, "conflict: hostname is held by qemu/101"}, reasons)
}

// The check of the token does not probe the account either: the token may
// have no tunnel permission there.
func TestTheAccountOfAZoneLeftOutIsNotLookedAt(t *testing.T) {
	e := newEnv(t)
	e.cf.AddZone("zone2", "example.org", "acc2")
	e.cf.Deny("dns.read", "zone2")
	e.cf.Deny("tunnel.read", "acc2")
	e.enforce()
	e.check(testCred)

	st := e.cycle()

	require.Empty(t, st.Problems)
	for _, c := range e.cf.Calls() {
		require.NotContains(t, c, "acc2")
	}
	keep, pruned := e.conn.lastPrune()
	require.True(t, pruned, "every account that matters answered")
	require.Len(t, keep, 1)
}

func TestAZoneLeftOutThatLeavesTheListingIsNotInDoubt(t *testing.T) {
	e := unreadableEnv(t)
	view := newZoneView(e.cf)
	e.useAPI(testToken, view)
	e.check(testCred)
	e.cycle()

	view.hide("zone2", true)
	e.clock.advance(zoneRefreshEvery)
	st := e.cycle()

	require.Empty(t, st.Problems, "pco never served the zone")
	require.Equal(t, planner.StateActive, route(st, "www.example.com").State)
}

func TestAPinToACredentialThatLeavesTheZoneOut(t *testing.T) {
	e := unreadableEnv(t)
	e.check(testCred)
	e.settings(func(s *store.Settings) { s.ZonePins = map[string]string{"example.org": testCred} })

	st := e.cycle()

	require.Equal(t, []string{"zone example.org is pinned to credential cred1, which can list it but not read its DNS; " +
		"it is not served until the pin is changed or the credential is granted Zone > DNS > Edit on it"}, st.Problems)
	require.Equal(t, planner.StateActive, route(st, "www.example.com").State, "the account is not frozen")
	require.Equal(t, leftOutReason, route(st, "www.example.org").Reason)
	require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules())
}

func TestAZoneOneCredentialLeavesOutIsServedByTheOther(t *testing.T) {
	e := newEnv(t)
	e.cf.AddZone("zone2", "example.org", testAccount)
	e.useAPI(testToken, unreadable{API: e.cf, zones: []string{"zone2"}})
	second := newZoneView(e.cf)
	second.hide(testZone, true)
	e.addSecondCredential("second-token", second)
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com www.example.org -> :8080")))
	e.enforce()

	st := e.cycle()
	require.True(t, hasProblem(st, "zone example.org is visible through credentials cred1 and cred2 and none of them served it before"),
		"unchecked, the first credential is taken to serve the zone: %v", st.Problems)

	e.check(testCred)
	e.check("cred2")
	e.clock.advance(20 * time.Second)
	st = e.cycle()

	require.Empty(t, st.Problems, "only one credential can serve the zone")
	require.Equal(t, planner.StateActive, route(st, "www.example.org").State)
	require.Equal(t, withSentinel(hostRule("www.example.com"), hostRule("www.example.org")), e.rules())
	require.Len(t, e.cf.RecordsIn("zone2"), 1)
	i := slices.IndexFunc(st.Actions, func(a reconcile.Action) bool { return a.Target == "www.example.org" })
	require.GreaterOrEqual(t, i, 0)
	require.Equal(t, "cred2", st.Actions[i].Credential)
	raw, err := json.Marshal(credentialView(st, "cred2").Report)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"excluded":[]`, "a view shows an empty list, not none")

	e.settings(func(s *store.Settings) { s.ZonePins = map[string]string{"example.org": testCred} })
	e.clock.advance(20 * time.Second)
	st = e.cycle()

	require.True(t, hasProblem(st, "zone example.org is pinned to credential cred1, which can list it but not read its DNS"), "%v", st.Problems)
	org := route(st, "www.example.org")
	require.Equal(t, planner.StateNoZone, org.State, "the pin keeps the other credential from serving it")
	require.Equal(t, leftOutReason, org.Reason)
}

func TestRemovingACredentialThatLeftAZoneOutIsNoRefusal(t *testing.T) {
	e := unreadableEnv(t)
	e.inv.set(snapshot())
	e.check(testCred)

	require.NoError(t, e.eng.RemoveCredential(t.Context(), testCred))

	require.Equal(t, []string{`cred1: credential "main" removed`}, adminEvents(e, time.Time{}))
}
