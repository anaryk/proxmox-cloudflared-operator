package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// routeStateOrder is the order routes are counted in; a state that is not
// listed follows, by name.
var routeStateOrder = []planner.RouteState{
	planner.StateActive,
	planner.StateUnreachable,
	planner.StateWithdrawn,
	planner.StateConflict,
	planner.StateNoZone,
	planner.StateHeld,
	engine.RouteFrozen,
}

const (
	// tunnelIDWidth is how much of the id of a tunnel is shown.
	tunnelIDWidth = 8
	// maxIssuesShown is how many issues of guest notes the status lists.
	maxIssuesShown = 10
)

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show what the daemon found and did",
		Long: "Show the mode of the daemon, whether the inventory is complete, the routes by state,\n" +
			"the tunnels with their connectors, the credentials, the issues found in guest notes\n" +
			"and the problems. The exit status is 1 when there are problems.\n\n" +
			"With --json the state of the daemon is printed as the daemon sent it, re-indented, with\n" +
			"control and bidirectional characters escaped.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := a.rawState(cmd.Context())
			if err != nil {
				return err
			}
			var st engine.State
			// The state is printed even when it cannot be read, but then the
			// exit status cannot be told.
			decodeErr := json.Unmarshal(raw, &st)
			if a.json {
				if err := printJSON(cmd.OutOrStdout(), raw); err != nil {
					return err
				}
			}
			if decodeErr != nil {
				return fmt.Errorf("decoding the state of the daemon: %w", decodeErr)
			}
			if !a.json {
				if err := a.renderStatus(cmd.OutOrStdout(), st); err != nil {
					return err
				}
			}
			if hasProblems(st) {
				return errReported
			}
			return nil
		},
	}
}

// hasProblems says whether the state shows anything the admin has to look at.
// A cycle that has run and found the inventory incomplete or the writer not
// in order has said so in the problems too; the checks do not rely on that.
func hasProblems(st engine.State) bool {
	if len(st.Problems) > 0 {
		return true
	}
	if st.At.IsZero() {
		return false
	}
	return !st.Complete || (st.WriterVerdict != "" && st.WriterVerdict != "ok")
}

// statusKeyWidth is the width of the key of a line of the status: the longest,
// "Credentials:", and a space.
const statusKeyWidth = 13

// statusLine writes a key and its value; a key without a value heads the lines
// that follow it.
func statusLine(s *screen, key, value string) {
	if value == "" {
		s.printf("%s:\n", key)
		return
	}
	s.printf("%-*s%s\n", statusKeyWidth, key+":", value)
}

func (a *app) renderStatus(w io.Writer, st engine.State) error {
	s := &screen{w: w}
	statusLine(s, "Mode", modeText(st))
	if st.Profile != "" {
		statusLine(s, "Profile", st.Profile)
	}
	statusLine(s, "Inventory", inventoryText(st))
	statusLine(s, "Writer", writerText(st.WriterVerdict))
	statusLine(s, "Routes", routeCounts(st.Routes))
	if n := len(st.Unapproved); n == 1 {
		statusLine(s, "Approval", "1 guest waits (pco guest list)")
	} else if n > 1 {
		statusLine(s, "Approval", fmt.Sprintf("%d guests wait (pco guest list)", n))
	}
	if st.At.IsZero() {
		statusLine(s, "Last cycle", "no cycle has run yet")
	} else {
		statusLine(s, "Last cycle", a.when(st.At))
	}

	a.tunnelSection(s, st)
	a.credentialSection(s, st)
	issueSection(s, st.Issues)
	problemSection(s, st.Problems)
	if next := nextStep(st); next != "" {
		s.printf("\n%s\n", next)
	}
	return s.done()
}

// modeText names the mode. Before the first cycle the state says "observe",
// but only because that is its zero.
func modeText(st engine.State) string {
	switch {
	case st.At.IsZero():
		return "unknown"
	case st.Mode == "observe":
		return "observe-only"
	}
	return st.Mode
}

// inventoryText says whether the inventory is complete. What is wrong with it
// is in the problems.
func inventoryText(st engine.State) string {
	switch {
	case st.At.IsZero():
		return "unknown"
	case st.Complete:
		return "complete"
	}
	return "incomplete"
}

func writerText(verdict string) string {
	switch verdict {
	case "ok":
		return "ok"
	case "stale":
		return "stale (a newer generation of this install is writing)"
	case "foreign":
		return "foreign (another installation is writing)"
	case "unknown":
		return "unknown (leader.json could not be used)"
	}
	return dash(verdict)
}

// routeCounts says how many routes are in each state: "active 3, held 1".
func routeCounts(routes []engine.RouteView) string {
	if len(routes) == 0 {
		return "none"
	}
	count := make(map[planner.RouteState]int)
	for _, r := range routes {
		count[r.State]++
	}
	var states []planner.RouteState
	for _, s := range routeStateOrder {
		if count[s] > 0 {
			states = append(states, s)
		}
	}
	var others []planner.RouteState
	for s := range maps.Keys(count) {
		if !slices.Contains(routeStateOrder, s) {
			others = append(others, s)
		}
	}
	slices.Sort(others)
	states = append(states, others...)

	parts := make([]string, len(states))
	for i, s := range states {
		parts[i] = fmt.Sprintf("%s %d", s, count[s])
	}
	return strings.Join(parts, ", ")
}

func (a *app) tunnelSection(s *screen, st engine.State) {
	if len(st.Tunnels) == 0 {
		statusLine(s, "Tunnels", "none")
		return
	}
	statusLine(s, "Tunnels", "")
	t := s.table()
	t.row("  NAME", "ID", "VERIFIED", "CONNECTOR")
	for _, tun := range st.Tunnels {
		t.row("  "+dash(tun.Name), shortID(tun.ID), verifiedText(tun), connectorText(st.Connectors, tun.ID))
	}
	t.flush()
}

func shortID(id string) string {
	if len(id) > tunnelIDWidth {
		id = id[:tunnelIDWidth]
	}
	return dash(id)
}

func verifiedText(t engine.TunnelView) string {
	switch {
	case t.Held != "":
		return "held"
	case t.Unknown:
		return "unknown"
	case t.Verified:
		return "yes"
	}
	return "no"
}

// connectorText describes the connector of a tunnel: active, ready and with
// how many connections to Cloudflare.
func connectorText(conns []connector.Status, tunnelID string) string {
	i := slices.IndexFunc(conns, func(c connector.Status) bool { return c.TunnelID == tunnelID })
	switch {
	case tunnelID == "" || i < 0:
		return "none"
	case !conns[i].Active:
		return "inactive"
	case !conns[i].Ready:
		return "active, not ready"
	case conns[i].Connections == 1:
		return "active, ready, 1 connection"
	}
	return fmt.Sprintf("active, ready, %d connections", conns[i].Connections)
}

func (a *app) credentialSection(s *screen, st engine.State) {
	if len(st.Credentials) == 0 {
		statusLine(s, "Credentials", "none")
		return
	}
	statusLine(s, "Credentials", "")
	t := s.table()
	t.row("  LABEL", "STATE", "NOTE")
	for _, c := range st.Credentials {
		t.row("  "+dash(c.Label), credentialState(c), dash(a.credentialNote(c)))
	}
	t.flush()
}

// issueSection lists the first issues found in guest notes, and in the
// settings, with the count of all of them.
func issueSection(s *screen, issues []planner.Issue) {
	if len(issues) == 0 {
		statusLine(s, "Issues", "none")
		return
	}
	statusLine(s, "Issues", fmt.Sprint(len(issues)))
	for _, is := range issues[:min(len(issues), maxIssuesShown)] {
		s.printf("  %s\n", issueText(is))
	}
	if more := len(issues) - maxIssuesShown; more > 0 {
		s.printf("  ... and %d more (pco status --json)\n", more)
	}
}

// issueText says where an issue is and what it is: "qemu/103 line 2, column 5:
// message", or "settings: message" for an issue that no guest has.
func issueText(is planner.Issue) string {
	where := "settings"
	if is.Guest != (model.GuestRef{}) {
		where = is.Guest.String()
		switch {
		case is.Line > 0 && is.Col > 0:
			where += fmt.Sprintf(" line %d, column %d", is.Line, is.Col)
		case is.Line > 0:
			where += fmt.Sprintf(" line %d", is.Line)
		}
	}
	return where + ": " + is.Msg
}

func problemSection(s *screen, problems []string) {
	if len(problems) == 0 {
		statusLine(s, "Problems", "none")
		return
	}
	statusLine(s, "Problems", "")
	for _, p := range problems {
		s.printf("  - %s\n", p)
	}
}

// nextStep is the one thing to do next, when there is an obvious one. Nothing
// is said while the daemon waits for a setup, as the problems say what to do.
func nextStep(st engine.State) string {
	switch {
	case st.At.IsZero(), slices.ContainsFunc(st.Problems, func(p string) bool { return strings.Contains(p, "pco setup") }):
		return ""
	case len(st.Credentials) == 0:
		return "Add a Cloudflare token with pco credential add."
	case st.Mode == "observe":
		return "Run pco apply to start publishing."
	}
	return ""
}
