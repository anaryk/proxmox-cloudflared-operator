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
	"time"

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

	ethHeaderLen  = 14
	etherTypeARP  = 0x0806
	etherTypeIPv4 = 0x0800
	arpEthernet   = 1 // ARP hardware type of Ethernet
	arpOpRequest  = 1
	arpOpReply    = 2
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
// 600 ms, a zero dialTimeout 2 s. It keeps no state between calls, and every
// call opens sockets of its own, so it is safe for concurrent use.
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
	bridges := map[string]bool{}
	for _, l := range links {
		names[l.Attrs().Index] = l.Attrs().Name
		if _, ok := l.(*netlink.Bridge); ok {
			bridges[l.Attrs().Name] = true
		}
	}
	out := make([]HostIface, 0, len(links))
	for _, l := range links {
		at := l.Attrs()
		info := linkInfo{name: at.Name}
		switch v := l.(type) {
		case *netlink.Bridge:
			info.bridge = true
		case *netlink.Vlan:
			info.vlanParent, info.vlanID = names[at.ParentIndex], v.VlanId
		}
		out = append(out, HostIface{Name: at.Name, Addrs: prefixes[at.Index], Master: names[at.MasterIndex], Segment: segmentOf(info, bridges)})
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
	if !src.Is4() || !addr.Is4() {
		return nil, fmt.Errorf("building request from %s for %s: not IPv4", src, addr)
	}
	spa, tpa := src.As4(), addr.As4()
	frame := make([]byte, 0, ethHeaderLen+28)
	frame = append(frame, broadcastMAC()...)
	frame = append(frame, hw...)
	frame = binary.BigEndian.AppendUint16(frame, etherTypeARP)
	frame = binary.BigEndian.AppendUint16(frame, arpEthernet)
	frame = binary.BigEndian.AppendUint16(frame, etherTypeIPv4)
	frame = append(frame, 6, 4) // address lengths
	frame = binary.BigEndian.AppendUint16(frame, arpOpRequest)
	frame = append(frame, hw...)
	frame = append(frame, spa[:]...)
	frame = append(frame, make([]byte, 6)...)
	return append(frame, tpa[:]...), nil
}

// arpPacket is what an ARP payload says about its sender and target.
type arpPacket struct {
	op       uint16
	senderHW net.HardwareAddr
	senderIP netip.Addr
	targetIP netip.Addr
}

// parseARP reads an ARP payload with IPv4 addresses. The hardware type, the
// protocol type and the length of the hardware addresses are taken as they
// come: the kernel accepts more than one hardware type, and reading a claim
// the kernel would ignore only makes the identity check stricter.
func parseARP(b []byte) (arpPacket, bool) {
	if len(b) < 8 || b[5] != 4 {
		return arpPacket{}, false
	}
	hlen := int(b[4])
	sender := 8 + hlen          // offset of the sender address
	target := sender + 4 + hlen // offset of the target address
	if len(b) < target+4 {
		return arpPacket{}, false
	}
	return arpPacket{
		op:       binary.BigEndian.Uint16(b[6:8]),
		senderHW: slices.Clone(net.HardwareAddr(b[8:sender])),
		senderIP: netip.AddrFrom4([4]byte(b[sender : sender+4])),
		targetIP: netip.AddrFrom4([4]byte(b[target : target+4])),
	}, true
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
	pkt, ok := parseARP(frame[ethHeaderLen:])
	if !ok || pkt.senderIP != addr || (pkt.op != arpOpRequest && pkt.op != arpOpReply) {
		return macs
	}
	for _, hw := range []net.HardwareAddr{pkt.senderHW, frame[6:12]} {
		if mac := hw.String(); len(macs) < maxClaimants && !slices.Contains(macs, mac) {
			macs = append(macs, mac)
		}
	}
	return macs
}

func broadcastMAC() net.HardwareAddr {
	return net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
}

// Route asks the kernel for the route it would use to send to addr from this
// host, as "ip route get" does, and asks again from the source address the
// kernel picked. Both answers must leave through the same interface, both
// on-link or both through a gateway; otherwise a rule on the source address
// sends the host's own traffic elsewhere, and Route fails with
// ErrRouteDiffers. Policy routing on anything else, such as the user of a
// socket or a firewall mark, is not looked at.
func (p *hostProber) Route(ctx context.Context, addr netip.Addr) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if !addr.Is4() {
		return "", false, fmt.Errorf("%s is not an IPv4 address", addr)
	}
	first, err := routeGet(addr, nil)
	if err != nil {
		return "", false, err
	}
	if first.Src == nil {
		return "", false, fmt.Errorf("route to %s: the kernel picked no source address", addr)
	}
	again, err := routeGet(addr, &netlink.RouteGetOptions{SrcAddr: first.Src})
	switch {
	case errors.Is(err, ErrNoRoute), err == nil && (again.LinkIndex != first.LinkIndex || onLink(again) != onLink(first)):
		return "", false, fmt.Errorf("route to %s from %s: %w", addr, first.Src, ErrRouteDiffers)
	case err != nil:
		return "", false, err
	}
	link, err := netlink.LinkByIndex(first.LinkIndex)
	if err != nil {
		return "", false, fmt.Errorf("looking up interface %d: %w", first.LinkIndex, err)
	}
	return link.Attrs().Name, onLink(first), nil
}

// routeGet looks up the route to addr, with ErrNoRoute when the kernel has
// none it would send on.
func routeGet(addr netip.Addr, opts *netlink.RouteGetOptions) (netlink.Route, error) {
	routes, err := netlink.RouteGetWithOptions(addr.AsSlice(), opts)
	switch {
	case noRoute(err), err == nil && len(routes) == 0:
		return netlink.Route{}, fmt.Errorf("route to %s: %w", addr, ErrNoRoute)
	case err != nil:
		return netlink.Route{}, fmt.Errorf("looking up route: %w", err)
	}
	return routes[0], nil
}

// noRoute reports whether the kernel answered a route lookup with having no
// route to the address (ENETUNREACH) or with one that drops the traffic:
// unreachable (EHOSTUNREACH), blackhole (EINVAL) or prohibit (EACCES). The
// address is IPv4 by then, so EINVAL does not stand for a malformed request.
func noRoute(err error) bool {
	return errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.EHOSTUNREACH) ||
		errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EACCES)
}

// onLink reports whether r sends to the destination itself rather than to a
// gateway.
func onLink(r netlink.Route) bool {
	return r.Gw == nil && r.Via == nil && len(r.MultiPath) == 0
}

// FDBPorts returns the ports in name order, so that the same table always
// reads the same.
func (p *hostProber) FDBPorts(_ context.Context, bridge string, vlan int, mac string) ([]string, error) {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return nil, fmt.Errorf("parsing MAC: %w", err)
	}
	link, err := netlink.LinkByName(bridge)
	if err != nil {
		return nil, fmt.Errorf("looking up bridge: %w", err)
	}
	br, ok := link.(*netlink.Bridge)
	if !ok {
		return nil, fmt.Errorf("%s is not a Linux bridge", bridge)
	}
	entries, err := retryDump(func() ([]netlink.Neigh, error) { return netlink.NeighList(0, unix.AF_BRIDGE) })
	if err != nil {
		return nil, fmt.Errorf("dumping forwarding database: %w", err)
	}
	indexes, err := fdbPorts(entries, br, vlan, hw)
	if err != nil {
		return nil, err
	}
	ports, err := linkNames(indexes)
	if err != nil {
		return nil, err
	}
	slices.Sort(ports)
	return ports, nil
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
