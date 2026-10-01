package resolve

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

const (
	localNode  = "pve1"
	foreignMAC = "bc:24:11:ff:ff:01"
	webOwner   = "qemu/101"
	webHost    = "web.example.com"
)

var (
	t0       = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	db1Ref   = model.GuestRef{Kind: model.KindQEMU, VMID: 102}
	ct200Ref = model.GuestRef{Kind: model.KindLXC, VMID: 200}
	errDial  = errors.New("connection refused")
)

// probeCall is one call the resolver made on the prober.
type probeCall struct {
	op    string // interfaces, arp, fdb, dial
	iface string // arp: interface; fdb: bridge
	vlan  int
	mac   string
	addr  netip.Addr
	port  uint16
}

// fakeProber answers from a script and records every call.
type fakeProber struct {
	ifaces    []HostIface
	ifacesErr error
	arp       map[string][]string // arpKey -> answering MACs
	arpErr    map[string]error    // arpKey
	fdb       map[string]string   // fdbKey -> port
	fdbErr    error
	dialErr   map[netip.Addr]error
	calls     []probeCall
}

func newFakeProber() *fakeProber {
	return &fakeProber{
		arp:     map[string][]string{},
		arpErr:  map[string]error{},
		fdb:     map[string]string{},
		dialErr: map[netip.Addr]error{},
	}
}

func arpKey(iface, addr string) string { return iface + " " + addr }

func fdbKey(bridge string, vlan int, mac string) string {
	return fmt.Sprintf("%s/%d/%s", bridge, vlan, mac)
}

func (f *fakeProber) Interfaces(context.Context) ([]HostIface, error) {
	f.calls = append(f.calls, probeCall{op: "interfaces"})
	return f.ifaces, f.ifacesErr
}

func (f *fakeProber) ARP(_ context.Context, iface string, addr netip.Addr) ([]string, error) {
	f.calls = append(f.calls, probeCall{op: "arp", iface: iface, addr: addr})
	key := arpKey(iface, addr.String())
	return f.arp[key], f.arpErr[key]
}

func (f *fakeProber) FDBPort(_ context.Context, bridge string, vlan int, mac string) (string, bool, error) {
	f.calls = append(f.calls, probeCall{op: "fdb", iface: bridge, vlan: vlan, mac: mac})
	if f.fdbErr != nil {
		return "", false, f.fdbErr
	}
	port, ok := f.fdb[fdbKey(bridge, vlan, mac)]
	return port, ok, nil
}

func (f *fakeProber) Dial(_ context.Context, target netip.AddrPort) error {
	f.calls = append(f.calls, probeCall{op: "dial", addr: target.Addr(), port: target.Port()})
	return f.dialErr[target.Addr()]
}

// touched reports whether addr was ever ARPed or dialled.
func (f *fakeProber) touched(addr netip.Addr) bool {
	for _, c := range f.calls {
		if (c.op == "arp" || c.op == "dial") && c.addr == addr {
			return true
		}
	}
	return false
}

func (f *fakeProber) ops(op string) []probeCall {
	var out []probeCall
	for _, c := range f.calls {
		if c.op == op {
			out = append(out, c)
		}
	}
	return out
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

func nicOn(index int, mac, bridge string, vlan int, static ...string) model.NIC {
	return model.NIC{Index: index, MAC: mac, Bridge: bridge, VLAN: vlan, Static: ips(static...)}
}

// scenario is web-1 (qemu/101) running on the local node with net0 on vmbr0
// and the static address 10.20.0.10, which answers ARP from its own MAC on its
// own port. The node has 10.20.0.2/24 on vmbr0.
type scenario struct {
	prober   *fakeProber
	clock    *clock
	settings Settings
	guests   []model.Guest
	deny     Denylist
}

func newScenario() *scenario {
	p := newFakeProber()
	p.ifaces = []HostIface{{Name: "vmbr0", Addrs: prefixes("10.20.0.2/24")}}
	p.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0}
	p.fdb[fdbKey("vmbr0", 0, mac0)] = "tap101i0"
	web := model.Guest{
		Ref:     web1Ref,
		Name:    "web-1",
		Node:    localNode,
		Running: true,
		NICs:    []model.NIC{nicOn(0, mac0, "vmbr0", 0, "10.20.0.10")},
	}
	return &scenario{
		prober:   p,
		clock:    &clock{t: t0},
		settings: Settings{LocalNode: localNode},
		guests:   []model.Guest{web},
		deny:     NewDenylist(ips("10.20.0.2"), nil),
	}
}

func (s *scenario) web() *model.Guest { return &s.guests[0] }

func (s *scenario) addDB1(running bool, nics ...model.NIC) {
	s.guests = append(s.guests, model.Guest{Ref: db1Ref, Name: "db-1", Node: localNode, Running: running, NICs: nics})
}

func (s *scenario) resolve(t *testing.T, route model.Route, prev *Binding) Result {
	t.Helper()
	r := NewResolver(s.prober, s.settings, s.clock.now)
	snap := inventory.Snapshot{Guests: s.guests, Complete: true, TakenAt: s.clock.t}
	return r.Resolve(t.Context(), route, snap, prev, s.deny)
}

func webRoute() model.Route { return routeFor(netip.Addr{}, "") }

func routeTo(ref model.GuestRef) model.Route {
	route := webRoute()
	route.Guest = &ref
	return route
}

func boundTo(addr string) *Binding {
	return &Binding{Owner: webOwner, Hostname: webHost, Addr: ip(addr), MAC: mac0, VerifiedAt: t0.Add(-time.Hour)}
}

func timePtr(t time.Time) *time.Time { return &t }

func requireServed(t *testing.T, res Result, addr string, at time.Time) {
	t.Helper()
	require.Equal(t, planner.ResolvedTarget{Addr: ip(addr), Reachable: true}, res.Target)
	require.Equal(t, &Binding{Owner: webOwner, Hostname: webHost, Addr: ip(addr), MAC: mac0, VerifiedAt: at}, res.Binding)
}

// requireNotServed asserts the outcome for a candidate that never was bound:
// no address, no binding.
func requireNotServed(t *testing.T, res Result, reason string) {
	t.Helper()
	require.Equal(t, planner.ResolvedTarget{Reason: reason}, res.Target)
	require.Nil(t, res.Binding)
}

func TestResolveUntaggedBridge(t *testing.T) {
	s := newScenario()

	res := s.resolve(t, webRoute(), nil)

	requireServed(t, res, "10.20.0.10", t0)
	require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true}}, res.Candidates)
	require.Equal(t, []probeCall{
		{op: "interfaces"},
		{op: "arp", iface: "vmbr0", addr: ip("10.20.0.10")},
		{op: "fdb", iface: "vmbr0", mac: mac0},
		{op: "dial", addr: ip("10.20.0.10"), port: 80},
	}, s.prober.calls)
}

func TestResolveExplicitAddress(t *testing.T) {
	s := newScenario()

	res := s.resolve(t, routeFor(ip("10.20.0.10"), ""), nil)

	requireServed(t, res, "10.20.0.10", t0)
	require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.10"), Source: FromVia, OK: true}}, res.Candidates)
	require.Len(t, s.prober.ops("arp"), 1)
	require.Len(t, s.prober.ops("fdb"), 1)
}

func TestResolveVLANInterface(t *testing.T) {
	tests := []struct {
		name      string
		iface     string
		fdbBridge string
		fdbVLAN   int
	}{
		{"vlan interface on a vlan-aware bridge", "vmbr0.10", "vmbr0", 10},
		{"bridge of the vlan", "vmbr0v10", "vmbr0v10", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario()
			s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 10, "10.30.0.10")}
			s.prober.ifaces = []HostIface{
				{Name: "vmbr0", Addrs: prefixes("10.20.0.2/24")},
				{Name: tt.iface, Addrs: prefixes("10.30.0.2/24")},
			}
			s.prober.arp[arpKey(tt.iface, "10.30.0.10")] = []string{mac0}
			s.prober.fdb[fdbKey(tt.fdbBridge, tt.fdbVLAN, mac0)] = "tap101i0"

			res := s.resolve(t, webRoute(), nil)

			requireServed(t, res, "10.30.0.10", t0)
			require.Equal(t, []probeCall{{op: "arp", iface: tt.iface, addr: ip("10.30.0.10")}}, s.prober.ops("arp"))
			require.Equal(t, []probeCall{{op: "fdb", iface: tt.fdbBridge, vlan: tt.fdbVLAN, mac: mac0}}, s.prober.ops("fdb"))
		})
	}
}

func TestResolveNotAdjacent(t *testing.T) {
	tests := []struct {
		name   string
		vlan   int
		ifaces []HostIface
		reason string
	}{
		{
			name:   "no interface for the bridge",
			ifaces: []HostIface{{Name: "vmbr1", Addrs: prefixes("10.20.0.2/24")}},
			reason: "node has no address on vmbr0 in the guest's network",
		},
		{
			name:   "bridge in another network",
			ifaces: []HostIface{{Name: "vmbr0", Addrs: prefixes("10.99.0.2/24")}},
			reason: "node has no address on vmbr0 in the guest's network",
		},
		{
			name:   "bridge without addresses",
			ifaces: []HostIface{{Name: "vmbr0"}},
			reason: "node has no address on vmbr0 in the guest's network",
		},
		{
			name:   "untagged nic and only a vlan interface in the network",
			ifaces: []HostIface{{Name: "vmbr0.10", Addrs: prefixes("10.20.0.2/24")}},
			reason: "node has no address on vmbr0 in the guest's network",
		},
		{
			name:   "tagged nic and only the untagged bridge in the network",
			vlan:   10,
			ifaces: []HostIface{{Name: "vmbr0", Addrs: prefixes("10.20.0.2/24")}},
			reason: "node has no address on vmbr0.10 or vmbr0v10 in the guest's network",
		},
		{
			name: "tagged nic and only another vlan in the network",
			vlan: 10,
			ifaces: []HostIface{
				{Name: "vmbr0.20", Addrs: prefixes("10.20.0.2/24")},
				{Name: "vmbr0v20", Addrs: prefixes("10.20.0.3/24")},
			},
			reason: "node has no address on vmbr0.10 or vmbr0v10 in the guest's network",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario()
			s.web().NICs[0].VLAN = tt.vlan
			s.prober.ifaces = tt.ifaces

			res := s.resolve(t, webRoute(), nil)

			requireNotServed(t, res, tt.reason)
			require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.10"), Source: FromStatic, Reason: tt.reason}}, res.Candidates)
			require.False(t, s.prober.touched(ip("10.20.0.10")))
		})
	}
}

func TestResolveTrustedStatic(t *testing.T) {
	const notAdjacent = "node has no address on vmbr0 in the guest's network"
	tests := []struct {
		name   string
		trust  bool
		nic    model.NIC
		rep    []model.ReportedAddr
		route  model.Route
		addr   string
		source CandidateSource
		reason string // empty when accepted
	}{
		{
			name:   "static inside the trusted range",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.40.0.10"),
			route:  webRoute(),
			addr:   "10.40.0.10",
			source: FromStatic,
		},
		{
			name:   "static outside the trusted range",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.50.0.10"),
			route:  webRoute(),
			addr:   "10.50.0.10",
			source: FromStatic,
			reason: notAdjacent,
		},
		{
			name:   "trust switched off",
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.40.0.10"),
			route:  webRoute(),
			addr:   "10.40.0.10",
			source: FromStatic,
			reason: notAdjacent,
		},
		{
			name:   "reported address inside the trusted range",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0),
			rep:    []model.ReportedAddr{rep(mac0, "10.40.0.11")},
			route:  webRoute(),
			addr:   "10.40.0.11",
			source: FromAgent,
			reason: notAdjacent,
		},
		{
			name:   "address named by the route inside the trusted range",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.40.0.10"),
			route:  routeFor(ip("10.40.0.10"), ""),
			addr:   "10.40.0.10",
			source: FromVia,
			reason: notAdjacent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario()
			s.settings.TrustStatic = tt.trust
			s.settings.TrustedCIDRs = prefixes("10.40.0.0/24")
			s.web().NICs = []model.NIC{tt.nic}
			s.web().Reported = tt.rep

			res := s.resolve(t, tt.route, nil)

			require.Empty(t, s.prober.ops("arp"))
			require.Empty(t, s.prober.ops("fdb"))
			if tt.reason != "" {
				requireNotServed(t, res, tt.reason)
				require.Equal(t, []CandidateResult{{Addr: ip(tt.addr), Source: tt.source, Reason: tt.reason}}, res.Candidates)
				require.Empty(t, s.prober.ops("dial"))
				return
			}
			requireServed(t, res, tt.addr, t0)
			require.Equal(t, []probeCall{{op: "dial", addr: ip(tt.addr), port: 80}}, s.prober.ops("dial"))
		})
	}
}

func TestResolveRejectsSecondMACAnswering(t *testing.T) {
	s := newScenario()
	s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0, foreignMAC}

	res := s.resolve(t, webRoute(), nil)

	requireNotServed(t, res, "10.20.0.10 answered by bc:24:11:ff:ff:01, which is not this guest")
	require.Empty(t, s.prober.ops("fdb"))
	require.Empty(t, s.prober.ops("dial"))
}

func TestResolveARPAnswers(t *testing.T) {
	tests := []struct {
		name    string
		nics    []model.NIC
		answers []string
		fdb     map[string]string
		reason  string // empty when accepted
	}{
		{
			name:    "foreign MAC",
			answers: []string{foreignMAC},
			reason:  "10.20.0.10 answered by bc:24:11:ff:ff:01, which is not this guest",
		},
		{
			name:    "no answer",
			answers: nil,
			reason:  "no ARP answer on vmbr0",
		},
		{
			name:    "own MAC in another spelling",
			answers: []string{"BC-24-11-00-00-01"},
		},
		{
			name:    "unparsable answer",
			answers: []string{"garbage"},
			reason:  "10.20.0.10 answered by garbage, which is not this guest",
		},
		{
			name: "two NICs of the guest on the bridge",
			nics: []model.NIC{
				nicOn(0, mac0, "vmbr0", 0, "10.20.0.10"),
				nicOn(1, mac1, "vmbr0", 0),
			},
			answers: []string{mac1, mac0},
			fdb: map[string]string{
				fdbKey("vmbr0", 0, mac0): "tap101i0",
				fdbKey("vmbr0", 0, mac1): "tap101i1",
			},
		},
		{
			name: "second NIC of the guest on the port of the first",
			nics: []model.NIC{
				nicOn(0, mac0, "vmbr0", 0, "10.20.0.10"),
				nicOn(1, mac1, "vmbr0", 0),
			},
			answers: []string{mac0, mac1},
			fdb: map[string]string{
				fdbKey("vmbr0", 0, mac0): "tap101i0",
				fdbKey("vmbr0", 0, mac1): "tap101i0",
			},
			reason: "MAC bc:24:11:00:00:02 is on port tap101i0, not on the guest's own port",
		},
		{
			name: "NIC of the guest on another bridge",
			nics: []model.NIC{
				nicOn(0, mac0, "vmbr0", 0, "10.20.0.10"),
				nicOn(1, mac1, "vmbr1", 0),
			},
			answers: []string{mac1},
			reason:  "10.20.0.10 answered by bc:24:11:00:00:02, which is not this guest",
		},
		{
			name: "NIC of the guest in another vlan of the bridge",
			nics: []model.NIC{
				nicOn(0, mac0, "vmbr0", 0, "10.20.0.10"),
				nicOn(1, mac1, "vmbr0", 10),
			},
			answers: []string{mac0, mac1},
			reason:  "10.20.0.10 answered by bc:24:11:00:00:02, which is not this guest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario()
			if tt.nics != nil {
				s.web().NICs = tt.nics
			}
			s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = tt.answers
			for k, v := range tt.fdb {
				s.prober.fdb[k] = v
			}

			res := s.resolve(t, webRoute(), nil)

			if tt.reason != "" {
				requireNotServed(t, res, tt.reason)
				require.Empty(t, s.prober.ops("dial"))
				return
			}
			requireServed(t, res, "10.20.0.10", t0)
		})
	}
}

func TestResolveForwardingTable(t *testing.T) {
	tests := []struct {
		name   string
		ref    model.GuestRef
		port   string // empty: MAC not learned
		reason string // empty when accepted
	}{
		{name: "tap of the guest", ref: web1Ref, port: "tap101i0"},
		{name: "firewall bridge of the guest", ref: web1Ref, port: "fwpr101p0"},
		{name: "veth of the container", ref: ct200Ref, port: "veth200i0"},
		{
			name: "tap of another guest", ref: web1Ref, port: "tap102i0",
			reason: "MAC bc:24:11:00:00:01 is on port tap102i0, not on the guest's own port",
		},
		{
			name: "tap of another NIC of the guest", ref: web1Ref, port: "tap101i1",
			reason: "MAC bc:24:11:00:00:01 is on port tap101i1, not on the guest's own port",
		},
		{
			name: "tap of a guest whose vmid starts the same", ref: web1Ref, port: "tap1010i0",
			reason: "MAC bc:24:11:00:00:01 is on port tap1010i0, not on the guest's own port",
		},
		{
			name: "uplink", ref: web1Ref, port: "eno1",
			reason: "MAC bc:24:11:00:00:01 is on port eno1, not on the guest's own port",
		},
		{
			name: "not learned", ref: web1Ref,
			reason: "MAC bc:24:11:00:00:01 not seen on bridge vmbr0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario()
			s.web().Ref = tt.ref
			delete(s.prober.fdb, fdbKey("vmbr0", 0, mac0))
			if tt.port != "" {
				s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = tt.port
			}

			res := s.resolve(t, routeTo(tt.ref), nil)

			if tt.reason != "" {
				require.Equal(t, planner.ResolvedTarget{Reason: tt.reason}, res.Target)
				require.Nil(t, res.Binding)
				require.Empty(t, s.prober.ops("dial"))
				return
			}
			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true}, res.Target)
			require.NotNil(t, res.Binding)
			require.Equal(t, tt.ref.String(), res.Binding.Owner)
		})
	}
}

func TestResolveSkipsForwardingTableOnAnotherNode(t *testing.T) {
	s := newScenario()
	s.web().Node = "pve2"
	delete(s.prober.fdb, fdbKey("vmbr0", 0, mac0))

	res := s.resolve(t, webRoute(), nil)

	requireServed(t, res, "10.20.0.10", t0)
	require.Len(t, s.prober.ops("arp"), 1)
	require.Empty(t, s.prober.ops("fdb"))
}

func TestResolveDuplicateMAC(t *testing.T) {
	const dupMAC0 = "MAC bc:24:11:00:00:01 is also configured on qemu/102"

	t.Run("on a running guest", func(t *testing.T) {
		s := newScenario()
		s.addDB1(true, nicOn(0, mac0, "vmbr1", 0))

		res := s.resolve(t, webRoute(), nil)

		requireNotServed(t, res, dupMAC0)
		require.False(t, s.prober.touched(ip("10.20.0.10")))
	})

	t.Run("on a stopped guest", func(t *testing.T) {
		s := newScenario()
		s.addDB1(false, nicOn(0, mac0, "vmbr0", 0))

		res := s.resolve(t, webRoute(), nil)

		requireServed(t, res, "10.20.0.10", t0)
	})

	t.Run("in another spelling", func(t *testing.T) {
		s := newScenario()
		s.addDB1(true, nicOn(0, "BC:24:11:00:00:01", "vmbr1", 0))

		res := s.resolve(t, webRoute(), nil)

		requireNotServed(t, res, dupMAC0)
	})

	t.Run("earlier binding wins", func(t *testing.T) {
		s := newScenario()
		s.addDB1(true, nicOn(0, mac0, "vmbr1", 0))

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireServed(t, res, "10.20.0.10", t0)
	})

	t.Run("binding of the other guest does not help", func(t *testing.T) {
		s := newScenario()
		s.addDB1(true, nicOn(0, mac0, "vmbr1", 0))
		prev := boundTo("10.20.0.10")
		prev.Owner = db1Ref.String()

		res := s.resolve(t, webRoute(), prev)

		requireNotServed(t, res, dupMAC0)
	})

	t.Run("on another answering NIC of the guest", func(t *testing.T) {
		s := newScenario()
		s.web().NICs = append(s.web().NICs, nicOn(1, mac1, "vmbr0", 0))
		s.addDB1(true, nicOn(0, mac1, "vmbr1", 0))
		s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0, mac1}
		s.prober.fdb[fdbKey("vmbr0", 0, mac1)] = "tap101i1"

		res := s.resolve(t, webRoute(), nil)

		requireNotServed(t, res, "MAC bc:24:11:00:00:02 is also configured on qemu/102")
		require.Empty(t, s.prober.ops("dial"))
	})
}

// stickyScenario has two healthy candidates on net0; the binding is on the
// second one, which stops answering on port 80.
func stickyScenario() (*scenario, *Binding) {
	s := newScenario()
	s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 0, "10.20.0.10", "10.20.0.11")}
	s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}
	s.prober.dialErr[ip("10.20.0.11")] = errDial
	return s, boundTo("10.20.0.11")
}

func TestResolveStickyBindingDoesNotFlap(t *testing.T) {
	s, prev := stickyScenario()
	const failed = "port 80: connection refused"

	for _, after := range []time.Duration{0, 60 * time.Second} {
		s.clock.t = t0.Add(after)
		s.prober.calls = nil

		res := s.resolve(t, webRoute(), prev)

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.11"), Reason: failed}, res.Target, "after %s", after)
		want := *boundTo("10.20.0.11")
		want.FailingSince = timePtr(t0)
		require.Equal(t, &want, res.Binding, "after %s", after)
		require.Equal(t, []CandidateResult{
			{Addr: ip("10.20.0.11"), Source: FromStatic, Reason: failed},
			{Addr: ip("10.20.0.10"), Source: FromStatic, Reason: "not tried"},
		}, res.Candidates)
		require.False(t, s.prober.touched(ip("10.20.0.10")), "after %s", after)
		prev = res.Binding
	}

	s.clock.t = t0.Add(121 * time.Second)
	res := s.resolve(t, webRoute(), prev)

	requireServed(t, res, "10.20.0.10", t0.Add(121*time.Second))
	require.Equal(t, []CandidateResult{
		{Addr: ip("10.20.0.11"), Source: FromStatic, Reason: failed},
		{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true},
	}, res.Candidates)
}

func TestResolveStickyBindingKeptWithoutAlternative(t *testing.T) {
	s, prev := stickyScenario()
	s.prober.dialErr[ip("10.20.0.10")] = errDial
	prev.FailingSince = timePtr(t0)
	s.clock.t = t0.Add(10 * time.Minute)

	res := s.resolve(t, webRoute(), prev)

	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.11"), Reason: "port 80: connection refused"}, res.Target)
	require.Equal(t, prev, res.Binding)
	require.Equal(t, []CandidateResult{
		{Addr: ip("10.20.0.11"), Source: FromStatic, Reason: "port 80: connection refused"},
		{Addr: ip("10.20.0.10"), Source: FromStatic, Reason: "port 80: connection refused"},
	}, res.Candidates)
}

func TestResolveStickyFor(t *testing.T) {
	s, prev := stickyScenario()
	s.settings.StickyFor = 30 * time.Second
	prev.FailingSince = timePtr(t0)
	s.clock.t = t0.Add(30 * time.Second)

	res := s.resolve(t, webRoute(), prev)

	requireServed(t, res, "10.20.0.10", s.clock.t)
}

func TestResolveBindingRecovers(t *testing.T) {
	s := newScenario()
	prev := boundTo("10.20.0.10")
	prev.FailingSince = timePtr(t0.Add(-time.Minute))

	res := s.resolve(t, webRoute(), prev)

	requireServed(t, res, "10.20.0.10", t0)
	require.Equal(t, timePtr(t0.Add(-time.Minute)), prev.FailingSince, "prev must not change")
}

func TestResolveIdentityLossWithdrawsBinding(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(s *scenario)
		reason string
	}{
		{
			name:   "foreign MAC answers",
			setup:  func(s *scenario) { s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{foreignMAC} },
			reason: "10.20.0.10 answered by bc:24:11:ff:ff:01, which is not this guest",
		},
		{
			name:   "second MAC answers",
			setup:  func(s *scenario) { s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0, foreignMAC} },
			reason: "10.20.0.10 answered by bc:24:11:ff:ff:01, which is not this guest",
		},
		{
			name:   "no ARP answer",
			setup:  func(s *scenario) { delete(s.prober.arp, arpKey("vmbr0", "10.20.0.10")) },
			reason: "no ARP answer on vmbr0",
		},
		{
			name:   "MAC on another port",
			setup:  func(s *scenario) { s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = "tap102i0" },
			reason: "MAC bc:24:11:00:00:01 is on port tap102i0, not on the guest's own port",
		},
		{
			name:   "MAC not learned",
			setup:  func(s *scenario) { delete(s.prober.fdb, fdbKey("vmbr0", 0, mac0)) },
			reason: "MAC bc:24:11:00:00:01 not seen on bridge vmbr0",
		},
		{
			name:   "no longer adjacent",
			setup:  func(s *scenario) { s.prober.ifaces = nil },
			reason: "node has no address on vmbr0 in the guest's network",
		},
		{
			name: "NIC MAC changed to a duplicate",
			setup: func(s *scenario) {
				s.web().NICs[0].MAC = mac2
				s.addDB1(true, nicOn(0, mac2, "vmbr1", 0))
			},
			reason: "MAC bc:24:11:00:00:03 is also configured on qemu/102",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario()
			s.web().NICs[0].Static = ips("10.20.0.10", "10.20.0.11")
			s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}
			tt.setup(s)
			prev := boundTo("10.20.0.10")

			res := s.resolve(t, webRoute(), prev)

			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: tt.reason}, res.Target)
			want := *boundTo("10.20.0.10")
			want.FailingSince = timePtr(t0)
			require.Equal(t, &want, res.Binding)
			require.Nil(t, prev.FailingSince, "prev must not change")
			require.Empty(t, s.prober.ops("dial"))
			require.False(t, s.prober.touched(ip("10.20.0.11")), "no other candidate within StickyFor")
		})
	}
}

func TestResolveIdentityLossKeepsFailingSince(t *testing.T) {
	s := newScenario()
	s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{foreignMAC}
	prev := boundTo("10.20.0.10")
	prev.FailingSince = timePtr(t0.Add(-30 * time.Second))

	res := s.resolve(t, webRoute(), prev)

	require.True(t, res.Target.Withdrawn)
	require.Equal(t, prev, res.Binding)
}

func TestResolveGuestNotRunning(t *testing.T) {
	tests := []struct {
		name   string
		prev   *Binding
		target planner.ResolvedTarget
	}{
		{
			name:   "with a binding",
			prev:   boundTo("10.20.0.10"),
			target: planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: "guest is not running"},
		},
		{
			name:   "without a binding",
			target: planner.ResolvedTarget{Reason: "guest is not running"},
		},
		{
			name:   "with a binding to a denied address",
			prev:   boundTo("10.20.0.2"),
			target: planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario()
			s.web().Running = false

			res := s.resolve(t, webRoute(), tt.prev)

			require.Equal(t, tt.target, res.Target)
			if tt.target.Addr.IsValid() {
				require.Equal(t, tt.prev, res.Binding)
			} else {
				require.Nil(t, res.Binding)
			}
			require.Empty(t, s.prober.calls)
		})
	}
}

func TestResolveGuestNotFound(t *testing.T) {
	const reason = "guest not found in inventory"
	for _, withBinding := range []bool{true, false} {
		t.Run(fmt.Sprintf("binding %v", withBinding), func(t *testing.T) {
			s := newScenario()
			s.guests = nil
			var prev *Binding
			want := planner.ResolvedTarget{Reason: reason}
			if withBinding {
				prev = boundTo("10.20.0.10")
				want.Addr = ip("10.20.0.10")
			}

			res := s.resolve(t, webRoute(), prev)

			require.Equal(t, want, res.Target)
			require.Equal(t, prev, res.Binding)
			require.Empty(t, s.prober.calls)
		})
	}
}

func TestResolveIgnoresBindingOfAnotherRoute(t *testing.T) {
	tests := []struct {
		name string
		edit func(b *Binding)
	}{
		{"another owner", func(b *Binding) { b.Owner = db1Ref.String() }},
		{"another hostname", func(b *Binding) { b.Hostname = "db.example.com" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario()
			s.web().Running = false
			prev := boundTo("10.20.0.10")
			tt.edit(prev)

			res := s.resolve(t, webRoute(), prev)

			requireNotServed(t, res, "guest is not running")
		})
	}
}

func TestResolveDeniedAddressNeverProbed(t *testing.T) {
	node := ip("10.20.0.2")

	t.Run("only candidate", func(t *testing.T) {
		s := newScenario()
		s.web().NICs[0].Static = ips("10.20.0.2")
		s.prober.arp[arpKey("vmbr0", "10.20.0.2")] = []string{mac0}

		res := s.resolve(t, webRoute(), nil)

		require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"}, res.Target)
		require.Nil(t, res.Binding)
		require.Equal(t, []CandidateResult{{Addr: node, Source: FromStatic, Reason: "address of a cluster node"}}, res.Candidates)
		require.Empty(t, s.prober.calls)
	})

	t.Run("named by the route", func(t *testing.T) {
		s := newScenario()

		res := s.resolve(t, routeFor(node, ""), nil)

		require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"}, res.Target)
		require.Empty(t, s.prober.calls)
	})

	t.Run("before a good candidate", func(t *testing.T) {
		s := newScenario()
		s.web().NICs[0].Static = ips("10.20.0.2")
		s.web().Reported = []model.ReportedAddr{rep(mac0, "169.254.10.10"), rep(mac0, "10.20.0.10"), rep(mac0, "10.20.0.11")}
		s.prober.arp[arpKey("vmbr0", "169.254.10.10")] = []string{mac0}

		res := s.resolve(t, webRoute(), nil)

		requireServed(t, res, "10.20.0.10", t0)
		require.Equal(t, []CandidateResult{
			{Addr: node, Source: FromStatic, Reason: "address of a cluster node"},
			{Addr: ip("169.254.10.10"), Source: FromAgent, Reason: "link-local address"},
			{Addr: ip("10.20.0.10"), Source: FromAgent, OK: true},
			{Addr: ip("10.20.0.11"), Source: FromAgent, Reason: "not tried"},
		}, res.Candidates)
		require.False(t, s.prober.touched(node))
		require.False(t, s.prober.touched(ip("169.254.10.10")))
		require.False(t, s.prober.touched(ip("10.20.0.11")))
	})

	t.Run("bound address became denied", func(t *testing.T) {
		s := newScenario()
		s.web().NICs[0].Static = ips("10.20.0.2", "10.20.0.10")

		res := s.resolve(t, webRoute(), boundTo("10.20.0.2"))

		requireServed(t, res, "10.20.0.10", t0)
		require.False(t, s.prober.touched(node))
	})

	t.Run("bound address became denied and nothing else verifies", func(t *testing.T) {
		s := newScenario()
		s.web().NICs[0].Static = ips("10.20.0.2")

		res := s.resolve(t, webRoute(), boundTo("10.20.0.2"))

		require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"}, res.Target)
		require.Nil(t, res.Binding)
		require.Empty(t, s.prober.calls)
	})
}

func TestResolveCandidatesErrorRejects(t *testing.T) {
	s := newScenario()

	res := s.resolve(t, routeFor(netip.Addr{}, "net9"), boundTo("10.20.0.10"))

	require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: "guest has no net9"}, res.Target)
	require.Nil(t, res.Binding)
	require.Empty(t, res.Candidates)
	require.Empty(t, s.prober.calls)
}

func TestResolveNoCandidates(t *testing.T) {
	s := newScenario()
	s.web().NICs[0].Static = nil

	res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

	requireNotServed(t, res, "no candidate address")
	require.Empty(t, s.prober.calls)
}

func TestResolveProberErrorIsNotWithdrawal(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(s *scenario)
		reason string
	}{
		{
			name:   "interfaces",
			setup:  func(s *scenario) { s.prober.ifacesErr = errors.New("netlink closed") },
			reason: "listing host interfaces: netlink closed",
		},
		{
			name:   "ARP",
			setup:  func(s *scenario) { s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = errors.New("socket closed") },
			reason: "ARP on vmbr0: socket closed",
		},
		{
			name:   "forwarding table",
			setup:  func(s *scenario) { s.prober.fdbErr = errors.New("dump interrupted") },
			reason: "forwarding table of vmbr0: dump interrupted",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name+" on the binding", func(t *testing.T) {
			s := newScenario()
			tt.setup(s)

			res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: tt.reason}, res.Target)
			want := *boundTo("10.20.0.10")
			want.FailingSince = timePtr(t0)
			require.Equal(t, &want, res.Binding)
			require.Empty(t, s.prober.ops("dial"))
		})
		t.Run(tt.name+" without a binding", func(t *testing.T) {
			s := newScenario()
			tt.setup(s)

			res := s.resolve(t, webRoute(), nil)

			requireNotServed(t, res, tt.reason)
		})
	}
}

func TestResolveDialFailureWithoutBinding(t *testing.T) {
	s := newScenario()
	s.prober.dialErr[ip("10.20.0.10")] = errDial

	res := s.resolve(t, webRoute(), nil)

	requireNotServed(t, res, "port 80: connection refused")
}

func TestResolveBoundAddressNoLongerCandidate(t *testing.T) {
	t.Run("another candidate verifies", func(t *testing.T) {
		s := newScenario()

		res := s.resolve(t, webRoute(), boundTo("10.20.0.99"))

		requireServed(t, res, "10.20.0.10", t0)
		require.False(t, s.prober.touched(ip("10.20.0.99")))
	})

	t.Run("nothing verifies", func(t *testing.T) {
		s := newScenario()
		s.prober.dialErr[ip("10.20.0.10")] = errDial

		res := s.resolve(t, webRoute(), boundTo("10.20.0.99"))

		requireNotServed(t, res, "port 80: connection refused")
		require.False(t, s.prober.touched(ip("10.20.0.99")))
	})
}

func TestResolveListsEveryCandidate(t *testing.T) {
	s := newScenario()
	s.web().NICs = []model.NIC{
		nicOn(0, mac0, "vmbr0", 0, "10.20.0.12", "10.20.0.2"),
		nicOn(1, mac1, "vmbr1", 0, "10.30.0.10"),
	}
	s.web().Reported = []model.ReportedAddr{rep(mac0, "10.20.0.13")}
	s.prober.arp[arpKey("vmbr0", "10.20.0.13")] = []string{mac0}
	s.prober.dialErr[ip("10.20.0.13")] = errDial

	res := s.resolve(t, webRoute(), nil)

	requireNotServed(t, res, "no ARP answer on vmbr0")
	require.Equal(t, []CandidateResult{
		{Addr: ip("10.20.0.12"), Source: FromStatic, Reason: "no ARP answer on vmbr0"},
		{Addr: ip("10.20.0.2"), Source: FromStatic, Reason: "address of a cluster node"},
		{Addr: ip("10.30.0.10"), Source: FromStatic, Reason: "node has no address on vmbr1 in the guest's network"},
		{Addr: ip("10.20.0.13"), Source: FromAgent, Reason: "port 80: connection refused"},
	}, res.Candidates)
	require.Len(t, s.prober.ops("interfaces"), 1, "interfaces are listed once per call")
}

func TestResolveRouteWithoutGuest(t *testing.T) {
	route := model.Route{
		Hostname: "app.example.com",
		Target:   model.Target{Scheme: model.SchemeHTTP, Addr: ip("10.20.0.50"), Port: 8080},
		Source:   model.SourceManual,
		ManualID: "app",
	}

	t.Run("answering", func(t *testing.T) {
		s := newScenario()

		res := s.resolve(t, route, &Binding{Owner: "manual/app", Hostname: "app.example.com", Addr: ip("10.20.0.51")})

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.50"), Reachable: true}, res.Target)
		require.Nil(t, res.Binding)
		require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.50"), Source: FromVia, OK: true}}, res.Candidates)
		require.Equal(t, []probeCall{{op: "dial", addr: ip("10.20.0.50"), port: 8080}}, s.prober.calls)
	})

	t.Run("not answering", func(t *testing.T) {
		s := newScenario()
		s.prober.dialErr[ip("10.20.0.50")] = errDial

		res := s.resolve(t, route, nil)

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.50"), Reason: "port 8080: connection refused"}, res.Target)
		require.Nil(t, res.Binding)
	})

	t.Run("denied", func(t *testing.T) {
		s := newScenario()
		denied := route
		denied.Target.Addr = ip("10.20.0.2")

		res := s.resolve(t, denied, nil)

		require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"}, res.Target)
		require.Nil(t, res.Binding)
		require.Empty(t, s.prober.calls)
	})

	t.Run("no address", func(t *testing.T) {
		s := newScenario()
		empty := route
		empty.Target.Addr = netip.Addr{}

		res := s.resolve(t, empty, nil)

		require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: "not an IPv4 address"}, res.Target)
		require.Empty(t, s.prober.calls)
	})
}
