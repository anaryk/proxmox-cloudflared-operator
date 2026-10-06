//go:build linux

package appnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// NewNetlink returns the Netlink of the network namespace that the calling
// thread is in at each call.
func NewNetlink() Netlink { return linuxNetlink{} }

type linuxNetlink struct{}

// linkByName returns the device, or nil when there is none.
func linkByName(name string) (netlink.Link, error) {
	l, err := netlink.LinkByName(name)
	var missing netlink.LinkNotFoundError
	if errors.As(err, &missing) {
		return nil, nil
	}
	return l, err
}

func (linuxNetlink) EnsureDummy(_ context.Context, name string) error {
	l, err := linkByName(name)
	switch {
	case err != nil:
		return err
	case l == nil:
		if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}); err != nil {
			return err
		}
		if l, err = netlink.LinkByName(name); err != nil {
			return err
		}
	case l.Type() != "dummy":
		return fmt.Errorf("%s is a device of the type %s, not a dummy one", name, l.Type())
	}
	return netlink.LinkSetUp(l)
}

func (linuxNetlink) HasDummy(_ context.Context, name string) (bool, error) {
	l, err := linkByName(name)
	if l == nil || err != nil {
		return false, err
	}
	return l.Type() == "dummy" && l.Attrs().Flags&net.FlagUp != 0, nil
}

func (linuxNetlink) EnsureAddr(_ context.Context, dev string, addr netip.Prefix) error {
	l, err := netlink.LinkByName(dev)
	if err != nil {
		return err
	}
	return netlink.AddrReplace(l, &netlink.Addr{IPNet: ipNet(addr)})
}

func (linuxNetlink) HasAddr(_ context.Context, dev string, addr netip.Prefix) (bool, error) {
	l, err := linkByName(dev)
	if l == nil || err != nil {
		return false, err
	}
	addrs, err := netlink.AddrList(l, netlink.FAMILY_V4)
	if err != nil {
		return false, err
	}
	for _, a := range addrs {
		if p, ok := prefixOf(a.IPNet); ok && p == addr {
			return true, nil
		}
	}
	return false, nil
}

func (linuxNetlink) EnsureRoute(_ context.Context, prefix netip.Prefix, dev string, src netip.Addr) error {
	l, err := netlink.LinkByName(dev)
	if err != nil {
		return err
	}
	return netlink.RouteReplace(&netlink.Route{
		LinkIndex: l.Attrs().Index,
		Dst:       ipNet(prefix),
		Src:       src.AsSlice(),
		Scope:     netlink.SCOPE_LINK,
		Table:     unix.RT_TABLE_MAIN,
	})
}

func (linuxNetlink) HasRoute(_ context.Context, prefix netip.Prefix, dev string, src netip.Addr) (bool, error) {
	l, err := linkByName(dev)
	if l == nil || err != nil {
		return false, err
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
		&netlink.Route{LinkIndex: l.Attrs().Index, Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE)
	if err != nil {
		return false, err
	}
	for _, r := range routes {
		dst, ok := prefixOf(r.Dst)
		got, _ := netip.AddrFromSlice(r.Src)
		if ok && dst == prefix && got.Unmap() == src && r.Gw == nil && r.Type == unix.RTN_UNICAST {
			return true, nil
		}
	}
	return false, nil
}

func (linuxNetlink) EnsureRule(_ context.Context, pref int, src netip.Addr, dst netip.Prefix, unreachable bool) error {
	r := netlink.NewRule()
	r.Family = unix.AF_INET
	r.Priority = pref
	r.Src = ipNet(netip.PrefixFrom(src, src.BitLen()))
	if dst.IsValid() {
		r.Dst = ipNet(dst)
	}
	if unreachable {
		r.Type = unix.RTN_UNREACHABLE
	} else {
		r.Table = unix.RT_TABLE_MAIN
	}
	return netlink.RuleAdd(r)
}

func (linuxNetlink) HasRule(_ context.Context, pref int, src netip.Addr, dst netip.Prefix, unreachable bool) (bool, error) {
	rules, err := listRules()
	if err != nil {
		return false, err
	}
	want := rule{pref: pref, src: netip.PrefixFrom(src, src.BitLen()), dst: dst, action: unix.FR_ACT_TO_TBL, table: unix.RT_TABLE_MAIN}
	if unreachable {
		want.action, want.table = unix.FR_ACT_UNREACHABLE, unix.RT_TABLE_UNSPEC
	}
	for _, r := range rules {
		if r == want {
			return true, nil
		}
	}
	return false, nil
}

// rule is an IPv4 policy rule as the kernel lists it, with what the netlink
// package leaves out: its action. One that matches on anything but its source
// and destination is marked other, and never equals a rule of Load.
type rule struct {
	pref     int
	src, dst netip.Prefix
	action   uint8
	table    uint32
	other    bool
}

// listRules lists the IPv4 policy rules, once more when the kernel says the
// list changed while it was read.
func listRules() ([]rule, error) {
	var err error
	for range 3 {
		var msgs [][]byte
		req := nl.NewNetlinkRequest(unix.RTM_GETRULE, unix.NLM_F_DUMP)
		req.AddData(&nl.RtMsg{RtMsg: unix.RtMsg{Family: unix.AF_INET}})
		msgs, err = req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWRULE)
		if errors.Is(err, nl.ErrDumpInterrupted) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("listing the policy rules: %w", err)
		}
		rules := make([]rule, 0, len(msgs))
		for _, m := range msgs {
			r, err := parseRule(m)
			if err != nil {
				return nil, fmt.Errorf("reading a policy rule: %w", err)
			}
			rules = append(rules, r)
		}
		return rules, nil
	}
	return nil, fmt.Errorf("listing the policy rules: %w", err)
}

// parseRule reads a rule message: a fib_rule_hdr, whose action sits where
// the type of a route message does, and its attributes.
func parseRule(m []byte) (rule, error) {
	if len(m) < unix.SizeofRtMsg {
		return rule{}, errors.New("message too short")
	}
	hdr := nl.DeserializeRtMsg(m)
	attrs, err := nl.ParseRouteAttr(m[hdr.Len():])
	if err != nil {
		return rule{}, err
	}
	r := rule{action: hdr.Type, table: uint32(hdr.Table), other: hdr.Family != unix.AF_INET || hdr.Tos != 0 || hdr.Flags != 0}
	for _, a := range attrs {
		switch a.Attr.Type {
		case nl.FRA_PRIORITY:
			r.pref = int(native32(a.Value))
		case nl.FRA_TABLE:
			r.table = native32(a.Value)
		case nl.FRA_SRC:
			r.src = prefixFrom(a.Value, hdr.Src_len)
		case nl.FRA_DST:
			r.dst = prefixFrom(a.Value, hdr.Dst_len)
		case nl.FRA_PROTOCOL:
		case nl.FRA_SUPPRESS_PREFIXLEN, nl.FRA_SUPPRESS_IFGROUP:
			// Unset, the kernel lists them as -1.
			r.other = r.other || native32(a.Value) != ^uint32(0)
		default:
			r.other = true
		}
	}
	return r, nil
}

func native32(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	return nl.NativeEndian().Uint32(b[:4])
}

func prefixFrom(b []byte, bits uint8) netip.Prefix {
	a, ok := netip.AddrFromSlice(b)
	if !ok {
		return netip.Prefix{}
	}
	return netip.PrefixFrom(a.Unmap(), int(bits))
}

func ipNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}

func prefixOf(n *net.IPNet) (netip.Prefix, bool) {
	if n == nil {
		return netip.Prefix{}, false
	}
	a, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, _ := n.Mask.Size()
	return netip.PrefixFrom(a.Unmap(), ones), true
}
