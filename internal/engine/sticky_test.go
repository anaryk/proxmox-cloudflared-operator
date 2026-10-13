package engine

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// attached stores g as attached to the managed network on card 1 with the
// lease of addr, as the step of the managed network leaves a guest it
// attached, and returns g with that card.
func (m *managedEnv) attached(g model.Guest, addr string, firewall bool) model.Guest {
	m.t.Helper()
	a := netip.MustParseAddr(addr)
	mac := store.ManagedMAC(a)
	owner := g.Ref.String()
	require.NoError(m.t, m.store.SaveLease(store.Lease{MAC: mac, Addr: a, Owner: owner, Identity: g.Identity, NIC: 1, Since: t0}, 0))
	require.NoError(m.t, m.store.SaveAttachment(store.Attachment{Owner: owner, Identity: g.Identity, NIC: 1, MAC: mac, Addr: a,
		Consent: managednet.ByTag, Since: t0}))
	g = withCard(g, mac, "")
	g.NICs[len(g.NICs)-1].Firewall = firewall
	return g
}

func vm(g model.Guest) model.Guest {
	g.Ref.Kind = model.KindQEMU
	return g
}

func lxcGuest(vmid int, name string) model.Guest {
	return managedGuest(vmid, name, "www.example.com -> :8080")
}

// Every attached guest of this node gets a sticky entry of its card's MAC on
// the port the card takes on the managed bridge, running or not: the port of
// a guest that starts later gets it as soon as it appears. A guest on another
// node has none here.
func TestTheGatewayKeepsAStickyEntryPerAttachedGuest(t *testing.T) {
	m := newManagedEnv(t)
	ct := m.attached(lxcGuest(101, "web-1"), "10.203.118.2", false)
	qm := m.attached(vm(lxcGuest(102, "web-2")), "10.203.118.3", false)
	fw := m.attached(vm(lxcGuest(103, "web-3")), "10.203.118.4", true)
	fw.Running = false
	away := m.attached(lxcGuest(104, "web-4"), "10.203.118.5", false)
	away.Node = "pve2"
	m.inv.set(snapshot(ct, qm, fw, away))

	m.warm()
	m.next()

	require.Equal(t, []gateway.Sticky{
		{MAC: hardware(t, "06:50:43:4f:76:04"), Bridge: gateway.ManagedBridge, Port: "fwpr103p1"},
		{MAC: hardware(t, "06:50:43:4f:76:03"), Bridge: gateway.ManagedBridge, Port: "tap102i1"},
		{MAC: hardware(t, "06:50:43:4f:76:02"), Bridge: gateway.ManagedBridge, Port: "veth101i1"},
	}, m.gw.last(t).sticky)
}

// The namespace pins the address of every lease to its card's MAC, whether a
// route is served there or not, and whichever guest answers there now.
func TestTheGatewayPinsTheAddressOfEveryLease(t *testing.T) {
	m := newManagedEnv(t)
	ct := m.attached(lxcGuest(101, "web-1"), "10.203.118.2", false)
	quiet := m.attached(unconsented(lxcGuest(102, "quiet")), "10.203.118.3", false)
	quiet.Description = ""
	m.inv.set(snapshot(ct, quiet))

	m.warm()
	m.next()

	pins := m.gw.last(t).pins
	require.Equal(t, []gateway.Pin{
		{Leg: gateway.ManagedBridge, Addr: netip.MustParseAddr("10.203.118.2"), MAC: hardware(t, "06:50:43:4f:76:02")},
		{Leg: gateway.ManagedBridge, Addr: netip.MustParseAddr("10.203.118.3"), MAC: hardware(t, "06:50:43:4f:76:03")},
	}, pins, "one pin per lease, whether a route is served there or not")
}

// A lease that is gone takes its entry and its pin along.
func TestALeaseThatGoesTakesItsEntryAndPin(t *testing.T) {
	m := newManagedEnv(t)
	ct := m.attached(lxcGuest(101, "web-1"), "10.203.118.2", false)
	m.inv.set(snapshot(ct))
	m.warm()
	m.next()
	require.Len(t, m.gw.last(t).sticky, 1)

	require.NoError(t, m.store.DeleteAttachment("lxc/101"))
	require.NoError(t, m.store.DeleteLease("06:50:43:4f:76:02"))
	m.inv.set(snapshot(unconsented(lxcGuest(101, "web-1"))))
	m.next()
	last := m.gw.last(t)
	require.Empty(t, last.sticky)
	for _, p := range last.pins {
		require.NotEqual(t, netip.MustParseAddr("10.203.118.2"), p.Addr)
	}
}

// What pco netns up restores at boot holds the sticky entries: the ports of
// guests that start later get theirs from the daemon's port watch, which
// starts from the same cache.
func TestTheCacheOfTheGatewayHoldsTheStickyEntries(t *testing.T) {
	m := newManagedEnv(t)
	m.inv.set(snapshot(m.attached(lxcGuest(101, "web-1"), "10.203.118.2", false)))
	m.warm()
	m.next()

	c, found, err := gateway.ReadCache(m.paths.Local)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, []gateway.Sticky{{MAC: hardware(t, "06:50:43:4f:76:02"), Bridge: gateway.ManagedBridge, Port: "veth101i1"}}, c.Sticky)
	require.Contains(t, c.Pins, gateway.Pin{Leg: gateway.ManagedBridge, Addr: netip.MustParseAddr("10.203.118.2"), MAC: hardware(t, "06:50:43:4f:76:02")})
}

// In the appliance the bridge is the node's, which the container cannot
// write: no sticky entries; its own leg pins the leases all the same.
func TestTheApplianceKeepsNoStickyEntriesButPinsTheLeases(t *testing.T) {
	m := newManagedApplianceEnv(t)
	g := managedGuest(101, "web-1", "www.example.com -> :8080")
	require.NoError(t, m.store.SaveLease(store.Lease{MAC: leaseMAC2, Addr: netip.MustParseAddr("10.203.118.2"), Owner: "lxc/101", Identity: g.Identity, NIC: 1, Since: t0}, 0))
	require.NoError(t, m.store.SaveAttachment(store.Attachment{Owner: "lxc/101", Identity: g.Identity, NIC: 1, MAC: leaseMAC2,
		Addr: netip.MustParseAddr("10.203.118.2"), Consent: managednet.ByTag, Since: t0}))
	m.inv.set(snapshot(withCard(g, leaseMAC2, "10.203.118.2"), ownGuest(ownMAC)))
	m.warm()
	m.next()

	gw := m.gateway.(*fakeGateway)
	last := gw.last(t)
	require.Empty(t, last.sticky)
	require.Contains(t, last.pins, gateway.Pin{Leg: gateway.ManagedBridge, Addr: netip.MustParseAddr("10.203.118.2"), MAC: hardware(t, leaseMAC2)})
}

// A route proven through the managed leg is pinned at the MAC its proof
// found, which the lease's does not replace: a target is reached only at the
// guest its identity was proven for. A lease no route is proven at is pinned
// at its own.
func TestAProvenBindingKeepsItsPinOverTheLease(t *testing.T) {
	m := newManagedEnv(t)
	ct := m.attached(lxcGuest(101, "web-1"), "10.203.118.2", false)
	m.attached(unconsented(lxcGuest(102, "quiet")), "10.203.118.3", false)
	m.inv.set(snapshot(ct))
	m.res.goVia("www.example.com", gateway.ManagedBridge)
	m.res.moveTo("www.example.com", netip.MustParseAddr("10.203.118.2"))
	m.res.answerFrom("www.example.com", "bc:24:11:00:00:aa")

	m.warm()
	m.next()
	m.next()

	require.Equal(t, []gateway.Pin{
		{Leg: gateway.ManagedBridge, Addr: netip.MustParseAddr("10.203.118.2"), MAC: hardware(t, "bc:24:11:00:00:aa")},
		{Leg: gateway.ManagedBridge, Addr: netip.MustParseAddr("10.203.118.3"), MAC: hardware(t, "06:50:43:4f:76:03")},
	}, m.gw.last(t).pins)
}
