package pve

import (
	"context"
	"net/http"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeDNS(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{"/nodes/pco-test-2/dns": okReply(t, "node_dns.json")})
	got, err := c.NodeDNS(context.Background(), "pco-test-2")
	require.NoError(t, err)
	require.Equal(t, NodeDNS{Servers: []netip.Addr{netip.MustParseAddr("127.0.0.53")}}, got)
	require.Equal(t, "/api2/json/nodes/pco-test-2/dns", rec.requests()[0].uri)
}

func TestNodeDNSForms(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{"/nodes/pve1/dns": {http.StatusOK,
		`{"data":{"search":"lab.invalid","dns1":"10.20.0.1","dns2":"2001:db8::53","dns3":"1.1.1.1"}}`}})
	got, err := c.NodeDNS(context.Background(), "pve1")
	require.NoError(t, err)
	require.Equal(t, NodeDNS{Servers: []netip.Addr{netip.MustParseAddr("10.20.0.1"), netip.MustParseAddr("1.1.1.1")}}, got)

	c, _ = newTestClient(t, map[string]reply{"/nodes/pve1/dns": {http.StatusOK, `{"data":{"search":"lab.invalid"}}`}})
	got, err = c.NodeDNS(context.Background(), "pve1")
	require.NoError(t, err)
	require.Empty(t, got.Servers)

	c, _ = newTestClient(t, map[string]reply{"/nodes/pve1/dns": {http.StatusOK, `{"data":{"dns1":"resolver"}}`}})
	_, err = c.NodeDNS(context.Background(), "pve1")
	require.ErrorContains(t, err, "dns1")

	_, err = c.NodeDNS(context.Background(), "../cluster")
	require.ErrorContains(t, err, "invalid node name")
}

func TestFirewallOptions(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/cluster/firewall/options":          okReply(t, "firewall_options.json"),
		"/nodes/pve1/firewall/options":       {http.StatusOK, `{"data":{"enable":1,"log_level_in":"nolog"}}`},
		"/nodes/pve2/firewall/options":       {http.StatusOK, `{"data":{"enable":0}}`},
		"/nodes/pve3/firewall/options":       {http.StatusOK, `{"data":{"enable":"maybe"}}`},
		"/nodes/pco-test-2/firewall/options": okReply(t, "firewall_options.json"),
	})
	ctx := context.Background()

	dc, err := c.DatacenterFirewall(ctx)
	require.NoError(t, err)
	require.Equal(t, FirewallOptions{}, dc, "a firewall never configured is off")
	require.Equal(t, "/api2/json/cluster/firewall/options", rec.requests()[0].uri)

	for node, want := range map[string]bool{"pve1": true, "pve2": false, "pco-test-2": false} {
		got, err := c.NodeFirewall(ctx, node)
		require.NoError(t, err, node)
		require.Equal(t, FirewallOptions{Enabled: want}, got, node)
	}
	_, err = c.NodeFirewall(ctx, "pve3")
	require.Error(t, err)
	_, err = c.NodeFirewall(ctx, "pve 1")
	require.ErrorContains(t, err, "invalid node name")
}

func TestNodeNetworkGateway(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{"/nodes/pve1/network": {http.StatusOK, `{"data":[
		{"iface":"vmbr0","type":"bridge","active":1,"cidr":"10.20.0.2/24","gateway":"10.20.0.1","gateway6":"fd00::1"},
		{"iface":"vmbr1","type":"bridge","active":1,"gateway":"fd00::1"}
	]}`}})
	got, err := c.NodeNetwork(context.Background(), "pve1")
	require.NoError(t, err)
	require.Equal(t, []NodeIface{
		{Name: "vmbr0", Type: "bridge", Active: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.20.0.2/24")}, Gateway: netip.MustParseAddr("10.20.0.1")},
		{Name: "vmbr1", Type: "bridge", Active: true},
	}, got)

	c, _ = newTestClient(t, map[string]reply{"/nodes/pve1/network": {http.StatusOK,
		`{"data":[{"iface":"vmbr0","type":"bridge","gateway":"10.20.0.300"}]}`}})
	_, err = c.NodeNetwork(context.Background(), "pve1")
	require.ErrorContains(t, err, "vmbr0")
}

func TestNodeNetworkOnDHCP(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{"/nodes/pco-test-2/network": okReply(t, "node_network_dhcp.json")})
	got, err := c.NodeNetwork(context.Background(), "pco-test-2")
	require.NoError(t, err)
	require.Equal(t, []NodeIface{
		{Name: "ens18", Type: "eth", Active: true},
		{Name: "vmbr0", Type: "bridge", Active: true, Ports: []string{"ens18"}},
		{Name: "vmbr1", Type: "bridge", Active: true},
	}, got, "an address from DHCP is in neither cidr nor gateway")
}
