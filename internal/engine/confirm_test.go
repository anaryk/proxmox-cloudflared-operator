package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
)

// guarded is an engine in enforce mode whose last state shows the mass
// delete guard holding six removals. wrap, when set, wraps the client of the
// first credential.
func guarded(t *testing.T, wrap func(cfapi.API) cfapi.API) *env {
	t.Helper()
	e := newEnv(t)
	if wrap != nil {
		e.useAPI(testToken, wrap(e.cf))
	}
	e.enforce()
	hosts := []string{"a", "b", "c", "d", "e", "f"}
	for i := range hosts {
		hosts[i] += ".example.com"
	}
	e.inv.set(snapshot(guest(101, "web-1", strings.Join(hosts, " ")+" -> :8080")))
	e.cycle()
	require.Len(t, e.records(), 6)

	e.inv.set(snapshot(untagged(guest(101, "web-1"))))
	e.clock.advance(20 * time.Second)
	e.cycle()
	e.clock.advance(61 * time.Second)
	e.cycle()
	e.clock.advance(61 * time.Second)
	st := e.cycle()
	require.Contains(t, actionKinds(st), "delete-record a.example.com held: mass delete guard: 6 of 6 records are being removed; confirm to proceed")
	return e
}

func TestAConfirmationWithoutAGuardHoldingIsNoDNSConfirmation(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()

	e.apply(true)

	require.Nil(t, e.eng.confirm, "the last state showed no removal held by the guard")
	require.Contains(t, adminEvents(e, time.Time{}), ": the last state showed nothing that waits for a confirmation")
}

// Sequence A: a vanish hold hides the line of a tunnel no credential sees;
// confirming the vanished guests must not let go of that tunnel.
func TestAConfirmationOfVanishedGuestsKeepsATunnelNoCredentialSees(t *testing.T) {
	e := newEnv(t)
	other := cffake.New()
	other.AddAccount("acc2", "Other")
	other.AddZone("zone2", "example.org", "acc2")
	e.addSecondCredential("other-token", other)
	e.enforce()
	e.inv.set(snapshot(many(10)...))
	e.cycle()
	tun := e.tunnels()[0]
	require.NoError(t, e.store.DeleteCredential(testCred))
	e.clock.advance(20 * time.Second)
	st := e.cycle()
	require.True(t, hasProblem(st, "is not visible through any credential"))

	e.inv.set(snapshot(many(10)[6:]...))
	e.clock.advance(20 * time.Second)
	st = e.cycle()
	require.True(t, hasProblem(st, "6 of 10 guests that hold a hostname are no longer listed by Proxmox"))
	require.False(t, hasProblem(st, "not visible"), "the hold hides the line of the tunnel")

	e.apply(true)
	e.clock.advance(20 * time.Second)
	st = e.cycle()

	require.False(t, hasProblem(st, "no longer listed by Proxmox"), "the guests were confirmed")
	require.True(t, hasProblem(st, "is not visible through any credential"), "the tunnel was not")
	keep, _ := e.conn.lastPrune()
	require.Contains(t, keep, tun.ID, "its connector stays")
}

// Sequence B: a vanish hold hides the line of a stale zone; confirming the
// vanished guests must not accept the zone as gone.
func TestAConfirmationOfVanishedGuestsKeepsAStaleZone(t *testing.T) {
	e := newEnv(t)
	e.cf.AddZone("zone2", "example.net", testAccount)
	view := newZoneView(e.cf)
	e.useAPI(testToken, view)
	e.enforce()
	e.inv.set(snapshot(append(many(10), guest(201, "net", "www.example.net -> :8080"))...))
	e.cycle()
	require.Contains(t, e.rules(), hostRule("www.example.net"))

	view.hide("zone2", true)
	e.clock.advance(zoneRefreshEvery)
	st := e.cycle()
	require.True(t, hasProblem(st, "zone example.net is no longer listed by credential cred1"))

	e.inv.set(snapshot(append(many(10)[6:], guest(201, "net", "www.example.net -> :8080"))...))
	e.clock.advance(20 * time.Second)
	st = e.cycle()
	require.True(t, hasProblem(st, "6 of 11 guests that hold a hostname are no longer listed by Proxmox"))
	require.False(t, hasProblem(st, "no longer listed by credential"), "the hold hides the line of the zone")

	e.apply(true)
	e.clock.advance(20 * time.Second)
	st = e.cycle()

	require.True(t, hasProblem(st, "zone example.net is no longer listed by credential cred1"), "the zone was not confirmed gone")
	require.Contains(t, e.rules(), hostRule("www.example.net"), "its rule stays")
}

// When the state shows a stale zone, a tunnel no credential sees and the
// mass delete guard together, one confirmation accepts all three.
func TestOneConfirmationAcceptsEverythingTheStateShowed(t *testing.T) {
	e := newEnv(t)
	e.cf.AddAccount("acc3", "Third")
	e.cf.AddZone("zone3", "example.info", "acc3")
	view := newZoneView(e.cf)
	e.useAPI(testToken, view)
	other := cffake.New()
	other.AddAccount("acc2", "Other")
	other.AddZone("zone2", "example.org", "acc2")
	// Each fake numbers its tunnels from one; Cloudflare's ids are unique.
	for range 5 {
		other.SeedTunnel("acc9", "unrelated", nil)
	}
	e.addSecondCredential("other-token", other)
	e.enforce()
	hosts := []string{"a", "b", "c", "d", "e", "f"}
	for i := range hosts {
		hosts[i] += ".example.com"
	}
	gone := guest(101, "web-1", strings.Join(hosts, " ")+" -> :8080")
	org, info := guest(102, "org", "www.example.org -> :8080"), guest(103, "info", "www.example.info -> :8080")
	e.inv.set(snapshot(gone, org, info))
	e.cycle()
	invisible := other.TunnelsIn("acc2")[0]

	// The removals start, the second credential goes and example.info
	// leaves the listing.
	e.inv.set(snapshot(untagged(gone), org, info))
	require.NoError(t, e.store.DeleteCredential("cred2"))
	view.hide("zone3", true)
	e.clock.advance(zoneRefreshEvery)
	e.cycle()
	e.clock.advance(61 * time.Second)
	e.cycle()
	e.clock.advance(61 * time.Second)
	st := e.cycle()
	require.True(t, hasProblem(st, "zone example.info is no longer listed by credential cred1"))
	require.True(t, hasProblem(st, "is not visible through any credential"))
	require.Contains(t, actionKinds(st), "delete-record a.example.com held: mass delete guard: 6 of 6 records are being removed; confirm to proceed")

	e.apply(true)
	e.clock.advance(10 * time.Second)
	st = e.cycle()

	require.False(t, hasProblem(st, "no longer listed by credential"))
	require.False(t, hasProblem(st, "not visible through any credential"))
	require.Empty(t, e.records(), "the removals the guard held went ahead")
	keep, _ := e.conn.lastPrune()
	require.NotContains(t, keep, invisible.ID)
}
