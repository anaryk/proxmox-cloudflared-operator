package main

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/present"
)

func (a *app) clusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "The cluster: its status, the lease, join tokens, the upgrade",
		Long: "The cluster pco runs on: which node is the controller and holds the lease, the nodes and\n" +
			"how each fares, the quorum, the coverage of the forwarding-table watches and the rotation of\n" +
			"secrets after a node was forgotten as compromised. The commands that change something ask the\n" +
			"controller: on another node they say which node to run them on. A node alone is its own\n" +
			"cluster of one, and pco cluster status shows that.",
		Example: "  # The cluster as the controller found it\n" +
			"  pco cluster status\n\n" +
			"  # Hand the lease to another node before maintenance of this one\n" +
			"  pco cluster handoff --to pve2\n\n" +
			"  # A token for an appliance agent to join as node pve3\n" +
			"  pco cluster join-token --node pve3 --file /root/pve3.join",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(a.clusterStatusCmd(), a.clusterHandoffCmd(), a.clusterJoinTokenCmd(), a.clusterFinishUpgradeCmd(), a.clusterUnblockCmd())
	return cmd
}

func (a *app) clusterStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the controller, the lease, the quorum and every node",
		Long: "Show the cluster as the controller's last cycle found it: the controller and the epoch, who\n" +
			"holds the lease and how long ago it was refreshed, the quorum and its device or overrides,\n" +
			"whether every node's forwarding-table watch covers the cluster, an upgrade not finished, the\n" +
			"rotation of secrets after a node was forgotten as compromised, and each node with its state,\n" +
			"role, kind, build and protocol, when its agent last reported, whether its watch runs and its\n" +
			"replica of the shared tunnel is ready, how its targets fare, the routes pinned to it, when its\n" +
			"certificate ends and the skew of its clock. The exit status is 1 when a node is unknown, down\n" +
			"or contested, or a rotation waits on the admin.\n\n" +
			"With --json the answer of the daemon is printed as it was sent.\n\n" + askHelp,
		Example: "  # The cluster\n" +
			"  pco cluster status\n\n" +
			"  # The nodes alone, as JSON\n" +
			"  pco cluster status --json | jq '.nodes[] | {name, state, statusAge}'",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if a.json {
				raw, err := a.client().ClusterStatusRaw(ctx)
				if err != nil {
					return a.explain(ctx, err)
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			cs, err := a.client().ClusterStatus(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			a.renderCluster(s, cs)
			if err := s.done(); err != nil {
				return err
			}
			if clusterNeedsALook(cs) {
				return errReported
			}
			return nil
		},
	}
}

// renderCluster prints the cluster and its nodes.
func (a *app) renderCluster(s *screen, cs engine.ClusterStatus) {
	c := cs.Cluster
	if c.Controller == "" && len(cs.Nodes) == 0 {
		s.println("This node alone runs the install; there is no cluster to show.")
		return
	}
	controller := dash(c.Controller)
	if c.LeaseExpired {
		controller += ", its lease expired"
	}
	statusLine(s, "Controller", controller)
	if c.Generation > 0 {
		statusLine(s, "Generation", fmt.Sprint(c.Generation))
	}
	if !c.HeldSince.IsZero() {
		judged := "as judged at " + a.when(c.JudgedAt)
		if c.JudgedAt.IsZero() {
			judged = "never judged by this controller"
		}
		statusLine(s, "Held", fmt.Sprintf("the cycles hold since %s; coverage and blocks %s", a.when(c.HeldSince), judged))
	}
	if l := cs.Lease; l != nil {
		statusLine(s, "Lease", leaseText(l))
	} else {
		statusLine(s, "Lease", "none (a node alone, or the appliance)")
	}
	statusLine(s, "Quorum", quorumText(c))
	statusLine(s, "Coverage", coverageText(c.Coverage))
	if u := c.Upgrade; u != nil {
		text := fmt.Sprintf("not finished since %s, from %s (pco cluster finish-upgrade)", a.when(u.Since), u.From)
		if len(u.Older) > 0 {
			text += "; still on an older build: " + strings.Join(u.Older, ", ")
		}
		statusLine(s, "Upgrade", text)
	}
	if r := c.CARotation; r != nil {
		statusLine(s, "Rotation", present.RotationText(r))
	}
	if len(c.Blocks) > 0 {
		statusLine(s, "Blocks", fmt.Sprintf("%d addresses blocked on every node (pco cluster unblock)", len(c.Blocks)))
	}
	s.println("")
	if len(cs.Nodes) == 0 {
		s.println("Nodes: this node alone.")
		return
	}
	s.println("Nodes:")
	t := s.table()
	t.row("  NAME", "STATE", "ROLE", "KIND", "AGENT", "REPLICA", "TARGETS", "PINNED", "CERT ENDS")
	for _, n := range cs.Nodes {
		t.row("  "+n.Name, nodeStateCell(n), present.NodeRoleText(n), n.Kind, present.NodeAgentText(n),
			replicaText(n), fmt.Sprintf("%d ok, %d failing, %d below", n.Verified, n.Failing, n.BelowMinimum), fmt.Sprint(n.Pinned), a.when(n.CertExpires))
	}
	t.flush()
}

func leaseText(l *engine.LeaseView) string {
	if l.Holder == "" {
		return "no node holds it"
	}
	text := "held by " + l.Holder
	if l.Valid {
		text += fmt.Sprintf(", valid, refreshed %s ago", l.RefreshAge.Round(time.Second))
	} else if l.Fenced != "" {
		text += ", fenced: " + l.Fenced
	}
	if l.Behind != "" {
		text += "; behind: " + l.Behind
	}
	if l.Handoff != "" {
		text += "; handed off to " + l.Handoff
	}
	return text
}

func quorumText(c engine.ClusterView) string {
	text := "not quorate"
	if c.Quorate {
		text = "quorate"
	}
	text += fmt.Sprintf(", %d members", len(c.Members))
	if c.QuorumDevice {
		text += ", with a quorum device"
	}
	if len(c.Overrides) > 0 {
		text += ", overrides " + strings.Join(c.Overrides, ", ")
	}
	return text
}

func coverageText(c engine.CoverageView) string {
	switch {
	case c.Pinned:
		return "pinned: the appliances watch no forwarding tables, and the routes of each node's guests are served from it"
	case c.Covered && !c.PromoteAt.IsZero():
		return "every node watches; routes are shared again once it held for ten minutes"
	case c.Covered:
		return "every node watches its forwarding tables"
	case len(c.Uncovered) > 0:
		return "lost: no running watch on " + strings.Join(c.Uncovered, ", ") + "; routes of other nodes' guests are pinned"
	}
	return "lost"
}

func nodeStateCell(n engine.NodeView) string {
	if n.Drained != "" {
		return n.State + " (" + n.Drained + ")"
	}
	return dash(n.State)
}

func replicaText(n engine.NodeView) string {
	switch {
	case n.ReplicaReady:
		return "ready"
	case n.State == "active":
		return "not ready"
	}
	return "-"
}

// clusterNeedsALook says whether the status exits with 1: a node that is
// unknown, down or contested, or a rotation that waits on the admin.
func clusterNeedsALook(cs engine.ClusterStatus) bool {
	for _, n := range cs.Nodes {
		switch n.State {
		case "unknown", "down", "contested":
			return true
		}
	}
	if r := cs.Cluster.CARotation; r != nil && r.FinishedAt.IsZero() && (r.Next == "credentials" || r.Next == "proxmox") {
		return true
	}
	return cs.Cluster.Upgrade != nil && len(cs.Cluster.Upgrade.Older) > 0
}

func (a *app) clusterHandoffCmd() *cobra.Command {
	var to string
	var yes bool
	cmd := &cobra.Command{
		Use:   "handoff",
		Short: "Hand the lease to another node",
		Long: "Hand the lease off: the controller stops writing after its running cycle and releases the\n" +
			"lease, and the node named with --to alone may take it for the next 30 seconds; without --to,\n" +
			"whichever node comes first takes it, and this node holds back as long. Use it before\n" +
			"maintenance of the controller's node, so that another node takes over within seconds rather\n" +
			"than when the lease runs out. The question needs a terminal; a script passes --yes.\n\n" + askHelp,
		Example: "  # To whichever node comes first\n" +
			"  pco cluster handoff\n\n" +
			"  # To pve2, from a script\n" +
			"  pco cluster handoff --to pve2 --yes",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			question := "Hand the lease off to whichever node comes first? [y/N]"
			if to != "" {
				question = fmt.Sprintf("Hand the lease off to node %s? [y/N]", to)
			}
			ok, err := a.confirm(cmd, yes, question)
			if err != nil {
				return err
			}
			if !ok {
				return errAborted
			}
			ctx := cmd.Context()
			if err := a.client().Handoff(ctx, to); err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			if to != "" {
				s.printf("The lease is handed off to node %s, which alone may take it for 30 seconds.\n", to)
			} else {
				s.println("The lease is handed off; whichever node comes first takes it, and this node holds back for 30 seconds.")
			}
			return s.done()
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "the node that takes the lease; none for whichever comes first")
	addYesFlag(cmd, &yes)
	return cmd
}

func (a *app) clusterJoinTokenCmd() *cobra.Command {
	var node, file string
	cmd := &cobra.Command{
		Use:   "join-token",
		Short: "Make the token an appliance agent joins with",
		Long: "Make the token an appliance agent container of a node enrols with, once, within 15 minutes:\n" +
			"it names the controller's endpoints and the fingerprint of the install's CA, and carries a\n" +
			"secret the daemon keeps only the hash of. The token is written to the file --file names, which\n" +
			"only root may read, or printed once when the output is a terminal; it never goes into a log or\n" +
			"an event. On the node: pco appliance install --kind agent --join-file <path>. A token for a\n" +
			"contested node is refused; pco node forget ends that first.\n\n" + askHelp,
		Example: "  # Into a file for pct push\n" +
			"  pco cluster join-token --node pve3 --file /root/pve3.join\n\n" +
			"  # Shown once on the terminal\n" +
			"  pco cluster join-token --node pve3",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			if node == "" {
				return errors.New("name the node the agent joins as, with --node")
			}
			if file == "" && !a.stderrTerminal(cmd.OutOrStdout()) {
				return errors.New("the output is no terminal: write the token to a file with --file, so that it is shown nowhere")
			}
			if file != "" {
				if _, err := os.Lstat(file); err == nil {
					return fmt.Errorf("%s exists already; name a new file", file)
				}
			}
			ctx := cmd.Context()
			v, err := a.client().JoinToken(ctx, node)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			if file != "" {
				if err := writeTokenFile(file, v.Token); err != nil {
					return err
				}
				s.printf("The join token for node %s is in %s (mode 0600); it is good once, until %s.\n"+
					"On the node: pco appliance install --kind agent --join-file %s\n", v.Node, file, a.when(v.Expires), filepath.Base(file))
				return s.done()
			}
			s.printf("The join token for node %s, good once, until %s:\n\n%s\n\n"+
				"On the node: pco appliance install --kind agent --join -\n", v.Node, a.when(v.Expires), v.Token)
			return s.done()
		},
	}
	cmd.Flags().StringVar(&node, "node", "", "the node the agent joins as")
	cmd.Flags().StringVar(&file, "file", "", "the file to write the token to, made with mode 0600; without it the token is printed once, on a terminal only")
	return cmd
}

// writeTokenFile writes a token to a file that only its owner may read.
func writeTokenFile(path, token string) error {
	if err := atomicfile.Write(path, []byte(token+"\n"), atomicfile.Options{Mode: 0o600}); err != nil {
		return fmt.Errorf("writing the token to %s: %w", path, err)
	}
	return nil
}

func (a *app) clusterFinishUpgradeCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "finish-upgrade",
		Short: "End an upgrade from an earlier release",
		Long: "End an upgrade from an earlier release of pco. Until then the previous release can be\n" +
			"installed again on any node and serves the same routes, and the settings this release adds\n" +
			"cannot be set. It is refused while a registered node that was not forgotten runs a build that\n" +
			"does not read this release's store; a node that is down counts, so a dead node is forgotten\n" +
			"first with pco node forget. The store is copied below /var/lib/pco/backup on the controller's\n" +
			"node before anything changes; pco export is the backup to keep. The question needs a\n" +
			"terminal; a script passes --yes. It runs as root on the controller.\n\n" + askHelp,
		Example: "  # Once every node runs this release\n" +
			"  pco cluster finish-upgrade\n\n" +
			"  # From a script\n" +
			"  pco cluster finish-upgrade --yes",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			ok, err := a.confirm(cmd, yes, "End the upgrade? The previous release cannot be installed again afterwards. [y/N]")
			if err != nil {
				return err
			}
			if !ok {
				return errAborted
			}
			ctx := cmd.Context()
			res, err := a.client().FinishUpgrade(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			s.printf("The upgrade from %s is finished; the store is at schema %d.\nA copy of the store from before is in %s.\n", res.From, res.Schema, res.Backup)
			return s.done()
		},
	}
	addYesFlag(cmd, &yes)
	return cmd
}

func (a *app) clusterUnblockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unblock <address>",
		Short: "End the block of an address every node blocks",
		Long: "End the block of an address that every node blocks because an agent's forwarding-table watch\n" +
			"saw the MAC of its guest on the port of another guest, or leave the guest's port while the\n" +
			"guest did not migrate. A block ends by itself ten minutes after the last report of it, once\n" +
			"the guest's node attests the card again; this ends it now, when what blocked it is found and\n" +
			"gone. pco cluster status counts the blocks; pco status --json lists them under\n" +
			"cluster.blocks.\n\n" + askHelp,
		Example: "  # End the block of an address\n" +
			"  pco cluster unblock 10.0.0.7\n\n" +
			"  # The blocks\n" +
			"  pco status --json | jq '.cluster.blocks'",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			addr, err := netip.ParseAddr(args[0])
			if err != nil {
				return fmt.Errorf("%q is no IP address", args[0])
			}
			ctx := cmd.Context()
			if err := a.client().Unblock(ctx, addr); err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			s.printf("The block of %s is ended; the next cycle lets the connectors reach it again.\n", addr)
			return s.done()
		},
	}
}
