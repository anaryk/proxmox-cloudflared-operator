//go:build linux

package gateway

import (
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// neighbours lists the permanent IPv4 neighbours of an interface of the
// namespace: the pins, which no ARP answer changes.
func (s *linuxSystem) neighbours(dev string) ([]neighbour, error) {
	var out []neighbour
	err := s.withLink(inNS, dev, func(h *netlink.Handle, l netlink.Link) error {
		list, err := h.NeighList(l.Attrs().Index, netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		for _, n := range list {
			a, ok := netip.AddrFromSlice(n.IP)
			if !ok || n.State&unix.NUD_PERMANENT == 0 {
				continue
			}
			out = append(out, neighbour{addr: a.Unmap(), mac: n.HardwareAddr})
		}
		return nil
	})
	return out, err
}

func (s *linuxSystem) setNeighbour(dev string, n neighbour) error {
	return s.withLink(inNS, dev, func(h *netlink.Handle, l netlink.Link) error {
		return h.NeighSet(&netlink.Neigh{
			LinkIndex: l.Attrs().Index, Family: netlink.FAMILY_V4, State: unix.NUD_PERMANENT,
			IP: n.addr.AsSlice(), HardwareAddr: n.mac,
		})
	})
}

func (s *linuxSystem) delNeighbour(dev string, addr netip.Addr) error {
	return s.withLink(inNS, dev, func(h *netlink.Handle, l netlink.Link) error {
		return h.NeighDel(&netlink.Neigh{LinkIndex: l.Attrs().Index, Family: netlink.FAMILY_V4, IP: addr.AsSlice()})
	})
}
