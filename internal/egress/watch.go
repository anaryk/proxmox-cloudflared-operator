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
	Own    []net.HardwareAddr
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
// pin learned the pinned MAC on another port than the pinned one.
func (e entry) moved(pins map[netip.Addr]Pin) []netip.Addr {
	if len(e.mac) == 0 {
		return nil
	}
	var out []netip.Addr
	if e.addr.IsValid() {
		p, ok := pins[e.addr]
		if ok && !bytes.Equal(p.MAC, e.mac) && !slices.ContainsFunc(p.Own, func(m net.HardwareAddr) bool { return bytes.Equal(m, e.mac) }) {
			out = append(out, e.addr)
		}
		return out
	}
	for addr, p := range pins {
		if p.Port != "" && p.Bridge == e.bridge && bytes.Equal(p.MAC, e.mac) && p.Port != e.port {
			out = append(out, addr)
		}
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return out
}
