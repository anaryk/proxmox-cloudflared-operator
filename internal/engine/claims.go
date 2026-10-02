package engine

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// The states of a claim, as the last cycle that settled the claims left it.
const (
	ClaimServing  = "serving"  // its holder won the hostname, and nobody else wants it
	ClaimConflict = "conflict" // its holder won the hostname, and others wait for it
	ClaimHeld     = "held"     // nobody won the hostname: nobody serves it
	ClaimPending  = "pending"  // another owner won the hostname: the claim moved since
	ClaimUnknown  = "unknown"  // no cycle of this process has settled the claims yet
)

// ClaimView is a claim on a hostname as the store keeps it, with who holds it
// and who waits for it.
type ClaimView struct {
	Hostname     string         `json:"hostname"`
	Holder       string         `json:"holder"`
	Guest        *GuestView     `json:"guest,omitempty"` // the holder, when it is a guest
	Since        time.Time      `json:"since"`
	MissingSince *time.Time     `json:"missingSince,omitempty"` // since when its holder no longer asks for it
	State        string         `json:"state"`                  // "serving", "conflict", "held", "pending" or "unknown"
	Waiting      []ClaimantView `json:"waiting"`                // longest waiting first
}

// ClaimantView is an owner that waits for a hostname somebody else holds.
type ClaimantView struct {
	Owner string     `json:"owner"`
	Guest *GuestView `json:"guest,omitempty"`
	Since time.Time  `json:"since"`
}

// Claims returns the claims the store keeps, by hostname. The state of each
// is what the last cycle that settled the claims found, and unknown before
// one has; the guests are named as the last listing named them.
func (e *Engine) Claims() ([]ClaimView, error) {
	claims, err := e.d.Store.Claims()
	if err != nil {
		return nil, fmt.Errorf("reading the claims: %w", err)
	}
	e.stateMu.RLock()
	l, served := e.listed, e.served
	e.stateMu.RUnlock()

	out := make([]ClaimView, 0, len(claims))
	for _, host := range slices.Sorted(maps.Keys(claims)) {
		c := claims[host]
		v := ClaimView{
			Hostname: host, Holder: c.Owner, Guest: l.guestView(c.Owner), Since: c.Since,
			State: claimState(c, served), Waiting: make([]ClaimantView, 0, len(c.Waiting)),
		}
		if c.MissingSince != nil {
			missing := *c.MissingSince
			v.MissingSince = &missing
		}
		for _, w := range c.Waiting {
			v.Waiting = append(v.Waiting, ClaimantView{Owner: w.Owner, Guest: l.guestView(w.Owner), Since: w.FirstSeen})
		}
		out = append(out, v)
	}
	return out, nil
}

// claimState is the state of a stored claim against the owners that won the
// hostnames when the claims were last settled; served is nil before that.
func claimState(c planner.Claim, served map[string]string) string {
	winner, won := served[c.Hostname]
	switch {
	case served == nil:
		return ClaimUnknown
	case !won:
		return ClaimHeld
	case winner != c.Owner:
		return ClaimPending
	case len(c.Waiting) > 0:
		return ClaimConflict
	}
	return ClaimServing
}

// ResolveClaim hands the claim on a hostname to owner, which must not hold it
// already and has to claim it now, with a route or a name its Notes hold:
// the inventory is asked afresh, as the last cycle may be a poll interval
// old, and a hostname is moved only to an owner a complete listing shows
// claiming it. The holder waits for it from then on, in the place in line its
// own claim gives it, so that the hostname comes back to it when owner stops
// asking.
//
// Nothing else changes: the claim rules keep the hostname with owner for as
// long as owner asks for it, so the old holder and any clone of it wait as
// every other claimant does. The move is saved before ResolveClaim returns,
// and the next cycle, which it asks for, serves the hostname from owner.
func (e *Engine) ResolveClaim(ctx context.Context, name, owner string) error {
	host, err := hostname.Normalize(name)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if !validOwner(owner) {
		return fmt.Errorf("%w: %q is no owner: name a guest, as qemu/101 or lxc/200, or a manual route, as manual/<id>", ErrInvalid, owner)
	}
	if err := e.acquire(ctx); err != nil {
		return err
	}
	defer e.release()

	claims, err := e.d.Store.Claims()
	if err != nil {
		return fmt.Errorf("reading the claims: %w", err)
	}
	old, ok := claims[host]
	switch {
	case !ok:
		return fmt.Errorf("%w: nobody holds a claim on %s", ErrNotFound, host)
	case old.Owner == owner:
		return fmt.Errorf("%w: %s holds %s already", ErrInvalid, owner, host)
	}
	now, err := e.claimantsNow(ctx, host)
	if err != nil {
		return err
	}
	if !now.claims(host, owner) {
		return fmt.Errorf("%w: %s no longer claims %s: it has no route for it and does not name it", ErrRefused, owner, host)
	}

	claims[host] = planner.Claim{
		Hostname: host,
		Owner:    owner,
		Identity: now.identity(owner),
		Since:    e.d.Now(),
		Waiting:  movedWaiting(old, owner),
	}
	if err := e.d.Store.SaveClaims(claims); err != nil {
		return fmt.Errorf("saving the claims: %w", err)
	}
	e.adminEvent(host, fmt.Sprintf("the claim on %s was moved from %s to %s by the admin; %s waits for it from now on",
		host, old.Owner, owner, old.Owner))
	e.Trigger()
	return nil
}

// claimantsNow lists the guests afresh and works out who claims what, as a
// cycle does: the second look an admin action takes before it moves a
// hostname, as the DNS run takes one before a delete. A listing that is not
// complete, or settings with a policy that cannot be read, tell nothing, and
// the action is refused. The caller holds the cycle lock.
func (e *Engine) claimantsNow(ctx context.Context, host string) (listing, error) {
	s, err := e.d.Store.Settings()
	if err != nil {
		return listing{}, fmt.Errorf("reading the settings: %w", err)
	}
	manual, approvals, doing, err := e.routeSources(s)
	if err != nil {
		return listing{}, fmt.Errorf("%s: %w", doing, err)
	}
	ctx, cancel := e.timeout(ctx, refreshTimeout)
	defer cancel()
	snap := e.d.Inventory.Refresh(ctx)
	if !snap.Complete {
		return listing{}, fmt.Errorf("%w: the inventory is incomplete (%s), so who claims %s now is not known; "+
			"try again once it is complete", ErrRefused, strings.Join(snap.Problems, "; "), host)
	}
	col, _ := collectRoutes(snap, manual, s, approvals)
	if col.PolicyInvalid {
		return listing{}, fmt.Errorf("%w: the settings contain an invalid allow or deny pattern, so who claims %s is not known",
			ErrRefused, host)
	}
	return listingOf(snap).withClaims(col), nil
}

// movedWaiting is the line of those that wait for a hostname once its claim
// went to owner: owner is out of it, and the old holder is in it in the place
// its claim gives it, as one that has asked since the claim began.
func movedWaiting(old planner.Claim, owner string) []planner.Waiter {
	waiting := slices.DeleteFunc(slices.Clone(old.Waiting), func(w planner.Waiter) bool {
		return w.Owner == owner || w.Owner == old.Owner
	})
	waiting = append(waiting, planner.Waiter{Owner: old.Owner, FirstSeen: old.Since})
	slices.SortStableFunc(waiting, func(a, b planner.Waiter) int {
		return cmp.Or(a.FirstSeen.Compare(b.FirstSeen), model.CompareOwners(a.Owner, b.Owner))
	})
	return waiting
}

// validOwner reports whether owner is written as the owner of a route is: a
// guest in its one form, or a manual route.
func validOwner(owner string) bool {
	if _, err := model.ParseGuestRef(owner); err == nil {
		return true
	}
	id, ok := strings.CutPrefix(owner, model.ManualPrefix)
	return ok && id != "" && strings.TrimSpace(id) == id
}
