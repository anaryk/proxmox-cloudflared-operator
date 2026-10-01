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

// Collected is the candidate routes found in one pass. Several routes may
// claim the same hostname; ResolveClaims settles that.
type Collected struct {
	Routes []model.Route // sorted by hostname, then owner
	Issues []Issue       // sorted by guest, then position; settings issues last
}

// Collect turns guest annotations and manual routes into candidate routes.
// Templates and guests without the gate tag are ignored. The allow and deny
// lists apply to annotations only: manual routes are the admin's own.
//
// A deny pattern that does not normalise denies every hostname and an allow
// pattern that does not normalise matches none; each is reported once.
func Collect(guests []model.Guest, manual []model.Route, s Settings) Collected {
	gate := cmp.Or(s.GateTag, defaultGateTag)
	pol, issues := newPolicy(s.AllowHosts, s.DenyHosts)

	out := Collected{Issues: issues}
	for _, g := range guests {
		if g.Template || !g.HasTag(gate) {
			continue
		}
		out.addGuest(g, gate, pol)
	}
	out.Routes = append(out.Routes, manual...)

	slices.SortStableFunc(out.Routes, func(a, b model.Route) int {
		return cmp.Or(
			strings.Compare(a.Hostname, b.Hostname),
			model.CompareOwners(a.Owner(), b.Owner()),
		)
	})
	slices.SortStableFunc(out.Issues, func(a, b Issue) int {
		return cmp.Or(
			model.CompareOwners(a.Guest.String(), b.Guest.String()),
			cmp.Compare(a.Line, b.Line),
			cmp.Compare(a.Col, b.Col),
		)
	})
	return out
}

func (c *Collected) addGuest(g model.Guest, gate string, pol policy) {
	res := annotation.Parse(g.Description)
	if len(res.Entries) == 0 && len(res.Errors) == 0 {
		c.addIssue(g.Ref, 0, 0, fmt.Sprintf("tagged %s but no routes found in Notes", gate))
		return
	}
	for _, e := range res.Entries {
		for i, host := range e.Hosts {
			if !pol.allows(host) {
				at := e.Positions[i]
				c.addIssue(g.Ref, at.Line, at.Col, fmt.Sprintf("hostname %q is not allowed by policy", host))
				continue
			}
			guest := g.Ref
			c.Routes = append(c.Routes, model.Route{
				Hostname: host,
				Target:   e.Target,
				Options:  e.Options,
				Source:   model.SourceAnnotation,
				Guest:    &guest,
			})
		}
	}
	for _, e := range res.Errors {
		c.addIssue(g.Ref, e.Line, e.Col, e.Msg)
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
			p.denyAll = true
			issues = append(issues, Issue{Msg: fmt.Sprintf(
				"invalid deny pattern %q: %v; all hostnames are denied until it is fixed", s, err)})
			continue
		}
		p.deny = append(p.deny, pattern)
	}
	for _, s := range allow {
		pattern, err := hostname.NormalizePattern(s)
		if err != nil {
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
