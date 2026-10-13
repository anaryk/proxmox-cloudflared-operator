package planner

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
)

// The kinds of placement: a hostname is served through the shared tunnel of
// its account, which every replica runs, or through the tunnel pinned to one
// node, which that node's agent alone runs.
const (
	PlacementShared = "shared"
	PlacementPinned = "pinned"
)

// PlacementInput says which tunnel of its account serves a hostname. A
// hostname without one, or with Kind empty, is shared.
type PlacementInput struct {
	Kind string `json:"kind"` // shared or pinned
	Node string `json:"node,omitempty"`
}

// tunnelKey names one tunnel of an account in the plan.
type tunnelKey struct {
	account string
	name    string
	node    string // empty for the shared tunnel
}

func compareTunnelKeys(a, b tunnelKey) int {
	return cmp.Or(strings.Compare(a.account, b.account), strings.Compare(a.name, b.name))
}

// tunnelOf returns the tunnel that serves host in the account of zone, by
// its placement. A placement that names no tunnel this install can have is
// an error, which says why.
func (b *builder) tunnelOf(zone Zone, host string) (tunnelKey, error) {
	p := b.in.Placements[host]
	switch p.Kind {
	case "", PlacementShared:
		return tunnelKey{account: zone.AccountID, name: TunnelName(b.in.Writer.InstallID)}, nil
	case PlacementPinned:
		name, err := PinnedTunnelName(b.in.Writer.InstallID, p.Node)
		if err != nil {
			return tunnelKey{}, fmt.Errorf("cannot be pinned to node %q: %w", p.Node, err)
		}
		return tunnelKey{account: zone.AccountID, name: name, node: p.Node}, nil
	}
	return tunnelKey{}, fmt.Errorf("placement kind %q is not known", p.Kind)
}

// tunnels returns one plan per tunnel that serves a rule or has a record,
// by account and name, so the shared tunnel of an account comes before the
// pinned ones. Each carries the blocks of its account: a wildcard of another
// owner may be placed in any tunnel of the account, and the block must come
// before it wherever it is.
func (b *builder) tunnels() []TunnelPlan {
	out := []TunnelPlan{}
	for _, key := range slices.SortedFunc(maps.Keys(b.live), compareTunnelKeys) {
		rules := slices.Concat(b.rules[key], b.blocks[key.account])
		slices.SortStableFunc(rules, func(x, y IngressRule) int { return hostname.Compare(x.Hostname, y.Hostname) })
		rules = append(rules, SentinelRule(b.in.Writer), CatchAllRule())
		tp := TunnelPlan{
			AccountID:    key.account,
			CredentialID: b.zones.credential[key.account],
			Name:         key.name,
			Rules:        rules,
		}
		if key.node != "" {
			tp.Kind, tp.Node = PlacementPinned, key.node
		}
		out = append(out, tp)
	}
	return out
}
