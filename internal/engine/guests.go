package engine

import (
	"fmt"
	"maps"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/annotation"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// maxAnnotation is how many characters of the route block of a guest
// Annotation returns at most.
const maxAnnotation = 8192

// The values of GuestListView.Approval.
const (
	ApprovalApproved  = "approved"   // approved in the identity the guest has
	ApprovalWaiting   = "waiting"    // admission mode approve, and no approval
	ApprovalChanged   = "changed"    // approved in another identity than it has
	ApprovalNotNeeded = "not-needed" // admission mode tag, or no gate tag
)

// GuestListView is a guest as the last listing showed it, with how many
// routes and issues the last cycle found of it.
type GuestListView struct {
	Ref      string `json:"ref"`
	Name     string `json:"name,omitempty"`
	Node     string `json:"node"`
	Running  bool   `json:"running"`
	Tagged   bool   `json:"tagged"`
	Identity string `json:"identity,omitempty"`
	Approval string `json:"approval"` // "approved", "waiting", "changed", "not-needed"
	Routes   int    `json:"routes"`
	Issues   int    `json:"issues"`
}

// Guests returns the guests of the last listing, in the order of their
// owners; none when the last cycle did not list every guest.
func (e *Engine) Guests() ([]GuestListView, error) {
	s, err := e.d.Store.Settings()
	if err != nil {
		return nil, fmt.Errorf("reading the settings: %w", err)
	}
	approvals, err := e.d.Store.Approvals()
	if err != nil {
		return nil, fmt.Errorf("reading the approvals: %w", err)
	}
	l := e.lastListing()
	st := e.State()
	routes := make(map[string]int)
	for _, r := range st.Routes {
		routes[r.Owner]++
	}
	issues := make(map[model.GuestRef]int)
	for _, is := range guestIssues(st.Issues) {
		issues[is.Guest]++
	}
	refs := slices.SortedFunc(maps.Keys(l.guests), func(a, b model.GuestRef) int {
		return model.CompareOwners(a.String(), b.String())
	})
	out := make([]GuestListView, 0, len(refs))
	for _, ref := range refs {
		g := l.guests[ref]
		v := GuestListView{
			Ref: ref.String(), Name: g.name, Node: g.node, Running: g.running, Tagged: slices.Contains(g.tags, s.GateTag),
			Identity: g.identity, Routes: routes[ref.String()], Issues: issues[ref],
		}
		a, approved := approvals[v.Ref]
		v.Approval = approvalOf(s.Admission, v.Tagged, a, approved, g.identity)
		out = append(out, v)
	}
	return out, nil
}

// approvalOf says where a guest stands with its approval.
func approvalOf(mode string, tagged bool, a store.Approval, approved bool, identity string) string {
	switch {
	case approved && identity != "" && a.Identity == identity:
		return ApprovalApproved
	case approved:
		return ApprovalChanged
	case tagged && mode == store.AdmissionApprove:
		return ApprovalWaiting
	}
	return ApprovalNotNeeded
}

// guestIssues are the issues of the Notes of guests, without the one every
// guest that waits for approval has: it is no fault of its Notes.
func guestIssues(issues []planner.Issue) []planner.Issue {
	return slices.DeleteFunc(slices.Clone(issues), func(is planner.Issue) bool {
		return is.Guest == (model.GuestRef{}) || is.Msg == IssueWaitingApproval
	})
}

// AnnotationView is the route text of the Notes of a guest, and the issues
// the last cycle found in them. The issues give their line in the Notes, of
// which the block begins at StartLine; a guest without route text has an
// empty block at line 0.
type AnnotationView struct {
	Ref       string          `json:"ref"`
	Block     string          `json:"block"`
	StartLine int             `json:"startLine"`
	Issues    []planner.Issue `json:"issues"`
}

// Annotation returns the route block of the Notes of a guest of the last
// listing, at most maxAnnotation characters of it, and never the rest of the
// Notes.
func (e *Engine) Annotation(ref model.GuestRef) (AnnotationView, error) {
	l := e.lastListing()
	g, ok := l.guests[ref]
	switch {
	case !l.complete:
		return AnnotationView{}, fmt.Errorf("%w: the last cycle did not list every guest, so the Notes of %s are not known", ErrNotFound, ref)
	case !ok:
		return AnnotationView{}, fmt.Errorf("%w: %s is not in the last listing of Proxmox", ErrNotFound, ref)
	}
	block, line := annotation.Block(g.notes)
	if r := []rune(block); len(r) > maxAnnotation {
		block = string(r[:maxAnnotation])
	}
	issues := slices.DeleteFunc(guestIssues(e.State().Issues), func(is planner.Issue) bool { return is.Guest != ref })
	return AnnotationView{Ref: ref.String(), Block: block, StartLine: line, Issues: nonNil(issues)}, nil
}
