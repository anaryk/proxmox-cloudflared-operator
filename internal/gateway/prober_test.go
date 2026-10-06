package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// recordingProber answers from a script and records each call as a line.
type recordingProber struct {
	name   string
	ifaces []resolve.HostIface
	macs   []string
	err    error

	mu    *sync.Mutex
	calls *[]string
}

func (p recordingProber) record(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	*p.calls = append(*p.calls, p.name+" "+fmt.Sprintf(format, args...))
}

func (p recordingProber) Interfaces(context.Context) ([]resolve.HostIface, error) {
	p.record("interfaces")
	return p.ifaces, p.err
}

func (p recordingProber) Route(_ context.Context, iface string, addr netip.Addr) (string, bool, error) {
	p.record("route %s %s", iface, addr)
	return iface, true, p.err
}

func (p recordingProber) ARP(_ context.Context, iface string, addr netip.Addr) ([]string, error) {
	p.record("arp %s %s", iface, addr)
	return p.macs, p.err
}

func (p recordingProber) FDBPorts(_ context.Context, bridge string, vlan int, mac string) ([]string, error) {
	p.record("fdb %s %d %s", bridge, vlan, mac)
	return []string{"tap101i0"}, p.err
}

func (p recordingProber) Dial(_ context.Context, iface string, target netip.AddrPort) error {
	p.record("dial %s %s", iface, target)
	return p.err
}

// dialBackend is a backend whose namespace is a handle of the test's and
// whose dials are recorded; nothing else of it is called.
type dialBackend struct {
	Backend
	ns      netns.NsHandle
	dialErr error
	rec     recordingProber
	closed  *bool
}

func (b dialBackend) Netns() netns.NsHandle { return b.ns }

func (b dialBackend) Dial(_ context.Context, leg Leg, addr netip.AddrPort) (net.Conn, error) {
	b.rec.record("dial %s mark %d from %s to %s", leg.Iface(), leg.Mark, leg.Prefix.Addr(), addr)
	if b.dialErr != nil {
		return nil, b.dialErr
	}
	c, s := net.Pipe()
	_ = s.Close()
	*b.closed = false
	return closeRecorder{Conn: c, closed: b.closed}, nil
}

type closeRecorder struct {
	net.Conn
	closed *bool
}

func (c closeRecorder) Close() error {
	*c.closed = true
	return c.Conn.Close()
}

var (
	proberLegA = Leg{ID: "vmbr1", Mark: 1, Bridge: "vmbr1", Prefix: prefix("10.50.0.250/24"), MAC: mac("02:50:43:4f:00:01")}
	proberLegB = Leg{ID: "vmbr0.20", Mark: 2, Bridge: "vmbr0", VLAN: 20, Prefix: prefix("10.60.0.250/24"), MAC: mac("02:50:43:4f:00:02")}
	proberLegC = Leg{ID: "vmbr2.30", Mark: 3, Bridge: "vmbr2", VLAN: 30, Prefix: prefix("10.70.0.250/24"), MAC: mac("02:50:43:4f:00:03")}
	proberLegM = Leg{ID: ManagedBridge, Mark: 4, Bridge: ManagedBridge, Prefix: prefix("10.203.118.1/24"), MAC: ManagedLegMAC, Managed: true}
)

// proberHarness is a gateway prober over recording fakes: the host's
// prober, the prober asked on a leg, the backend, the move into the
// namespace and the route lookup in it.
type proberHarness struct {
	p      *prober
	legs   []Leg
	calls  []string
	mu     sync.Mutex
	closed bool
	route  legRoute
}

const testNS = netns.NsHandle(42)

func newProberHarness(host []resolve.HostIface, legs ...Leg) *proberHarness {
	h := &proberHarness{legs: legs}
	rec := func(name string) recordingProber {
		return recordingProber{name: name, mu: &h.mu, calls: &h.calls}
	}
	hostRec := rec("host")
	hostRec.ifaces = host
	hostRec.macs = []string{"bc:24:11:00:00:09"}
	legRec := rec("leg")
	legRec.macs = []string{"bc:24:11:00:00:01"}
	b := dialBackend{ns: testNS, rec: rec("backend"), closed: &h.closed}
	h.p = NewProber(hostRec, func() []Leg { return h.legs }, b).(*prober)
	h.p.arp = legRec
	h.p.do = func(ns netns.NsHandle, f func() error) error {
		hostRec.record("enter %d", ns)
		defer hostRec.record("leave %d", ns)
		return f()
	}
	h.p.route = func(ns netns.NsHandle, mark int, src, dst netip.Addr) (legRoute, error) {
		hostRec.record("lookup in %d mark %d from %s to %s", ns, mark, src, dst)
		return h.route, nil
	}
	return h
}

func (h *proberHarness) take() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.calls
	h.calls = nil
	return out
}

func TestTheProberListsTheLegsBesideTheHostsInterfaces(t *testing.T) {
	host := []resolve.HostIface{
		{Name: "vmbr0", Addrs: []netip.Prefix{prefix("10.20.0.2/24")}, Segment: resolve.Segment{Bridge: "vmbr0"}, Table: resolve.FDBRef{Bridge: "vmbr0"}},
		{Name: "vmbr1", Segment: resolve.Segment{Bridge: "vmbr1"}, Table: resolve.FDBRef{Bridge: "vmbr1"}},
		{Name: "vmbr0v20", Segment: resolve.Segment{Bridge: "vmbr0", VLAN: 20}, Table: resolve.FDBRef{Bridge: "vmbr0v20"}},
		{Name: "vethpco1", Master: "vmbr1"},
		{Name: "vethpco2", Master: "vmbr0v20"},
		{Name: "vethpco3", Master: "vmbr2"},
	}
	h := newProberHarness(host, proberLegA, proberLegB, proberLegC, proberLegM)

	got, err := h.p.Interfaces(t.Context())

	require.NoError(t, err)
	require.Equal(t, append(host,
		resolve.HostIface{
			Name: "leg1", Addrs: []netip.Prefix{prefix("10.50.0.250/24")},
			Segment: resolve.Segment{Bridge: "vmbr1"}, Table: resolve.FDBRef{Bridge: "vmbr1"}, Via: "vmbr1",
		},
		resolve.HostIface{
			Name: "leg2", Addrs: []netip.Prefix{prefix("10.60.0.250/24")},
			Segment: resolve.Segment{Bridge: "vmbr0", VLAN: 20}, Table: resolve.FDBRef{Bridge: "vmbr0v20"}, Via: "vmbr0.20",
		},
		resolve.HostIface{
			Name: "leg3", Addrs: []netip.Prefix{prefix("10.70.0.250/24")},
			Segment: resolve.Segment{Bridge: "vmbr2", VLAN: 30}, Table: resolve.FDBRef{Bridge: "vmbr2", VLAN: 30}, Via: "vmbr2.30",
		},
		resolve.HostIface{
			Name: "leg4", Addrs: []netip.Prefix{prefix("10.203.118.1/24")},
			Segment: resolve.Segment{Bridge: ManagedBridge}, Table: resolve.FDBRef{Bridge: ManagedBridge}, Via: ManagedBridge,
		},
	), got)
}

// In the appliance the legs are the container's own cards, which its
// prober lists by name: each is the leg's entry once, on the leg's bridge
// and VLAN.
func TestTheAppliancesOwnLegIsReplacedNotListedTwice(t *testing.T) {
	host := []resolve.HostIface{
		{Name: "net0", Addrs: []netip.Prefix{prefix("10.20.0.150/24")}, Segment: resolve.Segment{Bridge: "vmbr0"}},
		{Name: "leg1", Addrs: []netip.Prefix{prefix("10.50.0.250/24")}, Segment: resolve.Segment{Bridge: "vmbr1"}},
		{Name: "pco0", Addrs: []netip.Prefix{prefix("198.18.0.1/32")}},
	}
	h := newProberHarness(host, proberLegA, proberLegC)

	got, err := h.p.Interfaces(t.Context())

	require.NoError(t, err)
	require.Equal(t, []resolve.HostIface{
		host[0],
		host[2],
		{
			Name: "leg1", Addrs: []netip.Prefix{prefix("10.50.0.250/24")},
			Segment: resolve.Segment{Bridge: "vmbr1"}, Table: resolve.FDBRef{Bridge: "vmbr1"}, Via: "vmbr1",
		},
		{
			Name: "leg3", Addrs: []netip.Prefix{prefix("10.70.0.250/24")},
			Segment: resolve.Segment{Bridge: "vmbr2", VLAN: 30}, Table: resolve.FDBRef{Bridge: "vmbr2", VLAN: 30}, Via: "vmbr2.30",
		},
	}, got)
}

func TestTheProberListsALegOnce(t *testing.T) {
	again := proberLegA
	again.ID = "vmbr9"
	h := newProberHarness(nil, proberLegA, again)

	got, err := h.p.Interfaces(t.Context())

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "vmbr1", got[0].Via)
}

func TestTheProberFailsWithTheHost(t *testing.T) {
	h := newProberHarness(nil, proberLegA)
	h.p.host = recordingProber{name: "host", err: errors.New("netlink is gone"), mu: &h.mu, calls: &h.calls}

	_, err := h.p.Interfaces(t.Context())

	require.EqualError(t, err, "netlink is gone")
}

// ARP, the route and the dial of a leg happen in the namespace, the way the
// leg sends; the forwarding table is the host's, where the bridges are.
func TestALegIsAskedInsideTheNamespace(t *testing.T) {
	guest := addr("10.50.0.10")
	h := newProberHarness(nil, proberLegA, proberLegB)

	t.Run("ARP", func(t *testing.T) {
		macs, err := h.p.ARP(t.Context(), "leg1", guest)

		require.NoError(t, err)
		require.Equal(t, []string{"bc:24:11:00:00:01"}, macs)
		require.Equal(t, []string{"host enter 42", "leg arp leg1 10.50.0.10", "host leave 42"}, h.take())
	})

	t.Run("the route of the leg's own table", func(t *testing.T) {
		h.route = legRoute{dev: "leg1", direct: true, table: 101}

		out, onLink, err := h.p.Route(t.Context(), "leg1", guest)

		require.NoError(t, err)
		require.Equal(t, "leg1", out)
		require.True(t, onLink)
		require.Equal(t, []string{"host lookup in 42 mark 1 from 10.50.0.250 to 10.50.0.10"}, h.take())
	})

	t.Run("a route of another table is not on the leg's link", func(t *testing.T) {
		for _, r := range []legRoute{
			{dev: "leg1", direct: true, table: 254},
			{dev: "leg1", direct: true, table: 102},
			{dev: "leg1", table: 101},
		} {
			h.route = r

			out, onLink, err := h.p.Route(t.Context(), "leg1", guest)

			require.NoError(t, err)
			require.Equal(t, "leg1", out)
			require.False(t, onLink, "%+v", r)
		}
		h.take()
	})

	t.Run("dial", func(t *testing.T) {
		require.NoError(t, h.p.Dial(t.Context(), "leg2", netip.AddrPortFrom(addr("10.60.0.10"), 80)))

		require.Equal(t, []string{"backend dial leg2 mark 2 from 10.60.0.250 to 10.60.0.10:80"}, h.take())
		require.True(t, h.closed, "the connection is closed")
	})

	t.Run("a dial that fails", func(t *testing.T) {
		h.p.b = dialBackend{ns: testNS, rec: recordingProber{name: "backend", mu: &h.mu, calls: &h.calls}, closed: &h.closed, dialErr: errors.New("connection refused")}

		require.EqualError(t, h.p.Dial(t.Context(), "leg2", netip.AddrPortFrom(addr("10.60.0.10"), 80)), "connection refused")
		h.take()
	})

	t.Run("the forwarding table", func(t *testing.T) {
		ports, err := h.p.FDBPorts(t.Context(), "vmbr1", 0, "bc:24:11:00:00:01")

		require.NoError(t, err)
		require.Equal(t, []string{"tap101i0"}, ports)
		require.Equal(t, []string{"host fdb vmbr1 0 bc:24:11:00:00:01"}, h.take())
	})
}

func TestAnInterfaceThatIsNoLegIsTheHosts(t *testing.T) {
	h := newProberHarness(nil, proberLegA)
	target := netip.AddrPortFrom(addr("10.20.0.10"), 80)

	macs, err := h.p.ARP(t.Context(), "vmbr0", target.Addr())
	require.NoError(t, err)
	require.Equal(t, []string{"bc:24:11:00:00:09"}, macs)
	_, _, err = h.p.Route(t.Context(), "vmbr0", target.Addr())
	require.NoError(t, err)
	_, _, err = h.p.Route(t.Context(), "", target.Addr())
	require.NoError(t, err)
	require.NoError(t, h.p.Dial(t.Context(), "vmbr0", target))
	require.NoError(t, h.p.Dial(t.Context(), "", target))

	require.Equal(t, []string{
		"host arp vmbr0 10.20.0.10",
		"host route vmbr0 10.20.0.10",
		"host route  10.20.0.10",
		"host dial vmbr0 10.20.0.10:80",
		"host dial  10.20.0.10:80",
	}, h.take())

	// A leg that is gone is no leg any more.
	h.legs = nil
	_, err = h.p.ARP(t.Context(), "leg1", target.Addr())
	require.NoError(t, err)
	require.Equal(t, []string{"host arp leg1 10.20.0.10"}, h.take())
}

func TestARouteOfALegIsLookedUpNoLongerOnceCancelled(t *testing.T) {
	h := newProberHarness(nil, proberLegA)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err := h.p.Route(ctx, "leg1", addr("10.50.0.10"))

	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, h.take())
}
