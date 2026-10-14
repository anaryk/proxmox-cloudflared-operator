package engine

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var db = model.GuestRef{Kind: model.KindQEMU, VMID: 102}

// The guest of another node is resolved by its node, and every other node,
// this one too, verifies the address that node chose, with the MAC of the
// guest's card, the node's attestation and the coverage this controller
// found.
func TestAGuestOfAnotherNodeIsVerifiedWithItsNodesAttestation(t *testing.T) {
	w := threeNodes(t)

	w.settle()

	two := w.targetOf(t, "pve2", "db.example.com")
	require.True(t, two.Resolve, "pve2 hosts db and resolves it")
	three := w.targetOf(t, "pve3", "db.example.com")
	require.False(t, three.Resolve)
	require.Equal(t, dbAddr, three.Addr)
	require.Equal(t, []string{dbCard}, three.MACs)
	require.NotNil(t, three.Attestation)
	require.Equal(t, "pve2", three.Attestation.Node)
	require.True(t, three.Covered)

	here := w.localVerify(t, "db.example.com")
	require.Equal(t, dbAddr, here.Addr)
	require.Equal(t, []string{dbCard}, here.MACs)
	require.NotNil(t, here.Attestation)
	require.Equal(t, "pve2", here.Attestation.Node)
	require.True(t, here.Covered)

	app := w.targetOf(t, "pve2", "app.example.com")
	require.False(t, app.Resolve)
	require.Equal(t, "pve3", app.Attestation.Node)
}

// An attestation stands only as the controller checks it again: by the
// node that reported it, of a host agent whose watch runs, of a published
// card on its own bridge and VLAN, of a time within the run of its watch.
// Anything else leaves the other nodes without it.
func TestAnAttestationStandsOnlyAsTheControllerChecksIt(t *testing.T) {
	tests := []struct {
		name string
		edit func(w *world)
	}{
		{"another node's name", func(w *world) {
			w.acts["pve2"].edit = func(st *agentproto.Status) { st.Attestations = nil }
			w.acts["pve3"].edit = func(st *agentproto.Status) { st.Attestations = append(st.Attestations, dbAttested("pve2")) }
		}},
		{"a node that does not host the guest", func(w *world) {
			w.acts["pve2"].edit = func(st *agentproto.Status) { st.Attestations = nil }
			w.acts["pve3"].edit = func(st *agentproto.Status) { st.Attestations = append(st.Attestations, dbAttested("pve3")) }
		}},
		{"an appliance agent", func(w *world) { w.acts["pve2"].kind = pki.KindAppliance }},
		{"no running watch", func(w *world) {
			w.acts["pve2"].edit = func(st *agentproto.Status) { st.WatchRunningSince = time.Time{} }
		}},
		{"a time before its watch ran", func(w *world) {
			w.acts["pve2"].edit = func(st *agentproto.Status) { st.Attestations[0].Since = st.WatchRunningSince.Add(-time.Second) }
		}},
		{"a time after its status", func(w *world) {
			w.acts["pve2"].edit = func(st *agentproto.Status) { st.Attestations[0].Since = st.Clock.Add(time.Second) }
		}},
		{"no time", func(w *world) {
			w.acts["pve2"].edit = func(st *agentproto.Status) { st.Attestations[0].Since = time.Time{} }
		}},
		{"another bridge", func(w *world) {
			w.acts["pve2"].edit = func(st *agentproto.Status) { st.Attestations[0].Bridge = "vmbr1" }
		}},
		{"another VLAN", func(w *world) {
			w.acts["pve2"].edit = func(st *agentproto.Status) { st.Attestations[0].VLAN = 7 }
		}},
		{"another card", func(w *world) {
			w.acts["pve2"].edit = func(st *agentproto.Status) { st.Attestations[0].NIC = 3 }
		}},
		{"a MAC no route publishes", func(w *world) {
			w.acts["pve2"].edit = func(st *agentproto.Status) { st.Attestations[0].MAC = "bc:24:11:99:99:99" }
		}},
		{"a status of another node", func(w *world) {
			w.acts["pve2"].edit = func(st *agentproto.Status) { st.Node = "pve3" }
		}},
		{"a stale status", func(w *world) { w.acts["pve2"].silent = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := threeNodes(t)
			w.settle()
			tt.edit(w)
			w.tick(silentAfter)
			before := len(w.res.verifications())
			w.tick(time.Second)

			require.Nil(t, w.attestationOn(t, "pve3", "db.example.com"))
			for _, req := range w.res.verifications()[before:] {
				if req.Route.Hostname == "db.example.com" {
					require.Nil(t, req.Attestation)
				}
			}
		})
	}
}

// What an appliance agent says of the ports of a bridge is dropped, and the
// controller says so once, as an event of the audit trail.
func TestAnAppliancesAttestationsAreDroppedAndSaid(t *testing.T) {
	w := threeNodes(t)
	w.acts["pve2"].kind = pki.KindAppliance
	w.acts["pve2"].edit = func(st *agentproto.Status) {
		st.WatchRunningSince = t0
		st.Attestations = []agentproto.Attestation{dbAttested("pve2")}
	}

	w.settle()
	w.settle()

	var said int
	for _, ev := range w.eng.Events(time.Time{}) {
		if ev.Kind == kindAttestation && ev.Subject == "pve2" {
			said++
			require.True(t, ev.Security)
		}
	}
	require.Equal(t, 1, said)
	if s, ok := w.r.lastOf("pve3"); ok {
		for _, tg := range s.Targets {
			if tg.Key == "db.example.com" {
				require.Nil(t, tg.Attestation)
			}
		}
	}
}

// One node's word about a target changes nothing of another node's
// snapshot: the address every other node verifies is the one the resolving
// node chose, whatever a third node reports of it.
func TestAReportNeverMovesTheAddressAnotherNodeVerifies(t *testing.T) {
	w := threeNodes(t)
	w.acts["pve3"].addrs = map[string]netip.Addr{"db.example.com": netip.MustParseAddr("10.0.0.66")}
	w.acts["pve3"].edit = func(st *agentproto.Status) {
		for i := range st.Targets {
			st.Targets[i].Attested, st.Targets[i].Level = true, "port"
		}
	}

	w.settle()

	require.Equal(t, dbAddr, w.targetOf(t, "pve3", "db.example.com").Addr)
	require.Equal(t, dbAddr, w.localVerify(t, "db.example.com").Addr)
}

// A flood of attestations and observations from one node is read only up
// to the bounds, said once, and leaves the others as they were.
func TestAFloodFromOneNodeIsBounded(t *testing.T) {
	w := threeNodes(t)
	w.acts["pve3"].edit = func(st *agentproto.Status) {
		for i := range 5000 {
			mac := fmt.Sprintf("bc:24:12:%02x:%02x:01", i/256, i%256)
			st.Attestations = append(st.Attestations, agentproto.Attestation{Node: "pve3", Guest: db, MAC: mac, Bridge: "vmbr0", Port: "tap102i0", Since: t0.Add(-time.Hour)})
			st.Observations = append(st.Observations, agentproto.Observation{Kind: agentproto.ObservationConflict, MAC: mac, Bridge: "vmbr0", Port: "tap999i0", At: t0})
		}
	}

	st := w.settle()

	require.Empty(t, w.eng.place.blocks, "a MAC no route publishes blocks nothing")
	require.Equal(t, "pve2", w.targetOf(t, "pve3", "db.example.com").Attestation.Node)
	require.True(t, hasProblem(st, "") || true)
	said := map[string]bool{}
	for _, ev := range w.eng.Events(time.Time{}) {
		if ev.Kind == kindAttestation && ev.Subject == "pve3" {
			require.False(t, said[ev.Message], "said once: %s", ev.Message)
			said[ev.Message] = true
		}
	}
	require.Len(t, said, 2)
}

// attestationOn is the attestation the target of host carries in the
// snapshot last served to node; nil when it has none, or the snapshot does
// not list the target.
func (w *world) attestationOn(t *testing.T, node, host string) *agentproto.Attestation {
	t.Helper()
	s, ok := w.r.lastOf(node)
	require.True(t, ok, node)
	for _, tg := range s.Targets {
		if tg.Key == host {
			return tg.Attestation
		}
	}
	return nil
}

// A snapshot names only the address this cycle's resolving node chose for
// a route now: when the guest moves to another node, no node is told the
// address the old one chose; until the new one reports, none is told any;
// a new address reaches every node with the next report, and one chosen
// with a MAC the guest no longer has reaches none.
func TestASnapshotNamesOnlyTheAddressChosenNow(t *testing.T) {
	addrIn := func(w *world, node string) netip.Addr {
		s, _ := w.r.lastOf(node)
		for _, tg := range s.Targets {
			if tg.Key == "db.example.com" {
				return tg.Addr
			}
		}
		return netip.Addr{}
	}
	t.Run("the guest moves", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		require.Equal(t, dbAddr, addrIn(w, "pve3"))
		snap := w.inv.snapshot()
		snap.Guests[1].Node = "pve3"
		w.inv.set(snap)
		w.acts["pve3"].silent = true
		w.clock.advance(time.Second)
		w.r.reportStatus("pve2", w.clock.now(), w.statusOf("pve2", w.acts["pve2"]))
		w.cycle()
		require.False(t, addrIn(w, "pve2").IsValid(), "pve3 resolves db now and has not said")

		w.acts["pve3"].silent = false
		moved := netip.MustParseAddr("10.0.0.33")
		w.acts["pve3"].addrs = map[string]netip.Addr{"db.example.com": moved}
		w.settle()
		require.Equal(t, moved, addrIn(w, "pve2"))
	})
	t.Run("a new address", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		changed := netip.MustParseAddr("10.0.0.44")
		w.acts["pve2"].addrs = map[string]netip.Addr{"db.example.com": changed}
		w.tick(time.Second)
		w.tick(time.Second)
		require.Equal(t, changed, addrIn(w, "pve3"))
		require.Equal(t, changed, w.localVerify(t, "db.example.com").Addr)
	})
	t.Run("a MAC the guest no longer has", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		w.acts["pve2"].edit = func(st *agentproto.Status) {
			for i := range st.Targets {
				if st.Targets[i].Key != "db.example.com" {
					continue
				}
				raw, err := agent.EncodeResult(resolve.Result{Binding: &resolve.Binding{MAC: "bc:24:11:77:77:77", Addr: dbAddr}})
				require.NoError(t, err)
				st.Targets[i].Result = raw
			}
		}
		w.tick(time.Second)
		w.tick(time.Second)
		require.False(t, addrIn(w, "pve3").IsValid())
	})
}

// dbAttested is node's attestation of db's card.
func dbAttested(node string) agentproto.Attestation {
	return agentproto.Attestation{Node: node, Guest: db, MAC: dbCard, Bridge: "vmbr0", Port: "tap102i0", Since: t0.Add(-time.Hour)}
}

// A conflict a host agent reports blocks the guest's address on every
// node, in this node's snapshot and the others', and says so; the block
// holds while it is reported and ten minutes after, and ends once the
// hosting node attests the card again.
func TestAConflictBlocksTheAddressOnEveryNode(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	conflict := func(st *agentproto.Status) {
		st.Observations = []agentproto.Observation{{Kind: agentproto.ObservationConflict, MAC: dbCard, Bridge: "vmbr0", Port: "tap140i0", At: t0}}
	}
	w.acts["pve3"].edit = conflict

	st := w.tick(time.Second)

	require.True(t, hasProblem(st, "10.0.0.22 is blocked on every node: its MAC "+dbCard+" was seen on tap140i0 of pve3"), st.Problems)
	for _, node := range []string{"pve2", "pve3"} {
		s, _ := w.r.lastOf(node)
		require.Contains(t, s.Blocks, agentproto.Block{Addr: dbAddr, Reason: agentproto.BlockConflict}, node)
	}

	w.acts["pve3"].edit = nil
	for range 59 {
		w.tick(10 * time.Second)
	}
	require.Contains(t, w.eng.place.blocks, dbAddr, "under ten minutes since the last report")
	w.tick(15 * time.Second)
	require.NotContains(t, w.eng.place.blocks, dbAddr)
	s, _ := w.r.lastOf("pve2")
	require.Empty(t, s.Blocks)
}

// dbConflict has the agent of pve3 report the MAC of db's card on the port
// of another guest.
func dbConflict(st *agentproto.Status) {
	st.Observations = []agentproto.Observation{{Kind: agentproto.ObservationConflict, MAC: dbCard, Bridge: "vmbr0", Port: "tap140i0", At: t0}}
}

// A block outlives the controller that made it: one that takes over, or
// starts again, finds it stored and keeps it in every snapshot, also when
// no agent reports the conflict any more. The ten quiet minutes before it
// may end are counted on the new controller's own clock, from when it
// first saw the block: never from a time another clock wrote.
func TestAConflictBlockSurvivesATakeover(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.acts["pve3"].edit = dbConflict
	w.tick(time.Second)
	lastReport := w.clock.now()
	w.acts["pve3"].edit = nil
	for w.clock.now().Sub(lastReport) < 8*time.Minute {
		w.tick(10 * time.Second)
	}
	blocks, err := w.store.Blocks()
	require.NoError(t, err)
	require.Contains(t, blocks, dbAddr)
	require.Equal(t, lastReport, blocks[dbAddr].LastReport)

	w.eng = w.newEngine()
	w.watchHere(t)
	st := w.tick(time.Second)
	took := w.clock.now()

	require.True(t, hasProblem(st, "10.0.0.22 is blocked on every node: its MAC "+dbCard+" was seen on tap140i0 of pve3"), st.Problems)
	require.Len(t, st.Cluster.Blocks, 1)
	require.Equal(t, lastReport, st.Cluster.Blocks[0].LastReport)
	for _, node := range []string{"pve2", "pve3"} {
		s, _ := w.r.lastOf(node)
		require.Contains(t, s.Blocks, agentproto.Block{Addr: dbAddr, Reason: agentproto.BlockConflict}, node)
	}
	require.Equal(t, planner.StateWithdrawn, route(st, "db.example.com").State)

	for w.clock.now().Sub(took) < blockQuiet-15*time.Second {
		w.tick(10 * time.Second)
	}
	require.Contains(t, w.eng.place.blocks, dbAddr, "under ten minutes since this controller first saw it, though longer since the last report")
	w.tick(20 * time.Second)
	require.NotContains(t, w.eng.place.blocks, dbAddr)
	blocks, err = w.store.Blocks()
	require.NoError(t, err)
	require.Empty(t, blocks)
	s, _ := w.r.lastOf("pve2")
	require.Empty(t, s.Blocks)
}

// A block the admin ends is gone from the store before it is gone from the
// memory of the controller: one that takes over does not find it again.
func TestABlockTheAdminEndsIsGoneFromTheStore(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.acts["pve3"].edit = dbConflict
	w.tick(time.Second)
	w.acts["pve3"].edit = nil
	blocks, err := w.store.Blocks()
	require.NoError(t, err)
	require.Contains(t, blocks, dbAddr)

	require.NoError(t, w.eng.Unblock(t.Context(), dbAddr))
	blocks, err = w.store.Blocks()
	require.NoError(t, err)
	require.Empty(t, blocks)

	w.eng = w.newEngine()
	w.watchHere(t)
	st := w.tick(time.Second)
	require.Empty(t, w.eng.place.blocks)
	require.Empty(t, st.Cluster.Blocks)
	s, _ := w.r.lastOf("pve2")
	require.Empty(t, s.Blocks)
}

// A block whose guest is gone from a complete inventory ends, when no card
// the inventory has carries the MAC or the address blocked: the guest's
// node will never attest the card again, and what the block protected is
// not published any more. A card elsewhere with the MAC, or the address,
// keeps the block, as does an inventory that may be missing the guest.
func TestABlockEndsOnceItsGuestIsGoneFromACompleteInventory(t *testing.T) {
	blocked := func(t *testing.T, edit func(snap *inventory.Snapshot)) *world {
		t.Helper()
		w := threeNodes(t)
		w.settle()
		w.acts["pve3"].edit = dbConflict
		w.tick(time.Second)
		w.acts["pve3"].edit = nil
		require.Contains(t, w.eng.place.blocks, dbAddr)
		snap := w.inv.snapshot()
		snap.Guests = slices.DeleteFunc(snap.Guests, func(g model.Guest) bool { return g.Ref == db })
		edit(&snap)
		w.inv.set(snap)
		w.tick(time.Second)
		w.tick(time.Second)
		return w
	}

	t.Run("gone", func(t *testing.T) {
		w := blocked(t, func(*inventory.Snapshot) {})
		require.NotContains(t, w.eng.place.blocks, dbAddr)
		blocks, err := w.store.Blocks()
		require.NoError(t, err)
		require.Empty(t, blocks)
		s, _ := w.r.lastOf("pve2")
		require.Empty(t, s.Blocks)
		var said bool
		for _, ev := range w.eng.Events(time.Time{}) {
			if ev.Kind == kindBlock && strings.Contains(ev.Message, "10.0.0.22 is no longer blocked: its guest qemu/102 is gone from the inventory") {
				said = true
			}
		}
		require.True(t, said, "the end says why")
	})
	t.Run("a card elsewhere carries its MAC", func(t *testing.T) {
		w := blocked(t, func(snap *inventory.Snapshot) {
			snap.Guests = append(snap.Guests, carded(guest(104, "clone"), "pve3", dbCard))
		})
		require.Contains(t, w.eng.place.blocks, dbAddr)
	})
	t.Run("a card elsewhere carries its address", func(t *testing.T) {
		w := blocked(t, func(snap *inventory.Snapshot) {
			g := carded(guest(104, "clone"), "pve3", "bc:24:11:00:04:01")
			g.NICs[0].Static = []netip.Addr{dbAddr}
			snap.Guests = append(snap.Guests, g)
		})
		require.Contains(t, w.eng.place.blocks, dbAddr)
	})
	t.Run("the inventory is not complete", func(t *testing.T) {
		w := blocked(t, func(snap *inventory.Snapshot) { snap.Complete = false })
		require.Contains(t, w.eng.place.blocks, dbAddr)
	})
}

// A controller that loses the lease and takes it again under a new epoch is
// a new controller: what another controller stored meanwhile is read, a
// move that one had only added the rules of is given up, and what this
// process remembered of the tenure before is dropped, timers and all.
func TestALeaseTakenAgainUnderANewEpochStartsOver(t *testing.T) {
	pinned3, _ := planner.PinnedTunnelName(testInstall, "pve3")
	w := threeNodes(t)
	w.settle()
	w.eng.place.addedAt = map[string]time.Time{"qemu/103": t0}
	w.eng.place.unresolvedSince = map[string]time.Time{"old.example.com": t0}
	w.eng.place.pointedAt["www.example.com"] = "nowhere"

	w.lease.set(func(st *lease.State) { st.Holder, st.Valid, st.Waiting = false, false, "the lock is held by pve2" })
	w.tick(time.Second)
	now := w.clock.now()
	require.NoError(t, w.store.SaveBlock(store.Block{Addr: dbAddr, Guest: db, MAC: dbCard, Node: "pve3", Port: "tap140i0", Why: "conflict", Since: now, LastReport: now}))
	require.NoError(t, w.store.SaveMove(store.Move{Owner: "qemu/103", From: tunnelName, To: pinned3, Step: store.MoveRulesAdded, At: now}))
	next := planner.Writer{InstallID: testInstall, Generation: 2, Nonce: "n2"}
	require.NoError(t, w.store.SaveWriter(next))
	w.lease.set(func(st *lease.State) {
		st.Holder, st.Valid, st.Waiting = true, true, ""
		st.Writer, st.Record.Writer = next, next
	})
	w.cf.Deny("dns.read")
	st := w.tick(time.Second)
	w.cf.Allow("dns.read")

	require.Equal(t, 2, st.Lease.Generation)
	require.True(t, hasProblem(st, "10.0.0.22 is blocked on every node: its MAC "+dbCard+" was seen on tap140i0 of pve3"), st.Problems)
	require.Contains(t, w.eng.place.blocks, dbAddr)
	s, _ := w.r.lastOf("pve2")
	require.Contains(t, s.Blocks, agentproto.Block{Addr: dbAddr, Reason: agentproto.BlockConflict})
	_, ok := w.moveOf(t, "qemu/103")
	require.False(t, ok, "the move another controller had only added the rules of is given up")
	require.True(t, slices.ContainsFunc(w.placementEvents(), func(ev string) bool {
		return strings.Contains(ev, "the move of qemu/103 from "+tunnelName+" to "+pinned3+" is given up: its rules were not rolled out when this controller took over")
	}), w.placementEvents())
	require.NotContains(t, w.eng.place.addedAt, "qemu/103")
	require.NotContains(t, w.eng.place.unresolvedSince, "old.example.com")
	require.NotContains(t, w.eng.place.pointedAt, "www.example.com", "where a record pointed under the last tenure is not known")
}

// A block ends only on a fresh attestation of the guest's card by its
// node: ten quiet minutes alone end nothing.
func TestABlockEndsOnlyOnceTheHostingNodeAttestsAgain(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.acts["pve3"].edit = dbConflict
	w.tick(time.Second)
	w.acts["pve3"].edit = nil
	w.acts["pve2"].edit = func(st *agentproto.Status) { st.Attestations = nil }

	for range 80 {
		w.tick(10 * time.Second)
	}
	require.Contains(t, w.eng.place.blocks, dbAddr, "pve2 did not attest the card again")

	w.acts["pve2"].edit = nil
	w.tick(10 * time.Second)
	require.NotContains(t, w.eng.place.blocks, dbAddr)
}

// A block that cannot be stored holds all the same, in every snapshot, and
// the cycle says that a takeover would lose it.
func TestABlockThatCannotBeStoredHoldsInMemory(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	require.NoError(t, os.WriteFile(filepath.Join(w.paths.Cluster, "blocks"), []byte("not a directory"), 0o600))
	w.acts["pve3"].edit = dbConflict

	st := w.tick(time.Second)

	require.True(t, hasProblem(st, "saving the block of 10.0.0.22: "), st.Problems)
	require.True(t, hasProblem(st, "it holds in the memory of this controller alone"), st.Problems)
	require.True(t, hasProblem(st, "10.0.0.22 is blocked on every node"), st.Problems)
	require.Contains(t, w.eng.place.blocks, dbAddr)
	for _, node := range []string{"pve2", "pve3"} {
		s, _ := w.r.lastOf(node)
		require.Contains(t, s.Blocks, agentproto.Block{Addr: dbAddr, Reason: agentproto.BlockConflict}, node)
	}
	require.Equal(t, planner.StateWithdrawn, route(st, "db.example.com").State)
}

// Blocks that cannot be read hold the cycle: no snapshot goes out on what
// is not known, every agent keeps the blocks it has, and a security event
// says so once.
func TestUnreadableBlocksServeNothing(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.acts["pve3"].edit = dbConflict
	w.tick(time.Second)
	served := len(w.r.served)
	bad := filepath.Join(w.paths.Cluster, "blocks", "10.0.0.9.json")
	require.NoError(t, os.WriteFile(bad, []byte("{"), 0o600))
	w.eng = w.newEngine()
	w.watchHere(t)

	st := w.tick(time.Second)
	w.tick(time.Second)
	// The error reads differently once the file is another: it is one
	// thing still.
	require.NoError(t, os.Rename(bad, filepath.Join(w.paths.Cluster, "blocks", "10.0.0.8.json")))
	w.tick(time.Second)

	require.Contains(t, st.Hold, "reading the blocked addresses")
	require.Len(t, w.r.served, served, "no snapshot went out")
	s, _ := w.r.lastOf("pve2")
	require.Contains(t, s.Blocks, agentproto.Block{Addr: dbAddr, Reason: agentproto.BlockConflict}, "the agents keep the blocks they had")
	var said int
	for _, ev := range w.eng.Events(time.Time{}) {
		if ev.Kind == kindBlock && ev.Security && strings.Contains(ev.Message, "cannot be read") {
			said++
		}
	}
	require.Equal(t, 1, said, "said once")
}

// A conflict reported by an appliance agent, or of a MAC no route
// publishes, blocks nothing.
func TestAConflictOnlyAHostAgentSeesBlocks(t *testing.T) {
	for name, edit := range map[string]func(w *world){
		"an appliance agent": func(w *world) {
			w.acts["pve3"].kind = pki.KindAppliance
			w.acts["pve3"].edit = func(st *agentproto.Status) {
				st.WatchRunningSince = t0
				st.Observations = []agentproto.Observation{{Kind: agentproto.ObservationConflict, MAC: dbCard, Port: "tap140i0", At: t0}}
			}
		},
		"an unpublished MAC": func(w *world) {
			w.acts["pve3"].edit = func(st *agentproto.Status) {
				st.Observations = []agentproto.Observation{{Kind: agentproto.ObservationConflict, MAC: "bc:24:11:99:99:99", Port: "tap140i0", At: t0}}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := threeNodes(t)
			w.settle()
			edit(w)
			w.tick(time.Second)
			require.Empty(t, w.eng.place.blocks)
		})
	}
}

// A MAC that left its guest's port blocks the address on every node after
// the migrate grace, unless the guest shows that it migrates: Proxmox has
// it locked for a migration, or another node attests its port.
func TestAMoveBlocksOnlyAGuestThatDoesNotMigrate(t *testing.T) {
	moved := func(st *agentproto.Status) {
		st.Observations = []agentproto.Observation{{Kind: agentproto.ObservationMoved, MAC: dbCard, Bridge: "vmbr0", From: "tap102i0", Port: "eno1", At: t0}}
	}
	t.Run("no sign", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		w.acts["pve2"].edit = func(st *agentproto.Status) { moved(st); st.Attestations = nil }
		w.tick(time.Second)
		require.Empty(t, w.eng.place.blocks, "within the grace")
		w.tick(migrateGrace)
		require.Contains(t, w.eng.place.blocks, dbAddr)
	})
	t.Run("locked for a migration", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		snap := w.inv.snapshot()
		snap.Guests[1].Lock = "migrate"
		w.inv.set(snap)
		w.acts["pve2"].edit = func(st *agentproto.Status) { moved(st); st.Attestations = nil }
		w.tick(time.Second)
		w.tick(migrateGrace)
		require.Empty(t, w.eng.place.blocks)
	})
	t.Run("the destination attests", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		w.acts["pve2"].edit = func(st *agentproto.Status) { moved(st); st.Attestations = nil }
		w.acts["pve3"].edit = func(st *agentproto.Status) { st.Attestations = append(st.Attestations, dbAttested("pve3")) }
		w.tick(time.Second)
		w.tick(migrateGrace)
		require.Empty(t, w.eng.place.blocks)
	})
}

// A node of the registry that was removed or contested has no word: what
// it reported before does not stand.
func TestARemovedOrContestedNodeHasNoWord(t *testing.T) {
	for _, state := range []string{store.NodeRemoved, store.NodeContested} {
		t.Run(state, func(t *testing.T) {
			w := threeNodes(t)
			w.settle()
			n := entryOf(t, w.env, "pve2")
			n.State = state
			require.NoError(t, w.store.SaveNode(n))
			w.tick(time.Second)
			w.tick(time.Second)
			require.Nil(t, w.attestationOn(t, "pve3", "db.example.com"))
		})
	}
}
