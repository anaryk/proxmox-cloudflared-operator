package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
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

// The rules of a move are rolled out only once every node of the tunnel
// moved to reaches every route of the owner: a ready connector on a node
// that does not reach the target serves nothing, so the records stay where
// they are until it does.
func TestAMoveRollsOutOnlyToNodesThatReachItsRoutes(t *testing.T) {
	pinned1, _ := planner.PinnedTunnelName(testInstall, "pve1")
	w := servedAlone(t)
	w.res.setUnreachable("pve.example.com", "connection refused")
	one := 1
	now := w.clock.now()
	require.NoError(t, w.store.SaveNode(store.NodeEntry{Name: "pve2", Since: now, State: store.NodeActive, StateSince: now, Index: &one}))
	*w.listed = append(*w.listed, members.Member{Name: "pve2", ID: 2, Online: true})
	w.acts["pve2"] = &agentAct{kind: pki.KindHost, started: now.Add(-time.Hour)}

	w.untilMove(t, model.ManualPrefix+"pve", store.MoveRulesAdded, 10)
	for range 20 {
		w.tick(5 * time.Second)
		m, ok := w.moveOf(t, model.ManualPrefix+"pve")
		require.True(t, ok)
		require.Equal(t, store.MoveRulesAdded, m.Step, "not rolled out while this node does not reach it")
		require.Equal(t, tunnelName, w.recordTunnel(t, "pve.example.com"))
	}

	w.res.mu.Lock()
	delete(w.res.unreachable, "pve.example.com")
	w.res.mu.Unlock()
	w.untilMove(t, model.ManualPrefix+"pve", store.MoveCNAMEsSwitched, 20, "pve.example.com")
	require.Equal(t, pinned1, w.recordTunnel(t, "pve.example.com"))
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

// The records of a move count as switched only where a DNS run listed their
// zone and saw each of them point at the tunnel moved to, or wrote it: a
// zone that could not be listed says nothing of its records, and a move
// that went on without them would drop the rules of the tunnel the records
// still point at.
func TestAMoveWaitsForTheRecordsOfAZoneThatCouldNotBeListed(t *testing.T) {
	pinned2, _ := planner.PinnedTunnelName(testInstall, "pve2")
	w := threeNodes(t)
	w.settle()
	w.acts["pve3"].fail = map[string]string{"db.example.com": "not reached"}
	w.untilMove(t, "qemu/102", store.MoveRolledOut, 40, hosts3...)
	require.Equal(t, tunnelName, w.recordTunnel(t, "db.example.com"))

	w.cf.FailNext("dns.read", 2, errors.New("connection reset"))
	for range 2 {
		w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, hosts3...)
		m, ok := w.moveOf(t, "qemu/102")
		require.True(t, ok)
		require.Equal(t, store.MoveRolledOut, m.Step, "nothing is known of the records of a zone that was not listed")
		require.Equal(t, tunnelName, w.recordTunnel(t, "db.example.com"))
	}

	w.untilMove(t, "qemu/102", store.MoveCNAMEsSwitched, 10, hosts3...)
	require.Equal(t, pinned2, w.recordTunnel(t, "db.example.com"))
}

// A move whose owner is wanted at a third tunnel once its rules are rolled
// out goes on to its end: its records may point at the tunnel it goes to
// already, and a move turned around toward a tunnel it never had the rules
// in would be turned around again in the next cycle, and the next. The move
// to where it is wanted starts once it is done.
func TestAMoveWantedAtAThirdTunnelGoesOnToItsEnd(t *testing.T) {
	pinned2, _ := planner.PinnedTunnelName(testInstall, "pve2")
	pinned3, _ := planner.PinnedTunnelName(testInstall, "pve3")
	w := threeNodes(t)
	w.settle()
	w.acts["pve3"].fail = map[string]string{"db.example.com": "not reached"}
	w.untilMove(t, "qemu/102", store.MoveRolledOut, 40, hosts3...)
	require.Equal(t, tunnelName, w.recordTunnel(t, "db.example.com"))

	// The records cannot be written for a while; the placement is stored at
	// the tunnel moved to meanwhile. Then db migrates to pve3, which reaches
	// it now: it is wanted in the tunnel pinned there.
	w.cf.Deny("dns.write")
	w.tick(5 * time.Second)
	before, ok := w.moveOf(t, "qemu/102")
	require.True(t, ok)
	require.Equal(t, store.MoveRolledOut, before.Step)
	snap := w.inv.snapshot()
	snap.Guests[1] = carded(guest(102, "db", "db.example.com -> :8080"), "pve3", dbCard)
	w.inv.set(snap)
	w.acts["pve3"].fail = nil
	for range 2 {
		w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, hosts3...)
		m, ok := w.moveOf(t, "qemu/102")
		require.True(t, ok)
		require.Equal(t, before, m, "the move is neither turned around nor written again")
	}
	w.cf.Allow("dns.write")

	w.untilMove(t, "qemu/102", store.MoveCNAMEsSwitched, 10, hosts3...)
	require.Equal(t, pinned2, w.recordTunnel(t, "db.example.com"))
	for range 80 {
		w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, hosts3...)
		if m, ok := w.moveOf(t, "qemu/102"); ok && m.From == pinned2 {
			break
		}
	}
	m, ok := w.moveOf(t, "qemu/102")
	require.True(t, ok, "the move to where it is wanted follows")
	require.Equal(t, pinned2, m.From)
	require.Equal(t, pinned3, m.To)
	w.untilMove(t, "qemu/102", store.MoveCNAMEsSwitched, 40, hosts3...)
	require.Equal(t, pinned3, w.recordTunnel(t, "db.example.com"))
	for _, ev := range w.placementEvents() {
		require.NotContains(t, ev, "given up", ev)
	}
}

// A record pco will not change, as one of someone else at a hostname of
// the owner, never points at the tunnel moved to: the move does not wait
// for it, and goes on once the records pco writes were switched.
func TestAMoveDoesNotWaitForARecordInConflict(t *testing.T) {
	pinned1, _ := planner.PinnedTunnelName(testInstall, "pve1")
	w := twoNames(t)
	for _, r := range w.records() {
		if r.Name == "web2.example.com" {
			r.Type, r.Content, r.Comment = "A", "192.0.2.10", ""
			w.cf.SeedRecord(testZone, r)
		}
	}
	w.acts["pve3"].fail = map[string]string{"www.example.com": "not reached"}
	w.untilMove(t, "qemu/101", store.MoveRolledOut, 40, "www.example.com")
	st := w.tick(5 * time.Second)
	require.True(t, slices.ContainsFunc(st.Conflicts, func(c reconcile.Conflict) bool { return c.Name == "web2.example.com" }), st.Conflicts)

	w.untilMove(t, "qemu/101", store.MoveCNAMEsSwitched, 10, "www.example.com")
	require.Equal(t, pinned1, w.recordTunnel(t, "www.example.com"))
}

// A move that waits at its rollout for a record says so once it has waited
// long, naming the move and the hostname.
func TestAMoveThatWaitsLongForARecordSaysSo(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.acts["pve3"].fail = map[string]string{"db.example.com": "not reached"}
	w.untilMove(t, "qemu/102", store.MoveRolledOut, 40, hosts3...)

	w.cf.FailNext("dns.read", 1000, errors.New("connection reset"))
	st := w.tick(5 * time.Second)
	require.False(t, hasProblem(st, "the move of qemu/102"), st.Problems)
	for range 10 {
		st = w.tick(time.Minute)
	}
	m, _ := w.moveOf(t, "qemu/102")
	require.Equal(t, store.MoveRolledOut, m.Step)
	require.True(t, hasProblem(st, "the move of qemu/102 from "+tunnelName), st.Problems)
	require.True(t, hasProblem(st, "db.example.com"), st.Problems)
}

// A record whose marker is lost may still point at the tunnel a move leaves,
// so it is waited for even while its adoption waits; a record in conflict or
// waiting for adoption alone is not pco's to switch, whatever its case.
func TestARecordWithItsMarkerLostHoldsAMove(t *testing.T) {
	c := &cycleRun{adoptWaits: map[string]string{"www.example.com": "the tunnel is not ready"}}
	require.True(t, c.notSwitched("WWW.example.com"))

	c.st.Lost = []string{"www.example.com"}
	require.False(t, c.notSwitched("www.example.com"))

	c = &cycleRun{}
	c.st.Conflicts = []reconcile.Conflict{{Name: "web2.example.com"}}
	require.True(t, c.notSwitched("web2.example.com"))
	c.st.Lost = []string{"web2.example.com"}
	require.False(t, c.notSwitched("web2.example.com"))
}
