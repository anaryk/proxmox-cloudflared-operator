package egress

import (
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func mac(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	m, err := net.ParseMAC(s)
	require.NoError(t, err)
	return m
}

func TestWhatAMoveIs(t *testing.T) {
	web, nic1, nic2, stranger := "bc:24:11:00:00:01", "bc:24:11:00:00:02", "bc:24:11:00:00:04", "bc:24:11:ff:ff:01"
	pins := map[netip.Addr]Pin{
		addr("10.20.0.10"): {MAC: mac(t, web), Bridge: "vmbr0", Port: "tap101i0", Own: []OwnMAC{
			{MAC: mac(t, nic1), Ports: []string{"tap101i1"}},
			{MAC: mac(t, nic2)},
		}},
		addr("10.20.0.11"): {MAC: mac(t, web), Bridge: "vmbr0", Port: "tap101i0"},
		addr("10.30.0.10"): {MAC: mac(t, "bc:24:11:00:00:03")},
	}
	tests := []struct {
		name  string
		entry entry
		moved []netip.Addr
	}{
		{name: "the neighbour table gives a bound address a stranger's MAC",
			entry: entry{addr: addr("10.20.0.10"), mac: mac(t, stranger)}, moved: []netip.Addr{addr("10.20.0.10")}},
		{name: "the neighbour table gives it the pinned MAC",
			entry: entry{addr: addr("10.20.0.10"), mac: mac(t, web)}},
		{name: "the neighbour table gives it another MAC of its guest",
			entry: entry{addr: addr("10.20.0.10"), mac: mac(t, nic1)}},
		{name: "the bridge learns another MAC of the guest on another port",
			entry: entry{mac: mac(t, nic1), bridge: "vmbr0", port: "veth200i0"}, moved: []netip.Addr{addr("10.20.0.10")}},
		{name: "the bridge learns another MAC of the guest on its port",
			entry: entry{mac: mac(t, nic1), bridge: "vmbr0", port: "tap101i1"}},
		{name: "another bridge learns another MAC of the guest",
			entry: entry{mac: mac(t, nic1), bridge: "vmbr1", port: "veth200i0"}},
		{name: "a MAC of the guest without a port is not watched in the forwarding table",
			entry: entry{mac: mac(t, nic2), bridge: "vmbr0", port: "veth200i0"}},
		{name: "a pin without a port is watched in the neighbour table",
			entry: entry{addr: addr("10.30.0.10"), mac: mac(t, stranger)}, moved: []netip.Addr{addr("10.30.0.10")}},
		{name: "an address nothing is bound to",
			entry: entry{addr: addr("10.20.0.99"), mac: mac(t, stranger)}},
		{name: "a neighbour without a link-layer address",
			entry: entry{addr: addr("10.20.0.10")}},
		{name: "the bridge learns the pinned MAC on another port",
			entry: entry{mac: mac(t, web), bridge: "vmbr0", port: "veth200i0"}, moved: []netip.Addr{addr("10.20.0.10"), addr("10.20.0.11")}},
		{name: "the bridge learns it on the uplink",
			entry: entry{mac: mac(t, web), bridge: "vmbr0", port: "nic3"}, moved: []netip.Addr{addr("10.20.0.10"), addr("10.20.0.11")}},
		{name: "a port whose name is not known",
			entry: entry{mac: mac(t, web), bridge: "vmbr0"}, moved: []netip.Addr{addr("10.20.0.10"), addr("10.20.0.11")}},
		{name: "the bridge learns it on the pinned port",
			entry: entry{mac: mac(t, web), bridge: "vmbr0", port: "tap101i0"}},
		{name: "another bridge learns it",
			entry: entry{mac: mac(t, web), bridge: "vmbr1", port: "veth300i0"}},
		{name: "the bridge learns another MAC on another port",
			entry: entry{mac: mac(t, stranger), bridge: "vmbr0", port: "veth200i0"}},
		{name: "the bridge learns a MAC of a pin it does not place",
			entry: entry{mac: mac(t, "bc:24:11:00:00:03"), bridge: "vmbr0", port: "veth200i0"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.moved, tc.entry.moved(pins))
		})
	}
}

// legPins are two networks with the same subnet, each with a guest at
// 10.50.0.10, published under their service addresses, and a direct target
// on vmbr0.
func legPins(t *testing.T) map[netip.Addr]Pin {
	return map[netip.Addr]Pin{
		addr("198.18.1.0"): {MAC: mac(t, "bc:24:11:00:00:01"), Bridge: "vmbr1", Port: "tap101i0", Dev: "leg1", Real: addr("10.50.0.10")},
		addr("198.18.1.1"): {MAC: mac(t, "bc:24:11:00:00:02"), Bridge: "vmbr2", Port: "tap102i0", Dev: "leg2", Real: addr("10.50.0.10")},
		addr("10.20.0.10"): {MAC: mac(t, "bc:24:11:00:00:03"), Bridge: "vmbr0", Port: "tap103i0"},
	}
}

func TestWhatAMoveIsOnALeg(t *testing.T) {
	pins := legPins(t)
	stranger := mac(t, "bc:24:11:ff:ff:01")
	tests := []struct {
		name  string
		entry entry
		moved []netip.Addr
	}{
		{name: "a stranger answers on the first leg",
			entry: entry{addr: addr("10.50.0.10"), dev: "leg1", mac: stranger}, moved: []netip.Addr{addr("198.18.1.0")}},
		{name: "a stranger answers on the second leg",
			entry: entry{addr: addr("10.50.0.10"), dev: "leg2", mac: stranger}, moved: []netip.Addr{addr("198.18.1.1")}},
		{name: "the first guest answers on its leg",
			entry: entry{addr: addr("10.50.0.10"), dev: "leg1", mac: mac(t, "bc:24:11:00:00:01")}},
		{name: "the second guest answers on its leg, with a MAC the first has not",
			entry: entry{addr: addr("10.50.0.10"), dev: "leg2", mac: mac(t, "bc:24:11:00:00:02")}},
		{name: "the real address on a device that is no leg of a pin",
			entry: entry{addr: addr("10.50.0.10"), dev: "vmbr0", mac: stranger}},
		{name: "the service address in the neighbour table",
			entry: entry{addr: addr("198.18.1.0"), dev: "leg1", mac: stranger}},
		{name: "a direct target on its bridge, another MAC",
			entry: entry{addr: addr("10.20.0.10"), dev: "vmbr0", mac: stranger}, moved: []netip.Addr{addr("10.20.0.10")}},
		{name: "a direct target on any device, another MAC",
			entry: entry{addr: addr("10.20.0.10"), dev: "net0", mac: stranger}, moved: []netip.Addr{addr("10.20.0.10")}},
		{name: "a direct target on its bridge, its own MAC",
			entry: entry{addr: addr("10.20.0.10"), dev: "vmbr0", mac: mac(t, "bc:24:11:00:00:03")}},
		{name: "the bridge of the first leg learns its guest elsewhere",
			entry: entry{mac: mac(t, "bc:24:11:00:00:01"), bridge: "vmbr1", port: "veth200i0"}, moved: []netip.Addr{addr("198.18.1.0")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.moved, tc.entry.moved(pins))
		})
	}
}

// The neighbour entries of both guests come and go every second for a
// minute, each on its own leg with its own MAC, and nothing moves; then one
// guest's MAC changes, and only its service address moves.
func TestTwoLegsWithOneAddressDoNotFlap(t *testing.T) {
	pins := legPins(t)
	for range 60 {
		for _, e := range []entry{
			{addr: addr("10.50.0.10"), dev: "leg1", mac: mac(t, "bc:24:11:00:00:01")},
			{addr: addr("10.50.0.10"), dev: "leg2", mac: mac(t, "bc:24:11:00:00:02")},
		} {
			require.Empty(t, e.moved(pins), "%s on %s", e.mac, e.dev)
		}
	}

	moved := entry{addr: addr("10.50.0.10"), dev: "leg2", mac: mac(t, "bc:24:11:00:00:09")}.moved(pins)
	require.Equal(t, []netip.Addr{addr("198.18.1.1")}, moved)
	require.Empty(t, entry{addr: addr("10.50.0.10"), dev: "leg1", mac: mac(t, "bc:24:11:00:00:01")}.moved(pins))
}
