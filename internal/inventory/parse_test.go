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
	models := []string{
		"e1000", "e1000-82540em", "e1000-82544gc", "e1000-82545em", "e1000e",
		"i82551", "i82557b", "i82559er", "ne2k_isa", "ne2k_pci", "pcnet",
		"rtl8139", "virtio", "vmxnet3",
	}
	require.Len(t, models, 14)
	want := []model.NIC{{Index: 0, MAC: "bc:24:11:00:aa:b5", Bridge: "vmbr0"}}

	for _, nicModel := range models {
		t.Run(nicModel+" as key", func(t *testing.T) {
			g, err := BuildGuest(qemuResource(), config(map[string]string{
				"net0": nicModel + "=BC:24:11:00:AA:B5,bridge=vmbr0",
			}))
			require.NoError(t, err)
			require.Equal(t, want, g.NICs)
		})
		t.Run(nicModel+" bare", func(t *testing.T) {
			g, err := BuildGuest(qemuResource(), config(map[string]string{
				"net0": nicModel + ",macaddr=BC:24:11:00:AA:B5,bridge=vmbr0",
			}))
			require.NoError(t, err)
			require.Equal(t, want, g.NICs)
		})
	}
}

func TestBuildGuestQEMUUnknownModel(t *testing.T) {
	for _, value := range []string{
		"e1000-bogus=BC:24:11:00:AA:B5,bridge=vmbr0",
		"e1000-bogus,macaddr=BC:24:11:00:AA:B5,bridge=vmbr0",
		"VIRTIO=BC:24:11:00:AA:B5,bridge=vmbr0",
	} {
		t.Run(value, func(t *testing.T) {
			g, err := BuildGuest(qemuResource(), config(map[string]string{"net0": value}))
			require.NoError(t, err)
			require.Empty(t, g.NICs)
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
		{"broadcast", "ip=255.255.255.255/32"},
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
		Identity:    "mac:b3e879d1c4f671039fee56bd057bc96c",
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

func TestBuildGuestPool(t *testing.T) {
	res := lxcResource()
	res.Pool = "pco"
	g, err := BuildGuest(res, config(map[string]string{"hostname": "db-1"}))
	require.NoError(t, err)
	require.Equal(t, "pco", g.Pool)

	g, err = BuildGuest(lxcResource(), config(map[string]string{"hostname": "db-1"}))
	require.NoError(t, err)
	require.Empty(t, g.Pool)
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
		{"firewall=on", true},
		{"firewall=yes", true},
		{"firewall=true", true},
		{"firewall=ON", true},
		{"firewall=Yes", true},
		{"firewall=TRUE", true},
		{"firewall=0", false},
		{"firewall=off", false},
		{"firewall=no", false},
		{"firewall=false", false},
		{"firewall=2", false},
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
		{"config fallback keeps case", nil, "Web;Prod", []string{"Web", "Prod"}},
		{"resource keeps case", []string{"Web"}, "", []string{"Web"}},
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

const (
	uuidA = "80e8ef19-34bf-4cb3-b314-96870ca14d1e"
	uuidB = "80e8ef19-34bf-4cb3-b314-96870ca14d1f"

	// sha256 of the normalised MAC, first 32 hex characters.
	identMAC1 = "mac:2842e4cb363717b5ef1f4fd21178ed7a" // bc:24:11:00:aa:b5
	identMAC2 = "mac:b306fc623c756e68d8ea45a502a76f04" // bc:24:11:00:aa:b6
	identMAC3 = "mac:b3e879d1c4f671039fee56bd057bc96c" // bc:24:11:11:22:33
	identMAC4 = "mac:883b315347cfbf2a9832eaba320803d7" // bc:24:11:11:22:34
)

func identity(t *testing.T, res pve.Resource, values map[string]string) string {
	t.Helper()
	g, err := BuildGuest(res, config(values))
	require.NoError(t, err)
	return g.Identity
}

// withValues returns base with the extra keys set; base is not modified.
func withValues(base map[string]string, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestBuildGuestIdentityQEMUUUID(t *testing.T) {
	tests := []struct {
		name   string
		smbios string
		want   string
	}{
		{"plain", "uuid=" + uuidA, "uuid:" + uuidA},
		{"upper case", "uuid=80E8EF19-34BF-4CB3-B314-96870CA14D1E", "uuid:" + uuidA},
		{"with other keys", "manufacturer=QUJD,uuid=" + uuidA + ",base64=1", "uuid:" + uuidA},
		{"padded base64 value", "manufacturer=TWljcm9zb2Z0IA==,uuid=" + uuidA + ",base64=1", "uuid:" + uuidA},
		{"trailing comma", "uuid=" + uuidA + ",", "uuid:" + uuidA},
		{"empty fragment", "manufacturer=QUJD,,uuid=" + uuidA, "uuid:" + uuidA},
		{"bare fragment", "oops,uuid=" + uuidA, "uuid:" + uuidA},
		{"invalid first", "uuid=nonsense,uuid=" + uuidA, "uuid:" + uuidA},
		{"two valid", "uuid=" + uuidA + ",uuid=" + uuidB, "uuid:" + uuidA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := identity(t, qemuResource(), map[string]string{
				"net0":    "virtio=BC:24:11:00:AA:B5,bridge=vmbr0",
				"smbios1": tt.smbios,
				"meta":    "creation-qemu=10.1.2,ctime=1790847599",
			})
			require.Equal(t, tt.want, got)
		})
	}
}

func TestBuildGuestIdentityQEMUUUIDFallsThrough(t *testing.T) {
	for _, smbios := range []string{
		"uuid=80e8ef19-34bf-4cb3-b314",
		"uuid=zze8ef19-34bf-4cb3-b314-96870ca14d1e",
		"uuid=80e8ef1934bf4cb3b31496870ca14d1e",
		"uuid=",
		"manufacturer=QUJD,base64=1",
		"",
	} {
		t.Run(smbios, func(t *testing.T) {
			got := identity(t, qemuResource(), map[string]string{
				"net0":    "virtio=BC:24:11:00:AA:B5,bridge=vmbr0",
				"smbios1": smbios,
				"meta":    "ctime=1790847599",
			})
			require.Equal(t, "ctime:1790847599", got)
		})
	}
}

func TestBuildGuestIdentityQEMUUUIDWithoutNIC(t *testing.T) {
	g, err := BuildGuest(qemuResource(), config(map[string]string{"smbios1": "uuid=" + uuidA}))
	require.NoError(t, err)
	require.Equal(t, "uuid:"+uuidA, g.Identity)
	require.Empty(t, g.NICs)
}

func TestBuildGuestIdentityQEMUUUIDStability(t *testing.T) {
	base := map[string]string{
		"net0":    "virtio=BC:24:11:00:AA:B5,bridge=vmbr0",
		"smbios1": "uuid=" + uuidA,
		"meta":    "ctime=1790847599",
	}
	want := "uuid:" + uuidA

	require.Equal(t, want, identity(t, qemuResource(), base))
	require.Equal(t, want, identity(t, qemuResource(), withValues(base, map[string]string{
		"smbios1": "manufacturer=QUJD,uuid=" + uuidA + ",product=QUJD,base64=1",
	})))
	require.Equal(t, want, identity(t, qemuResource(), withValues(base, map[string]string{
		"net1": "virtio=BC:24:11:00:AA:B6,bridge=vmbr0",
	})))
	require.Equal(t, want, identity(t, qemuResource(), withValues(base, map[string]string{
		"net0": "virtio=BC:24:11:00:AA:B7,bridge=vmbr0",
		"meta": "ctime=1790000000",
	})))
	require.NotEqual(t, want, identity(t, qemuResource(), withValues(base, map[string]string{
		"smbios1": "uuid=" + uuidB,
	})))
}

func TestBuildGuestIdentityQEMUCtime(t *testing.T) {
	base := map[string]string{
		"net0": "virtio=BC:24:11:00:AA:B5,bridge=vmbr0",
		"meta": "creation-qemu=10.1.2,ctime=1790847599",
	}
	want := "ctime:1790847599"

	require.Equal(t, want, identity(t, qemuResource(), base))
	require.Equal(t, want, identity(t, qemuResource(), withValues(base, map[string]string{
		"net0": "virtio=BC:24:11:00:AA:B6,bridge=vmbr1,tag=5000",
		"net1": "virtio=BC:24:11:00:AA:B7,bridge=vmbr0",
	})))
	require.Equal(t, want, identity(t, qemuResource(), withValues(base, map[string]string{
		"meta": "creation-qemu=10.1.2,ctime=1790847599,extra=1",
	})))
	require.Equal(t, want, identity(t, qemuResource(), withValues(base, map[string]string{
		"meta": "ctime=1790847599,",
	})))
	require.Equal(t, want, identity(t, qemuResource(), withValues(base, map[string]string{
		"meta": "ctime=junk,ctime=1790847599",
	})))
	require.Equal(t, "ctime:1790847600", identity(t, qemuResource(), withValues(base, map[string]string{
		"meta": "creation-qemu=10.1.2,ctime=1790847600",
	})))
}

func TestBuildGuestIdentityQEMUCtimeWithoutNIC(t *testing.T) {
	require.Equal(t, "ctime:1790847599", identity(t, qemuResource(), map[string]string{
		"meta": "ctime=1790847599",
	}))
}

func TestBuildGuestIdentityQEMUMAC(t *testing.T) {
	base := map[string]string{"net0": "virtio=BC:24:11:00:AA:B5,bridge=vmbr0"}

	require.Equal(t, identMAC1, identity(t, qemuResource(), base))
	require.Equal(t, identMAC1, identity(t, qemuResource(), base), "stable across calls")
	require.Equal(t, identMAC2, identity(t, qemuResource(), map[string]string{
		"net0": "virtio=BC:24:11:00:AA:B6,bridge=vmbr0",
	}))
}

func TestBuildGuestIdentityQEMUMACFallbacks(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"no meta":          {},
		"meta without":     {"meta": "creation-qemu=10.1.2"},
		"ctime not number": {"meta": "ctime=yesterday"},
		"ctime negative":   {"meta": "ctime=-5"},
		"ctime empty":      {"meta": "ctime="},
		"uuid invalid":     {"smbios1": "uuid=nonsense"},
	} {
		t.Run(name, func(t *testing.T) {
			got := identity(t, qemuResource(), withValues(map[string]string{
				"net0": "virtio=BC:24:11:00:AA:B5,bridge=vmbr0",
			}, values))
			require.Equal(t, identMAC1, got)
		})
	}
}

func TestBuildGuestIdentityQEMUMACStability(t *testing.T) {
	base := map[string]string{"net0": "virtio=BC:24:11:00:AA:B5,bridge=vmbr0"}

	t.Run("second nic at a higher index", func(t *testing.T) {
		got := identity(t, qemuResource(), withValues(base, map[string]string{
			"net1": "virtio=BC:24:11:00:AA:B6,bridge=vmbr0",
		}))
		require.Equal(t, identMAC1, got)
	})
	t.Run("first nic loses its bridge", func(t *testing.T) {
		got := identity(t, qemuResource(), map[string]string{"net0": "virtio=BC:24:11:00:AA:B5"})
		require.Equal(t, identMAC1, got)
	})
	t.Run("first nic gets a bad tag", func(t *testing.T) {
		got := identity(t, qemuResource(), map[string]string{
			"net0": "virtio=BC:24:11:00:AA:B5,bridge=vmbr0,tag=9999",
		})
		require.Equal(t, identMAC1, got)
	})
	t.Run("first nic written in the older form", func(t *testing.T) {
		got := identity(t, qemuResource(), map[string]string{
			"net0": "e1000,macaddr=bc:24:11:00:aa:b5,bridge=vmbr0",
		})
		require.Equal(t, identMAC1, got)
	})
	t.Run("lowest index with a mac wins", func(t *testing.T) {
		got := identity(t, qemuResource(), map[string]string{
			"net0": "garbage",
			"net1": "virtio=nonsense,bridge=vmbr0",
			"net2": "virtio=BC:24:11:00:AA:B6,bridge=vmbr0",
			"net3": "virtio=BC:24:11:00:AA:B5,bridge=vmbr0",
		})
		require.Equal(t, identMAC2, got)
	})
	t.Run("nic added below the first", func(t *testing.T) {
		got := identity(t, qemuResource(), map[string]string{
			"net0": "virtio=BC:24:11:00:AA:B6,bridge=vmbr0",
			"net1": "virtio=BC:24:11:00:AA:B5,bridge=vmbr0",
		})
		require.NotEqual(t, identMAC1, got)
	})
}

func TestBuildGuestIdentityQEMUWithoutAnything(t *testing.T) {
	require.Equal(t, "vmid:qemu/101", identity(t, qemuResource(), map[string]string{"net0": "garbage"}))
	require.Equal(t, "vmid:qemu/101", identity(t, qemuResource(), map[string]string{
		"net0": "virtio=nonsense,bridge=vmbr0",
	}))
	require.Equal(t, "vmid:qemu/101", identity(t, qemuResource(), nil))
}

func TestBuildGuestIdentityLXC(t *testing.T) {
	base := map[string]string{
		"hostname": "db-1",
		"net0":     "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:11:22:33",
	}

	require.Equal(t, identMAC3, identity(t, lxcResource(), base))
	require.Equal(t, identMAC3, identity(t, lxcResource(), base), "stable across calls")
	require.Equal(t, identMAC4, identity(t, lxcResource(), withValues(base, map[string]string{
		"net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:11:22:34",
	})))
}

func TestBuildGuestIdentityLXCStability(t *testing.T) {
	base := map[string]string{
		"hostname": "db-1",
		"net0":     "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:11:22:33",
	}

	tests := []struct {
		name  string
		extra map[string]string
	}{
		{"renamed", map[string]string{"hostname": "db-2"}},
		{"no hostname", map[string]string{"hostname": ""}},
		{"second nic at a higher index", map[string]string{
			"net1": "name=eth1,bridge=vmbr0,hwaddr=BC:24:11:11:22:34",
		}},
		{"first nic loses its bridge", map[string]string{
			"net0": "name=eth0,hwaddr=BC:24:11:11:22:33",
		}},
		{"first nic gets a bad tag", map[string]string{
			"net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:11:22:33,tag=5000",
		}},
		{"first nic gets a static address", map[string]string{
			"net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:11:22:33,ip=10.20.0.30/24",
		}},
		{"first nic has a trailing comma", map[string]string{
			"net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:11:22:33,",
		}},
		{"unrelated keys", map[string]string{
			"smbios1": "uuid=" + uuidA,
			"meta":    "ctime=1790847599",
			"memory":  "512",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, identMAC3, identity(t, lxcResource(), withValues(base, tt.extra)))
		})
	}
}

func TestBuildGuestIdentityLXCUsesLowestNICWithMAC(t *testing.T) {
	got := identity(t, lxcResource(), map[string]string{
		"net0": "garbage",
		"net1": "name=eth1,bridge=vmbr0,hwaddr=nonsense",
		"net2": "name=eth2,bridge=vmbr0,hwaddr=BC:24:11:11:22:34",
		"net3": "name=eth3,bridge=vmbr0,hwaddr=BC:24:11:11:22:33",
	})
	require.Equal(t, identMAC4, got)
}

func TestBuildGuestIdentityLXCNICAddedBelowFirst(t *testing.T) {
	before := identity(t, lxcResource(), map[string]string{
		"net1": "name=eth1,bridge=vmbr0,hwaddr=BC:24:11:11:22:33",
	})
	after := identity(t, lxcResource(), map[string]string{
		"net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:11:22:34",
		"net1": "name=eth1,bridge=vmbr0,hwaddr=BC:24:11:11:22:33",
	})
	require.Equal(t, identMAC3, before)
	require.Equal(t, identMAC4, after)
}

func TestBuildGuestIdentityLXCWithoutMAC(t *testing.T) {
	require.Equal(t, "vmid:lxc/200", identity(t, lxcResource(), map[string]string{"hostname": "db-1"}))
	require.Equal(t, "vmid:lxc/200", identity(t, lxcResource(), map[string]string{
		"hostname": "db-1",
		"net0":     "name=eth0,bridge=vmbr0",
	}))
}

func TestBuildGuestIdentityDiffersAcrossVMIDs(t *testing.T) {
	a := qemuResource()
	b := qemuResource()
	b.VMID = 102
	require.NotEqual(t, identity(t, a, nil), identity(t, b, nil))
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
