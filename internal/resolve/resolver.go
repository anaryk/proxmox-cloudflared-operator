package resolve

import (
	"context"
	"net/netip"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

const (
	defaultStickyFor = 2 * time.Minute

	reasonNotRunning  = "guest is not running"
	reasonNotFound    = "guest not found in inventory"
	reasonNoCandidate = "no candidate address"
	reasonNotTried    = "not tried"
	reasonCancelled   = "resolve cancelled"
	reasonNodeAddress = "address of this node"
)

// HostIface is a network interface of the node.
type HostIface struct {
	Name   string
	Addrs  []netip.Prefix
	Master string // bridge this interface is enslaved to, if any
}

// Prober is the host-side view the resolver needs.
type Prober interface {
	Interfaces(ctx context.Context) ([]HostIface, error)
	// ARP asks for addr on iface and returns every MAC that answered in the window.
	ARP(ctx context.Context, iface string, addr netip.Addr) ([]string, error)
	// FDBPort returns the port of bridge on which mac is learned for vlan (0 = untagged).
	FDBPort(ctx context.Context, bridge string, vlan int, mac string) (port string, found bool, err error)
	Dial(ctx context.Context, target netip.AddrPort) error
}

// Settings tunes the resolver.
type Settings struct {
	LocalNode    string
	StickyFor    time.Duration // keep a failing binding this long before trying others, default 2m
	TrustStatic  bool          // resolve.trustStaticConfig
	TrustedCIDRs []netip.Prefix
}

// CandidateResult is how one candidate fared.
type CandidateResult struct {
	Addr   netip.Addr
	Source CandidateSource
	OK     bool
	Reason string
}

// Result is the outcome of resolving one route.
type Result struct {
	Target     planner.ResolvedTarget
	Binding    *Binding          // nil when nothing is bound
	Candidates []CandidateResult // every candidate with its outcome, for diagnosis
}

// Resolver decides which address a route may be served on. It keeps nothing
// between calls; what it needs from the past is the previous binding.
type Resolver struct {
	prober   Prober
	settings Settings
	now      func() time.Time
}

// NewResolver returns a Resolver that looks at the host through p. A nil now
// selects time.Now.
func NewResolver(p Prober, s Settings, now func() time.Time) *Resolver {
	if s.StickyFor <= 0 {
		s.StickyFor = defaultStickyFor
	}
	s.TrustedCIDRs = slices.Clone(s.TrustedCIDRs)
	if now == nil {
		now = time.Now
	}
	return &Resolver{prober: p, settings: s, now: now}
}

// Resolve decides the address for one route of a guest.
//
// A candidate is served only when it is neither denied nor an address of
// this node, ARP for it is answered only by this guest's NICs on that bridge,
// the bridge has learned those MACs on the guest's own ports (unless the
// guest is known to run on another node), no other running guest has those
// MACs configured unless the binding already had them and the forwarding
// table placed them, and the port answers.
//
// The previous binding is verified first, even when no source reports its
// address any more. While it fails for less than StickyFor it stays the
// target and no other candidate is tried. Losing identity, a stopped or
// missing guest, or a host that cannot be asked for StickyFor withdraws it,
// and only a full verification serves it again. When nothing passes and
// nothing is bound, the target says why the first candidate failed and has
// no address. A cancelled ctx leaves the previous state as it was.
func (r *Resolver) Resolve(ctx context.Context, route model.Route, snap inventory.Snapshot, prev *Binding, deny Denylist) Result {
	res := r.resolve(ctx, route, snap, prev, deny)
	if res.Target.Addr.IsValid() {
		res.Target.Owner = route.Owner()
	}
	return res
}

func (r *Resolver) resolve(ctx context.Context, route model.Route, snap inventory.Snapshot, prev *Binding, deny Denylist) Result {
	if route.Guest == nil {
		return r.resolveAddress(ctx, route, deny)
	}
	if !prev.appliesTo(route) {
		prev = nil
	}
	if ctx.Err() != nil {
		return unchanged(prev, deny)
	}
	now := r.now()
	guest, ok := snap.Guest(*route.Guest)
	switch {
	case !ok:
		return hold(prev, deny, reasonNotFound, now)
	case !guest.Running:
		return hold(prev, deny, reasonNotRunning, now)
	}
	cands, err := Candidates(route, guest)
	if err != nil {
		return Result{Target: planner.ResolvedTarget{Rejected: true, Reason: err.Error()}}
	}
	return newAttempt(r, route, guest, snap, deny, prev, now, cands).resolve(ctx)
}

// hold withdraws the previous binding without probing while its guest
// cannot be checked. The denylist still applies to it.
func hold(prev *Binding, deny Denylist, reason string, now time.Time) Result {
	if prev == nil {
		return Result{Target: planner.ResolvedTarget{Reason: reason}}
	}
	if why, denied := deny.Check(prev.Addr); denied {
		return Result{Target: planner.ResolvedTarget{Rejected: true, Reason: why}}
	}
	return Result{Target: planner.ResolvedTarget{Addr: prev.Addr, Withdrawn: true, Reason: reason}, Binding: prev.withdrawn(now)}
}

// unchanged returns what prev says, for a call that could not finish. A
// denied address is still rejected.
func unchanged(prev *Binding, deny Denylist) Result {
	if prev == nil {
		return Result{Target: planner.ResolvedTarget{Reason: reasonCancelled}}
	}
	if why, denied := deny.Check(prev.Addr); denied {
		return Result{Target: planner.ResolvedTarget{Rejected: true, Reason: why}}
	}
	return Result{Target: prev.target(reasonCancelled), Binding: prev.clone()}
}

// resolveAddress handles a route an admin wrote without a guest: there is no
// identity to check, so only the denylist and the dial apply.
func (r *Resolver) resolveAddress(ctx context.Context, route model.Route, deny Denylist) Result {
	addr := route.Target.Addr
	cr := CandidateResult{Addr: addr, Source: FromVia}
	if why, denied := deny.Check(addr); denied {
		cr.Reason = why
		return Result{Target: planner.ResolvedTarget{Rejected: true, Reason: why}, Candidates: []CandidateResult{cr}}
	}
	o := r.dial(ctx, addr, route.Target.Port)
	cr.OK, cr.Reason = o.ok(), o.reason
	return Result{
		Target:     planner.ResolvedTarget{Addr: addr, Reachable: o.ok(), Reason: o.reason},
		Candidates: []CandidateResult{cr},
	}
}

// attempt is one Resolve call for a running guest.
type attempt struct {
	r     *Resolver
	route model.Route
	guest model.Guest
	snap  inventory.Snapshot
	deny  Denylist
	prev  *Binding // the binding of this route, if it still applies
	now   time.Time

	list     []Candidate // in the order they are tried
	bound    bool        // list[0] carries prev
	outcomes []outcome   // by list index
	tried    []bool      // by list index
	results  []CandidateResult

	ifacesRead bool
	ifaces     []HostIface
	ifacesErr  error
}

// newAttempt puts the candidate of the previous binding first, or drops the
// binding when it no longer applies.
func newAttempt(r *Resolver, route model.Route, guest model.Guest, snap inventory.Snapshot, deny Denylist, prev *Binding, now time.Time, cands []Candidate) *attempt {
	a := &attempt{r: r, route: route, guest: guest, snap: snap, deny: deny, now: now, list: cands}
	if prev != nil {
		if c, ok := prev.boundCandidate(route, guest, cands); ok {
			rest := slices.DeleteFunc(slices.Clone(cands), func(o Candidate) bool { return o.Addr == c.Addr })
			a.list, a.prev, a.bound = append([]Candidate{c}, rest...), prev, true
		}
	}
	a.outcomes = make([]outcome, len(a.list))
	a.tried = make([]bool, len(a.list))
	return a
}

func (a *attempt) resolve(ctx context.Context) Result {
	if a.bound {
		o := a.try(ctx, 0)
		switch {
		case o.verdict == cancelled:
			return a.unchanged()
		case o.ok():
			return a.served(a.list[0])
		case o.verdict == rejected:
			a.prev, a.bound = nil, false
		case a.prev.holds(a.now, a.r.settings.StickyFor):
			return a.boundFailed(o)
		}
	}
	for i := range a.list {
		if a.tried[i] {
			continue
		}
		o := a.try(ctx, i)
		switch {
		case o.verdict == cancelled:
			return a.unchanged()
		case o.ok():
			return a.served(a.list[i])
		}
	}
	switch {
	case a.bound:
		return a.boundFailed(a.outcomes[0])
	case len(a.list) == 0:
		return a.result(planner.ResolvedTarget{Reason: reasonNoCandidate}, nil)
	}
	first := a.outcomes[0]
	return a.result(planner.ResolvedTarget{Rejected: first.verdict == rejected, Reason: first.reason}, nil)
}

func (a *attempt) try(ctx context.Context, i int) outcome {
	c := a.list[i]
	o := a.verify(ctx, c)
	a.outcomes[i], a.tried[i] = o, true
	a.results = append(a.results, CandidateResult{Addr: c.Addr, Source: c.Source, OK: o.ok(), Reason: o.reason})
	return o
}

func (a *attempt) served(c Candidate) Result {
	return a.result(planner.ResolvedTarget{Addr: c.Addr, Reachable: true}, newBinding(a.route, c, a.now))
}

// boundFailed keeps the failing binding as the target. It is withdrawn when
// identity is lost, when it already was, or when the host could not be asked
// for StickyFor; otherwise it stays served and unreachable.
func (a *attempt) boundFailed(o outcome) Result {
	b := a.prev.failing(a.now)
	b.Withdrawn = b.Withdrawn || o.verdict == notIdentified ||
		(o.verdict == probeFailed && !a.prev.holds(a.now, a.r.settings.StickyFor))
	return a.result(planner.ResolvedTarget{Addr: b.Addr, Withdrawn: b.Withdrawn, Reason: o.reason}, b)
}

// unchanged reports the previous state for a call that was cancelled. A
// binding found rejected in this call is already gone.
func (a *attempt) unchanged() Result {
	res := unchanged(a.prev, a.deny)
	return a.result(res.Target, res.Binding)
}

// result lists the candidates in the order tried, then those not tried.
func (a *attempt) result(target planner.ResolvedTarget, b *Binding) Result {
	out := a.results
	for i, c := range a.list {
		if !a.tried[i] {
			out = append(out, CandidateResult{Addr: c.Addr, Source: c.Source, Reason: reasonNotTried})
		}
	}
	return Result{Target: target, Binding: b, Candidates: out}
}
