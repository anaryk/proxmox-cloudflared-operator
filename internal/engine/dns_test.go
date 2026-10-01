package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

// retired publishes x and y on guest 101, then takes them from it, so that
// both records are due for deletion once the claims and then the records
// have waited out their grace.
func retired(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot(guest(101, "web-1", "x.example.com y.example.com -> :8080")))
	e.cycle()
	require.Equal(t, []string{"x.example.com", "y.example.com"}, e.recordNames())

	e.inv.set(snapshot(untagged(guest(101, "web-1"))))
	e.clock.advance(20 * time.Second)
	e.cycle()
	e.clock.advance(61 * time.Second)
	st := e.cycle()
	require.Contains(t, actionKinds(st), "delete-record x.example.com held: grace period: 1m0s left")
	e.clock.advance(61 * time.Second)
	return e
}

func TestStillUnwantedKeepsANameAnotherGuestPublishes(t *testing.T) {
	e := retired(t)
	// The cycle sees no route for x; the look right before the deletes finds
	// another guest that publishes it now.
	idle := untagged(guest(101, "web-1"))
	e.inv.enqueue(snapshot(idle), snapshot(idle, guest(102, "web-2", "x.example.com -> :8080")))
	refreshes := e.inv.refreshes()

	e.cycle()

	require.Equal(t, []string{"x.example.com"}, e.recordNames(), "x is published again, y is gone")
	require.Equal(t, refreshes+2, e.inv.refreshes(), "the second look is taken once per cycle, not once per delete")
}

func TestStillUnwantedCountsAHostnameAGuestStillNames(t *testing.T) {
	e := retired(t)
	// Another guest names x in its Notes, outside a route: a held name.
	named := guest(102, "web-2")
	named.Description = "Takes over from x.example.com next week."
	idle := untagged(guest(101, "web-1"))
	e.inv.enqueue(snapshot(idle), snapshot(idle, named))

	e.cycle()

	require.Equal(t, []string{"x.example.com"}, e.recordNames(), "a held name is still wanted")
}

func TestStillUnwantedHoldsTheDeletesWhenTheSecondLookListsNoGuest(t *testing.T) {
	e := retired(t)
	e.inv.enqueue(snapshot(untagged(guest(101, "web-1"))), snapshot())

	st := e.cycle()

	require.Equal(t, []string{"x.example.com", "y.example.com"}, e.recordNames())
	require.True(t, hasProblem(st, "asking the inventory before deleting: the inventory lists no guest any more"))
}

func TestStillUnwantedHoldsTheDeletesWhenTheSecondLookIsIncomplete(t *testing.T) {
	e := retired(t)
	idle := untagged(guest(101, "web-1"))
	e.inv.enqueue(snapshot(idle), incomplete("node pve1: network not refreshed: timeout", idle))

	st := e.cycle()

	require.Equal(t, []string{"x.example.com", "y.example.com"}, e.recordNames())
	require.Contains(t, strings.Join(st.Problems, "\n"), "asking the inventory before deleting: the inventory is incomplete")

	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Empty(t, e.records(), "the grace was kept, so the deletes go ahead once the inventory answers")
}

func TestAdoptWaitsForAnEnforcingRun(t *testing.T) {
	e := newEnv(t)
	// The tunnel is there, so that observing finds the record in the way.
	e.cf.SeedTunnel(testAccount, tunnelName, nil)
	e.cf.SeedRecord(testZone, cfapi.Record{Type: "A", Name: "www.example.com", Content: "192.0.2.10", Comment: "by hand"})
	st := e.cycle()
	require.Len(t, st.Conflicts, 1)
	require.NoError(t, e.eng.Adopt(t.Context(), "www.example.com"))

	e.clock.advance(10 * time.Second)
	e.cycle()

	require.Contains(t, e.eng.adopt, "www.example.com", "an observing run does not use the adoption up")
	require.Equal(t, "A", e.records()[0].Type)
	require.Equal(t, 1, eventsContaining(e, "adoption of www.example.com waits: pco is in observe-only mode"))

	e.apply(false)
	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Equal(t, "CNAME", e.records()[0].Type)
	require.Empty(t, e.eng.adopt)
}

func TestConfirmedDeletesLiftTheGuardOnce(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	names := []string{"a", "b", "c", "d", "e", "f"}
	hosts := make([]string, len(names))
	for i, n := range names {
		hosts[i] = n + ".example.com"
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

	require.Len(t, e.records(), 6, "six of six records at once trip the guard")
	require.Contains(t, actionKinds(st), "delete-record a.example.com held: mass delete guard: 6 of 6 records are being removed; confirm to proceed")

	e.apply(true)
	e.clock.advance(10 * time.Second)
	e.cycle()

	require.Empty(t, e.records())
	require.Nil(t, e.eng.confirm)
}
