package store

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appnet"
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

	maxMark = 250

	// The MAC of the namespace end of a leg. An attach leg carries its mark
	// in the last byte; the managed leg has one MAC, so that its guests see
	// the same gateway on every node.
	attachLegMAC  = "02:50:43:4f:00:%02x"
	managedLegMAC = "02:50:43:4f:ff:01"
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
	Mark      int                   `json:"mark"`                // 1..250, unique
	Uplink    bool                  `json:"uplink,omitempty"`    // bridge with a physical uplink, confirmed
	OverlapOK bool                  `json:"overlapOK,omitempty"` // attached over a node route with --overlap-ok
	CreatedAt time.Time             `json:"createdAt"`
	// Rev is the revision of the network's file, for compare-and-swap. The
	// envelope holds it, not the network.
	Rev int64 `json:"rev,omitempty"`
}

// LegMACFor returns the MAC of the namespace end of the leg of a network of
// kind with mark.
func LegMACFor(kind string, mark int) string {
	if kind == NetworkManaged {
		return managedLegMAC
	}
	return fmt.Sprintf(attachLegMAC, mark)
}

// checked returns n with its leg MAC in normal form, or refuses it.
func (n Network) checked() (Network, error) {
	if err := n.checkID(); err != nil {
		return n, err
	}
	switch {
	case n.Mark < 1 || n.Mark > maxMark:
		return n, fmt.Errorf("network %s: mark %d is not 1 to %d", n.ID, n.Mark, maxMark)
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
		return n, fmt.Errorf("network %s: the leg MAC is %s, not %s", n.ID, want, mac)
	}
	n.LegMAC = mac
	return n, nil
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
	p, service := n.Prefix, appnet.ServicePrefix
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
// nothing is written. A mark that another network holds is refused, and so
// is a leg MAC that is not the one of the network's kind and mark.
func (s *Store) SaveNetwork(n Network, rev int64) error {
	n, err := n.checked()
	if err != nil {
		return err
	}
	s.netMu.Lock()
	defer s.netMu.Unlock()
	have, err := s.Networks()
	if err != nil {
		return err
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
