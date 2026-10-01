package resolve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mdlayher/arp"
	"github.com/mdlayher/packet"
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

	newBridge := func(filtering *bool, pvid *uint16) *netlink.Bridge {
		return &netlink.Bridge{
			LinkAttrs:       netlink.LinkAttrs{Index: bridge, Name: "vmbr0"},
			VlanFiltering:   filtering,
			VlanDefaultPVID: pvid,
		}
	}
	on, off := true, false
	pvid1, pvid100 := uint16(1), uint16(100)
	plain := newBridge(&off, &pvid1)
	aware := newBridge(&on, &pvid1)

	tests := []struct {
		name    string
		bridge  *netlink.Bridge
		entries []netlink.Neigh
		vlan    int
		want    []int
		wantErr string
	}{
		{name: "learned on a port", bridge: plain, entries: []netlink.Neigh{learned(tap1, 0)}, want: []int{tap1}},
		{name: "stale entry", bridge: plain, entries: []netlink.Neigh{entry(tap1, bridge, 0, netlink.NUD_STALE)}, want: []int{tap1}},
		{name: "static entry", bridge: plain, entries: []netlink.Neigh{entry(tap1, bridge, 0, netlink.NUD_NOARP)}, want: []int{tap1}},
		{name: "no entry", bridge: plain},
		{name: "other MAC", bridge: plain, entries: []netlink.Neigh{otherMAC}},
		{name: "other bridge", bridge: plain, entries: []netlink.Neigh{entry(tap1, otherBridge, 0, netlink.NUD_REACHABLE)}},
		{name: "entry on the bridge itself", bridge: plain, entries: []netlink.Neigh{learned(bridge, 0)}},
		{name: "permanent entry of a port", bridge: plain, entries: []netlink.Neigh{entry(tap1, bridge, 0, netlink.NUD_PERMANENT)}},
		{name: "entry of the port device itself", bridge: plain, entries: []netlink.Neigh{ownEntry}},
		{name: "filtering off ignores VID 1", bridge: plain, entries: []netlink.Neigh{learned(tap1, 1)}},
		{name: "filtering not reported counts as off", bridge: newBridge(nil, nil), entries: []netlink.Neigh{learned(tap1, 1), learned(tap2, 0)}, want: []int{tap2}},
		{name: "filtering on matches default PVID 1", bridge: aware, entries: []netlink.Neigh{learned(tap1, 0), learned(tap2, 1)}, want: []int{tap2}},
		{name: "filtering on matches default PVID 100", bridge: newBridge(&on, &pvid100), entries: []netlink.Neigh{learned(tap1, 1), learned(tap2, 100)}, want: []int{tap2}},
		{name: "filtering on without default PVID", bridge: newBridge(&on, nil), entries: []netlink.Neigh{learned(tap1, 1)}, wantErr: "vmbr0 filters VLANs but reports no default PVID"},
		{name: "VLAN matches only itself", bridge: plain, entries: []netlink.Neigh{learned(tap1, 0), learned(tap1, 1), learned(tap2, 10)}, vlan: 10, want: []int{tap2}},
		{name: "VLAN ignores the default PVID", bridge: aware, entries: []netlink.Neigh{learned(tap1, 1)}, vlan: 10},
		{name: "same port twice", bridge: plain, entries: []netlink.Neigh{learned(tap1, 0), entry(tap1, bridge, 0, netlink.NUD_NOARP)}, want: []int{tap1}},
		{name: "two ports", bridge: plain, entries: []netlink.Neigh{learned(tap1, 0), learned(tap2, 0)}, want: []int{tap1, tap2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fdbPorts(tc.entries, tc.bridge, tc.vlan, mac)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestLinuxRetryDump(t *testing.T) {
	errOther := errors.New("permission denied")
	tests := []struct {
		name      string
		failures  []error // returned by the first attempts, in order
		wantCalls int
		wantErr   error
	}{
		{name: "first attempt", wantCalls: 1},
		{name: "interrupted twice", failures: []error{netlink.ErrDumpInterrupted, netlink.ErrDumpInterrupted}, wantCalls: 3},
		{name: "interrupted three times", failures: []error{netlink.ErrDumpInterrupted, netlink.ErrDumpInterrupted, netlink.ErrDumpInterrupted}, wantCalls: 3, wantErr: netlink.ErrDumpInterrupted},
		{name: "other error", failures: []error{errOther}, wantCalls: 1, wantErr: errOther},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got, err := retryDump(func() ([]int, error) {
				calls++
				if calls <= len(tc.failures) {
					return []int{-1}, tc.failures[calls-1]
				}
				return []int{calls}, nil
			})
			require.Equal(t, tc.wantCalls, calls)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, []int{calls}, got)
		})
	}
}

// TestLinuxARPRequest compares the probe with the request the kernel sends
// for a neighbour it has no MAC for. The reference follows arp_create() in
// net/ipv4/arp.c as arp_solicit() calls it without a destination or target
// hardware address: Ethernet broadcast from the device address, hardware
// type ARPHRD_ETHER, protocol ETH_P_IP, address lengths 6 and 4,
// ARPOP_REQUEST, the device address and source address as sender, the target
// hardware address zeroed, then the target address. That is 42 bytes:
// padding to the 60-byte Ethernet minimum is left to the driver, and virtual
// devices such as bridges, taps and veths send the frame unpadded.
// TestLinuxProberOnBridge compares it with a request the kernel really sent.
func TestLinuxARPRequest(t *testing.T) {
	want := []byte{
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, // destination: broadcast
		0xbc, 0x24, 0x11, 0x00, 0x00, 0xfe, // source: the interface
		0x08, 0x06, // EtherType ARP
		0x00, 0x01, // hardware type Ethernet
		0x08, 0x00, // protocol type IPv4
		0x06, 0x04, // hardware and protocol address lengths
		0x00, 0x01, // operation: request
		0xbc, 0x24, 0x11, 0x00, 0x00, 0xfe, // sender hardware address
		10, 20, 0, 1, // sender protocol address
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // target hardware address: unknown
		10, 20, 0, 5, // target protocol address
	}
	got, err := arpRequest(mustMAC(t, "bc:24:11:00:00:fe"), netip.MustParseAddr("10.20.0.1"), netip.MustParseAddr("10.20.0.5"))
	require.NoError(t, err)
	require.Equal(t, want, got)

	_, err = arpRequest(make(net.HardwareAddr, 20), netip.MustParseAddr("10.20.0.1"), netip.MustParseAddr("10.20.0.5"))
	require.Error(t, err)
}

func TestLinuxAppendClaimants(t *testing.T) {
	addr := netip.MustParseAddr("10.20.0.5")
	other := netip.MustParseAddr("10.20.0.6")
	host := netip.MustParseAddr("10.20.0.1")
	guest := mustMAC(t, "bc:24:11:00:00:01")
	stranger := mustMAC(t, "bc:24:11:00:00:02")
	hostMAC := mustMAC(t, "bc:24:11:00:00:fe")
	almostFull, full := stationMACs(maxClaimants-1), stationMACs(maxClaimants)

	reply := arpFrame(t, guest, arp.OperationReply, guest, addr, host)
	forged := arpFrame(t, stranger, arp.OperationReply, guest, addr, host)
	probe, err := arpRequest(hostMAC, host, addr)
	require.NoError(t, err)
	// What this host sends when it probes an address it holds itself.
	ownProbe, err := arpRequest(hostMAC, addr, addr)
	require.NoError(t, err)

	tests := []struct {
		name   string
		macs   []string
		frame  []byte
		sent   []byte // the probe; the usual one when nil
		expect []string
	}{
		{name: "reply from addr", frame: reply, expect: []string{"bc:24:11:00:00:01"}},
		{name: "repeated reply", macs: []string{"bc:24:11:00:00:01"}, frame: reply, expect: []string{"bc:24:11:00:00:01"}},
		{name: "second replier", macs: []string{"bc:24:11:00:00:02"}, frame: reply, expect: []string{"bc:24:11:00:00:02", "bc:24:11:00:00:01"}},
		{name: "request from addr", frame: arpFrame(t, guest, arp.OperationRequest, guest, addr, host), expect: []string{"bc:24:11:00:00:01"}},
		{name: "gratuitous announcement", frame: arpFrame(t, guest, arp.OperationRequest, guest, addr, addr), expect: []string{"bc:24:11:00:00:01"}},
		{name: "request for addr from another sender", frame: arpFrame(t, hostMAC, arp.OperationRequest, hostMAC, host, addr)},
		{name: "reply about another address", frame: arpFrame(t, guest, arp.OperationReply, guest, other, host)},
		{name: "other operation", frame: arpFrame(t, guest, arp.Operation(4), guest, addr, host)},
		{name: "frame source differs", frame: forged, expect: []string{"bc:24:11:00:00:01", "bc:24:11:00:00:02"}},
		{name: "claim sent with this host's MAC as source", frame: arpFrame(t, hostMAC, arp.OperationRequest, stranger, addr, addr), expect: []string{"bc:24:11:00:00:02", "bc:24:11:00:00:fe"}},
		{name: "copy of the probe sent here", frame: ownProbe, sent: ownProbe},
		{name: "padded copy of the probe sent here", frame: append(slices.Clone(ownProbe), make([]byte, 18)...), sent: ownProbe},
		{name: "stops at the limit", macs: almostFull, frame: forged, expect: append(slices.Clone(almostFull), "bc:24:11:00:00:01")},
		{name: "already at the limit", macs: full, frame: reply, expect: full},
		{name: "truncated frame", frame: reply[:20]},
		{name: "not ARP", frame: ethFrame(hostMAC, guest, 0x0800, make([]byte, 46))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sent := tc.sent
			if sent == nil {
				sent = probe
			}
			require.Equal(t, tc.expect, appendClaimants(slices.Clone(tc.macs), tc.frame, addr, sent))
		})
	}
}

func TestLinuxExchange(t *testing.T) {
	addr := netip.MustParseAddr("10.20.0.5")
	host := netip.MustParseAddr("10.20.0.1")
	req, err := arpRequest(mustMAC(t, "bc:24:11:00:00:fe"), host, addr)
	require.NoError(t, err)
	// replies returns one reply for addr from each of the stations first to
	// first+n-1, each sent from the MAC it names.
	replies := func(first, n int) [][]byte {
		out := make([][]byte, 0, n)
		for i := first; i < first+n; i++ {
			mac := net.HardwareAddr{0x02, 0, 0, 0, byte(i >> 8), byte(i)}
			out = append(out, arpFrame(t, mac, arp.OperationReply, mac, addr, host))
		}
		return out
	}
	foreign := arpFrame(t, mustMAC(t, "bc:24:11:00:00:66"), arp.OperationReply, mustMAC(t, "bc:24:11:00:00:66"), addr, host)

	run := func(ctx context.Context, frames [][]byte) ([]string, int, error) {
		clock := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
		conn := &scriptedConn{clock: &clock, frames: frames}
		p := &hostProber{arpWindow: defaultARPWindow, now: func() time.Time { return clock }}
		macs, err := p.exchange(ctx, conn, "vmbr0", req, addr)
		return macs, conn.sent, err
	}

	t.Run("no answer", func(t *testing.T) {
		macs, sent, err := run(t.Context(), nil)
		require.NoError(t, err)
		require.Empty(t, macs)
		require.Equal(t, arpRequests, sent)
	})

	t.Run("a guest with 32 NICs", func(t *testing.T) {
		macs, _, err := run(t.Context(), replies(1, 32))
		require.NoError(t, err)
		require.Equal(t, stationMACs(32), macs)
	})

	t.Run("a foreign claim after the guest's own", func(t *testing.T) {
		macs, _, err := run(t.Context(), append(replies(1, 32), foreign))
		require.NoError(t, err)
		require.Len(t, macs, 33)
		require.Contains(t, macs, "bc:24:11:00:00:66")
	})

	t.Run("too many stations", func(t *testing.T) {
		_, _, err := run(t.Context(), append(replies(1, maxClaimants), foreign))
		require.EqualError(t, err, "too many stations claim 10.20.0.5 on vmbr0")
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, _, err := run(ctx, replies(1, 1))
		require.ErrorIs(t, err, context.Canceled)
	})
}

// scriptedConn hands out queued frames. Once they are used up, it moves the
// clock to the read deadline as if nothing more had arrived.
type scriptedConn struct {
	net.PacketConn
	clock    *time.Time
	frames   [][]byte
	deadline time.Time
	sent     int
}

func (c *scriptedConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	c.sent++
	return len(b), nil
}

func (c *scriptedConn) SetReadDeadline(t time.Time) error {
	c.deadline = t
	return nil
}

func (c *scriptedConn) ReadFrom(b []byte) (int, net.Addr, error) {
	if len(c.frames) == 0 {
		*c.clock = c.deadline
		return 0, nil, os.ErrDeadlineExceeded
	}
	n := copy(b, c.frames[0])
	c.frames = c.frames[1:]
	return n, nil, nil
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
// namespaces and probes them for real. The subtests run in order and some
// depend on the ones before.
func TestLinuxProberOnBridge(t *testing.T) {
	lab := newBridgeLab(t)
	p := NewHostProber(0, 0)
	guest := netip.MustParseAddr("10.99.0.11")
	second := netip.MustParseAddr("10.99.0.12")

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

	t.Run("probe looks like the kernel's own request", func(t *testing.T) {
		capture, _ := lab.arpSocketIn(t, 0)
		unknown, probed := netip.MustParseAddr("10.99.0.50"), netip.MustParseAddr("10.99.0.51")

		// A datagram to an address without a neighbour entry makes the kernel ask for it.
		conn, err := net.Dial("udp4", netip.AddrPortFrom(unknown, 9).String())
		require.NoError(t, err)
		_, err = conn.Write([]byte{0})
		require.NoError(t, err)
		require.NoError(t, conn.Close())
		kernel := requestFor(t, capture, unknown)
		require.Len(t, kernel, 42)

		ctx := t.Context()
		done := make(chan error, 1)
		go func() {
			_, err := p.ARP(ctx, labBridge, probed)
			done <- err
		}()
		probe := requestFor(t, capture, probed)
		require.NoError(t, <-done)

		want := slices.Clone(kernel)
		target := probed.As4()
		copy(want[len(want)-4:], target[:])
		require.Equal(t, want, probe)
	})

	t.Run("forwarding table", func(t *testing.T) {
		// Answering an ARP request makes each guest send a frame the bridge learns from.
		for i, addr := range []netip.Addr{guest, second} {
			macs, err := p.ARP(t.Context(), labBridge, addr)
			require.NoError(t, err)
			require.Equal(t, []string{lab.guestMAC[i]}, macs)
		}
		for i, port := range []string{labPort1, labPort2} {
			got, found, err := p.FDBPort(t.Context(), labBridge, 0, lab.guestMAC[i])
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, port, got)
		}
		for _, own := range []string{lab.bridgeMAC, lab.portMAC[0], "bc:24:11:99:99:99"} {
			_, found, err := p.FDBPort(t.Context(), labBridge, 0, own)
			require.NoError(t, err, own)
			require.False(t, found, own)
		}
	})

	t.Run("dial", func(t *testing.T) {
		ln := listenIn(t, lab.ns[0], "10.99.0.11:0")
		port := uint16(ln.Addr().(*net.TCPAddr).Port)
		require.NoError(t, p.Dial(t.Context(), netip.AddrPortFrom(guest, port)))
		require.Error(t, p.Dial(t.Context(), netip.AddrPortFrom(guest, 1)))
	})

	t.Run("arp does not see the host's own frames", func(t *testing.T) {
		// Probing the bridge's own address sends requests with that address
		// as sender; the kernel must not hand them back to the socket.
		macs, err := p.ARP(t.Context(), labBridge, netip.MustParseAddr("10.99.0.1"))
		require.NoError(t, err)
		require.Empty(t, macs)
	})

	// The announcements below move the host's neighbour entry for the first
	// guest away from it, so nothing after them may rely on reaching the
	// first guest over IP.
	t.Run("arp sees an announcement nobody asked for", func(t *testing.T) {
		stop := lab.announce(t, 1, guest, nil)
		macs, err := p.ARP(t.Context(), labBridge, guest)
		stop()
		require.NoError(t, err)
		require.ElementsMatch(t, lab.guestMAC[:], macs)
	})

	t.Run("arp sees a claim sent with the bridge's MAC as source", func(t *testing.T) {
		stop := lab.announce(t, 1, guest, mustMAC(t, lab.bridgeMAC))
		macs, err := p.ARP(t.Context(), labBridge, guest)
		stop()
		require.NoError(t, err)
		require.ElementsMatch(t, []string{lab.guestMAC[0], lab.guestMAC[1], lab.bridgeMAC}, macs)
	})

	t.Run("arp answered by two guests", func(t *testing.T) {
		lab.addAddr(t, 1, "10.99.0.11/24")
		macs, err := p.ARP(t.Context(), labBridge, guest)
		require.NoError(t, err)
		require.ElementsMatch(t, lab.guestMAC[:], macs)
	})

	t.Run("forwarding table follows the bridge VLAN setting", func(t *testing.T) {
		mac := "bc:24:11:99:99:98"
		lab.staticFDB(t, labPort2, mac, 1)

		_, found, err := p.FDBPort(t.Context(), labBridge, 0, mac)
		require.NoError(t, err)
		require.False(t, found, "VID 1 is not untagged while VLAN filtering is off")
		port, found, err := p.FDBPort(t.Context(), labBridge, 1, mac)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, labPort2, port)

		// Only the name: the attributes read back from the bridge are not all
		// accepted when sent again.
		attrs := netlink.NewLinkAttrs()
		attrs.Name = labBridge
		require.NoError(t, netlink.BridgeSetVlanFiltering(&netlink.Bridge{LinkAttrs: attrs}, true))
		port, found, err = p.FDBPort(t.Context(), labBridge, 0, mac)
		require.NoError(t, err)
		require.True(t, found, "VID 1 is the default PVID once VLAN filtering is on")
		require.Equal(t, labPort2, port)
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
		skipLab(t, "needs root to build a bridge and network namespaces")
	}
	if _, err := netlink.LinkByName(labBridge); err == nil {
		skipLab(t, "%s already exists, probably left by an earlier run that was killed; remove it with: ip link del %s", labBridge, labBridge)
	}
	for _, name := range []string{labPort1, labPort2} {
		if _, err := netlink.LinkByName(name); err == nil {
			skipLab(t, "%s already exists and may belong to a guest; not touching it", name)
		}
	}
	if what, ok := labNetInUse(t); ok {
		skipLab(t, "%s overlaps the test network 10.99.0.0/24", what)
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

// skipLab skips the bridge test, or fails it when PCO_REQUIRE_LINUX_TESTS=1
// says this environment exists to run it.
func skipLab(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("PCO_REQUIRE_LINUX_TESTS") == "1" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// labNetInUse reports an address or route of the host, in any routing table,
// that overlaps the test network. Default routes do not count.
func labNetInUse(t *testing.T) (string, bool) {
	t.Helper()
	labNet := netip.MustParsePrefix("10.99.0.0/24")
	addrs, err := retryDump(func() ([]netlink.Addr, error) { return netlink.AddrList(nil, netlink.FAMILY_V4) })
	require.NoError(t, err)
	for _, a := range addrs {
		if pfx, ok := ipv4Prefix(a.IPNet); ok && pfx.Masked().Overlaps(labNet) {
			return fmt.Sprintf("address %s", pfx), true
		}
	}
	routes, err := retryDump(func() ([]netlink.Route, error) {
		return netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_UNSPEC}, netlink.RT_FILTER_TABLE)
	})
	require.NoError(t, err)
	for _, r := range routes {
		if pfx, ok := ipv4Prefix(r.Dst); ok && pfx.Bits() > 0 && pfx.Masked().Overlaps(labNet) {
			return fmt.Sprintf("route %s", pfx), true
		}
	}
	return "", false
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

// arpSocketIn opens a raw ARP socket on eth0 of a guest namespace and returns
// it with the MAC of eth0.
func (l *bridgeLab) arpSocketIn(t *testing.T, guest int) (*packet.Conn, net.HardwareAddr) {
	t.Helper()
	var conn *packet.Conn
	var ifi *net.Interface
	var err error
	onThrowawayThread(func() {
		if err = netns.Set(l.ns[guest]); err != nil {
			return
		}
		if ifi, err = net.InterfaceByName("eth0"); err != nil {
			return
		}
		conn, err = packet.Listen(ifi, packet.Raw, etherTypeARP, nil)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn, ifi.HardwareAddr
}

// announce makes a guest announce claimed as its address every 50 ms until
// the returned stop is called. The frame leaves from src, or from the
// guest's own MAC when src is nil.
func (l *bridgeLab) announce(t *testing.T, guest int, claimed netip.Addr, src net.HardwareAddr) (stop func()) {
	t.Helper()
	conn, mac := l.arpSocketIn(t, guest)
	frame, err := arpRequest(mac, claimed, claimed)
	require.NoError(t, err)
	if src != nil {
		copy(frame[6:12], src)
	}
	done := make(chan struct{})
	var sendErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			if _, err := conn.WriteTo(frame, &packet.Addr{HardwareAddr: broadcastMAC()}); err != nil {
				sendErr = err
				return
			}
			select {
			case <-done:
				return
			case <-tick.C:
			}
		}
	})
	return func() {
		close(done)
		wg.Wait()
		require.NoError(t, sendErr, "announcing %s", claimed)
	}
}

// requestFor reads conn until an ARP request for target arrives and returns
// the frame.
func requestFor(t *testing.T, conn *packet.Conn, target netip.Addr) []byte {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	buf := make([]byte, 128)
	for {
		n, _, err := conn.ReadFrom(buf)
		require.NoError(t, err)
		var pkt arp.Packet
		if n > ethHeaderLen && pkt.UnmarshalBinary(buf[ethHeaderLen:n]) == nil &&
			pkt.Operation == arp.OperationRequest && pkt.TargetIP == target {
			return slices.Clone(buf[:n])
		}
	}
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

// arpFrame builds a broadcast ARP frame sent from src.
func arpFrame(t *testing.T, src net.HardwareAddr, op arp.Operation, sender net.HardwareAddr, senderIP, targetIP netip.Addr) []byte {
	t.Helper()
	pkt, err := arp.NewPacket(op, sender, senderIP, make(net.HardwareAddr, 6), targetIP)
	require.NoError(t, err)
	payload, err := pkt.MarshalBinary()
	require.NoError(t, err)
	return ethFrame(broadcastMAC(), src, etherTypeARP, payload)
}

func ethFrame(dst, src net.HardwareAddr, etherType uint16, payload []byte) []byte {
	return slices.Concat(dst, src, []byte{byte(etherType >> 8), byte(etherType)}, payload)
}

// stationMACs returns n distinct locally administered MACs, the nth being
// 02:00:00:00:00:<n> for small n.
func stationMACs(n int) []string {
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, net.HardwareAddr{0x02, 0, 0, 0, byte(i >> 8), byte(i)}.String())
	}
	return out
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
