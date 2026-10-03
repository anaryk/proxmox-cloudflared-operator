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
	web, nic1, stranger := "bc:24:11:00:00:01", "bc:24:11:00:00:02", "bc:24:11:ff:ff:01"
	pins := map[netip.Addr]Pin{
		addr("10.20.0.10"): {MAC: mac(t, web), Bridge: "vmbr0", Port: "tap101i0", Own: []net.HardwareAddr{mac(t, nic1)}},
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
