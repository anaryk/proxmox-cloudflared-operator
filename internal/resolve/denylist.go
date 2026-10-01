package resolve

import (
	"fmt"
	"net/netip"
)

const reasonClusterNode = "address of a cluster node"

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
// copied. A node address that is not IPv4 is ignored, but a prefix in extra
// that is neither IPv4 nor IPv4-mapped of /96 or longer is an error naming
// it, so that callers can validate their settings with it.
func NewDenylist(nodeAddrs []netip.Addr, extra []netip.Prefix) (Denylist, error) {
	d := Denylist{nodes: make(map[netip.Addr]struct{}, len(nodeAddrs))}
	for _, a := range nodeAddrs {
		if a = a.Unmap(); a.Is4() {
			d.nodes[a] = struct{}{}
		}
	}
	for _, p := range extra {
		v4, err := unmapPrefix(p)
		if err != nil {
			return Denylist{}, err
		}
		d.prefixes = append(d.prefixes, v4)
	}
	return d, nil
}

// unmapPrefix returns p as the IPv4 prefix it covers. An IPv4-mapped IPv6
// prefix shorter than /96 reaches beyond the mapped range, and any other
// prefix that is not IPv4 covers no IPv4 address at all.
func unmapPrefix(p netip.Prefix) (netip.Prefix, error) {
	switch {
	case p.IsValid() && p.Addr().Is4():
		return p, nil
	case !p.IsValid() || !p.Addr().Is4In6():
		return netip.Prefix{}, fmt.Errorf("deny prefix %s is not an IPv4 range", p)
	case p.Bits() < 96:
		return netip.Prefix{}, fmt.Errorf("deny prefix %s reaches beyond the IPv4-mapped range", p)
	}
	return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96), nil
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
		return reasonClusterNode, true
	}
	for _, p := range d.prefixes {
		if p.Contains(addr) {
			return "reserved by pco", true
		}
	}
	return "", false
}
