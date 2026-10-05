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
	"strconv"
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
	// AllowNode marks the target of a manual route with allowNode, which
	// root wrote to publish an address of the node. Such a target is accepted
	// before the addresses of the node are refused; every other one after.
	AllowNode bool
}

func (t Target) String() string { return netip.AddrPortFrom(t.Addr, t.Port).String() }

func (t Target) compare(o Target) int {
	return cmp.Or(CompareEndpoints(t, o), compareBool(t.AllowNode, o.AllowNode))
}

// CompareEndpoints orders targets by address and port, whether they are of
// allowNode or not: two that compare equal are the same element of a set.
func CompareEndpoints(a, b Target) int {
	return cmp.Or(a.Addr.Compare(b.Addr), cmp.Compare(a.Port, b.Port))
}

// CompareAllowNodeFirst orders targets by address and port, and one of
// allowNode before the other of an endpoint: of the duplicates of an endpoint,
// the first is the one to keep.
func CompareAllowNodeFirst(a, b Target) int {
	return cmp.Or(CompareEndpoints(a, b), compareBool(b.AllowNode, a.AllowNode))
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
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
// and replaces the whole table when that fails; before the filter applied
// anything, it deletes them from the table as it finds it. A resolver with the
// same address stays.
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
	switch {
	case f.applied == nil:
		return f.removeLive(ctx, addr)
	case f.stale:
		return f.sync(ctx)
	}
	gone := slices.DeleteFunc(slices.Clone(f.applied.targets), func(t Target) bool { return t.Addr != addr })
	if len(gone) == 0 {
		return nil
	}
	// The table last applied counts every target it holds.
	if err := f.nft.Apply(ctx, deleteScript(gone, gone, nil)); err == nil {
		next := *f.applied
		next.targets = slices.DeleteFunc(slices.Clone(next.targets), func(t Target) bool { return t.Addr == addr })
		f.applied = &next
		return nil
	}
	// The live table does not hold what was last applied.
	f.stale = true
	return f.sync(ctx)
}

// removeLive takes the targets of addr out of the live table, for a filter
// that has applied nothing yet: a daemon that starts finds the table it loaded
// before, or the one of the boot unit, and does not know its targets until its
// first Set. Loading the table anew would take the other targets out as well.
// The table may be one an older pco loaded, without the counting sets.
func (f *Filter) removeLive(ctx context.Context, addr netip.Addr) error {
	l, err := list(ctx, f.nft)
	switch {
	case errors.Is(err, ErrNotLoaded):
		return nil
	case err != nil:
		return fmt.Errorf("taking %s out of the egress table: %w", addr, err)
	}
	c, _ := l.contents()
	gone := slices.DeleteFunc(c.targets, func(t Target) bool { return t.Addr != addr })
	counted := l.countedOf(addr)
	if len(gone) == 0 && len(counted) == 0 {
		return nil
	}
	if err := f.nft.Apply(ctx, deleteScript(gone, counted, nil)); err != nil {
		return fmt.Errorf("taking %s out of the egress table: %w", addr, err)
	}
	return nil
}

// Verify reads the live table back and reports whether it is the one last
// applied, less the addresses blocked since: its flags, its chains, their
// hooks and rules in order, its sets with their elements and its counters.
// Before the filter applied anything, the elements are not compared. A
// difference, a table that is gone or a listing that cannot be read is
// ErrChanged, and makes the next Set apply even with the same targets; while
// the filter is switched off it is ErrOff, and what the table holds once it is
// switched on again is not known. Other errors say that nft could not list the
// table.
//
// What pco egress block took out of the live table stays out of what Verify
// expects, also once the address is unblocked: pco egress unblock puts
// nothing back, the next Set does.
func (f *Filter) Verify(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	off, err := f.ov.switchedOff()
	if err != nil {
		return err
	}
	if off {
		f.applied = nil
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
		f.applied, want = &c, &c
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

// Counters reads the counters of the counting sets of the live table, each
// with the generation of the table it was read from. It reads what the table
// holds, whatever the filter applied, without waiting for a Set or a Verify
// under way: the generation tells a table loaded in between.
func (f *Filter) Counters(ctx context.Context) ([]TargetFlows, error) {
	table, flows, err := ReadCounters(ctx, f.nft)
	if err != nil {
		return nil, err
	}
	for i := range flows {
		flows[i].Generation = Generation{Table: table}
	}
	return flows, nil
}

// FlowCounts is Counters by address and port, with the generation as text:
// two reads with the same generation are of the same counters.
func (f *Filter) FlowCounts(ctx context.Context) (generation string, counts map[netip.AddrPort]uint64, err error) {
	table, flows, err := ReadCounters(ctx, f.nft)
	if err != nil {
		return "", nil, err
	}
	counts = make(map[netip.AddrPort]uint64, len(flows))
	for _, fl := range flows {
		counts[netip.AddrPortFrom(fl.Target.Addr, fl.Target.Port)] = fl.Flows
	}
	return strconv.FormatUint(table, 10), counts, nil
}

// Rebind makes the filter confine the processes of another uid, as after
// the connector user was made anew, and reports whether the uid changed. The
// live table confines the old one until the next Set or Reapply.
func (f *Filter) Rebind(connectorUID uint32) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if connectorUID == f.uid {
		return false
	}
	f.uid, f.stale = connectorUID, true
	return true
}

// Reapply loads the table again with what the filter was last given, also
// when nothing changed since: after Verify found it gone, dormant or not the
// one applied. While the filter is switched off it loads nothing.
func (f *Filter) Reapply(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stale = true
	return f.sync(ctx)
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
// and the IPv4 mapping taken off their addresses, which nft does not know. An
// address and port given both as a target of allowNode and as another is one
// of allowNode, so that it is one element of one set.
func normalizeTargets(targets []Target) ([]Target, error) {
	out := make([]Target, 0, len(targets))
	for _, t := range targets {
		if !t.Addr.IsValid() || t.Port == 0 {
			return nil, fmt.Errorf("invalid egress target %q", t.String())
		}
		out = append(out, Target{Addr: normalizeAddr(t.Addr), Port: t.Port, AllowNode: t.AllowNode})
	}
	slices.SortFunc(out, CompareAllowNodeFirst)
	return slices.CompactFunc(out, func(a, b Target) bool { return CompareEndpoints(a, b) == 0 }), nil
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
