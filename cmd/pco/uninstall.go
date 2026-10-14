package main

import (
	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

func (a *app) uninstallCmd() *cobra.Command {
	var o setup.UninstallOptions
	var cluster bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove pco from this node",
		Long: "Remove pco from this node: the daemon, the connectors, the egress filter, what setup created\n" +
			"in Proxmox as its manifest lists, and the store. It looks first, lists what is there and asks\n" +
			"once; --yes answers that. Deleting the DNS records and the tunnel of the install at\n" +
			"Cloudflare is asked on its own, or done with --purge-cloudflare and left with\n" +
			"--keep-cloudflare; with --yes, an install with Cloudflare credentials needs one of the two,\n" +
			"as nothing on this node can remove those objects once the store is gone. Removing the\n" +
			"cloudflared package setup installed is asked as well, or done with --remove-cloudflared;\n" +
			"with --yes it stays without that flag. A part that fails is\n" +
			"reported and the rest goes on; the store is then kept, and running pco uninstall again\n" +
			"finishes the rest. It runs as root on the node.\n\n" +
			"The SDN zone pco and vnet pconet of the managed network go last, under the lock of the SDN\n" +
			"configuration, and only once no guest of the cluster has a card on pconet: the apply takes\n" +
			"the bridge away from every node. --detach-managed removes the cards pco attached right after\n" +
			"the daemon stopped and waits, bounded, for the configurations to drop them; a VM without\n" +
			"network hotplug drops its card when it restarts.\n\n" +
			"With --cluster, on the controller, the whole install goes: every other node is removed from it\n" +
			"first, so that its agent stands down and stops its connectors, and the lines to run on each\n" +
			"later are printed; then this node is uninstalled as the last one, with the offers of the last\n" +
			"node. A node that is not online holds it unless --force.\n\n" +
			"On a node of a cluster that other nodes keep the install running on, the node leaves it:\n" +
			"it hands the lease off when it holds it, stops and removes its connectors, the egress filter,\n" +
			"the web interface, its address in the firewall ipset pco-nodes and its local state, and its\n" +
			"entry of the node registry becomes removed, so that its certificate is refused. What the\n" +
			"nodes share stays: the store, the credentials, what is at Cloudflare and in Proxmox, and\n" +
			"the firewall rule, which the last node removes. A node that holds the lease and cannot hand\n" +
			"it off, and one whose connectors may be the only ones serving the shared tunnel, are\n" +
			"refused unless --force says to remove them regardless. Drain the node first with\n" +
			"pco node drain.",
		Example: "  # Remove pco, asking about everything\n" +
			"  pco uninstall\n\n" +
			"  # Without a question, deleting the DNS records and the tunnel and removing cloudflared\n" +
			"  pco uninstall --yes --purge-cloudflare --remove-cloudflared\n\n" +
			"  # Without a question, leaving Cloudflare as it is and taking pco's cards off the guests\n" +
			"  pco uninstall --yes --keep-cloudflare --detach-managed\n\n" +
			"  # Take a node out of a cluster that keeps running the install, even while it holds the lease\n" +
			"  pco uninstall --yes --force",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			p, err := a.prompter(cmd, o.Yes)
			if err != nil {
				return err
			}
			s, err := a.nodeSetup(p)
			if err != nil {
				return err
			}
			if cluster {
				if err := a.uninstallCluster(cmd, o.Force); err != nil {
					return err
				}
			}
			return s.Uninstall(cmd.Context(), o)
		},
	}
	flags := cmd.Flags()
	flags.BoolVarP(&o.Yes, "yes", "y", false,
		"remove pco without asking (needed without a terminal); an install with Cloudflare credentials needs "+
			"--purge-cloudflare or --keep-cloudflare as well, and cloudflared stays without --remove-cloudflared")
	flags.BoolVar(&o.PurgeCloudflare, "purge-cloudflare", false, "delete the DNS records and the tunnel of the install at Cloudflare")
	flags.BoolVar(&o.KeepCloudflare, "keep-cloudflare", false,
		"leave the DNS records and the tunnel of the install at Cloudflare as they are, without looking at them")
	flags.BoolVar(&o.RemoveCloudflared, "remove-cloudflared", false, "remove the cloudflared package and its apt source, when setup installed them")
	flags.BoolVar(&o.DetachManaged, "detach-managed", false, "remove the cards pco attached on the managed network first, so that its zone and vnet can go")
	flags.BoolVar(&o.Force, "force", false, "take a node out of a cluster although it holds the lease and cannot hand it off, or no other node is active; "+
		"with --cluster, remove the other nodes although one is not online")
	flags.BoolVar(&cluster, "cluster", false, "uninstall the whole install: remove every other node from it first, then this node as the last one")
	cmd.MarkFlagsMutuallyExclusive("purge-cloudflare", "keep-cloudflare")
	return cmd
}

// uninstallCluster removes every other node from the install through the
// controller on this node, before this node is uninstalled as the last one:
// their agents stand down, and what pco keeps on each is removed there.
func (a *app) uninstallCluster(cmd *cobra.Command, force bool) error {
	ctx := cmd.Context()
	res, err := a.client().UninstallCluster(ctx, force)
	if err != nil {
		return a.explain(ctx, err)
	}
	s := &screen{w: cmd.OutOrStdout()}
	if len(res.Left) == 0 {
		s.println("No other node is in the install.")
	} else {
		s.println("The other nodes are removed from the install; their agents stand down. On each, later:")
		for _, line := range res.Left {
			s.printf("  - %s\n", line)
		}
	}
	return s.done()
}
