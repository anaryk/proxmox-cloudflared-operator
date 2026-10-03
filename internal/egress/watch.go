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
type Pin struct {
	MAC    net.HardwareAddr
	Bridge string
	Port   string
	Own    []OwnMAC
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
	mac    net.HardwareAddr
	bridge string // of a forwarding table entry: the bridge, and the port that learned the MAC
	port   string
}

// moved returns, sorted, the bound addresses e shows moved: the neighbour
// table gives one of them a MAC its guest does not have, or the bridge of a
// pin learned one of the guest's MACs on another port than its own.
func (e entry) moved(pins map[netip.Addr]Pin) []netip.Addr {
	if len(e.mac) == 0 {
		return nil
	}
	var out []netip.Addr
	if e.addr.IsValid() {
		p, ok := pins[e.addr]
		if ok && !bytes.Equal(p.MAC, e.mac) && !slices.ContainsFunc(p.Own, func(o OwnMAC) bool { return bytes.Equal(o.MAC, e.mac) }) {
			out = append(out, e.addr)
		}
		return out
	}
	for addr, p := range pins {
		if p.Bridge == e.bridge && p.learnedElsewhere(e) {
			out = append(out, addr)
		}
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return out
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
