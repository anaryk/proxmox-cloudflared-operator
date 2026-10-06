package store

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"time"
)

// The kinds of Placement.
const (
	PlacementShared = "shared" // in the install's tunnel, served by every replica
	PlacementPinned = "pinned" // in the tunnel of one node, served by that node alone
)

// Placement is where the route of a hostname is served: in which tunnel, at
// which address, and which node resolves it. A new controller keeps the
// placements the last one made rather than move every route at a takeover.
type Placement struct {
	Hostname string `json:"hostname"`
	// Tunnel is the name of the tunnel, and Kind whether it is the shared
	// one or one pinned to Node.
	Tunnel string `json:"tunnel"`
	Kind   string `json:"kind"`
	Node   string `json:"node,omitempty"`
	// Addr is the target's address, and Service its service address when it
	// is reached through a leg.
	Addr          netip.Addr `json:"addr,omitzero"`
	Service       netip.Addr `json:"service,omitzero"`
	ResolvingNode string     `json:"resolvingNode,omitempty"`
	Since         time.Time  `json:"since"`
}

func (p Placement) check() error {
	switch {
	case p.Tunnel == "":
		return fmt.Errorf("placement of %s names no tunnel", p.Hostname)
	case p.Kind != PlacementShared && p.Kind != PlacementPinned:
		return fmt.Errorf("placement of %s: kind %q: want %q or %q", p.Hostname, p.Kind, PlacementShared, PlacementPinned)
	case p.Kind == PlacementPinned && p.Node == "":
		return fmt.Errorf("placement of %s is pinned to no node", p.Hostname)
	case p.Kind == PlacementShared && p.Node != "":
		return fmt.Errorf("placement of %s is shared and names node %s", p.Hostname, p.Node)
	case p.Since.IsZero():
		return fmt.Errorf("placement of %s has no time it is placed since", p.Hostname)
	}
	return nil
}

// Placements returns the placements by hostname.
func (s *Store) Placements() (map[string]Placement, error) {
	return loadMap(s.cluster, kindPlacements, "hostname", func(p *Placement) *string { return &p.Hostname })
}

// SavePlacements makes next the stored placements: it writes the ones that
// changed and deletes the ones that are gone. A placement that is not valid
// is refused before anything is written.
func (s *Store) SavePlacements(next map[string]Placement) error {
	for _, host := range slices.Sorted(maps.Keys(next)) {
		p := next[host]
		if p.Hostname == "" {
			p.Hostname = host
		}
		if err := p.check(); err != nil {
			return err
		}
	}
	return saveMap(s.cluster, kindPlacements, next, func(p *Placement) *string { return &p.Hostname })
}
