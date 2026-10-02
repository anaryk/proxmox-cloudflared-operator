package engine

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// The modes of State.Mode, and the verdicts of State.WriterVerdict.
const (
	ModeObserve = "observe"
	ModeEnforce = "enforce"

	VerdictOK      = "ok"
	VerdictStale   = "stale"
	VerdictForeign = "foreign"
	VerdictUnknown = "unknown"

	// RouteFrozen is the state of a route whose account is frozen: what its
	// tunnel serves for it is not known.
	RouteFrozen planner.RouteState = "frozen"
)

// RouteView is a route as the plan left it, with what resolution found.
type RouteView struct {
	planner.RouteStatus
	Guest      *GuestView                `json:"guest,omitempty"` // nil for a route without a guest
	Candidates []resolve.CandidateResult `json:"candidates,omitempty"`
	// Account and Rule are, for a route that won its hostname, the account
	// whose tunnel carries the hostname and the rule the plan gives it there:
	// the one that serves it, or the one that answers 503 for it. Both are
	// empty for a route that lost its hostname or has no tunnel.
	Account string               `json:"accountId,omitempty"`
	Rule    *planner.IngressRule `json:"rule,omitempty"`
}

// TunnelView is a tunnel of the install as the last cycle found it, or, when
// Unchecked, as an earlier cycle found it or only as the plan wants it.
type TunnelView struct {
	reconcile.TunnelState
	// Held says in words why the tunnel is left as it is: for a tunnel the
	// cycle looked up without bringing it in line, that its account is
	// frozen, that it serves no zone pco sees, or that no credential sees
	// its account; for an Unchecked one, why the cycle did not check it.
	// Empty for a tunnel the cycle reconciled. Read Unchecked, not the
	// words, to tell the two apart.
	Held string `json:"held,omitempty"`
	// Unchecked says that the last cycle did not check Cloudflare, so that
	// nothing the view shows of the tunnel, its connector or its records is
	// known to be there now; such a tunnel is never Verified. State.Hold
	// says why.
	Unchecked bool `json:"unchecked"`
}

// GuestView names a guest: its reference, as an issue names it, and its name.
type GuestView struct {
	model.GuestRef
	Name string `json:"name,omitempty"`
}

// CredentialView is a stored Cloudflare credential without its token.
type CredentialView struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
	// Checked says whether Report holds a check: the last one run, also by an
	// earlier process of the daemon; zero when there was none.
	Checked bool               `json:"checked"`
	Report  credentials.Report `json:"report,omitzero"`
}

// State is what the last cycle found and did. Every slice is sorted, so that
// two cycles over the same world give equal states.
type State struct {
	At time.Time `json:"at,omitzero"` // when the cycle began
	// FinishedAt is when the cycle ended; zero before the first cycle.
	FinishedAt  time.Time            `json:"finishedAt,omitzero"`
	Mode        string               `json:"mode"`     // "observe" or "enforce"
	Complete    bool                 `json:"complete"` // inventory completeness
	Routes      []RouteView          `json:"routes"`   // by hostname, then owner
	Issues      []planner.Issue      `json:"issues"`   // by guest, then position
	Tunnels     []TunnelView         `json:"tunnels"`  // by account id
	Connectors  []connector.Status   `json:"connectors"`
	Credentials []CredentialView     `json:"credentials"`
	Actions     []reconcile.Action   `json:"actions"` // tunnels by account, then records by zone and name
	Conflicts   []reconcile.Conflict `json:"conflicts"`
	Lost        []string             `json:"lost"`
	Problems    []string             `json:"problems"`
	// WriterVerdict is what the cycle found of the writer: "ok", "stale" or
	// "foreign" as the reconcilers judged it, or "unknown" when leader.json
	// could not be read or used. A cycle that did not get as far keeps the
	// last one.
	WriterVerdict string `json:"writerVerdict"`
	// Hold says why the last cycle did not check Cloudflare: the reason of
	// the step that held it, or ended it before its DNS run looked at the
	// records as the writer. Every tunnel then is Unchecked. Empty when the
	// cycle checked, and before the first cycle.
	Hold string `json:"hold,omitempty"`
	// Profile is the profile of the install, "host" or "appliance". It is
	// empty until a cycle has read the install.
	Profile string `json:"profile,omitempty"`
	// Waiting is what a confirmation would accept now, as the problems tell
	// it in words; [] when nothing. Offer names exactly this Waiting, and a
	// confirmation must quote it; it is empty when nothing waits.
	Waiting []Waiting `json:"waiting"`
	Offer   string    `json:"offer,omitempty"`
	// Unapproved are the guests whose routes are held until an admin approves
	// them, in admission mode approve; [] in mode tag.
	Unapproved []UnapprovedGuest `json:"unapproved"`
	// Egress is the egress filter as the daemon last found it; empty until
	// it has looked.
	Egress EgressView `json:"egress,omitzero"`
}

// UnapprovedGuest is a guest whose routes wait for an approval: the identity
// an approval would be of, and the hostnames the guest would publish.
type UnapprovedGuest struct {
	GuestView
	Identity  string   `json:"identity,omitempty"`
	Hostnames []string `json:"hostnames"`
}

func emptyState() State {
	return State{Mode: ModeObserve, WriterVerdict: VerdictOK}.normalized()
}

// carried is the start of the next state: what the cycle does not find out
// again stays as the last cycle left it. Actions, problems, what waits and the
// hold are the cycle's own.
func (s State) carried(at time.Time) State {
	next := s.clone()
	next.At = at
	next.Complete = false
	next.Actions = nil
	next.Problems = nil
	next.Waiting = nil
	next.Offer = ""
	next.Hold = ""
	return next
}

// clone copies s down to the slices and pointers its parts hold, so that a
// copy handed out shares nothing with the state the engine keeps.
func (s State) clone() State {
	s.Routes = slices.Clone(s.Routes)
	for i := range s.Routes {
		s.Routes[i].Warnings = slices.Clone(s.Routes[i].Warnings)
		s.Routes[i].Candidates = slices.Clone(s.Routes[i].Candidates)
		if g := s.Routes[i].Guest; g != nil {
			copied := *g
			s.Routes[i].Guest = &copied
		}
		if r := s.Routes[i].Rule; r != nil {
			copied := *r
			s.Routes[i].Rule = &copied
		}
	}
	s.Issues = slices.Clone(s.Issues)
	s.Tunnels = slices.Clone(s.Tunnels)
	s.Connectors = slices.Clone(s.Connectors)
	s.Credentials = slices.Clone(s.Credentials)
	for i := range s.Credentials {
		s.Credentials[i].Report = cloneReport(s.Credentials[i].Report)
	}
	s.Actions = slices.Clone(s.Actions)
	s.Conflicts = slices.Clone(s.Conflicts)
	s.Lost = slices.Clone(s.Lost)
	s.Problems = slices.Clone(s.Problems)
	s.Waiting = cloneWaiting(s.Waiting)
	s.Unapproved = slices.Clone(s.Unapproved)
	for i := range s.Unapproved {
		s.Unapproved[i].Hostnames = slices.Clone(s.Unapproved[i].Hostnames)
	}
	return s
}

// shownReport is a report as a view shows it: a copy whose lists are empty
// rather than missing.
func shownReport(r credentials.Report) credentials.Report {
	r = cloneReport(r)
	r.Accounts, r.Zones, r.Checks, r.Leftovers = nonNil(r.Accounts), nonNil(r.Zones), nonNil(r.Checks), nonNil(r.Leftovers)
	return r
}

func cloneReport(r credentials.Report) credentials.Report {
	if r.Token.ExpiresOn != nil {
		at := *r.Token.ExpiresOn
		r.Token.ExpiresOn = &at
	}
	r.Accounts = slices.Clone(r.Accounts)
	r.Zones = slices.Clone(r.Zones)
	r.Checks = slices.Clone(r.Checks)
	r.Leftovers = slices.Clone(r.Leftovers)
	return r
}

// normalized sorts what has no order of its own and makes every slice
// non-nil, so that a state reads the same in JSON whatever led to it.
func (s State) normalized() State {
	s.Problems = slices.Compact(slices.Sorted(slices.Values(s.Problems)))
	s.Lost = slices.Compact(slices.Sorted(slices.Values(s.Lost)))
	slices.SortStableFunc(s.Tunnels, func(a, b TunnelView) int {
		return cmp.Or(cmp.Compare(a.AccountID, b.AccountID), cmp.Compare(a.ID, b.ID))
	})
	slices.SortStableFunc(s.Connectors, func(a, b connector.Status) int { return cmp.Compare(a.TunnelID, b.TunnelID) })
	slices.SortStableFunc(s.Credentials, func(a, b CredentialView) int { return cmp.Compare(a.ID, b.ID) })
	s.Routes = nonNil(s.Routes)
	s.Issues = nonNil(s.Issues)
	s.Tunnels = nonNil(s.Tunnels)
	s.Connectors = nonNil(s.Connectors)
	s.Credentials = nonNil(s.Credentials)
	s.Actions = nonNil(s.Actions)
	s.Conflicts = nonNil(s.Conflicts)
	s.Lost = nonNil(s.Lost)
	s.Problems = nonNil(s.Problems)
	s.Waiting = nonNil(s.Waiting)
	s.Unapproved = nonNil(s.Unapproved)
	return s
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func modeName(observe bool) string {
	if observe {
		return ModeObserve
	}
	return ModeEnforce
}

func verdictName(v reconcile.WriterVerdict) string {
	switch v {
	case reconcile.WriterStale:
		return VerdictStale
	case reconcile.WriterForeign:
		return VerdictForeign
	}
	return VerdictOK
}

// routeViews pairs every route status of the plan with its guest and, for the
// route that won its hostname, the candidates resolution tried.
func (c *cycleRun) routeViews() []RouteView {
	type key struct{ host, owner string }
	routes := make(map[key]model.Route, len(c.claims.Winners)+len(c.claims.Conflicts))
	for _, rt := range append(slices.Clone(c.claims.Winners), c.claims.Conflicts...) {
		routes[key{rt.Hostname, rt.Owner()}] = rt
	}
	winner := make(map[string]string, len(c.claims.Winners))
	for _, rt := range c.claims.Winners {
		winner[rt.Hostname] = rt.Owner()
	}
	type planned struct {
		account string
		rule    planner.IngressRule
	}
	rules := make(map[string]planned)
	for _, t := range c.plan.Tunnels {
		for _, r := range t.Rules {
			if r.Hostname != "" {
				rules[r.Hostname] = planned{account: t.AccountID, rule: r}
			}
		}
	}

	out := make([]RouteView, 0, len(c.plan.Routes))
	for _, st := range c.plan.Routes {
		if why, frozen := c.frozenFor(st); frozen {
			st.State, st.Reason, st.Service = RouteFrozen, "account frozen: "+why, ""
		}
		v := RouteView{RouteStatus: st}
		if p, ok := rules[st.Hostname]; ok && st.State != planner.StateConflict {
			rule := p.rule
			v.Account, v.Rule = p.account, &rule
		}
		rt, ok := routes[key{st.Hostname, st.Owner}]
		switch {
		case ok && rt.Guest != nil:
			v.Guest = c.guestView(*rt.Guest)
		case !ok:
			// A claim nobody serves: its owner is a guest or a manual route.
			if ref, err := model.ParseGuestRef(st.Owner); err == nil {
				v.Guest = c.guestView(ref)
			}
		}
		if res, ok := c.results[st.Hostname]; ok && winner[st.Hostname] == st.Owner {
			v.Candidates = slices.Clone(res.Candidates)
		}
		out = append(out, v)
	}
	return out
}

// frozenFor says why the route's account is frozen, when it is. A route the
// planner gave no zone, as one in a zone two credentials see, is matched to
// the zone of its name. A route that lost its hostname to another owner keeps
// its state: it is served by nobody either way.
func (c *cycleRun) frozenFor(st planner.RouteStatus) (string, bool) {
	if st.State == planner.StateConflict || len(c.zones.frozen) == 0 {
		return "", false
	}
	zone := st.Zone
	names := make([]string, 0, len(c.zones.planned))
	for _, z := range c.zones.planned {
		names = append(names, z.Name)
	}
	if zone == "" {
		zone, _ = hostname.MatchZone(st.Hostname, names)
	}
	for _, z := range c.zones.planned {
		if z.Name == zone && c.zones.frozen[z.AccountID] {
			return c.zones.frozenWhy[z.AccountID], true
		}
	}
	return "", false
}

// guestView names a guest of the snapshot.
func (c *cycleRun) guestView(ref model.GuestRef) *GuestView {
	v := &GuestView{GuestRef: ref}
	if g, ok := c.snap.Guest(ref); ok {
		v.Name = strings.TrimSpace(g.Name)
	}
	return v
}

// credentialViews lists the stored credentials with the last report of each.
func (e *Engine) credentialViews(ids []credentialInfo) []CredentialView {
	e.repMu.Lock()
	defer e.repMu.Unlock()
	out := make([]CredentialView, 0, len(ids))
	for _, c := range ids {
		v := CredentialView{ID: c.id, Label: c.label, Kind: c.kind}
		if r, checked := e.reports[c.id]; checked {
			v.Checked, v.Report = true, shownReport(r)
		}
		out = append(out, v)
	}
	return out
}

// credentialInfo is what a view shows of a stored credential.
type credentialInfo struct{ id, label, kind string }
