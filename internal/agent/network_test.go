package agent

import (
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
)

// managedNetworks is the networks of a snapshot with the managed leg and an
// attached one, the sticky entry of a card of this node's guest and the pins
// of two leases.
func managedNetworks(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(gateway.Cache{
		Legs: []gateway.Leg{
			{ID: "lan", Mark: 3, Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.0.0.250/24"), MAC: mustMAC("02:50:43:4f:01:03")},
			{ID: gateway.ManagedBridge, Mark: 1, Bridge: gateway.ManagedBridge, Prefix: netip.MustParsePrefix("10.99.0.1/24"),
				MAC: gateway.ManagedLegMAC, Managed: true},
		},
		Sticky: []gateway.Sticky{{MAC: mustMAC("06:50:43:4f:00:14"), Bridge: gateway.ManagedBridge, Port: "tap120i1",
			Addr: netip.MustParseAddr("10.99.0.20")}},
		Pins: []gateway.Pin{
			{Leg: gateway.ManagedBridge, Addr: netip.MustParseAddr("10.99.0.20"), MAC: mustMAC("06:50:43:4f:00:14")},
			{Leg: gateway.ManagedBridge, Addr: netip.MustParseAddr("10.99.0.21"), MAC: mustMAC("06:50:43:4f:00:15")},
			{Leg: "gone", Addr: netip.MustParseAddr("10.1.0.5"), MAC: mustMAC("06:50:43:4f:00:16")},
		},
	})
	require.NoError(t, err)
	return raw
}

// The controller gives a node its legs, the sticky entries of the managed
// cards of its guests and the leases of the managed network: the gateway
// gets them all, the leases pinned to their MACs so that no other card
// answers for their addresses.
func TestTheGatewayGetsTheLegsStickyEntriesAndLeasesOfTheSnapshot(t *testing.T) {
	e := newEnv(t)
	e.gw = &fakeGateway{}
	e.agent = e.newAgent()
	s := e.snapshot()
	s.Networks = managedNetworks(t)

	e.apply(s)

	got, ok := e.gw.last()
	require.True(t, ok)
	require.Len(t, got.legs, 2)
	require.Equal(t, "02:50:43:4f:01:03", got.legs[0].MAC.String(), "the leg has the MAC of this node")
	require.Equal(t, []gateway.Sticky{{MAC: mustMAC("06:50:43:4f:00:14"), Bridge: gateway.ManagedBridge, Port: "tap120i1",
		Addr: netip.MustParseAddr("10.99.0.20")}}, got.sticky)
	require.Equal(t, []gateway.Pin{
		{Leg: gateway.ManagedBridge, Addr: netip.MustParseAddr("10.99.0.20"), MAC: mustMAC("06:50:43:4f:00:14")},
		{Leg: gateway.ManagedBridge, Addr: netip.MustParseAddr("10.99.0.21"), MAC: mustMAC("06:50:43:4f:00:15")},
	}, got.pins, "a lease on a network without a leg here pins nothing")
}

// A pin the pass proved for a service is never replaced by a lease's: the
// service forwards to the card whose identity was proven for its routes.
func TestAProvenPinWinsOverALeaseAtTheSameAddress(t *testing.T) {
	e := newEnv(t)
	e.gw = &fakeGateway{}
	e.agent = e.newAgent()
	e.res.onLeg("www.example.com", "lan")
	raw, err := json.Marshal(gateway.Cache{
		Legs: []gateway.Leg{{ID: "lan", Mark: 3, Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.0.0.250/24"), MAC: mustMAC("02:50:43:4f:00:03")}},
		Pins: []gateway.Pin{{Leg: "lan", Addr: guestAddr, MAC: mustMAC("06:50:43:4f:00:99")}},
	})
	require.NoError(t, err)
	s := e.snapshot("www.example.com -> :8080")
	s.Networks = raw
	s.Targets[0].Addr, s.Targets[0].Service = guestAddr, netip.MustParseAddr("198.18.1.5")

	e.apply(s)

	got, ok := e.gw.last()
	require.True(t, ok)
	require.Equal(t, []gateway.Pin{{Leg: "lan", Addr: guestAddr, MAC: mustMAC(testMAC)}}, got.pins)
}

// The status says which service addresses the gateway of the node maps, so
// that a controller that takes over never gives one of them out again.
func TestTheStatusNamesTheServiceAddressesTheGatewayMaps(t *testing.T) {
	e := newEnv(t)
	e.gw = &fakeGateway{}
	e.agent = e.newAgent()
	e.res.onLeg("www.example.com", "lan")
	e.res.onLeg("api.example.com", "lan")
	raw, err := json.Marshal(gateway.Cache{
		Legs: []gateway.Leg{{ID: "lan", Mark: 3, Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.0.0.250/24"), MAC: mustMAC("02:50:43:4f:00:03")}},
	})
	require.NoError(t, err)
	s := e.snapshot("www.example.com -> :8080", "api.example.com -> :9090")
	s.Networks = raw
	for i := range s.Targets {
		s.Targets[i].Addr, s.Targets[i].Service = guestAddr, netip.MustParseAddr("198.18.1.7")
	}

	st := e.apply(s)

	require.Equal(t, []netip.Addr{netip.MustParseAddr("198.18.1.7")}, st.ServiceAddrs)

	t.Run("a snapshot that takes them away takes them out of the status", func(t *testing.T) {
		st := e.apply(func() agentproto.Snapshot { s := e.snapshot(); s.Networks = raw; return s }())
		require.Empty(t, st.ServiceAddrs)
	})
}
