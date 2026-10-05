package engine

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// twoUncheckedCredentials is an engine in enforce mode with two credentials
// on one account, both checked before example.org was listed, and a clock
// at the time of the next listing.
func twoUncheckedCredentials(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.enforce()
	e.addSecondCredential("second-token", nil)
	e.cycle()
	e.check(testCred)
	e.check("cred2")
	e.cf.AddZone("zone2", "example.org", testAccount)
	e.clock.advance(zoneRefreshEvery)
	return e
}

// The problem line names the first of the checks that are due, whichever
// credential it belongs to.
func TestTheLineOfTwoUncheckedCredentialsShowsTheEarlierCheck(t *testing.T) {
	e := twoUncheckedCredentials(t)
	e.cf.FailNext("verify", 100, unavailable)
	e.cycle()
	e.eng.recheck(t.Context())
	e.clock.advance(5 * time.Minute)
	e.check(testCred)

	st := e.cycle()

	require.Contains(t, st.Problems, "zone example.org is listed by credential cred1 and cred2, whose last check did not look at it; "+
		"account acc1 is left as it is, checking again at 12:20", "cred1 is checked at 12:25, cred2 at 12:20")
}

// A pin names the credential whose check is waited for; the other one does
// not matter.
func TestAPinToAnUncheckedCredentialNamesThatOneOnly(t *testing.T) {
	e := twoUncheckedCredentials(t)
	e.settings(func(s *store.Settings) { s.ZonePins = map[string]string{"example.org": "cred2"} })

	st := e.cycle()

	require.Contains(t, st.Problems, "zone example.org is listed by credential cred2, whose last check did not look at it; "+
		"account acc1 is left as it is, checking again at 12:05")
	require.False(t, hasProblem(st, "credential cred1 and cred2"), "%v", st.Problems)
	require.Equal(t, RouteFrozen, route(st, "www.example.com").State)
}

// While the store is held no check is made: the line says so instead of
// promising one for the time of the cycle.
func TestTheCheckOfAZoneNewlyListedWaitsForAHeldStore(t *testing.T) {
	const waits = "zone example.org is listed by credential cred1, whose last check did not look at it; " +
		"account acc1 is left as it is, and the check waits for the store"

	t.Run("the memory cannot be read", func(t *testing.T) {
		e := newEnv(t)
		e.enforce()
		e.cycle()
		e.check(testCred)
		require.NoError(t, os.WriteFile(e.memoryFile(), []byte("{"), 0o600))
		e.restart()
		e.check(testCred)
		e.cf.AddZone("zone2", "example.org", testAccount)
		e.clock.advance(20 * time.Second)

		st := e.cycle()

		require.Contains(t, st.Problems, waits)
		n := verifies(e.cf)
		e.eng.recheck(t.Context())
		require.Equal(t, n, verifies(e.cf), "no check is made while the store is held")
	})
	t.Run("the claims cannot be saved", func(t *testing.T) {
		e := newEnv(t)
		e.enforce()
		e.cycle()
		e.check(testCred)
		e.cf.AddZone("zone2", "example.org", testAccount)
		e.inv.set(snapshot(guest(101, "web-1", "www.example.com api.example.com -> :8080")))
		require.NoError(t, os.MkdirAll(filepath.Join(e.paths.Cluster, "claims", "api.example.com.json"), 0o700))
		e.clock.advance(zoneRefreshEvery)
		e.cycle()

		e.clock.advance(20 * time.Second)
		st := e.cycle()

		require.True(t, hasProblem(st, "saving the claims"), "%v", st.Problems)
		require.Contains(t, st.Problems, waits)
		n := verifies(e.cf)
		e.eng.recheck(t.Context())
		require.Equal(t, n, verifies(e.cf), "no check is made while the store is held")
	})
}

// A zone listed while a check runs is not in its report: the check is older
// than the listing, as the time it started says, and is made again at once.
func TestAZoneListedWhileACheckRunsIsCheckedAgainAtOnce(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	e.check(testCred)
	e.clock.advance(recheckEvery)
	var armed atomic.Bool
	armed.Store(true)
	e.useAPI(testToken, hookedAPI{API: e.cf, before: func(method string) {
		if method == "Records" && armed.CompareAndSwap(true, false) {
			e.cf.AddZone("zone2", "example.org", testAccount)
			e.clock.advance(20 * time.Second)
			e.cycle()
		}
	}})

	e.eng.recheck(t.Context())
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.True(t, hasProblem(st, "whose last check did not look at it"), "%v", st.Problems)
	require.True(t, e.eng.recheckDue(testCred), "the check is due at once")

	e.eng.recheck(t.Context())
	st = e.cycle()

	require.Empty(t, st.Problems)
}
