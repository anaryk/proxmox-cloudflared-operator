package resolve

import (
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

func TestTheSegmentOfAHostInterface(t *testing.T) {
	bridges := map[string]bool{"vmbr0": true, "vmbr1": true, "vmbr0v20": true, "lanv": true, "vmbr9v3": true}
	tests := []struct {
		name   string
		link   linkInfo
		want   Segment
		reason string
	}{
		{name: "a bridge", link: linkInfo{name: "vmbr1", bridge: true}, want: Segment{Bridge: "vmbr1"}},
		{name: "a VLAN interface on a bridge", link: linkInfo{name: "vmbr0.20", vlanParent: "vmbr0", vlanID: 20}, want: Segment{Bridge: "vmbr0", VLAN: 20}},
		{
			name: "a VLAN interface named otherwise", link: linkInfo{name: "srv", vlanParent: "vmbr1", vlanID: 7},
			want: Segment{Bridge: "vmbr1", VLAN: 7},
		},
		{name: "the bridge Proxmox makes for a VLAN", link: linkInfo{name: "vmbr0v20", bridge: true}, want: Segment{Bridge: "vmbr0", VLAN: 20}},
		{
			name: "a bridge named like one for a VLAN of no bridge", link: linkInfo{name: "vmbr9v3", bridge: true},
			want: Segment{Bridge: "vmbr9v3"},
		},
		{name: "a bridge whose name ends in v", link: linkInfo{name: "lanv", bridge: true}, want: Segment{Bridge: "lanv"}},
		{name: "a bridge for VLAN 0", link: linkInfo{name: "vmbr0v0", bridge: true}, want: Segment{Bridge: "vmbr0v0"}},
		{name: "a bridge for a VLAN out of range", link: linkInfo{name: "vmbr0v4095", bridge: true}, want: Segment{Bridge: "vmbr0v4095"}},
		{name: "a bridge for a VLAN with a sign", link: linkInfo{name: "vmbr0v+5", bridge: true}, want: Segment{Bridge: "vmbr0v+5"}},
		{name: "a port of a bridge", link: linkInfo{name: "eno1"}},
		{name: "a VLAN interface on a port", link: linkInfo{name: "eno1.20", vlanParent: "eno1", vlanID: 20}},
		{name: "a VLAN interface without a VLAN", link: linkInfo{name: "vmbr0.x", vlanParent: "vmbr0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, segmentOf(tt.link, bridges))
		})
	}
}

func TestASegmentIsNamedWithItsVLAN(t *testing.T) {
	require.Equal(t, "vmbr1", Segment{Bridge: "vmbr1"}.String())
	require.Equal(t, "vmbr1 VLAN 20", Segment{Bridge: "vmbr1", VLAN: 20}.String())
	require.True(t, Segment{}.IsZero())
	require.False(t, Segment{Bridge: "vmbr1"}.IsZero())
}

// containerScenario is the appliance: its eth0 is mapped to the untagged
// segment of vmbr1, where web-1 has net0, and no bridge table is visible.
func containerScenario(t *testing.T) *scenario {
	s := newScenario(t)
	s.settings.NoForwardingTable = true
	s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr1", 0, "10.20.0.10")}
	s.prober.ifaces = []HostIface{{Name: "eth0", Addrs: prefixes("10.20.0.2/24"), Segment: Segment{Bridge: "vmbr1"}}}
	s.prober.gateway = "eth0"
	s.prober.arp = map[string][]string{arpKey("eth0", "10.20.0.10"): {mac0}}
	return s
}

func TestResolveARPsOnTheInterfaceOfTheNICsSegment(t *testing.T) {
	s := containerScenario(t)

	res := s.resolve(t, webRoute(), nil)

	requireServedAt(t, res, "10.20.0.10", t0, LevelObserved)
	require.Equal(t, Segment{Bridge: "vmbr1"}, res.Binding.Segment, "the binding keeps the segment the address was proven on")
	require.Equal(t, []probeCall{
		{op: "interfaces"},
		{op: "route", addr: ip("10.20.0.10")},
		{op: "arp", iface: "eth0", addr: ip("10.20.0.10")},
		{op: "dial", addr: ip("10.20.0.10"), port: 80},
	}, s.prober.calls, "no forwarding table is asked")
}

func TestResolveFindsTheVLANOfASegment(t *testing.T) {
	tests := []struct {
		name      string
		iface     HostIface
		fdbBridge string
		fdbVLAN   int
	}{
		{
			name: "vlan interface", iface: HostIface{Name: "vmbr0.10", Addrs: prefixes("10.30.0.2/24"), Segment: Segment{Bridge: "vmbr0", VLAN: 10}},
			fdbBridge: "vmbr0", fdbVLAN: 10,
		},
		{
			name: "bridge of the vlan", iface: HostIface{Name: "vmbr0v10", Addrs: prefixes("10.30.0.2/24"), Segment: Segment{Bridge: "vmbr0", VLAN: 10}},
			fdbBridge: "vmbr0v10",
		},
		{
			name: "container interface on the vlan", iface: HostIface{Name: "eth1", Addrs: prefixes("10.30.0.2/24"), Segment: Segment{Bridge: "vmbr0", VLAN: 10}},
			fdbBridge: "vmbr0", fdbVLAN: 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 10, "10.30.0.10")}
			s.prober.ifaces = []HostIface{{Name: "vmbr0", Addrs: prefixes("10.20.0.2/24"), Segment: Segment{Bridge: "vmbr0"}}, tt.iface}
			s.prober.arp[arpKey(tt.iface.Name, "10.30.0.10")] = []string{mac0}
			s.prober.fdb[fdbKey(tt.fdbBridge, tt.fdbVLAN, mac0)] = []string{"tap101i0"}

			res := s.resolve(t, webRoute(), nil)

			requireServed(t, res, "10.30.0.10", t0)
			require.Equal(t, []probeCall{{op: "arp", iface: tt.iface.Name, addr: ip("10.30.0.10")}}, s.prober.ops("arp"))
			require.Equal(t, Segment{Bridge: "vmbr0", VLAN: 10}, res.Binding.Segment)
		})
	}
}

// Of two interfaces on one segment, the one Proxmox names for it comes first,
// as before segments were known.
func TestResolvePrefersTheInterfaceProxmoxNames(t *testing.T) {
	s := newScenario(t)
	s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 10, "10.30.0.10")}
	on := Segment{Bridge: "vmbr0", VLAN: 10}
	s.prober.ifaces = []HostIface{
		{Name: "eth9", Addrs: prefixes("10.30.0.3/24"), Segment: on},
		{Name: "vmbr0v10", Addrs: prefixes("10.30.0.4/24"), Segment: on},
		{Name: "vmbr0.10", Addrs: prefixes("10.30.0.2/24"), Segment: on},
	}
	s.prober.routes["10.30.0.10"] = fakeRoute{iface: "vmbr0.10", onLink: true}
	s.prober.arp[arpKey("vmbr0.10", "10.30.0.10")] = []string{mac0}
	s.prober.fdb[fdbKey("vmbr0", 10, mac0)] = []string{"tap101i0"}

	res := s.resolve(t, webRoute(), nil)

	requireServed(t, res, "10.30.0.10", t0)
	require.Equal(t, []probeCall{{op: "arp", iface: "vmbr0.10", addr: ip("10.30.0.10")}}, s.prober.ops("arp"))
}

// The name of an interface counts only for one without a segment: an
// interface mapped to another segment is not the NIC's, whatever its name.
func TestResolveTakesNoInterfaceOfAnotherSegmentByItsName(t *testing.T) {
	s := containerScenario(t)
	s.prober.ifaces = []HostIface{
		{Name: "vmbr1", Addrs: prefixes("10.20.0.2/24"), Segment: Segment{Bridge: "vmbr9"}},
	}

	res := s.resolve(t, webRoute(), nil)

	requireNotServed(t, res, "node has no address on vmbr1 in the guest's network")
	require.Empty(t, s.prober.ops("arp"))
}

func TestNoForwardingTableCapsEveryProofAtObserved(t *testing.T) {
	tests := []struct {
		name  string
		setup func(s *scenario)
	}{
		{name: "a guest of this node"},
		{name: "a guest of another node", setup: func(s *scenario) { s.web().Node = "pve2" }},
		{name: "a guest of no known node", setup: func(s *scenario) { s.web().Node = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.settings.NoForwardingTable = true
			if tt.setup != nil {
				tt.setup(s)
			}

			res := s.resolve(t, webRoute(), nil)

			requireServedAt(t, res, "10.20.0.10", t0, LevelObserved)
			require.Empty(t, s.prober.ops("fdb"), "the forwarding table is never asked")
			require.Equal(t, Segment{Bridge: "vmbr0"}, res.Binding.Segment)

			s.clock.t = t0.Add(time.Minute)
			again := s.resolve(t, webRoute(), res.Binding)
			requireServedAt(t, again, "10.20.0.10", t0.Add(time.Minute), LevelObserved)
			require.Empty(t, s.prober.ops("fdb"))
		})
	}
}

// Without the forwarding table, a MAC another running guest has too cannot
// be placed, so it never passes.
func TestNoForwardingTableRefusesAMACAnotherGuestHas(t *testing.T) {
	s := newScenario(t)
	s.settings.NoForwardingTable = true
	s.addDB1(true, nicOn(0, mac0, "vmbr0", 0))

	res := s.resolve(t, webRoute(), nil)

	requireNotServed(t, res, "MAC "+mac0+" is also configured on qemu/102")
}

func TestASoftDeniedCandidateIsServedAndMarked(t *testing.T) {
	s := newScenario(t)
	s.deny = s.deny.WithSoft(map[netip.Addr]string{ip("10.20.0.10"): "gateway of node pve1"})

	res := s.resolve(t, webRoute(), nil)

	requireServed(t, res, "10.20.0.10", t0)
	require.Equal(t, "gateway of node pve1", res.SoftDenied)

	s.web().Running = false
	held := s.resolve(t, webRoute(), res.Binding)
	require.True(t, held.Target.Withdrawn)
	require.Equal(t, "gateway of node pve1", held.SoftDenied, "a withdrawn address is still the gateway")
}

func TestAnAddressThatIsNotSoftDeniedIsNotMarked(t *testing.T) {
	s := newScenario(t)
	s.deny = s.deny.WithSoft(map[netip.Addr]string{ip("10.20.0.1"): "gateway of node pve1"})

	res := s.resolve(t, webRoute(), nil)

	requireServed(t, res, "10.20.0.10", t0)
	require.Empty(t, res.SoftDenied)

	s.web().NICs = nil
	none := s.resolve(t, webRoute(), nil)
	require.Empty(t, none.SoftDenied, "a target without an address is nothing")
}

func TestTheSoftDenyOfADenylist(t *testing.T) {
	base := denylist(t, ips("10.20.0.2"), nil)
	entries := map[netip.Addr]string{
		ip("10.20.0.1"):        "gateway of node pve1",
		ip("::ffff:10.20.0.3"): "resolver of node pve1",
		ip("fd00::1"):          "resolver of the appliance",
	}
	d := base.WithSoft(entries)
	entries[ip("10.20.0.9")] = "added later"

	why, soft := d.Soft(ip("10.20.0.1"))
	require.True(t, soft)
	require.Equal(t, "gateway of node pve1", why)
	why, soft = d.Soft(ip("10.20.0.3"))
	require.True(t, soft, "an IPv4-mapped entry is the IPv4 address")
	require.Equal(t, "resolver of node pve1", why)
	_, soft = d.Soft(ip("10.20.0.9"))
	require.False(t, soft, "the entries are copied")
	_, soft = base.Soft(ip("10.20.0.1"))
	require.False(t, soft, "the denylist it was made from is unchanged")

	_, denied := d.Check(ip("10.20.0.1"))
	require.False(t, denied, "a soft entry is no hard deny")
	why, denied = d.Check(ip("10.20.0.2"))
	require.True(t, denied)
	require.Equal(t, "address of a cluster node", why)
}

func TestTheDenylistDeniesAddressesWithTheirReason(t *testing.T) {
	d := denylist(t, ips("10.20.0.2"), nil).WithDenied(map[netip.Addr]string{
		ip("10.50.0.1"): "gateway of SDN subnet 10.50.0.0/24",
	})

	why, denied := d.Check(ip("10.50.0.1"))
	require.True(t, denied)
	require.Equal(t, "gateway of SDN subnet 10.50.0.0/24", why)
	why, denied = d.withoutNodes().Check(ip("10.50.0.1"))
	require.True(t, denied, "only the node addresses are lifted for a route to a node")
	require.Equal(t, "gateway of SDN subnet 10.50.0.0/24", why)
	_, denied = d.withoutNodes().Check(ip("10.20.0.2"))
	require.False(t, denied)
}

func TestABindingKeepsItsSegmentInJSON(t *testing.T) {
	b, err := json.Marshal(Binding{Segment: Segment{Bridge: "vmbr1", VLAN: 20}})
	require.NoError(t, err)
	require.Contains(t, string(b), `"segment":{"bridge":"vmbr1","vlan":20}`)
	var back Binding
	require.NoError(t, json.Unmarshal(b, &back))
	require.Equal(t, Segment{Bridge: "vmbr1", VLAN: 20}, back.Segment)

	b, err = json.Marshal(Binding{})
	require.NoError(t, err)
	require.NotContains(t, string(b), "segment", "a binding on no segment says nothing of one")
}
