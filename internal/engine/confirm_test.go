package engine

import (
	"os"
	"path/filepath"
	"slices"
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

// A confirmation that cannot be kept changes nothing, and that includes the
// mode: the daemon stays in observe-only mode.
func TestAConfirmationThatCannotBeKeptLeavesTheModeAsItWas(t *testing.T) {
	e := newEnv(t)
	e.inv.set(snapshot(many(10)...))
	e.cycle()
	e.inv.set(snapshot(many(10)[6:]...))
	e.clock.advance(20 * time.Second)
	shown := e.cycle()
	require.Equal(t, "observe", shown.Mode)
	require.NotEmpty(t, shown.Offer)
	require.NoError(t, os.Remove(e.memoryFile()))
	require.NoError(t, os.Mkdir(e.memoryFile(), 0o700))
	before, since := e.files(), e.clock.now()

	_, err := e.eng.Apply(t.Context(), true, shown.Offer)

	require.ErrorContains(t, err, "keeping the confirmation")
	s, err := e.store.Settings()
	require.NoError(t, err)
	require.True(t, s.ObserveOnly, "still observe-only")
	require.Equal(t, before, e.files())
	require.Empty(t, e.eng.gone)
	require.Empty(t, adminEvents(e, since.Add(-time.Nanosecond)))
	require.Equal(t, shown.Offer, e.eng.State().Offer)
}

// When the mode cannot be left after the confirmation was kept, the memory is
// put back as it was: the call changes nothing.
func TestAModeThatCannotBeLeftTakesTheKeptConfirmationBack(t *testing.T) {
	skipAsRoot(t)
	e := newEnv(t)
	e.inv.set(snapshot(many(10)...))
	e.cycle()
	e.inv.set(snapshot(many(10)[6:]...))
	e.clock.advance(20 * time.Second)
	shown := e.cycle()
	memory, err := e.store.EngineMemory()
	require.NoError(t, err)
	meta := filepath.Join(e.paths.Cluster, "meta")
	require.NoError(t, os.Chmod(meta, 0o500))
	t.Cleanup(func() { _ = os.Chmod(meta, 0o700) })

	_, err = e.eng.Apply(t.Context(), true, shown.Offer)

	require.ErrorContains(t, err, "leaving observe-only mode")
	after, err := e.store.EngineMemory()
	require.NoError(t, err)
	require.Equal(t, memory, after)
	require.Empty(t, after.GoneGuests)
	require.Empty(t, e.eng.gone)
	require.Equal(t, shown.Offer, e.eng.State().Offer, "the offer still stands")
}

// Once confirmed, the problem lines that asked for the confirmation go with
// what waited; the others stay until the next cycle.
func TestAConfirmationTakesTheProblemsThatAskedForItAlong(t *testing.T) {
	e, _ := everythingWaits(t)
	// A tunnel of this install in an account without a zone is a problem
	// that asks for nothing. The accounts are listed again at once.
	e.cf.AddAccount("acc5", "Fifth")
	e.cf.SeedTunnel("acc5", tunnelName, nil)
	e.eng.zones.due = true
	e.clock.advance(61 * time.Second)
	shown := e.cycle()
	asked := []string{
		"mass delete guard: 6 of 6 records are being removed; confirm to proceed",
		"zone example.info is no longer listed by credential cred1",
		"is not visible through any credential",
	}
	for _, part := range asked {
		require.True(t, hasProblem(shown, part), part)
	}
	var others []string
	for _, p := range shown.Problems {
		if !slices.ContainsFunc(asked, func(part string) bool { return strings.Contains(p, part) }) {
			others = append(others, p)
		}
	}
	require.NotEmpty(t, others, "the state shows other problems too")

	e.apply(true)

	st := e.eng.State()
	require.Empty(t, st.Waiting)
	require.Equal(t, others, st.Problems)
}
