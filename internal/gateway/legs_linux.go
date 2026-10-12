//go:build linux

package gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// ntfSticky marks a forwarding entry that no frame from another port moves.
const ntfSticky = 0x40

func linkOf(l netlink.Link, names map[int]string) link {
	a := l.Attrs()
	out := link{
		name: a.Name, kind: l.Type(), mac: a.HardwareAddr, mtu: a.MTU,
		up: a.Flags&net.FlagUp != 0, carrier: a.RawFlags&unix.IFF_LOWER_UP != 0,
		master: names[a.MasterIndex],
	}
	if br, ok := l.(*netlink.Bridge); ok {
		if br.VlanFiltering != nil {
			out.vlanFiltering = *br.VlanFiltering
		}
		if br.VlanDefaultPVID != nil {
			out.defaultPVID = int(*br.VlanDefaultPVID)
		}
	}
	return out
}

func (s *linuxSystem) link(sd side, name string) (link, bool, error) {
	h, _, err := s.handles(sd)
	if err != nil {
		return link{}, false, err
	}
	l, err := h.LinkByName(name)
	var missing netlink.LinkNotFoundError
	switch {
	case errors.As(err, &missing):
		return link{}, false, nil
	case err != nil:
		return link{}, false, err
	}
	names := map[int]string{}
	if m := l.Attrs().MasterIndex; m != 0 {
		master, err := h.LinkByIndex(m)
		if err != nil {
			return link{}, false, err
		}
		names[m] = master.Attrs().Name
	}
	out := linkOf(l, names)
	if out.master != "" {
		if pi, err := h.LinkGetProtinfo(l); err == nil {
			out.isolated = pi.Isolated
		}
	}
	return out, true, nil
}

func (s *linuxSystem) links(sd side) ([]link, error) {
	h, _, err := s.handles(sd)
	if err != nil {
		return nil, err
	}
	list, err := h.LinkList()
	if err != nil {
		return nil, err
	}
	names := make(map[int]string, len(list))
	for _, l := range list {
		names[l.Attrs().Index] = l.Attrs().Name
	}
	out := make([]link, 0, len(list))
	for _, l := range list {
		out = append(out, linkOf(l, names))
	}
	return out, nil
}

func (s *linuxSystem) addVeth(hostName string, hostMAC net.HardwareAddr, nsName string, nsMAC net.HardwareAddr, mtu int) error {
	h, _, err := s.handles(onHost)
	if err != nil {
		return err
	}
	ns, err := s.nsOf(inNS)
	if err != nil {
		return err
	}
	v := &netlink.Veth{
		LinkAttrs:        netlink.LinkAttrs{Name: hostName, HardwareAddr: hostMAC, MTU: mtu},
		PeerName:         nsName,
		PeerHardwareAddr: nsMAC,
		PeerNamespace:    netlink.NsFd(ns),
	}
	if mtu > 0 {
		v.PeerMTU = uint32(mtu)
	}
	return h.LinkAdd(v)
}

func (s *linuxSystem) withLink(sd side, name string, f func(h *netlink.Handle, l netlink.Link) error) error {
	h, _, err := s.handles(sd)
	if err != nil {
		return err
	}
	l, err := h.LinkByName(name)
	if err != nil {
		return err
	}
	return f(h, l)
}

func (s *linuxSystem) delLink(sd side, name string) error {
	return s.withLink(sd, name, func(h *netlink.Handle, l netlink.Link) error { return h.LinkDel(l) })
}

func (s *linuxSystem) setMTU(sd side, name string, mtu int) error {
	return s.withLink(sd, name, func(h *netlink.Handle, l netlink.Link) error { return h.LinkSetMTU(l, mtu) })
}

func (s *linuxSystem) setUp(sd side, name string) error {
	return s.withLink(sd, name, func(h *netlink.Handle, l netlink.Link) error { return h.LinkSetUp(l) })
}

func (s *linuxSystem) setMaster(name, master string) error {
	return s.withLink(onHost, name, func(h *netlink.Handle, l netlink.Link) error {
		m, err := h.LinkByName(master)
		if err != nil {
			return err
		}
		return h.LinkSetMaster(l, m)
	})
}

func (s *linuxSystem) setIsolated(name string, isolated bool) error {
	return s.withLink(onHost, name, func(h *netlink.Handle, l netlink.Link) error { return h.LinkSetIsolated(l, isolated) })
}

// flushZone removes the IPv4 conntrack entries of a zone of the namespace:
// the flows through the leg whose mark is the zone.
func (s *linuxSystem) flushZone(zone int) error {
	h, _, err := s.handles(inNS)
	if err != nil {
		return err
	}
	var f netlink.ConntrackFilter
	if err := f.AddZone(uint16(zone)); err != nil {
		return err
	}
	_, err = h.ConntrackDeleteFilters(netlink.ConntrackTable, unix.AF_INET, &f)
	return err
}

func (s *linuxSystem) portVLANs(port string) ([]portVLAN, error) {
	h, _, err := s.handles(onHost)
	if err != nil {
		return nil, err
	}
	l, err := h.LinkByName(port)
	if err != nil {
		return nil, err
	}
	all, err := h.BridgeVlanList()
	if err != nil {
		return nil, err
	}
	var out []portVLAN
	for _, v := range all[int32(l.Attrs().Index)] {
		out = append(out, portVLAN{
			vid:      int(v.Vid),
			pvid:     v.Flags&nl.BRIDGE_VLAN_INFO_PVID != 0,
			untagged: v.Flags&nl.BRIDGE_VLAN_INFO_UNTAGGED != 0,
		})
	}
	return out, nil
}

func (s *linuxSystem) addPortVLAN(port string, v portVLAN) error {
	return s.withLink(onHost, port, func(h *netlink.Handle, l netlink.Link) error {
		return h.BridgeVlanAdd(l, uint16(v.vid), v.pvid, v.untagged, false, false)
	})
}

func (s *linuxSystem) delPortVLAN(port string, vid int) error {
	return s.withLink(onHost, port, func(h *netlink.Handle, l netlink.Link) error {
		return h.BridgeVlanDel(l, uint16(vid), false, false, false, false)
	})
}

func (s *linuxSystem) fdb(port string) ([]fdbEntry, error) {
	h, _, err := s.handles(onHost)
	if err != nil {
		return nil, err
	}
	l, err := h.LinkByName(port)
	if err != nil {
		return nil, err
	}
	list, err := h.NeighList(l.Attrs().Index, unix.AF_BRIDGE)
	if err != nil {
		return nil, err
	}
	out := make([]fdbEntry, 0, len(list))
	for _, n := range list {
		out = append(out, fdbEntry{
			mac: n.HardwareAddr, vlan: n.Vlan,
			static: n.State&(unix.NUD_NOARP|unix.NUD_PERMANENT) != 0,
			sticky: n.Flags&ntfSticky != 0,
		})
	}
	return out, nil
}

func (s *linuxSystem) addSticky(port string, mac net.HardwareAddr, vlan int) error {
	return s.withLink(onHost, port, func(h *netlink.Handle, l netlink.Link) error {
		return h.NeighSet(&netlink.Neigh{
			LinkIndex: l.Attrs().Index, Family: unix.AF_BRIDGE, State: unix.NUD_NOARP,
			Flags: netlink.NTF_MASTER | ntfSticky, HardwareAddr: mac, Vlan: vlan,
		})
	})
}

func (s *linuxSystem) delFDB(port string, e fdbEntry) error {
	return s.withLink(onHost, port, func(h *netlink.Handle, l netlink.Link) error {
		return h.NeighDel(&netlink.Neigh{
			LinkIndex: l.Attrs().Index, Family: unix.AF_BRIDGE, Flags: netlink.NTF_MASTER,
			HardwareAddr: e.mac, Vlan: e.vlan,
		})
	})
}

const (
	etherTypeARP = 0x0806
	arpRequest   = 1
	arpReply     = 2
	arpFrameLen  = 42
)

// arpProbe sends three probes for addr, with the sender 0.0.0.0 that a host
// sends before it takes an address, through dev of the namespace, within
// the window, and returns who claims the address: whoever answers, announces
// it, or probes for it at the same time.
func (s *linuxSystem) arpProbe(ctx context.Context, dev string, addr netip.Addr, window time.Duration) ([]net.HardwareAddr, error) {
	return s.arpProbeOn(ctx, inNS, dev, addr, window)
}

func (s *linuxSystem) arpProbeOn(ctx context.Context, sd side, dev string, addr netip.Addr, window time.Duration) ([]net.HardwareAddr, error) {
	ns, err := s.nsOf(sd)
	if err != nil {
		return nil, err
	}
	l, ok, err := s.link(sd, dev)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, fmt.Errorf("%s is missing", dev)
	}
	f, err := Socket(ns, unix.AF_PACKET, unix.SOCK_RAW, HostToNet16(etherTypeARP), SocketOptions{Device: dev})
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	probe := arpProbeFrame(l.mac, addr)
	end := time.Now().Add(window)
	if d, ok := ctx.Deadline(); ok && d.Before(end) {
		end = d
	}
	var claimed []net.HardwareAddr
	buf := make([]byte, 128)
	for i := range 3 {
		if _, err := f.Write(probe); err != nil {
			return nil, fmt.Errorf("sending a probe through %s: %w", dev, err)
		}
		next := time.Now().Add(window / 3)
		if i == 2 || next.After(end) {
			next = end
		}
		if err := f.SetReadDeadline(next); err != nil {
			return nil, err
		}
		for {
			n, err := f.Read(buf)
			if errors.Is(err, os.ErrDeadlineExceeded) {
				break
			}
			if err != nil {
				return nil, err
			}
			if m, ok := claimant(buf[:n], l.mac, addr); ok {
				claimed = append(claimed, m)
			}
		}
	}
	return claimed, nil
}

func arpProbeFrame(own net.HardwareAddr, addr netip.Addr) []byte {
	tpa := addr.As4()
	f := make([]byte, 0, arpFrameLen)
	f = append(f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
	f = append(f, own...)
	f = binary.BigEndian.AppendUint16(f, etherTypeARP)
	f = binary.BigEndian.AppendUint16(f, 1)      // Ethernet
	f = binary.BigEndian.AppendUint16(f, 0x0800) // IPv4
	f = append(f, 6, 4)
	f = binary.BigEndian.AppendUint16(f, arpRequest)
	f = append(f, own...)
	f = append(f, 0, 0, 0, 0)
	f = append(f, make([]byte, 6)...)
	return append(f, tpa[:]...)
}

func (s *linuxSystem) announce(dev string, addr netip.Addr) error {
	return s.announceOn(inNS, dev, addr)
}

// announceOn sends one gratuitous ARP request for addr from the MAC of dev:
// a guest that has an entry for addr takes the MAC from it.
func (s *linuxSystem) announceOn(sd side, dev string, addr netip.Addr) error {
	ns, err := s.nsOf(sd)
	if err != nil {
		return err
	}
	l, ok, err := s.link(sd, dev)
	switch {
	case err != nil:
		return err
	case !ok:
		return fmt.Errorf("%s is missing", dev)
	}
	f, err := Socket(ns, unix.AF_PACKET, unix.SOCK_RAW, HostToNet16(etherTypeARP), SocketOptions{Device: dev})
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(arpAnnounceFrame(l.mac, addr)); err != nil {
		return fmt.Errorf("announcing %s through %s: %w", addr, dev, err)
	}
	return nil
}

// arpAnnounceFrame is a gratuitous ARP request: the sender and the target
// are both addr, the sender's MAC is own.
func arpAnnounceFrame(own net.HardwareAddr, addr netip.Addr) []byte {
	f := arpProbeFrame(own, addr)
	spa := addr.As4()
	copy(f[28:32], spa[:])
	return f
}

// claimant returns the sender of an ARP frame that claims addr: a reply or
// an announcement from it, or another host's probe for it.
func claimant(f []byte, own net.HardwareAddr, addr netip.Addr) (net.HardwareAddr, bool) {
	if len(f) < arpFrameLen || binary.BigEndian.Uint16(f[12:14]) != etherTypeARP {
		return nil, false
	}
	op := binary.BigEndian.Uint16(f[20:22])
	sha := net.HardwareAddr(f[22:28])
	spa := netip.AddrFrom4([4]byte(f[28:32]))
	tpa := netip.AddrFrom4([4]byte(f[38:42]))
	if macEqual(sha, own) || (op != arpRequest && op != arpReply) {
		return nil, false
	}
	if spa == addr || (op == arpRequest && spa.IsUnspecified() && tpa == addr) {
		return append(net.HardwareAddr(nil), sha...), true
	}
	return nil, false
}

// WatchLinks calls changed with the name of every link of the host that is
// added, changed or removed, until ctx ends: a bridge vmbr<X>v<tag> that
// appears, a port of a guest that appears or a bridge that ifreload made
// again is then set up by the next Sync. It returns an error when it cannot
// subscribe.
func WatchLinks(ctx context.Context, changed func(name string)) error {
	ch := make(chan netlink.LinkUpdate, 64)
	done := make(chan struct{})
	defer close(done)
	failed := make(chan error, 1)
	err := netlink.LinkSubscribeWithOptions(ch, done, netlink.LinkSubscribeOptions{
		ErrorCallback: func(err error) {
			select {
			case failed <- err:
			default:
			}
		},
	})
	if err != nil {
		return fmt.Errorf("watching the links: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failed:
			return fmt.Errorf("watching the links: %w", err)
		case u, ok := <-ch:
			if !ok {
				return errors.New("watching the links: the subscription ended")
			}
			if u.Link != nil {
				changed(u.Link.Attrs().Name)
			}
		}
	}
}
