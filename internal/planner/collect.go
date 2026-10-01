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

// Issue is a problem with a guest's annotation. It does not stop collection;
// the affected route is left out and the issue is reported.
type Issue struct {
	Guest model.GuestRef
	Line  int // 0 when the issue is not tied to a position in the Notes
	Col   int
	Msg   string
}

// Collected is the candidate routes found in one pass. Several routes may
// claim the same hostname; ResolveClaims settles that.
type Collected struct {
	Routes []model.Route // sorted by hostname, then owner
	Issues []Issue       // sorted by guest, then position
}

// Collect turns guest annotations and manual routes into candidate routes.
// Templates and guests without the gate tag are ignored. The allow and deny
// lists apply to annotations only: manual routes are the admin's own.
func Collect(guests []model.Guest, manual []model.Route, s Settings) Collected {
	gate := cmp.Or(s.GateTag, defaultGateTag)
	pol := newPolicy(s.AllowHosts, s.DenyHosts)

	var out Collected
	for _, g := range guests {
		if g.Template || !g.HasTag(gate) {
			continue
		}
		out.addGuest(g, pol)
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

func (c *Collected) addGuest(g model.Guest, pol policy) {
	res := annotation.Parse(g.Description)
	if !res.Found {
		c.addIssue(g.Ref, 0, 0, "tagged cf-tunnel but no routes found in Notes")
		return
	}
	for _, e := range res.Entries {
		for _, host := range e.Hosts {
			if !pol.allows(host) {
				c.addIssue(g.Ref, e.Line, e.Col, fmt.Sprintf("hostname %q is not allowed by policy", host))
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

// policy is the allow and deny lists with their patterns in the form
// hostname.MatchPattern expects.
type policy struct {
	allow, deny []string
}

func newPolicy(allow, deny []string) policy {
	return policy{allow: normalizePatterns(allow), deny: normalizePatterns(deny)}
}

func normalizePatterns(patterns []string) []string {
	out := make([]string, len(patterns))
	for i, p := range patterns {
		out[i] = strings.ToLower(strings.TrimSuffix(p, "."))
	}
	return out
}

func (p policy) allows(host string) bool {
	matches := func(pattern string) bool { return hostname.MatchPattern(pattern, host) }
	if slices.ContainsFunc(p.deny, matches) {
		return false
	}
	return len(p.allow) == 0 || slices.ContainsFunc(p.allow, matches)
}
