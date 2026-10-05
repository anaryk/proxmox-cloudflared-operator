// Package present holds the words that pco derives from the state of the
// daemon, for every client of the API: the command line uses them, and the
// web interface gets the ones that are lookups as generated tables and checks
// its own versions of the others against testdata/words.json.
package present

import (
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// RouteStateOrder is the order routes are counted in; a state that is not
// listed follows, by name.
var RouteStateOrder = []planner.RouteState{
	planner.StateActive,
	planner.StateUnreachable,
	planner.StateWithdrawn,
	planner.StateConflict,
	planner.StateNoZone,
	planner.StateHeld,
	planner.StateRejected,
	engine.RouteFrozen,
}

// ModeText names the mode. Before the first cycle the state says "observe",
// but only because that is its zero.
func ModeText(st engine.State) string {
	switch {
	case st.At.IsZero():
		return "unknown"
	case st.Mode == engine.ModeObserve:
		return "observe-only"
	}
	return st.Mode
}

// InventoryText says whether the inventory is complete. What is wrong with it
// is in the problems.
func InventoryText(st engine.State) string {
	switch {
	case st.At.IsZero():
		return "unknown"
	case st.Complete:
		return "complete"
	}
	return "incomplete"
}

// EgressText says what the egress filter does, as the daemon last found it.
func EgressText(v engine.EgressView) string {
	switch v.State {
	case "":
		return "not checked yet"
	case engine.EgressOn:
		return "on"
	case engine.EgressOff:
		return "off: the connectors are not confined"
	case engine.EgressNotLoaded:
		return "not loaded: the connectors are not confined"
	case engine.EgressChanged:
		return "not the one pco loads: the connectors may not be confined"
	}
	return v.State
}

// WriterText says what the last cycle found of the writer; a dash when it
// found nothing.
func WriterText(verdict string) string {
	switch verdict {
	case engine.VerdictOK:
		return "ok"
	case engine.VerdictStale:
		return "stale (a newer generation of this install is writing)"
	case engine.VerdictForeign:
		return "foreign (another installation is writing)"
	case engine.VerdictUnknown:
		return "unknown (leader.json could not be used)"
	case "":
		return "-"
	}
	return verdict
}

// VerifiedText says whether the configuration of a tunnel is the one the plan
// wants.
func VerifiedText(t engine.TunnelView) string {
	switch {
	case t.Unchecked:
		return "unchecked"
	case t.Held != "":
		return "held"
	case t.Unknown:
		return "unknown"
	case t.Verified:
		return "yes"
	}
	return "no"
}

// ConnectorText says how a connector fares, as connector.Status.Text does, and
// why one that runs is not ready when its journal said: that Cloudflare
// refuses its token, or that its metrics port is held. The command line shows
// those two through the problems the engine writes.
func ConnectorText(s connector.Status) string {
	text := s.Text()
	if s.TokenRefused {
		text += ", Cloudflare refuses its token"
	}
	if s.MetricsPortHeld {
		text += ", its metrics port is held by another process"
	}
	return text
}

// CredentialState is "usable" for a credential whose last check passed,
// "problem" for one that failed it, and "unknown" for one that was never
// checked or whose check got no answer.
func CredentialState(v engine.CredentialView) string {
	switch {
	case !v.Checked, v.Report.Unanswered():
		return "unknown"
	case v.Report.Usable:
		return "usable"
	}
	return "problem"
}

// IdentityNow says whether a guest still has the identity it was approved in.
func IdentityNow(v engine.ApprovalView) string {
	switch {
	case v.Matches:
		return "the same"
	case v.Current == "":
		return "not in the last listing"
	}
	return "changed to " + v.Current + ": approve it again to publish it"
}

// RouteNote is the reason a route is in its state, or else its first warning.
func RouteNote(r engine.RouteView) string {
	if r.Reason != "" {
		return r.Reason
	}
	if len(r.Warnings) > 0 {
		return r.Warnings[0]
	}
	return ""
}

// NextStep is the one thing to do next, when there is an obvious one. Nothing
// is said while the daemon waits for a setup, as the problems say what to do.
func NextStep(st engine.State) string {
	switch {
	case st.At.IsZero(), slices.ContainsFunc(st.Problems, func(p string) bool { return strings.Contains(p, "pco setup") }):
		return ""
	case len(st.Credentials) == 0:
		return "Add a Cloudflare token with pco credential add."
	case st.Mode == engine.ModeObserve:
		return "Run pco apply to start publishing."
	}
	return ""
}

// Unaffected returns the destructive actions of the last cycle that were not
// carried out and that a confirmation does not let through: removals in their
// grace while the guard holds nothing, holds of observe-only mode, adoptions.
func Unaffected(st engine.State) []reconcile.Action {
	offered := make(map[string]bool)
	for _, w := range st.Waiting {
		if w.Kind == engine.WaitingRemovals {
			for _, name := range w.Items {
				offered[name] = true
			}
		}
	}
	var out []reconcile.Action
	for _, act := range st.Actions {
		accepted := act.Kind == reconcile.DeleteRecord && offered[act.Target]
		if !act.Applied && act.Destructive && !accepted {
			out = append(out, act)
		}
	}
	return out
}

// BudgetWait says whether a problem line is the one that says what waits for
// Cloudflare's rate limit, the line of reconcile.Waiting, and how many changes
// wait; none when only reads do. The line is read back in full, so one that
// only ends in the same words, as a message of a guest may, is not taken for
// it.
func BudgetWait(line string) (int, bool) {
	list, ok := strings.CutSuffix(line, " waits "+reconcile.RateLimitWait)
	if !ok {
		list, ok = strings.CutSuffix(line, " wait "+reconcile.RateLimitWait)
	}
	if !ok {
		return 0, false
	}
	parts := strings.Split(list, ", ")
	if first, last, two := strings.Cut(parts[len(parts)-1], " and "); two {
		parts = append(parts[:len(parts)-1], first, last)
	}
	var w reconcile.Waiting
	if n, ok := changesOf(parts[len(parts)-1]); ok {
		w.Changes, parts = n, parts[:len(parts)-1]
	}
	for _, p := range parts {
		if !strings.HasPrefix(p, reconcile.ZoneListingRead) && !strings.HasPrefix(p, reconcile.TunnelRead) {
			return 0, false
		}
	}
	w.Reads = parts
	if w.Line() != line {
		return 0, false
	}
	return w.Changes, true
}

// changesOf reads the count of "1 change" or "23 changes". The form of the
// count is left to the line read back.
func changesOf(part string) (int, bool) {
	if part == "1 change" {
		return 1, true
	}
	count, ok := strings.CutSuffix(part, " changes")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(count)
	return n, err == nil
}
