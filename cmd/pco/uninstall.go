package main

import (
	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

func (a *app) uninstallCmd() *cobra.Command {
	var o setup.UninstallOptions
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
			"finishes the rest. It runs as root on the node.",
		Example: "  # Remove pco, asking about everything\n" +
			"  pco uninstall\n\n" +
			"  # Without a question, deleting the DNS records and the tunnel and removing cloudflared\n" +
			"  pco uninstall --yes --purge-cloudflare --remove-cloudflared\n\n" +
			"  # Without a question, leaving Cloudflare as it is\n" +
			"  pco uninstall --yes --keep-cloudflare",
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
	cmd.MarkFlagsMutuallyExclusive("purge-cloudflare", "keep-cloudflare")
	return cmd
}
