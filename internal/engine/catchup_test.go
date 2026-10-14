package engine

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// A store that lags behind, as one restored or one whose last writes did not
// reach this node, may say that a service address is free which an agent's
// gateway still maps: a controller that takes over moves the pool past it
// before it allocates anything, and never gives it out again.
func TestAStoreBehindAnAgentsGatewayNeverGivesItsAddressAgain(t *testing.T) {
	mapped := netip.MustParseAddr("198.18.1.5")
	w := threeNodes(t)
	w.mappings(t, 5)
	w.acts["pve2"].edit = func(st *agentproto.Status) { st.ServiceAddrs = []netip.Addr{mapped} }

	w.tick(time.Second)

	next, _, _, err := w.store.ServicePool()
	require.NoError(t, err)
	require.Equal(t, mapped.Next(), next)
	require.Contains(t, w.eventMessages(), "the pool of service addresses moved on from 198.18.1.5 to 198.18.1.6: "+
		"an agent's gateway or a configuration verified at Cloudflare holds 198.18.1.5")

	t.Run("an address of a configuration verified at Cloudflare counts too", func(t *testing.T) {
		w := threeNodes(t)
		w.eng.verified[tunnelRef{testAccount, tunnelName}] = []egress.Target{{Addr: netip.MustParseAddr("198.18.1.9"), Port: 8080}}

		w.tick(time.Second)

		next, _, _, err := w.store.ServicePool()
		require.NoError(t, err)
		require.Equal(t, netip.MustParseAddr("198.18.1.10"), next)
	})

	t.Run("a node alone changes nothing", func(t *testing.T) {
		e := newEnv(t)
		e.eng.verified[tunnelRef{testAccount, tunnelName}] = []egress.Target{{Addr: netip.MustParseAddr("198.18.1.9"), Port: 8080}}
		e.cycle()
		next, _, _, err := e.store.ServicePool()
		require.NoError(t, err)
		require.Equal(t, netip.MustParseAddr("198.18.1.0"), next)
	})
}

// A controller that takes over checks the leases against the managed cards
// the guests have: a card whose lease is missing gets it again, from its MAC,
// so that its address is never given to another card; a lease whose card is
// gone is kept, and said, never given out again.
func TestATakeoverGivesAManagedCardItsMissingLease(t *testing.T) {
	w, _ := legWorld(t)
	lost := netip.MustParseAddr("10.203.118.30")
	orphan := netip.MustParseAddr("10.203.118.31")
	snap := w.inv.snapshot()
	g := carded(guest(105, "lost"), "pve3", "bc:24:11:00:05:01")
	g.NICs = append(g.NICs, model.NIC{Index: 2, MAC: store.ManagedMAC(lost), Bridge: gateway.ManagedBridge})
	snap.Guests = append(snap.Guests, g)
	w.inv.set(snap)
	require.NoError(t, w.store.SaveLease(store.Lease{MAC: store.ManagedMAC(orphan), Addr: orphan, Owner: "qemu/106", Identity: "uuid:106", NIC: 1, Since: t0}, 0))

	w.tick(time.Second)

	leases, err := w.store.Leases()
	require.NoError(t, err)
	l, ok := leases[store.ManagedMAC(lost)]
	require.True(t, ok, "the card's lease is back")
	require.Equal(t, lost, l.Addr)
	require.Equal(t, "qemu/105", l.Owner)
	require.Equal(t, "uuid:105", l.Identity)
	require.Equal(t, 2, l.NIC)
	require.Contains(t, leases, store.ManagedMAC(orphan), "a lease whose card is gone is kept")
	require.Contains(t, w.eventMessages(), "the lease of 10.203.118.30 for net2 of qemu/105 was missing from the store and is saved again")
	require.Contains(t, w.eventMessages(), "the lease of 10.203.118.31 of qemu/106 has no card in any guest; "+
		"its address is kept and not given again until the lease is released")
}

// mappings gives the store n mappings of service addresses, from the start
// of the pool, which moves past them.
func (w *world) mappings(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		addr := netip.AddrFrom4([4]byte{10, 9, 0, byte(10 + i)})
		_, err := w.store.AllocateServiceAddr("vmbr9", addr, store.ServiceOwner{Owner: "manual/m" + addr.String()}, t0)
		require.NoError(t, err)
	}
}

// eventMessages are the messages of the engine's events.
func (w *world) eventMessages() []string {
	var out []string
	for _, ev := range w.eng.Events(time.Time{}) {
		out = append(out, ev.Message)
	}
	return out
}
