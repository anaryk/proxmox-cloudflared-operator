package resolve

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
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
	op    string // interfaces, route, arp, fdb, dial
	iface string // arp: interface; fdb: bridge
	vlan  int
	mac   string
	addr  netip.Addr
	port  uint16
}

// fakeRoute is the kernel's route to one address.
type fakeRoute struct {
	iface  string
	onLink bool
	err    error
}

// fakeProber answers from a script and records every call. The script must
// not change while a call runs; recording is safe for concurrent calls.
type fakeProber struct {
	ifaces    []HostIface
	ifacesErr error
	// routes overrides the route to an address. Without an entry, an address
	// in the network of an interface is on-link there (the most specific
	// network wins), and any other leaves through gateway, or has no route
	// when gateway is empty.
	routes  map[string]fakeRoute
	gateway string
	arp     map[string][]string // arpKey -> answering MACs
	arpErr  map[string]error    // arpKey
	fdb     map[string][]string // fdbKey -> ports the MAC is learned on
	fdbErr  error               // every forwarding-table lookup
	fdbErrs map[string]error    // fdbKey
	dialErr map[netip.Addr]error

	mu    sync.Mutex
	calls []probeCall

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
		routes:  map[string]fakeRoute{},
		arp:     map[string][]string{},
		arpErr:  map[string]error{},
		fdb:     map[string][]string{},
		fdbErrs: map[string]error{},
		dialErr: map[netip.Addr]error{},
	}
}

func arpKey(iface, addr string) string { return iface + " " + addr }

func fdbKey(bridge string, vlan int, mac string) string {
	return fmt.Sprintf("%s/%d/%s", bridge, vlan, mac)
}

func (f *fakeProber) record(c probeCall) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func (f *fakeProber) Route(_ context.Context, addr netip.Addr) (string, bool, error) {
	if err := f.record(probeCall{op: "route", addr: addr}); err != nil {
		return "", false, err
	}
	if r, ok := f.routes[addr.String()]; ok {
		return r.iface, r.onLink, r.err
	}
	iface, bits := "", -1
	for _, ifc := range f.ifaces {
		for _, p := range ifc.Addrs {
			if p.Contains(addr) && p.Bits() > bits {
				iface, bits = ifc.Name, p.Bits()
			}
		}
	}
	switch {
	case iface != "":
		return iface, true, nil
	case f.gateway != "":
		return f.gateway, false, nil
	}
	return "", false, fmt.Errorf("route to %s: %w", addr, ErrNoRoute)
}

func (f *fakeProber) ARP(_ context.Context, iface string, addr netip.Addr) ([]string, error) {
	if err := f.record(probeCall{op: "arp", iface: iface, addr: addr}); err != nil {
		return nil, err
	}
	key := arpKey(iface, addr.String())
	return f.arp[key], f.arpErr[key]
}

func (f *fakeProber) FDBPorts(_ context.Context, bridge string, vlan int, mac string) ([]string, error) {
	if err := f.record(probeCall{op: "fdb", iface: bridge, vlan: vlan, mac: mac}); err != nil {
		return nil, err
	}
	if f.fdbErr != nil {
		return nil, f.fdbErr
	}
	if err := f.fdbErrs[fdbKey(bridge, vlan, mac)]; err != nil {
		return nil, err
	}
	return slices.Clone(f.fdb[fdbKey(bridge, vlan, mac)]), nil
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

func denylist(t *testing.T, nodes []netip.Addr, extra []netip.Prefix) Denylist {
	t.Helper()
	d, err := NewDenylist(nodes, extra)
	require.NoError(t, err)
	return d
}

// scenario is web-1 (qemu/101) running on the local node pve1 with net0 on
// vmbr0 and the static address 10.20.0.10, which answers ARP from its own MAC
// on its own port. The node has 10.20.0.2/24 on vmbr0 and its default route
// leaves through vmbr0; pve2 is the other node of the cluster.
type scenario struct {
	prober   *fakeProber
	clock    *clock
	settings Settings
	guests   []model.Guest
	nodes    []inventory.Node
	complete bool
	deny     Denylist
}

func newScenario(t *testing.T) *scenario {
	p := newFakeProber()
	p.ifaces = []HostIface{{Name: "vmbr0", Addrs: prefixes("10.20.0.2/24")}}
	p.gateway = "vmbr0"
	p.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0}
	p.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"tap101i0"}
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
		nodes:    []inventory.Node{{Name: localNode, Online: true, Local: true}, {Name: "pve2", Online: true}},
		complete: true,
		deny:     denylist(t, ips("10.20.0.2"), nil),
	}
}

func (s *scenario) web() *model.Guest { return &s.guests[0] }

func (s *scenario) addDB1(running bool, nics ...model.NIC) {
	s.guests = append(s.guests, model.Guest{Ref: db1Ref, Name: "db-1", Node: localNode, Running: running, NICs: nics})
}

func (s *scenario) snapshot() inventory.Snapshot {
	return inventory.Snapshot{Guests: s.guests, Nodes: s.nodes, Complete: s.complete, TakenAt: s.clock.t}
}

func (s *scenario) resolve(t *testing.T, route model.Route, prev *Binding) Result {
	t.Helper()
	return s.resolveCtx(t.Context(), route, prev)
}

func (s *scenario) resolveCtx(ctx context.Context, route model.Route, prev *Binding) Result {
	r := NewResolver(s.prober, s.settings, s.clock.now)
	return r.Resolve(ctx, route, s.snapshot(), prev, s.deny)
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

// requireServed asserts that addr is served and bound, proven at at on the
// guest's own port.
func requireServed(t *testing.T, res Result, addr string, at time.Time) {
	t.Helper()
	requireServedAt(t, res, addr, at, LevelPort)
}

// requireServedAt is requireServed for a proof of the given level.
func requireServedAt(t *testing.T, res Result, addr string, at time.Time, level Level) {
	t.Helper()
	require.Equal(t, planner.ResolvedTarget{Addr: ip(addr), Reachable: true, Owner: webOwner}, res.Target)
	require.Equal(t, &Binding{Owner: webOwner, Hostname: webHost, Guest: webOwner, Addr: ip(addr), MAC: mac0, VerifiedAt: at, Level: level}, unplaced(res.Binding))
	require.Equal(t, level, res.Level)
}

// unplaced returns a copy of b without when it was bound and where its MAC
// was placed, which the tests of those compare.
func unplaced(b *Binding) *Binding {
	if b == nil {
		return nil
	}
	c := *b
	c.Since, c.Bridge, c.Port, c.Ports = time.Time{}, "", "", nil
	return &c
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
	require.Empty(t, res.Level, "a withdrawn address is proven at no level")
}

// stickyScenario has two healthy candidates on net0; the binding is on the
// second one, which stops answering on port 80.
func stickyScenario(t *testing.T) (*scenario, *Binding) {
	s := newScenario(t)
	s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 0, "10.20.0.10", "10.20.0.11")}
	s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}
	s.prober.dialErr[ip("10.20.0.11")] = errDial
	return s, boundTo("10.20.0.11")
}
