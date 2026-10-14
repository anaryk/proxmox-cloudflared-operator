package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/present"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

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
		Long: "Show the mode of the daemon, whether the inventory is complete, the egress filter, the\n" +
			"routes by state, the tunnels with their connectors, the credentials, the issues found in\n" +
			"guest notes and the problems. The exit status is 1 when there are problems or the egress\n" +
			"filter does not confine the connectors.\n\n" +
			"On a node of a cluster it also says which node is the controller and at which address, and\n" +
			"how the agent of this node fares: whether it hears the controller, the snapshot it applied\n" +
			"and how old it is, its connectors and egress targets, and its problems. On a node that is\n" +
			"not the controller the rest is what that node last found; pco cluster status on the\n" +
			"controller shows the cluster.\n\n" +
			"With --json the state of the daemon is printed as the daemon sent it, re-indented, with\n" +
			"control and bidirectional characters escaped.\n\n" + askHelp,
		Example: "  # What the daemon found and did\n" +
			"  pco status\n\n" +
			"  # The problems alone, from the state as JSON\n" +
			"  pco status --json | jq -r '.problems[]'",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := a.rawState(cmd.Context())
			if err != nil {
				if line := a.noVolumeLine(err); line != "" {
					return couldNotAsk{errors.New(line)}
				}
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
				return couldNotAsk{fmt.Errorf("decoding the state of the daemon: %w", decodeErr)}
			}
			if !a.json {
				// The agent of a node that follows another node's controller
				// says so also while no node holds the lease.
				var agent *agentAnswer
				v, err := a.client().AgentStatus(cmd.Context())
				if inCluster(st) || err == nil && v.Remote {
					agent = &agentAnswer{view: v, err: err}
				}
				if err := a.renderStatus(cmd.OutOrStdout(), st, agent); err != nil {
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

// noVolumeLine says, in an appliance whose daemon is not running, why: its
// state volume is not there, as the daemon would say. It is empty on the host,
// for any other failure to ask, and while the volume is there.
func (a *app) noVolumeLine(err error) string {
	if !apiclient.NotRunning(err) {
		return ""
	}
	if !a.onAppliance() {
		return ""
	}
	switch err := a.volumeCheck()(store.ApplianceLocal, store.VolumeMarker); {
	case err == nil:
		return ""
	case errors.Is(err, fs.ErrPermission):
		return "cannot read " + store.ApplianceLocal + ": run pco status as root"
	default:
		return appliance.VolumeLine(store.ApplianceLocal, err, a.daemon.Appliance.System.VMIDHint(store.ApplianceLocal))
	}
}

// identityText says what the last self-identification of the appliance
// found: "ok (lxc/120 on pve1)", or why not.
func identityText(id *engine.IdentityView) string {
	self := fmt.Sprintf("lxc/%d on %s", id.VMID, id.Node)
	switch {
	case id.Copy:
		return fmt.Sprintf("copy: connectors stopped (not %s: %s)", self, id.Why)
	case len(id.Exposed) > 0:
		return fmt.Sprintf("serving nothing: %s can reach into %s", strings.Join(id.Exposed, ", "), self)
	case !id.OK:
		return fmt.Sprintf("not proven (%s): %s", self, id.Why)
	}
	return "ok (" + self + ")"
}

// hasProblems says whether the state shows anything the admin has to look at.
// A cycle that has run and found the inventory incomplete or the writer not
// in order has said so in the problems too; the checks do not rely on that.
func hasProblems(st engine.State) bool {
	if len(st.Problems) > 0 || unconfined(st) {
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

// unconfined says whether the egress filter, as the daemon last found it,
// does not confine the connectors.
func unconfined(st engine.State) bool {
	switch st.Egress.State {
	case engine.EgressOff, engine.EgressNotLoaded, engine.EgressChanged:
		return true
	}
	return false
}

// agentAnswer is what the daemon said of the agent of this node.
type agentAnswer struct {
	view agentproto.AgentView
	err  error
}

// inCluster says whether the state is of a node of a cluster: the registry
// has other nodes, Proxmox lists other members, or another node holds the
// lease.
func inCluster(st engine.State) bool {
	switch {
	case len(st.Nodes) > 1:
		return true
	case st.Cluster != nil && len(st.Cluster.Members) > 1:
		return true
	}
	return st.Lease != nil && st.Lease.Holder != "" && st.Lease.Holder != st.Node
}

// controllerText says which node is the controller: the holder of the lease
// and its agent address on the host, what the agent follows in the
// appliance.
func controllerText(st engine.State, agent *agentAnswer) string {
	l := st.Lease
	switch {
	case l == nil && agent != nil && agent.err == nil && agent.view.Controller != "":
		if !agent.view.Remote {
			return "this node"
		}
		return agent.view.Controller
	case l == nil:
		return "not known"
	case l.Holder == "":
		return "none: no node holds the lease"
	}
	where := l.Holder
	if l.Holder == st.Node {
		where = "this node"
	}
	if l.Address.IsValid() {
		where += fmt.Sprintf(" (%s)", l.Address)
	}
	return fmt.Sprintf("%s, generation %d", where, l.Generation)
}

// agentText says how the agent of this node fares.
func agentText(v agentproto.AgentView) string {
	var follows string
	switch {
	case v.StoodDown != "":
		return "stood down: " + v.StoodDown
	case !v.Remote:
		follows = "in the controller's process"
	case !v.Heard:
		follows = fmt.Sprintf("has heard no controller (%s)", v.SilentFor.Round(time.Second))
	case v.SilentFor >= time.Minute:
		follows = fmt.Sprintf("the controller is silent for %s", v.SilentFor.Round(time.Second))
	default:
		follows = fmt.Sprintf("hears the controller, heartbeat %s old", v.HeartbeatAge.Round(time.Second))
	}
	return fmt.Sprintf("%s; snapshot %d of generation %d, %s old; %s, %s", follows, v.Seq, v.Epoch.Generation,
		v.SeqAge.Round(time.Second), plural(v.Connectors, "connector", "connectors"), plural(v.EgressTargets, "egress target", "egress targets"))
}

// agentSection writes the controller and the agent of this node, and the
// agent's problems.
func agentSection(s *screen, st engine.State, agent *agentAnswer) {
	statusLine(s, "Controller", controllerText(st, agent))
	if agent.err != nil {
		statusLine(s, "Agent", "did not answer: "+agent.err.Error())
		return
	}
	statusLine(s, "Agent", agentText(agent.view))
	for _, p := range agent.view.Problems {
		s.printf("  - %s\n", p)
	}
}

// renderStatus writes the status; agent is the agent of this node, nil
// where the node is alone.
func (a *app) renderStatus(w io.Writer, st engine.State, agent *agentAnswer) error {
	s := &screen{w: w}
	if st.Egress.State == engine.EgressOff {
		s.printf("Warning: the egress filter is switched off since %s: the connectors are not confined. "+
			"pco egress on switches it back on.\n\n", a.since(st.Egress.Since))
	}
	statusLine(s, "Mode", present.ModeText(st))
	if st.Profile != "" {
		statusLine(s, "Profile", st.Profile)
	}
	if st.Identity != nil {
		statusLine(s, "Identity", identityText(st.Identity))
	}
	if agent != nil {
		agentSection(s, st, agent)
	}
	statusLine(s, "Inventory", present.InventoryText(st))
	statusLine(s, "Writer", present.WriterText(st.WriterVerdict))
	statusLine(s, "Egress", present.EgressText(st.Egress))
	statusLine(s, "Routes", routeCounts(st.Routes))
	if line := managedSummary(st.Managed); line != "" {
		statusLine(s, "Managed", line)
	}
	if n := unacknowledged(st.Segments); n > 0 {
		statusLine(s, "Segments", fmt.Sprintf("%d not acknowledged (pco segment list)", n))
	}
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
	if next := present.NextStep(st); next != "" {
		s.printf("\n%s\n", next)
	}
	return s.done()
}

// unacknowledged counts the segments routes at observed were proven on that
// nobody acknowledged.
func unacknowledged(segments []engine.SegmentView) int {
	n := 0
	for _, v := range segments {
		if !v.Acknowledged {
			n++
		}
	}
	return n
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
	for _, s := range present.RouteStateOrder {
		if count[s] > 0 {
			states = append(states, s)
		}
	}
	var others []planner.RouteState
	for s := range maps.Keys(count) {
		if !slices.Contains(present.RouteStateOrder, s) {
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
		t.row("  "+dash(tun.Name), shortID(tun.ID), present.VerifiedText(tun), connectorText(st.Connectors, tun.ID))
	}
	t.flush()
}

func shortID(id string) string {
	if len(id) > tunnelIDWidth {
		id = id[:tunnelIDWidth]
	}
	return dash(id)
}

// connectorText describes the connector of a tunnel: active, ready and with
// how many connections to Cloudflare, or none.
func connectorText(conns []connector.Status, tunnelID string) string {
	i := slices.IndexFunc(conns, func(c connector.Status) bool { return c.TunnelID == tunnelID })
	if tunnelID == "" || i < 0 {
		return "none"
	}
	return conns[i].Text()
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
		t.row("  "+dash(c.Label), present.CredentialState(c), dash(a.credentialNote(c)))
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
