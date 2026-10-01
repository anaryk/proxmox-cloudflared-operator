package resolve

import "net/netip"

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
		if a = a.Unmap(); a.Is4() {
			d.nodes[a] = struct{}{}
		}
	}
	for _, p := range extra {
		if p = unmapPrefix(p); p.IsValid() && p.Addr().Is4() {
			d.prefixes = append(d.prefixes, p)
		}
	}
	return d
}

// unmapPrefix returns an IPv4-mapped IPv6 prefix as the IPv4 prefix it
// covers. One shorter than /96 reaches beyond the mapped range and is no
// IPv4 prefix at all.
func unmapPrefix(p netip.Prefix) netip.Prefix {
	if !p.IsValid() || !p.Addr().Is4In6() {
		return p
	}
	if p.Bits() < 96 {
		return netip.Prefix{}
	}
	return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
}

// withoutNodes returns d without the addresses of the cluster nodes, for a
// route an admin points at a node on purpose. Everything else stays denied.
func (d Denylist) withoutNodes() Denylist {
	return Denylist{prefixes: d.prefixes}
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
