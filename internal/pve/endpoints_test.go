package pve

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

func TestVersion(t *testing.T) {
	tests := []struct {
		name string
		body string
		want Version
	}{
		{"release", fixture(t, "version.json"), Version{Release: "9.1", Major: 9, Minor: 1}},
		{"older release", `{"data":{"release":"8.4","version":"8.4.14"}}`, Version{Release: "8.4", Major: 8, Minor: 4}},
		{"release wins over version", `{"data":{"release":"9.1","version":"9.0.3"}}`, Version{Release: "9.1", Major: 9, Minor: 1}},
		{"version only", `{"data":{"version":"9.1.1"}}`, Version{Release: "9.1", Major: 9, Minor: 1}},
		{"unparsable release falls back", `{"data":{"release":"next","version":"9.2.0"}}`, Version{Release: "9.2", Major: 9, Minor: 2}},
		{"suffix on the minor part", `{"data":{"version":"9.0~rc1"}}`, Version{Release: "9.0", Major: 9, Minor: 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, map[string]reply{"/version": {http.StatusOK, tt.body}})
			got, err := c.Version(context.Background())
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}

	for _, body := range []string{`{"data":{}}`, `{"data":null}`, `{}`, `{"data":{"release":"x","version":"y"}}`, `{"data":{"version":"9"}}`} {
		t.Run("rejects "+body, func(t *testing.T) {
			c, _ := newTestClient(t, map[string]reply{"/version": {http.StatusOK, body}})
			_, err := c.Version(context.Background())
			require.ErrorContains(t, err, "version")
		})
	}
}

func TestResources(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/cluster/resources?type=vm": okReply(t, "resources.json"),
	})
	got, err := c.Resources(context.Background())
	require.NoError(t, err)

	require.Equal(t, []Resource{
		{Kind: model.KindQEMU, VMID: 101, Name: "web-1", Node: "pve1", Status: "running", Tags: []string{"cf-tunnel", "prod"}},
		{Kind: model.KindLXC, VMID: 200, Name: "db-1", Node: "pve2", Status: "stopped"},
		{Kind: model.KindQEMU, VMID: 9000, Name: "base-image", Node: "pve1", Status: "stopped", Template: true, Tags: []string{"Template", "Base"}},
	}, got)
	require.Equal(t, "/api2/json/cluster/resources?type=vm", rec.requests()[0].uri)
}

func TestResourcesSkipsOtherTypes(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/cluster/resources?type=vm": {http.StatusOK, `{"data":[
			{"id":"storage/pve1/local","type":"storage","storage":"local","node":"pve1","status":"available"},
			{"id":"node/pve1","type":"node","node":"pve1","status":"online"},
			{"id":"qemu/101","type":"qemu","vmid":101,"name":"web-1","node":"pve1","status":"running"},
			{"id":"pool/prod","type":"pool","pool":"prod"}
		]}`},
	})
	got, err := c.Resources(context.Background())
	require.NoError(t, err)
	require.Equal(t, []Resource{
		{Kind: model.KindQEMU, VMID: 101, Name: "web-1", Node: "pve1", Status: "running"},
	}, got)
}

func TestResourcesEmpty(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{"/cluster/resources?type=vm": {http.StatusOK, `{"data":[]}`}})
	got, err := c.Resources(context.Background())
	require.NoError(t, err)
	require.Empty(t, got)

	for _, body := range []string{`{"data":null}`, `{}`} {
		t.Run("no data "+body, func(t *testing.T) {
			c, _ := newTestClient(t, map[string]reply{"/cluster/resources?type=vm": {http.StatusOK, body}})
			_, err := c.Resources(context.Background())
			require.ErrorContains(t, err, "unexpected response: no data")
		})
	}
}

func TestResourcesRejectsRowsWithoutIdentity(t *testing.T) {
	tests := []struct {
		name string
		row  string
	}{
		{"qemu without vmid", `{"type":"qemu","node":"pve1","name":"web-1"}`},
		{"lxc with zero vmid", `{"type":"lxc","vmid":0,"node":"pve1"}`},
		{"qemu with negative vmid", `{"type":"qemu","vmid":-5,"node":"pve1"}`},
		{"qemu without node", `{"type":"qemu","vmid":101,"name":"web-1"}`},
		{"lxc with empty node", `{"type":"lxc","vmid":200,"node":""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			good := `{"type":"qemu","vmid":101,"node":"pve1"}`
			c, _ := newTestClient(t, map[string]reply{
				"/cluster/resources?type=vm": {http.StatusOK, `{"data":[` + good + `,` + tt.row + `]}`},
			})
			got, err := c.Resources(context.Background())
			require.Error(t, err)
			require.Nil(t, got)
		})
	}
}

func TestResourceBooleanForms(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/cluster/resources?type=vm": {http.StatusOK, `{"data":[
			{"type":"qemu","vmid":1,"node":"pve1","template":1},
			{"type":"qemu","vmid":2,"node":"pve1","template":0},
			{"type":"qemu","vmid":3,"node":"pve1","template":true},
			{"type":"qemu","vmid":4,"node":"pve1","template":false},
			{"type":"qemu","vmid":5,"node":"pve1","template":"1"},
			{"type":"qemu","vmid":6,"node":"pve1","template":"0"},
			{"type":"qemu","vmid":7,"node":"pve1"},
			{"type":"qemu","vmid":8,"node":"pve1","template":null}
		]}`},
	})
	got, err := c.Resources(context.Background())
	require.NoError(t, err)

	templates := map[int]bool{}
	for _, r := range got {
		templates[r.VMID] = r.Template
	}
	require.Equal(t, map[int]bool{1: true, 2: false, 3: true, 4: false, 5: true, 6: false, 7: false, 8: false}, templates)
}

func TestResourceRejectsUnknownBoolean(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/cluster/resources?type=vm": {http.StatusOK, `{"data":[{"type":"qemu","vmid":1,"node":"pve1","template":"maybe"}]}`},
	})
	_, err := c.Resources(context.Background())
	require.Error(t, err)
}

func TestSplitTags(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"mixed separators", "A; b,c  d", []string{"A", "b", "c", "d"}},
		{"semicolons", "cf-tunnel;prod", []string{"cf-tunnel", "prod"}},
		{"case is kept", "CF-Tunnel;Prod", []string{"CF-Tunnel", "Prod"}},
		{"single", "Prod", []string{"Prod"}},
		{"tabs and newlines", "a\tb\nc", []string{"a", "b", "c"}},
		{"empty parts dropped", ";;a;; ,b,", []string{"a", "b"}},
		{"empty", "", nil},
		{"only separators", " ; , ", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, SplitTags(tt.in))
		})
	}
}

func TestResourceTagsDecode(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/cluster/resources?type=vm": {http.StatusOK, `{"data":[{"type":"lxc","vmid":3,"node":"pve1","tags":"A; b,c  d"}]}`},
	})
	got, err := c.Resources(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"A", "b", "c", "d"}, got[0].Tags)
}

func TestGuestConfig(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/nodes/pve1/qemu/101/config": okReply(t, "qemu_config.json"),
	})
	got, err := c.GuestConfig(context.Background(), "pve1", model.GuestRef{Kind: model.KindQEMU, VMID: 101})
	require.NoError(t, err)

	require.Equal(t, "fb4a8e8f8e7b16bb727fc538bd3b9c5244e2c57e", got.Digest)
	require.Equal(t, map[string]string{
		"cores":       "4",
		"memory":      "8192",
		"name":        "web-1",
		"description": "notes\n",
		"net0":        "virtio=BC:24:11:00:AA:B5,bridge=vmbr0,firewall=1",
		"ipconfig0":   "ip=10.20.0.15/24,gw=10.20.0.1",
		"smbios1":     "uuid=80e8ef19-34bf-4cb3-b314-96870ca14d1e",
		"meta":        "creation-qemu=10.1.2,ctime=1790847599",
		"onboot":      "0",
		"tags":        "cf-tunnel;prod",
	}, got.Values)
	require.Equal(t, "/api2/json/nodes/pve1/qemu/101/config", rec.requests()[0].uri)
}

func TestGuestConfigLXC(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/nodes/pve2/lxc/200/config": {http.StatusOK, `{"data":{"hostname":"db-1","memory":2048,"unprivileged":true,"net0":"name=eth0,bridge=vmbr0,hwaddr=BC:24:11:11:22:33,ip=dhcp,type=veth","digest":"abc123","lock":null}}`},
	})
	got, err := c.GuestConfig(context.Background(), "pve2", model.GuestRef{Kind: model.KindLXC, VMID: 200})
	require.NoError(t, err)

	require.Equal(t, "abc123", got.Digest)
	require.Equal(t, map[string]string{
		"hostname":     "db-1",
		"memory":       "2048",
		"unprivileged": "true",
		"net0":         "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:11:22:33,ip=dhcp,type=veth",
	}, got.Values)
}

func TestGuestConfigWithoutDigest(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/nodes/pve1/qemu/101/config": {http.StatusOK, `{"data":{"name":"web-1"}}`},
	})
	got, err := c.GuestConfig(context.Background(), "pve1", model.GuestRef{Kind: model.KindQEMU, VMID: 101})
	require.NoError(t, err)
	require.Empty(t, got.Digest)
	require.Equal(t, map[string]string{"name": "web-1"}, got.Values)
}

func TestGuestConfigWithoutData(t *testing.T) {
	bodies := []string{`{}`, `{"data":null}`, `{"data":{}}`}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			c, _ := newTestClient(t, map[string]reply{"/nodes/pve1/qemu/101/config": {http.StatusOK, body}})
			got, err := c.GuestConfig(context.Background(), "pve1", model.GuestRef{Kind: model.KindQEMU, VMID: 101})
			require.ErrorContains(t, err, "unexpected response")
			require.Zero(t, got)
		})
	}
}

func TestGuestConfigMissingGuest(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/nodes/pve1/qemu/999/config": {http.StatusInternalServerError, fixture(t, "config_missing.json")},
	})
	_, err := c.GuestConfig(context.Background(), "pve1", model.GuestRef{Kind: model.KindQEMU, VMID: 999})
	require.True(t, IsNotFound(err))
}

func TestPathArgumentsAreValidated(t *testing.T) {
	c, rec := newTestClient(t, nil)
	ctx := context.Background()
	qemu := model.GuestRef{Kind: model.KindQEMU, VMID: 101}

	tests := []struct {
		name string
		call func() error
	}{
		{"empty node", func() error { _, err := c.GuestConfig(ctx, "", qemu); return err }},
		{"node with a slash", func() error { _, err := c.GuestConfig(ctx, "pve1/qemu", qemu); return err }},
		{"node dot dot", func() error { _, err := c.GuestConfig(ctx, "..", qemu); return err }},
		{"node with a query", func() error { _, err := c.AgentInterfaces(ctx, "pve1?x=1", 101); return err }},
		{"node with a space", func() error { _, err := c.LXCInterfaces(ctx, "pve 1", 200); return err }},
		{"node for network", func() error { _, err := c.NodeNetwork(ctx, "../cluster"); return err }},
		{"unknown kind", func() error { _, err := c.GuestConfig(ctx, "pve1", model.GuestRef{Kind: "vm", VMID: 101}); return err }},
		{"zero vmid", func() error { _, err := c.GuestConfig(ctx, "pve1", model.GuestRef{Kind: model.KindQEMU}); return err }},
		{"negative vmid", func() error { _, err := c.AgentInterfaces(ctx, "pve1", -1); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Error(t, tt.call())
		})
	}
	require.Empty(t, rec.requests())
}

func TestAgentInterfaces(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/nodes/pve1/qemu/101/agent/network-get-interfaces": okReply(t, "agent_interfaces.json"),
	})
	got, err := c.AgentInterfaces(context.Background(), "pve1", 101)
	require.NoError(t, err)

	require.Equal(t, []GuestIface{
		{Name: "lo", MAC: "00:00:00:00:00:00", Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		{Name: "eth0", MAC: "bc:24:11:00:aa:b5", Addrs: []netip.Addr{netip.MustParseAddr("10.20.0.15")}},
	}, got)
	require.Equal(t, "/api2/json/nodes/pve1/qemu/101/agent/network-get-interfaces", rec.requests()[0].uri)
}

func TestAgentInterfacesTolerance(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/nodes/pve1/qemu/101/agent/network-get-interfaces": {http.StatusOK, `{"data":{"result":[
			{"name":"eth0","hardware-address":"BC:24:11:00:AA:B5","ip-addresses":[
				{"ip-address":"10.20.0.15","ip-address-type":"ipv4","prefix":24},
				{"ip-address":"10.20.0.16","ip-address-type":"ipv4","prefix":24},
				{"ip-address":"2001:db8::15","ip-address-type":"ipv6","prefix":64},
				{"ip-address":"not-an-address","ip-address-type":"ipv6","prefix":64},
				{"ip-address":"10.20.0.17","ip-address-type":"ipv6","prefix":24}
			]},
			{"name":"tun0","ip-addresses":[{"ip-address":"10.99.0.2","ip-address-type":"ipv4","prefix":32}]},
			{"name":"ib0","hardware-address":"80:00:02:08:fe:80:00:00:00:00:00:00:00:02:c9:03:00:12:34:56"},
			{"name":"eth1","hardware-address":"bc:24:11:00:aa:b6","ip-addresses":[{"ip-address":"fe80::1","ip-address-type":"ipv6","prefix":64}]}
		]}}`},
	})
	got, err := c.AgentInterfaces(context.Background(), "pve1", 101)
	require.NoError(t, err)

	require.Equal(t, []GuestIface{
		{Name: "eth0", MAC: "bc:24:11:00:aa:b5", Addrs: []netip.Addr{netip.MustParseAddr("10.20.0.15"), netip.MustParseAddr("10.20.0.16")}},
		{Name: "tun0", Addrs: []netip.Addr{netip.MustParseAddr("10.99.0.2")}},
		{Name: "ib0"},
		{Name: "eth1", MAC: "bc:24:11:00:aa:b6"},
	}, got)
}

func TestAgentInterfacesRejectsBadAddresses(t *testing.T) {
	tests := []struct {
		name string
		addr string
	}{
		{"unparsable", `{"ip-address":"not-an-address","ip-address-type":"ipv4","prefix":24}`},
		{"empty", `{"ip-address":"","ip-address-type":"ipv4","prefix":24}`},
		{"missing", `{"ip-address-type":"ipv4","prefix":24}`},
		{"ipv6 literal typed ipv4", `{"ip-address":"fd00::15","ip-address-type":"ipv4","prefix":64}`},
		{"out of range octet", `{"ip-address":"10.20.0.256","ip-address-type":"ipv4","prefix":24}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, map[string]reply{
				"/nodes/pve1/qemu/101/agent/network-get-interfaces": {http.StatusOK, `{"data":{"result":[
					{"name":"eth0","hardware-address":"bc:24:11:00:aa:b5","ip-addresses":[
						{"ip-address":"10.20.0.15","ip-address-type":"ipv4","prefix":24},
						` + tt.addr + `
					]}
				]}}`},
			})
			got, err := c.AgentInterfaces(context.Background(), "pve1", 101)
			require.ErrorContains(t, err, "eth0")
			require.Nil(t, got)
		})
	}
}

func TestAgentInterfacesWithoutResult(t *testing.T) {
	bodies := []string{`{}`, `{"data":null}`, `{"data":{}}`, `{"data":{"result":null}}`}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			c, _ := newTestClient(t, map[string]reply{
				"/nodes/pve1/qemu/101/agent/network-get-interfaces": {http.StatusOK, body},
			})
			got, err := c.AgentInterfaces(context.Background(), "pve1", 101)
			require.ErrorContains(t, err, "unexpected response")
			require.Nil(t, got)
		})
	}

	c, _ := newTestClient(t, map[string]reply{
		"/nodes/pve1/qemu/101/agent/network-get-interfaces": {http.StatusOK, `{"data":{"result":[]}}`},
	})
	got, err := c.AgentInterfaces(context.Background(), "pve1", 101)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestLXCInterfaces(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/nodes/pve1/lxc/200/interfaces": okReply(t, "lxc_interfaces.json"),
	})
	got, err := c.LXCInterfaces(context.Background(), "pve1", 200)
	require.NoError(t, err)

	require.Equal(t, []GuestIface{
		{Name: "lo", MAC: "00:00:00:00:00:00", Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		{Name: "eth0", MAC: "bc:24:11:11:22:33", Addrs: []netip.Addr{netip.MustParseAddr("10.20.0.30")}},
	}, got)
	require.Equal(t, "/api2/json/nodes/pve1/lxc/200/interfaces", rec.requests()[0].uri)
}

func TestLXCInterfacesTolerance(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/nodes/pve1/lxc/200/interfaces": {http.StatusOK, `{"data":[
			{"name":"eth0","hwaddr":"BC:24:11:11:22:33","inet":"10.20.0.30/24"},
			{"name":"eth1","hwaddr":"BC:24:11:11:22:34","inet6":"fe80::1/64"},
			{"name":"eth2","hwaddr":"BC:24:11:11:22:35","inet":"fd00::5/64"},
			{"name":"eth3"}
		]}`},
	})
	got, err := c.LXCInterfaces(context.Background(), "pve1", 200)
	require.NoError(t, err)

	require.Equal(t, []GuestIface{
		{Name: "eth0", MAC: "bc:24:11:11:22:33", Addrs: []netip.Addr{netip.MustParseAddr("10.20.0.30")}},
		{Name: "eth1", MAC: "bc:24:11:11:22:34"},
		{Name: "eth2", MAC: "bc:24:11:11:22:35"},
		{Name: "eth3"},
	}, got)
}

func TestLXCInterfacesRejectsBadInet(t *testing.T) {
	for _, inet := range []string{"garbage", "10.20.0.30", "10.20.0.30/33", "10.20.0.300/24", "10.20.0.30/"} {
		t.Run(inet, func(t *testing.T) {
			c, _ := newTestClient(t, map[string]reply{
				"/nodes/pve1/lxc/200/interfaces": {http.StatusOK, `{"data":[
					{"name":"lo","hwaddr":"00:00:00:00:00:00","inet":"127.0.0.1/8"},
					{"name":"eth0","hwaddr":"bc:24:11:11:22:33","inet":"` + inet + `"}
				]}`},
			})
			got, err := c.LXCInterfaces(context.Background(), "pve1", 200)
			require.ErrorContains(t, err, "eth0")
			require.Nil(t, got)
		})
	}
}

func TestClusterNodes(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/cluster/status": okReply(t, "cluster_status.json"),
	})
	got, err := c.ClusterNodes(context.Background())
	require.NoError(t, err)

	require.Equal(t, []ClusterNode{
		{Name: "pve1", Addr: netip.MustParseAddr("10.20.0.2"), Online: true, Local: true},
		{Name: "pve2", Addr: netip.MustParseAddr("10.20.0.3")},
	}, got)
	require.Equal(t, "/api2/json/cluster/status", rec.requests()[0].uri)
}

func TestClusterNodesStandalone(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/cluster/status": {http.StatusOK, `{"data":[{"id":"node/pve1","type":"node","name":"pve1","ip":"10.20.0.2","online":1,"local":1}]}`},
	})
	got, err := c.ClusterNodes(context.Background())
	require.NoError(t, err)
	require.Equal(t, []ClusterNode{
		{Name: "pve1", Addr: netip.MustParseAddr("10.20.0.2"), Online: true, Local: true},
	}, got)
}

func TestClusterNodesUnknownAddress(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/cluster/status": {http.StatusOK, `{"data":[
			{"type":"node","name":"pve1","online":1,"local":1},
			{"type":"node","name":"pve2","ip":"fd00::2","online":1,"local":0}
		]}`},
	})
	got, err := c.ClusterNodes(context.Background())
	require.NoError(t, err)
	require.Equal(t, []ClusterNode{
		{Name: "pve1", Online: true, Local: true},
		{Name: "pve2", Online: true},
	}, got)
}

func TestClusterNodesRejectsBadAddress(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/cluster/status": {http.StatusOK, `{"data":[
			{"type":"node","name":"pve1","ip":"10.20.0.2","online":1,"local":1},
			{"type":"node","name":"pve2","ip":"not-an-address","online":1,"local":0}
		]}`},
	})
	got, err := c.ClusterNodes(context.Background())
	require.ErrorContains(t, err, "pve2")
	require.Nil(t, got)
}

func TestClusterNodesNeedsANode(t *testing.T) {
	bodies := []string{
		`{}`,
		`{"data":null}`,
		`{"data":[]}`,
		`{"data":{}}`,
		`{"data":[{"type":"cluster","name":"lab","nodes":2,"quorate":1}]}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			c, _ := newTestClient(t, map[string]reply{"/cluster/status": {http.StatusOK, body}})
			got, err := c.ClusterNodes(context.Background())
			require.Error(t, err)
			require.Nil(t, got)
		})
	}
}

func TestNodeNetwork(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/nodes/pve1/network": okReply(t, "node_network.json"),
	})
	got, err := c.NodeNetwork(context.Background(), "pve1")
	require.NoError(t, err)

	require.Equal(t, []NodeIface{
		{Name: "vmbr0", Type: "bridge", Active: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.20.0.2/24")}, Ports: []string{"nic3"}},
		{Name: "vmbr1", Type: "bridge", Active: true},
		{Name: "nic3", Type: "eth", Active: true},
	}, got)
	require.Equal(t, "/api2/json/nodes/pve1/network", rec.requests()[0].uri)
}

func TestNodeNetworkRejectsBadCIDR(t *testing.T) {
	for _, cidr := range []string{"broken", "10.20.0.2", "10.20.0.2/33", "10.20.0.2/"} {
		t.Run(cidr, func(t *testing.T) {
			c, _ := newTestClient(t, map[string]reply{
				"/nodes/pve1/network": {http.StatusOK, `{"data":[
					{"iface":"nic3","type":"eth","active":1},
					{"iface":"vmbr0","type":"bridge","active":1,"cidr":"` + cidr + `"}
				]}`},
			})
			got, err := c.NodeNetwork(context.Background(), "pve1")
			require.ErrorContains(t, err, "vmbr0")
			require.Nil(t, got)
		})
	}
}

func TestNodeNetworkTolerance(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/nodes/pve1/network": {http.StatusOK, `{"data":[
			{"iface":"vmbr0","type":"bridge","active":1,"cidr":"10.20.0.2/24","cidr6":"fd00::2/64","bridge_ports":"nic3  nic4 tap101i0"},
			{"iface":"vmbr1","type":"bridge","active":0,"cidr":"fd00::3/64"},
			{"iface":"bond0","type":"bond","cidr6":"fd00::9/64"},
			{"iface":"vmbr2","type":"bridge","active":"1","autostart":1}
		]}`},
	})
	got, err := c.NodeNetwork(context.Background(), "pve1")
	require.NoError(t, err)

	require.Equal(t, []NodeIface{
		{Name: "vmbr0", Type: "bridge", Active: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.20.0.2/24")}, Ports: []string{"nic3", "nic4", "tap101i0"}},
		{Name: "vmbr1", Type: "bridge"},
		{Name: "bond0", Type: "bond"},
		{Name: "vmbr2", Type: "bridge", Active: true},
	}, got)
}

func TestWrongShapeIsAnError(t *testing.T) {
	ctx := context.Background()
	qemu := model.GuestRef{Kind: model.KindQEMU, VMID: 101}
	const (
		object = `{"data":{"qemu":[1,2]}}`
		array  = `{"data":[{"a":1}]}`
	)
	endpoints := []struct {
		name  string
		route string
		wrong string // the container the endpoint does not return
		call  func(c *Client) error
	}{
		{"Version", "/version", array, func(c *Client) error { _, err := c.Version(ctx); return err }},
		{"Resources", "/cluster/resources?type=vm", object, func(c *Client) error { _, err := c.Resources(ctx); return err }},
		{"GuestConfig", "/nodes/pve1/qemu/101/config", array, func(c *Client) error { _, err := c.GuestConfig(ctx, "pve1", qemu); return err }},
		{"AgentInterfaces", "/nodes/pve1/qemu/101/agent/network-get-interfaces", array, func(c *Client) error { _, err := c.AgentInterfaces(ctx, "pve1", 101); return err }},
		{"LXCInterfaces", "/nodes/pve1/lxc/200/interfaces", object, func(c *Client) error { _, err := c.LXCInterfaces(ctx, "pve1", 200); return err }},
		{"ClusterNodes", "/cluster/status", object, func(c *Client) error { _, err := c.ClusterNodes(ctx); return err }},
		{"NodeNetwork", "/nodes/pve1/network", object, func(c *Client) error { _, err := c.NodeNetwork(ctx, "pve1"); return err }},
	}
	for _, ep := range endpoints {
		bodies := []struct{ name, body string }{
			{"wrong container", ep.wrong},
			{"string", `{"data":"pve1"}`},
			{"number", `{"data":7}`},
			{"truncated", `{"data":[{"type":"qemu","vmid":10`},
			{"not json", `<html>proxy error</html>`},
			{"empty body", ``},
		}
		for _, b := range bodies {
			t.Run(ep.name+" "+b.name, func(t *testing.T) {
				c, _ := newTestClient(t, map[string]reply{ep.route: {http.StatusOK, b.body}})
				err := ep.call(c)
				require.Error(t, err)
				var apiErr *APIError
				require.False(t, errors.As(err, &apiErr))
			})
		}
	}
}
