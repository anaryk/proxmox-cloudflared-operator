// Package resolve decides which address of a guest a published route may
// point at.
//
// A Resolver keeps nothing between calls, so Resolve may be called from
// several goroutines at once as long as its Prober allows that; the one
// NewHostProber returns does.
package resolve

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

const (
	defaultStickyFor   = 2 * time.Minute
	defaultMaxProofAge = 5 * time.Minute

	// maxTries bounds the candidates verified in one call, so that a guest
	// reporting many addresses cannot make a call arbitrarily long.
	maxTries = 16

	reasonNotRunning    = "guest is not running"
	reasonNotFound      = "guest not found in inventory"
	reasonGuestUnknown  = "guest state unknown"
	reasonNoCandidate   = "no candidate address"
	reasonNotTried      = "not tried"
	reasonTooMany       = "not tried: too many candidates"
	reasonCancelled     = "resolve cancelled"
	reasonNodeAddress   = "address of this node"
	reasonProofInFuture = "identity proof is dated in the future"
)

// HostIface is a network interface of the node.
type HostIface struct {
	Name   string
	Addrs  []netip.Prefix
	Master string // bridge this interface is enslaved to, if any
}

// ErrTooManyClaimants is wrapped by an ARP error when so many stations claim
// the address that the answer was cut short. That is an identity failure, not
// a host that could not be asked.
var ErrTooManyClaimants = errors.New("too many stations claim the address")

// ErrNoRoute is wrapped by a Route error when the kernel has no route to the
// address at all, as opposed to one that could not be looked up.
var ErrNoRoute = errors.New("no route to the address")

// ErrRouteDiffers is wrapped by a Route error when the route from the source
// address the kernel picks for addr is not the one found without it.
var ErrRouteDiffers = errors.New("route depends on the source address")

// Prober is the host-side view the resolver needs. Its methods may be called
// concurrently.
type Prober interface {
	Interfaces(ctx context.Context) ([]HostIface, error)
	// Route returns the interface the kernel sends traffic for addr out of,
	// and whether it goes to addr directly rather than through a gateway. A
	// route that changes with the source address the kernel picks fails with
	// ErrRouteDiffers; policy routing on anything else, such as the user of a
	// socket or a firewall mark, is not supported.
	Route(ctx context.Context, addr netip.Addr) (iface string, onLink bool, err error)
	// ARP asks for addr on iface and returns every MAC that answered in the window.
	ARP(ctx context.Context, iface string, addr netip.Addr) ([]string, error)
	// FDBPorts returns every port of bridge on which mac is learned for vlan
	// (0 = untagged).
	FDBPorts(ctx context.Context, bridge string, vlan int, mac string) ([]string, error)
	Dial(ctx context.Context, target netip.AddrPort) error
}

// Settings tunes the resolver.
type Settings struct {
	// LocalNode is the name of the node the resolver runs on. A guest that
	// runs on another node gets no forwarding-table step, only the check that
	// no local guest port has its MAC; when LocalNode is empty or names none
	// of the nodes the inventory lists, every guest gets the step. When the
	// inventory lists no node at all, a guest said to run elsewhere gets it
	// too, but its failure proves nothing unless the MAC is on the port of a
	// local guest.
	LocalNode string
	// StickyFor is how long a failing binding is kept before other candidates
	// are tried, and how long a new one is kept before a candidate proven
	// higher is looked for; default 2m.
	StickyFor    time.Duration
	TrustStatic  bool // resolve.trustStaticConfig
	TrustedCIDRs []netip.Prefix
	// MaxProofAge is how long a bound address stays served on an old proof of
	// identity when a call cannot prove it anew, default 5m.
	MaxProofAge time.Duration
}

// CandidateResult is how one candidate fared. Level is the level its identity
// was proven at, also when its port then failed; it is a plain string, as in
// the other views of a route.
type CandidateResult struct {
	Addr   netip.Addr      `json:"addr"`
	Source CandidateSource `json:"source"`
	OK     bool            `json:"ok"`
	Reason string          `json:"reason,omitempty"` // why it failed, or was not tried
	Level  string          `json:"level,omitempty"`
}

// Result is the outcome of resolving one route.
type Result struct {
	Target     planner.ResolvedTarget
	Binding    *Binding          // nil when nothing is bound
	Candidates []CandidateResult // every candidate with its outcome, for diagnosis
	// Level is the level the target's address is proven at, whether or not
	// its port answers. It is empty when the target has no address, or one
	// that is withdrawn or rejected.
	Level Level
}

// Resolver decides which address a route may be served on. It keeps nothing
// between calls; what it needs from the past is the previous binding. It is
// safe for concurrent use.
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
	if s.MaxProofAge <= 0 {
		s.MaxProofAge = defaultMaxProofAge
	}
	s.TrustedCIDRs = slices.Clone(s.TrustedCIDRs)
	if now == nil {
		now = time.Now
	}
	return &Resolver{prober: p, settings: s, now: now}
}

// Resolve decides the address for one route of a guest.
//
// A candidate is served only when it is neither denied nor an address of any
// node of the cluster or of this host, the host's own traffic for it leaves
// directly through the interface on the NIC's bridge and VLAN, ARP for it
// there is answered only by this guest's NICs, the bridge has learned each
// of those MACs on exactly one port, the guest's own (for a guest known to
// run on another node: on no port of a local guest), no other running guest
// has those MACs configured unless the binding already had them and the
// forwarding table placed them, and the port answers. Of the candidates that
// pass, the one proven at the highest level is served; the order decides only
// between equal levels. A binding that passes is kept without looking further
// until it has been bound for StickyFor.
//
// The previous binding is verified first, even when no source reports its
// address any more. After a dial failure it stays the target for StickyFor
// before other candidates are tried; after a prober error, while an
// incomplete inventory does not list the guest, or while Proxmox has never
// reported whether it runs, while its last proof is not in doubt. Lost
// identity, a stopped guest, a guest missing from a complete inventory, or a
// doubtful proof (older than MaxProofAge, dated in the future, or for a MAC
// another guest that runs or may run has too) withdraws it, and only
// identity passing again lifts that. When nothing passes and nothing is
// bound, the target says why the first candidate failed and has no address.
// Whether the binding still applies is decided before ctx is looked at; a
// cancelled ctx then leaves the previous state as it was, except for what the
// call has already learned about the bound address.
//
// required is the least level the caller serves a guest's address at. A
// binding proven below it is not kept without looking further, as it is not
// served; what is served, and at which level, stays the caller's to decide.
func (r *Resolver) Resolve(ctx context.Context, route model.Route, snap inventory.Snapshot, prev *Binding, deny Denylist, required Level) Result {
	res := r.resolve(ctx, route, snap, prev, deny, required)
	if res.Target.Addr.IsValid() {
		res.Target.Owner = route.Owner()
	}
	return res
}

func (r *Resolver) resolve(ctx context.Context, route model.Route, snap inventory.Snapshot, prev *Binding, deny Denylist, required Level) Result {
	if route.Guest == nil {
		return r.resolveAddress(ctx, route, snap, deny)
	}
	if !prev.appliesTo(route) {
		prev = nil
	}
	now := r.now()
	guest, ok := snap.Guest(*route.Guest)
	switch {
	case !ok && !snap.Complete:
		a := &attempt{r: r, route: route, guest: model.Guest{Ref: *route.Guest}, snap: snap, deny: deny, prev: prev, now: now}
		return a.guestUnknown()
	case !ok:
		return hold(prev, deny, snap, reasonNotFound, now)
	case !guest.Running && !guest.StatusUnknown:
		return hold(prev, deny, snap, reasonNotRunning, now)
	}
	cands, err := Candidates(route, guest)
	if err != nil {
		return Result{Target: planner.ResolvedTarget{Rejected: true, Reason: err.Error()}}
	}
	// Whether the binding still applies is settled before ctx is looked at,
	// so that a cancelled call cannot keep one that does not.
	a := newAttempt(r, route, guest, snap, deny, prev, now, cands)
	a.required = required
	if !guest.Running {
		return a.guestUnknown()
	}
	if ctx.Err() != nil {
		return a.cancelled()
	}
	return a.resolve(ctx)
}

// hold withdraws the previous binding without probing while its guest
// cannot be checked. The denylist and the node addresses still apply to it.
func hold(prev *Binding, deny Denylist, snap inventory.Snapshot, reason string, now time.Time) Result {
	if prev == nil {
		return Result{Target: planner.ResolvedTarget{Reason: reason}}
	}
	if why, denied := forbidden(deny, snap, prev.Addr); denied {
		return Result{Target: planner.ResolvedTarget{Rejected: true, Reason: why}}
	}
	return Result{Target: planner.ResolvedTarget{Addr: prev.Addr, Withdrawn: true, Reason: reason}, Binding: prev.withdrawn(now)}
}

// resolveAddress handles a route an admin wrote without a guest: there is no
// identity to check, so only the denylist, the addresses of the nodes and the
// dial apply. AllowNode lifts the node rules, nothing else, and only on a
// manual route.
func (r *Resolver) resolveAddress(ctx context.Context, route model.Route, snap inventory.Snapshot, deny Denylist) Result {
	addr := route.Target.Addr
	allowNode := route.Options.AllowNode && route.Source == model.SourceManual
	if allowNode {
		deny = deny.withoutNodes()
	}
	o := r.checkAddress(ctx, addr, allowNode, snap, deny)
	if o.ok() {
		o = r.dial(ctx, addr, route.Target.Port)
	}
	cr := []CandidateResult{{Addr: addr, Source: FromVia, OK: o.ok(), Reason: o.reason}}
	switch o.verdict {
	case passed, unreachable:
		target := planner.ResolvedTarget{Addr: addr, Reachable: o.ok(), Reason: o.reason}
		return Result{Target: target, Candidates: cr, Level: LevelManual}
	case rejected:
		return Result{Target: planner.ResolvedTarget{Rejected: true, Reason: o.reason}, Candidates: cr}
	}
	// Nothing was learned about the address: serve nothing but keep DNS.
	return Result{Target: planner.ResolvedTarget{Addr: addr, Withdrawn: true, Reason: o.reason}, Candidates: cr}
}

// checkAddress rejects an address a guest-less route must not point at.
func (r *Resolver) checkAddress(ctx context.Context, addr netip.Addr, allowNode bool, snap inventory.Snapshot, deny Denylist) outcome {
	if why, denied := deny.Check(addr); denied {
		return outcome{rejected, why}
	}
	if allowNode {
		return outcome{}
	}
	if isClusterNodeAddr(snap.Nodes, addr) {
		return outcome{rejected, reasonClusterNode}
	}
	if ctx.Err() != nil {
		return stopped()
	}
	ifaces, err := r.prober.Interfaces(ctx)
	if o := answered(ctx, err, "listing host interfaces: %v"); !o.ok() {
		return o
	}
	if isNodeAddr(ifaces, addr) {
		return outcome{rejected, reasonNodeAddress}
	}
	return outcome{}
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
	// required is the least level the caller serves at: a binding proven
	// below it does not settle.
	required Level

	list     []Candidate // in the order they are tried
	bound    bool        // list[0] carries prev
	outcomes []outcome   // by list index
	proofs   []proof     // by list index: what proved the identity, if it held
	tried    []bool      // by list index
	results  []CandidateResult

	ifacesRead bool
	ifaces     []HostIface
	ifacesErr  error
}

// newAttempt puts the candidate of the previous binding first, or drops the
// binding when it no longer applies. The same address on another NIC stays a
// candidate of its own, so that an address that moved can be bound again.
func newAttempt(r *Resolver, route model.Route, guest model.Guest, snap inventory.Snapshot, deny Denylist, prev *Binding, now time.Time, cands []Candidate) *attempt {
	a := &attempt{r: r, route: route, guest: guest, snap: snap, deny: deny, now: now, list: cands}
	if prev != nil {
		if c, ok := prev.boundCandidate(route, guest, cands); ok {
			rest := slices.DeleteFunc(slices.Clone(cands), c.sameAs)
			a.list, a.prev, a.bound = append([]Candidate{c}, rest...), prev, true
		}
	}
	a.outcomes = make([]outcome, len(a.list))
	a.proofs = make([]proof, len(a.list))
	a.tried = make([]bool, len(a.list))
	return a
}

// resolve serves, of the candidates that pass, the one proven at the highest
// level, the first of them when several are equal. Once one passes, a
// candidate that cannot be proven higher is not tried, and none is while a
// passing binding has been bound for less than StickyFor: a candidate whose
// identity comes and goes would otherwise move the route up and down every
// call. A candidate proven before the call is cancelled is served all the
// same.
func (a *attempt) resolve(ctx context.Context) Result {
	best := -1
	if a.bound {
		o := a.try(ctx, 0)
		switch {
		case o.verdict == cancelled:
			return a.cancelled()
		case o.ok() && a.proofs[0].level.AtLeast(a.required) && a.prev.settling(a.now, a.r.settings.StickyFor):
			return a.served(0)
		case o.ok():
			best = 0
		case o.verdict == rejected:
			a.prev, a.bound = nil, false
		case a.keepsBound(o):
			return a.boundFailed(o)
		}
	}
	for i := range a.list[:min(len(a.list), maxTries)] {
		if a.tried[i] || best >= 0 && a.proofs[best].level.AtLeast(a.ceilingOf(a.list[i])) {
			continue
		}
		switch o := a.try(ctx, i); {
		case o.verdict == cancelled && best >= 0:
			return a.served(best)
		case o.verdict == cancelled:
			return a.cancelled()
		case o.ok() && (best < 0 || !a.proofs[best].level.AtLeast(a.proofs[i].level)):
			best = i
		}
	}
	if best >= 0 {
		return a.served(best)
	}
	var res Result
	switch {
	case a.bound:
		res = a.boundFailed(a.outcomes[0])
	case len(a.list) == 0:
		return a.result(planner.ResolvedTarget{Reason: reasonNoCandidate}, nil)
	default:
		first := a.outcomes[0]
		res = a.result(planner.ResolvedTarget{Rejected: first.verdict == rejected, Reason: first.reason}, nil)
	}
	if n := len(a.list) - maxTries; n > 0 {
		res.Target.Reason += moreNotTried(n)
	}
	return res
}

func moreNotTried(n int) string {
	if n == 1 {
		return "; 1 more candidate not tried"
	}
	return fmt.Sprintf("; %d more candidates not tried", n)
}

// keepsBound reports whether the failing bound candidate stays the target
// without other candidates being tried: after a dial failure for StickyFor,
// after a prober error while its proof is not in doubt. Lost identity looks
// for another address at once.
func (a *attempt) keepsBound(o outcome) bool {
	switch o.verdict {
	case unreachable:
		return a.prev.holds(a.now, a.r.settings.StickyFor)
	case probeFailed:
		_, doubted := a.doubt(a.prev)
		return !a.prev.Withdrawn && !doubted
	}
	return false
}

func (a *attempt) try(ctx context.Context, i int) outcome {
	c := a.list[i]
	p, o := a.verify(ctx, c)
	a.outcomes[i], a.proofs[i], a.tried[i] = o, p, true
	a.results = append(a.results, CandidateResult{Addr: c.Addr, Source: c.Source, OK: o.ok(), Reason: o.reason, Level: string(p.level)})
	return o
}

// ceiling is the highest level a candidate of this guest can be proven at:
// port where the forwarding-table step applies, observed for a guest known to
// run on another node.
func (a *attempt) ceiling() Level {
	if a.checksFDB() {
		return LevelPort
	}
	return LevelObserved
}

// ceilingOf is the highest level c can be proven at: observed when the node
// has no address next to it, as it then passes on the trusted path only, and
// the guest's ceiling otherwise. It needs the host's interfaces, which have
// been read once a candidate passed.
func (a *attempt) ceilingOf(c Candidate) Level {
	if arpInterface(a.ifaces, c) == "" {
		return LevelObserved
	}
	return a.ceiling()
}

// served binds the route to the candidate at i; the bound candidate keeps
// the time it was bound.
func (a *attempt) served(i int) Result {
	c := a.list[i]
	b := newBinding(a.route, c, a.now, a.proofs[i])
	if a.bound && i == 0 {
		b.Since = a.prev.boundSince()
	}
	return a.result(planner.ResolvedTarget{Addr: c.Addr, Reachable: true}, b)
}

// boundFailed keeps the failing binding as the target. Lost identity
// withdraws it. A dial failure comes after identity passed, which renews the
// proof and lifts a withdrawal. A prober error proves nothing either way.
func (a *attempt) boundFailed(o outcome) Result {
	b := a.prev.failing(a.now)
	switch o.verdict {
	case notIdentified:
		b.Withdrawn = true
		return a.result(planner.ResolvedTarget{Addr: b.Addr, Withdrawn: true, Reason: o.reason}, b)
	case unreachable:
		b = b.proven(a.now, a.proofs[0])
		return a.result(planner.ResolvedTarget{Addr: b.Addr, Reason: o.reason}, b)
	}
	return a.unproven(b, o.reason)
}

// cancelled reports a call that could not finish. What it learned about the
// first candidate stands; otherwise the previous state is kept.
func (a *attempt) cancelled() Result {
	if len(a.list) > 0 && a.tried[0] {
		switch first := a.outcomes[0]; {
		case first.verdict == cancelled:
		case a.bound:
			return a.boundFailed(first)
		case first.verdict == rejected:
			return a.result(planner.ResolvedTarget{Rejected: true, Reason: first.reason}, nil)
		}
	}
	return a.unchanged()
}

// unchanged returns what the binding says, for a call that learned nothing
// about it. A forbidden address is still rejected.
func (a *attempt) unchanged() Result {
	if a.prev == nil {
		return a.result(planner.ResolvedTarget{Reason: reasonCancelled}, nil)
	}
	if why, denied := a.forbidden(a.prev.Addr); denied {
		return a.result(planner.ResolvedTarget{Rejected: true, Reason: why}, nil)
	}
	return a.unproven(a.prev.clone(), reasonCancelled)
}

// guestUnknown keeps the binding of a guest whose state is not known: an
// incomplete inventory does not list it, or Proxmox has never reported
// whether it runs. Nothing shows that the guest is gone or stopped, or that
// it is there, so the address is treated as after a prober error: served
// while its last proof is fresh, and not probed.
func (a *attempt) guestUnknown() Result {
	if a.prev == nil {
		return a.result(planner.ResolvedTarget{Reason: reasonGuestUnknown}, nil)
	}
	if why, denied := a.forbidden(a.prev.Addr); denied {
		return a.result(planner.ResolvedTarget{Rejected: true, Reason: why}, nil)
	}
	return a.unproven(a.prev.failing(a.now), reasonGuestUnknown)
}

// unproven returns b as the target of a call that did not prove its
// identity, withdrawn when its old proof is in doubt. The old proof stands for
// no more than observed while the guest runs, or may run, on another node,
// where this node's forwarding table cannot place its MACs.
func (a *attempt) unproven(b *Binding, reason string) Result {
	if why, doubted := a.doubt(b); doubted && !b.Withdrawn {
		b.Withdrawn, reason = true, why
	}
	res := a.result(b.target(reason), b)
	if res.Level != "" && (!a.checksFDB() || a.mayRunElsewhere()) {
		res.Level = LevelObserved
	}
	return res
}

// doubt says why b may not be served on an old proof: another guest that runs
// or may run has its MAC, or the proof is older than MaxProofAge or dated in
// the future.
func (a *attempt) doubt(b *Binding) (string, bool) {
	if mac, err := model.NormalizeMAC(b.MAC); err == nil {
		if other, shared := a.sharedWith(mac); shared {
			return fmt.Sprintf("MAC %s is also configured on %s", mac, other), true
		}
	}
	switch age := a.now.Sub(b.VerifiedAt); {
	case age < 0:
		return reasonProofInFuture, true
	case age > a.r.settings.MaxProofAge:
		return fmt.Sprintf("identity not confirmed for %s", age.Round(time.Second)), true
	}
	return "", false
}

// result lists the candidates in the order tried, then those not tried, and
// gives the target the level of the proof it stands on.
func (a *attempt) result(target planner.ResolvedTarget, b *Binding) Result {
	out := a.results
	for i, c := range a.list {
		switch {
		case a.tried[i]:
		case i >= maxTries:
			out = append(out, CandidateResult{Addr: c.Addr, Source: c.Source, Reason: reasonTooMany})
		default:
			out = append(out, CandidateResult{Addr: c.Addr, Source: c.Source, Reason: reasonNotTried})
		}
	}
	res := Result{Target: target, Binding: b, Candidates: out}
	if b != nil && target.Addr.IsValid() && !target.Withdrawn && !target.Rejected {
		res.Level = b.Proven()
	}
	return res
}
