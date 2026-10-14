package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// moveOf is the move stored for an owner; false when none is.
func (w *world) moveOf(t *testing.T, owner string) (store.Move, bool) {
	t.Helper()
	moves, err := w.store.Moves()
	require.NoError(t, err)
	m, ok := moves[owner]
	return m, ok
}

// untilMove ticks until the move of owner is stored at step, with nothing
// withdrawn meanwhile, and fails when it does not get there in n ticks.
func (w *world) untilMove(t *testing.T, owner, step string, n int, hosts ...string) {
	t.Helper()
	for range n {
		w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, hosts...)
		if m, ok := w.moveOf(t, owner); ok && m.Step == step {
			return
		}
	}
	m, _ := w.moveOf(t, owner)
	t.Fatalf("the move of %s did not reach %s: it is at %q", owner, step, m.Step)
}

// stepBack rewrites the stored move of owner to a step before the one it
// is at, as a controller leaves it when it dies after the work of a step and
// before it noted the step.
func (w *world) stepBack(t *testing.T, owner, step string) {
	t.Helper()
	m, ok := w.moveOf(t, owner)
	require.True(t, ok)
	m.Step, m.CNAMEAt = step, time.Time{}
	require.NoError(t, w.store.SaveMove(m))
}

// twoNames gives the guest on pve1 a second hostname, so that the records
// of its routes can be switched in part.
func twoNames(t *testing.T) *world {
	t.Helper()
	w := threeNodes(t)
	snap := w.inv.snapshot()
	snap.Guests[0] = carded(guest(101, "web", "www.example.com -> :8080", "web2.example.com -> :8080"), "pve1", webCard)
	w.inv.set(snap)
	w.settle()
	w.settle()
	return w
}

// A new controller that finds a move whose rules were rolled out rolls it
// forward: it cannot tell whether the last controller switched the records
// before it died, and only a DNS run of its own does. Nothing is withdrawn
// at any point, and no record goes back.
func TestANewControllerRollsAMoveForwardOnceItsRulesAreRolledOut(t *testing.T) {
	pinned1, _ := planner.PinnedTunnelName(testInstall, "pve1")
	start := func(t *testing.T) *world {
		w := threeNodes(t)
		w.settle()
		w.acts["pve3"].fail = map[string]string{"www.example.com": "not reached"}
		return w
	}
	check := func(t *testing.T, w *world) {
		t.Helper()
		w.eng = w.newEngine()
		w.watchHere(t)
		for range 12 {
			w.tick(5 * time.Second)
			w.requireNothingWithdrawn(t, hosts3...)
		}
		require.Equal(t, pinned1, w.recordTunnel(t, "www.example.com"))
		m, ok := w.moveOf(t, "qemu/101")
		require.True(t, ok, "the move goes on to its end")
		require.Equal(t, store.MoveCNAMEsSwitched, m.Step)
		require.Equal(t, tunnelName, m.From)
		for _, ev := range w.placementEvents() {
			require.NotContains(t, ev, "given up", ev)
		}
	}

	t.Run("before the switch", func(t *testing.T) {
		// The new controller's timers start over: it sees no divergence
		// yet, turns the move around with its records where they are, and
		// pins the route again once it has seen the divergence for long.
		w := start(t)
		w.untilMove(t, "qemu/101", store.MoveRolledOut, 40, hosts3...)
		require.Equal(t, tunnelName, w.recordTunnel(t, "www.example.com"))
		w.eng = w.newEngine()
		w.watchHere(t)
		for range 90 {
			w.tick(5 * time.Second)
			w.requireNothingWithdrawn(t, hosts3...)
		}
		require.Equal(t, pinned1, w.recordTunnel(t, "www.example.com"))
	})
	t.Run("after the switch, before it was noted", func(t *testing.T) {
		w := start(t)
		w.untilMove(t, "qemu/101", store.MoveCNAMEsSwitched, 60, hosts3...)
		require.Equal(t, pinned1, w.recordTunnel(t, "www.example.com"))
		w.stepBack(t, "qemu/101", store.MoveRolledOut)
		writes := len(w.writes())
		check(t, w)
		for _, call := range w.writes()[writes:] {
			require.False(t, strings.HasPrefix(call, "UpdateRecord"), "no record is switched back: %s", call)
		}
	})
	t.Run("with the records switched in part", func(t *testing.T) {
		w := twoNames(t)
		w.requireNothingWithdrawn(t, "www.example.com", "web2.example.com")
		w.acts["pve3"].fail = map[string]string{"www.example.com": "not reached"}
		w.untilMove(t, "qemu/101", store.MoveRolledOut, 40, "www.example.com", "web2.example.com")
		w.cf.FailNext("dns.write", 1, errors.New("gone away"))
		w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, "www.example.com", "web2.example.com")
		tunnels := []string{w.recordTunnel(t, "www.example.com"), w.recordTunnel(t, "web2.example.com")}
		slices.Sort(tunnels)
		require.Equal(t, []string{tunnelName, pinned1}, tunnels, "one record switched, one did not")
		m, ok := w.moveOf(t, "qemu/101")
		require.True(t, ok)
		require.Equal(t, store.MoveRolledOut, m.Step)

		// The record that failed fails once more for the new controller:
		// the rule of the tunnel it still points at must stay meanwhile.
		w.eng = w.newEngine()
		w.watchHere(t)
		w.cf.FailNext("dns.write", 1, errors.New("gone away"))
		for range 12 {
			w.tick(5 * time.Second)
			w.requireNothingWithdrawn(t, "www.example.com", "web2.example.com")
		}
		require.Equal(t, pinned1, w.recordTunnel(t, "www.example.com"))
		require.Equal(t, pinned1, w.recordTunnel(t, "web2.example.com"))
	})
}

// A move given up once its rules were rolled out does not drop them: its
// records may point at the tunnel it went to, so the rules stay in both
// tunnels, the records are switched back, and the rules of the tunnel left
// go when the old ones of any move would.
func TestAMoveGivenUpAfterItsRolloutKeepsBothRulesUntilTheRecordsAreBack(t *testing.T) {
	w, pinned2 := pinnedDB(t)
	w.acts["pve3"].fail = nil
	for range 125 {
		w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, hosts3...)
	}
	w.untilMove(t, "qemu/102", store.MoveCNAMEsSwitched, 40, hosts3...)
	require.Equal(t, tunnelName, w.recordTunnel(t, "db.example.com"))
	w.stepBack(t, "qemu/102", store.MoveRolledOut)

	w.acts["pve3"].noWatch = true
	sawBoth := false
	for range 90 {
		st := w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, hosts3...)
		if w.recordTunnel(t, "db.example.com") == pinned2 && slices.Contains(w.servedIn(t, "db.example.com"), tunnelName) {
			sawBoth = true
		}
		_ = st
	}
	require.True(t, sawBoth, "the rule of the shared tunnel stayed while the record went back")
	require.Equal(t, pinned2, w.recordTunnel(t, "db.example.com"))
	require.Equal(t, []string{pinned2}, w.servedIn(t, "db.example.com"))
	_, ok := w.moveOf(t, "qemu/102")
	require.False(t, ok, "the move is done")
	require.True(t, slices.ContainsFunc(w.placementEvents(), func(ev string) bool { return strings.Contains(ev, "given up") }), w.placementEvents())
}
