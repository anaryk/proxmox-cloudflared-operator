//go:build linux

package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/rs/zerolog"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// NewContainer returns the backend of the appliance: legs are NICs net1..net9
// of the container itself, added through the Proxmox API. With a nil api no
// NIC is ever added or removed: the boot path configures what exists. expect
// is told the MAC of a card before the API adds or removes it. The backend
// works in the network namespace of the calling thread, the appliance's own,
// whichever thread calls it later; nft is to run there too.
func NewContainer(nft Nft, api NICWriter, self model.GuestRef, node string, expect func(mac net.HardwareAddr), log zerolog.Logger,
	opts ...ContainerOption,
) Backend {
	ns, err := netns.Get()
	if err != nil {
		ns = netns.None()
	}
	return newContainer(newContainerSystem(ns), nft, api, self, node, expect, log, time.Now, opts...)
}

// containerLinux is the network of the appliance: the namespace of the
// process, unless a test gives another.
type containerLinux struct{ ls *linuxSystem }

func newContainerSystem(ns netns.NsHandle) containerLinux {
	return containerLinux{ls: newLinuxSystem("", ns)}
}

func (s containerLinux) handle() netns.NsHandle { return s.ls.hostNS }

func (s containerLinux) sysctl(key string) (string, error) { return s.ls.sysctl(onHost, key) }

func (s containerLinux) setSysctl(key, value string) error { return s.ls.setSysctl(onHost, key, value) }

func (s containerLinux) link(name string) (link, bool, error) { return s.ls.link(onHost, name) }

func (s containerLinux) links() ([]link, error) { return s.ls.links(onHost) }

func (s containerLinux) addDummy(name string) error {
	h, _, err := s.ls.handles(onHost)
	if err != nil {
		return err
	}
	return h.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}})
}

func (s containerLinux) rename(name, to string) error {
	return s.ls.withLink(onHost, name, func(h *netlink.Handle, l netlink.Link) error { return h.LinkSetName(l, to) })
}

func (s containerLinux) setUp(name string) error { return s.ls.setUp(onHost, name) }

func (s containerLinux) setDown(name string) error {
	return s.ls.withLink(onHost, name, func(h *netlink.Handle, l netlink.Link) error { return h.LinkSetDown(l) })
}

func (s containerLinux) addrs(dev string) ([]address, error) { return s.ls.addrs(onHost, dev) }

func (s containerLinux) addAddr(dev string, a address) error { return s.ls.addAddr(onHost, dev, a) }

func (s containerLinux) delAddr(dev string, p netip.Prefix) error {
	return s.ls.delAddr(onHost, dev, p)
}

func (s containerLinux) routes(table int) ([]route, error) { return s.ls.routes(onHost, table) }

func (s containerLinux) addRoute(r route) error { return s.ls.addRoute(onHost, r) }

func (s containerLinux) delRoute(r route) error { return s.ls.delRoute(onHost, r) }

func (s containerLinux) addRule(r rule) error { return s.ls.addRule(onHost, r) }

func (s containerLinux) delRule(r rule) error { return s.ls.delRule(onHost, r) }

func (s containerLinux) neighbours(dev string) ([]neighbour, error) {
	return s.ls.neighboursOn(onHost, dev)
}

func (s containerLinux) setNeighbour(dev string, n neighbour) error {
	return s.ls.setNeighbourOn(onHost, dev, n)
}

func (s containerLinux) delNeighbour(dev string, addr netip.Addr) error {
	return s.ls.delNeighbourOn(onHost, dev, addr)
}

func (s containerLinux) arpProbe(ctx context.Context, dev string, addr netip.Addr, window time.Duration) ([]net.HardwareAddr, error) {
	return s.ls.arpProbeOn(ctx, onHost, dev, addr, window)
}

func (s containerLinux) dial(ctx context.Context, mark int, src netip.Addr, dst netip.AddrPort) (net.Conn, error) {
	return dialIn(ctx, s.ls.hostNS, mark, src, dst)
}

// rules lists the IPv4 policy rules with what the netlink package leaves
// out of a listing, their action: a rule that is unreachable is told from
// one that looks a table up only by it. It reads them once more when the
// kernel says the list changed meanwhile.
func (s containerLinux) rules() ([]rule, error) {
	var out []rule
	err := Do(s.ls.hostNS, func() error {
		var err error
		for range 3 {
			req := nl.NewNetlinkRequest(unix.RTM_GETRULE, unix.NLM_F_DUMP)
			req.AddData(&nl.RtMsg{RtMsg: unix.RtMsg{Family: unix.AF_INET}})
			var msgs [][]byte
			msgs, err = req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWRULE)
			if errors.Is(err, nl.ErrDumpInterrupted) {
				continue
			}
			if err != nil {
				return err
			}
			out = out[:0]
			for _, m := range msgs {
				r, err := parseRule(m)
				if err != nil {
					return fmt.Errorf("reading a policy rule: %w", err)
				}
				out = append(out, r)
			}
			return nil
		}
		return err
	})
	return out, err
}

// parseRule reads a rule message: a fib_rule_hdr, whose action sits where
// the type of a route message does, and its attributes. A rule that matches
// on more than a mark, a source address and a destination prefix, or does
// more than look a table up or be unreachable, gets the mark -1.
func parseRule(m []byte) (rule, error) {
	if len(m) < unix.SizeofRtMsg {
		return rule{}, errors.New("message too short")
	}
	hdr := nl.DeserializeRtMsg(m)
	attrs, err := nl.ParseRouteAttr(m[hdr.Len():])
	if err != nil {
		return rule{}, err
	}
	r := rule{table: int(hdr.Table)}
	other := hdr.Family != unix.AF_INET || hdr.Tos != 0 || hdr.Flags != 0
	switch hdr.Type {
	case unix.FR_ACT_TO_TBL:
	case unix.FR_ACT_UNREACHABLE:
		r.unreachable = true
	default:
		other = true
	}
	for _, a := range attrs {
		switch a.Attr.Type {
		case nl.FRA_PRIORITY:
			r.pref = int(native32(a.Value))
		case nl.FRA_TABLE:
			r.table = int(native32(a.Value))
		case nl.FRA_FWMARK:
			r.mark = int(native32(a.Value))
		case nl.FRA_FWMASK:
			other = other || native32(a.Value) != ^uint32(0)
		case nl.FRA_SRC:
			addr, ok := netip.AddrFromSlice(a.Value)
			if !ok || int(hdr.Src_len) != addr.BitLen() {
				other = true
			}
			r.src = addr.Unmap()
		case nl.FRA_DST:
			addr, ok := netip.AddrFromSlice(a.Value)
			if !ok {
				other = true
			}
			r.dst = netip.PrefixFrom(addr.Unmap(), int(hdr.Dst_len))
		case nl.FRA_PROTOCOL:
		case nl.FRA_SUPPRESS_PREFIXLEN, nl.FRA_SUPPRESS_IFGROUP:
			// Unset, the kernel lists them as -1.
			other = other || native32(a.Value) != ^uint32(0)
		default:
			other = true
		}
	}
	if r.unreachable {
		r.table = 0
	}
	if other {
		r.mark = -1
	}
	return r, nil
}

func native32(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	return nl.NativeEndian().Uint32(b[:4])
}
