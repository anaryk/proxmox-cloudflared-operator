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
// drops those of guests that wait for approval.
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
	c.col = c.collectFrom(c.snap)
	c.st.Issues = c.col.Issues
	if c.col.PolicyInvalid {
		c.problem(problemPolicyInvalid)
		return false
	}
	return true
}

func (c *cycleRun) collectFrom(snap inventory.Snapshot) planner.Collected {
	col := planner.Collect(snap.Guests, c.manual, planner.Settings{
		GateTag:    c.settings.GateTag,
		AllowHosts: c.settings.AllowHosts,
		DenyHosts:  c.settings.DenyHosts,
	})
	if c.approvals != nil {
		col = admit(col, snap, c.approvals)
	}
	return col
}

// admit takes the routes from guests whose identity is not approved, with
// one issue per guest, and keeps their hostnames as held names: a guest that
// waits for approval keeps its claims, served by nobody, as a guest whose
// entry is broken does. Manual routes are the admin's own and pass. An empty
// identity is never approved.
func admit(col planner.Collected, snap inventory.Snapshot, approvals map[string]string) planner.Collected {
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
	for ref := range waiting {
		issues = append(issues, planner.Issue{Guest: ref, Msg: issueWaitingApproval})
	}
	slices.SortStableFunc(issues, planner.CompareIssues)
	col.Issues = issues
	return col
}
