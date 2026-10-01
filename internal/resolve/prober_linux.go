package resolve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/mdlayher/arp"
	"github.com/mdlayher/ethernet"
	"github.com/mdlayher/packet"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	defaultARPWindow   = 600 * time.Millisecond
	defaultDialTimeout = 2 * time.Second
	arpRequests        = 3
	arpInterval        = 100 * time.Millisecond
)

// hostProber looks at the node through netlink, a raw ARP socket and TCP.
type hostProber struct {
	arpWindow   time.Duration
	dialTimeout time.Duration
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
	return &hostProber{arpWindow: arpWindow, dialTimeout: dialTimeout}
}

func (p *hostProber) Interfaces(context.Context) ([]HostIface, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("listing links: %w", err)
	}
	addrs, err := netlink.AddrList(nil, netlink.FAMILY_V4)
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
	conn, err := packet.Listen(ifi, packet.Raw, int(ethernet.EtherTypeARP), nil)
	if err != nil {
		return nil, fmt.Errorf("opening raw socket: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// A deadline in the past wakes the pending read as soon as ctx ends.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()
	return p.exchange(ctx, conn, req, addr)
}

// exchange sends req arpRequests times, arpInterval apart, and collects the
// replies about addr until the window ends.
func (p *hostProber) exchange(ctx context.Context, conn net.PacketConn, req []byte, addr netip.Addr) ([]string, error) {
	start := time.Now()
	end := start.Add(p.arpWindow)
	to := &packet.Addr{HardwareAddr: ethernet.Broadcast}
	buf := make([]byte, 128)
	var macs []string
	for sent := 0; time.Now().Before(end); {
		if sent < arpRequests && !time.Now().Before(start.Add(time.Duration(sent)*arpInterval)) {
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
			macs = appendClaimants(macs, buf[:n], addr)
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

// arpRequest builds the broadcast frame that asks who has addr.
func arpRequest(hw net.HardwareAddr, src, addr netip.Addr) ([]byte, error) {
	pkt, err := arp.NewPacket(arp.OperationRequest, hw, src, ethernet.Broadcast, addr)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	payload, err := pkt.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	f := ethernet.Frame{Destination: ethernet.Broadcast, Source: hw, EtherType: ethernet.EtherTypeARP, Payload: payload}
	return f.MarshalBinary()
}

// appendClaimants adds the MACs behind frame to macs when it is an ARP reply
// from addr. Both the sender hardware address and the frame's source count,
// so a reply sent from one MAC on behalf of another names both. Frames that
// do not parse are skipped: anyone on the segment can send them.
func appendClaimants(macs []string, frame []byte, addr netip.Addr) []string {
	var f ethernet.Frame
	if f.UnmarshalBinary(frame) != nil || f.EtherType != ethernet.EtherTypeARP {
		return macs
	}
	var pkt arp.Packet
	if pkt.UnmarshalBinary(f.Payload) != nil || pkt.Operation != arp.OperationReply || pkt.SenderIP != addr {
		return macs
	}
	for _, hw := range []net.HardwareAddr{pkt.SenderHardwareAddr, f.Source} {
		if mac := hw.String(); !slices.Contains(macs, mac) {
			macs = append(macs, mac)
		}
	}
	return macs
}

func (p *hostProber) FDBPort(_ context.Context, bridge string, vlan int, mac string) (string, bool, error) {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return "", false, fmt.Errorf("parsing MAC: %w", err)
	}
	br, err := netlink.LinkByName(bridge)
	if err != nil {
		return "", false, fmt.Errorf("looking up bridge: %w", err)
	}
	if _, ok := br.(*netlink.Bridge); !ok {
		return "", false, fmt.Errorf("%s is not a Linux bridge", bridge)
	}
	entries, err := netlink.NeighList(0, unix.AF_BRIDGE)
	if err != nil {
		return "", false, fmt.Errorf("dumping forwarding database: %w", err)
	}
	ports, err := linkNames(fdbPorts(entries, br.Attrs().Index, vlan, hw))
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

// fdbPorts returns the ports of bridge on which mac is learned for vlan, in
// the order found. Entries of the bridge device itself and permanent ones
// hold the host's own addresses, not learned ones, and are skipped.
func fdbPorts(entries []netlink.Neigh, bridge, vlan int, mac net.HardwareAddr) []int {
	var ports []int
	for _, e := range entries {
		learned := e.MasterIndex == bridge && e.LinkIndex != bridge && e.State&netlink.NUD_PERMANENT == 0
		if learned && bytes.Equal(e.HardwareAddr, mac) && vlanMatches(e.Vlan, vlan) && !slices.Contains(ports, e.LinkIndex) {
			ports = append(ports, e.LinkIndex)
		}
	}
	return ports
}

// vlanMatches reports whether an entry of VLAN entry belongs to want. Untagged
// (0) matches entries without a VLAN and those of PVID 1, under which a bridge
// without VLAN filtering keeps them too.
func vlanMatches(entry, want int) bool {
	if want == 0 {
		return entry == 0 || entry == 1
	}
	return entry == want
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

func (p *hostProber) Dial(ctx context.Context, target netip.AddrPort) error {
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
