package resolve

import (
	"net/netip"
	"slices"
)

// Denylist holds the addresses that must never be published, whatever a
// route or a guest says. A guest controls its own configuration and what its
// agent reports, so without this it could claim the address of the
// hypervisor and have the Proxmox web interface published.
type Denylist struct {
	nodes    map[netip.Addr]struct{}
	prefixes []netip.Prefix
}

// NewDenylist denies the addresses of the cluster nodes, matched exactly, and
// every prefix in extra on top of the built-in ranges. Its arguments are
// copied.
func NewDenylist(nodeAddrs []netip.Addr, extra []netip.Prefix) Denylist {
	d := Denylist{nodes: make(map[netip.Addr]struct{}, len(nodeAddrs))}
	for _, a := range nodeAddrs {
		if a.Is4() {
			d.nodes[a] = struct{}{}
		}
	}
	d.prefixes = slices.DeleteFunc(slices.Clone(extra), func(p netip.Prefix) bool {
		return !p.IsValid()
	})
	return d
}

// Check returns a reason when addr must never be published. Only IPv4
// addresses can be published, so anything else is denied too.
func (d Denylist) Check(addr netip.Addr) (reason string, denied bool) {
	switch {
	case !addr.Is4():
		return "not an IPv4 address", true
	case addr.IsLoopback():
		return "loopback address", true
	case addr.IsLinkLocalUnicast():
		return "link-local address", true
	case addr.IsMulticast():
		return "multicast address", true
	case addr.IsUnspecified():
		return "unspecified address", true
	case addr == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return "broadcast address", true
	}
	if _, ok := d.nodes[addr]; ok {
		return "address of a cluster node", true
	}
	for _, p := range d.prefixes {
		if p.Contains(addr) {
			return "reserved by pco", true
		}
	}
	return "", false
}
