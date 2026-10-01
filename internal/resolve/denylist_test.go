package resolve

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

func TestDenylistCheck(t *testing.T) {
	d := denylist(t,
		ips("10.20.0.2", "10.20.0.3"),
		[]netip.Prefix{netip.MustParsePrefix("10.99.0.0/24"), netip.MustParsePrefix("100.64.1.7/32")},
	)
	tests := []struct {
		name   string
		addr   netip.Addr
		reason string
	}{
		{"loopback", ip("127.0.0.1"), "loopback address"},
		{"loopback range end", ip("127.255.255.254"), "loopback address"},
		{"link-local", ip("169.254.169.254"), "link-local address"},
		{"multicast", ip("224.0.0.251"), "multicast address"},
		{"multicast range end", ip("239.255.255.255"), "multicast address"},
		{"unspecified", ip("0.0.0.0"), "unspecified address"},
		{"broadcast", ip("255.255.255.255"), "broadcast address"},
		{"first node", ip("10.20.0.2"), "address of a cluster node"},
		{"second node", ip("10.20.0.3"), "address of a cluster node"},
		{"extra prefix start", ip("10.99.0.0"), "reserved by pco"},
		{"extra prefix inside", ip("10.99.0.200"), "reserved by pco"},
		{"extra single address", ip("100.64.1.7"), "reserved by pco"},
		{"IPv6", ip("fd00::1"), "not an IPv4 address"},
		{"IPv6 loopback", ip("::1"), "not an IPv4 address"},
		{"IPv4-mapped IPv6", ip("::ffff:127.0.0.1"), "not an IPv4 address"},
		{"invalid", netip.Addr{}, "not an IPv4 address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, denied := d.Check(tt.addr)
			require.True(t, denied)
			require.Equal(t, tt.reason, reason)
		})
	}
}

func TestDenylistAllowsOrdinaryAddresses(t *testing.T) {
	d := denylist(t,
		ips("10.20.0.2"),
		[]netip.Prefix{netip.MustParsePrefix("10.99.0.0/24")},
	)
	for _, s := range []string{
		"10.20.0.1", "10.20.0.3", "10.20.0.30", "10.98.255.255", "10.100.0.0",
		"192.168.1.10", "172.17.0.2", "100.64.1.8", "126.255.255.255", "128.0.0.0",
		"169.253.255.255", "169.255.0.0", "223.255.255.255", "240.0.0.1", "255.255.255.254",
	} {
		t.Run(s, func(t *testing.T) {
			reason, denied := d.Check(ip(s))
			require.False(t, denied, "reason %q", reason)
			require.Empty(t, reason)
		})
	}
}

func TestDenylistNodeAddressIsExact(t *testing.T) {
	d := denylist(t, ips("10.20.0.2"), nil)
	for _, s := range []string{"10.20.0.1", "10.20.0.3", "10.20.1.2"} {
		_, denied := d.Check(ip(s))
		require.False(t, denied, s)
	}
}

// A guest controls what its agent reports and what is written in its own
// config, so it can claim the address of the hypervisor. The address is a
// candidate like any other; the denylist is what keeps it from being served.
func TestDenylistNodeAddress(t *testing.T) {
	const node = "10.20.0.2"
	g := guestWith(
		[]model.NIC{{Index: 0, MAC: mac0, Bridge: "vmbr0", Static: ips(node, "10.20.0.10")}},
		rep(mac0, node),
		rep(mac0, "10.20.0.11"),
	)
	d := denylist(t, ips(node), nil)

	routes := []struct {
		name  string
		route model.Route
	}{
		{"discovered", routeFor(netip.Addr{}, "")},
		{"via nic", routeFor(netip.Addr{}, "net0")},
		{"via address", routeFor(netip.Addr{}, node)},
		{"explicit", routeFor(ip(node), "")},
		{"other target", routeFor(ip("10.20.0.10"), "")},
	}
	for _, tt := range routes {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Candidates(tt.route, g)
			require.NoError(t, err)
			require.NotEmpty(t, got)
			for _, c := range got {
				reason, denied := d.Check(c.Addr)
				if c.Addr == ip(node) {
					require.True(t, denied, "node address must be denied")
					require.Equal(t, "address of a cluster node", reason)
					continue
				}
				require.False(t, denied, "%s: %s", c.Addr, reason)
			}
		})
	}

	got, err := Candidates(routeFor(netip.Addr{}, ""), g)
	require.NoError(t, err)
	var addrs []netip.Addr
	for _, c := range got {
		addrs = append(addrs, c.Addr)
	}
	require.Equal(t, ips(node, "10.20.0.10", "10.20.0.11"), addrs, "candidates do not apply the denylist")
}

func TestDenylistNodeReasonBeatsExtraPrefix(t *testing.T) {
	d := denylist(t, ips("10.20.0.2"), []netip.Prefix{netip.MustParsePrefix("10.20.0.0/24")})
	reason, denied := d.Check(ip("10.20.0.2"))
	require.True(t, denied)
	require.Equal(t, "address of a cluster node", reason)
}

func TestDenylistIgnoresNonIPv4NodeAddresses(t *testing.T) {
	d := denylist(t, []netip.Addr{{}, ip("fd00::2"), ip("10.20.0.2")}, nil)
	reason, denied := d.Check(ip("10.20.0.2"))
	require.True(t, denied)
	require.Equal(t, "address of a cluster node", reason)

	_, denied = d.Check(ip("10.20.0.3"))
	require.False(t, denied)
}

func TestNewDenylistCopiesInput(t *testing.T) {
	nodes := ips("10.20.0.2")
	extra := []netip.Prefix{netip.MustParsePrefix("10.99.0.0/24")}
	d := denylist(t, nodes, extra)

	nodes[0] = ip("10.20.0.77")
	extra[0] = netip.MustParsePrefix("10.77.0.0/24")

	_, denied := d.Check(ip("10.20.0.2"))
	require.True(t, denied, "node address must survive a change to the caller's slice")
	_, denied = d.Check(ip("10.99.0.5"))
	require.True(t, denied, "prefix must survive a change to the caller's slice")
	_, denied = d.Check(ip("10.20.0.77"))
	require.False(t, denied)
	_, denied = d.Check(ip("10.77.0.5"))
	require.False(t, denied)
}

func TestZeroDenylist(t *testing.T) {
	var d Denylist
	reason, denied := d.Check(ip("127.0.0.1"))
	require.True(t, denied)
	require.Equal(t, "loopback address", reason)

	_, denied = d.Check(ip("10.20.0.2"))
	require.False(t, denied)
}

func TestNewDenylistUnmapsIPv4InIPv6(t *testing.T) {
	d := denylist(t,
		[]netip.Addr{ip("::ffff:10.20.0.2")},
		[]netip.Prefix{netip.MustParsePrefix("::ffff:10.99.0.0/120")},
	)

	reason, denied := d.Check(ip("10.20.0.2"))
	require.True(t, denied)
	require.Equal(t, "address of a cluster node", reason)

	reason, denied = d.Check(ip("10.99.0.5"))
	require.True(t, denied)
	require.Equal(t, "reserved by pco", reason)

	_, denied = d.Check(ip("10.98.0.5"))
	require.False(t, denied)
}

func TestNewDenylistRejectsPrefixesOutsideIPv4(t *testing.T) {
	tests := []struct {
		name   string
		prefix netip.Prefix
		err    string
	}{
		{"mapped prefix shorter than /96", netip.MustParsePrefix("::ffff:0:0/80"), "deny prefix ::ffff:0.0.0.0/80 reaches beyond the IPv4-mapped range"},
		{"mapped prefix of /95", netip.MustParsePrefix("::ffff:0:0/95"), "deny prefix ::ffff:0.0.0.0/95 reaches beyond the IPv4-mapped range"},
		{"IPv6 prefix", netip.MustParsePrefix("fd00::/8"), "deny prefix fd00::/8 is not an IPv4 range"},
		{"every IPv6 address", netip.MustParsePrefix("::/0"), "deny prefix ::/0 is not an IPv4 range"},
		{"zero prefix", netip.Prefix{}, "deny prefix invalid Prefix is not an IPv4 range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extra := []netip.Prefix{netip.MustParsePrefix("10.99.0.0/24"), tt.prefix}

			_, err := NewDenylist(ips("10.20.0.2"), extra)

			require.EqualError(t, err, tt.err)
		})
	}

	t.Run("whole mapped range", func(t *testing.T) {
		d := denylist(t, nil, []netip.Prefix{netip.MustParsePrefix("::ffff:0:0/96")})
		reason, denied := d.Check(ip("192.168.1.10"))
		require.True(t, denied)
		require.Equal(t, "reserved by pco", reason)
	})
}
