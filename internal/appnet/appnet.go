// Package appnet is the network boot of the appliance, what pco-net.service
// loads before the connectors start: the dummy device pco0 that takes the
// service prefix, so that nothing sent to it leaves through net0, the address
// and the policy rules the gateway of the managed network needs, and the
// table that rejects whatever is still sent to the prefix.
package appnet

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
)

const (
	// Device takes the service prefix; Table holds the rule that rejects it.
	Device = "pco0"
	Table  = "inet pco_net"

	// The preferences of the two policy rules. A packet from ServiceSource
	// to the prefix is looked up in main, which routes it to pco0; any other
	// packet from that source goes nowhere. The unreachable rule alone would
	// stop every request to the prefix.
	PrefToPrefix    = 1890
	PrefUnreachable = 1900
)

// ServicePrefix and ServiceSource carry the values and the meaning of the
// gateway's ServicePrefix and TransitHost, which take their place once the
// managed network has a gateway.
var (
	ServicePrefix = netip.MustParsePrefix("198.18.0.0/16")
	ServiceSource = netip.MustParseAddr("198.18.0.1")
)

// Netlink is what Load and Verify change and read of the network of the
// container. Each Ensure leaves alone what is there already.
type Netlink interface {
	// EnsureDummy makes a dummy device of that name unless there is one and
	// brings it up; HasDummy reports whether there is one and it is up.
	EnsureDummy(ctx context.Context, name string) error
	HasDummy(ctx context.Context, name string) (bool, error)
	// EnsureAddr gives the device the address; HasAddr reports it.
	EnsureAddr(ctx context.Context, dev string, addr netip.Prefix) error
	HasAddr(ctx context.Context, dev string, addr netip.Prefix) (bool, error)
	// EnsureRoute routes the prefix through the device with the source in
	// the main table, in place of a route to the same prefix; HasRoute
	// reports one.
	EnsureRoute(ctx context.Context, prefix netip.Prefix, dev string, src netip.Addr) error
	HasRoute(ctx context.Context, prefix netip.Prefix, dev string, src netip.Addr) (bool, error)
	// EnsureRule adds an IPv4 policy rule at pref: from src, to dst (the
	// zero prefix: anywhere), lookup main or unreachable; HasRule reports
	// one with nothing more to match on.
	EnsureRule(ctx context.Context, pref int, src netip.Addr, dst netip.Prefix, unreachable bool) error
	HasRule(ctx context.Context, pref int, src netip.Addr, dst netip.Prefix, unreachable bool) (bool, error)
}

// link is one thing Load puts in place through netlink.
type link struct {
	name    string
	missing string
	has     func(ctx context.Context) (bool, error)
	ensure  func(ctx context.Context) error
}

// links are the things in the order the kernel takes them: the route needs
// the device up, and its source an address of the device. The unreachable
// rule comes before the one to the prefix, so that a load cut short between
// them leaves the source going nowhere rather than anywhere main routes it.
func links(nl Netlink) []link {
	addr := netip.PrefixFrom(ServiceSource, ServiceSource.BitLen())
	route := fmt.Sprintf("the route %s dev %s src %s", ServicePrefix, Device, ServiceSource)
	toPrefix := fmt.Sprintf("the rule %d: from %s to %s lookup main", PrefToPrefix, ServiceSource, ServicePrefix)
	unreachable := fmt.Sprintf("the rule %d: from %s unreachable", PrefUnreachable, ServiceSource)
	return []link{
		{
			name: "the dummy device " + Device, missing: "the dummy device " + Device + " is missing or down",
			has:    func(ctx context.Context) (bool, error) { return nl.HasDummy(ctx, Device) },
			ensure: func(ctx context.Context) error { return nl.EnsureDummy(ctx, Device) },
		},
		{
			name: fmt.Sprintf("the address %s on %s", addr, Device),
			has:  func(ctx context.Context) (bool, error) { return nl.HasAddr(ctx, Device, addr) },
			ensure: func(ctx context.Context) error {
				return nl.EnsureAddr(ctx, Device, addr)
			},
		},
		{
			name: route,
			has: func(ctx context.Context) (bool, error) {
				return nl.HasRoute(ctx, ServicePrefix, Device, ServiceSource)
			},
			ensure: func(ctx context.Context) error {
				return nl.EnsureRoute(ctx, ServicePrefix, Device, ServiceSource)
			},
		},
		{
			name: unreachable,
			has: func(ctx context.Context) (bool, error) {
				return nl.HasRule(ctx, PrefUnreachable, ServiceSource, netip.Prefix{}, true)
			},
			ensure: func(ctx context.Context) error {
				return nl.EnsureRule(ctx, PrefUnreachable, ServiceSource, netip.Prefix{}, true)
			},
		},
		{
			name: toPrefix,
			has: func(ctx context.Context) (bool, error) {
				return nl.HasRule(ctx, PrefToPrefix, ServiceSource, ServicePrefix, false)
			},
			ensure: func(ctx context.Context) error {
				return nl.EnsureRule(ctx, PrefToPrefix, ServiceSource, ServicePrefix, false)
			},
		},
	}
}

func (l link) missingText() string {
	if l.missing != "" {
		return l.missing
	}
	return l.name + " is missing"
}

// Load brings the device, its address, the route with its source, the two
// rules (the unreachable one first) and the table in place and reports what
// it changed, in that order. A
// table that is there as Script loads it is kept, with its counter. It stops
// at the first step that fails.
func Load(ctx context.Context, nl Netlink, nft egress.Nft) (changed []string, err error) {
	for _, l := range links(nl) {
		ok, err := l.has(ctx)
		if err != nil {
			return changed, fmt.Errorf("reading %s: %w", l.name, err)
		}
		if ok {
			continue
		}
		if err := l.ensure(ctx); err != nil {
			return changed, fmt.Errorf("adding %s: %w", l.name, err)
		}
		changed = append(changed, l.name)
	}
	table, err := tablePart(ctx, nft)
	if err != nil {
		return changed, err
	}
	if !table.OK() {
		if err := nft.Apply(ctx, Script()); err != nil {
			return changed, fmt.Errorf("loading the table %s: %w", Table, err)
		}
		changed = append(changed, "the table "+Table)
	}
	return changed, nil
}

// Part is one of the things Load puts in place, as Inspect found it.
type Part struct {
	Name string
	// Differences says what of it is not as Load leaves it; Missing that it
	// is not there at all, or for the device, down.
	Differences []string
	Missing     bool
}

// OK reports whether the part is as Load leaves it.
func (p Part) OK() bool { return len(p.Differences) == 0 }

// Inspect reports each of the things Load puts in place, in its order, the
// table last.
func Inspect(ctx context.Context, nl Netlink, nft egress.Nft) ([]Part, error) {
	var parts []Part
	for _, l := range links(nl) {
		ok, err := l.has(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", l.name, err)
		}
		p := Part{Name: l.name}
		if !ok {
			p.Missing, p.Differences = true, []string{l.missingText()}
		}
		parts = append(parts, p)
	}
	table, err := tablePart(ctx, nft)
	if err != nil {
		return nil, err
	}
	return append(parts, table), nil
}

// ErrNotLoaded says that the table is not there, ErrChanged that what Load
// puts in place is not as it leaves it. Each is also the egress error of the
// same meaning, which a caller that keeps both tables checks for.
var (
	ErrNotLoaded error = &sentinel{msg: "the table " + Table + " is not loaded", also: egress.ErrNotLoaded}
	ErrChanged   error = &sentinel{msg: "the service-prefix route or table is not as pco loads it", also: egress.ErrChanged}
)

type sentinel struct {
	msg  string
	also error
}

func (s *sentinel) Error() string { return s.msg }

func (s *sentinel) Unwrap() error { return s.also }

// ChangedError is what Verify says when the network is not as Load leaves
// it. It is an ErrChanged, and says nothing but the differences.
type ChangedError struct {
	Differences []string
}

func (e *ChangedError) Error() string { return strings.Join(e.Differences, "; ") }

func (e *ChangedError) Unwrap() error { return ErrChanged }

// Verify reports whether the device, the address, the route, the rules and
// the table are as Load leaves them; the differences are a ChangedError. What
// cannot be read is an error of its own.
func Verify(ctx context.Context, nl Netlink, nft egress.Nft) error {
	parts, err := Inspect(ctx, nl, nft)
	if err != nil {
		return err
	}
	var d []string
	for _, p := range parts {
		d = append(d, p.Differences...)
	}
	if len(d) == 0 {
		return nil
	}
	return &ChangedError{Differences: d}
}
