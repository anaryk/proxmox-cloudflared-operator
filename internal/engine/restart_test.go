package engine

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// restart replaces the engine with a new one on the same store, as a
// restarted daemon does.
func (e *env) restart() {
	e.t.Helper()
	e.eng = e.newEngine()
}

func (e *env) memoryFile() string { return filepath.Join(e.paths.Local, "meta", "engine-memory.json") }

// Reproduced: cred1 serves acc1 and cred2 acc2; cred1 is removed, and the
// daemon restarts.
func TestTheTunnelOfARemovedCredentialIsKeptAcrossARestart(t *testing.T) {
	e := newEnv(t)
	other := cffake.New()
	other.AddAccount("acc2", "Other")
	other.AddZone("zone2", "example.org", "acc2")
	e.addSecondCredential("other-token", other)
	e.enforce()
	e.cycle()
	tun := e.tunnels()[0]

	require.NoError(t, e.store.DeleteCredential(testCred))
	e.restart()
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.True(t, hasProblem(st, "tunnel pco-abc123 in account acc1 is not visible through any credential; its connector is kept"))
	keep, pruned := e.conn.lastPrune()
	require.True(t, pruned)
	require.Contains(t, keep, tun.ID)
}

// Reproduced: a zone leaves cred1's listing; removing cred1 must not pass,
// before or after a restart.
func TestRemovingACredentialWhoseZoneLeftItsListingIsRefused(t *testing.T) {
	e, view, tun := servingThrough(t)
	view.hide(testZone, true)
	e.clock.advance(zoneRefreshEvery)
	e.cycle()

	for _, restarted := range []bool{false, true} {
		if restarted {
			e.restart()
			e.clock.advance(20 * time.Second)
			e.cycle()
		}

		err := e.eng.RemoveCredential(t.Context(), testCred)

		require.ErrorIs(t, err, ErrRefused, "restarted: %v", restarted)
		require.Contains(t, err.Error(), "tunnel pco-abc123 in account acc1")
		require.Contains(t, err.Error(), "record www.example.com in zone example.com")
	}
	keep, _ := e.conn.lastPrune()
	require.Contains(t, keep, tun.ID)
	creds, err := e.store.Credentials()
	require.NoError(t, err)
	require.Len(t, creds, 1)
}

// twoZones serves www.example.com and www.example.net, two zones of one
// account, with the first credential reaching Cloudflare through view.
func twoZones(t *testing.T) (*env, zoneView, []planner.IngressRule) {
	t.Helper()
	e := newEnv(t)
	e.cf.AddZone("zone2", "example.net", testAccount)
	view := newZoneView(e.cf)
	e.useAPI(testToken, view)
	e.enforce()
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com www.example.net -> :8080")))
	e.cycle()
	both := withSentinel(hostRule("www.example.com"), hostRule("www.example.net"))
	require.Equal(t, both, e.rules())
	return e, view, both
}

// Reproduced: example.net leaves the listing; after a restart it is still
// stale and its rule stays.
func TestAStaleZoneStaysStaleAcrossARestart(t *testing.T) {
	e, view, both := twoZones(t)
	view.hide("zone2", true)
	e.clock.advance(zoneRefreshEvery)
	e.cycle()

	e.restart()
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.True(t, hasProblem(st, "zone example.net is no longer listed by credential cred1"))
	require.Equal(t, both, e.rules())
}

func TestAZoneThatLeftItsListingWhileTheDaemonWasDownIsStale(t *testing.T) {
	e, view, both := twoZones(t)

	view.hide("zone2", true)
	e.restart()
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.True(t, hasProblem(st, "zone example.net is no longer listed by credential cred1"))
	require.Equal(t, both, e.rules())
}

func TestConfirmedGoneGuestsStayConfirmedAcrossARestart(t *testing.T) {
	e := publishedMany(t, 10)
	e.inv.set(snapshot(many(10)[6:]...))
	e.clock.advance(20 * time.Second)
	e.cycle()
	e.apply(true)

	e.restart()
	e.clock.advance(10 * time.Second)
	st := e.cycle()

	require.False(t, hasProblem(st, "no longer listed by Proxmox"))
	claims, err := e.store.Claims()
	require.NoError(t, err)
	require.NotNil(t, claims["g101.example.com"].MissingSince)
}

func TestAMemoryThatCannotBeReadHoldsCloudflare(t *testing.T) {
	e := published(t)
	writes, prunes := e.writes(), len(e.conn.prunes())
	require.NoError(t, os.WriteFile(e.memoryFile(), []byte("{"), 0o600))
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com api.example.com -> :8080")))

	e.restart()
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.True(t, hasProblem(st, "reading what the engine remembered"))
	require.Equal(t, writes, e.writes(), "it must not read as nothing remembered")
	require.Len(t, e.conn.prunes(), prunes)
	require.Equal(t, planner.StateActive, route(st, "api.example.com").State, "the routes are still shown")

	require.NoError(t, os.Remove(e.memoryFile()))
	e.clock.advance(20 * time.Second)
	st = e.cycle()
	require.False(t, hasProblem(st, "remembered"))
	require.Equal(t, []string{"api.example.com", "www.example.com"}, e.recordNames())
}

// A1: a tunnel in an account the credential sees, though none of its zones
// is there, keeps the credential.
func TestRemovingACredentialWithATunnelInAnAccountWithoutZonesIsRefused(t *testing.T) {
	e := newEnv(t)
	other := cffake.New()
	other.AddAccount("acc2", "Other")
	other.SeedTunnel("acc2", tunnelName, nil)
	e.addSecondCredential("other-token", other)

	err := e.eng.RemoveCredential(t.Context(), "cred2")

	require.ErrorIs(t, err, ErrRefused)
	require.Contains(t, err.Error(), "tunnel pco-abc123 in account acc2")
}

// The stale zones are kept for themselves: here the zones served name
// another credential for the zone, as after a pin, so only the stale entry
// keeps the account frozen.
func TestARememberedStaleZoneIsInDoubtAfterARestart(t *testing.T) {
	e, view, both := twoZones(t)
	m, err := e.store.EngineMemory()
	require.NoError(t, err)
	m.Served = slices.DeleteFunc(m.Served, func(z store.RememberedZone) bool { return z.Name == "example.net" })
	m.Stale = []store.RememberedZone{{ID: "zone2", Name: "example.net", AccountID: testAccount, CredentialID: testCred}}
	require.NoError(t, e.store.SaveEngineMemory(m))
	view.hide("zone2", true)

	e.restart()
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.True(t, hasProblem(st, "zone example.net is no longer listed by credential cred1"))
	require.Equal(t, both, e.rules())
}
