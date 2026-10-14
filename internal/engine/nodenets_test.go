package engine

import (
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var (
	managedPrefix = netip.MustParsePrefix("10.203.118.0/24")
	mgdAddr       = netip.MustParseAddr("10.203.118.20")
	mgdRef        = model.GuestRef{Kind: model.KindQEMU, VMID: 104}
)

// legWorld is the three nodes of threeNodes with a gateway on this node and
// two networks: vmbr1, one segment with a leg on pve1 and pve2, and the
// managed network with its leg on the same two; mgd, a guest of pve2, has a
// card on it with its lease.
func legWorld(t *testing.T) (*world, *fakeGateway) {
	t.Helper()
	w := threeNodes(t)
	gw := &fakeGateway{log: &callLog{}, down: map[string]string{}}
	w.gateway = gw
	indexes := map[string]int{"pve1": 0, "pve2": 1, "pve3": 2}
	require.NoError(t, w.store.SaveNetworkIndexed(store.Network{
		ID: "vmbr1", Kind: store.NetworkAttach, Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.50.0.0/24"),
		Legs:   map[string]netip.Addr{"pve1": netip.MustParseAddr("10.50.0.250"), "pve2": netip.MustParseAddr("10.50.0.251")},
		LegMAC: store.LegMACFor(store.NetworkAttach, 1), Mark: 1, CreatedAt: t0,
	}, 0, indexes))
	require.NoError(t, w.store.SaveNetworkIndexed(store.Network{
		ID: gateway.ManagedBridge, Kind: store.NetworkManaged, Bridge: gateway.ManagedBridge, Prefix: managedPrefix,
		Legs:   map[string]netip.Addr{"pve1": managedPrefix.Addr().Next(), "pve2": managedPrefix.Addr().Next()},
		LegMAC: store.LegMACFor(store.NetworkManaged, 2), Mark: 2, Scope: store.ScopeNode, CreatedAt: t0,
	}, 0, indexes))
	mac := store.ManagedMAC(mgdAddr)
	require.NoError(t, w.store.SaveLease(store.Lease{MAC: mac, Addr: mgdAddr, Owner: mgdRef.String(), Identity: "uuid:104", NIC: 1, Since: t0}, 0))
	require.NoError(t, w.store.SaveAttachment(store.Attachment{Owner: mgdRef.String(), Identity: "uuid:104", NIC: 1, MAC: mac, Addr: mgdAddr,
		Consent: "tag", Since: t0}))
	snap := w.inv.snapshot()
	mgd := carded(guest(104, "mgd"), "pve2", "bc:24:11:00:04:01")
	mgd.NICs = append(mgd.NICs, model.NIC{Index: 1, MAC: mac, Bridge: gateway.ManagedBridge, Leased: mgdAddr})
	snap.Guests = append(snap.Guests, mgd)
	w.inv.set(snap)
	w.eng = w.newEngine()
	w.watchHere(t)
	return w, gw
}

// networksOf are the networks of the snapshot last served to node.
func (w *world) networksOf(t *testing.T, node string) gateway.Cache {
	t.Helper()
	s, ok := w.r.lastOf(node)
	require.True(t, ok, "node %s was served no snapshot", node)
	require.NotNil(t, s.Networks, "the snapshot of %s gives no networks", node)
	var c gateway.Cache
	require.NoError(t, json.Unmarshal(s.Networks, &c))
	return c
}

// Every node is given the legs it has, each with the MAC its index makes,
// so that the legs of one uplinked network never share a MAC; the managed
// leg has its one MAC everywhere. The node of a managed guest gets the
// guest's sticky entry, and every node with the managed leg every lease, so
// that a guest that migrates renews its address from its new node.
func TestEveryNodeIsGivenItsLegsWithTheMACOfItsIndex(t *testing.T) {
	w, gw := legWorld(t)

	w.settle()

	two := w.networksOf(t, "pve2")
	require.Equal(t, []gateway.Leg{
		{ID: gateway.ManagedBridge, Mark: 2, Bridge: gateway.ManagedBridge, Prefix: netip.MustParsePrefix("10.203.118.1/24"),
			MAC: gateway.ManagedLegMAC, Managed: true},
		{ID: "vmbr1", Mark: 1, Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.50.0.251/24"), MAC: hardware(t, "02:50:43:4f:01:01")},
	}, two.Legs)
	require.Equal(t, []gateway.Pin{{Leg: gateway.ManagedBridge, Addr: mgdAddr, MAC: hardware(t, store.ManagedMAC(mgdAddr))}}, two.Pins)
	require.Equal(t, []gateway.Sticky{{MAC: hardware(t, store.ManagedMAC(mgdAddr)), Bridge: gateway.ManagedBridge, Port: "tap104i1", Addr: mgdAddr}},
		two.Sticky)

	three := w.networksOf(t, "pve3")
	require.Empty(t, three.Legs, "pve3 has no leg, and its gateway is told so")
	require.Empty(t, three.Sticky)

	local := gw.last(t)
	require.Equal(t, "02:50:43:4f:00:01", local.legs[1].MAC.String(), "this node's leg has the MAC of index 0")
	require.Empty(t, local.sticky, "mgd runs on pve2: this node holds no entry for its card")

	t.Run("a guest that migrates takes its sticky entry to its new node", func(t *testing.T) {
		snap := w.inv.snapshot()
		for i := range snap.Guests {
			if snap.Guests[i].Ref == mgdRef {
				snap.Guests[i].Node = "pve1"
			}
		}
		w.inv.set(snap)

		w.settle()

		require.Empty(t, w.networksOf(t, "pve2").Sticky)
		require.Equal(t, []gateway.Pin{{Leg: gateway.ManagedBridge, Addr: mgdAddr, MAC: hardware(t, store.ManagedMAC(mgdAddr))}},
			w.networksOf(t, "pve2").Pins, "the lease stays, and pve2 still answers it")
		require.Equal(t, "tap104i1", gw.last(t).sticky[0].Port)
		leases, err := w.store.Leases()
		require.NoError(t, err)
		require.Equal(t, mgdAddr, leases[store.ManagedMAC(mgdAddr)].Addr)
	})
}

// A node of one install keeps the MAC its legs were made with: a network
// saved before the registry numbered the node has no leg MAC by node.
func TestANodeAloneKeepsTheMACItsLegsWereMadeWith(t *testing.T) {
	e := newLegEnv(t)
	e.warm()
	e.cycle()

	require.Equal(t, store.LegMACFor(store.NetworkAttach, 1), e.gw.last(t).legs[0].MAC.String())

}
