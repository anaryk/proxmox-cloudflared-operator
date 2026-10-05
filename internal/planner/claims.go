package planner

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// Waiter is an owner that wants a hostname somebody else holds.
type Waiter struct {
	Owner     string    `json:"owner"`
	FirstSeen time.Time `json:"firstSeen"`
}

// Claim records who serves a hostname. It is persisted between cycles, which
// is what keeps the first claimant in place: a guest cloned together with its
// Notes asks for the same hostnames but never takes them over.
type Claim struct {
	Hostname     string     `json:"hostname"`
	Owner        string     `json:"owner"`
	Identity     string     `json:"identity,omitempty"`
	Since        time.Time  `json:"since"`
	MissingSince *time.Time `json:"missingSince,omitempty"` // set while the holder has no route for the hostname
	Waiting      []Waiter   `json:"waiting,omitempty"`
}

// ClaimInput is everything ResolveClaims needs.
//
// Routes must come from a complete inventory. An owner missing from Routes,
// and from Held, is taken to no longer claim its hostnames, so a partial
// inventory would start their grace and in the end hand them to someone else;
// when the inventory is incomplete, callers skip the call and keep the stored
// claims.
type ClaimInput struct {
	Routes   []model.Route
	Held     []HeldName        // hostnames owners still name without a route; see Collected.Held
	Claims   map[string]Claim  // by hostname
	Identity map[string]string // owner -> identity now; "" when unknown, as for manual routes
	// Refused are the names owners give that may take no claim, as
	// RefuseUnnamed, RefuseUnzoned and their two forms for Held took them
	// out: a claim of such an owner on the hostname ends at once, whatever
	// Held says.
	Refused []RouteStatus
	Now     time.Time
	Grace   time.Duration // how long a holder may be absent before it loses the hostname
}

// ClaimEventKind says what happened to a claim.
type ClaimEventKind string

const (
	ClaimTaken           ClaimEventKind = "claimed"
	ClaimIdentityChanged ClaimEventKind = "identity-changed"
	ClaimTransferred     ClaimEventKind = "transferred"
	ClaimReleased        ClaimEventKind = "released"
	ClaimConflict        ClaimEventKind = "conflict"
)

// ClaimEvent reports a change made by one ResolveClaims call.
type ClaimEvent struct {
	Kind     ClaimEventKind `json:"kind"`
	Hostname string         `json:"hostname"`
	Owner    string         `json:"owner"`
	Detail   string         `json:"detail"`
}

// ClaimResult is the outcome of ResolveClaims.
type ClaimResult struct {
	Winners   []model.Route    `json:"winners"`   // at most one per hostname, sorted by hostname
	Conflicts []model.Route    `json:"conflicts"` // sorted by hostname, then owner
	Claims    map[string]Claim `json:"claims"`    // state to persist
	Events    []ClaimEvent     `json:"events"`    // only for changes made in this call
}

// ResolveClaims decides which owner serves each hostname.
//
// The first owner to claim a hostname keeps it for as long as it keeps
// asking, whatever its identity does and whoever else turns up. If the holder
// stops asking, the hostname stays reserved for Grace so that a restart or a
// brief edit of the Notes loses nothing. After that it goes to the waiter
// that has been waiting longest, or is released when nobody wants it.
//
// A holder that still names the hostname in Held has not stopped asking: its
// entry is broken, or the policy cannot be read. It keeps the claim for as
// long as that lasts, and nobody serves the hostname meanwhile. A waiter that
// still names it keeps its place in line the same way. Held only ever keeps
// what exists: it never creates a claim and never lets a waiter win. A holder
// whose name for the hostname is in Refused loses the claim at once, as if its
// grace were over, whatever Held says.
func ResolveClaims(in ClaimInput) ClaimResult {
	r := resolver{
		in:      in,
		held:    make(map[HeldName]bool, len(in.Held)),
		refused: make(map[HeldName]string, len(in.Refused)),
		res: ClaimResult{
			Winners:   []model.Route{},
			Conflicts: []model.Route{},
			Claims:    make(map[string]Claim, len(in.Claims)),
			Events:    []ClaimEvent{},
		},
	}
	for _, h := range in.Held {
		r.held[h] = true
	}
	for _, st := range in.Refused {
		r.refused[HeldName{Hostname: st.Hostname, Owner: st.Owner}] = st.Reason
	}
	groups := groupClaimants(in.Routes)
	for _, host := range hostnamesOf(groups, in.Claims) {
		r.resolve(host, groups[host])
	}
	slices.SortStableFunc(r.res.Events, func(a, b ClaimEvent) int {
		return cmp.Or(
			strings.Compare(a.Hostname, b.Hostname),
			strings.Compare(string(a.Kind), string(b.Kind)),
			model.CompareOwners(a.Owner, b.Owner),
		)
	})
	return r.res
}

// claimant is an owner that has a route for a hostname.
type claimant struct {
	owner string
	route model.Route
}

// groupClaimants collects the claimants of each hostname in CompareOwners
// order. An owner counts once per hostname; its first route is the one used.
func groupClaimants(routes []model.Route) map[string][]claimant {
	groups := make(map[string][]claimant)
	for _, rt := range routes {
		owner := rt.Owner()
		if slices.ContainsFunc(groups[rt.Hostname], func(c claimant) bool { return c.owner == owner }) {
			continue
		}
		groups[rt.Hostname] = append(groups[rt.Hostname], claimant{owner: owner, route: rt})
	}
	for _, cs := range groups {
		slices.SortStableFunc(cs, func(a, b claimant) int { return model.CompareOwners(a.owner, b.owner) })
	}
	return groups
}

// hostnamesOf lists, in order, every hostname that has a claimant or a stored
// claim.
func hostnamesOf(groups map[string][]claimant, claims map[string]Claim) []string {
	all := make(map[string]struct{}, len(groups)+len(claims))
	for h := range groups {
		all[h] = struct{}{}
	}
	for h := range claims {
		all[h] = struct{}{}
	}
	return slices.Sorted(maps.Keys(all))
}

type resolver struct {
	in      ClaimInput
	held    map[HeldName]bool
	refused map[HeldName]string // why, of each name that may take no claim
	res     ClaimResult
}

// resolve settles one hostname. Claimants are in CompareOwners order.
func (r *resolver) resolve(host string, cs []claimant) {
	claim, ok := r.in.Claims[host]
	if !ok {
		r.firstClaim(host, cs)
		return
	}
	why, refused := r.refused[HeldName{Hostname: host, Owner: claim.Owner}]
	switch i := slices.IndexFunc(cs, func(c claimant) bool { return c.owner == claim.Owner }); {
	case i >= 0:
		r.keep(host, claim, cs, i)
	case refused:
		r.end(host, claim, cs, "may take no claim on it: "+why)
	case r.held[HeldName{Hostname: host, Owner: claim.Owner}]:
		r.hold(host, claim, cs)
	default:
		r.holderMissing(host, claim, cs)
	}
}

// firstClaim gives a hostname nobody holds to the first claimant.
func (r *resolver) firstClaim(host string, cs []claimant) {
	winner := cs[0]
	claim := Claim{Hostname: host, Owner: winner.owner, Identity: r.in.Identity[winner.owner], Since: r.in.Now}
	r.award(claim, winner, cs[1:], nil)
	r.event(ClaimTaken, host, winner.owner, "first claim on the hostname")
}

// keep leaves the hostname with its holder, cs[i], and records the others as
// conflicts. A changed identity is noted and nothing more: the holder is the
// same owner, so the hostname stays. An identity that is not known says
// nothing about the guest: it never replaces a stored one, and the first one
// that becomes known is stored without an event.
func (r *resolver) keep(host string, claim Claim, cs []claimant, i int) {
	holder := cs[i]
	next := Claim{Hostname: host, Owner: claim.Owner, Identity: claim.Identity, Since: claim.Since}
	switch now := r.in.Identity[claim.Owner]; {
	case now == "" || now == claim.Identity:
		// Nothing new is known.
	case claim.Identity == "":
		next.Identity = now
	default:
		next.Identity = now
		r.event(ClaimIdentityChanged, host, claim.Owner, fmt.Sprintf("identity changed from %q to %q, claim kept", claim.Identity, now))
	}
	r.award(next, holder, without(cs, i), claim.Waiting)
}

// hold keeps a claim whose holder has no route for the hostname but still
// names it. The claim stays as it is, no longer missing, and nobody wins the
// hostname; the other claimants wait as they would behind a present holder.
func (r *resolver) hold(host string, claim Claim, cs []claimant) {
	next := Claim{Hostname: host, Owner: claim.Owner, Identity: claim.Identity, Since: claim.Since}
	if next.Identity == "" {
		// As in keep, the first identity that becomes known is stored without
		// an event. A change of a known one is left for keep to report once
		// the route is back.
		next.Identity = r.in.Identity[claim.Owner]
	}
	next.Waiting = r.queue(host, claim.Owner, claim.Waiting, cs)
	r.res.Claims[host] = next
}

// holderMissing handles a stored claim whose holder has no route for the
// hostname and does not name it either.
func (r *resolver) holderMissing(host string, claim Claim, cs []claimant) {
	missing := r.in.Now
	// A time after Now means the clock stepped back; counting from it would
	// keep the hostname reserved for longer than the grace.
	if claim.MissingSince != nil && !claim.MissingSince.After(r.in.Now) {
		missing = *claim.MissingSince
	}
	absent := r.in.Now.Sub(missing)

	if absent < r.in.Grace {
		next := Claim{
			Hostname:     host,
			Owner:        claim.Owner,
			Identity:     claim.Identity,
			Since:        claim.Since,
			MissingSince: &missing,
			Waiting:      r.queue(host, claim.Owner, claim.Waiting, cs),
		}
		r.res.Claims[host] = next
		return
	}

	if len(cs) == 0 {
		r.event(ClaimReleased, host, claim.Owner, fmt.Sprintf("holder missing for %s and nobody else claims it", absent))
		return
	}
	r.handOn(host, claim, cs, fmt.Sprintf("previous holder %s was missing for %s", claim.Owner, absent))
}

// end ends at once a claim whose holder names the hostname where it may take
// no claim, as if its grace were over: why says so.
func (r *resolver) end(host string, claim Claim, cs []claimant, why string) {
	if len(cs) == 0 {
		r.event(ClaimReleased, host, claim.Owner, "it "+why)
		return
	}
	r.handOn(host, claim, cs, "previous holder "+claim.Owner+" "+why)
}

// handOn gives the hostname of claim, which its holder lost, to the claimant
// that has waited longest.
func (r *resolver) handOn(host string, claim Claim, cs []claimant, detail string) {
	i := r.longestWaiting(claim.Waiting, cs)
	winner := cs[i]
	next := Claim{Hostname: host, Owner: winner.owner, Identity: r.in.Identity[winner.owner], Since: r.in.Now}
	r.award(next, winner, without(cs, i), claim.Waiting)
	r.event(ClaimTransferred, host, winner.owner, detail)
}

// longestWaiting returns the index of the claimant that has been waiting the
// longest. A claimant that is not a waiter yet has been waiting since Now.
// Ties go to the earlier claimant, which is the first in CompareOwners order.
func (r *resolver) longestWaiting(prior []Waiter, cs []claimant) int {
	best, bestSeen := 0, r.firstSeen(prior, cs[0].owner)
	for i := 1; i < len(cs); i++ {
		if seen := r.firstSeen(prior, cs[i].owner); seen.Before(bestSeen) {
			best, bestSeen = i, seen
		}
	}
	return best
}

func (r *resolver) firstSeen(prior []Waiter, owner string) time.Time {
	if i := slices.IndexFunc(prior, func(w Waiter) bool { return w.Owner == owner }); i >= 0 {
		return prior[i].FirstSeen
	}
	return r.in.Now
}

// award stores claim for the winner and records the losers as waiting.
func (r *resolver) award(claim Claim, winner claimant, losers []claimant, prior []Waiter) {
	claim.Waiting = r.queue(claim.Hostname, winner.owner, prior, losers)
	r.res.Claims[claim.Hostname] = claim
	r.res.Winners = append(r.res.Winners, cloneRoute(winner.route))
}

// queue records losers as conflicts and returns them as the waiting list,
// longest wait first. Waiters already in prior keep their FirstSeen; each new
// one gets a conflict event. A waiter in prior that is not among the losers
// but still names the hostname in Held keeps its place too, though it is no
// conflict and cannot win until it has a route. Every other waiter in prior is
// dropped.
func (r *resolver) queue(host, holder string, prior []Waiter, losers []claimant) []Waiter {
	var waiting []Waiter
	for _, c := range losers {
		r.res.Conflicts = append(r.res.Conflicts, cloneRoute(c.route))
		i := slices.IndexFunc(prior, func(w Waiter) bool { return w.Owner == c.owner })
		if i >= 0 {
			waiting = append(waiting, prior[i])
			continue
		}
		waiting = append(waiting, Waiter{Owner: c.owner, FirstSeen: r.in.Now})
		r.event(ClaimConflict, host, c.owner, "hostname is held by "+holder)
	}
	for _, w := range prior {
		claims := slices.ContainsFunc(losers, func(c claimant) bool { return c.owner == w.Owner })
		if !claims && w.Owner != holder && r.held[HeldName{Hostname: host, Owner: w.Owner}] {
			waiting = append(waiting, w)
		}
	}
	slices.SortStableFunc(waiting, func(a, b Waiter) int {
		return cmp.Or(a.FirstSeen.Compare(b.FirstSeen), model.CompareOwners(a.Owner, b.Owner))
	})
	return waiting
}

func (r *resolver) event(kind ClaimEventKind, host, owner, detail string) {
	r.res.Events = append(r.res.Events, ClaimEvent{Kind: kind, Hostname: host, Owner: owner, Detail: detail})
}

// without returns cs minus element i, leaving cs itself alone.
func without(cs []claimant, i int) []claimant {
	return slices.Delete(slices.Clone(cs), i, i+1)
}

func cloneRoute(rt model.Route) model.Route {
	if rt.Guest != nil {
		guest := *rt.Guest
		rt.Guest = &guest
	}
	return rt
}
