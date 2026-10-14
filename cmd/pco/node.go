package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/present"
)

func (a *app) nodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "The nodes of the install: list, admit, drain, forget",
		Long: "The nodes that run pco, as the controller's registry has them. A node joins with pco setup\n" +
			"(a host) or a join token (an appliance agent), is admitted once it reports, serves the shared\n" +
			"tunnel while it is active, is drained before maintenance and forgotten once it is gone. Every\n" +
			"command but list asks the controller: on another node they say which node to run them on.",
		Example: "  # The nodes\n" +
			"  pco node list\n\n" +
			"  # Take a node out of the shared tunnel before its maintenance\n" +
			"  pco node drain pve2\n\n" +
			"  # Remove a node that is gone\n" +
			"  pco node forget pve3",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(a.nodeListCmd(), a.nodeAdmitCmd(), a.nodeDrainCmd(), a.nodeUndrainCmd(), a.nodeForgetCmd(), a.nodeSetCmd())
	return cmd
}

func (a *app) nodeListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the nodes and their states",
		Long: "List the nodes of the registry as the controller's last cycle found them: the state of each,\n" +
			"its role, kind and index, its agent address, when its agent last reported and whether its\n" +
			"forwarding-table watch runs. Active nodes come first, then draining, joining, unknown, down,\n" +
			"contested and removed ones. A node alone lists nothing. The exit status is 1 when a node is\n" +
			"unknown, down or contested.\n\n" +
			"With --json the nodes are printed as the daemon sent them.\n\n" + askHelp,
		Example: "  # The nodes\n" +
			"  pco node list\n\n" +
			"  # The nodes that are not active\n" +
			"  pco node list --json | jq '.[] | select(.state != \"active\") | .name'",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if a.json {
				raw, err := a.client().NodesRaw(ctx)
				if err != nil {
					return a.explain(ctx, err)
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			nodes, err := a.client().Nodes(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			a.renderNodes(s, nodes)
			if err := s.done(); err != nil {
				return err
			}
			if slices.ContainsFunc(nodes, func(n engine.NodeView) bool {
				return n.State == "unknown" || n.State == "down" || n.State == "contested"
			}) {
				return errReported
			}
			return nil
		},
	}
}

// renderNodes prints the nodes by state, in the order present lists them.
func (a *app) renderNodes(s *screen, nodes []engine.NodeView) {
	if len(nodes) == 0 {
		s.println("This node alone runs the install.")
		return
	}
	nodes = slices.Clone(nodes)
	rank := func(state string) int {
		i := slices.Index(present.NodeStateOrder, agentproto.NodeState(state))
		if i < 0 {
			return len(present.NodeStateOrder)
		}
		return i
	}
	slices.SortStableFunc(nodes, func(x, y engine.NodeView) int {
		if rx, ry := rank(x.State), rank(y.State); rx != ry {
			return rx - ry
		}
		return strings.Compare(x.Name, y.Name)
	})
	t := s.table()
	t.row("NAME", "STATE", "ROLE", "KIND", "INDEX", "ADDRESS", "AGENT")
	for _, n := range nodes {
		index := "-"
		if n.Index != nil {
			index = fmt.Sprint(*n.Index)
		}
		addr := "-"
		if n.AgentAddress.IsValid() {
			addr = n.AgentAddress.String()
		}
		t.row(n.Name, nodeStateCell(n), present.NodeRoleText(n), n.Kind, index, addr, present.NodeAgentText(n))
	}
	t.flush()
	for _, n := range nodes {
		if len(n.Contested) > 0 {
			s.println("")
			s.printf("%s is %s; its join token was used:\n", n.Name, present.NodeStateText(n.State))
			for _, u := range n.Contested {
				s.printf("  %s from %s, container %d, MACs %s\n", a.when(u.At), u.Source, u.VMID, dash(strings.Join(u.MACs, " ")))
			}
		}
	}
}

func (a *app) nodeAdmitCmd() *cobra.Command {
	var fingerprint string
	var renew bool
	cmd := &cobra.Command{
		Use:   "admit <node>",
		Short: "Admit a node that joins",
		Long: "Admit a node that joins. An appliance agent is trusted by the fingerprint of its certificate:\n" +
			"--fingerprint takes at least eight hex digits of the SHA-256 that pco agent status shows in the\n" +
			"container (pct exec <vmid> -- pco agent status), and they must be the beginning of the one\n" +
			"enrolled. A node that stays joining because more shared routes would move to pinned tunnels\n" +
			"than cluster.admissionMaxMoves allows is admitted all the same: every route it fails moves\n" +
			"first. A contested node is not admitted: pco node forget ends that, and a new token enrols it\n" +
			"again. An appliance agent renews its own certificate over the channel while it is valid;\n" +
			"--renew, for one whose certificate ended, is refused with the way to enrol it again: forget\n" +
			"the node, uninstall the container on its node, and install it with a new join token.\n\n" + askHelp,
		Example: "  # An appliance agent, by its fingerprint\n" +
			"  pco node admit pve3 --fingerprint 9f86d081884c7d65\n\n" +
			"  # A host node that would move too many routes\n" +
			"  pco node admit pve2",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			ctx := cmd.Context()
			v, err := a.client().AdmitNode(ctx, args[0], fingerprint, renew)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			s.printf("Node %s is admitted; it becomes active once every route it fails moved. Follow it with pco node list.\n", v.Name)
			return s.done()
		},
	}
	cmd.Flags().StringVar(&fingerprint, "fingerprint", "", "at least eight hex digits of the SHA-256 of the agent's certificate, as pco agent status shows it")
	cmd.Flags().BoolVar(&renew, "renew", false, "for an appliance agent whose certificate ended: refused, with the way to enrol it again")
	return cmd
}

func (a *app) nodeDrainCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "drain <node>",
		Short: "Take a node out of the shared tunnel before its maintenance",
		Long: "Take a node out of the shared tunnel's replicas: its connector of the shared tunnel stops, and\n" +
			"the routes of its own guests keep being served by its pinned tunnel until they migrate. The\n" +
			"last active replica is not drained without --force, which stops the tunnel's only connector.\n" +
			"A node that is not active yet keeps the intent and is drained as it is admitted. Proxmox's\n" +
			"maintenance mode drains a node by itself while cluster.followMaintenance is on. pco node\n" +
			"undrain reverses it.\n\n" + askHelp,
		Example: "  # Before maintenance of pve2\n" +
			"  pco node drain pve2\n\n" +
			"  # The last replica, although nothing serves the shared tunnel then\n" +
			"  pco node drain pve1 --force",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			ctx := cmd.Context()
			v, err := a.client().DrainNode(ctx, args[0], force)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			if v.State == "draining" {
				s.printf("Node %s is draining: it leaves the shared tunnel's replicas; its pinned tunnel keeps the routes of its own guests.\n", v.Name)
			} else {
				s.printf("Node %s is %s: it is drained as it is admitted.\n", v.Name, v.State)
			}
			return s.done()
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "drain the last active replica too")
	return cmd
}

func (a *app) nodeUndrainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "undrain <node>",
		Short: "Put a drained node back among the replicas",
		Long: "End the drain of a node: it is active again and runs the shared tunnel. A drain that Proxmox's\n" +
			"maintenance mode made ends with the mode, not here.\n\n" + askHelp,
		Example: "  # After maintenance of pve2\n" +
			"  pco node undrain pve2\n\n" +
			"  # Where the drain came from\n" +
			"  pco node list",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			ctx := cmd.Context()
			v, err := a.client().UndrainNode(ctx, args[0])
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			s.printf("Node %s is no longer drained; it is %s.\n", v.Name, v.State)
			return s.done()
		},
	}
}

func (a *app) nodeForgetCmd() *cobra.Command {
	var compromised, yes bool
	cmd := &cobra.Command{
		Use:   "forget <node>",
		Short: "Remove a node that is gone, and the secrets it held",
		Long: "Remove a node that is gone from the install: its entry becomes removed, so that its agent is\n" +
			"refused from its next request, its connections are closed, its pinned tunnels are deleted and\n" +
			"the shared tunnel's secret is rotated (cluster.rotateOnForget), so that the run token the node\n" +
			"keeps stops working, and its connector is dropped at Cloudflare; the remaining replicas keep\n" +
			"their connections until each restarts with the new token. What cannot be done at once, the\n" +
			"next cycles finish; in observe-only mode they wait for pco apply. Replacing a node with\n" +
			"the same name: forget it, pvecm delnode and pvecm add for the new machine, then pco setup on\n" +
			"it, or a new join token.\n\n" +
			"--compromised is for a node whose disk is out of your hands: it held every credential of the\n" +
			"install. It is refused while the node is still in the Proxmox cluster, which would hand it\n" +
			"every new secret: pvecm delnode first. Then every secret is rotated, step by step, in the\n" +
			"order that keeps each new one from the node: you replace the Cloudflare API tokens (pco\n" +
			"credential add, then remove the old ones and revoke them at Cloudflare) and give the Proxmox\n" +
			"token a new secret (pco setup --repair); the daemon then rotates every tunnel's secret, waits\n" +
			"for the connectors, rotates the CA of the install and, ten minutes later, stops taking the\n" +
			"old one. pco cluster status follows it; a controller that takes over in the middle finishes\n" +
			"it. What the disk holds that pco cannot rotate is listed. With --compromised it runs as root\n" +
			"on the controller. The question needs a terminal; a script passes --yes.\n\n" +
			"An appliance agent's container holds less: the run tokens of its tunnels and its own Proxmox\n" +
			"token, which holds the reading role PCO alone. --compromised for one is refused while that\n" +
			"token is there: pco appliance uninstall --vmid <vmid> on its node removes the container and the\n" +
			"token first. A token removed by hand while the container runs is taken as gone: the removed\n" +
			"entry makes the agent endpoint refuse the container's certificate, so it gets no new secret.\n" +
			"Then the secrets of the tunnels are rotated, whatever cluster.rotateOnForget says; the\n" +
			"credentials, the controller's Proxmox secret and the CA stay, as the container never held them.\n\n" + askHelp,
		Example: "  # A node that was destroyed\n" +
			"  pco node forget pve3\n\n" +
			"  # A node whose disk was stolen, once pvecm delnode ran\n" +
			"  pco node forget pve3 --compromised",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			question := fmt.Sprintf("Forget node %s? Its agent is refused and the shared tunnel's secret is rotated. [y/N]", args[0])
			if compromised {
				question = fmt.Sprintf("Forget node %s as compromised? Every secret of the install is rotated, and you replace the API tokens and the Proxmox secret. [y/N]", args[0])
			}
			ok, err := a.confirm(cmd, yes, question)
			if err != nil {
				return err
			}
			if !ok {
				return errAborted
			}
			ctx := cmd.Context()
			res, err := a.client().ForgetNode(ctx, args[0], compromised)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			describeForget(s, res)
			return s.done()
		},
	}
	cmd.Flags().BoolVar(&compromised, "compromised", false, "the node's disk is out of your hands: rotate every secret of the install")
	addYesFlag(cmd, &yes)
	return cmd
}

// describeForget prints what forgetting a node did and what is left.
func describeForget(s *screen, res engine.ForgetResult) {
	s.printf("Node %s is forgotten.\n", res.Node)
	for _, line := range res.Done {
		s.printf("  - %s\n", line)
	}
	if len(res.Pending) > 0 {
		s.println("")
		if res.Compromised {
			s.println("The rotation of every secret runs step by step; pco cluster status follows it:")
		} else {
			s.println("Left to do:")
		}
		for _, line := range res.Pending {
			s.printf("  - %s\n", line)
		}
	}
	if len(res.NotRotated) > 0 {
		s.println("")
		s.println("What the node's disk holds that pco cannot rotate; change these by hand:")
		for _, line := range res.NotRotated {
			s.printf("  - %s\n", line)
		}
	}
}

func (a *app) nodeSetCmd() *cobra.Command {
	var eligible string
	cmd := &cobra.Command{
		Use:   "set <node>",
		Short: "Change what the admin may of a node's entry",
		Long: "Change a node's entry in the registry: --controller-eligible says whether the node may hold\n" +
			"the lease and be the controller. A node that may not keeps serving its guests and the shared\n" +
			"tunnel; it only never takes over.\n\n" + askHelp,
		Example: "  # Keep the lease off a small node\n" +
			"  pco node set pve3 --controller-eligible=false\n\n" +
			"  # Let it hold the lease again\n" +
			"  pco node set pve3 --controller-eligible=true",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			var p engine.NodePatch
			switch eligible {
			case "":
				return fmt.Errorf("nothing to set: give --controller-eligible=true or false")
			case "true", "false":
				v := eligible == "true"
				p.ControllerEligible = &v
			default:
				return fmt.Errorf("--controller-eligible takes true or false, not %q", eligible)
			}
			ctx := cmd.Context()
			v, err := a.client().SetNode(ctx, args[0], p)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			word := "may"
			if !*p.ControllerEligible {
				word = "may not"
			}
			s.printf("Node %s %s hold the lease from now on.\n", v.Name, word)
			return s.done()
		},
	}
	cmd.Flags().StringVar(&eligible, "controller-eligible", "", "whether the node may hold the lease: true or false")
	return cmd
}
