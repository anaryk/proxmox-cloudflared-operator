package gateway

import (
	"net"
	"slices"
)

// SourceWatch reads the source MACs of the frames that reach the managed
// leg. The leg's port is not isolated, so the broadcasts of every card on
// the managed network reach it, ARP and DHCP among them: a card no lease
// knows shows itself there as soon as it speaks, where the configuration of
// its guest is otherwise seen at the next full read of the inventory only.
type SourceWatch struct {
	// Known reports whether a MAC is a lease's; nil knows none, and then
	// nothing is reported, as nothing can be told.
	Known func(mac net.HardwareAddr) bool
	// Own are the leg's MACs, whose frames are pco's: the namespace end and,
	// with fe as its first octet, the bridge end of each.
	Own []net.HardwareAddr
	// Unknown is called with the source of every frame that is neither a
	// lease's nor the leg's own.
	Unknown func(mac net.HardwareAddr)
}

// ethHeaderLen is the length of an Ethernet header without a VLAN tag.
const ethHeaderLen = 14

// Frame looks at one frame that reached the leg.
func (w SourceWatch) Frame(f []byte) {
	if len(f) < ethHeaderLen || w.Known == nil || w.Unknown == nil {
		return
	}
	src := net.HardwareAddr(f[6:12])
	if src[0]&1 != 0 || isZero(src) || w.own(src) || w.Known(src) {
		return
	}
	w.Unknown(slices.Clone(src))
}

func (w SourceWatch) own(src net.HardwareAddr) bool {
	for _, m := range w.Own {
		if macEqual(m, src) || macEqual(bridgeEndMAC(m), src) {
			return true
		}
	}
	return false
}

func isZero(mac net.HardwareAddr) bool {
	return !slices.ContainsFunc(mac, func(b byte) bool { return b != 0 })
}
