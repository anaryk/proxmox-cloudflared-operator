package egress

import (
	"bytes"
	"net"
	"net/netip"
	"slices"
)

// Pin is what a bound address was verified with: the MAC that answered for
// it and, when the proof placed that MAC, the bridge whose forwarding table
// did and the port of the bridge it was learned on. Own are the other MACs of
// the same guest, which may answer for the address as well.
//
// A target on a leg of the gateway is bound under its service address, the
// address the egress set holds for it: Dev is then the leg its guest answers
// on and Real the guest's own address there. Two networks with the same
// subnet each have a guest at the same real address, so only the leg tells
// their neighbour entries apart. Both are empty for a target the node
// reaches directly.
type Pin struct {
	MAC    net.HardwareAddr
	Bridge string
	Port   string
	Own    []OwnMAC
	Dev    string
	Real   netip.Addr
}

// OwnMAC is another MAC of a bound guest, with the ports of the bridge of
// the pin it belongs on: the one the proof found it on, or else those of its
// NIC. One without ports is not watched in the forwarding table.
type OwnMAC struct {
	MAC   net.HardwareAddr
	Ports []string
}

// entry is a notification of the neighbour table of the node, or of the
// forwarding table of a bridge, as the watch reads it.
type entry struct {
	addr   netip.Addr // of a neighbour table entry; invalid for a forwarding table entry
	dev    string     // of a neighbour table entry: the device it is on, when its name is known
	mac    net.HardwareAddr
	bridge string // of a forwarding table entry: the bridge, and the port that learned the MAC
	port   string
	vlan   int  // of a forwarding table entry: the VLAN it is in, 0 on a bridge that has none
	gone   bool // of a forwarding table entry: the bridge dropped it
}

// moved returns, sorted, the bound addresses e shows moved: the neighbour
// table gives one of them a MAC its guest does not have, or the bridge of a
// pin learned one of the guest's MACs on another port than its own. A
// neighbour entry is matched against a direct target's pin by its address,
// whatever device it is on, and against a pin on a leg by the leg and the
// guest's real address there.
func (e entry) moved(pins map[netip.Addr]Pin) []netip.Addr {
	if len(e.mac) == 0 {
		return nil
	}
	var out []netip.Addr
	if e.addr.IsValid() {
		for addr, p := range pins {
			if p.matches(addr, e) && p.foreign(e.mac) {
				out = append(out, addr)
			}
		}
	} else {
		for addr, p := range pins {
			if p.Bridge == e.bridge && p.learnedElsewhere(e) {
				out = append(out, addr)
			}
		}
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return out
}

// matches reports whether the neighbour entry e is of the target pinned
// under addr.
func (p Pin) matches(addr netip.Addr, e entry) bool {
	if p.Dev == "" {
		return addr == e.addr
	}
	return p.Dev == e.dev && p.Real == e.addr
}

// foreign reports whether mac is no MAC of the pinned guest.
func (p Pin) foreign(mac net.HardwareAddr) bool {
	return !bytes.Equal(p.MAC, mac) && !slices.ContainsFunc(p.Own, func(o OwnMAC) bool { return bytes.Equal(o.MAC, mac) })
}

// learnedElsewhere reports whether an entry of the forwarding table of the
// pin's bridge puts one of the guest's MACs on a port it does not belong on.
func (p Pin) learnedElsewhere(e entry) bool {
	if p.Port != "" && bytes.Equal(p.MAC, e.mac) && p.Port != e.port {
		return true
	}
	return slices.ContainsFunc(p.Own, func(o OwnMAC) bool {
		return len(o.Ports) > 0 && bytes.Equal(o.MAC, e.mac) && !slices.Contains(o.Ports, e.port)
	})
}
