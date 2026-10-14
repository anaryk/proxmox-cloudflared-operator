package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

func (a *app) agentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "The agent of this node",
		Long: "The agent that runs on this node: it follows the controller where the lease is, applies the\n" +
			"desired state it is given, runs the connectors and the egress filter, and reports what it\n" +
			"sees. Both commands are answered by this node, whatever its role.",
		Example: "  # What the agent of this node follows and runs\n" +
			"  pco agent status\n\n" +
			"  # Forget the epoch it remembers, after the store was restored on purpose\n" +
			"  pco agent reset-epoch",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(a.agentStatusCmd(), a.agentResetEpochCmd())
	return cmd
}

func (a *app) agentStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show what the agent of this node follows and runs",
		Long: "Show the agent of this node as it sees itself: the controller it follows and since when none\n" +
			"answered, the epoch and the seq of the desired state it applied and how old that is, its\n" +
			"connectors, the size of its egress set, whether it stood down and why, its certificate's\n" +
			"fingerprint and end, the ready replicas of each tunnel by the controller's last heartbeat, and\n" +
			"its problems. The fingerprint is what pco node admit --fingerprint takes for an appliance\n" +
			"agent. The exit status is 1 when it stood down or has a problem.\n\n" +
			"With --json the answer of the daemon is printed as it was sent.\n\n" + askHelp,
		Example: "  # The agent of this node\n" +
			"  pco agent status\n\n" +
			"  # In an appliance agent container, from the node\n" +
			"  pct exec 9003 -- pco agent status",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if a.json {
				raw, err := a.client().AgentStatusRaw(ctx)
				if err != nil {
					return a.explain(ctx, err)
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			v, err := a.client().AgentStatus(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			a.renderAgent(s, v)
			if err := s.done(); err != nil {
				return err
			}
			if v.StoodDown != "" || len(v.Problems) > 0 {
				return errReported
			}
			return nil
		},
	}
}

// renderAgent prints the agent's view.
func (a *app) renderAgent(s *screen, v agentproto.AgentView) {
	statusLine(s, "Node", v.Node)
	statusLine(s, "Kind", string(v.Kind))
	controller := dash(v.Controller)
	if v.SilentFor > 0 {
		controller += fmt.Sprintf(" (silent for %s)", v.SilentFor.Round(time.Second))
	}
	statusLine(s, "Controller", controller)
	if v.Epoch.Generation > 0 {
		statusLine(s, "Epoch", v.Epoch.String())
	} else {
		statusLine(s, "Epoch", "none yet")
	}
	statusLine(s, "Desired", fmt.Sprintf("seq %d, %s old", v.Seq, v.SeqAge.Round(time.Second)))
	statusLine(s, "Connectors", fmt.Sprint(v.Connectors))
	statusLine(s, "Egress set", fmt.Sprintf("%d targets", v.EgressTargets))
	if v.StoodDown != "" {
		statusLine(s, "Stood down", v.StoodDown)
	}
	if v.CertFingerprint != "" {
		statusLine(s, "Certificate", fmt.Sprintf("%s, ends %s", v.CertFingerprint, a.when(v.CertExpires)))
	}
	if len(v.ReadyReplicas) > 0 {
		var parts []string
		for _, id := range slices.Sorted(maps.Keys(v.ReadyReplicas)) {
			parts = append(parts, fmt.Sprintf("%s: %d", shortID(id), v.ReadyReplicas[id]))
		}
		statusLine(s, "Replicas", fmt.Sprintf("ready %s (heartbeat %s old)", strings.Join(parts, ", "), v.HeartbeatAge.Round(time.Second)))
	}
	if len(v.Problems) > 0 {
		s.println("")
		s.println("Problems:")
		for _, p := range v.Problems {
			s.printf("  - %s\n", p)
		}
	}
}

func (a *app) agentResetEpochCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "reset-epoch",
		Short: "Forget the epoch the agent of this node remembers",
		Long: "Have the agent of this node forget the epoch it remembers, with the desired state kept with\n" +
			"it. The agent refuses a controller of an epoch below the one it remembers, which keeps a\n" +
			"restored store from writing until pco promote draws above it; when the store was restored on\n" +
			"purpose and this node's memory is what stands in the way, this forgets it instead. It takes\n" +
			"effect at the next start of pco: systemctl restart pco. Only root may, and the question needs\n" +
			"a terminal; a script passes --yes.\n\n" +
			"It asks the daemon through its socket, which answers this request for root alone; the exit\n" +
			"status is 2 when the daemon could not be asked.",
		Example: "  # Forget the epoch, then restart pco\n" +
			"  pco agent reset-epoch && systemctl restart pco\n\n" +
			"  # From a script\n" +
			"  pco agent reset-epoch --yes",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			ok, err := a.confirm(cmd, yes, "Forget the epoch the agent of this node remembers? [y/N]")
			if err != nil {
				return err
			}
			if !ok {
				return errAborted
			}
			ctx := cmd.Context()
			if err := a.client().ResetAgentEpoch(ctx); err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			s.println("The epoch the agent remembers is forgotten; it follows the store's at the next start of pco (systemctl restart pco).")
			return s.done()
		},
	}
	addYesFlag(cmd, &yes)
	return cmd
}
