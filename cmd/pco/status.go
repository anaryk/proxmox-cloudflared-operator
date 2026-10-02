package main

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
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
}

// tunnelIDWidth is how much of the id of a tunnel is shown.
const tunnelIDWidth = 8

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show what the daemon found and did",
		Long: "Show the mode of the daemon, whether the inventory is complete, the routes by state,\n" +
			"the tunnels with their connectors, the credentials and the problems.\n" +
			"The exit status is 1 when there are problems.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, raw, err := a.state(cmd.Context())
			if err != nil {
				return err
			}
			if a.json {
				if err := printJSON(cmd.OutOrStdout(), raw); err != nil {
					return err
				}
			} else if err := a.renderStatus(cmd.OutOrStdout(), st); err != nil {
				return err
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
func statusLine(w io.Writer, key, value string) {
	if value == "" {
		_, _ = fmt.Fprintf(w, "%s:\n", key)
		return
	}
	_, _ = fmt.Fprintf(w, "%-*s%s\n", statusKeyWidth, key+":", value)
}

func (a *app) renderStatus(w io.Writer, st engine.State) error {
	statusLine(w, "Mode", modeText(st.Mode))
	if st.Profile != "" {
		statusLine(w, "Profile", st.Profile)
	}
	statusLine(w, "Inventory", inventoryText(st))
	statusLine(w, "Writer", writerText(st.WriterVerdict))
	statusLine(w, "Routes", routeCounts(st.Routes))
	if st.At.IsZero() {
		statusLine(w, "Last cycle", "no cycle has run yet")
	} else {
		statusLine(w, "Last cycle", a.when(st.At))
	}

	if err := a.tunnelSection(w, st); err != nil {
		return err
	}
	if err := a.credentialSection(w, st); err != nil {
		return err
	}
	problemSection(w, st.Problems)
	if next := nextStep(st); next != "" {
		_, _ = fmt.Fprintf(w, "\n%s\n", next)
	}
	return nil
}

func modeText(mode string) string {
	if mode == "observe" {
		return "observe-only"
	}
	return mode
}

func inventoryText(st engine.State) string {
	switch {
	case st.Complete:
		return "complete"
	case st.At.IsZero():
		return "not read yet"
	case len(st.Problems) > 0:
		return "incomplete: " + st.Problems[0]
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

func (a *app) tunnelSection(w io.Writer, st engine.State) error {
	if len(st.Tunnels) == 0 {
		statusLine(w, "Tunnels", "none")
		return nil
	}
	statusLine(w, "Tunnels", "")
	t := newTable(w)
	_, _ = fmt.Fprintln(t, "  NAME\tID\tVERIFIED\tCONNECTOR")
	for _, tun := range st.Tunnels {
		_, _ = fmt.Fprintf(t, "  %s\t%s\t%s\t%s\n",
			dash(tun.Name), shortID(tun.ID), verifiedText(tun), connectorText(st.Connectors, tun.ID))
	}
	return t.Flush()
}

func shortID(id string) string {
	if len(id) > tunnelIDWidth {
		id = id[:tunnelIDWidth]
	}
	return dash(id)
}

func verifiedText(t reconcile.TunnelState) string {
	switch {
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

func (a *app) credentialSection(w io.Writer, st engine.State) error {
	if len(st.Credentials) == 0 {
		statusLine(w, "Credentials", "none")
		return nil
	}
	statusLine(w, "Credentials", "")
	t := newTable(w)
	_, _ = fmt.Fprintln(t, "  LABEL\tSTATE\tNOTE")
	for _, c := range st.Credentials {
		_, _ = fmt.Fprintf(t, "  %s\t%s\t%s\n", dash(c.Label), credentialState(c), dash(a.credentialNote(c)))
	}
	return t.Flush()
}

func problemSection(w io.Writer, problems []string) {
	if len(problems) == 0 {
		statusLine(w, "Problems", "none")
		return
	}
	statusLine(w, "Problems", "")
	for _, p := range problems {
		_, _ = fmt.Fprintf(w, "  - %s\n", p)
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
