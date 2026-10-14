package engine

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// What an agent reports moves the pool only as far as its word is worth: a
// node that is not admitted moves it not at all, and an address far past the
// pool's next is not taken, so that no node can give the pool away.
func TestAnAgentsReportMovesThePoolOnlyAsFarAsItIsTaken(t *testing.T) {
	poolNext := func(t *testing.T, w *world) netip.Addr {
		t.Helper()
		next, _, _, err := w.store.ServicePool()
		require.NoError(t, err)
		return next
	}

	t.Run("a node that is not admitted", func(t *testing.T) {
		w := threeNodes(t)
		n := entryOf(t, w.env, "pve2")
		n.State, n.StateSince = store.NodeContested, w.clock.now()
		require.NoError(t, w.store.SaveNode(n))
		w.acts["pve2"].edit = func(st *agentproto.Status) { st.ServiceAddrs = []netip.Addr{netip.MustParseAddr("198.18.1.5")} }

		w.tick(time.Second)

		require.Equal(t, netip.MustParseAddr("198.18.1.0"), poolNext(t, w))
	})

	t.Run("an address far past the pool", func(t *testing.T) {
		w := threeNodes(t)
		w.acts["pve2"].edit = func(st *agentproto.Status) {
			st.ServiceAddrs = []netip.Addr{netip.MustParseAddr("198.18.255.254"), netip.MustParseAddr("198.18.1.3")}
		}

		w.tick(time.Second)

		require.Equal(t, netip.MustParseAddr("198.18.1.4"), poolNext(t, w), "the near one is taken, the far one not")
		require.Contains(t, w.eventMessages(), "node pve2 reports that its gateway maps 198.18.255.254, more than 1024 past 198.18.1.0, "+
			"the next address of the pool; it is not taken")
	})

	t.Run("a report that names another node", func(t *testing.T) {
		w := threeNodes(t)
		w.acts["pve2"].edit = func(st *agentproto.Status) {
			st.Node = "pve3"
			st.ServiceAddrs = []netip.Addr{netip.MustParseAddr("198.18.1.5")}
		}

		w.tick(time.Second)

		require.Equal(t, netip.MustParseAddr("198.18.1.0"), poolNext(t, w))
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
