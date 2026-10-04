package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	refusedOnce = "account frozen: the last check of credential cred1 was refused the DNS of zone example.org, which it serves"
	refusedLost = "credential cred1 can no longer read the DNS of zone example.org, which it serves: grant it Zone > DNS > Edit there; " +
		"account acc1 is left as it is until a check finds it readable again or pco apply --confirm-deletes lets the zone go"
	refusedWarning = `the check of credential "main" was refused the DNS of zone example.org, which it serves; ` +
		"account acc1 is left as it is, and the token is checked again in 15m0s"
)

// servingTwoZones is an engine in enforce mode that serves www.example.com and
// www.example.org, in two zones of one account, through one credential that
// view wraps; its token was checked once.
func servingTwoZones(t *testing.T) (*env, zoneView) {
	t.Helper()
	e := newEnv(t)
	e.cf.AddZone("zone2", "example.org", testAccount)
	view := newZoneView(e.cf)
	e.useAPI(testToken, view)
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com www.example.org -> :8080")))
	e.enforce()
	e.cycle()
	e.eng.recheck(t.Context())
	e.clock.advance(20 * time.Second)
	st := e.cycle()
	require.Empty(t, st.Problems)
	requireBothServed(t, e)
	return e, view
}

// requireBothServed checks that the tunnel and the records still serve both
// names.
func requireBothServed(t *testing.T, e *env) {
	t.Helper()
	require.Equal(t, withSentinel(hostRule("www.example.com"), hostRule("www.example.org")), e.rules())
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
	org := e.cf.RecordsIn("zone2")
	require.Len(t, org, 1)
	require.Equal(t, "www.example.org", org[0].Name)
}

// refuse makes Cloudflare refuse the DNS of example.org to the token, which
// the daily check finds.
func (e *env) refuse() {
	e.t.Helper()
	e.cf.Deny("dns.read", "zone2")
	e.clock.advance(recheckEvery)
	e.eng.recheck(e.t.Context())
}

// Reproduced: rules 4 -> 3, the record left unmanaged, no problem line.
func TestAServedZoneWhoseDNSIsRefusedFreezesItsAccount(t *testing.T) {
	e, _ := servingTwoZones(t)
	writes := e.writes()

	e.refuse()
	st := e.cycle()

	require.Empty(t, st.Problems, "one refusal is a warning")
	require.Empty(t, st.Hold)
	for _, host := range []string{"www.example.com", "www.example.org"} {
		r := route(st, host)
		require.Equal(t, RouteFrozen, r.State, host)
		require.Equal(t, refusedOnce, r.Reason, host)
	}
	events := credentialEvents(e)
	require.Len(t, events, 1)
	require.Equal(t, levelWarn, events[0].Level)
	require.Equal(t, refusedWarning, events[0].Message)
	require.Equal(t, writes, e.writes())
	requireBothServed(t, e)

	e.clock.advance(recheckFailedEvery)
	e.eng.recheck(t.Context())
	st = e.cycle()

	require.Equal(t, []string{refusedLost}, st.Problems)
	require.Equal(t, "account frozen: credential cred1 can no longer read the DNS of zone example.org, which it serves: "+
		"grant it Zone > DNS > Edit there", route(st, "www.example.org").Reason)
	require.Len(t, credentialEvents(e), 1, "the second refusal is the problem line")
	require.Equal(t, writes, e.writes())
	requireBothServed(t, e)

	e.cf.Allow("dns.read", "zone2")
	e.clock.advance(recheckFailedEvery)
	e.eng.recheck(t.Context())
	st = e.cycle()

	require.Empty(t, st.Problems, "readable again, the zone is served again")
	require.Empty(t, st.Waiting)
	require.Equal(t, planner.StateActive, route(st, "www.example.org").State)
	require.Equal(t, writes, e.writes())
	requireBothServed(t, e)
}

func TestASingleRefusalOfAServedZoneChangesNothing(t *testing.T) {
	e, _ := servingTwoZones(t)
	writes := e.writes()

	e.refuse()
	e.cf.Allow("dns.read", "zone2")
	st := e.cycle()
	require.Equal(t, RouteFrozen, route(st, "www.example.org").State)
	require.Empty(t, st.Waiting, "a first refusal is not offered to be let go")

	e.clock.advance(recheckFailedEvery)
	e.eng.recheck(t.Context())
	st = e.cycle()

	require.Empty(t, st.Problems)
	require.Equal(t, planner.StateActive, route(st, "www.example.com").State)
	require.Equal(t, planner.StateActive, route(st, "www.example.org").State)
	require.Len(t, credentialEvents(e), 1, "the warning is all there is")
	require.Equal(t, writes, e.writes())
	requireBothServed(t, e)
}

func TestAPinDoesNotLetAServedZoneWhoseDNSIsRefusedGo(t *testing.T) {
	e, _ := servingTwoZones(t)
	e.settings(func(s *store.Settings) { s.ZonePins = map[string]string{"example.org": testCred} })
	writes := e.writes()

	e.refuse()
	st := e.cycle()

	require.Equal(t, refusedOnce, route(st, "www.example.org").Reason)
	require.Equal(t, writes, e.writes())
	requireBothServed(t, e)
}

func TestAServedZoneLeftOutThatLeavesItsListingIsStale(t *testing.T) {
	e, view := servingTwoZones(t)
	writes := e.writes()
	e.refuse()

	view.hide("zone2", true)
	e.clock.advance(zoneRefreshEvery)
	st := e.cycle()

	require.Contains(t, st.Problems, "zone example.org is no longer listed by credential cred1; account acc1 is left as it is "+
		"until the zone is listed again or pco apply --confirm-deletes confirms it is gone")
	require.Equal(t, writes, e.writes())
	requireBothServed(t, e)
}

func TestARestartKeepsAServedZoneWhoseDNSIsRefusedFrozen(t *testing.T) {
	e, _ := servingTwoZones(t)
	writes := e.writes()
	e.refuse()
	e.clock.advance(recheckFailedEvery)
	e.eng.recheck(t.Context())
	require.Equal(t, []string{refusedLost}, e.cycle().Problems)

	e.restart()
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Equal(t, refusedOnce, route(st, "www.example.org").Reason, "the memory says the zone was served")
	require.Equal(t, writes, e.writes())
	requireBothServed(t, e)

	e.eng.recheck(t.Context())
	st = e.cycle()

	require.Equal(t, []string{refusedLost}, st.Problems, "the report before the restart counts")
	require.Empty(t, credentialEvents(e), "the restart warns of nothing new")
	require.Equal(t, writes, e.writes())
	requireBothServed(t, e)
}

// letGo confirms what the state offers for a zone whose DNS two checks in a
// row were refused, and restarts the daemon first when restart is set.
func letGo(t *testing.T, restart bool) *env {
	t.Helper()
	e, _ := servingTwoZones(t)
	e.refuse()
	e.clock.advance(recheckFailedEvery)
	e.eng.recheck(t.Context())
	st := e.cycle()
	require.Equal(t, []Waiting{{
		Kind: WaitingZone, Subject: "example.org",
		Detail: "credential cred1 can no longer read the DNS of zone example.org; a confirmation lets the zone go: " +
			"its hostnames are taken off the tunnel, and its records are left as they are",
		Items: []string{},
	}}, st.Waiting)

	e.apply(true)
	require.Contains(t, adminEvents(e, time.Time{}), "example.org: the zone whose DNS its credential can no longer read is let go")
	if restart {
		e.restart()
	}
	e.clock.advance(20 * time.Second)
	st = e.cycle()

	require.Empty(t, st.Problems)
	org := route(st, "www.example.org")
	require.Equal(t, planner.StateNoZone, org.State)
	require.Equal(t, leftOutReason, org.Reason)
	require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules())
	require.Len(t, e.cf.RecordsIn("zone2"), 1, "pco cannot read the records there, so it leaves them")
	return e
}

func TestAConfirmationOutlivesARestartBeforeTheNextCycle(t *testing.T) {
	letGo(t, true)
}

func TestAConfirmationLetsGoOfAZoneWhoseDNSIsRefused(t *testing.T) {
	e := letGo(t, false)

	e.eng.recheck(t.Context())
	e.clock.advance(recheckFailedEvery)
	e.eng.recheck(t.Context())
	require.Empty(t, e.cycle().Problems, "let go, the zone is left out like any other")
}

// The first check of a credential has none before it that showed the DNS of
// a zone readable: pco cannot have managed the records of a zone it refuses.
func TestTheFirstCheckOfACredentialLeavesOutWhatItCannotRead(t *testing.T) {
	e := unreadableEnv(t)
	st := e.cycle()
	require.True(t, hasProblem(st, "zone example.org: listing the records"), "unchecked, the zone is tried: %v", st.Problems)

	e.eng.recheck(t.Context())
	st = e.cycle()
	require.Empty(t, st.Problems)
	e.clock.advance(recheckFailedEvery)
	e.eng.recheck(t.Context())
	st = e.cycle()

	require.Empty(t, st.Problems)
	require.Empty(t, credentialEvents(e))
	require.Equal(t, planner.StateActive, route(st, "www.example.com").State)
	require.Equal(t, leftOutReason, route(st, "www.example.org").Reason)
	require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules())
}

// Reproduced: a 403 problem line on three cycles, a rule published with no
// record, the route shown active.
func TestAZoneListedSinceTheLastCheckWaitsForTheNextOne(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	e.check(testCred)
	e.cf.AddZone("zone2", "example.org", testAccount)
	e.cf.Deny("dns.read", "zone2")
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com www.example.org -> :8080")))
	writes := e.writes()

	e.clock.advance(zoneRefreshEvery)
	st := e.cycle()

	require.Equal(t, []string{"zone example.org is listed by credential cred1, whose last check did not look at it; " +
		"account acc1 is left as it is until the next check of the credential, due now, does"}, st.Problems)
	require.Equal(t, writes, e.writes(), "no rule is published for it")
	require.True(t, e.eng.recheckDue(testCred), "the check is due at once")

	e.eng.recheck(t.Context())
	st = e.cycle()

	require.Empty(t, st.Problems)
	org := route(st, "www.example.org")
	require.Equal(t, planner.StateNoZone, org.State)
	require.Equal(t, leftOutReason, org.Reason)
	require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules())
}

func TestACredentialThatLeavesAZoneOutIsCheckedEveryQuarterHour(t *testing.T) {
	e := unreadableEnv(t)
	e.check(testCred)
	n := verifies(e.cf)

	e.clock.advance(recheckFailedEvery)
	e.eng.recheck(t.Context())

	require.Equal(t, n+1, verifies(e.cf), "a grant is picked up within a quarter of an hour")
}

func TestANameUnderAZoneLeftOutIsNotPlacedInItsParent(t *testing.T) {
	e := newEnv(t)
	e.cf.AddZone("zone2", "app.example.com", testAccount)
	e.cf.Deny("dns.read", "zone2")
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com www.app.example.com -> :8080")))
	e.enforce()
	e.check(testCred)

	st := e.cycle()

	require.Empty(t, st.Problems)
	r := route(st, "www.app.example.com")
	require.Equal(t, planner.StateNoZone, r.State)
	require.Equal(t, "credential main can list app.example.com but not read its DNS: grant Zone > DNS > Edit to serve it; "+
		"pco credential check cred1 picks the grant up at once", r.Reason)
	require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules())
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
}
