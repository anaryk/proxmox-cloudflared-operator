package resolve

import (
	"maps"
	"net/netip"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// FromBinding marks the address of the previous binding when no source
// reports it any more. It is still verified first, so that an agent that
// stops answering for a while does not unpublish a verified route.
const FromBinding CandidateSource = "bound"

// Binding remembers the address verified for a guest NIC. VerifiedAt is the
// last time its identity was proven, whether or not the port answered then.
type Binding struct {
	Owner        string     `json:"owner"`
	Hostname     string     `json:"hostname"`
	Guest        string     `json:"guest"` // the guest the address was verified for
	Addr         netip.Addr `json:"addr"`
	MAC          string     `json:"mac"`
	VerifiedAt   time.Time  `json:"verifiedAt"`
	FailingSince *time.Time `json:"failingSince,omitempty"`
	// Withdrawn is set once the address has lost its proof: identity failed,
	// the guest stopped or vanished, or the last proof is too old or in doubt.
	// Only identity passing again lifts it, even if the port then fails.
	Withdrawn bool `json:"withdrawn,omitempty"`
	// Level is the level of the proof made at VerifiedAt. A binding stored
	// before levels were kept has none until its identity is proven again.
	Level Level `json:"level,omitempty"`
	// Since is when the route was bound to this address on this NIC; later
	// proofs of the same candidate keep it.
	Since time.Time `json:"since,omitzero"`
	// Bridge and Port are where the proof made at VerifiedAt found the MAC:
	// the bridge whose forwarding table placed it, and the port of the bridge
	// it was learned on. Both are empty for a proof that placed it nowhere.
	Bridge string `json:"bridge,omitempty"`
	Port   string `json:"port,omitempty"`
	// Ports are, by MAC, the ports of Bridge that same proof found every MAC
	// that answered for the address on, the NIC's own among them.
	Ports map[string]string `json:"ports,omitempty"`
}

// proof is what proved the identity of a candidate: its level, and where the
// forwarding table placed the MAC of its NIC and every MAC that answered.
type proof struct {
	level        Level
	bridge, port string
	ports        map[string]string
}

func guestOf(route model.Route) string {
	if route.Guest == nil {
		return ""
	}
	return route.Guest.String()
}

func newBinding(route model.Route, c Candidate, now time.Time, p proof) *Binding {
	return &Binding{
		Owner:      route.Owner(),
		Hostname:   route.Hostname,
		Guest:      guestOf(route),
		Addr:       c.Addr,
		MAC:        c.NIC.MAC,
		VerifiedAt: now,
		Level:      p.level,
		Since:      now,
		Bridge:     p.bridge,
		Port:       p.port,
		Ports:      maps.Clone(p.ports),
	}
}

// proven returns a copy of b renewed by a proof made at now.
func (b *Binding) proven(now time.Time, p proof) *Binding {
	c := b.clone()
	c.VerifiedAt, c.Withdrawn, c.Level, c.Bridge, c.Port = now, false, p.level, p.bridge, p.port
	c.Ports = maps.Clone(p.ports)
	return c
}

// boundSince is when b was bound. A binding stored before that was kept
// counts from its last proof.
func (b *Binding) boundSince() time.Time {
	if b.Since.IsZero() {
		return b.VerifiedAt
	}
	return b.Since
}

// settling reports whether b, bound at the latest, is too young at now to
// give way to a candidate proven higher. A Since after now, as after the
// clock was set back, says nothing and settles nothing.
func (b *Binding) settling(now time.Time, stickyFor time.Duration) bool {
	bound := now.Sub(b.boundSince())
	return bound >= 0 && bound < stickyFor
}

// Proven is the level b's last proof stands for, also once b is withdrawn.
// Every proof of a version that kept no level established at least that only
// the guest answers ARP for the address, so a binding without one counts as
// observed.
func (b *Binding) Proven() Level {
	if b.Level == "" {
		return LevelObserved
	}
	return b.Level
}

// appliesTo reports whether b was made for route. A binding of another owner
// or guest must never hand its address to whoever holds the hostname now.
func (b *Binding) appliesTo(route model.Route) bool {
	return b != nil && b.Owner == route.Owner() && b.Hostname == route.Hostname && b.Guest == guestOf(route)
}

// LostProof reports the address of prev when the call that returned res took
// it from the route because it lost its proof: the call withdrew it, or tried
// it first, as the bound address, found its identity failed, its proof in doubt
// or the address one that must never be served, and bound another. An address
// the route left for another reason, such as a candidate proven at a higher
// level, a port that does not answer or a route that names another address,
// did not lose its proof. A binding withdrawn before the call lost nothing in
// it, and one made for another route than route is not its to lose.
func LostProof(route model.Route, prev *Binding, res Result) (netip.Addr, bool) {
	if !prev.appliesTo(route) || prev.Withdrawn {
		return netip.Addr{}, false
	}
	b := res.Binding
	switch {
	case b != nil && b.Addr == prev.Addr:
		return prev.Addr, b.Withdrawn && res.Target.Withdrawn
	case len(res.Candidates) == 0:
		return netip.Addr{}, false
	}
	// The bound address is tried first; a failure that proved its identity
	// all the same has a level.
	first := res.Candidates[0]
	return prev.Addr, first.Addr == prev.Addr && !first.OK && first.Level == "" &&
		first.Reason != reasonNotTried && first.Reason != reasonTooMany
}

// clone returns a copy that shares no memory with b.
func (b *Binding) clone() *Binding {
	c := *b
	if b.FailingSince != nil {
		since := *b.FailingSince
		c.FailingSince = &since
	}
	c.Ports = maps.Clone(b.Ports)
	return &c
}

// failing returns a copy of b that has been failing since now, unless it
// already was failing.
func (b *Binding) failing(now time.Time) *Binding {
	c := b.clone()
	if c.FailingSince == nil {
		c.FailingSince = &now
	}
	return c
}

// withdrawn returns a failing copy of b that is withdrawn.
func (b *Binding) withdrawn(now time.Time) *Binding {
	c := b.failing(now)
	c.Withdrawn = true
	return c
}

// holds reports whether b, failing at now, is still kept without trying
// other candidates. A FailingSince after now, as after the clock was set
// back, says nothing about how long b has failed and counts as expired.
func (b *Binding) holds(now time.Time, stickyFor time.Duration) bool {
	since := now
	if b.FailingSince != nil {
		since = *b.FailingSince
	}
	failing := now.Sub(since)
	return failing >= 0 && failing < stickyFor
}

// target is what b says about its address when nothing new is known.
func (b *Binding) target(reason string) planner.ResolvedTarget {
	return planner.ResolvedTarget{
		Addr:      b.Addr,
		Reachable: !b.Withdrawn && b.FailingSince == nil,
		Withdrawn: b.Withdrawn,
		Reason:    reason,
	}
}

// boundCandidate returns the candidate that carries b: its address on the
// NIC that has its MAC now, with the source that reports it there, if any.
// It returns false when b no longer applies: no NIC of the guest has its
// MAC, or the route names another address or another NIC.
func (b *Binding) boundCandidate(route model.Route, guest model.Guest, cands []Candidate) (Candidate, bool) {
	if named, ok := namedAddr(route); ok && named != b.Addr {
		return Candidate{}, false
	}
	nics := sortedNICs(guest)
	i := slices.IndexFunc(nics, func(n model.NIC) bool { return sameMAC(n.MAC, b.MAC) })
	if i < 0 {
		return Candidate{}, false
	}
	if index, ok := parseNICName(route.Options.Via); ok && index != nics[i].Index {
		return Candidate{}, false
	}
	c := Candidate{Addr: b.Addr, NIC: copyNIC(nics[i]), Source: FromBinding}
	if j := slices.IndexFunc(cands, c.sameAs); j >= 0 {
		c.Source = cands[j].Source
	}
	return c, true
}

// sameAs reports whether o is c's address on c's NIC.
func (c Candidate) sameAs(o Candidate) bool {
	return o.Addr == c.Addr && o.NIC.Index == c.NIC.Index
}

// namedAddr returns the address a route names itself, as its target or as
// via=<ip>.
func namedAddr(route model.Route) (netip.Addr, bool) {
	if route.Target.Addr.IsValid() {
		return route.Target.Addr, true
	}
	addr, err := netip.ParseAddr(route.Options.Via)
	return addr, err == nil
}
