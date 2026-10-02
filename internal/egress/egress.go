// Package egress keeps the nftables table inet pco_egress, which confines the
// cloudflared connectors: a process of the connector user may open
// connections to the targets pco verified, to the resolvers of the node on
// port 53 and to Cloudflare's edge, and to nothing else. Whoever can change a
// tunnel's configuration at Cloudflare can then point a connector at a
// verified origin, but not at the management ports of the node or at any
// other host it can reach.
package egress

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
)

var (
	// ErrOff is what Verify says while the admin has switched the filter off.
	ErrOff = errors.New("the egress filter is switched off")
	// ErrChanged is what Verify says when the live table is not the one last
	// applied, or is gone.
	ErrChanged = errors.New("the egress table is not the one pco applied")
)

// maxDifferences is how many differences an error of Verify names.
const maxDifferences = 8

// Target is one origin a connector may open connections to.
type Target struct {
	Addr netip.Addr
	Port uint16
}

func (t Target) String() string { return netip.AddrPortFrom(t.Addr, t.Port).String() }

func (t Target) compare(o Target) int {
	return cmp.Or(t.Addr.Compare(o.Addr), cmp.Compare(t.Port, o.Port))
}

// Filter keeps the table in line with the targets it is given. Its methods
// may be called from more than one goroutine.
type Filter struct {
	nft       Nft
	uid       uint32
	resolvers func() ([]netip.Addr, error)
	ov        *Overrides

	mu      sync.Mutex
	want    []Target  // as the last Set gave them, less what Remove took out since
	applied *contents // what the live table was last made to hold; nil when that is not known
	stale   bool      // apply at the next Set even when nothing changed
}

// New returns a filter for the processes of the given uid. It calls resolvers
// whenever it renders the table, and leaves out of every set the addresses
// the overrides block; while they say the filter is off, it loads nothing.
func New(n Nft, connectorUID uint32, resolvers func() ([]netip.Addr, error), ov *Overrides) *Filter {
	return &Filter{nft: n, uid: connectorUID, resolvers: resolvers, ov: ov}
}

// Set replaces the allowed targets. It is a no-op when nothing changed: the
// targets, the resolvers and the block list are what the table was last made
// to hold, and nothing since said that the live table is another. A failed
// apply leaves the table as it was and is tried again at the next Set.
func (f *Filter) Set(ctx context.Context, targets []Target) error {
	want, err := normalizeTargets(targets)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.want = want
	return f.sync(ctx)
}

// Remove takes one address out at once, on every port, as when its identity
// failed between two cycles. It deletes just its elements from the live table,
// and replaces the whole table when that fails. A resolver with the same
// address stays.
func (f *Filter) Remove(ctx context.Context, addr netip.Addr) error {
	addr = normalizeAddr(addr)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.want = slices.DeleteFunc(slices.Clone(f.want), func(t Target) bool { return t.Addr == addr })
	off, err := f.ov.switchedOff()
	if err != nil {
		return err
	}
	if off {
		f.applied = nil
		return nil
	}
	if f.applied == nil || f.stale {
		return f.sync(ctx)
	}
	gone := slices.DeleteFunc(slices.Clone(f.applied.targets), func(t Target) bool { return t.Addr != addr })
	if len(gone) == 0 {
		return nil
	}
	if err := f.nft.Apply(ctx, deleteScript(gone, nil)); err == nil {
		next := *f.applied
		next.targets = slices.DeleteFunc(slices.Clone(next.targets), func(t Target) bool { return t.Addr == addr })
		f.applied = &next
		return nil
	}
	// The live table does not hold what was last applied.
	f.stale = true
	return f.sync(ctx)
}

// Verify reads the live table back and reports whether it is the one last
// applied, less the addresses blocked since: its flags, its chains, their
// hooks and rules in order, its sets with their elements and its counters.
// Before the filter applied anything, the elements are not compared. A
// difference, a table that is gone or a listing that cannot be read is
// ErrChanged, and makes the next Set apply even with the same targets; while
// the filter is switched off it is ErrOff. Other errors say that nft could not
// list the table.
func (f *Filter) Verify(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	off, err := f.ov.switchedOff()
	if err != nil {
		return err
	}
	if off {
		return ErrOff
	}
	l, err := list(ctx, f.nft)
	switch {
	case errors.Is(err, ErrNotLoaded), errors.Is(err, ErrUnreadable):
		// A listing that cannot be read is no table pco applied either.
		f.stale = true
		return fmt.Errorf("%w: %w", ErrChanged, err)
	case err != nil:
		return fmt.Errorf("listing the egress table: %w", err)
	}
	var want *contents
	if f.applied != nil {
		blocked, err := f.ov.Blocked()
		if err != nil {
			return err
		}
		c := f.applied.blocking(blocked)
		want = &c
	}
	d := l.differences(f.uid, want)
	if len(d) == 0 {
		return nil
	}
	f.stale = true
	if len(d) > maxDifferences {
		d = append(d[:maxDifferences], fmt.Sprintf("and %d more", len(d)-maxDifferences))
	}
	return fmt.Errorf("%w: %s", ErrChanged, strings.Join(d, "; "))
}

// sync makes the live table hold the wanted targets and the resolvers, less
// the blocked addresses, unless it holds them already.
func (f *Filter) sync(ctx context.Context) error {
	if f.uid == 0 {
		return errors.New("the egress filter does not confine root: the connector user has uid 0")
	}
	off, err := f.ov.switchedOff()
	if err != nil {
		return err
	}
	if off {
		// The switch removed the table; what it holds is not known.
		f.applied = nil
		return nil
	}
	next, err := f.contents()
	if err != nil {
		return err
	}
	if f.applied != nil && !f.stale && f.applied.equal(next) {
		return nil
	}
	if err := f.nft.Apply(ctx, render(f.uid, next)); err != nil {
		// A transaction that fails changes nothing, so the table still
		// holds what was applied before.
		f.stale = true
		return fmt.Errorf("applying the egress table: %w", err)
	}
	f.applied, f.stale = &next, false
	return nil
}

func (f *Filter) contents() (contents, error) {
	blocked, err := f.ov.Blocked()
	if err != nil {
		return contents{}, err
	}
	resolvers, err := f.resolvers()
	if err != nil {
		return contents{}, fmt.Errorf("reading the resolvers: %w", err)
	}
	c := contents{targets: f.want, resolvers: normalizeAddrs(resolvers)}
	return c.blocking(blocked), nil
}

// blocking returns c with the blocked addresses in its blocked sets and out of
// its targets and resolvers, so that the sets show what is allowed.
func (c contents) blocking(blocked []netip.Addr) contents {
	return contents{
		targets: slices.DeleteFunc(slices.Clone(c.targets), func(t Target) bool {
			return slices.Contains(blocked, t.Addr)
		}),
		resolvers: slices.DeleteFunc(slices.Clone(c.resolvers), func(a netip.Addr) bool {
			return slices.Contains(blocked, a)
		}),
		blocked: slices.Clone(blocked),
	}
}

// normalizeTargets returns the targets sorted and once each, with the zone
// and the IPv4 mapping taken off their addresses, which nft does not know.
func normalizeTargets(targets []Target) ([]Target, error) {
	out := make([]Target, 0, len(targets))
	for _, t := range targets {
		if !t.Addr.IsValid() || t.Port == 0 {
			return nil, fmt.Errorf("invalid egress target %q", t.String())
		}
		out = append(out, Target{Addr: normalizeAddr(t.Addr), Port: t.Port})
	}
	return sortTargets(out), nil
}

func sortTargets(targets []Target) []Target {
	slices.SortFunc(targets, Target.compare)
	return slices.Compact(targets)
}

func normalizeAddr(a netip.Addr) netip.Addr { return a.WithZone("").Unmap() }

// normalizeAddrs returns the valid addresses, normalized, sorted and once each.
func normalizeAddrs(addrs []netip.Addr) []netip.Addr {
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if a.IsValid() {
			out = append(out, normalizeAddr(a))
		}
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return slices.Compact(out)
}
