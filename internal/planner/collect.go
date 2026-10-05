// Package planner decides what the tunnel should serve: which routes exist
// and which owner gets each public hostname.
//
// Everything here is pure. Time comes in through the input and nothing is
// read from or written to the outside world.
package planner

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/annotation"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

const defaultGateTag = "cf-tunnel"

// Settings is the part of the configuration that decides which guests and
// hostnames are considered.
type Settings struct {
	GateTag    string   // default "cf-tunnel"
	AllowHosts []string // empty: everything allowed
	DenyHosts  []string
	// MaxHostnamesPerGuest is how many hostnames the Notes of one guest may
	// name; model.DefaultMaxHostnamesPerGuest when zero.
	MaxHostnamesPerGuest int
}

// Issue is a problem with a guest's annotation or with the settings. It does
// not stop collection; the affected route is left out and the issue is
// reported.
type Issue struct {
	Guest model.GuestRef `json:"guest,omitzero"` // zero for a problem with the settings
	Line  int            `json:"line,omitempty"` // 0 when the issue is not tied to a position in the Notes
	Col   int            `json:"col,omitempty"`
	Msg   string         `json:"msg"`
}

// HeldName is a hostname that a guest's Notes still name, anywhere, but that
// did not become a route of that guest, typically because its entry is broken.
// It keeps an existing claim of that guest from being released.
type HeldName struct {
	Hostname string `json:"hostname"`
	Owner    string `json:"owner"`
}

// Collected is the candidate routes found in one pass. Several routes may
// claim the same hostname; ResolveClaims settles that.
type Collected struct {
	Routes        []model.Route `json:"routes"`        // sorted by hostname, then owner
	Issues        []Issue       `json:"issues"`        // sorted by guest, then position; settings issues last
	Held          []HeldName    `json:"held"`          // sorted by hostname, then owner
	PolicyInvalid bool          `json:"policyInvalid"` // an allow or deny pattern does not normalise
}

// Collect turns guest annotations and manual routes into candidate routes.
// Templates and guests without the gate tag are ignored. The allow and deny
// lists apply to annotations only: manual routes are the admin's own. A
// manual route without an id is left out, since it would have no owner of its
// own.
//
// A deny pattern that does not normalise denies every hostname and an allow
// pattern that does not normalise matches none; each is reported once.
//
// Every hostname a guest's Notes name anywhere, in route text or not, that did
// not become one of its routes is held for that guest, unless a policy that
// could be read in full ruled it out. A broken entry, a stray fence or a typo
// in the settings therefore never makes a guest lose a hostname; only removing
// it from the Notes altogether, or a policy that says so, does.
func Collect(guests []model.Guest, manual []model.Route, s Settings) Collected {
	gate := cmp.Or(s.GateTag, defaultGateTag)
	pol, issues := newPolicy(s.AllowHosts, s.DenyHosts)

	out := Collected{
		Routes:        []model.Route{},
		Issues:        append([]Issue{}, issues...),
		Held:          []HeldName{},
		PolicyInvalid: pol.invalid,
	}
	limit := s.MaxHostnamesPerGuest
	if limit <= 0 {
		limit = model.DefaultMaxHostnamesPerGuest
	}
	for _, g := range guests {
		if g.Template || !g.HasTag(gate) {
			continue
		}
		out.addGuest(g, gate, pol, limit)
	}
	out.addManual(manual)

	slices.SortStableFunc(out.Routes, func(a, b model.Route) int {
		return cmp.Or(
			strings.Compare(a.Hostname, b.Hostname),
			model.CompareOwners(a.Owner(), b.Owner()),
		)
	})
	slices.SortStableFunc(out.Issues, CompareIssues)
	slices.SortFunc(out.Held, func(a, b HeldName) int {
		return cmp.Or(
			strings.Compare(a.Hostname, b.Hostname),
			model.CompareOwners(a.Owner, b.Owner),
		)
	})
	return out
}

// CompareIssues is the order of Collected.Issues: by guest, then position in
// the Notes; the issues of the settings, which name no guest, come last.
func CompareIssues(a, b Issue) int {
	return cmp.Or(
		model.CompareOwners(a.Guest.String(), b.Guest.String()),
		cmp.Compare(a.Line, b.Line),
		cmp.Compare(a.Col, b.Col),
	)
}

// addGuest adds the routes of a guest. A guest whose Notes name more than
// limit hostnames the policy lets through publishes none of them, and holds
// them all: a Notes field is no place for hundreds of names.
func (c *Collected) addGuest(g model.Guest, gate string, pol policy, limit int) {
	res := annotation.Parse(g.Description)
	if len(res.Entries) == 0 && len(res.Errors) == 0 {
		c.addIssue(g.Ref, 0, 0, fmt.Sprintf("tagged %s but no routes found in Notes", gate))
	}
	var routes []model.Route
	named := make(map[string]bool)
	for _, e := range res.Entries {
		for i, host := range e.Hosts {
			if !pol.allows(host) {
				at := e.Positions[i]
				c.addIssue(g.Ref, at.Line, at.Col, fmt.Sprintf("hostname %q is not allowed by policy", host))
				continue
			}
			guest := g.Ref
			routes = append(routes, model.Route{
				Hostname: host,
				Target:   e.Target,
				Options:  e.Options,
				Source:   model.SourceAnnotation,
				Guest:    &guest,
			})
			named[host] = true
		}
	}
	if len(named) > limit {
		c.addIssue(g.Ref, 0, 0, fmt.Sprintf("the Notes name %d hostnames, more than maxHostnamesPerGuest allows (%d); "+
			"none of them is published until they name at most %d", len(named), limit, limit))
		routes = nil
	}
	routed := make(map[string]bool)
	for _, rt := range routes {
		c.Routes = append(c.Routes, rt)
		routed[rt.Hostname] = true
	}
	// Hostnames covers res.Mentioned: every hostname of the route text is a
	// word of the whole description.
	for _, host := range annotation.Hostnames(g.Description) {
		// Only a policy that could be read in full may take a hostname away.
		if routed[host] || !pol.invalid && !pol.allows(host) {
			continue
		}
		c.Held = append(c.Held, HeldName{Hostname: host, Owner: g.Ref.String()})
	}
	for _, e := range res.Errors {
		c.addIssue(g.Ref, e.Line, e.Col, e.Msg)
	}
}

// addManual adds the manual routes that have an id and reports the others.
func (c *Collected) addManual(manual []model.Route) {
	var unnamed []string
	for _, rt := range manual {
		if rt.ManualID == "" {
			unnamed = append(unnamed, rt.Hostname)
			continue
		}
		c.Routes = append(c.Routes, rt)
	}
	// Sorted, so that the issues do not depend on the order of the input.
	slices.Sort(unnamed)
	for _, host := range unnamed {
		c.addIssue(model.GuestRef{}, 0, 0, fmt.Sprintf("manual route for %q has no id", host))
	}
}

func (c *Collected) addIssue(guest model.GuestRef, line, col int, msg string) {
	c.Issues = append(c.Issues, Issue{Guest: guest, Line: line, Col: col, Msg: msg})
}

// policy is the allow and deny lists with their patterns normalised.
type policy struct {
	allow, deny []string
	restricted  bool // an allow list was given, even if none of it is valid
	denyAll     bool // a deny pattern is invalid
	invalid     bool // a deny or allow pattern is invalid
}

// newPolicy normalises the patterns and returns an issue for each one that
// is invalid. Doubt fails closed: an invalid deny pattern denies everything,
// and an allow list keeps restricting even when every pattern in it is
// invalid.
func newPolicy(allow, deny []string) (policy, []Issue) {
	p := policy{restricted: len(allow) > 0}
	var issues []Issue
	for _, s := range deny {
		pattern, err := hostname.NormalizePattern(s)
		if err != nil {
			p.denyAll, p.invalid = true, true
			issues = append(issues, Issue{Msg: fmt.Sprintf(
				"invalid deny pattern %q: %v; all hostnames are denied until it is fixed", s, err)})
			continue
		}
		p.deny = append(p.deny, pattern)
	}
	for _, s := range allow {
		pattern, err := hostname.NormalizePattern(s)
		if err != nil {
			p.invalid = true
			issues = append(issues, Issue{Msg: fmt.Sprintf("invalid allow pattern %q: %v", s, err)})
			continue
		}
		p.allow = append(p.allow, pattern)
	}
	return p, issues
}

func (p policy) allows(host string) bool {
	denies := func(pattern string) bool { return hostname.Denies(pattern, host) }
	if p.denyAll || slices.ContainsFunc(p.deny, denies) {
		return false
	}
	matches := func(pattern string) bool { return hostname.MatchPattern(pattern, host) }
	return !p.restricted || slices.ContainsFunc(p.allow, matches)
}
