package egress

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
)

// ReadCounters reads the table listing (Nft.List, one bounded call) and
// returns the table's handle and the counting sets' elements with their
// counters. ErrNotLoaded without the table; ErrUnreadable for a listing whose
// counting sets are missing, have no counters, or cannot be read. A target of
// allowNode is marked as one, as the allownode sets of the same listing hold
// it.
func ReadCounters(ctx context.Context, n Nft) (table uint64, flows []TargetFlows, err error) {
	l, err := list(ctx, n)
	if err != nil {
		return 0, nil, err
	}
	for _, name := range []string{setFlows4, setFlows6} {
		s, ok := l.sets[name]
		switch {
		case !ok:
			return 0, nil, fmt.Errorf("%w: %w: no set %s", ErrUnreadable, ErrNoCounters, name)
		case !s.counts():
			return 0, nil, fmt.Errorf("%w: set %s has no counters", ErrUnreadable, name)
		}
	}
	flows, unreadable, uncounted := l.flows()
	if bad := slices.Concat(unreadable, uncounted); len(bad) > 0 {
		return 0, nil, fmt.Errorf("%w: %s", ErrUnreadable, bad[0])
	}
	if l.handle == 0 {
		return 0, nil, fmt.Errorf("%w: the table has no handle", ErrUnreadable)
	}
	c, _ := l.contents()
	allowNode := map[netip.AddrPort]bool{}
	for _, t := range c.targets {
		if t.AllowNode {
			allowNode[netip.AddrPortFrom(t.Addr, t.Port)] = true
		}
	}
	for i, f := range flows {
		flows[i].Target.AllowNode = allowNode[netip.AddrPortFrom(f.Target.Addr, f.Target.Port)]
	}
	return l.handle, flows, nil
}
