package engine

import (
	"cmp"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const issueWaitingApproval = "waiting for approval"

// collect turns the guests and the manual routes into candidate routes and
// drops those of guests that wait for approval. What it found claimed under a
// policy that could be read goes into the listing.
func (c *cycleRun) collect() bool {
	manual, err := c.e.d.Store.ManualRoutes()
	if err != nil {
		c.storeProblem("reading the manual routes", err)
		return false
	}
	c.manual = manual
	if c.settings.Admission == store.AdmissionApprove {
		approvals, err := c.e.d.Store.Approvals()
		if err != nil {
			c.storeProblem("reading the approvals", err)
			return false
		}
		c.approvals = approvals
	}
	var waiting []model.GuestRef
	c.col, waiting = c.collectFrom(c.snap)
	c.st.Issues = c.col.Issues
	c.st.Unapproved = make([]GuestView, 0, len(waiting))
	for _, ref := range waiting {
		c.st.Unapproved = append(c.st.Unapproved, *c.guestView(ref))
	}
	if c.col.PolicyInvalid {
		c.problem(problemPolicyInvalid)
		return false
	}
	c.listing = c.listing.withClaims(c.col)
	return true
}

// collectFrom collects the routes of a snapshot and, in admission mode
// approve, takes out those of the guests that wait for approval, which it
// returns too.
func (c *cycleRun) collectFrom(snap inventory.Snapshot) (planner.Collected, []model.GuestRef) {
	col := planner.Collect(snap.Guests, c.manual, planner.Settings{
		GateTag:    c.settings.GateTag,
		AllowHosts: c.settings.AllowHosts,
		DenyHosts:  c.settings.DenyHosts,
	})
	if c.approvals == nil {
		return col, nil
	}
	return admit(col, snap, c.approvals)
}

// admit takes the routes from guests whose identity is not approved, with
// one issue per guest, and keeps their hostnames as held names: a guest that
// waits for approval keeps its claims, served by nobody, as a guest whose
// entry is broken does. Manual routes are the admin's own and pass. An empty
// identity is never approved. It returns the guests that wait, sorted.
func admit(col planner.Collected, snap inventory.Snapshot, approvals map[string]string) (planner.Collected, []model.GuestRef) {
	waiting := make(map[model.GuestRef]bool)
	approved := func(owner string) bool {
		ref, err := model.ParseGuestRef(owner)
		if err != nil {
			return true
		}
		g, ok := snap.Guest(ref)
		if ok && g.Identity != "" && approvals[owner] == g.Identity {
			return true
		}
		waiting[ref] = true
		return false
	}
	routes := make([]model.Route, 0, len(col.Routes))
	held := slices.Clone(col.Held)
	for _, rt := range col.Routes {
		if approved(rt.Owner()) {
			routes = append(routes, rt)
			continue
		}
		held = append(held, planner.HeldName{Hostname: rt.Hostname, Owner: rt.Owner()})
	}
	slices.SortFunc(held, func(a, b planner.HeldName) int {
		return cmp.Or(strings.Compare(a.Hostname, b.Hostname), model.CompareOwners(a.Owner, b.Owner))
	})
	col.Routes, col.Held = routes, slices.Compact(held)
	issues := slices.Clone(col.Issues)
	refs := make([]model.GuestRef, 0, len(waiting))
	for ref := range waiting {
		issues = append(issues, planner.Issue{Guest: ref, Msg: issueWaitingApproval})
		refs = append(refs, ref)
	}
	slices.SortStableFunc(issues, planner.CompareIssues)
	col.Issues = issues
	slices.SortFunc(refs, func(a, b model.GuestRef) int { return model.CompareOwners(a.String(), b.String()) })
	return col, refs
}
