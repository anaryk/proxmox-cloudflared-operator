package egress

import (
	"context"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
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
}

func newWatchLab(t *testing.T) *watchLab {
	t.Helper()
	if os.Geteuid() != 0 {
		requireLab(t, "needs root to create network namespaces")
	}
	lab := &watchLab{node: newNamespace(t), guest: newNamespace(t), other: newNamespace(t), moves: make(chan netip.Addr, 64)}
	lab.h = linkHandle(t, lab.node)
	require.NoError(t, lab.h.LinkAdd(&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: watchBridge}}))
	setUp(t, lab.h, watchBridge, watchNode+"/24")
	for port, ns := range map[string]netns.NsHandle{"wport0": lab.guest, "wport1": lab.other} {
		require.NoError(t, lab.h.LinkAdd(&netlink.Veth{
			LinkAttrs: netlink.LinkAttrs{Name: port}, PeerName: "eth0", PeerNamespace: netlink.NsFd(ns),
		}))
		link, err := lab.h.LinkByName(port)
		require.NoError(t, err)
		br, err := lab.h.LinkByName(watchBridge)
		require.NoError(t, err)
		require.NoError(t, lab.h.LinkSetMaster(link, br))
		require.NoError(t, lab.h.LinkSetUp(link))
	}
	lab.machine(t, lab.guest, watchMAC)
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

func (l *watchLab) moved(t *testing.T, within time.Duration) netip.Addr {
	t.Helper()
	select {
	case a := <-l.moves:
		return a
	case <-time.After(within):
		t.Fatalf("no move within %s", within)
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

		require.Equal(t, addr(watchGuest), lab.moved(t, watchWithin))
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

		require.Equal(t, addr(watchGuest), lab.moved(t, watchWithin))
		t.Logf("seen after %s", time.Since(start))
	})
}

// A watch that starts after the MAC moved sees it in what the tables hold.
func TestLinuxWatchSeesWhatMovedBeforeItStarted(t *testing.T) {
	lab := newWatchLab(t)
	lab.machine(t, lab.other, watchMAC)
	lab.speak(t, lab.other)
	lab.learned(t, watchMAC, "wport1")

	lab.watch(t, map[netip.Addr]Pin{addr(watchGuest): {MAC: mac(t, watchMAC), Bridge: watchBridge, Port: "wport0"}})

	require.Equal(t, addr(watchGuest), lab.moved(t, watchWithin))
}
