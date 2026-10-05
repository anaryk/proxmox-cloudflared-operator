package engine

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	issueWaitingApproval = "waiting for approval"
	whyAdmission         = "admission mode approve"
)

// collect turns the guests and the manual routes into candidate routes and
// drops those of guests that wait for approval, which the state lists with
// what they would publish.
func (c *cycleRun) collect() bool {
	manual, approvals, doing, err := c.e.routeSources()
	if err != nil {
		c.hold(c.storeProblem(doing, err))
		return false
	}
	c.manual, c.approvals = manual, approvals
	c.st.Admission, c.st.GateTagged = c.settings.Admission, 0
	for _, g := range c.snap.Guests {
		if g.HasTag(c.settings.GateTag) {
			c.st.GateTagged++
		}
	}
	var waiting []waitingGuest
	c.col, waiting = c.collectFrom(c.snap)
	c.st.Issues = c.col.Issues
	c.st.Unapproved = make([]UnapprovedGuest, 0, len(waiting))
	for _, w := range waiting {
		v := UnapprovedGuest{GuestView: *c.guestView(w.ref), Hostnames: w.hostnames, Why: []string{whyAdmission}}
		if g, ok := c.snap.Guest(w.ref); ok {
			v.Identity = g.Identity
		}
		c.st.Unapproved = append(c.st.Unapproved, v)
	}
	if c.col.PolicyInvalid {
		c.hold(c.problem(problemPolicyInvalid))
		return false
	}
	return true
}

// routeSources reads what the routes are collected from besides the guests:
// the manual routes and the approvals, which admit guests in admission mode
// approve and release what waits at observed in either mode. On an error,
// doing says what was being read.
func (e *Engine) routeSources() (manual []model.Route, approvals map[string]store.Approval, doing string, err error) {
	if manual, err = e.d.Store.ManualRoutes(); err != nil {
		return nil, nil, "reading the manual routes", err
	}
	if approvals, err = e.d.Store.Approvals(); err != nil {
		return nil, nil, "reading the approvals", err
	}
	return manual, approvals, "", nil
}

// collectFrom collects the routes of a snapshot as the cycle does.
func (c *cycleRun) collectFrom(snap inventory.Snapshot) (planner.Collected, []waitingGuest) {
	col, waiting := collectRoutes(snap, c.manual, c.settings, c.approvals)
	return c.dropTenants(col), waiting
}

// dropTenants takes the routes of the guests configured with a MAC of the
// appliance out, with an issue for each guest, and keeps their hostnames as
// held names: what answers on such a guest's address may be the appliance.
func (c *cycleRun) dropTenants(col planner.Collected) planner.Collected {
	if len(c.tenants) == 0 {
		return col
	}
	self := model.GuestRef{Kind: model.KindLXC, VMID: c.install.Appliance.VMID}
	routes := make([]model.Route, 0, len(col.Routes))
	held := slices.Clone(col.Held)
	issued := map[model.GuestRef]bool{}
	issues := slices.Clone(col.Issues)
	for _, rt := range col.Routes {
		// A manual route is the admin's own, whatever guest it names.
		ref, err := model.ParseGuestRef(rt.Owner())
		mac, tenant := c.tenants[ref]
		if err != nil || !tenant {
			routes = append(routes, rt)
			continue
		}
		held = append(held, planner.HeldName{Hostname: rt.Hostname, Owner: rt.Owner()})
		if !issued[ref] {
			issued[ref] = true
			issues = append(issues, planner.Issue{Guest: ref, Msg: fmt.Sprintf(issueTenant, mac, self)})
		}
	}
	slices.SortFunc(held, func(a, b planner.HeldName) int {
		return cmp.Or(strings.Compare(a.Hostname, b.Hostname), model.CompareOwners(a.Owner, b.Owner))
	})
	slices.SortStableFunc(issues, planner.CompareIssues)
	col.Routes, col.Held, col.Issues = routes, slices.Compact(held), issues
	return col
}

// collectRoutes collects the routes of a snapshot and, in admission mode
// approve, takes out those of the guests that wait for approval, which it
// returns too.
func collectRoutes(snap inventory.Snapshot, manual []model.Route, s store.Settings, approvals map[string]store.Approval) (planner.Collected, []waitingGuest) {
	col := planner.Collect(snap.Guests, manual, planner.Settings{
		GateTag:              s.GateTag,
		AllowHosts:           s.AllowHosts,
		DenyHosts:            s.DenyHosts,
		MaxHostnamesPerGuest: s.MaxHostnamesPerGuest,
	})
	if s.Admission != store.AdmissionApprove {
		return col, nil
	}
	return admit(col, snap, approvals)
}

// waitingGuest is a guest that waits for approval, with the hostnames of the
// routes it would publish.
type waitingGuest struct {
	ref       model.GuestRef
	hostnames []string
}

// admit takes the routes from guests whose identity is not approved, with
// one issue per guest, and keeps their hostnames as held names: a guest that
// waits for approval keeps its claims, served by nobody, as a guest whose
// entry is broken does. Manual routes are the admin's own and pass. An empty
// identity is never approved. It returns the guests that wait, in the order
// of their owners.
func admit(col planner.Collected, snap inventory.Snapshot, approvals map[string]store.Approval) (planner.Collected, []waitingGuest) {
	waiting := make(map[model.GuestRef][]string)
	approved := func(rt model.Route) bool {
		ref, err := model.ParseGuestRef(rt.Owner())
		if err != nil {
			return true
		}
		g, ok := snap.Guest(ref)
		if ok && g.Identity != "" && approvals[rt.Owner()].Identity == g.Identity {
			return true
		}
		waiting[ref] = append(waiting[ref], rt.Hostname)
		return false
	}
	routes := make([]model.Route, 0, len(col.Routes))
	held := slices.Clone(col.Held)
	for _, rt := range col.Routes {
		if approved(rt) {
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
	out := make([]waitingGuest, 0, len(waiting))
	for _, ref := range slices.SortedFunc(maps.Keys(waiting), func(a, b model.GuestRef) int {
		return model.CompareOwners(a.String(), b.String())
	}) {
		issues = append(issues, planner.Issue{Guest: ref, Msg: issueWaitingApproval})
		out = append(out, waitingGuest{ref: ref, hostnames: slices.Compact(slices.Sorted(slices.Values(waiting[ref])))})
	}
	slices.SortStableFunc(issues, planner.CompareIssues)
	col.Issues = issues
	return col, out
}
