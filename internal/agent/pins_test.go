package agent

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// A service the set holds from a binding of an earlier pass is pinned to the
// binding's MAC only while the snapshot gives the route's guest a card of
// that MAC; a service with no MAC pinned behind it forwards nothing.
func TestAServiceForwardsOnlyToACardOfItsGuest(t *testing.T) {
	service := netip.MustParseAddr("198.18.1.5")
	s := agentproto.Snapshot{Targets: []agentproto.Target{guestTarget("www.example.com -> :8080")}}
	services := []gateway.Service{{Leg: "lan", Service: service, Addr: guestAddr, Ports: []uint16{8080}}}
	bound := func(mac string) *pass {
		return &pass{s: s, bindings: map[string]resolve.Binding{
			"www.example.com": {Via: "lan", Addr: guestAddr, Service: service, MAC: mac},
		}}
	}

	p := bound(testMAC)
	pins := p.withBindings(services, nil)
	require.Equal(t, []gateway.Pin{{Leg: "lan", Addr: guestAddr, MAC: mustMAC(testMAC)}}, pins)
	require.Equal(t, services, pinnedOnly(services, pins))

	p = bound("bc:24:11:00:00:99")
	pins = p.withBindings(services, nil)
	require.Empty(t, pins, "no card of the guest has that MAC")
	require.Empty(t, pinnedOnly(services, pins))
}
