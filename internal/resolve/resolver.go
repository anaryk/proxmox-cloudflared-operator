package resolve

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
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
// A candidate is served only when it is not denied, its NIC's MAC is not
// configured on another running guest, ARP for it is answered only by this
// guest's NICs on that bridge, the bridge has learned those MACs on the
// guest's own ports (when the guest runs on this node) and the port answers.
//
// The previous binding is verified first and, while it fails for less than
// StickyFor, stays the target without other candidates being tried: losing
// identity withdraws it, anything else leaves it unreachable. When nothing
// passes and nothing is bound, the target says why the first candidate
// failed and has no address.
func (r *Resolver) Resolve(ctx context.Context, route model.Route, snap inventory.Snapshot, prev *Binding, deny Denylist) Result {
	if route.Guest == nil {
		return r.resolveAddress(ctx, route, deny)
	}
	if !prev.appliesTo(route) {
		prev = nil
	}
	guest, ok := snap.Guest(*route.Guest)
	switch {
	case !ok:
		return hold(prev, deny, reasonNotFound)
	case !guest.Running:
		return hold(prev, deny, reasonNotRunning)
	}
	cands, err := Candidates(route, guest)
	if err != nil {
		return Result{Target: planner.ResolvedTarget{Rejected: true, Reason: err.Error()}}
	}
	a := &attempt{
		r:        r,
		route:    route,
		guest:    guest,
		snap:     snap,
		deny:     deny,
		prev:     prev,
		now:      r.now(),
		cands:    cands,
		outcomes: make([]outcome, len(cands)),
		tried:    make([]bool, len(cands)),
	}
	return a.resolve(ctx)
}

// hold keeps the previous binding without probing while its guest cannot be
// checked. The denylist still applies to it.
func hold(prev *Binding, deny Denylist, reason string) Result {
	if prev == nil {
		return Result{Target: planner.ResolvedTarget{Reason: reason}}
	}
	if why, denied := deny.Check(prev.Addr); denied {
		return Result{Target: planner.ResolvedTarget{Rejected: true, Reason: why}}
	}
	return Result{Target: planner.ResolvedTarget{Addr: prev.Addr, Reason: reason}, Binding: prev.clone()}
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

func (r *Resolver) dial(ctx context.Context, addr netip.Addr, port uint16) outcome {
	if err := r.prober.Dial(ctx, netip.AddrPortFrom(addr, port)); err != nil {
		return outcome{unreachable, fmt.Sprintf("port %d: %v", port, err)}
	}
	return outcome{}
}

// verdict says how a candidate fared.
type verdict int

const (
	passed        verdict = iota
	onDenylist            // never to be served
	notIdentified         // the wire does not show the address belongs to this guest
	probeFailed           // the host could not be asked; proves nothing either way
	unreachable           // identity holds but the port does not answer
)

type outcome struct {
	verdict verdict
	reason  string
}

func (o outcome) ok() bool { return o.verdict == passed }

func lost(format string, args ...any) outcome {
	return outcome{notIdentified, fmt.Sprintf(format, args...)}
}

func probeError(format string, args ...any) outcome {
	return outcome{probeFailed, fmt.Sprintf(format, args...)}
}

// attempt is one Resolve call for a running guest.
type attempt struct {
	r     *Resolver
	route model.Route
	guest model.Guest
	snap  inventory.Snapshot
	deny  Denylist
	prev  *Binding // the binding of this route, if any
	now   time.Time
	cands []Candidate

	outcomes []outcome // by candidate index
	tried    []bool    // by candidate index
	results  []CandidateResult

	ifacesRead bool
	ifaces     []HostIface
	ifacesErr  error
}

func (a *attempt) resolve(ctx context.Context) Result {
	bound := a.boundIndex()
	if bound >= 0 {
		if a.try(ctx, bound).ok() {
			return a.served(a.cands[bound])
		}
		if a.prev.holds(a.now, a.r.settings.StickyFor) {
			return a.boundFailed(a.outcomes[bound])
		}
	}
	for i := range a.cands {
		if !a.tried[i] && a.try(ctx, i).ok() {
			return a.served(a.cands[i])
		}
	}
	switch {
	case bound >= 0:
		return a.boundFailed(a.outcomes[bound])
	case len(a.cands) == 0:
		return a.result(planner.ResolvedTarget{Reason: reasonNoCandidate}, nil)
	}
	first := a.outcomes[0]
	return a.result(planner.ResolvedTarget{Rejected: first.verdict == onDenylist, Reason: first.reason}, nil)
}

// boundIndex returns the candidate that holds the previous binding, or -1.
// A bound address that is denied now has lost its binding.
func (a *attempt) boundIndex() int {
	if a.prev == nil {
		return -1
	}
	if _, denied := a.deny.Check(a.prev.Addr); denied {
		return -1
	}
	return slices.IndexFunc(a.cands, func(c Candidate) bool { return c.Addr == a.prev.Addr })
}

func (a *attempt) try(ctx context.Context, i int) outcome {
	c := a.cands[i]
	o := a.verify(ctx, c)
	a.outcomes[i], a.tried[i] = o, true
	a.results = append(a.results, CandidateResult{Addr: c.Addr, Source: c.Source, OK: o.ok(), Reason: o.reason})
	return o
}

func (a *attempt) served(c Candidate) Result {
	return a.result(planner.ResolvedTarget{Addr: c.Addr, Reachable: true}, newBinding(a.route, c, a.now))
}

// boundFailed keeps the failing binding as the target. Without identity it is
// withdrawn; otherwise it stays unreachable.
func (a *attempt) boundFailed(o outcome) Result {
	target := planner.ResolvedTarget{Addr: a.prev.Addr, Withdrawn: o.verdict == notIdentified, Reason: o.reason}
	return a.result(target, a.prev.failing(a.now))
}

// result lists the candidates in the order tried, then those not tried.
func (a *attempt) result(target planner.ResolvedTarget, b *Binding) Result {
	out := a.results
	for i, c := range a.cands {
		if !a.tried[i] {
			out = append(out, CandidateResult{Addr: c.Addr, Source: c.Source, Reason: reasonNotTried})
		}
	}
	return Result{Target: target, Binding: b, Candidates: out}
}

// verify runs the checks on one candidate in order and stops at the first
// that fails.
func (a *attempt) verify(ctx context.Context, c Candidate) outcome {
	if why, denied := a.deny.Check(c.Addr); denied {
		return outcome{onDenylist, why}
	}
	if o := a.identify(ctx, c); !o.ok() {
		return o
	}
	return a.r.dial(ctx, c.Addr, a.route.Target.Port)
}

// identify checks that only this guest answers for c.Addr on its own network.
func (a *attempt) identify(ctx context.Context, c Candidate) outcome {
	if o := a.uniqueMAC(c.NIC.MAC); !o.ok() {
		return o
	}
	iface, o := a.arpInterface(ctx, c)
	switch {
	case !o.ok():
		return o
	case iface == "" && a.trusted(c):
		return outcome{}
	case iface == "":
		return lost("node has no address on %s in the guest's network", strings.Join(ifaceNames(c.NIC), " or "))
	}
	own := a.ownMACs(c.NIC)
	macs, o := a.answeredBy(ctx, iface, c.Addr, own)
	if !o.ok() || a.guest.Node != a.r.settings.LocalNode {
		return o
	}
	return a.forwarding(ctx, iface, c.NIC, macs, own)
}

// uniqueMAC fails when another running guest has mac configured too, unless
// the previous binding already ties it to this route: the earlier binding
// wins.
func (a *attempt) uniqueMAC(mac string) outcome {
	key, err := model.NormalizeMAC(mac)
	if err != nil || (a.prev != nil && sameMAC(a.prev.MAC, key)) {
		return outcome{}
	}
	for _, g := range a.snap.Guests {
		if g.Ref == a.guest.Ref || !g.Running {
			continue
		}
		if slices.ContainsFunc(g.NICs, func(n model.NIC) bool { return sameMAC(n.MAC, key) }) {
			return lost("MAC %s is also configured on %s", key, g.Ref)
		}
	}
	return outcome{}
}

// arpInterface returns the host interface on the NIC's bridge and VLAN that
// has an address in the candidate's network, or "" when there is none.
func (a *attempt) arpInterface(ctx context.Context, c Candidate) (string, outcome) {
	if !a.ifacesRead {
		a.ifaces, a.ifacesErr = a.r.prober.Interfaces(ctx)
		a.ifacesRead = true
	}
	if a.ifacesErr != nil {
		return "", probeError("listing host interfaces: %v", a.ifacesErr)
	}
	onLink := func(p netip.Prefix) bool { return p.Contains(c.Addr) }
	for _, name := range ifaceNames(c.NIC) {
		for _, ifc := range a.ifaces {
			if ifc.Name == name && slices.ContainsFunc(ifc.Addrs, onLink) {
				return name, outcome{}
			}
		}
	}
	return "", outcome{}
}

// ifaceNames lists the host interfaces that sit in the NIC's network: the
// bridge when untagged; for a VLAN, its interface on a VLAN-aware bridge or
// the bridge Proxmox creates for it.
func ifaceNames(nic model.NIC) []string {
	if nic.VLAN == 0 {
		return []string{nic.Bridge}
	}
	return []string{fmt.Sprintf("%s.%d", nic.Bridge, nic.VLAN), vlanBridge(nic)}
}

func vlanBridge(nic model.NIC) string {
	return fmt.Sprintf("%sv%d", nic.Bridge, nic.VLAN)
}

// trusted reports whether a candidate the node has no address next to may be
// served anyway: only an address from the Proxmox config, only when the admin
// allows it, and only inside the admin's ranges.
func (a *attempt) trusted(c Candidate) bool {
	s := a.r.settings
	inRange := func(p netip.Prefix) bool { return p.Contains(c.Addr) }
	return c.Source == FromStatic && s.TrustStatic && slices.ContainsFunc(s.TrustedCIDRs, inRange)
}

// ownMACs maps the MACs of this guest's NICs on nic's bridge and VLAN to the
// indexes of the NICs that have them.
func (a *attempt) ownMACs(nic model.NIC) map[string][]int {
	own := map[string][]int{}
	for _, n := range a.guest.NICs {
		if n.Bridge != nic.Bridge || n.VLAN != nic.VLAN {
			continue
		}
		if mac, err := model.NormalizeMAC(n.MAC); err == nil {
			own[mac] = append(own[mac], n.Index)
		}
	}
	return own
}

// answeredBy ARPs for addr and returns the MACs that answered. Every one of
// them must be in own and on no other running guest.
func (a *attempt) answeredBy(ctx context.Context, iface string, addr netip.Addr, own map[string][]int) ([]string, outcome) {
	raw, err := a.r.prober.ARP(ctx, iface, addr)
	if err != nil {
		return nil, probeError("ARP on %s: %v", iface, err)
	}
	macs := normalizeMACs(raw)
	if len(macs) == 0 {
		return nil, lost("no ARP answer on %s", iface)
	}
	for _, mac := range macs {
		if _, ok := own[mac]; !ok {
			return nil, lost("%s answered by %s, which is not this guest", addr, mac)
		}
	}
	for _, mac := range macs {
		if o := a.uniqueMAC(mac); !o.ok() {
			return nil, o
		}
	}
	return macs, outcome{}
}

// forwarding checks that the bridge has learned each MAC on the port of the
// guest NIC that has it.
func (a *attempt) forwarding(ctx context.Context, iface string, nic model.NIC, macs []string, own map[string][]int) outcome {
	bridge, vlan := nic.Bridge, nic.VLAN
	if nic.VLAN != 0 && iface == vlanBridge(nic) {
		bridge, vlan = iface, 0
	}
	for _, mac := range macs {
		port, found, err := a.r.prober.FDBPort(ctx, bridge, vlan, mac)
		switch {
		case err != nil:
			return probeError("forwarding table of %s: %v", bridge, err)
		case !found:
			return lost("MAC %s not seen on bridge %s", mac, bridge)
		case !slices.Contains(guestPorts(a.guest.Ref.VMID, own[mac]), port):
			return lost("MAC %s is on port %s, not on the guest's own port", mac, port)
		}
	}
	return outcome{}
}

// guestPorts names the bridge ports Proxmox creates for the guest NICs with
// the given indexes.
func guestPorts(vmid int, indexes []int) []string {
	out := make([]string, 0, 3*len(indexes))
	for _, n := range indexes {
		out = append(out,
			fmt.Sprintf("tap%di%d", vmid, n),
			fmt.Sprintf("fwpr%dp%d", vmid, n),
			fmt.Sprintf("veth%di%d", vmid, n),
		)
	}
	return out
}

// normalizeMACs returns the distinct MACs in raw, in order. One that cannot be
// parsed is kept as it is, so that it is reported as a stranger.
func normalizeMACs(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if mac, err := model.NormalizeMAC(s); err == nil {
			s = mac
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func sameMAC(a, b string) bool {
	x, errA := model.NormalizeMAC(a)
	y, errB := model.NormalizeMAC(b)
	return errA == nil && errB == nil && x == y
}
