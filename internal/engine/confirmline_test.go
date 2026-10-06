package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
)

// AsksForConfirmation tells exactly the problem lines a confirmation of the
// last state withdraws, for every kind of what waits: a client that has no
// offer of its own, as a fake of the daemon, withdraws the same lines by it.
func TestAsksForConfirmationTellsTheLinesAConfirmationWithdraws(t *testing.T) {
	check := func(t *testing.T, e *env, st State) {
		t.Helper()
		lines := e.eng.offered.what.lines
		require.NotEmpty(t, lines)
		for _, line := range lines {
			require.True(t, AsksForConfirmation(line), line)
			require.Contains(t, st.Problems, line)
		}
		for _, p := range st.Problems {
			require.Equal(t, slices.Contains(lines, p), AsksForConfirmation(p), p)
		}
	}
	t.Run("the mass delete guard", func(t *testing.T) {
		e, st := twoHeld(t)
		check(t, e, st)
	})
	t.Run("vanished guests", func(t *testing.T) {
		e := publishedMany(t, 10)
		e.inv.set(snapshot(many(10)[6:]...))
		e.clock.advance(20 * time.Second)
		check(t, e, e.cycle())
	})
	t.Run("a zone that left its listing", func(t *testing.T) {
		e, view, _ := twoZones(t)
		view.hide("zone2", true)
		e.clock.advance(zoneRefreshEvery)
		check(t, e, e.cycle())
	})
	t.Run("a served zone whose DNS is refused twice", func(t *testing.T) {
		e, _ := servingTwoZones(t)
		e.refuse()
		e.cycle()
		e.clock.advance(recheckFailedEvery)
		e.eng.recheck(t.Context())
		check(t, e, e.cycle())
	})
	t.Run("a tunnel no credential sees", func(t *testing.T) {
		e := newEnv(t)
		other := cffake.New()
		other.AddAccount("acc2", "Other")
		other.AddZone("zone2", "example.org", "acc2")
		e.addSecondCredential("other-token", other)
		e.enforce()
		e.inv.set(snapshot(many(10)...))
		e.cycle()
		require.NoError(t, e.store.DeleteCredential(testCred))
		e.clock.advance(20 * time.Second)
		check(t, e, e.cycle())
	})
}
