package egress

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// The watch test builds three network namespaces of its own: a node with a
// bridge, a guest on one port of it and a second machine on another port.
// Nothing of the host it runs on is touched.
const (
	watchBridge  = "wbr0"
	watchGuest   = "10.79.0.2"
	watchNode    = "10.79.0.1"
	watchMAC     = "bc:24:11:79:00:02"
	watchOther   = "bc:24:11:79:00:03"
	watchWithin  = time.Second
	watchQuietly = 300 * time.Millisecond
)

type watchLab struct {
	node, guest, other netns.NsHandle
	h                  *netlink.Handle // of the node
	moves              chan netip.Addr
	ports              map[string]netns.NsHandle // the machine on each port of the bridge
	barriers           int
}

func newWatchLab(t *testing.T) *watchLab {
	t.Helper()
	lab := newBridgeLab(t, watchBridge, "wport0", "wport1")
	lab.guest, lab.other = lab.ports["wport0"], lab.ports["wport1"]
	lab.moves = make(chan netip.Addr, 64)
	lab.machine(t, lab.guest, watchMAC)
	return lab
}

// newBridgeLab builds a node with a bridge and a machine on each of the ports
// it names, in namespaces of their own.
func newBridgeLab(t *testing.T, bridge string, ports ...string) *watchLab {
	t.Helper()
	if os.Geteuid() != 0 {
		requireLab(t, "needs root to create network namespaces")
	}
	lab := &watchLab{node: newNamespace(t), ports: map[string]netns.NsHandle{}}
	lab.h = linkHandle(t, lab.node)
	require.NoError(t, lab.h.LinkAdd(&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: bridge}}))
	setUp(t, lab.h, bridge, watchNode+"/24")
	for _, port := range ports {
		ns := newNamespace(t)
		lab.ports[port] = ns
		require.NoError(t, lab.h.LinkAdd(&netlink.Veth{
			LinkAttrs: netlink.LinkAttrs{Name: port}, PeerName: "eth0", PeerNamespace: netlink.NsFd(ns),
		}))
		link, err := lab.h.LinkByName(port)
		require.NoError(t, err)
		br, err := lab.h.LinkByName(bridge)
		require.NoError(t, err)
		require.NoError(t, lab.h.LinkSetMaster(link, br))
		require.NoError(t, lab.h.LinkSetUp(link))
	}
	return lab
}

// machine gives the end of a port in ns its MAC and the guest's address.
func (l *watchLab) machine(t *testing.T, ns netns.NsHandle, hw string) {
	t.Helper()
	h := linkHandle(t, ns)
	link, err := h.LinkByName("eth0")
	require.NoError(t, err)
	require.NoError(t, h.LinkSetDown(link))
	require.NoError(t, h.LinkSetHardwareAddr(link, mac(t, hw)))
	addrs, err := h.AddrList(link, netlink.FAMILY_V4)
	require.NoError(t, err)
	if len(addrs) == 0 {
		setUp(t, h, "eth0", watchGuest+"/24")
	}
	require.NoError(t, h.LinkSetUp(link))
}

// speak makes the machine in ns send a frame to the node, which makes the
// bridge learn its MAC on its port.
func (l *watchLab) speak(t *testing.T, ns netns.NsHandle) {
	t.Helper()
	var err error
	onThrowawayThread(func() {
		if err = netns.Set(ns); err != nil {
			return
		}
		var c net.Conn
		if c, err = net.Dial("udp4", net.JoinHostPort(watchNode, "9")); err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, err = c.Write([]byte("x"))
	})
	require.NoError(t, err)
}

// learned waits until the bridge has learned mac on port.
func (l *watchLab) learned(t *testing.T, hw, port string) {
	t.Helper()
	link, err := l.h.LinkByName(port)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		entries, err := l.h.NeighList(link.Attrs().Index, unix.AF_BRIDGE)
		if err != nil {
			return false
		}
		for _, e := range entries {
			if e.HardwareAddr.String() == hw && e.MasterIndex != 0 {
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "%s is not learned on %s", hw, port)
}

func (l *watchLab) watch(t *testing.T, pins map[netip.Addr]Pin) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- watchIn(ctx, l.node, func() map[netip.Addr]Pin { return pins }, func(a netip.Addr) { l.moves <- a })
	}()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
}

func (l *watchLab) moved(t *testing.T) netip.Addr {
	t.Helper()
	select {
	case a := <-l.moves:
		return a
	case <-time.After(watchWithin):
		t.Fatalf("no move within %s", watchWithin)
	}
	return netip.Addr{}
}

func (l *watchLab) still(t *testing.T) {
	t.Helper()
	select {
	case a := <-l.moves:
		t.Fatalf("%s moved", a)
	case <-time.After(watchQuietly):
	}
}

func TestLinuxWatch(t *testing.T) {
	lab := newWatchLab(t)
	lab.speak(t, lab.guest)
	lab.learned(t, watchMAC, "wport0")
	pins := map[netip.Addr]Pin{addr(watchGuest): {MAC: mac(t, watchMAC), Bridge: watchBridge, Port: "wport0"}}
	lab.watch(t, pins)
	lab.still(t)

	t.Run("the guest that speaks again moves nothing", func(t *testing.T) {
		lab.speak(t, lab.guest)
		lab.still(t)
	})

	t.Run("the neighbour table gives the address another MAC", func(t *testing.T) {
		br, err := lab.h.LinkByName(watchBridge)
		require.NoError(t, err)
		start := time.Now()
		require.NoError(t, lab.h.NeighSet(&netlink.Neigh{
			LinkIndex: br.Attrs().Index, Family: netlink.FAMILY_V4, State: netlink.NUD_REACHABLE,
			IP: net.ParseIP(watchGuest), HardwareAddr: mac(t, watchOther),
		}))

		require.Equal(t, addr(watchGuest), lab.moved(t))
		t.Logf("seen after %s", time.Since(start))
		require.NoError(t, lab.h.NeighSet(&netlink.Neigh{
			LinkIndex: br.Attrs().Index, Family: netlink.FAMILY_V4, State: netlink.NUD_REACHABLE,
			IP: net.ParseIP(watchGuest), HardwareAddr: mac(t, watchMAC),
		}))
		lab.still(t)
	})

	t.Run("a second machine with the same MAC on another port", func(t *testing.T) {
		lab.machine(t, lab.other, watchMAC)
		start := time.Now()
		lab.speak(t, lab.other)

		require.Equal(t, addr(watchGuest), lab.moved(t))
		t.Logf("seen after %s", time.Since(start))
	})
}

// A machine on another port takes the guest's other MAC and the address: the
// neighbour table may give the address that MAC, which the guest has too, but
// the bridge learns it on a port the MAC does not belong on.
func TestLinuxWatchSeesAStrangerWithTheGuestsOtherMAC(t *testing.T) {
	const otherMAC = "bc:24:11:79:00:09"
	lab := newWatchLab(t)
	lab.speak(t, lab.guest)
	lab.learned(t, watchMAC, "wport0")
	lab.watch(t, map[netip.Addr]Pin{addr(watchGuest): {
		MAC: mac(t, watchMAC), Bridge: watchBridge, Port: "wport0",
		Own: []OwnMAC{{MAC: mac(t, otherMAC), Ports: []string{"wport0"}}},
	}})
	lab.still(t)

	lab.machine(t, lab.other, otherMAC)
	start := time.Now()
	lab.speak(t, lab.other)

	require.Equal(t, addr(watchGuest), lab.moved(t))
	t.Logf("seen after %s", time.Since(start))
	br, err := lab.h.LinkByName(watchBridge)
	require.NoError(t, err)
	require.NoError(t, lab.h.NeighSet(&netlink.Neigh{
		LinkIndex: br.Attrs().Index, Family: netlink.FAMILY_V4, State: netlink.NUD_REACHABLE,
		IP: net.ParseIP(watchGuest), HardwareAddr: mac(t, otherMAC),
	}))
	lab.still(t)
}

// A watch that starts after the MAC moved sees it in what the tables hold.
func TestLinuxWatchSeesWhatMovedBeforeItStarted(t *testing.T) {
	lab := newWatchLab(t)
	lab.machine(t, lab.other, watchMAC)
	lab.speak(t, lab.other)
	lab.learned(t, watchMAC, "wport1")

	lab.watch(t, map[netip.Addr]Pin{addr(watchGuest): {MAC: mac(t, watchMAC), Bridge: watchBridge, Port: "wport0"}})

	require.Equal(t, addr(watchGuest), lab.moved(t))
}

// Every entry says where it is: a neighbour the device it is on, which a pin
// on a leg is matched by, and a learned MAC its bridge and port.
func TestAnEntrySaysItsDevice(t *testing.T) {
	names := map[int]string{3: "vmbr0", 7: "leg1", 8: "tap101i0"}
	name := func(index int) string { return names[index] }
	hw := mac(t, "bc:24:11:00:00:01")
	neigh := func(index int, ip string) netlink.Neigh {
		return netlink.Neigh{Family: unix.AF_INET, LinkIndex: index, IP: net.ParseIP(ip), HardwareAddr: hw}
	}
	tests := []struct {
		name   string
		update netlink.NeighUpdate
		want   entry
		ok     bool
	}{
		{name: "a neighbour on a bridge", update: netlink.NeighUpdate{Type: unix.RTM_NEWNEIGH, Neigh: neigh(3, "10.20.0.10")},
			want: entry{addr: addr("10.20.0.10"), dev: "vmbr0", mac: hw}, ok: true},
		{name: "a neighbour on a leg", update: netlink.NeighUpdate{Type: unix.RTM_NEWNEIGH, Neigh: neigh(7, "10.50.0.10")},
			want: entry{addr: addr("10.50.0.10"), dev: "leg1", mac: hw}, ok: true},
		{name: "a neighbour on a device whose name is not known", update: netlink.NeighUpdate{Type: unix.RTM_NEWNEIGH, Neigh: neigh(9, "10.50.0.10")},
			want: entry{addr: addr("10.50.0.10"), mac: hw}, ok: true},
		{name: "a MAC a bridge learned", update: netlink.NeighUpdate{Type: unix.RTM_NEWNEIGH, Neigh: netlink.Neigh{
			Family: unix.AF_BRIDGE, MasterIndex: 3, LinkIndex: 8, HardwareAddr: hw,
		}}, want: entry{bridge: "vmbr0", port: "tap101i0", mac: hw}, ok: true},
		{name: "a neighbour that went", update: netlink.NeighUpdate{Type: unix.RTM_DELNEIGH, Neigh: neigh(3, "10.20.0.10")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := entryOf(tc.update, name)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

// The observer's lab is a node whose bridge has the ports of three guests and
// an uplink, with a machine on each.
const (
	observeBridge = "obr0"
	observeWeb    = "bc:24:11:79:01:01"
	observeDB     = "bc:24:11:79:01:02"
	observeUnseen = "bc:24:11:79:01:03"
	observeOther  = "bc:24:11:79:01:ff"
)

// observeExpecting is what the observer expects of the lab: the first two
// guests' MACs, and the MACs barrier uses.
func observeExpecting() map[string]Expected {
	expected := map[string]Expected{
		observeWeb: {MAC: observeWeb, Guest: web, NIC: 0},
		observeDB:  {MAC: observeDB, Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 102}, NIC: 0},
	}
	for n := range 10 {
		hw := fmt.Sprintf("bc:24:11:79:02:%02x", n)
		expected[hw] = Expected{MAC: hw, Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 104}, NIC: n}
	}
	return expected
}

func newObserveLab(t *testing.T) *watchLab {
	t.Helper()
	lab := newBridgeLab(t, observeBridge, "tap101i0", "tap102i0", "tap109i0", "up0")
	for _, ns := range lab.ports {
		silenceIPv6(t, ns)
	}
	lab.machine(t, lab.ports["tap101i0"], observeWeb)
	lab.machine(t, lab.ports["tap102i0"], observeDB)
	lab.machine(t, lab.ports["tap109i0"], observeOther)
	lab.machine(t, lab.ports["up0"], observeOther)
	return lab
}

// silenceIPv6 stops the machine in ns from speaking IPv6. Its duplicate
// address detection and announcements are frames from its MAC that the test
// did not send, and they move a MAC from port to port when two machines have
// it.
func silenceIPv6(t *testing.T, ns netns.NsHandle) {
	t.Helper()
	var err error
	onThrowawayThread(func() {
		if err = netns.Set(ns); err == nil {
			err = os.WriteFile("/proc/sys/net/ipv6/conf/eth0/disable_ipv6", []byte("1"), 0o644)
		}
	})
	require.NoError(t, err)
}

// observe starts observing the node, until the test ends.
func (l *watchLab) observe(t *testing.T, expected map[string]Expected) <-chan Observation {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	seen := make(chan Observation, 256)
	done := make(chan error, 1)
	go func() {
		done <- observeIn(ctx, l.node, func() map[string]Expected { return expected }, func(o Observation) { seen <- o })
	}()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
	return seen
}

// next waits for the next observation of kind about hw, and passes by what
// else the bridge shows meanwhile.
func next(t *testing.T, seen <-chan Observation, kind ObservationKind, hw string) Observation {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case o := <-seen:
			if o.Kind == kind && o.MAC == hw {
				return o
			}
			t.Logf("passing by %s of %s on %s", o.Kind, o.MAC, o.Port)
		case <-timeout:
			t.Fatalf("no %s of %s within 5s", kind, hw)
		}
	}
}

// barrier makes the bridge show a MAC of its own that the observer expects on
// a port of another guest's, and waits for the observation of it. The
// observer reads the notifications in the order they came, so whatever the
// bridge showed before has been observed by then; barrier returns it.
func (l *watchLab) barrier(t *testing.T, seen <-chan Observation) []Observation {
	t.Helper()
	l.barriers++
	hw := fmt.Sprintf("bc:24:11:79:02:%02x", l.barriers)
	l.machine(t, l.ports["tap109i0"], hw)
	l.speak(t, l.ports["tap109i0"])
	var before []Observation
	timeout := time.After(5 * time.Second)
	for {
		select {
		case o := <-seen:
			if o.MAC == hw {
				return before
			}
			before = append(before, o)
		case <-timeout:
			t.Fatalf("the barrier %s was not observed within 5s", hw)
		}
	}
}

// What a watch learned of the tables before it started is in the dump, and a
// watch that does not ask for it sees nothing of what moved while it was not
// running.
func TestEveryWatchStartsFromTheDump(t *testing.T) {
	require.True(t, subscription(netns.None()).ListExisting)
}

// A subscription that failed after it began is made again, and with it the
// dump.
func TestAFailedSubscriptionIsMadeAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	starts := 0

	err := follow(ctx, netns.None(), "watching", 0, func(*netlink.Handle) error {
		starts++
		if starts == 2 {
			cancel()
			return nil
		}
		return errUnsubscribed
	})

	require.NoError(t, err)
	require.Equal(t, 2, starts)
}

func TestASubscriptionThatCannotBeMadeIsAnError(t *testing.T) {
	err := follow(context.Background(), netns.None(), "watching the forwarding tables", 0, func(*netlink.Handle) error {
		return unix.EPERM
	})

	require.ErrorIs(t, err, unix.EPERM)
	require.ErrorContains(t, err, "watching the forwarding tables")
}

// A MAC that is where it is when the observer starts is reported from the
// dump, the one on its own port as learned and the one that an impostor has
// taken as a conflict, which is how an agent that was down finds out.
func TestLinuxObserveBeginsWithTheDump(t *testing.T) {
	lab := newObserveLab(t)
	lab.speak(t, lab.ports["tap101i0"])
	lab.learned(t, observeWeb, "tap101i0")
	lab.machine(t, lab.ports["tap102i0"], observeUnseen)
	lab.speak(t, lab.ports["tap102i0"])
	lab.learned(t, observeUnseen, "tap102i0")
	expected := map[string]Expected{
		observeWeb:    {MAC: observeWeb, Guest: web, NIC: 0},
		observeUnseen: {MAC: observeUnseen, Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 103}, NIC: 0},
	}

	seen := lab.observe(t, expected)

	first := next(t, seen, Reset, "")
	require.NotZero(t, first.At, "the subscription begins with a reset, then the dump")
	learned := next(t, seen, Learned, observeWeb)
	require.Equal(t, observeBridge, learned.Bridge)
	require.Equal(t, "tap101i0", learned.Port)
	require.NotZero(t, learned.At)
	conflict := next(t, seen, Conflict, observeUnseen)
	require.Equal(t, "tap102i0", conflict.Port)
	require.Equal(t, &model.GuestRef{Kind: model.KindQEMU, VMID: 102}, conflict.Owner)
}

func TestLinuxObserveFollowsAMACFromPortToPort(t *testing.T) {
	lab := newObserveLab(t)
	onWeb, onOther, onUplink := lab.ports["tap101i0"], lab.ports["tap102i0"], lab.ports["up0"]
	lab.speak(t, onWeb)
	lab.learned(t, observeWeb, "tap101i0")
	seen := lab.observe(t, observeExpecting())
	next(t, seen, Learned, observeWeb)

	t.Run("a machine on the uplink takes the MAC", func(t *testing.T) {
		lab.machine(t, onUplink, observeWeb)
		lab.speak(t, onUplink)

		moved := next(t, seen, Moved, observeWeb)
		require.Equal(t, "up0", moved.Port)
		require.Equal(t, "tap101i0", moved.From)
	})

	t.Run("the guest speaks again", func(t *testing.T) {
		lab.speak(t, onWeb)

		learned := next(t, seen, Learned, observeWeb)
		require.Equal(t, "tap101i0", learned.Port)
	})

	t.Run("a machine on the port of another guest takes it", func(t *testing.T) {
		lab.machine(t, onOther, observeWeb)
		lab.speak(t, onOther)

		conflict := next(t, seen, Conflict, observeWeb)
		require.Equal(t, "tap102i0", conflict.Port)
		require.Equal(t, "tap101i0", conflict.From)
		require.Equal(t, &model.GuestRef{Kind: model.KindQEMU, VMID: 102}, conflict.Owner)
	})
}

func TestLinuxObserveReportsNothingOfAMACItDoesNotExpect(t *testing.T) {
	lab := newObserveLab(t)
	seen := lab.observe(t, observeExpecting())
	for _, port := range []string{"tap101i0", "tap102i0", "up0"} {
		lab.machine(t, lab.ports[port], observeUnseen)
		lab.speak(t, lab.ports[port])
		lab.learned(t, observeUnseen, port)
	}

	for _, o := range lab.barrier(t, seen) {
		require.NotEqual(t, observeUnseen, o.MAC, "%+v", o)
	}
}

// A guest that stops loses its port, and the bridge its entries, which ends
// what was learned of it: the MAC that turns up on the uplink afterwards, as
// the guest starts on another node, is no move.
func TestLinuxObserveSeesAGuestThatStoppedGo(t *testing.T) {
	lab := newObserveLab(t)
	lab.speak(t, lab.ports["tap101i0"])
	lab.learned(t, observeWeb, "tap101i0")
	seen := lab.observe(t, observeExpecting())
	next(t, seen, Learned, observeWeb)

	port, err := lab.h.LinkByName("tap101i0")
	require.NoError(t, err)
	require.NoError(t, lab.h.LinkDel(port))

	gone := next(t, seen, Gone, observeWeb)
	require.Equal(t, observeBridge, gone.Bridge)
	require.Equal(t, "tap101i0", gone.Port)
	require.NotZero(t, gone.At)

	lab.machine(t, lab.ports["up0"], observeWeb)
	lab.speak(t, lab.ports["up0"])
	lab.learned(t, observeWeb, "up0")

	for _, o := range lab.barrier(t, seen) {
		require.NotEqual(t, observeWeb, o.MAC, "%+v", o)
	}
}
