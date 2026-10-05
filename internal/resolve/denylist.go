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
//
// Besides, it holds addresses that are only soft-denied: the gateways and
// resolvers of nodes and appliances. Those are served as any other, but a
// guest is published at one only with the admin's allowance, which is the
// engine's to check.
type Denylist struct {
	nodes    map[netip.Addr]struct{}
	prefixes []netip.Prefix
	denied   map[netip.Addr]string // denied with a reason of their own
	soft     map[netip.Addr]string
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

// WithDenied returns a copy of d that also denies the addresses of entries,
// each with its reason, as the gateway of an SDN subnet. An address that is
// not IPv4 is ignored.
func (d Denylist) WithDenied(entries map[netip.Addr]string) Denylist {
	d.denied = withEntries(d.denied, entries)
	return d
}

// WithSoft returns a copy of d that soft-denies the addresses of entries,
// each with why it needs the admin's allowance. An address that is not IPv4
// is ignored.
func (d Denylist) WithSoft(entries map[netip.Addr]string) Denylist {
	d.soft = withEntries(d.soft, entries)
	return d
}

// Soft says why addr needs the admin's allowance, when it is soft-denied.
func (d Denylist) Soft(addr netip.Addr) (reason string, soft bool) {
	reason, soft = d.soft[addr.Unmap()]
	return reason, soft
}

// withEntries returns a new map with the entries of both, those of add
// replacing those of base, keyed by their IPv4 address.
func withEntries(base, add map[netip.Addr]string) map[netip.Addr]string {
	out := make(map[netip.Addr]string, len(base)+len(add))
	for a, why := range base {
		out[a] = why
	}
	for a, why := range add {
		if a = a.Unmap(); a.Is4() {
			out[a] = why
		}
	}
	return out
}

// withoutNodes returns d without the addresses of the cluster nodes, for a
// route an admin points at a node on purpose. Everything else stays denied.
func (d Denylist) withoutNodes() Denylist {
	return Denylist{prefixes: d.prefixes, denied: d.denied, soft: d.soft}
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
	if why, ok := d.denied[addr]; ok {
		return why, true
	}
	for _, p := range d.prefixes {
		if p.Contains(addr) {
			return "reserved by pco", true
		}
	}
	return "", false
}
