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
			"in Proxmox as its manifest lists, and the store. It lists what goes and asks once; --yes\n" +
			"answers that. Deleting the DNS records and the tunnel of the install at Cloudflare is asked\n" +
			"on its own, or done with --purge-cloudflare, and so is removing the cloudflared package\n" +
			"setup installed, with --remove-cloudflared. A part that fails is reported and the rest goes\n" +
			"on; the store is then kept, and running pco uninstall again finishes the rest. It runs as\n" +
			"root on the node.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			p, err := a.uninstallPrompter(cmd, o.Yes)
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
		"remove pco without asking (needed without a terminal); Cloudflare and cloudflared are still asked on a terminal")
	flags.BoolVar(&o.PurgeCloudflare, "purge-cloudflare", false, "delete the DNS records and the tunnel of the install at Cloudflare")
	flags.BoolVar(&o.RemoveCloudflared, "remove-cloudflared", false, "remove the cloudflared package and its apt source, when setup installed them")
	return cmd
}

// uninstallPrompter is the operator of pco uninstall. On a terminal every
// question that no flag answers is asked; without one, --yes is needed, and
// what no flag says is not done.
func (a *app) uninstallPrompter(cmd *cobra.Command, yes bool) (*cliPrompter, error) {
	fd, terminal := a.stdinTerminal(cmd.InOrStdin())
	switch {
	case terminal:
		return a.newPrompter(cmd, fd, true), nil
	case yes:
		return a.newPrompter(cmd, fd, false), nil
	}
	return nil, errNoTerminal
}
