//go:build linux

package gateway

import (
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// sendFrom sends one broadcast ARP frame from eth0 of a namespace, with src
// as its source whatever eth0's MAC is.
func sendFrom(t *testing.T, ns netns.NsHandle, src net.HardwareAddr) {
	t.Helper()
	sendFrame(t, ns, broadcastARP(src))
}

func broadcastARP(src net.HardwareAddr) []byte {
	f := arpProbeFrame(src, netip.MustParseAddr("10.203.118.250"))
	copy(f[6:12], src)
	return f
}

// sendFrame sends a frame from eth0 of a namespace.
func sendFrame(t *testing.T, ns netns.NsHandle, f []byte) {
	t.Helper()
	s, err := Socket(ns, unix.AF_PACKET, unix.SOCK_RAW, HostToNet16(unix.ETH_P_ALL), SocketOptions{Device: "eth0"})
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	_, err = s.Write(f)
	require.NoError(t, err)
}

// entryPort returns the port of the bridge whose forwarding entries hold
// mac, and whether that entry is static and sticky.
func (l *labNet) entryPort(t *testing.T, ports []string, mac net.HardwareAddr) (string, bool) {
	t.Helper()
	for _, p := range ports {
		have, err := l.sys.fdb(p)
		require.NoError(t, err)
		if i := slices.IndexFunc(have, func(e fdbEntry) bool { return macEqual(e.mac, mac) }); i >= 0 {
			return p, have[i].static && have[i].sticky
		}
	}
	return "", false
}

// A guest that sends with another guest's MAC moves the bridge's learned
// entry to its own port, and with it the other's traffic; the sticky entry
// stays where pco put it.
func TestLinuxAForgedFrameDoesNotMoveTheStickyEntry(t *testing.T) {
	l := newLabNet(t)
	l.bridge(ManagedBridge, false)
	a := l.guest("a", ManagedBridge, 0, "10.203.118.2/24", "")
	b := l.guest("b", ManagedBridge, 0, "10.203.118.3/24", "")
	macA, err := macIn(a, "eth0")
	require.NoError(t, err)
	ports := []string{"ga", "gb"}

	// Frames are sent until the bridge learned from them: a port that has
	// just come up may drop the first.
	require.Eventually(t, func() bool {
		sendFrom(t, a, macA)
		p, _ := l.entryPort(t, ports, macA)
		return p == "ga"
	}, 2*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool {
		sendFrom(t, b, macA)
		p, _ := l.entryPort(t, ports, macA)
		return p == "gb"
	}, 2*time.Second, 20*time.Millisecond, "without the entry a forged frame takes the MAC over")

	require.NoError(t, ApplySticky(t.Context(), l.b, []Sticky{{MAC: macA, Bridge: ManagedBridge, Port: "ga"}}))
	p, sticky := l.entryPort(t, ports, macA)
	require.Equal(t, "ga", p)
	require.True(t, sticky)

	for range 5 {
		sendFrom(t, b, macA)
	}
	require.Never(t, func() bool {
		p, sticky := l.entryPort(t, ports, macA)
		return p != "ga" || !sticky
	}, 300*time.Millisecond, 10*time.Millisecond, "the forged frames moved the entry")
}

// The port watch names a guest's port once it is enslaved to the managed
// bridge, those there when it starts too, and no other link.
func TestLinuxThePortWatchNamesTheGuestPortsOfTheBridge(t *testing.T) {
	l := newLabNet(t)
	l.bridge(ManagedBridge, false)
	l.bridge("vmbr1", false)
	l.ip("-n", l.hostName, "link", "add", "veth9301i1", "type", "veth", "peer", "name", "p9301")
	l.ip("-n", l.hostName, "link", "set", "veth9301i1", "master", ManagedBridge)

	var mu sync.Mutex
	var seen []string
	saw := func(port string) bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(seen, port)
	}
	done := make(chan error, 1)
	go func() {
		done <- WatchGuestPorts(t.Context(), l.host, ManagedBridge, func(port string) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, port)
		}, nil)
	}()
	require.Eventually(t, func() bool { return saw("veth9301i1") }, 2*time.Second, 10*time.Millisecond, "a port there at the start")

	l.ip("-n", l.hostName, "tuntap", "add", "dev", "tap9302i1", "mode", "tap")
	require.Never(t, func() bool { return saw("tap9302i1") }, 200*time.Millisecond, 10*time.Millisecond, "not enslaved yet")
	start := time.Now()
	l.ip("-n", l.hostName, "link", "set", "tap9302i1", "master", ManagedBridge)
	require.Eventually(t, func() bool { return saw("tap9302i1") }, 2*time.Second, time.Millisecond)
	t.Logf("the port was named %s after it joined", time.Since(start))

	l.ip("-n", l.hostName, "tuntap", "add", "dev", "tap9303i1", "mode", "tap")
	l.ip("-n", l.hostName, "link", "set", "tap9303i1", "master", "vmbr1")
	l.ip("-n", l.hostName, "link", "add", "pcotestx", "type", "veth", "peer", "name", "pcotesty")
	l.ip("-n", l.hostName, "link", "set", "pcotestx", "master", ManagedBridge)
	require.Never(t, func() bool { return saw("tap9303i1") || saw("pcotestx") }, 300*time.Millisecond, 10*time.Millisecond,
		"a port of another bridge, or no guest's")

	select {
	case err := <-done:
		t.Fatalf("the watch ended: %v", err)
	default:
	}
}
