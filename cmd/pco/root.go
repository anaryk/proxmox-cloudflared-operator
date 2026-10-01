package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/version"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "pco",
		Short:         "Cloudflare Tunnel operator for Proxmox VE",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "pco %s (%s, %s)\n",
				version.Version, version.Commit, version.Date)
			return err
		},
	}
}
