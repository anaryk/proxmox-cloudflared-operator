package main

import "github.com/spf13/cobra"

// applianceCmd is the group of the commands of the appliance: those run on
// the node that installs, repairs and removes it, and those run inside it.
func (a *app) applianceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "appliance",
		Short: "Commands of the pco appliance",
		Long: "Commands of the pco appliance, the container that runs pco and its connectors on a\n" +
			"Proxmox VE node in place of the host install.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			return cmd.Help()
		},
	}
}
