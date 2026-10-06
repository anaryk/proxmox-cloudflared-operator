package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// fakeNetworks is the node the admin actions of the networks read, and the
// unit of the gateway namespace they run.
type fakeNetworks struct {
	mu        sync.Mutex
	config    []pve.NodeIface
	vnets     []pve.VNet
	host      []resolve.HostIface
	calls     []string
	enableErr error
}

func (f *fakeNetworks) NodeNetwork(_ context.Context, node string) ([]pve.NodeIface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if node != testNode {
		return nil, fmt.Errorf("node %s is not in this test", node)
	}
	return slices.Clone(f.config), nil
}

func (f *fakeNetworks) VNets(context.Context) ([]pve.VNet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.vnets), nil
}

func (f *fakeNetworks) Interfaces(context.Context) ([]resolve.HostIface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.host), nil
}

func (f *fakeNetworks) StartNamespace(context.Context) error {
	f.record("start")
	return nil
}

func (f *fakeNetworks) EnableNamespace(context.Context) error {
	f.record("enable")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enableErr
}

func (f *fakeNetworks) DisableNamespace(context.Context) error {
	f.record("disable")
	return nil
}

func (f *fakeNetworks) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeNetworks) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// probeGateway is the gateway of the tests of the networks: it answers a
// probe with the MACs a test gives an address, and has no namespace until
// the unit is started when unpinned.
type probeGateway struct {
	*fakeGateway
	nets *fakeNetworks

	pmu      sync.Mutex
	answers  map[netip.Addr][]net.HardwareAddr
	probes   []string
	detached []string
	unpinned bool
}

func (g *probeGateway) Probe(_ context.Context, bridge string, vlan int, addr netip.Addr) ([]net.HardwareAddr, error) {
	if g.unpinned && !slices.Contains(g.nets.called(), "start") {
		return nil, gateway.ErrNotPinned
	}
	g.pmu.Lock()
	defer g.pmu.Unlock()
	g.probes = append(g.probes, fmt.Sprintf("%s %d %s", bridge, vlan, addr))
	return g.answers[addr], nil
}

func (g *probeGateway) Detach(_ context.Context, id string) error {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	g.detached = append(g.detached, id)
	return nil
}

func (g *probeGateway) answer(addr netip.Addr, macs ...string) {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	for _, m := range macs {
		hw, err := net.ParseMAC(m)
		if err != nil {
			panic(err)
		}
		g.answers[addr] = append(g.answers[addr], hw)
	}
}

func (g *probeGateway) probed() []string {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	return slices.Clone(g.probes)
}

func (g *probeGateway) detaches() []string {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	return slices.Clone(g.detached)
}

// netEnv is an env on a node with these bridges: vmbr0 holds the node's
// address and the physical port eno1; vmbr1 is a bridge of its own with web-1
// at 10.77.0.11 and the container db-1 reporting 10.77.0.12; vmbr2 filters
// VLANs, has the bond bond0 and a guest on VLAN 20, and the node is on its
// VLAN 30; vmbr9 holds the node's address 10.50.0.1/24, and pcot1, a bridge
// of its own, has lab-1 at 10.50.0.11. The vnets are pconet, pco's managed
// network, and lab of a VXLAN zone. pcot2 is a bridge made at run time,
// which Proxmox does not list. A guest of another node is on vmbr1, one on
// vmbr0.
type netEnv struct {
	*env
	gw   *probeGateway
	nets *fakeNetworks
	log  *callLog
}

func newNetEnv(t *testing.T) *netEnv {
	t.Helper()
	e, log := orderLogged(t)
	nets := &fakeNetworks{
		config: []pve.NodeIface{
			{Name: "eno1", Type: "eth"},
			{Name: "bond0", Type: "bond"},
			{Name: "vmbr0", Type: "bridge", Ports: []string{"eno1"}, Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.4.207/24")}},
			{Name: "vmbr1", Type: "bridge"},
			{Name: "vmbr2", Type: "bridge", Ports: []string{"bond0"}, VLANAware: true},
			{Name: "vmbr9", Type: "bridge", Addrs: []netip.Prefix{netip.MustParsePrefix("10.50.0.1/24")}},
			{Name: "pcot1", Type: "bridge", Ports: []string{"none"}},
		},
		vnets: []pve.VNet{{Name: "pconet", Zone: "pco"}, {Name: "lab", Zone: "z1"}},
		host: []resolve.HostIface{
			{Name: "vmbr0", Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.4.207/24")}, Segment: resolve.Segment{Bridge: "vmbr0"}},
			{Name: "vmbr1", Segment: resolve.Segment{Bridge: "vmbr1"}},
			{Name: "tap101i0", Master: "vmbr1", Segment: resolve.Segment{Bridge: "vmbr1"}},
			{Name: "vmbr2", Segment: resolve.Segment{Bridge: "vmbr2"}},
			{Name: "vmbr2.30", Addrs: []netip.Prefix{netip.MustParsePrefix("10.30.0.2/24")}, Segment: resolve.Segment{Bridge: "vmbr2", VLAN: 30}},
			{Name: "vmbr9", Addrs: []netip.Prefix{netip.MustParsePrefix("10.50.0.1/24")}, Segment: resolve.Segment{Bridge: "vmbr9"}},
			{Name: "pcot1", Segment: resolve.Segment{Bridge: "pcot1"}},
			{Name: "lab", Segment: resolve.Segment{Bridge: "lab"}},
			{Name: "vxlan_lab", Master: "lab", Segment: resolve.Segment{Bridge: "lab"}},
			{Name: "veth102i0", Master: "pconet"},
			{Name: "pcot2", Segment: resolve.Segment{Bridge: "pcot2"}},
			{Name: "veth104i1", Master: "pcot2", Segment: resolve.Segment{Bridge: "pcot2"}},
			{Name: "fwbr101i0", Segment: resolve.Segment{Bridge: "fwbr101i0"}},
			{Name: "vmbr0v20", Segment: resolve.Segment{Bridge: "vmbr0", VLAN: 20}},
		},
	}
	gw := &probeGateway{
		fakeGateway: &fakeGateway{log: log, down: map[string]string{}},
		nets:        nets, answers: map[netip.Addr][]net.HardwareAddr{},
	}
	e.gateway, e.networks = gw, nets
	e.inv.set(netSnapshot())
	e.eng = e.newEngine()
	return &netEnv{env: e, gw: gw, nets: nets, log: log}
}

func onBridge(g model.Guest, bridge string, vlan int, mac string, static ...string) model.Guest {
	nic := model.NIC{Index: len(g.NICs), MAC: mac, Bridge: bridge, VLAN: vlan}
	for _, s := range static {
		nic.Static = append(nic.Static, netip.MustParseAddr(s))
	}
	g.NICs = append(g.NICs, nic)
	return g
}

func netSnapshot() inventory.Snapshot {
	web := onBridge(guest(101, "web-1", "www.example.com -> :8080"), "vmbr1", 0, "bc:24:11:00:01:01", "10.77.0.11")
	db := onBridge(guest(102, "db-1"), "vmbr1", 0, "bc:24:11:00:01:02")
	db.Ref.Kind = model.KindLXC
	db.Reported = []model.ReportedAddr{{Iface: "eth0", MAC: "BC:24:11:00:01:02", Addr: netip.MustParseAddr("10.77.0.12")}}
	tagged := onBridge(guest(103, "vlan-1"), "vmbr2", 20, "bc:24:11:00:01:03", "10.20.0.5")
	lab := onBridge(guest(104, "lab-1"), "pcot1", 0, "bc:24:11:00:01:04", "10.50.0.11")
	far := onBridge(guest(105, "far-1"), "vmbr1", 0, "bc:24:11:00:01:05", "10.77.0.99")
	far.Node = "pve2"
	lan := onBridge(guest(106, "lan-1"), "vmbr0", 0, "bc:24:11:00:01:06", "192.168.4.50")
	lan.Node = "pve2"
	s := snapshot(web, db, tagged, lab, far, lan)
	s.Nodes = append(s.Nodes, inventory.Node{Name: "pve2", Online: true})
	return s
}

func guestView(kind model.GuestKind, vmid int, name string) GuestView {
	return GuestView{GuestRef: model.GuestRef{Kind: kind, VMID: vmid}, Name: name}
}

// candidateFlags is a candidate in a line: its segment and what it is.
func candidateFlags(c CandidateNetwork) string {
	var flags []string
	for _, f := range []struct {
		on   bool
		name string
	}{{c.Vnet, "vnet"}, {c.VLANAware, "aware"}, {c.Uplink, "uplink"}, {c.Direct, "direct"}} {
		if f.on {
			flags = append(flags, f.name)
		}
	}
	if c.Attached != "" {
		flags = append(flags, "attached="+c.Attached)
	}
	if c.Refused != "" {
		flags = append(flags, "refused")
	}
	return strings.TrimSpace(SegmentArg(c.Bridge, c.VLAN) + " " + strings.Join(flags, " "))
}

func TestTheCandidatesAreTheBridgesAndVLANsOfTheNode(t *testing.T) {
	e := newNetEnv(t)
	saveNetwork(t, e.store, "pcot1", 1)

	got, err := e.eng.CandidateNetworks(t.Context())

	require.NoError(t, err)
	var lines []string
	for _, c := range got {
		lines = append(lines, candidateFlags(c))
	}
	require.Equal(t, []string{
		"lab vnet uplink",
		"pconet vnet refused",
		"pcot1 attached=pcot1",
		"pcot2",
		"vmbr0 uplink direct",
		"vmbr0:20 uplink",
		"vmbr1",
		"vmbr2 aware uplink",
		"vmbr2:20 aware uplink",
		"vmbr2:30 aware uplink direct",
		"vmbr9 direct",
	}, lines)

	vmbr1 := got[slices.IndexFunc(got, func(c CandidateNetwork) bool { return c.Bridge == "vmbr1" })]
	require.Equal(t, CandidateNetwork{
		Bridge: "vmbr1",
		Guests: []GuestAddr{
			{Guest: guestView(model.KindQEMU, 101, "web-1"), Addr: netip.MustParseAddr("10.77.0.11")},
			{Guest: guestView(model.KindLXC, 102, "db-1"), Addr: netip.MustParseAddr("10.77.0.12")},
		},
		Prefixes: []netip.Prefix{netip.MustParsePrefix("10.77.0.0/24")},
	}, vmbr1, "the guest of another node is on another bridge of that name")
	require.Equal(t, "pconet is pco's managed network, which is not attached", got[1].Refused)

	vmbr0 := got[slices.IndexFunc(got, func(c CandidateNetwork) bool { return c.Bridge == "vmbr0" })]
	require.Equal(t, []GuestAddr{{Guest: guestView(model.KindQEMU, 106, "lan-1"), Addr: netip.MustParseAddr("192.168.4.50")}}, vmbr0.Guests,
		"a bridge with an uplink is on the LAN of every node")
}

func TestTheSuggestedPrefixesAreTheMostCommonFirst(t *testing.T) {
	addrs := func(list ...string) []GuestAddr {
		var out []GuestAddr
		for _, a := range list {
			out = append(out, GuestAddr{Addr: netip.MustParseAddr(a)})
		}
		return out
	}

	require.Equal(t, []netip.Prefix{netip.MustParsePrefix("10.1.2.0/24"), netip.MustParsePrefix("10.1.1.0/24")},
		suggested(addrs("10.1.1.5", "10.1.2.7", "10.1.2.9", "10.1.2.9")))
	require.Equal(t, []netip.Prefix{netip.MustParsePrefix("10.1.1.0/24"), netip.MustParsePrefix("10.1.2.0/24")},
		suggested(addrs("10.1.2.7", "10.1.1.5")), "a tie goes by address")
	require.Empty(t, suggested(nil))
}

func TestTheApplianceRefusesWhatItsCardsCannotReach(t *testing.T) {
	e := newNetEnv(t)
	require.NoError(t, e.store.SaveInstall(applianceInstall()))
	e.nets.config = append(e.nets.config,
		pve.NodeIface{Name: "vmbr0v20", Type: "bridge"},
		pve.NodeIface{Name: "vmbr3", Type: "bridge"})
	snap := netSnapshot()
	self := model.Guest{Ref: model.GuestRef{Kind: model.KindLXC, VMID: 9200}, Name: "pco", Node: testNode, Running: true}
	for i := range 10 {
		self.NICs = append(self.NICs, model.NIC{Index: i, MAC: fmt.Sprintf("bc:24:11:00:92:%02x", i), Bridge: "vmbr0"})
	}
	snap.Guests = append(snap.Guests, onBridge(guest(107, "tagged"), "vmbr1", 20, "bc:24:11:00:01:07", "10.21.0.5"), self)
	e.inv.set(snap)
	saveNetwork(t, e.store, "vmbr3", 1)

	got, err := e.eng.CandidateNetworks(t.Context())

	require.NoError(t, err)
	refused := map[string]string{}
	for _, c := range got {
		refused[SegmentArg(c.Bridge, c.VLAN)] = c.Refused
	}
	require.Equal(t, "bridge vmbr0v20 is one Proxmox makes for a VLAN; attach the network on its bridge with the tag", refused["vmbr0v20"])
	require.Equal(t, "bridge vmbr1 is not VLAN-aware; the appliance attaches to VLAN 20 of a VLAN-aware bridge only", refused["vmbr1:20"])
	require.Equal(t, "no free NIC slot (net1..net9): the appliance has a network card at each", refused["vmbr1"])
	require.Empty(t, refused["vmbr3"], "an attached network has its card")

	t.Run("with a free card", func(t *testing.T) {
		self.NICs = slices.Delete(self.NICs, 4, 5)
		snap.Guests[len(snap.Guests)-1] = self
		e.inv.set(snap)

		got, err := e.eng.CandidateNetworks(t.Context())

		require.NoError(t, err)
		i := slices.IndexFunc(got, func(c CandidateNetwork) bool { return c.Bridge == "vmbr1" && c.VLAN == 0 })
		require.Empty(t, got[i].Refused)
	})
}

func TestAProbeProposesTheHighestAddressNoGuestHas(t *testing.T) {
	e := newNetEnv(t)
	snap := netSnapshot()
	snap.Guests = append(snap.Guests, onBridge(guest(108, "top"), "vmbr1", 0, "bc:24:11:00:01:08", "10.77.0.254"))
	e.inv.set(snap)

	res, err := e.eng.ProbeNetwork(t.Context(), ProbeRequest{Bridge: "vmbr1"})

	require.NoError(t, err)
	require.Equal(t, ProbeResult{
		Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"), Address: netip.MustParseAddr("10.77.0.253"),
		Free: true, AnsweredBy: []string{},
		Note: "10.77.0.0/24 holds 3 of the 3 guest addresses known on vmbr1; " +
			"10.77.0.253 is the highest address of 10.77.0.0/24 that no guest is known to have",
	}, res)
	require.Equal(t, []string{"vmbr1 0 10.77.0.253"}, e.gw.probed())
}

func TestAProbeOfOneGuestAddressSaysThatA24IsAssumed(t *testing.T) {
	e := newNetEnv(t)

	res, err := e.eng.ProbeNetwork(t.Context(), ProbeRequest{Bridge: "pcot1"})

	require.NoError(t, err)
	require.Equal(t, netip.MustParsePrefix("10.50.0.0/24"), res.Prefix)
	require.Equal(t, netip.MustParseAddr("10.50.0.254"), res.Address)
	require.Contains(t, res.Note, "a /24 is assumed: only 10.50.0.11 is known on pcot1")
	require.Equal(t, "10.50.0.0/24 overlaps the node's route on vmbr9", res.Overlap)
}

func TestAProbeSaysWhoAnswered(t *testing.T) {
	e := newNetEnv(t)
	addr := netip.MustParseAddr("10.20.0.250")
	e.gw.answer(addr, "52:54:00:12:34:56", "52:54:00:12:34:55")

	res, err := e.eng.ProbeNetwork(t.Context(), ProbeRequest{Bridge: "vmbr2", VLAN: 20, Prefix: netip.MustParsePrefix("10.20.0.0/24"), Address: addr})

	require.NoError(t, err)
	require.False(t, res.Free)
	require.Equal(t, []string{"52:54:00:12:34:55", "52:54:00:12:34:56"}, res.AnsweredBy)
	require.True(t, res.Uplink)
	require.Equal(t, "the leg's MAC becomes visible on the LAN of vmbr2", res.Warning)
	require.Empty(t, res.Note, "the request named the prefix and the address")
}

func TestAProbeStartsTheNamespaceWhenItIsNotThere(t *testing.T) {
	e := newNetEnv(t)
	e.gw.unpinned = true

	res, err := e.eng.ProbeNetwork(t.Context(), ProbeRequest{Bridge: "vmbr1"})

	require.NoError(t, err)
	require.True(t, res.Free)
	require.Equal(t, []string{"start"}, e.nets.called(), "started, not enabled: a probe attaches nothing")
}

func TestAProbeIsRefusedWhatAnAttachIsAlways(t *testing.T) {
	for _, tt := range []struct {
		name  string
		req   ProbeRequest
		is    error
		field string
		msg   string
	}{
		{"no bridge", ProbeRequest{}, ErrInvalid, "bridge", "bridge: name a bridge or a vnet"},
		{"a bridge the node does not have", ProbeRequest{Bridge: "vmbr7"}, ErrInvalid, "bridge", "vmbr7 is no bridge or vnet of node pve1"},
		{"a VLAN out of range", ProbeRequest{Bridge: "vmbr1", VLAN: 4095}, ErrInvalid, "vlan", "vlan 4095: want 0 for untagged, or 1 to 4094"},
		{"the node is on it", ProbeRequest{Bridge: "vmbr0"}, ErrRefused, "", "refused: the node reaches vmbr0 directly; nothing to attach"},
		{"the managed network", ProbeRequest{Bridge: "pconet"}, ErrRefused, "", "refused: pconet is pco's managed network, which is not attached"},
		{"no guest address to go by", ProbeRequest{Bridge: "vmbr2"}, ErrInvalid, "prefix",
			"no guest address is known on vmbr2; give the prefix of the network, as 10.77.0.0/24"},
		{"an address outside the prefix", ProbeRequest{Bridge: "vmbr1", Address: netip.MustParseAddr("10.78.0.5")}, ErrInvalid, "address",
			"address 10.78.0.5 is not in 10.77.0.0/24"},
		{"a prefix that is not a network", ProbeRequest{Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.5/24")}, ErrInvalid, "prefix",
			"prefix 10.77.0.5/24: want the address of the network, 10.77.0.0/24"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newNetEnv(t)

			_, err := e.eng.ProbeNetwork(t.Context(), tt.req)

			require.ErrorIs(t, err, tt.is)
			require.EqualError(t, err, tt.msg)
			var fe *FieldError
			if tt.field == "" {
				require.False(t, errors.As(err, &fe))
			} else {
				require.ErrorAs(t, err, &fe)
				require.Equal(t, tt.field, fe.Field)
			}
			require.Empty(t, e.gw.probed())
		})
	}
}

func TestNoNetworkIsAttachedWhereNoGatewayRuns(t *testing.T) {
	e := newNetEnv(t)
	e.gateway = nil
	e.eng = e.newEngine()

	_, err := e.eng.Attach(t.Context(), AttachRequest{Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"), Address: netip.MustParseAddr("10.77.0.250")})

	require.ErrorIs(t, err, ErrRefused)
	require.EqualError(t, err, "refused: no gateway runs on this node; no network can be attached here")
	_, err = e.eng.ProbeNetwork(t.Context(), ProbeRequest{Bridge: "vmbr1"})
	require.ErrorIs(t, err, ErrRefused)
}

func attachVmbr1() AttachRequest {
	return AttachRequest{Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"), Address: netip.MustParseAddr("10.77.0.250")}
}

func TestAnAttachIsRefused(t *testing.T) {
	for _, tt := range []struct {
		name  string
		req   AttachRequest
		setup func(e *netEnv)
		is    error
		field string
		msg   string
	}{
		{"a bridge the node is on", AttachRequest{Bridge: "vmbr0", Prefix: netip.MustParsePrefix("192.168.4.0/24"), Address: netip.MustParseAddr("192.168.4.250")},
			nil, ErrRefused, "", "refused: the node reaches vmbr0 directly; nothing to attach"},
		{"a VLAN the node is on", AttachRequest{Bridge: "vmbr2", VLAN: 30, Prefix: netip.MustParsePrefix("10.30.0.0/24"), Address: netip.MustParseAddr("10.30.0.250"), ConfirmUplink: true},
			nil, ErrRefused, "", "refused: the node reaches vmbr2 VLAN 30 directly; nothing to attach"},
		{"an uplink without the confirmation", AttachRequest{Bridge: "vmbr2", VLAN: 20, Prefix: netip.MustParsePrefix("10.20.0.0/24"), Address: netip.MustParseAddr("10.20.0.250")},
			nil, ErrInvalid, "confirmUplink",
			"vmbr2 VLAN 20 has a physical uplink: the leg's MAC becomes visible on the LAN of vmbr2; confirm it with --confirm-uplink, with an address reserved for pco on that LAN"},
		{"the service prefix", AttachRequest{Bridge: "vmbr1", Prefix: netip.MustParsePrefix("198.18.4.0/24"), Address: netip.MustParseAddr("198.18.4.250")},
			nil, ErrInvalid, "prefix", "198.18.4.0/24 overlaps the service prefix 198.18.0.0/16, whose addresses the connectors dial"},
		{"the managed subnet", AttachRequest{Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.203.118.0/25"), Address: netip.MustParseAddr("10.203.118.100")},
			nil, ErrInvalid, "prefix", "10.203.118.0/25 overlaps the managed subnet 10.203.118.0/24 (managed.subnet)"},
		{"a route of the node", AttachRequest{Bridge: "pcot1", Prefix: netip.MustParsePrefix("10.50.0.0/24"), Address: netip.MustParseAddr("10.50.0.249")},
			nil, ErrInvalid, "overlapOK",
			"10.50.0.0/24 overlaps the node's route on vmbr9; pass --overlap-ok if the guests there are meant to be reached through the leg"},
		{"another network's prefix on the same bridge", AttachRequest{Bridge: "vmbr2", VLAN: 20, Prefix: netip.MustParsePrefix("10.20.0.0/24"), Address: netip.MustParseAddr("10.20.0.250"), ConfirmUplink: true},
			func(e *netEnv) {
				require.NoError(t, e.store.SaveNetwork(store.Network{
					ID: "vmbr2.40", Kind: store.NetworkAttach, Bridge: "vmbr2", VLAN: 40, Prefix: netip.MustParsePrefix("10.20.0.0/23"),
					Legs: map[string]netip.Addr{testNode: netip.MustParseAddr("10.20.1.250")}, LegMAC: store.LegMACFor(store.NetworkAttach, 1), Mark: 1, CreatedAt: t0,
				}, 0))
			}, ErrInvalid, "prefix", "10.20.0.0/24 overlaps 10.20.0.0/23 of network vmbr2.40 on the same bridge vmbr2"},
		{"an address outside the prefix", AttachRequest{Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"), Address: netip.MustParseAddr("10.77.1.250")},
			nil, ErrInvalid, "address", "address 10.77.1.250 is not in 10.77.0.0/24"},
		{"the broadcast address", AttachRequest{Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"), Address: netip.MustParseAddr("10.77.0.255")},
			nil, ErrInvalid, "address", "address 10.77.0.255 is the network or the broadcast address of 10.77.0.0/24"},
		{"an address of a guest", AttachRequest{Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"), Address: netip.MustParseAddr("10.77.0.12")},
			nil, ErrInvalid, "address", "10.77.0.12 is an address of lxc/102 (db-1)"},
		{"an address in use", attachVmbr1(),
			func(e *netEnv) { e.gw.answer(netip.MustParseAddr("10.77.0.250"), "52:54:00:12:34:56") },
			ErrInvalid, "address", "10.77.0.250 is in use on vmbr1: 52:54:00:12:34:56 answered"},
		{"a network attached already", attachVmbr1(),
			func(e *netEnv) { saveNetwork(t, e.store, "vmbr1", 1) },
			ErrRefused, "", "refused: network vmbr1 is attached already; detach it first to attach it anew"},
		{"the managed network", AttachRequest{Bridge: "pconet", Prefix: netip.MustParsePrefix("10.77.0.0/24"), Address: netip.MustParseAddr("10.77.0.250")},
			nil, ErrRefused, "", "refused: pconet is pco's managed network, which is not attached"},
		{"a prefix of no room", AttachRequest{Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/31"), Address: netip.MustParseAddr("10.77.0.1")},
			nil, ErrInvalid, "prefix", "prefix 10.77.0.0/31: a network longer than /30 leaves no address for the leg beside its guests"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newNetEnv(t)
			if tt.setup != nil {
				tt.setup(e)
			}
			before, err := e.store.Networks()
			require.NoError(t, err)

			_, err = e.eng.Attach(t.Context(), tt.req)

			require.ErrorIs(t, err, tt.is)
			require.EqualError(t, err, tt.msg)
			var fe *FieldError
			if tt.field == "" {
				require.False(t, errors.As(err, &fe))
			} else {
				require.ErrorAs(t, err, &fe)
				require.Equal(t, tt.field, fe.Field)
			}
			after, err := e.store.Networks()
			require.NoError(t, err)
			require.Equal(t, before, after, "nothing is saved")
			require.Empty(t, e.nets.called(), "the unit is left alone")
		})
	}
}

func TestAnAttachOverARouteOfTheNodeIsAcceptedWithOverlapOK(t *testing.T) {
	e := newNetEnv(t)
	req := AttachRequest{Bridge: "pcot1", Prefix: netip.MustParsePrefix("10.50.0.0/24"), Address: netip.MustParseAddr("10.50.0.249"), OverlapOK: true}
	ctx := WithActor(t.Context(), "root (cli)")

	v, err := e.eng.Attach(ctx, req)

	require.NoError(t, err)
	require.True(t, v.OverlapOK, "the network records that it was attached over a route of the node")
	stored, err := e.store.Networks()
	require.NoError(t, err)
	require.True(t, stored["pcot1"].OverlapOK)
	st := e.cycle()
	require.Len(t, st.Networks, 1)
	require.True(t, st.Networks[0].OverlapOK, "the state shows it")
	require.Contains(t, unnumbered(e.eng.Events(time.Time{})), Event{
		At: t0, Level: levelInfo, Kind: kindNetwork, Subject: "pcot1", Actor: "root (cli)",
		Message: "network pcot1 is attached: the leg leg1 with 10.50.0.249/24 on pcot1, mark 1; " +
			"10.50.0.0/24 overlaps the node's route on vmbr9, allowed with --overlap-ok; the next cycle raises the leg",
	})

	t.Run("not recorded where nothing overlaps", func(t *testing.T) {
		req := attachVmbr1()
		req.OverlapOK = true

		v, err := e.eng.Attach(t.Context(), req)

		require.NoError(t, err)
		require.False(t, v.OverlapOK)
	})
}

func TestAnAttachSavesTheNetworkAndEnablesTheUnitForTheFirst(t *testing.T) {
	e := newNetEnv(t)
	e.gw.unpinned = true

	v, err := e.eng.Attach(t.Context(), attachVmbr1())

	require.NoError(t, err)
	require.Equal(t, []string{"start", "enable"}, e.nets.called(), "the probe starts the namespace, the first network enables its unit")
	require.Equal(t, []string{"vmbr1 0 10.77.0.250"}, e.gw.probed())
	want := store.Network{
		ID: "vmbr1", Kind: store.NetworkAttach, Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"),
		Legs: map[string]netip.Addr{testNode: netip.MustParseAddr("10.77.0.250")}, LegMAC: "02:50:43:4f:00:01", Mark: 1, CreatedAt: t0, Rev: 1,
	}
	require.Equal(t, want, v.Network)
	require.Equal(t, []LegView{{Node: testNode, Addr: netip.MustParseAddr("10.77.0.250"), Problem: "the gateway has not reported the leg yet"}}, v.Legs)
	stored, err := e.store.Networks()
	require.NoError(t, err)
	require.Equal(t, want, stored["vmbr1"])
	select {
	case <-e.eng.trigger:
	default:
		t.Fatal("an attach asks for a cycle")
	}

	t.Run("the second takes the next mark and leaves the unit alone", func(t *testing.T) {
		v, err := e.eng.Attach(t.Context(), AttachRequest{
			Bridge: "vmbr2", VLAN: 20, Prefix: netip.MustParsePrefix("10.20.0.0/24"), Address: netip.MustParseAddr("10.20.0.250"), ConfirmUplink: true,
		})

		require.NoError(t, err)
		require.Equal(t, "vmbr2.20", v.ID)
		require.Equal(t, 2, v.Mark)
		require.Equal(t, "02:50:43:4f:00:02", v.LegMAC)
		require.True(t, v.Uplink, "the network records the uplink it was confirmed on")
		require.Equal(t, []string{"start", "enable"}, e.nets.called())
	})
}

// A bridge made at run time, which Proxmox does not list, is attached as
// one it lists; one the node does not have is refused.
func TestABridgeMadeAtRunTimeIsAttached(t *testing.T) {
	e := newNetEnv(t)

	v, err := e.eng.Attach(t.Context(), AttachRequest{Bridge: "pcot2", Prefix: netip.MustParsePrefix("10.60.0.0/24"), Address: netip.MustParseAddr("10.60.0.250")})

	require.NoError(t, err)
	require.Equal(t, "pcot2", v.ID)
	require.False(t, v.Uplink, "its one port is a guest's")

	_, err = e.eng.Attach(t.Context(), AttachRequest{Bridge: "fwbr101i0", Prefix: netip.MustParsePrefix("10.61.0.0/24"), Address: netip.MustParseAddr("10.61.0.250")})
	require.EqualError(t, err, "fwbr101i0 is no bridge or vnet of node pve1", "the bridge of a guest's firewall is no network")
}

func TestAnAttachThatCannotEnableTheUnitSavesNothing(t *testing.T) {
	e := newNetEnv(t)
	e.nets.enableErr = errors.New("systemctl: Access denied")

	_, err := e.eng.Attach(t.Context(), attachVmbr1())

	require.EqualError(t, err, "enabling pco-netns.service for the first network: systemctl: Access denied")
	nets, err := e.store.Networks()
	require.NoError(t, err)
	require.Empty(t, nets)
}

func TestAMarkFreedIsNotTakenBeforeTheGatewayIsSynced(t *testing.T) {
	e := newNetEnv(t)
	for mark := 1; mark <= gateway.MaxMark; mark++ {
		id := fmt.Sprintf("pct%d", mark)
		e.nets.config = append(e.nets.config, pve.NodeIface{Name: id, Type: "bridge"})
		saveNetwork(t, e.store, id, mark)
	}
	_, err := e.eng.Attach(t.Context(), attachVmbr1())
	require.ErrorIs(t, err, ErrRefused)
	require.EqualError(t, err, "refused: this node reaches 15 networks already, the most it can: the SNAT through each takes a block of 4096 source ports")

	nets, err := e.store.Networks()
	require.NoError(t, err)
	require.NoError(t, e.eng.Detach(t.Context(), "pct3", nets["pct3"].Rev, false))

	_, err = e.eng.Attach(t.Context(), attachVmbr1())
	require.EqualError(t, err, "refused: the marks free now were freed by a detach the gateway has not been synced since; attach again after the next cycle")

	e.cycle()
	v, err := e.eng.Attach(t.Context(), attachVmbr1())
	require.NoError(t, err)
	require.Equal(t, 3, v.Mark)
}

// served is a netEnv whose web-1 is served through the leg of vmbr1: the
// network attached at 10.77.0.250, and www.example.com proven at 10.77.0.11
// through it.
func served(t *testing.T) *netEnv {
	t.Helper()
	e := newNetEnv(t)
	e.enforce()
	_, err := e.eng.Attach(t.Context(), attachVmbr1())
	require.NoError(t, err)
	e.res.goVia(legHost, "vmbr1")
	e.res.moveTo(legHost, netip.MustParseAddr("10.77.0.11"))
	e.cycle()
	e.clock.advance(20 * time.Second)
	st := e.cycle()
	require.Equal(t, "attach:vmbr1", route(st, legHost).Path.Via, st.Problems)
	return e
}

func TestTheStateShowsTheNetworks(t *testing.T) {
	e := served(t)

	st := e.eng.State()

	require.Len(t, st.Networks, 1)
	v := st.Networks[0]
	require.Equal(t, "vmbr1", v.ID)
	require.Equal(t, []LegView{{Node: testNode, Addr: netip.MustParseAddr("10.77.0.250"), Up: true}}, v.Legs)
	require.Equal(t, 1, v.Targets)
	require.Equal(t, 1, v.Routes)

	views, err := e.eng.Networks()
	require.NoError(t, err)
	require.Equal(t, st.Networks, views)

	d, err := e.eng.Network("vmbr1")
	require.NoError(t, err)
	require.Equal(t, v, d.NetworkView)
	require.Equal(t, []string{legHost}, d.Served)
	require.Len(t, d.Services, 1)
	require.Equal(t, svc0, d.Services[0].Service)
	require.Equal(t, netip.MustParseAddr("10.77.0.11"), d.Services[0].Addr)

	_, err = e.eng.Network("vmbr7")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestADetachWhileRoutesAreServedThroughItNeedsAConfirmation(t *testing.T) {
	e := served(t)
	n, err := e.store.Networks()
	require.NoError(t, err)
	rev := n["vmbr1"].Rev

	err = e.eng.Detach(t.Context(), "vmbr1", rev, false)

	require.ErrorIs(t, err, ErrRefused)
	require.EqualError(t, err, "refused: network vmbr1 serves 1 route through its leg: www.example.com; "+
		"detaching it stops serving them that way, confirm to detach it all the same")
	require.Empty(t, e.gw.detaches())

	ctx := WithActor(t.Context(), "alice@pve (ticket)")
	require.NoError(t, e.eng.Detach(ctx, "vmbr1", rev, true))

	require.Equal(t, []string{"vmbr1"}, e.gw.detaches())
	nets, err := e.store.Networks()
	require.NoError(t, err)
	require.Empty(t, nets)
	mappings, err := e.store.ServiceAddrs()
	require.NoError(t, err)
	require.Empty(t, mappings, "the service addresses of the network are released")
	require.Equal(t, []string{"enable", "disable"}, e.nets.called(), "the last network disables the unit")
	require.Contains(t, unnumbered(e.eng.Events(time.Time{})), Event{
		At: e.clock.now(), Level: levelInfo, Kind: kindNetwork, Subject: "vmbr1", Actor: "alice@pve (ticket)",
		Message: "network vmbr1 is detached: its leg leg1 is removed and 1 service address released; it served www.example.com; " +
			"no network is left, and pco-netns.service is disabled",
	})
}

func TestADetachOfANetworkThatServesNothing(t *testing.T) {
	e := newNetEnv(t)
	saveNetwork(t, e.store, "pcot1", 1)
	saveNetwork(t, e.store, "vmbr1", 2)
	nets, err := e.store.Networks()
	require.NoError(t, err)

	require.NoError(t, e.eng.Detach(t.Context(), "pcot1", nets["pcot1"].Rev, false))

	require.Equal(t, []string{"pcot1"}, e.gw.detaches())
	require.Empty(t, e.nets.called(), "a network is left: the unit stays enabled")
}

func TestADetachIsRefused(t *testing.T) {
	e := newNetEnv(t)
	saveNetwork(t, e.store, "vmbr1", 1)
	nets, err := e.store.Networks()
	require.NoError(t, err)
	rev := nets["vmbr1"].Rev

	err = e.eng.Detach(t.Context(), "vmbr1", rev+1, true)
	require.ErrorIs(t, err, ErrRefused)
	require.EqualError(t, err, fmt.Sprintf("refused: network vmbr1 changed since it was read at revision %d and is at revision %d now; read it again", rev+1, rev))

	err = e.eng.Detach(t.Context(), "vmbr7", 1, true)
	require.ErrorIs(t, err, ErrNotFound)
	require.EqualError(t, err, "not found: network vmbr7 is not attached")

	err = e.eng.Detach(t.Context(), "", 1, true)
	require.ErrorIs(t, err, ErrInvalid)

	require.Empty(t, e.gw.detaches())
	nets, err = e.store.Networks()
	require.NoError(t, err)
	require.Len(t, nets, 1)
}

func TestTheLegsAreEvents(t *testing.T) {
	up := gateway.LegState{ID: "vmbr1", Iface: "leg1", Up: true}
	down := gateway.LegState{ID: "vmbr1", Iface: "leg1", Problem: "bridge vmbr1 is absent"}
	noted := gateway.LegState{ID: "vmbr1", Iface: "leg1", Up: true, Problem: "no carrier, DAD not run"}

	for _, tt := range []struct {
		name          string
		before, after []gateway.LegState
		want          []Event
	}{
		{"raised", nil, []gateway.LegState{up}, []Event{{At: t0, Level: levelInfo, Kind: kindNetwork, Subject: "vmbr1", Message: "the leg leg1 of network vmbr1 is up"}}},
		{"as it was", []gateway.LegState{up}, []gateway.LegState{up}, nil},
		{"gone down", []gateway.LegState{up}, []gateway.LegState{down}, []Event{{At: t0, Level: levelWarn, Kind: kindNetwork, Subject: "vmbr1",
			Message: "the leg leg1 of network vmbr1 is not up: bridge vmbr1 is absent"}}},
		{"down as it was", []gateway.LegState{down}, []gateway.LegState{down}, nil},
		{"up with a problem", []gateway.LegState{up}, []gateway.LegState{noted}, []Event{{At: t0, Level: levelWarn, Kind: kindNetwork, Subject: "vmbr1",
			Message: "the leg leg1 of network vmbr1 is up, but no carrier, DAD not run"}}},
		{"back up", []gateway.LegState{down}, []gateway.LegState{up}, []Event{{At: t0, Level: levelInfo, Kind: kindNetwork, Subject: "vmbr1", Message: "the leg leg1 of network vmbr1 is up"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, legEvents(tt.before, tt.after, t0))
		})
	}
}

func TestACycleSaysWhenALegComesUp(t *testing.T) {
	e := served(t)

	events := unnumbered(e.eng.Events(time.Time{}))

	require.Contains(t, events, Event{At: t0, Level: levelInfo, Kind: kindNetwork, Subject: "vmbr1", Message: "the leg leg1 of network vmbr1 is up"})
}

func testLease() store.Lease {
	return store.Lease{MAC: "06:50:43:4f:76:02", Addr: netip.MustParseAddr("10.203.118.2"), Owner: "qemu/101", Identity: "uuid:101", NIC: 1, Since: t0}
}

// saveSubnet saves the settings with the managed subnet changed, at the
// revision they were read at.
func saveSubnet(t *testing.T, e *netEnv, subnet string) error {
	t.Helper()
	view, err := e.eng.SettingsView()
	require.NoError(t, err)
	s := view.Settings
	s.Managed.Subnet = netip.MustParsePrefix(subnet)
	_, _, err = e.eng.SaveSettings(t.Context(), view.Rev, s)
	return err
}

func TestTheManagedSubnetIsRefusedWhereItOverlaps(t *testing.T) {
	for _, tt := range []struct {
		name   string
		subnet string
		setup  func(t *testing.T, e *netEnv)
		msg    string
	}{
		{"a lease", "10.204.0.0/24", func(t *testing.T, e *netEnv) {
			require.NoError(t, e.store.SaveLease(testLease(), 0))
		}, "managed.subnet cannot be changed while 1 lease exists on the managed network"},
		{"the managed network", "10.204.0.0/24", func(t *testing.T, e *netEnv) {
			require.NoError(t, e.store.SaveNetwork(store.Network{
				ID: "pconet", Kind: store.NetworkManaged, Bridge: "pconet", Prefix: netip.MustParsePrefix("10.203.118.0/24"),
				Legs: map[string]netip.Addr{testNode: netip.MustParseAddr("10.203.118.1")}, LegMAC: store.LegMACFor(store.NetworkManaged, 1), Mark: 1, CreatedAt: t0,
			}, 0))
		}, "managed.subnet cannot be changed while the managed network exists: its leg and its leases are in 10.203.118.0/24"},
		{"a route of a node", "10.50.0.0/25", func(t *testing.T, e *netEnv) {
			snap := netSnapshot()
			snap.Nodes[0].Ifaces = []pve.NodeIface{{Name: "vmbr9", Type: "bridge", Addrs: []netip.Prefix{netip.MustParsePrefix("10.50.0.1/24")}}}
			e.inv.set(snap)
		}, "managed.subnet 10.50.0.0/25 overlaps the route 10.50.0.0/24 of node pve1 on vmbr9"},
		{"an address of a guest", "10.77.0.0/24", nil, "managed.subnet 10.77.0.0/24 holds 10.77.0.11, an address of qemu/101 (web-1)"},
		{"a reported address", "10.77.0.0/28", func(t *testing.T, e *netEnv) {
			snap := netSnapshot()
			snap.Guests = slices.DeleteFunc(snap.Guests, func(g model.Guest) bool { return g.Ref.VMID == 101 })
			e.inv.set(snap)
		}, "managed.subnet 10.77.0.0/28 holds 10.77.0.12, an address of lxc/102 (db-1)"},
		{"the prefix of an attached network", "10.60.0.0/24", func(t *testing.T, e *netEnv) {
			require.NoError(t, e.store.SaveNetwork(store.Network{
				ID: "vmbr1", Kind: store.NetworkAttach, Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.60.0.0/23"),
				Legs: map[string]netip.Addr{testNode: netip.MustParseAddr("10.60.1.250")}, LegMAC: store.LegMACFor(store.NetworkAttach, 1), Mark: 1, CreatedAt: t0,
			}, 0))
		}, "managed.subnet 10.60.0.0/24 overlaps 10.60.0.0/23, the prefix of network vmbr1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newNetEnv(t)
			if tt.setup != nil {
				tt.setup(t, e)
			}

			err := saveSubnet(t, e, tt.subnet)

			require.ErrorIs(t, err, ErrInvalid)
			require.EqualError(t, err, tt.msg)
			var fe *FieldError
			require.ErrorAs(t, err, &fe)
			require.Equal(t, "managed.subnet", fe.Field)
			s, err := e.store.Settings()
			require.NoError(t, err)
			require.Equal(t, netip.MustParsePrefix("10.203.118.0/24"), s.Managed.Subnet, "nothing is saved")
		})
	}
}

func TestTheManagedSubnetIsSavedWhereNothingOverlaps(t *testing.T) {
	e := newNetEnv(t)
	saveNetwork(t, e.store, "pcot1", 1)

	require.NoError(t, saveSubnet(t, e, "10.204.0.0/24"))

	s, err := e.store.Settings()
	require.NoError(t, err)
	require.Equal(t, netip.MustParsePrefix("10.204.0.0/24"), s.Managed.Subnet)
}

func TestTheManagedSubnetIsCheckedOnlyWhenItChanges(t *testing.T) {
	e := newNetEnv(t)
	require.NoError(t, e.store.SaveLease(testLease(), 0))
	view, err := e.eng.SettingsView()
	require.NoError(t, err)
	s := view.Settings
	s.GateTag = "publish"

	_, _, err = e.eng.SaveSettings(t.Context(), view.Rev, s)

	require.NoError(t, err)
	require.Zero(t, e.inv.refreshes(), "a subnet that stays is not checked")
}

func TestTheManagedSubnetIsNotCheckedAgainstAnIncompleteInventory(t *testing.T) {
	e := newNetEnv(t)
	e.inv.set(incomplete("node pve1: the network could not be read"))

	err := saveSubnet(t, e, "10.204.0.0/24")

	require.ErrorIs(t, err, ErrRefused)
	require.EqualError(t, err, "refused: the inventory is incomplete (node pve1: the network could not be read), "+
		"so managed.subnet cannot be checked against the routes of the nodes and the addresses of the guests; try again once it is complete")
}
