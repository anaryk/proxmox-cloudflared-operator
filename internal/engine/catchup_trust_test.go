package engine

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// held is the line of a cycle that allocates nothing until the store is
// checked against every node.
const held = "no service address and no managed card is made until the store is checked against every node: "

// poolNext is the next address the pool of service addresses gives.
func poolNext(t *testing.T, w *world) netip.Addr {
	t.Helper()
	next, _, _, err := w.store.ServicePool()
	require.NoError(t, err)
	return next
}

// What an agent reports moves the pool only as far as its word is worth: an
// address of a node that is not admitted, of a report that names another
// node, or far past the pool's next, moves it not at all, so that no node can
// move the pool on its own word. Such a report is a problem line and an
// event, and holds nothing: a report that is not taken is proof that it is
// wrong, not of an address in use.
func TestAnAgentsReportMovesThePoolOnlyAsFarAsItIsTaken(t *testing.T) {
	t.Run("a node that is not admitted", func(t *testing.T) {
		w := threeNodes(t)
		n := entryOf(t, w.env, "pve2")
		n.State, n.StateSince = store.NodeContested, w.clock.now()
		require.NoError(t, w.store.SaveNode(n))
		w.acts["pve2"].edit = func(st *agentproto.Status) { st.ServiceAddrs = []netip.Addr{netip.MustParseAddr("198.18.1.5")} }

		st := w.tick(time.Second)

		require.Equal(t, netip.MustParseAddr("198.18.1.0"), poolNext(t, w))
		require.Contains(t, st.Problems, "node pve2 reports that its gateway maps 198.18.1.5, not taken from it: the node is not admitted")
		require.False(t, hasProblem(st, held), st.Problems)
	})

	t.Run("an address far past the pool", func(t *testing.T) {
		w := threeNodes(t)
		w.mappings(t, 4)
		w.acts["pve2"].edit = func(st *agentproto.Status) {
			st.ServiceAddrs = []netip.Addr{netip.MustParseAddr("198.18.255.254"), netip.MustParseAddr("198.18.1.5")}
		}

		st := w.tick(time.Second)

		require.Equal(t, netip.MustParseAddr("198.18.1.6"), poolNext(t, w), "the near one is taken, the far one not")
		require.Contains(t, st.Problems, "node pve2 reports that its gateway maps 198.18.255.254, not taken from it: "+
			"an agent's word moves the pool at most 4 past 198.18.1.4, as many addresses as the store has mappings")
		require.False(t, hasProblem(st, held), st.Problems)
	})

	t.Run("a report that names another node", func(t *testing.T) {
		w := threeNodes(t)
		w.acts["pve2"].edit = func(st *agentproto.Status) {
			st.Node = "pve3"
			st.ServiceAddrs = []netip.Addr{netip.MustParseAddr("198.18.1.5")}
		}

		st := w.tick(time.Second)

		require.Equal(t, netip.MustParseAddr("198.18.1.0"), poolNext(t, w))
		require.Contains(t, st.Problems, "node pve2 reports that its gateway maps 198.18.1.5, not taken from it: its report names node pve3")
	})

	t.Run("an address below the pool says nothing", func(t *testing.T) {
		w := threeNodes(t)
		require.NoError(t, w.store.SaveServicePool(netip.MustParseAddr("198.18.1.10")))
		n := entryOf(t, w.env, "pve2")
		n.State, n.StateSince = store.NodeContested, w.clock.now()
		require.NoError(t, w.store.SaveNode(n))
		w.acts["pve2"].edit = func(st *agentproto.Status) { st.ServiceAddrs = []netip.Addr{netip.MustParseAddr("198.18.1.5")} }

		st := w.tick(time.Second)

		require.False(t, hasProblem(st, "198.18.1.5"), st.Problems)
	})

	t.Run("a report that is not fresh", func(t *testing.T) {
		w := threeNodes(t)
		w.acts["pve2"].silent = true
		late := w.statusOf("pve2", w.acts["pve2"])
		late.ServiceAddrs = []netip.Addr{netip.MustParseAddr("198.18.1.5")}
		w.r.reportStatus("pve2", w.clock.now().Add(-time.Minute), late)

		w.tick(time.Second)

		require.Equal(t, netip.MustParseAddr("198.18.1.0"), poolNext(t, w))
	})
}

// A report that is not taken does not keep the cycle from making a service
// address: it names an address no gateway can hold.
func TestARefusedReportHoldsNoAllocation(t *testing.T) {
	w, _ := legsWorld(t, func(w *world) {
		w.attach(t, "vmbr1", 1, store.ScopeCluster, "10.0.0.0/24", map[string]string{"pve1": "10.0.0.250", "pve2": "10.0.0.251", "pve3": "10.0.0.252"})
		w.cardOn(102, "vmbr1")
		w.res.goVia("db.example.com", "vmbr1")
		for _, n := range []string{"pve2", "pve3"} {
			w.acts[n].legs = map[string]string{"db.example.com": "vmbr1"}
		}
		w.acts["pve3"].edit = func(st *agentproto.Status) { st.ServiceAddrs = []netip.Addr{netip.MustParseAddr("198.18.255.254")} }
	})

	st := w.runFor(time.Minute)

	all, err := w.store.ServiceAddrs()
	require.NoError(t, err)
	require.Len(t, all, 1, "the service address is made")
	require.True(t, hasProblem(st, "node pve3 reports that its gateway maps 198.18.255.254, not taken from it"), st.Problems)
	require.False(t, hasProblem(st, held), st.Problems)
	require.Equal(t, 1, w.eventsStarting("node pve3 reports that its gateway maps"), "said once in the epoch")
}

// A node whose report names many addresses that are not taken is one line,
// with how many.
func TestARefusedReportIsOneLinePerNode(t *testing.T) {
	w := threeNodes(t)
	n := entryOf(t, w.env, "pve2")
	n.State, n.StateSince = store.NodeContested, w.clock.now()
	require.NoError(t, w.store.SaveNode(n))
	w.acts["pve2"].edit = func(st *agentproto.Status) {
		st.ServiceAddrs = []netip.Addr{netip.MustParseAddr("198.18.1.7"), netip.MustParseAddr("198.18.1.5"), netip.MustParseAddr("198.18.1.6")}
	}

	st := w.tick(time.Second)

	var lines []string
	for _, p := range st.Problems {
		if strings.HasPrefix(p, "node pve2 reports") {
			lines = append(lines, p)
		}
	}
	require.Equal(t, []string{"node pve2 reports that its gateway maps 198.18.1.5 and 2 more, not taken from it: the node is not admitted"}, lines)
}

// eventsStarting counts the engine's events whose message begins with prefix.
func (w *world) eventsStarting(prefix string) int {
	n := 0
	for _, m := range w.eventMessages() {
		if strings.HasPrefix(m, prefix) {
			n++
		}
	}
	return n
}

// The pool moves at any time for an address the controller can vouch for, one
// a configuration verified at Cloudflare sends a connector to; on an agent's
// word alone it moves once in an epoch, when every node has reported, and no
// further than the store has mappings. However far it moves, it moves at most
// maxCatchUp in an epoch; what is past that waits, and nothing is allocated.
func TestThePoolMovesOnAnAgentsWordOncePerEpoch(t *testing.T) {
	t.Run("once", func(t *testing.T) {
		w := threeNodes(t)
		w.mappings(t, 2)
		w.tick(time.Second)
		w.acts["pve2"].edit = func(st *agentproto.Status) { st.ServiceAddrs = []netip.Addr{netip.MustParseAddr("198.18.1.3")} }

		st := w.tick(time.Second)

		require.Equal(t, netip.MustParseAddr("198.18.1.2"), poolNext(t, w))
		require.Contains(t, st.Problems, "node pve2 reports that its gateway maps 198.18.1.3, not taken from it: "+
			"the pool moved on an agent's word once in this epoch already, and neither the store nor a configuration verified at Cloudflare has it")
		require.False(t, hasProblem(st, held), st.Problems)
	})

	t.Run("a configuration verified at Cloudflare at any time", func(t *testing.T) {
		w := threeNodes(t)
		w.tick(time.Second)
		w.eng.verified[tunnelRef{testAccount, tunnelName}] = []egress.Target{{Addr: netip.MustParseAddr("198.18.1.9"), Port: 8080}}

		w.tick(time.Second)

		require.Equal(t, netip.MustParseAddr("198.18.1.10"), poolNext(t, w))
	})

	t.Run("at most maxCatchUp in an epoch", func(t *testing.T) {
		w := threeNodes(t)
		w.eng.verified[tunnelRef{testAccount, tunnelName}] = []egress.Target{{Addr: netip.MustParseAddr("198.18.9.0"), Port: 8080}}

		st := w.tick(time.Second)

		require.Equal(t, netip.MustParseAddr("198.18.5.0"), poolNext(t, w), "1024 past 198.18.1.0")
		require.Contains(t, st.Problems, held+"the pool of service addresses would move on more than 1024 in this epoch, to 198.18.9.1")
	})
}

// A controller that takes over allocates nothing until every node the
// registry serves has reported: a silent node may map an address the store
// lost. Nor while the pool cannot be moved past what the nodes hold.
func TestNothingIsAllocatedUntilTheStoreIsCheckedAgainstEveryNode(t *testing.T) {
	t.Run("a silent node", func(t *testing.T) {
		w, _ := legsWorld(t, func(w *world) {
			w.attach(t, "vmbr1", 1, store.ScopeCluster, "10.0.0.0/24", map[string]string{"pve1": "10.0.0.250", "pve2": "10.0.0.251", "pve3": "10.0.0.252"})
			w.cardOn(102, "vmbr1")
			w.res.goVia("db.example.com", "vmbr1")
			w.acts["pve2"].legs = map[string]string{"db.example.com": "vmbr1"}
			w.acts["pve3"].silent = true
		})

		var st State
		for range 14 {
			st = w.tick(5 * time.Second)
		}

		require.Contains(t, st.Problems, held+"node pve3 has not reported to this controller yet")
		all, err := w.store.ServiceAddrs()
		require.NoError(t, err)
		require.Empty(t, all, "no service address is made while pve3 is silent")

		w.acts["pve3"].silent = false
		w.acts["pve3"].legs = map[string]string{"db.example.com": "vmbr1"}
		for range 4 {
			st = w.tick(5 * time.Second)
		}
		require.False(t, hasProblem(st, held), st.Problems)
		all, err = w.store.ServiceAddrs()
		require.NoError(t, err)
		require.Len(t, all, 1, "made once every node reported")
	})

	t.Run("a pool that cannot be moved", func(t *testing.T) {
		w := threeNodes(t)
		w.eng.verified[tunnelRef{testAccount, tunnelName}] = []egress.Target{{Addr: netip.MustParseAddr("198.18.1.5"), Port: 8080}}
		meta := filepath.Join(w.paths.Cluster, "meta")
		require.NoError(t, os.MkdirAll(meta, 0o700))
		require.NoError(t, os.Chmod(meta, 0o500))
		t.Cleanup(func() { _ = os.Chmod(meta, 0o700) })

		st := w.tick(time.Second)

		require.True(t, hasProblem(st, held+"moving the pool of service addresses on to 198.18.1.6: "), st.Problems)
	})
}

// Two guests with a managed card of one MAC both claim its address: neither
// is given the lease, and that is said.
func TestTwoCardsOfOneMACGetNoLease(t *testing.T) {
	w, _ := legWorld(t)
	addr := netip.MustParseAddr("10.203.118.40")
	snap := w.inv.snapshot()
	for _, vmid := range []int{107, 108} {
		g := carded(guest(vmid, "twin"), "pve3", "bc:24:11:00:07:01")
		g.NICs = append(g.NICs, model.NIC{Index: 1, MAC: store.ManagedMAC(addr), Bridge: gateway.ManagedBridge})
		snap.Guests = append(snap.Guests, g)
	}
	w.inv.set(snap)

	w.tick(time.Second)

	leases, err := w.store.Leases()
	require.NoError(t, err)
	require.NotContains(t, leases, store.ManagedMAC(addr))
	require.Contains(t, w.eventMessages(), "the managed card 06:50:43:4f:76:28 is a card of qemu/107 and qemu/108; "+
		"its lease, missing from the store, is not saved again for any of them")
}
