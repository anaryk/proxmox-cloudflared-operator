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

	// cancelOn names the op whose call cancels the caller's context with
	// cancel and fails the way a cancelled call fails, or answers as usual
	// when cancelSilently is set. When cancelAddr is set, only a call for
	// that address cancels.
	cancelOn       string
	cancelAddr     netip.Addr
	cancelSilently bool
	cancel         context.CancelFunc
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

func (f *fakeProber) record(c probeCall) error {
	f.calls = append(f.calls, c)
	if f.cancel == nil || f.cancelOn != c.op || (f.cancelAddr.IsValid() && f.cancelAddr != c.addr) {
		return nil
	}
	f.cancel()
	if f.cancelSilently {
		return nil
	}
	return context.Canceled
}

func (f *fakeProber) Interfaces(context.Context) ([]HostIface, error) {
	if err := f.record(probeCall{op: "interfaces"}); err != nil {
		return nil, err
	}
	return f.ifaces, f.ifacesErr
}

func (f *fakeProber) ARP(_ context.Context, iface string, addr netip.Addr) ([]string, error) {
	if err := f.record(probeCall{op: "arp", iface: iface, addr: addr}); err != nil {
		return nil, err
	}
	key := arpKey(iface, addr.String())
	return f.arp[key], f.arpErr[key]
}

func (f *fakeProber) FDBPort(_ context.Context, bridge string, vlan int, mac string) (string, bool, error) {
	if err := f.record(probeCall{op: "fdb", iface: bridge, vlan: vlan, mac: mac}); err != nil {
		return "", false, err
	}
	if f.fdbErr != nil {
		return "", false, f.fdbErr
	}
	port, ok := f.fdb[fdbKey(bridge, vlan, mac)]
	return port, ok, nil
}

func (f *fakeProber) Dial(_ context.Context, target netip.AddrPort) error {
	if err := f.record(probeCall{op: "dial", addr: target.Addr(), port: target.Port()}); err != nil {
		return err
	}
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
	return s.resolveCtx(t.Context(), route, prev)
}

func (s *scenario) resolveCtx(ctx context.Context, route model.Route, prev *Binding) Result {
	r := NewResolver(s.prober, s.settings, s.clock.now)
	snap := inventory.Snapshot{Guests: s.guests, Complete: true, TakenAt: s.clock.t}
	return r.Resolve(ctx, route, snap, prev, s.deny)
}

func webRoute() model.Route { return routeFor(netip.Addr{}, "") }

func routeTo(ref model.GuestRef) model.Route {
	route := webRoute()
	route.Guest = &ref
	return route
}

func boundTo(addr string) *Binding {
	return &Binding{Owner: webOwner, Hostname: webHost, Guest: webOwner, Addr: ip(addr), MAC: mac0, VerifiedAt: t0.Add(-time.Minute)}
}

// provenAt returns a copy of b whose identity was last proven at.
func provenAt(b *Binding, at time.Time) *Binding {
	c := *b
	c.VerifiedAt = at
	return &c
}

// failingAt returns a copy of b failing since at.
func failingAt(b *Binding, at time.Time) *Binding {
	c := *b
	c.FailingSince = &at
	return &c
}

// withdrawnAt returns a copy of b withdrawn and failing since at.
func withdrawnAt(b *Binding, at time.Time) *Binding {
	c := failingAt(b, at)
	c.Withdrawn = true
	return c
}

func timePtr(t time.Time) *time.Time { return &t }

func requireServed(t *testing.T, res Result, addr string, at time.Time) {
	t.Helper()
	require.Equal(t, planner.ResolvedTarget{Addr: ip(addr), Reachable: true, Owner: webOwner}, res.Target)
	require.Equal(t, &Binding{Owner: webOwner, Hostname: webHost, Guest: webOwner, Addr: ip(addr), MAC: mac0, VerifiedAt: at}, res.Binding)
}

// requireNotServed asserts the outcome for a candidate that never was bound:
// no address, no binding.
func requireNotServed(t *testing.T, res Result, reason string) {
	t.Helper()
	require.Equal(t, planner.ResolvedTarget{Reason: reason}, res.Target)
	require.Nil(t, res.Binding)
}

// requireWithdrawn asserts that the bound address is withdrawn and the binding
// kept as want.
func requireWithdrawn(t *testing.T, res Result, reason string, want *Binding) {
	t.Helper()
	require.Equal(t, planner.ResolvedTarget{Addr: want.Addr, Withdrawn: true, Reason: reason, Owner: webOwner}, res.Target)
	require.Equal(t, want, res.Binding)
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
			name:   "static address named by the route",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.40.0.10"),
			route:  routeFor(ip("10.40.0.10"), ""),
			addr:   "10.40.0.10",
			source: FromVia,
		},
		{
			name:   "static address named by via",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.40.0.10"),
			route:  routeFor(netip.Addr{}, "10.40.0.10"),
			addr:   "10.40.0.10",
			source: FromVia,
		},
		{
			name:   "address named by the route that is not static",
			trust:  true,
			nic:    nicOn(0, mac0, "vmbr0", 0, "10.40.0.10"),
			route:  routeFor(ip("10.40.0.12"), ""),
			addr:   "10.40.0.12",
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
				requireNotServed(t, res, tt.reason)
				require.Empty(t, s.prober.ops("dial"))
				return
			}
			owner := tt.ref.String()
			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Owner: owner}, res.Target)
			require.NotNil(t, res.Binding)
			require.Equal(t, owner, res.Binding.Owner)
			require.Equal(t, owner, res.Binding.Guest)
		})
	}
}

func TestResolveForwardingTableNode(t *testing.T) {
	tests := []struct {
		name      string
		guestNode string
		localNode string
		checked   bool
	}{
		{"guest on this node", localNode, localNode, true},
		{"guest on another node", "pve2", localNode, false},
		{"local node unknown", "pve2", "", true},
		{"guest node unknown", "", localNode, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario()
			s.web().Node = tt.guestNode
			s.settings.LocalNode = tt.localNode
			delete(s.prober.fdb, fdbKey("vmbr0", 0, mac0))

			res := s.resolve(t, webRoute(), nil)

			require.Len(t, s.prober.ops("arp"), 1, "ARP always applies")
			if tt.checked {
				requireNotServed(t, res, "MAC bc:24:11:00:00:01 not seen on bridge vmbr0")
				require.Len(t, s.prober.ops("fdb"), 1)
				return
			}
			requireServed(t, res, "10.20.0.10", t0)
			require.Empty(t, s.prober.ops("fdb"))
		})
	}
}

func TestResolveGuestMovedToAnotherNode(t *testing.T) {
	t.Run("answering", func(t *testing.T) {
		s := newScenario()
		s.web().Node = "pve2"

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireServed(t, res, "10.20.0.10", t0)
		require.Len(t, s.prober.ops("arp"), 1)
		require.Empty(t, s.prober.ops("fdb"))
	})

	t.Run("not answering ARP", func(t *testing.T) {
		s := newScenario()
		s.web().Node = "pve2"
		delete(s.prober.arp, arpKey("vmbr0", "10.20.0.10"))

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, "no ARP answer on vmbr0", withdrawnAt(boundTo("10.20.0.10"), t0))
		require.Empty(t, s.prober.ops("fdb"))
		require.Empty(t, s.prober.ops("dial"))
	})
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

	t.Run("earlier binding wins on the guest's own port", func(t *testing.T) {
		s := newScenario()
		s.addDB1(true, nicOn(0, mac0, "vmbr1", 0))

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireServed(t, res, "10.20.0.10", t0)
		require.Len(t, s.prober.ops("fdb"), 1)
	})

	t.Run("earlier binding on another port", func(t *testing.T) {
		s := newScenario()
		s.addDB1(true, nicOn(0, mac0, "vmbr0", 0))
		s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = "tap102i0"

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, "MAC bc:24:11:00:00:01 is on port tap102i0, not on the guest's own port",
			withdrawnAt(boundTo("10.20.0.10"), t0))
	})

	t.Run("earlier binding without the forwarding table", func(t *testing.T) {
		s := newScenario()
		s.web().Node = "pve2"
		s.addDB1(true, nicOn(0, mac0, "vmbr0", 0))

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, dupMAC0, withdrawnAt(boundTo("10.20.0.10"), t0))
		require.False(t, s.prober.touched(ip("10.20.0.10")))
	})

	t.Run("earlier binding on a trusted static address", func(t *testing.T) {
		s := newScenario()
		s.settings.TrustStatic = true
		s.settings.TrustedCIDRs = prefixes("10.20.0.0/24")
		s.prober.ifaces = []HostIface{{Name: "vmbr1", Addrs: prefixes("10.30.0.2/24")}}
		s.addDB1(true, nicOn(0, mac0, "vmbr1", 0))

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, dupMAC0, withdrawnAt(boundTo("10.20.0.10"), t0))
		require.Empty(t, s.prober.ops("dial"))
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

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, "MAC bc:24:11:00:00:02 is also configured on qemu/102", withdrawnAt(boundTo("10.20.0.10"), t0))
		require.Empty(t, s.prober.ops("dial"))
	})
}

func TestResolveBothGuestsBoundToOneMAC(t *testing.T) {
	setup := func(node string) (*scenario, model.Route, *Binding) {
		s := newScenario()
		s.web().Node = node
		s.addDB1(true, nicOn(0, mac0, "vmbr0", 0, "10.20.0.20"))
		s.guests[1].Node = node
		s.prober.arp[arpKey("vmbr0", "10.20.0.20")] = []string{mac0}
		dbRoute := routeTo(db1Ref)
		dbRoute.Hostname = "db.example.com"
		dbBound := &Binding{
			Owner: db1Ref.String(), Hostname: "db.example.com", Guest: db1Ref.String(),
			Addr: ip("10.20.0.20"), MAC: mac0, VerifiedAt: t0.Add(-time.Hour),
		}
		return s, dbRoute, dbBound
	}

	t.Run("on this node the forwarding table decides", func(t *testing.T) {
		s, dbRoute, dbBound := setup(localNode)

		web := s.resolve(t, webRoute(), boundTo("10.20.0.10"))
		db := s.resolve(t, dbRoute, dbBound)

		requireServed(t, web, "10.20.0.10", t0)
		require.Equal(t, planner.ResolvedTarget{
			Addr: ip("10.20.0.20"), Withdrawn: true, Owner: db1Ref.String(),
			Reason: "MAC bc:24:11:00:00:01 is on port tap101i0, not on the guest's own port",
		}, db.Target)
		require.Equal(t, withdrawnAt(dbBound, t0), db.Binding)
	})

	t.Run("on another node both are withdrawn", func(t *testing.T) {
		s, dbRoute, dbBound := setup("pve2")

		web := s.resolve(t, webRoute(), boundTo("10.20.0.10"))
		db := s.resolve(t, dbRoute, dbBound)

		requireWithdrawn(t, web, "MAC bc:24:11:00:00:01 is also configured on qemu/102", withdrawnAt(boundTo("10.20.0.10"), t0))
		require.Equal(t, planner.ResolvedTarget{
			Addr: ip("10.20.0.20"), Withdrawn: true, Owner: db1Ref.String(),
			Reason: "MAC bc:24:11:00:00:01 is also configured on qemu/101",
		}, db.Target)
		require.Empty(t, s.prober.ops("arp"))
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

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.11"), Reason: failed, Owner: webOwner}, res.Target, "after %s", after)
		require.Equal(t, provenAt(failingAt(boundTo("10.20.0.11"), t0), s.clock.t), res.Binding, "after %s", after)
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

	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.11"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)
	require.Equal(t, provenAt(prev, s.clock.t), res.Binding)
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
	prev := failingAt(boundTo("10.20.0.10"), t0.Add(-time.Minute))

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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario()
			tt.setup(s)
			prev := boundTo("10.20.0.10")

			res := s.resolve(t, webRoute(), prev)

			requireWithdrawn(t, res, tt.reason, withdrawnAt(boundTo("10.20.0.10"), t0))
			require.Nil(t, prev.FailingSince, "prev must not change")
			require.Empty(t, s.prober.ops("dial"))
		})
	}
}

func TestResolveIdentityLossKeepsFailingSince(t *testing.T) {
	s := newScenario()
	s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{foreignMAC}
	prev := failingAt(boundTo("10.20.0.10"), t0.Add(-30*time.Second))

	res := s.resolve(t, webRoute(), prev)

	requireWithdrawn(t, res, "10.20.0.10 answered by bc:24:11:ff:ff:01, which is not this guest",
		withdrawnAt(boundTo("10.20.0.10"), t0.Add(-30*time.Second)))
}

func TestResolveIdentityLossTriesOthersAtOnce(t *testing.T) {
	const foreign = "10.20.0.11 answered by bc:24:11:ff:ff:01, which is not this guest"

	t.Run("an alternative verifies", func(t *testing.T) {
		s := newScenario()
		s.web().NICs[0].Static = ips("10.20.0.10", "10.20.0.11")
		s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{foreignMAC}

		res := s.resolve(t, webRoute(), boundTo("10.20.0.11"))

		requireServed(t, res, "10.20.0.10", t0)
		require.Equal(t, []CandidateResult{
			{Addr: ip("10.20.0.11"), Source: FromStatic, Reason: foreign},
			{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true},
		}, res.Candidates)
	})

	t.Run("nothing else verifies", func(t *testing.T) {
		s := newScenario()
		s.web().NICs[0].Static = ips("10.20.0.10", "10.20.0.11")
		s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{foreignMAC}
		s.prober.dialErr[ip("10.20.0.10")] = errDial

		res := s.resolve(t, webRoute(), boundTo("10.20.0.11"))

		requireWithdrawn(t, res, foreign, withdrawnAt(boundTo("10.20.0.11"), t0))
		require.True(t, s.prober.touched(ip("10.20.0.10")))
	})
}

func TestResolveWithdrawnUntilIdentityPasses(t *testing.T) {
	s := newScenario()
	const foreign = "10.20.0.10 answered by bc:24:11:ff:ff:01, which is not this guest"

	// Identity lost.
	s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{foreignMAC}
	res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))
	requireWithdrawn(t, res, foreign, withdrawnAt(boundTo("10.20.0.10"), t0))

	// Guest stopped: still withdrawn, nothing probed.
	s.clock.t = t0.Add(10 * time.Second)
	s.web().Running = false
	s.prober.calls = nil
	res = s.resolve(t, webRoute(), res.Binding)
	requireWithdrawn(t, res, "guest is not running", withdrawnAt(boundTo("10.20.0.10"), t0))
	require.Empty(t, s.prober.calls)

	// Running again, but the host cannot be asked: still withdrawn, although
	// the last proof is fresh.
	s.clock.t = t0.Add(20 * time.Second)
	s.web().Running = true
	s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0}
	s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = errors.New("socket closed")
	res = s.resolve(t, webRoute(), res.Binding)
	requireWithdrawn(t, res, "ARP on vmbr0: socket closed", withdrawnAt(boundTo("10.20.0.10"), t0))

	// Identity holds but the port does not answer: no longer withdrawn.
	s.clock.t = t0.Add(30 * time.Second)
	delete(s.prober.arpErr, arpKey("vmbr0", "10.20.0.10"))
	s.prober.dialErr[ip("10.20.0.10")] = errDial
	res = s.resolve(t, webRoute(), res.Binding)
	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)
	require.Equal(t, provenAt(failingAt(boundTo("10.20.0.10"), t0), t0.Add(30*time.Second)), res.Binding)

	// Fully verified: served and reachable.
	s.clock.t = t0.Add(40 * time.Second)
	delete(s.prober.dialErr, ip("10.20.0.10"))
	res = s.resolve(t, webRoute(), res.Binding)
	requireServed(t, res, "10.20.0.10", t0.Add(40*time.Second))
}

func TestResolveTakeoverDuringLaterCandidate(t *testing.T) {
	s, prev := stickyScenario()
	s.prober.dialErr[ip("10.20.0.10")] = errDial

	// The port closes.
	res := s.resolve(t, webRoute(), prev)
	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.11"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)

	// Still closed beyond StickyFor; the other candidate does not answer either.
	s.clock.t = t0.Add(3 * time.Minute)
	res = s.resolve(t, webRoute(), res.Binding)
	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.11"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)
	require.True(t, s.prober.touched(ip("10.20.0.10")))

	// Another machine takes the address over, and the context ends while the
	// other candidate is being tried.
	s.clock.t = t0.Add(4 * time.Minute)
	s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{foreignMAC}
	ctx, cancel := context.WithCancel(t.Context())
	s.prober.cancelOn, s.prober.cancelAddr, s.prober.cancel = "arp", ip("10.20.0.10"), cancel
	res = s.resolveCtx(ctx, webRoute(), res.Binding)

	want := withdrawnAt(provenAt(boundTo("10.20.0.11"), t0.Add(3*time.Minute)), t0)
	requireWithdrawn(t, res, "10.20.0.11 answered by bc:24:11:ff:ff:01, which is not this guest", want)
}

func TestResolveIdentityClearsWithdrawn(t *testing.T) {
	s := newScenario()
	s.prober.dialErr[ip("10.20.0.10")] = errDial
	prev := withdrawnAt(boundTo("10.20.0.10"), t0.Add(-time.Minute))

	res := s.resolve(t, webRoute(), prev)

	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)
	require.Equal(t, provenAt(failingAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)), t0), res.Binding)
}

func TestResolveTriesAtMostSixteenCandidates(t *testing.T) {
	s := newScenario()
	var static []string
	for i := range 20 {
		static = append(static, fmt.Sprintf("10.20.0.%d", 100+i))
	}
	s.web().NICs[0].Static = ips(static...)

	res := s.resolve(t, webRoute(), boundTo("10.20.0.119"))

	requireWithdrawn(t, res, "no ARP answer on vmbr0", withdrawnAt(boundTo("10.20.0.119"), t0))
	require.Len(t, s.prober.ops("arp"), 16)
	require.Len(t, res.Candidates, 20)
	require.Equal(t, ip("10.20.0.119"), res.Candidates[0].Addr, "the bound address comes first")
	for i, c := range res.Candidates {
		if i < 16 {
			require.Equal(t, "no ARP answer on vmbr0", c.Reason, "candidate %d", i)
			continue
		}
		require.Equal(t, "not tried: too many candidates", c.Reason, "candidate %d", i)
		require.False(t, s.prober.touched(c.Addr))
	}
}

func TestResolveGuestNotRunning(t *testing.T) {
	const reason = "guest is not running"
	tests := []struct {
		name    string
		prev    *Binding
		target  planner.ResolvedTarget
		binding *Binding
	}{
		{
			name:    "with a binding",
			prev:    boundTo("10.20.0.10"),
			target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: reason, Owner: webOwner},
			binding: withdrawnAt(boundTo("10.20.0.10"), t0),
		},
		{
			name:    "with a failing binding",
			prev:    failingAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
			target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: reason, Owner: webOwner},
			binding: withdrawnAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
		},
		{
			name:   "without a binding",
			target: planner.ResolvedTarget{Reason: reason},
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
			require.Equal(t, tt.binding, res.Binding)
			require.Empty(t, s.prober.calls)
		})
	}
}

func TestResolveGuestNotFound(t *testing.T) {
	const reason = "guest not found in inventory"
	tests := []struct {
		name    string
		prev    *Binding
		target  planner.ResolvedTarget
		binding *Binding
	}{
		{
			name:    "with a binding",
			prev:    boundTo("10.20.0.10"),
			target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: reason, Owner: webOwner},
			binding: withdrawnAt(boundTo("10.20.0.10"), t0),
		},
		{
			name:   "without a binding",
			target: planner.ResolvedTarget{Reason: reason},
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
			s.guests = nil

			res := s.resolve(t, webRoute(), tt.prev)

			require.Equal(t, tt.target, res.Target)
			require.Equal(t, tt.binding, res.Binding)
			require.Empty(t, s.prober.calls)
		})
	}
}

// otherRoutes edit a binding of this route into one of another route.
var otherRoutes = []struct {
	name string
	edit func(b *Binding)
}{
	{"another owner", func(b *Binding) { b.Owner = db1Ref.String() }},
	{"another hostname", func(b *Binding) { b.Hostname = "db.example.com" }},
	{"another guest", func(b *Binding) { b.Guest = db1Ref.String() }},
}

func TestResolveIgnoresBindingOfAnotherRoute(t *testing.T) {
	for _, tt := range otherRoutes {
		t.Run(tt.name+" on a stopped guest", func(t *testing.T) {
			s := newScenario()
			s.web().Running = false
			prev := boundTo("10.20.0.10")
			tt.edit(prev)

			res := s.resolve(t, webRoute(), prev)

			requireNotServed(t, res, "guest is not running")
		})
		t.Run(tt.name+" on a running guest", func(t *testing.T) {
			s := newScenario()
			s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}
			prev := boundTo("10.20.0.11")
			tt.edit(prev)

			res := s.resolve(t, webRoute(), prev)

			requireServed(t, res, "10.20.0.10", t0)
			require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true}}, res.Candidates)
			require.False(t, s.prober.touched(ip("10.20.0.11")))
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

func TestResolveNodeAddress(t *testing.T) {
	const reason = "address of this node"
	tests := []struct {
		name   string
		static []string
		prev   *Binding
		served string // empty when nothing is served
	}{
		{name: "address of the bridge", static: []string{"10.20.0.2"}},
		{name: "address of another interface", static: []string{"10.30.0.2"}},
		{name: "bound address became the node's", static: []string{"10.20.0.2"}, prev: boundTo("10.20.0.2")},
		{name: "before a good candidate", static: []string{"10.30.0.2", "10.20.0.10"}, served: "10.20.0.10"},
		{
			name: "bound address became the node's before a good candidate", static: []string{"10.20.0.2", "10.20.0.10"},
			prev: boundTo("10.20.0.2"), served: "10.20.0.10",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario()
			s.deny = NewDenylist(nil, nil)
			s.prober.ifaces = append(s.prober.ifaces, HostIface{Name: "vmbr1", Addrs: prefixes("10.30.0.2/24")})
			s.web().NICs[0].Static = ips(tt.static...)
			for _, a := range tt.static {
				s.prober.arp[arpKey("vmbr0", a)] = []string{mac0}
			}

			res := s.resolve(t, webRoute(), tt.prev)

			require.Equal(t, CandidateResult{Addr: ip(tt.static[0]), Source: FromStatic, Reason: reason}, res.Candidates[0])
			require.False(t, s.prober.touched(ip(tt.static[0])))
			if tt.served != "" {
				requireServed(t, res, tt.served, t0)
				return
			}
			require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: reason}, res.Target)
			require.Nil(t, res.Binding)
		})
	}
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

	res := s.resolve(t, webRoute(), nil)

	requireNotServed(t, res, "no candidate address")
	require.Empty(t, s.prober.calls)
}

func TestResolveBindingOutlivesSources(t *testing.T) {
	t.Run("no source reports the address", func(t *testing.T) {
		s := newScenario()
		s.web().NICs[0].Static = nil

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireServed(t, res, "10.20.0.10", t0)
		require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.10"), Source: FromBinding, OK: true}}, res.Candidates)
	})

	t.Run("verified before the other candidates", func(t *testing.T) {
		s := newScenario()
		s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}

		res := s.resolve(t, webRoute(), boundTo("10.20.0.11"))

		requireServed(t, res, "10.20.0.11", t0)
		require.Equal(t, []CandidateResult{
			{Addr: ip("10.20.0.11"), Source: FromBinding, OK: true},
			{Addr: ip("10.20.0.10"), Source: FromStatic, Reason: "not tried"},
		}, res.Candidates)
	})

	t.Run("on the NIC that has the MAC", func(t *testing.T) {
		s := newScenario()
		s.web().NICs = []model.NIC{nicOn(0, mac1, "vmbr1", 0), nicOn(1, mac0, "vmbr0", 0)}
		s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = "tap101i1"

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireServed(t, res, "10.20.0.10", t0)
	})

	t.Run("unreported address that fails identity gives way", func(t *testing.T) {
		s := newScenario()

		res := s.resolve(t, webRoute(), boundTo("10.20.0.99"))

		requireServed(t, res, "10.20.0.10", t0)
		require.Equal(t, []CandidateResult{
			{Addr: ip("10.20.0.99"), Source: FromBinding, Reason: "no ARP answer on vmbr0"},
			{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true},
		}, res.Candidates)
	})

	t.Run("unreported address that fails identity with nothing else", func(t *testing.T) {
		s := newScenario()
		s.web().NICs[0].Static = nil

		res := s.resolve(t, webRoute(), boundTo("10.20.0.99"))

		requireWithdrawn(t, res, "no ARP answer on vmbr0", withdrawnAt(boundTo("10.20.0.99"), t0))
	})
}

func TestResolveBindingDropped(t *testing.T) {
	tests := []struct {
		name  string
		route model.Route
		edit  func(s *scenario)
	}{
		{
			name:  "no NIC has the MAC any more",
			route: webRoute(),
			edit: func(s *scenario) {
				s.web().NICs[0].MAC = mac2
				s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac2}
				s.prober.fdb[fdbKey("vmbr0", 0, mac2)] = "tap101i0"
			},
		},
		{name: "route names another address", route: routeFor(ip("10.20.0.10"), "")},
		{name: "via names another address", route: routeFor(netip.Addr{}, "10.20.0.10")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario()
			s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}
			if tt.edit != nil {
				tt.edit(s)
			}

			res := s.resolve(t, tt.route, boundTo("10.20.0.11"))

			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Owner: webOwner}, res.Target)
			require.Equal(t, ip("10.20.0.10"), res.Binding.Addr)
			require.Equal(t, s.web().NICs[0].MAC, res.Binding.MAC)
			require.Len(t, res.Candidates, 1)
			require.False(t, s.prober.touched(ip("10.20.0.11")))
		})
	}

	t.Run("kept when the route names the bound address", func(t *testing.T) {
		s := newScenario()
		s.prober.dialErr[ip("10.20.0.10")] = errDial

		res := s.resolve(t, routeFor(ip("10.20.0.10"), ""), boundTo("10.20.0.10"))

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)
		require.Equal(t, provenAt(failingAt(boundTo("10.20.0.10"), t0), t0), res.Binding)
		require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.10"), Source: FromVia, Reason: "port 80: connection refused"}}, res.Candidates)
	})

	t.Run("via names another NIC", func(t *testing.T) {
		s := newScenario()
		s.web().NICs = append(s.web().NICs, nicOn(1, mac1, "vmbr0", 0, "10.20.0.11"))
		s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac1}
		s.prober.fdb[fdbKey("vmbr0", 0, mac1)] = "tap101i1"

		res := s.resolve(t, routeFor(netip.Addr{}, "net1"), boundTo("10.20.0.10"))

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.11"), Reachable: true, Owner: webOwner}, res.Target)
		require.Equal(t, mac1, res.Binding.MAC)
		require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.11"), Source: FromStatic, OK: true}}, res.Candidates)
		require.False(t, s.prober.touched(ip("10.20.0.10")))
	})

	t.Run("kept when via names the NIC that has the MAC", func(t *testing.T) {
		s := newScenario()
		s.web().NICs[0].Static = nil

		res := s.resolve(t, routeFor(netip.Addr{}, "net0"), boundTo("10.20.0.10"))

		requireServed(t, res, "10.20.0.10", t0)
	})
}

func TestResolveAddressMovesToAnotherNIC(t *testing.T) {
	s := newScenario()
	s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 0), nicOn(1, mac1, "vmbr1", 0, "10.20.0.10")}
	s.prober.ifaces = append(s.prober.ifaces, HostIface{Name: "vmbr1", Addrs: prefixes("10.20.0.3/24")})
	delete(s.prober.arp, arpKey("vmbr0", "10.20.0.10"))
	s.prober.arp[arpKey("vmbr1", "10.20.0.10")] = []string{mac1}
	s.prober.fdb[fdbKey("vmbr1", 0, mac1)] = "tap101i1"

	res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Owner: webOwner}, res.Target)
	require.Equal(t, mac1, res.Binding.MAC)
	require.Equal(t, []CandidateResult{
		{Addr: ip("10.20.0.10"), Source: FromBinding, Reason: "no ARP answer on vmbr0"},
		{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true},
	}, res.Candidates)
}

// proberErrors fail each prober call that comes before the dial.
var proberErrors = []struct {
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

func TestResolveProberErrorIsNotWithdrawal(t *testing.T) {
	for _, tt := range proberErrors {
		t.Run(tt.name+" on the binding", func(t *testing.T) {
			s := newScenario()
			tt.setup(s)

			res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: tt.reason, Owner: webOwner}, res.Target)
			require.Equal(t, failingAt(boundTo("10.20.0.10"), t0), res.Binding)
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

func TestResolveProberErrorAndProofAge(t *testing.T) {
	for _, tt := range proberErrors {
		t.Run(tt.name+" with a fresh proof", func(t *testing.T) {
			s := newScenario()
			tt.setup(s)
			prev := provenAt(boundTo("10.20.0.10"), t0.Add(-5*time.Minute))

			res := s.resolve(t, webRoute(), prev)

			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: tt.reason, Owner: webOwner}, res.Target)
			require.Equal(t, failingAt(prev, t0), res.Binding)
		})
		t.Run(tt.name+" with a stale proof", func(t *testing.T) {
			s := newScenario()
			tt.setup(s)
			prev := provenAt(boundTo("10.20.0.10"), t0.Add(-5*time.Minute-time.Second))

			res := s.resolve(t, webRoute(), prev)

			requireWithdrawn(t, res, "identity not confirmed for 5m1s", withdrawnAt(prev, t0))
		})
	}

	t.Run("custom MaxProofAge", func(t *testing.T) {
		s := newScenario()
		s.settings.MaxProofAge = 30 * time.Second
		s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = errors.New("socket closed")

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, "identity not confirmed for 1m0s", withdrawnAt(boundTo("10.20.0.10"), t0))
	})

	t.Run("stale proof gives way to a verified alternative", func(t *testing.T) {
		s := newScenario()
		s.web().NICs[0].Static = ips("10.20.0.10", "10.20.0.11")
		s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}
		s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = errors.New("socket closed")

		res := s.resolve(t, webRoute(), provenAt(boundTo("10.20.0.10"), t0.Add(-10*time.Minute)))

		requireServed(t, res, "10.20.0.11", t0)
	})

	t.Run("fresh proof keeps the binding without trying others", func(t *testing.T) {
		s := newScenario()
		s.web().NICs[0].Static = ips("10.20.0.10", "10.20.0.11")
		s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}
		s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = errors.New("socket closed")

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		require.Equal(t, ip("10.20.0.10"), res.Target.Addr)
		require.False(t, res.Target.Withdrawn)
		require.False(t, s.prober.touched(ip("10.20.0.11")))
	})
}

func TestResolveDialFailureWithoutBinding(t *testing.T) {
	s := newScenario()
	s.prober.dialErr[ip("10.20.0.10")] = errDial

	res := s.resolve(t, webRoute(), nil)

	requireNotServed(t, res, "port 80: connection refused")
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
	const owner = "manual/app"
	route := func(addr string, allowNode bool) model.Route {
		return model.Route{
			Hostname: "app.example.com",
			Target:   model.Target{Scheme: model.SchemeHTTP, Addr: ip(addr), Port: 8080},
			Options:  model.RouteOptions{AllowNode: allowNode},
			Source:   model.SourceManual,
			ManualID: "app",
		}
	}
	served := func(addr string) planner.ResolvedTarget {
		return planner.ResolvedTarget{Addr: ip(addr), Reachable: true, Owner: owner}
	}
	rejected := func(reason string) planner.ResolvedTarget {
		return planner.ResolvedTarget{Rejected: true, Reason: reason}
	}
	tests := []struct {
		name   string
		route  model.Route
		target planner.ResolvedTarget
		calls  []string
	}{
		{name: "answering", route: route("10.20.0.50", false), target: served("10.20.0.50"), calls: []string{"interfaces", "dial"}},
		{name: "address of a cluster node", route: route("10.20.0.2", false), target: rejected("address of a cluster node")},
		{
			name: "address of a host interface", route: route("10.30.0.2", false),
			target: rejected("address of this node"), calls: []string{"interfaces"},
		},
		{name: "cluster node allowed", route: route("10.20.0.2", true), target: served("10.20.0.2"), calls: []string{"dial"}},
		{name: "host interface allowed", route: route("10.30.0.2", true), target: served("10.30.0.2"), calls: []string{"dial"}},
		{name: "loopback with allowNode", route: route("127.0.0.1", true), target: rejected("loopback address")},
		{name: "link-local with allowNode", route: route("169.254.0.1", true), target: rejected("link-local address")},
		{name: "reserved node address with allowNode", route: route("10.99.0.1", true), target: rejected("reserved by pco")},
		{name: "no address", route: model.Route{Hostname: "app.example.com", Source: model.SourceManual, ManualID: "app"}, target: rejected("not an IPv4 address")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario()
			s.deny = NewDenylist(ips("10.20.0.2", "10.99.0.1"), prefixes("10.99.0.0/24"))
			s.prober.ifaces = append(s.prober.ifaces, HostIface{Name: "vmbr1", Addrs: prefixes("10.30.0.2/24")})

			res := s.resolve(t, tt.route, &Binding{Owner: owner, Hostname: "app.example.com", Addr: ip("10.20.0.51")})

			require.Equal(t, tt.target, res.Target)
			require.Nil(t, res.Binding)
			var ops []string
			for _, c := range s.prober.calls {
				ops = append(ops, c.op)
			}
			require.Equal(t, tt.calls, ops)
		})
	}

	t.Run("not answering", func(t *testing.T) {
		s := newScenario()
		s.prober.dialErr[ip("10.20.0.50")] = errDial

		res := s.resolve(t, route("10.20.0.50", false), nil)

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.50"), Reason: "port 8080: connection refused", Owner: owner}, res.Target)
		require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.50"), Source: FromVia, Reason: "port 8080: connection refused"}}, res.Candidates)
	})

	t.Run("interfaces not listed", func(t *testing.T) {
		s := newScenario()
		s.prober.ifacesErr = errors.New("netlink closed")

		res := s.resolve(t, route("10.20.0.50", false), nil)

		require.Equal(t, planner.ResolvedTarget{Reason: "listing host interfaces: netlink closed"}, res.Target)
		require.Empty(t, s.prober.ops("dial"))
	})

	t.Run("cancelled", func(t *testing.T) {
		s := newScenario()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		res := s.resolveCtx(ctx, route("10.20.0.50", false), nil)

		require.Equal(t, planner.ResolvedTarget{Reason: "resolve cancelled"}, res.Target)
		require.Empty(t, s.prober.calls)
	})
}

func TestResolveAllowNodeIgnoredForGuests(t *testing.T) {
	tests := []struct {
		name   string
		static string
		reason string
	}{
		{"address of a cluster node", "10.20.0.2", "address of a cluster node"},
		{"address of a host interface", "10.30.0.2", "address of this node"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario()
			s.prober.ifaces = append(s.prober.ifaces, HostIface{Name: "vmbr1", Addrs: prefixes("10.30.0.2/24")})
			s.web().NICs[0].Static = ips(tt.static)
			route := webRoute()
			route.Options.AllowNode = true

			res := s.resolve(t, route, nil)

			require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: tt.reason}, res.Target)
			require.False(t, s.prober.touched(ip(tt.static)))
		})
	}
}

func TestResolveCancelled(t *testing.T) {
	const reason = "resolve cancelled"
	cancelled := func(t *testing.T) context.Context {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		return ctx
	}

	t.Run("before resolving", func(t *testing.T) {
		stale := provenAt(boundTo("10.20.0.10"), t0.Add(-10*time.Minute))
		staleWithdrawn := *stale
		staleWithdrawn.Withdrawn = true
		tests := []struct {
			name    string
			running bool
			missing bool
			prev    *Binding
			target  planner.ResolvedTarget
			binding *Binding
		}{
			{
				name: "fresh binding", running: true, prev: boundTo("10.20.0.10"),
				target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Reason: reason, Owner: webOwner},
				binding: boundTo("10.20.0.10"),
			},
			{
				name: "failing binding", running: true, prev: failingAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
				target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: reason, Owner: webOwner},
				binding: failingAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
			},
			{
				name: "withdrawn binding", running: true, prev: withdrawnAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
				target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: reason, Owner: webOwner},
				binding: withdrawnAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
			},
			{
				name: "stale binding", running: true, prev: stale,
				target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: "identity not confirmed for 10m0s", Owner: webOwner},
				binding: &staleWithdrawn,
			},
			{
				name: "stopped guest", prev: boundTo("10.20.0.10"),
				target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: "guest is not running", Owner: webOwner},
				binding: withdrawnAt(boundTo("10.20.0.10"), t0),
			},
			{
				name: "missing guest", missing: true, prev: boundTo("10.20.0.10"),
				target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: "guest not found in inventory", Owner: webOwner},
				binding: withdrawnAt(boundTo("10.20.0.10"), t0),
			},
			{name: "no binding", running: true, target: planner.ResolvedTarget{Reason: reason}},
			{
				name: "denied binding", running: true, prev: boundTo("10.20.0.2"),
				target: planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				s := newScenario()
				s.web().Running = tt.running
				if tt.missing {
					s.guests = nil
				}

				res := s.resolveCtx(cancelled(t), webRoute(), tt.prev)

				require.Equal(t, tt.target, res.Target)
				require.Equal(t, tt.binding, res.Binding)
				require.Empty(t, s.prober.calls)
			})
		}
	})

	for _, op := range []string{"interfaces", "arp", "fdb", "dial"} {
		t.Run("during "+op+" of the binding", func(t *testing.T) {
			s := newScenario()
			ctx, cancel := context.WithCancel(t.Context())
			s.prober.cancelOn, s.prober.cancel = op, cancel

			res := s.resolveCtx(ctx, webRoute(), boundTo("10.20.0.10"))

			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Reason: reason, Owner: webOwner}, res.Target)
			require.Equal(t, boundTo("10.20.0.10"), res.Binding, "no FailingSince stamp")
			require.Len(t, s.prober.ops(op), 1, "nothing is asked after the cancel")
			require.Equal(t, op, s.prober.calls[len(s.prober.calls)-1].op)
		})
	}

	t.Run("answer after the end is no proof", func(t *testing.T) {
		for _, op := range []string{"interfaces", "arp", "fdb", "dial"} {
			t.Run(op, func(t *testing.T) {
				s := newScenario()
				ctx, cancel := context.WithCancel(t.Context())
				s.prober.cancelOn, s.prober.cancelSilently, s.prober.cancel = op, true, cancel
				prev := withdrawnAt(boundTo("10.20.0.10"), t0.Add(-time.Minute))

				res := s.resolveCtx(ctx, webRoute(), prev)

				requireWithdrawn(t, res, reason, prev)
				require.Equal(t, op, s.prober.calls[len(s.prober.calls)-1].op, "nothing is asked after the end")
			})
		}
	})

	t.Run("negative answer after the end is not used", func(t *testing.T) {
		tests := []struct {
			op    string
			setup func(s *scenario)
		}{
			{"interfaces", func(s *scenario) { s.prober.ifaces = nil }},
			{"arp", func(s *scenario) { s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{foreignMAC} }},
			{"fdb", func(s *scenario) { s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = "tap102i0" }},
		}
		for _, tt := range tests {
			t.Run(tt.op, func(t *testing.T) {
				s := newScenario()
				tt.setup(s)
				ctx, cancel := context.WithCancel(t.Context())
				s.prober.cancelOn, s.prober.cancelSilently, s.prober.cancel = tt.op, true, cancel

				res := s.resolveCtx(ctx, webRoute(), boundTo("10.20.0.10"))

				require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Reason: reason, Owner: webOwner}, res.Target)
				require.Equal(t, boundTo("10.20.0.10"), res.Binding)
			})
		}
	})

	t.Run("answer after the end without a binding", func(t *testing.T) {
		s := newScenario()
		ctx, cancel := context.WithCancel(t.Context())
		s.prober.cancelOn, s.prober.cancelSilently, s.prober.cancel = "dial", true, cancel

		res := s.resolveCtx(ctx, webRoute(), nil)

		requireNotServed(t, res, reason)
	})

	t.Run("while trying other candidates after a dial failure", func(t *testing.T) {
		s, prev := stickyScenario()
		prev.FailingSince = timePtr(t0.Add(-time.Hour))
		ctx, cancel := context.WithCancel(t.Context())
		s.prober.cancelOn, s.prober.cancelAddr, s.prober.cancel = "arp", ip("10.20.0.10"), cancel

		res := s.resolveCtx(ctx, webRoute(), prev)

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.11"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)
		require.Equal(t, provenAt(prev, t0), res.Binding)
	})

	t.Run("while trying other candidates after a stale prober error", func(t *testing.T) {
		s := newScenario()
		s.web().NICs[0].Static = ips("10.20.0.10", "10.20.0.11")
		s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = errors.New("socket closed")
		ctx, cancel := context.WithCancel(t.Context())
		s.prober.cancelOn, s.prober.cancelAddr, s.prober.cancel = "arp", ip("10.20.0.11"), cancel
		prev := provenAt(boundTo("10.20.0.10"), t0.Add(-10*time.Minute))

		res := s.resolveCtx(ctx, webRoute(), prev)

		requireWithdrawn(t, res, "identity not confirmed for 10m0s", withdrawnAt(prev, t0))
	})

	t.Run("without a binding", func(t *testing.T) {
		s := newScenario()
		ctx, cancel := context.WithCancel(t.Context())
		s.prober.cancelOn, s.prober.cancel = "dial", cancel

		res := s.resolveCtx(ctx, webRoute(), nil)

		requireNotServed(t, res, reason)
	})
}
