package store

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// The kinds of Network.
const (
	NetworkAttach  = "attach"
	NetworkManaged = "managed"
)

const (
	// managedVnet is the vnet of the managed network, and the id of its
	// network.
	managedVnet = "pconet"

	// The MAC of the namespace end of a leg. An attach leg carries the index
	// of its node in the fifth byte and its mark in the last; the managed leg
	// has one MAC, so that its guests see the same gateway on every node.
	nodeLegMAC    = "02:50:43:4f:%02x:%02x"
	nodeLegPrefix = "02:50:43:4f:"
	managedLegMAC = "02:50:43:4f:ff:01"
)

// The scopes of Network.
const (
	ScopeCluster = "cluster" // one segment on every node with a leg: an uplinked bridge, a VXLAN or EVPN vnet
	ScopeNode    = "node"    // a segment of each node: an isolated bridge, a vnet of a Simple zone
)

// Network is a network pco reaches through a leg. ID is "<bridge>",
// "<bridge>.<vlan>" or "pconet".
type Network struct {
	ID        string                `json:"id"`
	Kind      string                `json:"kind"`   // attach or managed
	Bridge    string                `json:"bridge"` // "pconet" for managed
	VLAN      int                   `json:"vlan,omitempty"`
	Prefix    netip.Prefix          `json:"prefix"`              // the guest network
	Legs      map[string]netip.Addr `json:"legs"`                // by node: the leg address
	LegMAC    string                `json:"legMAC"`              // the namespace end; fixed for managed, per network for attach
	Mark      int                   `json:"mark"`                // 1..gateway.MaxMark, unique
	Uplink    bool                  `json:"uplink,omitempty"`    // bridge with a physical uplink, confirmed
	OverlapOK bool                  `json:"overlapOK,omitempty"` // attached over a node route with --overlap-ok
	CreatedAt time.Time             `json:"createdAt"`
	// LegMACs are the MACs of the legs of an attached network by node, each
	// with the index of its node: on an uplinked bridge every node's leg is
	// seen on one LAN. Saved with the indexes of the registry, every leg has
	// one. They sit beside Legs, which the previous release reads as it was,
	// and which it writes without them: LegMACOn makes them again from the
	// indexes. The managed network has one MAC on every node and none here.
	LegMACs map[string]string `json:"legMACs,omitempty"`
	// Scope is ScopeCluster or ScopeNode; a network stored before scopes
	// has none, which is ScopeCluster.
	Scope string `json:"scope,omitempty"`
	// Rev is the revision of the network's file, for compare-and-swap. The
	// envelope holds it, not the network.
	Rev int64 `json:"rev,omitempty"`
}

// networkLimit says how many networks a node reaches and why: the SNAT through
// each leg takes a block of source ports of its own, and the blocks have to
// end below port 65536.
func networkLimit() string {
	_, last := gateway.PortBlock(gateway.MaxMark)
	return fmt.Sprintf("pco reaches at most %d networks: the SNAT through each takes a block of %d source ports, "+
		"and %d blocks end at port %d, where one more would pass 65535",
		gateway.MaxMark, gateway.PortBlockSize, gateway.MaxMark, last)
}

// ScopeName returns the scope of the network, never an empty one.
func (n Network) ScopeName() string {
	if n.Scope == "" {
		return ScopeCluster
	}
	return n.Scope
}

// LegMACOn returns the MAC of the leg of the network on a node: the managed
// network's one MAC; else the one the index the registry gave the node makes,
// in indexes by node name; else the one LegMACs holds for the node. A node of
// neither has no MAC known, which is an error and never the MAC of node 0's
// leg.
func (n Network) LegMACOn(node string, indexes map[string]int) (string, error) {
	if n.Kind == NetworkManaged {
		return managedLegMAC, nil
	}
	if index, ok := indexes[node]; ok {
		return NodeLegMAC(index, n.Mark), nil
	}
	if mac, ok := n.LegMACs[node]; ok {
		return mac, nil
	}
	return "", fmt.Errorf("network %s: node %s has no index, so the MAC of its leg is not known", n.ID, node)
}

// LegIndexes returns the indexes the leg MACs of the nodes carry, by node: a
// registry entry that lost its index finds it here again.
func (n Network) LegIndexes() map[string]int {
	out := make(map[string]int, len(n.LegMACs))
	for node, mac := range n.LegMACs {
		if index, _, err := parseNodeLegMAC(mac); err == nil {
			out[node] = index
		}
	}
	return out
}

// LegMACFor returns the MAC of the namespace end of the leg of a network of
// kind with mark, on the node of index 0.
func LegMACFor(kind string, mark int) string {
	if kind == NetworkManaged {
		return managedLegMAC
	}
	return NodeLegMAC(0, mark)
}

// NodeLegMAC returns the MAC of the namespace end of the leg of an attached
// network with mark on the node of index. The node registered before a
// cluster has index 0, so its legs keep the MACs they had.
func NodeLegMAC(index, mark int) string { return fmt.Sprintf(nodeLegMAC, index, mark) }

// checked returns n with its leg MACs in normal form, or refuses it. The leg
// MAC of a node is checked against the index the registry gave the node, in
// indexes, and that of a node without one against the form of a node's leg
// MAC alone.
func (n Network) checked(indexes map[string]int) (Network, error) {
	if err := n.checkID(); err != nil {
		return n, err
	}
	switch {
	case n.Mark > gateway.MaxMark:
		return n, fmt.Errorf("network %s: mark %d is not 1 to %d: %s", n.ID, n.Mark, gateway.MaxMark, networkLimit())
	case n.Mark < 1:
		return n, fmt.Errorf("network %s: mark %d is not 1 to %d", n.ID, n.Mark, gateway.MaxMark)
	case n.CreatedAt.IsZero():
		return n, fmt.Errorf("network %s has no time it was made at", n.ID)
	}
	if err := n.checkPrefix(); err != nil {
		return n, err
	}
	if err := n.checkLegs(); err != nil {
		return n, err
	}
	mac, err := model.NormalizeMAC(n.LegMAC)
	if err != nil {
		return n, fmt.Errorf("network %s: leg MAC: %w", n.ID, err)
	}
	if want := LegMACFor(n.Kind, n.Mark); mac != want {
		return n, fmt.Errorf("network %s: the leg MAC is %s, want %s", n.ID, mac, want)
	}
	n.LegMAC = mac
	switch n.Scope {
	case "", ScopeCluster, ScopeNode:
	default:
		return n, fmt.Errorf("network %s: scope %q: want %q or %q", n.ID, n.Scope, ScopeCluster, ScopeNode)
	}
	macs, err := n.checkLegMACs(indexes)
	if err != nil {
		return n, err
	}
	n.LegMACs = macs
	return n, nil
}

// checkLegMACs returns the leg MACs of the nodes in normal form, or refuses
// one of a node without a leg, one that is not the MAC of the node's leg, and
// two legs with one MAC. With indexes, the store makes the MAC of every leg
// whose node has one from the index and the mark, and a MAC given for it must
// be that one; a leg of a node without an index needs a MAC given, which is
// checked by its form. Without indexes, as before a cluster, a leg without a
// MAC of its own has the network's.
func (n Network) checkLegMACs(indexes map[string]int) (map[string]string, error) {
	given := make(map[string]string, len(n.LegMACs))
	for _, node := range slices.Sorted(maps.Keys(n.LegMACs)) {
		if _, ok := n.Legs[node]; !ok {
			return nil, fmt.Errorf("network %s: a leg MAC of node %s, which has no leg", n.ID, node)
		}
		mac, err := model.NormalizeMAC(n.LegMACs[node])
		if err != nil {
			return nil, fmt.Errorf("network %s: leg MAC of node %s: %w", n.ID, node, err)
		}
		given[node] = mac
	}
	if n.Kind == NetworkManaged {
		for _, node := range slices.Sorted(maps.Keys(given)) {
			if mac := given[node]; mac != managedLegMAC {
				return nil, fmt.Errorf("network %s: the leg MAC of node %s is %s, want %s: the managed leg has one MAC on every node", n.ID, node, mac, managedLegMAC)
			}
		}
		return nil, nil
	}
	out := make(map[string]string, len(n.Legs))
	owner := make(map[string]string, len(n.Legs)) // MAC -> node
	for _, node := range slices.Sorted(maps.Keys(n.Legs)) {
		mac, has := given[node]
		switch index, known := indexes[node]; {
		case known:
			want := NodeLegMAC(index, n.Mark)
			if has && mac != want {
				return nil, fmt.Errorf("network %s: the leg MAC of node %s is %s, want %s", n.ID, node, mac, want)
			}
			mac = want
			out[node] = mac
		case has:
			if err := checkNodeLegMAC(mac, n.Mark); err != nil {
				return nil, fmt.Errorf("network %s: the leg MAC of node %s: %w", n.ID, node, err)
			}
			out[node] = mac
		case indexes != nil:
			return nil, fmt.Errorf("network %s: node %s has no index yet, so its leg needs a MAC of its own", n.ID, node)
		default:
			mac = n.LegMAC
		}
		if other, taken := owner[mac]; taken {
			return nil, fmt.Errorf("network %s: the legs of nodes %s and %s have the same MAC %s", n.ID, other, node, mac)
		}
		owner[mac] = node
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// parseNodeLegMAC reads the index and the mark of a leg MAC in normal form.
func parseNodeLegMAC(mac string) (index, mark int, err error) {
	rest, ok := strings.CutPrefix(mac, nodeLegPrefix)
	if !ok {
		return 0, 0, fmt.Errorf("%s does not start with %s", mac, nodeLegPrefix)
	}
	if _, err := fmt.Sscanf(rest, "%02x:%02x", &index, &mark); err != nil {
		return 0, 0, fmt.Errorf("%s is no leg MAC", mac)
	}
	return index, mark, nil
}

// checkNodeLegMAC checks a leg MAC in normal form by its form: pco's prefix,
// the index of a node and the mark.
func checkNodeLegMAC(mac string, mark int) error {
	index, got, err := parseNodeLegMAC(mac)
	switch {
	case err != nil:
		return err
	case index > maxNodeIndex:
		return fmt.Errorf("%s: index %d is not one of a node, 0 to %d", mac, index, maxNodeIndex)
	case got != mark:
		return fmt.Errorf("%s: the leg MAC carries mark %d, not %d", mac, got, mark)
	}
	return nil
}

// checkID checks the kind, the bridge and the VLAN of a network against its
// id.
func (n Network) checkID() error {
	switch {
	case n.Bridge == "":
		return fmt.Errorf("network %q has no bridge", n.ID)
	case strings.IndexFunc(n.Bridge, func(r rune) bool { return !isBridgeChar(r) }) >= 0:
		return fmt.Errorf("network %q: bridge %q: want letters, digits, - and _", n.ID, n.Bridge)
	case n.VLAN < 0 || n.VLAN > maxVLAN:
		return fmt.Errorf("network %q: VLAN %d is not 0 to %d", n.ID, n.VLAN, maxVLAN)
	}
	switch n.Kind {
	case NetworkAttach:
		if n.Bridge == managedVnet {
			return fmt.Errorf("network %q: %s is the managed network, which is not attached", n.ID, managedVnet)
		}
		if want := SegmentID(n.Bridge, n.VLAN); n.ID != want {
			return fmt.Errorf("network %q: an attach to bridge %s and VLAN %d has the id %q", n.ID, n.Bridge, n.VLAN, want)
		}
	case NetworkManaged:
		if n.ID != managedVnet || n.Bridge != managedVnet || n.VLAN != 0 {
			return fmt.Errorf("network %q: the managed network is %s, on the untagged vnet %s", n.ID, managedVnet, managedVnet)
		}
	default:
		return fmt.Errorf("network %q: kind %q: want %q or %q", n.ID, n.Kind, NetworkAttach, NetworkManaged)
	}
	return nil
}

// checkPrefix refuses a guest network that is no IPv4 prefix written as its
// network address, or that overlaps the service prefix, whose addresses the
// connectors dial.
func (n Network) checkPrefix() error {
	p, service := n.Prefix, gateway.ServicePrefix
	switch {
	case !p.IsValid() || !p.Addr().Is4():
		return fmt.Errorf("network %s: prefix %s: want an IPv4 prefix", n.ID, p)
	case p != p.Masked():
		return fmt.Errorf("network %s: prefix %s: want the address of the network, %s", n.ID, p, p.Masked())
	case p.Overlaps(service):
		return fmt.Errorf("network %s: prefix %s overlaps the service prefix %s", n.ID, p, service)
	}
	return nil
}

// checkLegs refuses a leg address outside the network, one that is its
// network or broadcast address, and, on the managed network, any but its
// first.
func (n Network) checkLegs() error {
	p := n.Prefix
	for _, node := range slices.Sorted(maps.Keys(n.Legs)) {
		addr := n.Legs[node]
		switch {
		case node == "":
			return fmt.Errorf("network %s: a leg has no node", n.ID)
		case !addr.Is4() || !p.Contains(addr):
			return fmt.Errorf("network %s: the leg address %v of node %s is not in %s", n.ID, addr, node, p)
		case p.Bits() <= 30 && (addr == p.Addr() || addr == lastAddr(p)):
			return fmt.Errorf("network %s: the leg address %s of node %s is the network or the broadcast address", n.ID, addr, node)
		case n.Kind == NetworkManaged && addr != p.Addr().Next():
			return fmt.Errorf("network %s: the leg address of node %s is %s, not %s", n.ID, node, p.Addr().Next(), addr)
		}
	}
	return nil
}

// Networks returns the networks by id, each with the revision of its file.
func (s *Store) Networks() (map[string]Network, error) {
	out := make(map[string]Network)
	err := eachRevision(s.cluster, kindNetworks, func(path, id string, rev int64, n Network) error {
		if n.ID != id {
			return fmt.Errorf("%s holds the network %q, which is stored as %q", path, n.ID, id)
		}
		n.Rev = rev
		out[id] = n
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SaveNetwork stores a network read at revision rev, 0 for a new one, while
// its file still has that revision; otherwise the error is ErrRevision and
// nothing is written. A network beyond the most that have a block of source
// ports is refused, so is a mark that another network holds, and so is a leg
// MAC that is not the one of the network's kind and mark.
func (s *Store) SaveNetwork(n Network, rev int64) error {
	return s.SaveNetworkIndexed(n, rev, nil)
}

// SaveNetworkIndexed is SaveNetwork that checks the leg MAC of each node
// against the index the registry gave the node, in indexes by node name.
func (s *Store) SaveNetworkIndexed(n Network, rev int64, indexes map[string]int) error {
	n, err := n.checked(indexes)
	if err != nil {
		return err
	}
	s.netMu.Lock()
	defer s.netMu.Unlock()
	have, err := s.Networks()
	if err != nil {
		return err
	}
	if _, saved := have[n.ID]; !saved && len(have) >= gateway.MaxMark {
		return fmt.Errorf("network %s: %s", n.ID, networkLimit())
	}
	for _, id := range slices.Sorted(maps.Keys(have)) {
		if id != n.ID && have[id].Mark == n.Mark {
			return fmt.Errorf("network %s: mark %d is the mark of network %s", n.ID, n.Mark, id)
		}
	}
	n.Rev = 0
	_, err = s.cluster.putIf(kindNetworks, n.ID, n, rev)
	return err
}

// DeleteNetwork removes a network while its file has revision rev; otherwise
// the error is ErrRevision, also for one that is not there.
func (s *Store) DeleteNetwork(id string, rev int64) error {
	return s.cluster.deleteIf(kindNetworks, id, rev)
}
