package engine

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// servedAlone is a node alone, as before a cluster, that serves the route
// of a guest and a manual route to its own address through the shared
// tunnel.
func servedAlone(t *testing.T) *world {
	t.Helper()
	e, r, listed := clusterOf(t)
	l := holding(testWriter)
	e.lease = l
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	e.inv.set(inventory.Snapshot{
		Guests:   []model.Guest{carded(guest(101, "web", "www.example.com -> :8080"), "pve1", webCard)},
		Nodes:    []inventory.Node{{Name: "pve1", Addr: nodeAddr, Online: true, Local: true}},
		Complete: true,
	})
	require.NoError(t, e.store.SaveManualRoute(model.Route{
		Hostname: "pve.example.com", ManualID: "pve", Source: model.SourceManual,
		Target:  model.Target{Scheme: model.SchemeHTTPS, Addr: nodeAddr, Port: 8006},
		Options: model.RouteOptions{AllowNode: true, NoTLSVerify: true},
	}))
	e.eng = e.newEngine()
	e.enforce()
	w := &world{env: e, r: r, lease: l, listed: listed, acts: map[string]*agentAct{}}
	w.watchHere(t)
	for range 3 {
		w.tick(time.Second)
	}
	for _, host := range []string{"pve.example.com", "www.example.com"} {
		require.Equal(t, tunnelName, w.recordTunnel(t, host), host)
		require.Equal(t, []string{tunnelName}, w.servedIn(t, host), host)
	}
	return w
}

// A route served through the shared tunnel before a second node registered
// has no placement stored, and goes to the tunnel pinned to its node, as a
// manual route to the node's own address must. It gets there by a move:
// its rule is in the pinned tunnel before its record points there, and
// nothing is withdrawn meanwhile, also when the controller started after
// the route was served.
func TestARouteServedBeforeTheClusterLeavesTheSharedTunnelByAMove(t *testing.T) {
	pinned1, _ := planner.PinnedTunnelName(testInstall, "pve1")
	for _, restart := range []bool{false, true} {
		name := "the controller that served it"
		if restart {
			name = "a controller started since"
		}
		t.Run(name, func(t *testing.T) {
			w := servedAlone(t)
			one := 1
			now := w.clock.now()
			require.NoError(t, w.store.SaveNode(store.NodeEntry{Name: "pve2", Since: now, State: store.NodeActive, StateSince: now, Index: &one}))
			*w.listed = append(*w.listed, members.Member{Name: "pve2", ID: 2, Online: true})
			snap := w.inv.snapshot()
			snap.Nodes = append(snap.Nodes, inventory.Node{Name: "pve2", Addr: netip.MustParseAddr("10.0.0.3"), Online: true})
			w.inv.set(snap)
			w.acts["pve2"] = &agentAct{kind: pki.KindHost, started: now.Add(-time.Hour)}
			if restart {
				w.eng = w.newEngine()
				w.watchHere(t)
			}

			sawBoth := false
			for range 40 {
				w.tick(5 * time.Second)
				w.requireNothingWithdrawn(t, "pve.example.com", "www.example.com")
				rec := w.recordTunnel(t, "pve.example.com")
				if rec == tunnelName && slices.Contains(w.servedIn(t, "pve.example.com"), pinned1) {
					sawBoth = true
				}
				if rec == pinned1 {
					require.True(t, sawBoth, "the rule was in the pinned tunnel before the record pointed there")
					break
				}
			}
			require.Equal(t, pinned1, w.recordTunnel(t, "pve.example.com"))
			require.Equal(t, tunnelName, w.recordTunnel(t, "www.example.com"), "the guest's route stays shared")
			require.True(t, slices.ContainsFunc(w.placementEvents(), func(ev string) bool {
				return strings.HasPrefix(ev, model.ManualPrefix+"pve: ") && strings.Contains(ev, " moves from "+tunnelName+" to "+pinned1)
			}), w.placementEvents())
		})
	}
}

var hosts3 = []string{"app.example.com", "db.example.com", "www.example.com"}

// rulesByTunnel are the rules of every tunnel of the account, by name.
func (w *world) rulesByTunnel(t *testing.T) map[string][]planner.IngressRule {
	t.Helper()
	out := map[string][]planner.IngressRule{}
	for _, tun := range w.cf.TunnelsIn(testAccount) {
		cfg, err := w.cf.TunnelConfig(t.Context(), testAccount, tun.ID)
		require.NoError(t, err)
		out[tun.Name] = cfg.Ingress
	}
	return out
}

// servedIn are the tunnels whose configuration sends host to a target.
func (w *world) servedIn(t *testing.T, host string) []string {
	t.Helper()
	var out []string
	for name, rules := range w.rulesByTunnel(t) {
		if slices.ContainsFunc(rules, func(r planner.IngressRule) bool { return r.Hostname == host && r.Service != planner.BlockedService }) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// recordTunnel is the tunnel name the record of host points at; empty
// when it has none.
func (w *world) recordTunnel(t *testing.T, host string) string {
	t.Helper()
	ids := map[string]string{}
	for _, tun := range w.cf.TunnelsIn(testAccount) {
		ids[tun.ID+".cfargotunnel.com"] = tun.Name
	}
	for _, r := range w.records() {
		if r.Name == host {
			return ids[r.Content]
		}
	}
	return ""
}

// requireNothingWithdrawn fails when a hostname of hosts has no rule that
// serves it in the tunnel its record points at, or when a record was
// deleted.
func (w *world) requireNothingWithdrawn(t *testing.T, hosts ...string) {
	t.Helper()
	for _, host := range hosts {
		rec := w.recordTunnel(t, host)
		require.NotEmpty(t, rec, "%s has a record", host)
		require.Contains(t, w.servedIn(t, host), rec, "%s is served where its record points", host)
	}
	for _, call := range w.writes() {
		require.False(t, strings.HasPrefix(call, "DeleteRecord"), call)
	}
}

// moves are the events of the moves of placements.
func (w *world) placementEvents() []string {
	var out []string
	for _, ev := range w.eng.Events(time.Time{}) {
		if ev.Kind == kindPlacement {
			out = append(out, ev.Subject+": "+ev.Message)
		}
	}
	return out
}

// Where a record pointed when its zone was last listed stands for a few
// minutes only: this controller may have switched it since, in a cycle
// whose run could not list the zone, and a route is not taken for served
// through the shared tunnel on a listing that old.
func TestWhereARecordPointedIsForgottenAfterAWhile(t *testing.T) {
	m := &placementMemory{pointedAt: map[string]pointedRecord{"www.example.com": {tunnel: tunnelName, at: t0}}}

	require.Equal(t, tunnelName, m.recordPointed("www.example.com", t0.Add(pointedFor-time.Second)))
	require.Empty(t, m.recordPointed("www.example.com", t0.Add(pointedFor)))
	require.Empty(t, m.recordPointed("db.example.com", t0))
}

// What the placement remembers by owner or hostname is pruned against the
// winners of every cycle: a route that is gone leaves no timer behind that
// would count for one with its name later.
func TestThePlacementMemoryIsPrunedAgainstTheWinners(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.eng.place.addedAt = map[string]time.Time{"qemu/101": t0, "qemu/999": t0}
	w.eng.place.unresolvedSince = map[string]time.Time{"old.example.com": t0}
	w.eng.place.waiting = map[string]time.Time{"qemu/999": t0}
	w.eng.place.reaching = map[string]time.Time{"qemu/999": t0}
	w.eng.place.diverged = map[string]time.Time{"qemu/999": t0}

	w.tick(time.Second)

	require.Contains(t, w.eng.place.addedAt, "qemu/101", "an owner of a winner keeps its timer")
	require.NotContains(t, w.eng.place.addedAt, "qemu/999")
	require.NotContains(t, w.eng.place.unresolvedSince, "old.example.com")
	require.NotContains(t, w.eng.place.waiting, "qemu/999")
	require.NotContains(t, w.eng.place.reaching, "qemu/999")
	require.NotContains(t, w.eng.place.diverged, "qemu/999")
}

// A route that waited long for the replicas to report it, was released and
// comes back waits for them again: the time it waited before does not let
// it be placed at once, which would store it shared without a word from
// any replica.
func TestARouteThatComesBackWaitsForTheReplicasAgain(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.eng.place.waiting = map[string]time.Time{"qemu/104": w.clock.now().Add(-time.Hour)}
	w.tick(time.Second)
	snap := w.inv.snapshot()
	snap.Guests = append(snap.Guests, carded(guest(104, "new", "new.example.com -> :8080"), "pve3", "bc:24:11:00:04:01"))
	w.inv.set(snap)
	w.acts["pve2"].silent = true

	st := w.tick(time.Second)
	require.Contains(t, route(st, "new.example.com").Reason, "waiting for pve2")
	placements, err := w.store.Placements()
	require.NoError(t, err)
	require.NotContains(t, placements, "new.example.com")
}

// Three nodes, a guest on each: every route is shared once every replica
// reached it, the guests of other nodes at port through their node's
// attestation, and every replica runs the shared tunnel.
func TestEveryRouteIsSharedOnceEveryReplicaReachesIt(t *testing.T) {
	w := threeNodes(t)

	st := w.tick(time.Second)
	for _, host := range hosts3 {
		require.Contains(t, route(st, host).Reason, "waiting for pve2, pve3 to verify it", host)
	}
	require.Empty(t, w.records(), "nothing is published before every replica has it")

	st = w.settle()

	for _, host := range hosts3 {
		r := route(st, host)
		require.Equal(t, planner.StateActive, r.State, host)
		require.Equal(t, TunnelShared, r.Placement.Kind, host)
		require.Len(t, r.Replicas, 3, host)
		require.Equal(t, tunnelName, w.recordTunnel(t, host))
	}
	require.Equal(t, "port", route(st, "db.example.com").Replicas[0].Level, "pve1 proves it through pve2's attestation")
	for _, node := range []string{"pve2", "pve3"} {
		s, _ := w.r.lastOf(node)
		i := slices.IndexFunc(s.Tunnels, func(tun agentproto.Tunnel) bool { return tun.Name == tunnelName })
		require.GreaterOrEqual(t, i, 0, node)
		require.True(t, s.Tunnels[i].Run, node)
		require.NotEmpty(t, s.Tunnels[i].Token.Reveal(), node)
	}
	placements, err := w.store.Placements()
	require.NoError(t, err)
	require.Len(t, placements, 3)
	require.Equal(t, "pve2", placements["db.example.com"].ResolvingNode)
	require.Equal(t, []string{w.tunnelID(t, tunnelName)}, w.targetOf(t, "pve3", "db.example.com").Tunnels, "the boot gate knows it is a shared target")
}

// An agent that stops: once its node has no status for long, every route
// of a guest on another node is pinned to its node at once, and nothing is
// withdrawn at Cloudflare at any point; the routes move with their rules in
// both tunnels until the records are switched.
func TestAStoppedAgentPinsEveryCrossNodeRouteAndWithdrawsNothing(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.requireNothingWithdrawn(t, hosts3...)

	w.acts["pve3"].silent = true
	for range 40 {
		st := w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, hosts3...)
		_ = st
	}

	st := w.tick(time.Second)
	require.True(t, hasProblem(st, "node pve3 has no pco agent with a running watch"), st.Problems)
	for host, node := range map[string]string{"www.example.com": "pve1", "db.example.com": "pve2"} {
		name, _ := planner.PinnedTunnelName(testInstall, node)
		require.Equal(t, TunnelPinned, route(st, host).Placement.Kind, host)
		require.Equal(t, name, w.recordTunnel(t, host), host)
	}
}

// Coverage lost pins the route of another node's guest to that node only
// while the node reaches it: a node that fails its own guest's route
// serves it no better than the replicas do, so the route is held as it is
// until the node reaches it.
func TestCoverageLostPinsARouteOnlyToANodeThatReachesIt(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.acts["pve3"].noWatch = true
	w.acts["pve2"].fail = map[string]string{"db.example.com": "not reached"}

	var st State
	for range 12 {
		st = w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, hosts3...)
	}
	require.True(t, hasProblem(st, "node pve3 has no pco agent with a running watch"), st.Problems)
	require.Equal(t, TunnelPinned, route(st, "www.example.com").Placement.Kind, "pve1 reaches its guest's route")
	require.Equal(t, TunnelShared, route(st, "db.example.com").Placement.Kind, "pve2 does not reach its guest's route")
	for _, ev := range w.placementEvents() {
		require.False(t, strings.HasPrefix(ev, "qemu/102: ") && strings.Contains(ev, " moves from "), ev)
	}

	w.acts["pve2"].fail = nil
	for range 12 {
		st = w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, hosts3...)
	}
	require.Equal(t, TunnelPinned, route(st, "db.example.com").Placement.Kind)
	require.Equal(t, "pve2", route(st, "db.example.com").Placement.Node)
}

// A route no node resolves now keeps its stored address, unreachable, only
// while its guest may still run and for as long as a proof may be old: the
// address of a stopped guest, and one no node resolved for longer, is
// withdrawn.
func TestAStoredAddressIsHeldOnlyWhileItsGuestMayStillRun(t *testing.T) {
	dropDB := func(st *agentproto.Status) {
		st.Targets = slices.DeleteFunc(st.Targets, func(r agentproto.TargetReport) bool { return r.Key == "db.example.com" })
	}
	t.Run("a running guest, for five minutes", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		w.acts["pve2"].edit = dropDB
		var st State
		for range 6 {
			st = w.tick(50 * time.Second)
			r := route(st, "db.example.com")
			require.Equal(t, planner.StateUnreachable, r.State, r.Reason)
			require.Contains(t, r.Reason, "held as it was")
		}
		st = w.tick(50 * time.Second)
		r := route(st, "db.example.com")
		require.Equal(t, planner.StateWithdrawn, r.State, r.Reason)
		require.Contains(t, r.Reason, "no node resolved it for 5m0s")
	})
	t.Run("a stopped guest, at once", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		w.acts["pve2"].edit = dropDB
		snap := w.inv.snapshot()
		snap.Guests[1].Running = false
		w.inv.set(snap)
		st := w.tick(time.Second)
		r := route(st, "db.example.com")
		require.Equal(t, planner.StateWithdrawn, r.State, r.Reason)
		require.Contains(t, r.Reason, "its guest is not running")
	})
	t.Run("a guest of unknown state, as a running one", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		w.acts["pve2"].edit = dropDB
		snap := w.inv.snapshot()
		snap.Guests[1].Running, snap.Guests[1].StatusUnknown = false, true
		w.inv.set(snap)
		st := w.tick(time.Second)
		r := route(st, "db.example.com")
		require.Equal(t, planner.StateUnreachable, r.State, r.Reason)
	})
}

// What the state shows of a replica is its own word, within what it may
// say: port (attested) only from a host agent, as an appliance reads no
// forwarding table; and a shared route's level is the lowest over its
// replicas, a pinned route's that of its node.
func TestAReplicaIsShownAtItsOwnLevelAndARouteAtTheLowest(t *testing.T) {
	replica := func(st State, host, node string) ReplicaView {
		for _, v := range route(st, host).Replicas {
			if v.Node == node {
				return v
			}
		}
		t.Fatalf("no replica %s of %s", node, host)
		return ReplicaView{}
	}
	t.Run("attested only from a host agent", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		attested := func(st *agentproto.Status) {
			for i := range st.Targets {
				st.Targets[i].Attested, st.Targets[i].AttestedBy = true, "pve1"
			}
		}
		w.acts["pve2"].edit = attested
		w.acts["pve3"].edit = attested
		w.acts["pve3"].kind = pki.KindAppliance
		st := w.tick(time.Second)
		require.True(t, replica(st, "www.example.com", "pve2").Attested)
		require.Equal(t, "pve1", replica(st, "www.example.com", "pve2").AttestedBy, "the node that runs the guest vouches")
		require.False(t, replica(st, "www.example.com", "pve3").Attested, "an appliance agent proves no port")
		require.Empty(t, replica(st, "www.example.com", "pve3").AttestedBy)
		require.Equal(t, "pve1", route(st, "www.example.com").GuestNode, "where the inventory runs the guest")
		require.Equal(t, "pve2", route(st, "db.example.com").GuestNode)
		require.False(t, replica(st, "db.example.com", "pve1").Attested, "the controller's own replica stands on no attestation")
	})
	t.Run("an attesting node the inventory does not run the guest on", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		w.acts["pve2"].edit = func(st *agentproto.Status) {
			for i := range st.Targets {
				st.Targets[i].Attested, st.Targets[i].AttestedBy = true, "pve2"
			}
		}
		st := w.tick(time.Second)
		v := replica(st, "www.example.com", "pve2")
		require.False(t, v.Attested, "a replica cannot vouch for a guest of another node with its own table")
		require.Empty(t, v.AttestedBy)
		require.Equal(t, "port", v.Level, "its level stays its own word")
	})
	t.Run("an attesting node the guest left since", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		w.acts["pve2"].edit = func(st *agentproto.Status) {
			for i := range st.Targets {
				st.Targets[i].Attested, st.Targets[i].AttestedBy = true, "pve1"
			}
		}
		snap := w.inv.snapshot()
		for i := range snap.Guests {
			if snap.Guests[i].Node == "pve1" {
				snap.Guests[i].Node = "pve3"
			}
		}
		w.inv.set(snap)
		st := w.tick(time.Second)
		v := replica(st, "www.example.com", "pve2")
		require.False(t, v.Attested, "the attestation was of where the guest ran before it moved")
		require.Empty(t, v.AttestedBy)
		require.Equal(t, "pve3", route(st, "www.example.com").GuestNode)
	})
	t.Run("an attestation that names no node", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		w.acts["pve2"].edit = func(st *agentproto.Status) {
			for i := range st.Targets {
				st.Targets[i].Attested = true
			}
		}
		st := w.tick(time.Second)
		require.False(t, replica(st, "www.example.com", "pve2").Attested)
	})
	t.Run("the lowest level over the replicas", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		w.acts["pve3"].edit = func(st *agentproto.Status) {
			for i := range st.Targets {
				if st.Targets[i].Key == "www.example.com" {
					st.Targets[i].Level = "observed"
				}
			}
		}
		w.tick(time.Second)
		st := w.tick(time.Second)
		require.Equal(t, "observed", route(st, "www.example.com").Level, "pve3 proves it at observed only")
		require.Equal(t, "port", replica(st, "www.example.com", "pve1").Level)
		require.Equal(t, "port", route(st, "db.example.com").Level)
	})
	t.Run("a pinned route at its node's level", func(t *testing.T) {
		w, _ := pinnedDB(t)
		w.acts["pve3"].edit = func(st *agentproto.Status) {
			for i := range st.Targets {
				st.Targets[i].Level = "observed"
			}
		}
		w.tick(time.Second)
		st := w.tick(time.Second)
		require.Equal(t, "port", route(st, "db.example.com").Level, "pve3 is no replica of db's tunnel")
	})
}

// An agent that restarts every 50 seconds for ten minutes: its first
// restart is forgiven, the second pins the routes, they stay pinned for as
// long as it flaps, and no route moves twice.
func TestAFlappingAgentMovesNothingTwice(t *testing.T) {
	w := threeNodes(t)
	w.settle()

	for i := range 12 {
		w.acts["pve3"].started = w.clock.now()
		for range 5 {
			w.tick(10 * time.Second)
			w.requireNothingWithdrawn(t, hosts3...)
		}
		_ = i
	}

	moved := map[string]int{}
	for _, ev := range w.placementEvents() {
		if strings.Contains(ev, " moves from ") {
			moved[strings.SplitN(ev, ":", 2)[0]]++
		}
	}
	for owner, n := range moved {
		require.Equal(t, 1, n, "%s moved %d times", owner, n)
	}
	st := w.tick(time.Second)
	require.Equal(t, TunnelPinned, route(st, "www.example.com").Placement.Kind)
	require.True(t, hasProblem(st, "the watch of the agent of node pve3 broke twice within ten minutes"), st.Problems)
}

// Coverage regained: the routes it pinned are shared again once every node
// watched for ten minutes without a break, not before.
func TestRoutesAreSharedAgainTenMinutesAfterCoverageIsBack(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.acts["pve3"].noWatch = true
	for range 10 {
		w.tick(10 * time.Second)
	}
	st := w.tick(time.Second)
	require.Equal(t, TunnelPinned, route(st, "www.example.com").Placement.Kind)

	w.acts["pve3"].noWatch = false
	w.acts["pve3"].started = w.clock.now()
	for range 50 {
		st = w.tick(10 * time.Second)
		w.requireNothingWithdrawn(t, hosts3...)
	}
	require.Equal(t, TunnelPinned, route(st, "www.example.com").Placement.Kind, "under ten minutes")
	for range 40 {
		st = w.tick(10 * time.Second)
		w.requireNothingWithdrawn(t, hosts3...)
	}
	require.Equal(t, TunnelShared, route(st, "www.example.com").Placement.Kind)
	requirePlacementReasons(t, w)
}

// A route that moves keeps its rule in the tunnel it leaves until its
// record was switched and five minutes more, and has it in the tunnel it
// goes to before the record points there.
func TestAMoveKeepsTheOldRulesUntilTheRecordsSwitched(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	pinned1, _ := planner.PinnedTunnelName(testInstall, "pve1")
	w.acts["pve3"].noWatch = true

	sawBoth := false
	switched := time.Time{}
	for range 80 {
		w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, "www.example.com")
		in := w.servedIn(t, "www.example.com")
		rec := w.recordTunnel(t, "www.example.com")
		if rec == tunnelName && slices.Contains(in, pinned1) {
			sawBoth = true
		}
		if rec == pinned1 && switched.IsZero() {
			switched = w.clock.now()
			require.True(t, sawBoth, "the rule was in the pinned tunnel before the record pointed there")
		}
		if !switched.IsZero() && w.clock.now().Sub(switched) < oldRulesFor {
			require.Contains(t, in, tunnelName, "the old rule stays for five minutes after the switch")
		}
	}
	require.False(t, switched.IsZero(), "the record was switched")
	require.Equal(t, []string{pinned1}, w.servedIn(t, "www.example.com"), "the old rule is gone at the end")
	moves, err := w.store.Moves()
	require.NoError(t, err)
	require.Empty(t, moves)
}

// A new controller takes over in the middle of a move: one that had only
// added its rules is given up, one whose rules were rolled out goes on,
// and one that had switched the records is finished.
func TestANewControllerAbortsAMoveBeforeTheSwitchAndFinishesOneAfter(t *testing.T) {
	for _, step := range []string{store.MoveRulesAdded, store.MoveRolledOut, store.MoveCNAMEsSwitched} {
		t.Run(step, func(t *testing.T) {
			w := threeNodes(t)
			w.settle()
			pinned1, _ := planner.PinnedTunnelName(testInstall, "pve1")
			require.NoError(t, w.store.SaveMove(store.Move{Owner: "qemu/101", From: tunnelName, To: pinned1, Step: step, At: w.clock.now(), CNAMEAt: w.clock.now()}))
			if step == store.MoveCNAMEsSwitched {
				placements, err := w.store.Placements()
				require.NoError(t, err)
				p := placements["www.example.com"]
				p.Kind, p.Node, p.Tunnel = store.PlacementPinned, "pve1", pinned1
				placements["www.example.com"] = p
				require.NoError(t, w.store.SavePlacements(placements))
			}

			w.eng = w.newEngine()
			w.watchHere(t)
			w.tick(time.Second)
			w.tick(time.Second)

			moves, err := w.store.Moves()
			require.NoError(t, err)
			if step == store.MoveCNAMEsSwitched {
				require.Contains(t, moves, "qemu/101", "finished, not given up")
				require.Equal(t, pinned1, w.recordTunnel(t, "www.example.com"))
				return
			}
			if step == store.MoveRolledOut {
				// Nothing wants the route pinned: the move is turned around,
				// and its rules stay in both tunnels until the records are
				// seen where they are.
				require.Contains(t, moves, "qemu/101", "turned around, not dropped")
				require.Equal(t, store.Move{Owner: "qemu/101", From: pinned1, To: tunnelName, Step: store.MoveCNAMEsSwitched, At: moves["qemu/101"].At, CNAMEAt: moves["qemu/101"].CNAMEAt}, moves["qemu/101"])
				require.Equal(t, tunnelName, w.recordTunnel(t, "www.example.com"))
				require.Equal(t, []string{tunnelName, pinned1}, w.servedIn(t, "www.example.com"))
				w.requireNothingWithdrawn(t, hosts3...)
				return
			}
			require.Empty(t, moves)
			require.Equal(t, tunnelName, w.recordTunnel(t, "www.example.com"))
			w.requireNothingWithdrawn(t, hosts3...)
		})
	}
}

// A controller that takes over writes nothing until the agents reported,
// then keeps every placement it finds stored: no route moves and nothing
// is withdrawn.
func TestATakeoverMovesNothing(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	before, err := w.store.Placements()
	require.NoError(t, err)
	writes := len(w.writes())

	w.eng = w.newEngine()
	w.watchHere(t)
	for range 10 {
		w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, hosts3...)
	}

	after, err := w.store.Placements()
	require.NoError(t, err)
	for host, p := range before {
		require.Equal(t, p.Kind, after[host].Kind, host)
		require.Equal(t, p.Node, after[host].Node, host)
	}
	for _, call := range w.writes()[writes:] {
		require.False(t, strings.HasPrefix(call, "DeleteRecord") || strings.HasPrefix(call, "UpdateRecord"), call)
	}
	require.Empty(t, w.placementEvents())
}

// A node that joins is admitted only once it reported every shared route,
// and a shared route it does not reach is moved to its guest's node first:
// no route it fails is served through its connector.
func TestAJoiningNodeIsAdmittedOnceTheRoutesItFailsMoved(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	n := entryOf(t, w.env, "pve3")
	n.State, n.StateSince = store.NodeJoining, w.clock.now()
	require.NoError(t, w.store.SaveNode(n))
	w.acts["pve3"].started = w.clock.now()
	w.acts["pve3"].fail = map[string]string{"www.example.com": "not reached"}

	pinned1, _ := planner.PinnedTunnelName(testInstall, "pve1")
	admitted := false
	for range 30 {
		w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, hosts3...)
		if entryOf(t, w.env, "pve3").State == store.NodeActive {
			admitted = true
			require.Equal(t, pinned1, w.recordTunnel(t, "www.example.com"), "moved before the node was admitted")
			break
		}
	}
	require.True(t, admitted)
}

// A joining node that would move more routes than cluster.admissionMaxMoves
// stays joining, and says why.
func TestAJoiningNodeThatWouldMoveTooMuchStaysJoining(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.settings(func(s *store.Settings) { s.Cluster.AdmissionMaxMoves = 1 })
	n := entryOf(t, w.env, "pve3")
	n.State, n.StateSince = store.NodeJoining, w.clock.now()
	require.NoError(t, w.store.SaveNode(n))
	w.acts["pve3"].started = w.clock.now()
	w.acts["pve3"].fail = map[string]string{"www.example.com": "no", "db.example.com": "no"}

	var st State
	for range 10 {
		st = w.tick(5 * time.Second)
	}
	require.Equal(t, store.NodeJoining, entryOf(t, w.env, "pve3").State)
	require.True(t, hasProblem(st, "node pve3 stays joining"), st.Problems)
	w.requireNothingWithdrawn(t, hosts3...)
}

// Nodes that come back joining while coverage is lost are not given the
// shared route of a guest on another node, so they never report it: it
// counts as a route they do not reach, moves to its guest's node, and they
// are admitted. While they join they run no connector of the shared tunnel
// and keep none of its token.
func TestJoiningNodesWithoutCoverageAreAdmittedAndRunNoSharedConnector(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	joining := []string{"pve2", "pve3"}
	for _, node := range joining {
		w.acts[node].noWatch = true
		// Started again, the agents have not applied a snapshot of this
		// epoch yet.
		w.acts[node].edit = func(st *agentproto.Status) { st.EpochSeen, st.Targets = agentproto.Epoch{}, nil }
		n := entryOf(t, w.env, node)
		n.State, n.StateSince = store.NodeJoining, w.clock.now()
		require.NoError(t, w.store.SaveNode(n))
	}
	w.tick(5 * time.Second)
	for _, node := range joining {
		w.acts[node].edit = nil
	}

	pinned1, _ := planner.PinnedTunnelName(testInstall, "pve1")
	pruned := false
	admitted := map[string]bool{}
	for range 60 {
		w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, hosts3...)
		for _, node := range joining {
			s, ok := w.r.lastOf(node)
			if !ok || s.State != agentproto.Joining {
				continue
			}
			for _, tun := range s.Tunnels {
				require.False(t, tun.Name == tunnelName && (tun.Run || tun.Token.Reveal() != ""), "a joining node runs no shared connector")
			}
			if s.Prune && !slices.ContainsFunc(s.Tunnels, func(tun agentproto.Tunnel) bool { return tun.Name == tunnelName }) {
				pruned = true
			}
		}
		for _, node := range joining {
			if entryOf(t, w.env, node).State == store.NodeActive && !admitted[node] {
				admitted[node] = true
				require.Equal(t, pinned1, w.recordTunnel(t, "www.example.com"), "moved before %s was admitted", node)
			}
		}
	}
	require.True(t, admitted["pve2"] && admitted["pve3"], "both nodes are admitted: %v", admitted)
	require.True(t, pruned, "the snapshot of a joining node has a shared connector it still runs removed")
	requirePlacementReasons(t, w)
}

// requirePlacementReasons fails the test on a placement event that does not
// say why the route went where it went.
func requirePlacementReasons(t *testing.T, w *world) {
	t.Helper()
	for _, ev := range w.placementEvents() {
		if !strings.Contains(ev, " moves from ") && !strings.Contains(ev, " is placed ") {
			continue
		}
		_, why, ok := strings.Cut(ev[strings.Index(ev, ": ")+2:], ": ")
		require.True(t, ok && strings.TrimSpace(why) != "", "no reason: %s", ev)
	}
}

// A pinned guest that moves to another node follows it: a move to the
// tunnel pinned to its new node, once that node reaches it.
func TestAPinnedGuestFollowsItsNode(t *testing.T) {
	w := threeNodes(t)
	w.acts["pve3"].fail = map[string]string{"db.example.com": "not reached"}
	for range 10 {
		w.tick(5 * time.Second)
	}
	st := w.tick(time.Second)
	require.Equal(t, TunnelPinned, route(st, "db.example.com").Placement.Kind)
	require.Equal(t, "pve2", route(st, "db.example.com").Placement.Node)

	snap := w.inv.snapshot()
	snap.Guests[1].Node = "pve3"
	w.inv.set(snap)
	w.acts["pve3"].fail = nil
	w.acts["pve2"].fail = map[string]string{"db.example.com": "gone"}
	pinned3, _ := planner.PinnedTunnelName(testInstall, "pve3")
	for range 30 {
		w.tick(5 * time.Second)
	}
	st = w.tick(time.Second)
	require.Equal(t, "pve3", route(st, "db.example.com").Placement.Node, fmt.Sprint(w.placementEvents()))
	require.Equal(t, pinned3, w.recordTunnel(t, "db.example.com"))
}

// A route stored shared that no replica can serve, as one placed before
// its node was admitted, moves to its guest's node all the same once
// coverage is lost: the rule of the tunnel it goes to is judged by that
// tunnel's node, not by the replicas of the shared tunnel, so the move
// gets its rule, rolls out and switches the record.
func TestARouteTheSharedTunnelNeverServedMovesToItsNode(t *testing.T) {
	w := threeNodes(t)
	pinned3, _ := planner.PinnedTunnelName(testInstall, "pve3")
	w.acts["pve2"].noWatch = true
	w.acts["pve2"].fail = map[string]string{"app.example.com": "not reached"}
	w.res.setUnreachable("app.example.com", "not reached")
	require.NoError(t, w.store.SavePlacements(map[string]store.Placement{
		"app.example.com": {Hostname: "app.example.com", Kind: store.PlacementShared, Tunnel: tunnelName, ResolvingNode: "pve3", Since: w.clock.now()},
	}))

	var st State
	for range 60 {
		st = w.tick(5 * time.Second)
	}
	require.Equal(t, pinned3, w.recordTunnel(t, "app.example.com"), fmt.Sprint(w.placementEvents()))
	require.Equal(t, []string{pinned3}, w.servedIn(t, "app.example.com"))
	require.Equal(t, planner.StateActive, route(st, "app.example.com").State, route(st, "app.example.com").Reason)
}

// The move of an owner that is released is dropped: nothing of it is left
// to move, and a route that comes back with its name starts afresh rather
// than in a move of its earlier self.
func TestTheMoveOfAReleasedOwnerIsDropped(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	pinned3, _ := planner.PinnedTunnelName(testInstall, "pve3")
	require.NoError(t, w.store.SaveMove(store.Move{Owner: "qemu/103", From: tunnelName, To: pinned3, Step: store.MoveRulesAdded, At: w.clock.now()}))
	snap := w.inv.snapshot()
	snap.Guests = snap.Guests[:2]
	w.inv.set(snap)

	for range 40 {
		w.tick(5 * time.Second)
	}
	_, ok := w.moveOf(t, "qemu/103")
	require.False(t, ok, "the move of a released owner is gone")
}

// A pinned guest that moves to another node while coverage is lost is
// resolved on its new node at once: that node is the one that resolves it,
// whatever tunnel the route is in. The route follows it by a move, and is
// served where its record points at every step.
func TestAPinnedGuestFollowsItsNodeWithoutCoverage(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.acts["pve3"].noWatch = true
	pinned2, _ := planner.PinnedTunnelName(testInstall, "pve2")
	pinned3, _ := planner.PinnedTunnelName(testInstall, "pve3")
	for range 100 {
		w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, hosts3...)
	}
	require.Equal(t, pinned2, w.recordTunnel(t, "db.example.com"))

	snap := w.inv.snapshot()
	snap.Guests[1].Node = "pve3"
	w.inv.set(snap)
	w.acts["pve2"].fail = map[string]string{"db.example.com": "gone"}
	w.tick(5 * time.Second)
	require.True(t, w.targetOf(t, "pve3", "db.example.com").Resolve, "its new node resolves it")
	for range 30 {
		w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, hosts3...)
	}
	st := w.tick(time.Second)
	require.Equal(t, "pve3", route(st, "db.example.com").Placement.Node, fmt.Sprint(w.placementEvents()))
	require.Equal(t, pinned3, w.recordTunnel(t, "db.example.com"))
}
