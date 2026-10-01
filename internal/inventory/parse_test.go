package inventory

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

func qemuResource() pve.Resource {
	return pve.Resource{
		Kind:   model.KindQEMU,
		VMID:   101,
		Name:   "web-1",
		Node:   "pve1",
		Status: "running",
		Tags:   []string{"cf-tunnel", "prod"},
	}
}

func lxcResource() pve.Resource {
	return pve.Resource{
		Kind:   model.KindLXC,
		VMID:   200,
		Name:   "db-1",
		Node:   "pve2",
		Status: "stopped",
	}
}

func config(values map[string]string) pve.GuestConfig {
	return pve.GuestConfig{Values: values, Digest: "d1g3st"}
}

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, 0, len(s))
	for _, a := range s {
		out = append(out, netip.MustParseAddr(a))
	}
	return out
}

func TestBuildGuestQEMU(t *testing.T) {
	cfg := config(map[string]string{
		"name":        "web-1",
		"description": "front end\n",
		"net0":        "virtio=BC:24:11:00:AA:B5,bridge=vmbr0,firewall=1,tag=10",
		"net1":        "virtio=BC:24:11:00:AA:B6,bridge=vmbr1",
		"ipconfig0":   "ip=10.20.0.15/24,gw=10.20.0.1",
		"smbios1":     "uuid=80e8ef19-34bf-4cb3-b314-96870ca14d1e",
		"meta":        "creation-qemu=10.1.2,ctime=1790847599",
	})

	g, err := BuildGuest(qemuResource(), cfg)
	require.NoError(t, err)

	require.Equal(t, model.Guest{
		Ref:         model.GuestRef{Kind: model.KindQEMU, VMID: 101},
		Name:        "web-1",
		Node:        "pve1",
		Running:     true,
		Tags:        []string{"cf-tunnel", "prod"},
		Description: "front end\n",
		Digest:      "d1g3st",
		Identity:    "uuid:80e8ef19-34bf-4cb3-b314-96870ca14d1e",
		NICs: []model.NIC{
			{
				Index:    0,
				MAC:      "bc:24:11:00:aa:b5",
				Bridge:   "vmbr0",
				VLAN:     10,
				Firewall: true,
				Static:   addrs("10.20.0.15"),
			},
			{Index: 1, MAC: "bc:24:11:00:aa:b6", Bridge: "vmbr1"},
		},
	}, g)
}

func TestBuildGuestQEMUMacaddrForm(t *testing.T) {
	g, err := BuildGuest(qemuResource(), config(map[string]string{
		"net1": "e1000,macaddr=AA:BB:CC:DD:EE:FF,bridge=vmbr1",
	}))
	require.NoError(t, err)
	require.Equal(t, []model.NIC{
		{Index: 1, MAC: "aa:bb:cc:dd:ee:ff", Bridge: "vmbr1"},
	}, g.NICs)
}

func TestBuildGuestQEMUModelKeyForm(t *testing.T) {
	g, err := BuildGuest(qemuResource(), config(map[string]string{
		"net0": "model=virtio,macaddr=AA:BB:CC:DD:EE:FF,bridge=vmbr1",
	}))
	require.NoError(t, err)
	require.Equal(t, []model.NIC{
		{Index: 0, MAC: "aa:bb:cc:dd:ee:ff", Bridge: "vmbr1"},
	}, g.NICs)
}

func TestBuildGuestQEMUAgreeingMACs(t *testing.T) {
	g, err := BuildGuest(qemuResource(), config(map[string]string{
		"net0": "virtio=BC:24:11:00:AA:B5,macaddr=bc:24:11:00:aa:b5,bridge=vmbr0",
	}))
	require.NoError(t, err)
	require.Equal(t, []model.NIC{
		{Index: 0, MAC: "bc:24:11:00:aa:b5", Bridge: "vmbr0"},
	}, g.NICs)
}

func TestBuildGuestQEMUModels(t *testing.T) {
	for _, nicModel := range []string{"virtio", "e1000", "e1000e", "rtl8139", "vmxnet3"} {
		t.Run(nicModel, func(t *testing.T) {
			g, err := BuildGuest(qemuResource(), config(map[string]string{
				"net0": nicModel + "=BC:24:11:00:AA:B5,bridge=vmbr0",
			}))
			require.NoError(t, err)
			require.Equal(t, []model.NIC{
				{Index: 0, MAC: "bc:24:11:00:aa:b5", Bridge: "vmbr0"},
			}, g.NICs)
		})
	}
}

func TestBuildGuestIPConfigLandsOnItsOwnNIC(t *testing.T) {
	g, err := BuildGuest(qemuResource(), config(map[string]string{
		"net0":      "virtio=BC:24:11:00:AA:B5,bridge=vmbr0",
		"net1":      "virtio=BC:24:11:00:AA:B6,bridge=vmbr0",
		"ipconfig1": "ip=10.20.0.16/24,gw=10.20.0.1",
	}))
	require.NoError(t, err)
	require.Len(t, g.NICs, 2)
	require.Empty(t, g.NICs[0].Static)
	require.Equal(t, addrs("10.20.0.16"), g.NICs[1].Static)
}

func TestBuildGuestIPConfigWithoutNICIsIgnored(t *testing.T) {
	g, err := BuildGuest(qemuResource(), config(map[string]string{
		"net0":      "virtio=BC:24:11:00:AA:B5,bridge=vmbr0",
		"ipconfig3": "ip=10.20.0.16/24",
	}))
	require.NoError(t, err)
	require.Len(t, g.NICs, 1)
	require.Empty(t, g.NICs[0].Static)
}

func TestBuildGuestIPConfigIgnored(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"dhcp with ip6 auto", "ip=dhcp,ip6=auto"},
		{"ipv6 only", "ip6=2001:db8::5/64,gw6=2001:db8::1"},
		{"ipv6 in ip", "ip=2001:db8::5/64"},
		{"mapped ipv6", "ip=::ffff:10.20.0.5/120"},
		{"no prefix", "ip=10.20.0.16"},
		{"garbage", "ip=garbage"},
		{"unspecified", "ip=0.0.0.0/0"},
		{"loopback", "ip=127.0.0.1/8"},
		{"multicast", "ip=224.0.0.5/24"},
		{"malformed fragment", "ip=10.20.0.16/24,oops"},
		{"duplicate ip", "ip=10.20.0.16/24,ip=10.20.0.17/24"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := BuildGuest(qemuResource(), config(map[string]string{
				"net0":      "virtio=BC:24:11:00:AA:B5,bridge=vmbr0",
				"ipconfig0": tt.value,
			}))
			require.NoError(t, err)
			require.Len(t, g.NICs, 1)
			require.Empty(t, g.NICs[0].Static)
		})
	}
}

func TestBuildGuestLXC(t *testing.T) {
	cfg := config(map[string]string{
		"hostname":    "db-1",
		"description": "database",
		"net0":        "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:11:22:33,ip=10.20.0.30/24,gw=10.20.0.1,tag=20,firewall=1,type=veth",
		"net1":        "name=eth1,bridge=vmbr0,hwaddr=BC:24:11:11:22:34,ip=dhcp,type=veth",
		"net2":        "name=eth2,bridge=vmbr1,hwaddr=BC:24:11:11:22:35,ip=manual,type=veth",
		"net3":        "name=eth3,bridge=vmbr1,hwaddr=BC:24:11:11:22:36,ip6=auto,type=veth",
	})

	g, err := BuildGuest(lxcResource(), cfg)
	require.NoError(t, err)

	require.Equal(t, model.Guest{
		Ref:         model.GuestRef{Kind: model.KindLXC, VMID: 200},
		Name:        "db-1",
		Node:        "pve2",
		Running:     false,
		Description: "database",
		Digest:      "d1g3st",
		Identity:    "mac:0ecc2605930427c000cfc6cad398e84c",
		NICs: []model.NIC{
			{
				Index:    0,
				MAC:      "bc:24:11:11:22:33",
				Bridge:   "vmbr0",
				VLAN:     20,
				Firewall: true,
				Static:   addrs("10.20.0.30"),
			},
			{Index: 1, MAC: "bc:24:11:11:22:34", Bridge: "vmbr0"},
			{Index: 2, MAC: "bc:24:11:11:22:35", Bridge: "vmbr1"},
			{Index: 3, MAC: "bc:24:11:11:22:36", Bridge: "vmbr1"},
		},
	}, g)
}

func TestBuildGuestLXCIgnoresBadAddress(t *testing.T) {
	for _, ip := range []string{"garbage", "10.20.0.30", "2001:db8::5/64", "0.0.0.0/0"} {
		t.Run(ip, func(t *testing.T) {
			g, err := BuildGuest(lxcResource(), config(map[string]string{
				"net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:11:22:33,ip=" + ip,
			}))
			require.NoError(t, err)
			require.Len(t, g.NICs, 1)
			require.Empty(t, g.NICs[0].Static)
		})
	}
}

func TestBuildGuestNICOrder(t *testing.T) {
	g, err := BuildGuest(qemuResource(), config(map[string]string{
		"net10": "virtio=BC:24:11:00:00:0A,bridge=vmbr0",
		"net2":  "virtio=BC:24:11:00:00:02,bridge=vmbr0",
		"net0":  "virtio=BC:24:11:00:00:00,bridge=vmbr0",
		"net31": "virtio=BC:24:11:00:00:1F,bridge=vmbr0",
	}))
	require.NoError(t, err)

	var indexes []int
	for _, n := range g.NICs {
		indexes = append(indexes, n.Index)
	}
	require.Equal(t, []int{0, 2, 10, 31}, indexes)
}

func TestBuildGuestIgnoresOtherNetKeys(t *testing.T) {
	nic := "virtio=BC:24:11:00:00:00,bridge=vmbr0"
	g, err := BuildGuest(qemuResource(), config(map[string]string{
		"net32": nic, "net01": nic, "net-1": nic, "net": nic, "netx": nic, "net1x": nic, "network": nic,
	}))
	require.NoError(t, err)
	require.Empty(t, g.NICs)
}

func TestBuildGuestSkipsMalformedNIC(t *testing.T) {
	const good = "virtio=BC:24:11:00:AA:B6,bridge=vmbr0"
	tests := []struct {
		name  string
		value string
	}{
		{"garbage", "garbage"},
		{"empty", ""},
		{"no bridge", "virtio=BC:24:11:00:AA:B5"},
		{"no mac", "virtio,bridge=vmbr0"},
		{"empty bridge", "virtio=BC:24:11:00:AA:B5,bridge="},
		{"bad mac", "virtio=BC:24:11:00:AA,bridge=vmbr0"},
		{"long mac", "virtio=BC:24:11:00:AA:B5:01:02,bridge=vmbr0"},
		{"bad macaddr", "e1000,macaddr=nonsense,bridge=vmbr0"},
		{"unknown model key", "unknown=BC:24:11:00:AA:B5,bridge=vmbr0"},
		{"two different macs", "virtio=BC:24:11:00:AA:B5,macaddr=BC:24:11:00:AA:B7,bridge=vmbr0"},
		{"two models", "virtio=BC:24:11:00:AA:B5,e1000=BC:24:11:00:AA:B7,bridge=vmbr0"},
		{"bare unknown", "virtio=BC:24:11:00:AA:B5,bridge=vmbr0,oops"},
		{"empty fragment", "virtio=BC:24:11:00:AA:B5,,bridge=vmbr0"},
		{"trailing comma", "virtio=BC:24:11:00:AA:B5,bridge=vmbr0,"},
		{"duplicate bridge", "virtio=BC:24:11:00:AA:B5,bridge=vmbr0,bridge=vmbr1"},
		{"tag zero", "virtio=BC:24:11:00:AA:B5,bridge=vmbr0,tag=0"},
		{"tag too large", "virtio=BC:24:11:00:AA:B5,bridge=vmbr0,tag=4095"},
		{"tag negative", "virtio=BC:24:11:00:AA:B5,bridge=vmbr0,tag=-5"},
		{"tag signed", "virtio=BC:24:11:00:AA:B5,bridge=vmbr0,tag=+5"},
		{"tag text", "virtio=BC:24:11:00:AA:B5,bridge=vmbr0,tag=abc"},
		{"tag empty", "virtio=BC:24:11:00:AA:B5,bridge=vmbr0,tag="},
		{"tag huge", "virtio=BC:24:11:00:AA:B5,bridge=vmbr0,tag=99999999999999999999"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := BuildGuest(qemuResource(), config(map[string]string{
				"net0": tt.value,
				"net1": good,
			}))
			require.NoError(t, err)
			require.Equal(t, []model.NIC{
				{Index: 1, MAC: "bc:24:11:00:aa:b6", Bridge: "vmbr0"},
			}, g.NICs)
		})
	}
}

func TestBuildGuestSkipsMalformedLXCNIC(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"garbage", "garbage"},
		{"no mac", "name=eth0,bridge=vmbr0"},
		{"no bridge", "name=eth0,hwaddr=BC:24:11:11:22:33"},
		{"bad mac", "name=eth0,bridge=vmbr0,hwaddr=zz"},
		{"qemu style mac", "name=eth0,bridge=vmbr0,virtio=BC:24:11:11:22:33"},
		{"bad tag", "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:11:22:33,tag=5000"},
		{"bare fragment", "eth0,bridge=vmbr0,hwaddr=BC:24:11:11:22:33"},
		{"duplicate hwaddr", "bridge=vmbr0,hwaddr=BC:24:11:11:22:33,hwaddr=BC:24:11:11:22:33"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := BuildGuest(lxcResource(), config(map[string]string{"net0": tt.value}))
			require.NoError(t, err)
			require.Empty(t, g.NICs)
		})
	}
}

func TestBuildGuestVLANBounds(t *testing.T) {
	tests := []struct {
		tag  string
		vlan int
	}{
		{"1", 1},
		{"4094", 4094},
	}
	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			g, err := BuildGuest(qemuResource(), config(map[string]string{
				"net0": "virtio=BC:24:11:00:AA:B5,bridge=vmbr0,tag=" + tt.tag,
			}))
			require.NoError(t, err)
			require.Len(t, g.NICs, 1)
			require.Equal(t, tt.vlan, g.NICs[0].VLAN)
		})
	}
}

func TestBuildGuestFirewallFlag(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"firewall=1", true},
		{"firewall=0", false},
		{"link_down=1", false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			g, err := BuildGuest(qemuResource(), config(map[string]string{
				"net0": "virtio=BC:24:11:00:AA:B5,bridge=vmbr0," + tt.value,
			}))
			require.NoError(t, err)
			require.Len(t, g.NICs, 1)
			require.Equal(t, tt.want, g.NICs[0].Firewall)
		})
	}
}

func TestBuildGuestTemplate(t *testing.T) {
	tests := []struct {
		name     string
		resource bool
		config   string
		want     bool
	}{
		{"neither", false, "", false},
		{"config zero", false, "0", false},
		{"config one", false, "1", true},
		{"resource", true, "", true},
		{"both", true, "1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := qemuResource()
			res.Template = tt.resource
			values := map[string]string{}
			if tt.config != "" {
				values["template"] = tt.config
			}
			g, err := BuildGuest(res, config(values))
			require.NoError(t, err)
			require.Equal(t, tt.want, g.Template)
		})
	}
}

func TestBuildGuestRunning(t *testing.T) {
	for status, want := range map[string]bool{"running": true, "stopped": false, "paused": false, "": false} {
		t.Run(status, func(t *testing.T) {
			res := qemuResource()
			res.Status = status
			g, err := BuildGuest(res, config(nil))
			require.NoError(t, err)
			require.Equal(t, want, g.Running)
		})
	}
}

func TestBuildGuestName(t *testing.T) {
	tests := []struct {
		name   string
		res    pve.Resource
		values map[string]string
		want   string
	}{
		{"resource wins", qemuResource(), map[string]string{"name": "other"}, "web-1"},
		{"qemu falls back to name", func() pve.Resource { r := qemuResource(); r.Name = ""; return r }(),
			map[string]string{"name": "web-9", "hostname": "ignored"}, "web-9"},
		{"lxc falls back to hostname", func() pve.Resource { r := lxcResource(); r.Name = ""; return r }(),
			map[string]string{"hostname": "db-9", "name": "ignored"}, "db-9"},
		{"nothing", func() pve.Resource { r := lxcResource(); r.Name = ""; return r }(), nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := BuildGuest(tt.res, config(tt.values))
			require.NoError(t, err)
			require.Equal(t, tt.want, g.Name)
		})
	}
}

func TestBuildGuestTags(t *testing.T) {
	tests := []struct {
		name string
		res  []string
		cfg  string
		want []string
	}{
		{"from resource", []string{"a", "b"}, "x;y", []string{"a", "b"}},
		{"config fallback", nil, "Web;Prod", []string{"web", "prod"}},
		{"config separators", nil, "a, b;c d", []string{"a", "b", "c", "d"}},
		{"none", nil, "", nil},
		{"only separators", nil, " ; ,", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := qemuResource()
			res.Tags = tt.res
			values := map[string]string{}
			if tt.cfg != "" {
				values["tags"] = tt.cfg
			}
			g, err := BuildGuest(res, config(values))
			require.NoError(t, err)
			require.Equal(t, tt.want, g.Tags)
		})
	}
}

func TestBuildGuestDescriptionVerbatim(t *testing.T) {
	const desc = "line one\r\nline two\r\n\r\n  indented \r\n"
	g, err := BuildGuest(qemuResource(), config(map[string]string{"description": desc}))
	require.NoError(t, err)
	require.Equal(t, desc, g.Description)
}

func TestBuildGuestIdentityQEMUUUID(t *testing.T) {
	tests := []struct {
		name    string
		smbios  string
		wantID  string
		useHash bool
	}{
		{"plain", "uuid=80e8ef19-34bf-4cb3-b314-96870ca14d1e", "uuid:80e8ef19-34bf-4cb3-b314-96870ca14d1e", false},
		{"upper case", "uuid=80E8EF19-34BF-4CB3-B314-96870CA14D1E", "uuid:80e8ef19-34bf-4cb3-b314-96870ca14d1e", false},
		{"with other keys", "manufacturer=QUJD,uuid=80e8ef19-34bf-4cb3-b314-96870ca14d1e,base64=1",
			"uuid:80e8ef19-34bf-4cb3-b314-96870ca14d1e", false},
		{"padded base64 value", "manufacturer=TWljcm9zb2Z0IA==,uuid=80e8ef19-34bf-4cb3-b314-96870ca14d1e,base64=1",
			"uuid:80e8ef19-34bf-4cb3-b314-96870ca14d1e", false},
		{"too short", "uuid=80e8ef19-34bf-4cb3-b314", "", true},
		{"not hex", "uuid=zze8ef19-34bf-4cb3-b314-96870ca14d1e", "", true},
		{"no dashes", "uuid=80e8ef1934bf4cb3b31496870ca14d1e", "", true},
		{"missing uuid", "manufacturer=QUJD,base64=1", "", true},
		{"duplicate uuid", "uuid=80e8ef19-34bf-4cb3-b314-96870ca14d1e,uuid=80e8ef19-34bf-4cb3-b314-96870ca14d1f", "", true},
		{"malformed", "uuid=80e8ef19-34bf-4cb3-b314-96870ca14d1e,", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := BuildGuest(qemuResource(), config(map[string]string{
				"net0":    "virtio=BC:24:11:00:AA:B5,bridge=vmbr0",
				"smbios1": tt.smbios,
				"meta":    "creation-qemu=10.1.2,ctime=1790847599",
			}))
			require.NoError(t, err)
			if tt.useHash {
				require.Equal(t, "mac:478fab427992e718e3319c1673d8b76f", g.Identity)
				return
			}
			require.Equal(t, tt.wantID, g.Identity)
		})
	}
}

func TestBuildGuestIdentityQEMUUUIDWithoutNIC(t *testing.T) {
	g, err := BuildGuest(qemuResource(), config(map[string]string{
		"smbios1": "uuid=80e8ef19-34bf-4cb3-b314-96870ca14d1e",
	}))
	require.NoError(t, err)
	require.Equal(t, "uuid:80e8ef19-34bf-4cb3-b314-96870ca14d1e", g.Identity)
	require.Empty(t, g.NICs)
}

func TestBuildGuestIdentityQEMUMAC(t *testing.T) {
	values := func(mac string) map[string]string {
		return map[string]string{
			"net0": "virtio=" + mac + ",bridge=vmbr0",
			"meta": "creation-qemu=10.1.2,ctime=1790847599",
		}
	}

	first, err := BuildGuest(qemuResource(), config(values("BC:24:11:00:AA:B5")))
	require.NoError(t, err)
	require.Equal(t, "mac:478fab427992e718e3319c1673d8b76f", first.Identity)

	again, err := BuildGuest(qemuResource(), config(values("BC:24:11:00:AA:B5")))
	require.NoError(t, err)
	require.Equal(t, first.Identity, again.Identity)

	other, err := BuildGuest(qemuResource(), config(values("BC:24:11:00:AA:B6")))
	require.NoError(t, err)
	require.Equal(t, "mac:45a4842b54771e4de6279aa0b3c3e665", other.Identity)
	require.NotEqual(t, first.Identity, other.Identity)
}

func TestBuildGuestIdentityQEMUUsesFirstNIC(t *testing.T) {
	g, err := BuildGuest(qemuResource(), config(map[string]string{
		"net2": "virtio=BC:24:11:00:AA:B6,bridge=vmbr0",
		"net1": "virtio=BC:24:11:00:AA:B5,bridge=vmbr0",
		"meta": "ctime=1790847599",
	}))
	require.NoError(t, err)
	require.Equal(t, "mac:478fab427992e718e3319c1673d8b76f", g.Identity)
}

func TestBuildGuestIdentityQEMUWithoutCtime(t *testing.T) {
	for name, meta := range map[string]string{
		"missing":    "",
		"no ctime":   "creation-qemu=10.1.2",
		"not a time": "creation-qemu=10.1.2,ctime=yesterday",
		"malformed":  "creation-qemu=10.1.2,",
	} {
		t.Run(name, func(t *testing.T) {
			values := map[string]string{"net0": "virtio=BC:24:11:00:AA:B5,bridge=vmbr0"}
			if meta != "" {
				values["meta"] = meta
			}
			g, err := BuildGuest(qemuResource(), config(values))
			require.NoError(t, err)
			require.Equal(t, "mac:badb79220ac46c9e4ee8060660a65eea", g.Identity)
		})
	}
}

func TestBuildGuestIdentityLXC(t *testing.T) {
	values := func(mac string) map[string]string {
		return map[string]string{
			"hostname": "db-1",
			"net0":     "name=eth0,bridge=vmbr0,hwaddr=" + mac,
		}
	}

	first, err := BuildGuest(lxcResource(), config(values("BC:24:11:11:22:33")))
	require.NoError(t, err)
	require.Equal(t, "mac:0ecc2605930427c000cfc6cad398e84c", first.Identity)

	again, err := BuildGuest(lxcResource(), config(values("BC:24:11:11:22:33")))
	require.NoError(t, err)
	require.Equal(t, first.Identity, again.Identity)

	other, err := BuildGuest(lxcResource(), config(values("BC:24:11:11:22:34")))
	require.NoError(t, err)
	require.Equal(t, "mac:a0198def8d8e5cf8a1389fe02ff37b44", other.Identity)
}

func TestBuildGuestIdentityIgnoresSMBIOSForLXC(t *testing.T) {
	g, err := BuildGuest(lxcResource(), config(map[string]string{
		"hostname": "db-1",
		"net0":     "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:11:22:33",
		"smbios1":  "uuid=80e8ef19-34bf-4cb3-b314-96870ca14d1e",
	}))
	require.NoError(t, err)
	require.Equal(t, "mac:0ecc2605930427c000cfc6cad398e84c", g.Identity)
}

func TestBuildGuestIdentityWithoutNICOrUUID(t *testing.T) {
	q, err := BuildGuest(qemuResource(), config(map[string]string{"net0": "garbage"}))
	require.NoError(t, err)
	require.Equal(t, "vmid:qemu/101", q.Identity)

	l, err := BuildGuest(lxcResource(), config(map[string]string{"hostname": "db-1"}))
	require.NoError(t, err)
	require.Equal(t, "vmid:lxc/200", l.Identity)
}

func TestBuildGuestIsDeterministic(t *testing.T) {
	cfg := config(map[string]string{
		"net3": "virtio=BC:24:11:00:00:03,bridge=vmbr0",
		"net1": "virtio=BC:24:11:00:00:01,bridge=vmbr0",
		"net2": "virtio=BC:24:11:00:00:02,bridge=vmbr0",
		"tags": "a;b",
	})
	first, err := BuildGuest(qemuResource(), cfg)
	require.NoError(t, err)
	for range 20 {
		again, err := BuildGuest(qemuResource(), cfg)
		require.NoError(t, err)
		require.Equal(t, first, again)
	}
}

func TestBuildGuestDoesNotRetainInput(t *testing.T) {
	res := qemuResource()
	cfg := config(map[string]string{
		"net0": "virtio=BC:24:11:00:AA:B5,bridge=vmbr0",
		"tags": "x",
	})
	g, err := BuildGuest(res, cfg)
	require.NoError(t, err)

	res.Tags[0] = "changed"
	cfg.Values["net0"] = "virtio=BC:24:11:00:AA:B6,bridge=vmbr9"
	cfg.Values["tags"] = "changed"

	require.Equal(t, []string{"cf-tunnel", "prod"}, g.Tags)
	require.Equal(t, "bc:24:11:00:aa:b5", g.NICs[0].MAC)
	require.Equal(t, "vmbr0", g.NICs[0].Bridge)

	cfgOnly := config(map[string]string{"tags": "x;y"})
	noTags := qemuResource()
	noTags.Tags = nil
	g, err = BuildGuest(noTags, cfgOnly)
	require.NoError(t, err)
	cfgOnly.Values["tags"] = "changed"
	require.Equal(t, []string{"x", "y"}, g.Tags)
}

func TestBuildGuestNilValues(t *testing.T) {
	g, err := BuildGuest(qemuResource(), pve.GuestConfig{})
	require.NoError(t, err)
	require.Equal(t, "vmid:qemu/101", g.Identity)
	require.Empty(t, g.NICs)
}

func TestBuildGuestUnknownKind(t *testing.T) {
	res := qemuResource()
	res.Kind = "vm"
	_, err := BuildGuest(res, config(nil))
	require.Error(t, err)
	require.Contains(t, err.Error(), `"vm"`)

	res.Kind = ""
	_, err = BuildGuest(res, config(nil))
	require.Error(t, err)
}
