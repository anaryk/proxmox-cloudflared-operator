package resolve

import (
	"context"
	"net"
	"net/netip"
	"os"
	"runtime"
	"testing"

	"github.com/mdlayher/arp"
	"github.com/mdlayher/ethernet"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const (
	labBridge = "pcotest0"
	labPort1  = "tap901i0"
	labPort2  = "tap902i0"
)

func TestLinuxFDBPorts(t *testing.T) {
	const bridge, tap1, tap2, otherBridge = 10, 11, 12, 20
	mac := mustMAC(t, "bc:24:11:00:00:01")
	entry := func(port, master, vlan, state int) netlink.Neigh {
		return netlink.Neigh{
			LinkIndex:    port,
			MasterIndex:  master,
			Family:       unix.AF_BRIDGE,
			Flags:        netlink.NTF_MASTER,
			State:        state,
			HardwareAddr: mac,
			Vlan:         vlan,
		}
	}
	learned := func(port, vlan int) netlink.Neigh { return entry(port, bridge, vlan, netlink.NUD_REACHABLE) }
	otherMAC := learned(tap2, 0)
	otherMAC.HardwareAddr = mustMAC(t, "bc:24:11:00:00:02")
	ownEntry := entry(tap1, 0, 0, netlink.NUD_PERMANENT)
	ownEntry.Flags = netlink.NTF_SELF

	tests := []struct {
		name    string
		entries []netlink.Neigh
		vlan    int
		want    []int
	}{
		{"learned on a port", []netlink.Neigh{learned(tap1, 0)}, 0, []int{tap1}},
		{"stale entry", []netlink.Neigh{entry(tap1, bridge, 0, netlink.NUD_STALE)}, 0, []int{tap1}},
		{"static entry", []netlink.Neigh{entry(tap1, bridge, 0, netlink.NUD_NOARP)}, 0, []int{tap1}},
		{"no entry", nil, 0, nil},
		{"other MAC", []netlink.Neigh{otherMAC}, 0, nil},
		{"other bridge", []netlink.Neigh{entry(tap1, otherBridge, 0, netlink.NUD_REACHABLE)}, 0, nil},
		{"entry on the bridge itself", []netlink.Neigh{learned(bridge, 0)}, 0, nil},
		{"permanent entry of a port", []netlink.Neigh{entry(tap1, bridge, 0, netlink.NUD_PERMANENT)}, 0, nil},
		{"entry of the port device itself", []netlink.Neigh{ownEntry}, 0, nil},
		{"untagged matches PVID 1", []netlink.Neigh{learned(tap1, 1)}, 0, []int{tap1}},
		{"untagged ignores other VLANs", []netlink.Neigh{learned(tap1, 10)}, 0, nil},
		{"VLAN matches only itself", []netlink.Neigh{learned(tap1, 0), learned(tap1, 1), learned(tap2, 10)}, 10, []int{tap2}},
		{"same port twice", []netlink.Neigh{learned(tap1, 0), learned(tap1, 1)}, 0, []int{tap1}},
		{"two ports", []netlink.Neigh{learned(tap1, 0), learned(tap2, 1)}, 0, []int{tap1, tap2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, fdbPorts(tc.entries, bridge, tc.vlan, mac))
		})
	}
}

func TestLinuxAppendClaimants(t *testing.T) {
	addr := netip.MustParseAddr("10.20.0.5")
	other := netip.MustParseAddr("10.20.0.6")
	host := netip.MustParseAddr("10.20.0.1")
	guest := mustMAC(t, "bc:24:11:00:00:01")
	stranger := mustMAC(t, "bc:24:11:00:00:02")
	hostMAC := mustMAC(t, "bc:24:11:00:00:fe")

	reply := arpFrame(t, arp.OperationReply, guest, guest, addr, hostMAC, host)
	tests := []struct {
		name   string
		macs   []string
		frame  []byte
		expect []string
	}{
		{"reply from addr", nil, reply, []string{"bc:24:11:00:00:01"}},
		{"repeated reply", []string{"bc:24:11:00:00:01"}, reply, []string{"bc:24:11:00:00:01"}},
		{"second replier", []string{"bc:24:11:00:00:02"}, reply, []string{"bc:24:11:00:00:02", "bc:24:11:00:00:01"}},
		{"request", nil, arpFrame(t, arp.OperationRequest, guest, guest, addr, ethernet.Broadcast, host), nil},
		{"reply about another address", nil, arpFrame(t, arp.OperationReply, guest, guest, other, hostMAC, host), nil},
		{"frame source differs", nil, arpFrame(t, arp.OperationReply, stranger, guest, addr, hostMAC, host), []string{"bc:24:11:00:00:01", "bc:24:11:00:00:02"}},
		{"truncated frame", nil, reply[:20], nil},
		{"not ARP", nil, ipv4Frame(t, guest, hostMAC), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expect, appendClaimants(tc.macs, tc.frame, addr))
		})
	}
}

func TestLinuxSourceAddr(t *testing.T) {
	addrs := []net.Addr{
		mustIPNet(t, "fe80::1/64"),
		mustIPNet(t, "192.168.10.2/24"),
		mustIPNet(t, "10.20.0.1/24"),
		mustIPNet(t, "10.20.0.2/24"),
		mustIPNet(t, "::ffff:10.30.0.1/120"),
	}
	src, err := sourceAddr(addrs, netip.MustParseAddr("10.20.0.5"))
	require.NoError(t, err)
	require.Equal(t, netip.MustParseAddr("10.20.0.1"), src)

	_, err = sourceAddr(addrs, netip.MustParseAddr("10.30.0.5"))
	require.ErrorContains(t, err, "no IPv4 address in the network of 10.30.0.5")
}

// TestLinuxProberOnBridge builds a bridge with two guests in network
// namespaces and probes them for real.
func TestLinuxProberOnBridge(t *testing.T) {
	lab := newBridgeLab(t)
	p := NewHostProber(0, 0)
	guest := netip.MustParseAddr("10.99.0.11")

	t.Run("interfaces", func(t *testing.T) {
		ifaces, err := p.Interfaces(t.Context())
		require.NoError(t, err)
		require.Contains(t, ifaces, HostIface{Name: labBridge, Addrs: []netip.Prefix{netip.MustParsePrefix("10.99.0.1/24")}})
		require.Contains(t, ifaces, HostIface{Name: labPort1, Master: labBridge})
	})

	t.Run("arp answered by one guest", func(t *testing.T) {
		macs, err := p.ARP(t.Context(), labBridge, guest)
		require.NoError(t, err)
		require.Equal(t, []string{lab.guestMAC[0]}, macs)
	})

	t.Run("arp without answer", func(t *testing.T) {
		macs, err := p.ARP(t.Context(), labBridge, netip.MustParseAddr("10.99.0.99"))
		require.NoError(t, err)
		require.Empty(t, macs)
	})

	t.Run("arp outside the networks of the interface", func(t *testing.T) {
		_, err := p.ARP(t.Context(), labBridge, netip.MustParseAddr("10.98.0.11"))
		require.ErrorContains(t, err, "no IPv4 address in the network of 10.98.0.11")
	})

	t.Run("arp with cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := p.ARP(ctx, labBridge, guest)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("forwarding table", func(t *testing.T) {
		port, found, err := p.FDBPort(t.Context(), labBridge, 0, lab.guestMAC[0])
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, labPort1, port)

		for _, own := range []string{lab.bridgeMAC, lab.portMAC[0], "bc:24:11:99:99:99"} {
			_, found, err = p.FDBPort(t.Context(), labBridge, 0, own)
			require.NoError(t, err, own)
			require.False(t, found, own)
		}
	})

	t.Run("forwarding table with a MAC on two ports", func(t *testing.T) {
		mac := "bc:24:11:99:99:98"
		lab.staticFDB(t, labPort1, mac, 0)
		lab.staticFDB(t, labPort2, mac, 1)
		_, _, err := p.FDBPort(t.Context(), labBridge, 0, mac)
		require.ErrorContains(t, err, "MAC bc:24:11:99:99:98 is learned on ports tap901i0 and tap902i0")
	})

	t.Run("dial", func(t *testing.T) {
		ln := listenIn(t, lab.ns[0], "10.99.0.11:0")
		port := uint16(ln.Addr().(*net.TCPAddr).Port)
		require.NoError(t, p.Dial(t.Context(), netip.AddrPortFrom(guest, port)))
		require.Error(t, p.Dial(t.Context(), netip.AddrPortFrom(guest, 1)))
	})

	t.Run("arp answered by two guests", func(t *testing.T) {
		lab.addAddr(t, 1, "10.99.0.11/24")
		macs, err := p.ARP(t.Context(), labBridge, guest)
		require.NoError(t, err)
		require.ElementsMatch(t, lab.guestMAC[:], macs)
	})
}

// bridgeLab is the bridge pcotest0 with one guest namespace on each of the
// ports tap901i0 and tap902i0.
type bridgeLab struct {
	ns        [2]netns.NsHandle
	nsLink    [2]*netlink.Handle
	guestMAC  [2]string // eth0 inside each namespace
	portMAC   [2]string // host end of each veth
	bridgeMAC string
}

func newBridgeLab(t *testing.T) *bridgeLab {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to build a bridge and network namespaces")
	}
	for _, name := range []string{labBridge, labPort1, labPort2} {
		if _, err := netlink.LinkByName(name); err == nil {
			t.Skipf("%s already exists; not touching it", name)
		}
	}
	br := addLabLink(t, &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: labBridge}})
	require.NoError(t, netlink.AddrAdd(br, &netlink.Addr{IPNet: mustIPNet(t, "10.99.0.1/24")}))
	require.NoError(t, netlink.LinkSetUp(br))

	lab := &bridgeLab{}
	for i, port := range []string{labPort1, labPort2} {
		lab.ns[i] = newNamespace(t)
		h, err := netlink.NewHandleAt(lab.ns[i])
		require.NoError(t, err)
		t.Cleanup(h.Close)
		lab.nsLink[i] = h

		veth := addLabLink(t, &netlink.Veth{
			LinkAttrs:     netlink.LinkAttrs{Name: port, MasterIndex: br.Attrs().Index},
			PeerName:      "eth0",
			PeerNamespace: netlink.NsFd(lab.ns[i]),
		})
		require.NoError(t, netlink.LinkSetUp(veth))
		lab.portMAC[i] = veth.Attrs().HardwareAddr.String()

		eth0, err := h.LinkByName("eth0")
		require.NoError(t, err)
		require.NoError(t, h.LinkSetUp(eth0))
		lab.guestMAC[i] = eth0.Attrs().HardwareAddr.String()
		lab.addAddr(t, i, []string{"10.99.0.11/24", "10.99.0.12/24"}[i])
	}
	br, err := netlink.LinkByName(labBridge)
	require.NoError(t, err)
	lab.bridgeMAC = br.Attrs().HardwareAddr.String()
	return lab
}

// addLabLink creates link and deletes it again when the test ends. Creation
// fails rather than reuse a link that already exists.
func addLabLink(t *testing.T, link netlink.Link) netlink.Link {
	t.Helper()
	name := link.Attrs().Name
	require.NoError(t, netlink.LinkAdd(link), "creating %s", name)
	t.Cleanup(func() {
		l, err := netlink.LinkByName(name)
		if err == nil {
			err = netlink.LinkDel(l)
		}
		if err != nil {
			t.Errorf("deleting %s: %v", name, err)
		}
	})
	created, err := netlink.LinkByName(name)
	require.NoError(t, err)
	return created
}

func (l *bridgeLab) addAddr(t *testing.T, guest int, prefix string) {
	t.Helper()
	h := l.nsLink[guest]
	eth0, err := h.LinkByName("eth0")
	require.NoError(t, err)
	require.NoError(t, h.AddrAdd(eth0, &netlink.Addr{IPNet: mustIPNet(t, prefix)}))
}

func (l *bridgeLab) staticFDB(t *testing.T, port, mac string, vlan int) {
	t.Helper()
	link, err := netlink.LinkByName(port)
	require.NoError(t, err)
	require.NoError(t, netlink.NeighSet(&netlink.Neigh{
		LinkIndex:    link.Attrs().Index,
		Family:       unix.AF_BRIDGE,
		Flags:        netlink.NTF_MASTER,
		State:        netlink.NUD_NOARP,
		HardwareAddr: mustMAC(t, mac),
		Vlan:         vlan,
	}))
}

// newNamespace creates a network namespace that lives until the test ends.
func newNamespace(t *testing.T) netns.NsHandle {
	t.Helper()
	var ns netns.NsHandle
	var err error
	onThrowawayThread(func() { ns, err = netns.New() })
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := ns.Close(); err != nil {
			t.Errorf("closing namespace: %v", err)
		}
	})
	return ns
}

// listenIn opens a TCP listener inside ns that closes when the test ends.
func listenIn(t *testing.T, ns netns.NsHandle, addr string) net.Listener {
	t.Helper()
	var ln net.Listener
	var err error
	onThrowawayThread(func() {
		if err = netns.Set(ns); err == nil {
			ln, err = net.Listen("tcp4", addr)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// onThrowawayThread runs fn on an OS thread that ends with it, so the network
// namespace fn switches to never reaches another goroutine.
func onThrowawayThread(fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		fn()
	}()
	<-done
}

func arpFrame(t *testing.T, op arp.Operation, src, sender net.HardwareAddr, senderIP netip.Addr, target net.HardwareAddr, targetIP netip.Addr) []byte {
	t.Helper()
	pkt, err := arp.NewPacket(op, sender, senderIP, target, targetIP)
	require.NoError(t, err)
	payload, err := pkt.MarshalBinary()
	require.NoError(t, err)
	return frame(t, src, target, ethernet.EtherTypeARP, payload)
}

func ipv4Frame(t *testing.T, src, dst net.HardwareAddr) []byte {
	t.Helper()
	return frame(t, src, dst, ethernet.EtherTypeIPv4, make([]byte, 46))
}

func frame(t *testing.T, src, dst net.HardwareAddr, et ethernet.EtherType, payload []byte) []byte {
	t.Helper()
	f := ethernet.Frame{Destination: dst, Source: src, EtherType: et, Payload: payload}
	b, err := f.MarshalBinary()
	require.NoError(t, err)
	return b
}

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(s)
	require.NoError(t, err)
	return mac
}

func mustIPNet(t *testing.T, s string) *net.IPNet {
	t.Helper()
	ip, n, err := net.ParseCIDR(s)
	require.NoError(t, err)
	n.IP = ip
	return n
}
