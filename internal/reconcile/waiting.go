package reconcile

import (
	"fmt"
	"slices"
	"strings"
)

// The words of the line of Waiting. present.BudgetWait reads the line back by
// them, so that the two say the same.
const (
	// RateLimitWait ends the line, after its verb.
	RateLimitWait = "for Cloudflare's rate limit"
	// ZoneListingRead and TunnelRead begin what a run could not read, before
	// the name of the zone or the id of the account.
	ZoneListingRead = "the listing of zone "
	TunnelRead      = "the tunnel of account "
)

// Waiting is what a run left for Cloudflare's rate limit, or the budget of a
// credential, to a later cycle: the changes it held, and what it could not
// read, each as a phrase such as "the listing of zone example.com".
type Waiting struct {
	Changes int
	Reads   []string
}

// Add adds what another run left.
func (w *Waiting) Add(o Waiting) {
	w.Changes += o.Changes
	w.Reads = append(w.Reads, o.Reads...)
}

// Line says in one line what waits, or nothing when nothing does.
func (w Waiting) Line() string {
	parts := slices.Clone(w.Reads)
	switch {
	case w.Changes == 1:
		parts = append(parts, "1 change")
	case w.Changes > 1:
		parts = append(parts, fmt.Sprintf("%d changes", w.Changes))
	}
	verb := "wait"
	if len(parts) == 1 && w.Changes <= 1 {
		verb = "waits"
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0] + " " + verb + " " + RateLimitWait
	}
	last := len(parts) - 1
	return strings.Join(parts[:last], ", ") + " and " + parts[last] + " " + verb + " " + RateLimitWait
}
