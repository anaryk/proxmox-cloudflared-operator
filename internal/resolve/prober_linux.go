package resolve

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/mdlayher/arp"
	"github.com/mdlayher/packet"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	defaultARPWindow   = 600 * time.Millisecond
	defaultDialTimeout = 2 * time.Second
	arpRequests        = 3
	arpInterval        = 100 * time.Millisecond
	// maxClaimants bounds the MACs one probe collects. A guest may have all
	// 32 of its NICs on one bridge and answer from each; reaching the bound
	// fails the probe, since a foreign claim could be among the frames not
	// read.
	maxClaimants = 64
	dumpAttempts = 3
	etherTypeARP = 0x0806
	ethHeaderLen = 14
)

// hostProber looks at the node through netlink, a raw ARP socket and TCP.
type hostProber struct {
	arpWindow   time.Duration
	dialTimeout time.Duration
	// now schedules the ARP exchange; its read deadlines must be real time
	// unless the connection is a test double.
	now func() time.Time
}

// NewHostProber returns the Prober of this host. A zero arpWindow selects
// 600 ms, a zero dialTimeout 2 s.
func NewHostProber(arpWindow, dialTimeout time.Duration) Prober {
	if arpWindow <= 0 {
		arpWindow = defaultARPWindow
	}
	if dialTimeout <= 0 {
		dialTimeout = defaultDialTimeout
	}
	return &hostProber{arpWindow: arpWindow, dialTimeout: dialTimeout, now: time.Now}
}

func (p *hostProber) Interfaces(context.Context) ([]HostIface, error) {
	links, err := retryDump(netlink.LinkList)
	if err != nil {
		return nil, fmt.Errorf("listing links: %w", err)
	}
	addrs, err := retryDump(func() ([]netlink.Addr, error) { return netlink.AddrList(nil, netlink.FAMILY_V4) })
	if err != nil {
		return nil, fmt.Errorf("listing addresses: %w", err)
	}
	prefixes := map[int][]netip.Prefix{}
	for _, a := range addrs {
		if pfx, ok := ipv4Prefix(a.IPNet); ok {
			prefixes[a.LinkIndex] = append(prefixes[a.LinkIndex], pfx)
		}
	}
	names := make(map[int]string, len(links))
	for _, l := range links {
		names[l.Attrs().Index] = l.Attrs().Name
	}
	out := make([]HostIface, 0, len(links))
	for _, l := range links {
		at := l.Attrs()
		out = append(out, HostIface{Name: at.Name, Addrs: prefixes[at.Index], Master: names[at.MasterIndex]})
	}
	return out, nil
}

// ARP sends its own requests from a raw socket rather than reading the
// neighbour table, which keeps only one MAC per address.
func (p *hostProber) ARP(ctx context.Context, iface string, addr netip.Addr) ([]string, error) {
	if !addr.Is4() {
		return nil, fmt.Errorf("%s is not an IPv4 address", addr)
	}
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("looking up interface: %w", err)
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, fmt.Errorf("listing addresses: %w", err)
	}
	src, err := sourceAddr(addrs, addr)
	if err != nil {
		return nil, err
	}
	req, err := arpRequest(ifi.HardwareAddr, src, addr)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := packet.Listen(ifi, packet.Raw, etherTypeARP, nil)
	if err != nil {
		return nil, fmt.Errorf("opening raw socket: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// A deadline in the past wakes the pending read as soon as ctx ends.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()
	return p.exchange(ctx, conn, iface, req, addr)
}

// exchange sends req arpRequests times, arpInterval apart, and collects the
// MACs that claim addr until the window ends. Finding maxClaimants of them is
// an error rather than a partial answer.
func (p *hostProber) exchange(ctx context.Context, conn net.PacketConn, iface string, req []byte, addr netip.Addr) ([]string, error) {
	start := p.now()
	end := start.Add(p.arpWindow)
	to := &packet.Addr{HardwareAddr: broadcastMAC()}
	buf := make([]byte, 128)
	var macs []string
	for sent := 0; p.now().Before(end); {
		if sent < arpRequests && !p.now().Before(start.Add(time.Duration(sent)*arpInterval)) {
			if _, err := conn.WriteTo(req, to); err != nil {
				return nil, fmt.Errorf("sending request: %w", err)
			}
			sent++
		}
		wake := end
		if next := start.Add(time.Duration(sent) * arpInterval); sent < arpRequests && next.Before(end) {
			wake = next
		}
		// ctx is checked only after the deadline is set, so a cancellation
		// that set its own deadline in between is not lost.
		if err := conn.SetReadDeadline(wake); err != nil {
			return nil, fmt.Errorf("setting read deadline: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, _, err := conn.ReadFrom(buf)
		switch {
		case err == nil:
			if macs = appendClaimants(macs, buf[:n], addr, req); len(macs) == maxClaimants {
				return nil, fmt.Errorf("too many stations claim %s on %s: %w", addr, iface, ErrTooManyClaimants)
			}
		case !errors.Is(err, os.ErrDeadlineExceeded):
			return nil, fmt.Errorf("reading replies: %w", err)
		}
	}
	return macs, nil
}

// sourceAddr returns the first IPv4 address in addrs whose network holds addr.
func sourceAddr(addrs []net.Addr, addr netip.Addr) (netip.Addr, error) {
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if pfx, ok := ipv4Prefix(n); ok && pfx.Contains(addr) {
			return pfx.Addr(), nil
		}
	}
	return netip.Addr{}, fmt.Errorf("no IPv4 address in the network of %s", addr)
}

// arpRequest builds the frame the kernel itself sends to resolve addr from
// src: broadcast, target hardware address zero and no padding. A probe that
// looked different could be told apart and answered differently from the
// kernel's own requests, which are what the host's traffic follows.
func arpRequest(hw net.HardwareAddr, src, addr netip.Addr) ([]byte, error) {
	if len(hw) != 6 {
		return nil, errors.New("interface has no Ethernet address")
	}
	pkt, err := arp.NewPacket(arp.OperationRequest, hw, src, make(net.HardwareAddr, 6), addr)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	payload, err := pkt.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	return slices.Concat(broadcastMAC(), hw, binary.BigEndian.AppendUint16(nil, etherTypeARP), payload), nil
}

// appendClaimants adds to macs, up to maxClaimants, the MACs behind frame
// when it claims addr: an ARP request, reply or announcement with addr as
// its sender address, which is what makes a host update its neighbour
// table. Both the sender hardware address and the frame's source count, so a
// claim sent from one MAC on behalf of another names both. Frames that do
// not parse are skipped: anyone on the segment can send them.
//
// The kernel does not pass the frames this host sends to a socket bound to
// ARP, so sent, the request sent here, comes back only when the network
// reflects it; such a copy is skipped. Frames are not skipped for merely
// coming from this host's MAC: anyone can put that MAC as source on a claim.
func appendClaimants(macs []string, frame []byte, addr netip.Addr, sent []byte) []string {
	if len(frame) < ethHeaderLen || binary.BigEndian.Uint16(frame[12:ethHeaderLen]) != etherTypeARP {
		return macs
	}
	if len(sent) > 0 && bytes.HasPrefix(frame, sent) {
		return macs
	}
	var pkt arp.Packet
	if pkt.UnmarshalBinary(frame[ethHeaderLen:]) != nil || pkt.SenderIP != addr {
		return macs
	}
	if pkt.Operation != arp.OperationRequest && pkt.Operation != arp.OperationReply {
		return macs
	}
	for _, hw := range []net.HardwareAddr{pkt.SenderHardwareAddr, frame[6:12]} {
		if mac := hw.String(); len(macs) < maxClaimants && !slices.Contains(macs, mac) {
			macs = append(macs, mac)
		}
	}
	return macs
}

func broadcastMAC() net.HardwareAddr {
	return net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
}

func (p *hostProber) FDBPort(_ context.Context, bridge string, vlan int, mac string) (string, bool, error) {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return "", false, fmt.Errorf("parsing MAC: %w", err)
	}
	link, err := netlink.LinkByName(bridge)
	if err != nil {
		return "", false, fmt.Errorf("looking up bridge: %w", err)
	}
	br, ok := link.(*netlink.Bridge)
	if !ok {
		return "", false, fmt.Errorf("%s is not a Linux bridge", bridge)
	}
	entries, err := retryDump(func() ([]netlink.Neigh, error) { return netlink.NeighList(0, unix.AF_BRIDGE) })
	if err != nil {
		return "", false, fmt.Errorf("dumping forwarding database: %w", err)
	}
	indexes, err := fdbPorts(entries, br, vlan, hw)
	if err != nil {
		return "", false, err
	}
	ports, err := linkNames(indexes)
	switch {
	case err != nil:
		return "", false, err
	case len(ports) == 0:
		return "", false, nil
	case len(ports) > 1:
		return "", false, fmt.Errorf("MAC %s is learned on ports %s", hw, strings.Join(ports, " and "))
	}
	return ports[0], true, nil
}

// fdbPorts returns the ports of br on which mac is learned for vlan, in the
// order found. Entries of the bridge device itself and permanent ones hold
// the host's own addresses, not learned ones, and are skipped.
func fdbPorts(entries []netlink.Neigh, br *netlink.Bridge, vlan int, mac net.HardwareAddr) ([]int, error) {
	vid, err := fdbVID(br, vlan)
	if err != nil {
		return nil, err
	}
	bridge := br.Attrs().Index
	var ports []int
	for _, e := range entries {
		learned := e.MasterIndex == bridge && e.LinkIndex != bridge && e.State&netlink.NUD_PERMANENT == 0
		if learned && e.Vlan == vid && bytes.Equal(e.HardwareAddr, mac) && !slices.Contains(ports, e.LinkIndex) {
			ports = append(ports, e.LinkIndex)
		}
	}
	return ports, nil
}

// fdbVID returns the VLAN under which br keeps the entries of vlan. Untagged
// (0) is VID 0 while VLAN filtering is off and the default PVID while it is
// on.
func fdbVID(br *netlink.Bridge, vlan int) (int, error) {
	switch {
	case vlan != 0:
		return vlan, nil
	case br.VlanFiltering == nil || !*br.VlanFiltering:
		return 0, nil
	case br.VlanDefaultPVID == nil:
		return 0, fmt.Errorf("%s filters VLANs but reports no default PVID", br.Attrs().Name)
	}
	return int(*br.VlanDefaultPVID), nil
}

func linkNames(indexes []int) ([]string, error) {
	names := make([]string, 0, len(indexes))
	for _, i := range indexes {
		l, err := netlink.LinkByIndex(i)
		if err != nil {
			return nil, fmt.Errorf("looking up port %d: %w", i, err)
		}
		names = append(names, l.Attrs().Name)
	}
	return names, nil
}

// retryDump runs list again while the kernel reports that the dump was
// interrupted by a concurrent change, up to dumpAttempts times in all.
func retryDump[T any](list func() ([]T, error)) ([]T, error) {
	for attempt := 1; ; attempt++ {
		out, err := list()
		if !errors.Is(err, netlink.ErrDumpInterrupted) || attempt == dumpAttempts {
			return out, err
		}
	}
}

func (p *hostProber) Dial(ctx context.Context, target netip.AddrPort) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d := net.Dialer{Timeout: p.dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", target.String())
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// ipv4Prefix converts n when it is an IPv4 address with its prefix length.
func ipv4Prefix(n *net.IPNet) (netip.Prefix, bool) {
	if n == nil {
		return netip.Prefix{}, false
	}
	ip, ok := netip.AddrFromSlice(n.IP.To4())
	ones, bits := n.Mask.Size()
	if !ok || bits != 32 {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(ip, ones), true
}
