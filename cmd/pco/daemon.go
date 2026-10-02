package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/daemon"
)

func (a *app) daemonCmd() *cobra.Command {
	var (
		pveURL, pveCA, node, logLevel string
		clusterDir, privateDir, local string
	)
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the daemon on this node",
		Long: "Run the daemon: it reads the guests from Proxmox, keeps the tunnels, the connectors and\n" +
			"the DNS records at Cloudflare in line with their notes, and answers the commands of\n" +
			"pco on its socket. It runs until SIGINT or SIGTERM.\n\n" +
			"The directories of the store are those of a Proxmox node; the flags move them for a test\n" +
			"or an unusual install, and the check for the cluster filesystem goes with the cluster and\n" +
			"private directories.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			level, err := zerolog.ParseLevel(logLevel)
			if err != nil || level == zerolog.NoLevel {
				return fmt.Errorf("unknown log level %q: want trace, debug, info, warn or error", logLevel)
			}
			log := zerolog.New(cmd.ErrOrStderr()).Level(level).With().Timestamp().Logger()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return daemon.Run(ctx, daemon.Config{
				Version:    a.version,
				PVEURL:     pveURL,
				PVECAFile:  pveCA,
				Node:       node,
				SocketPath: a.socket,
				Paths:      daemon.StorePaths(clusterDir, privateDir, local),
				Log:        log,
			}, a.daemon)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&pveURL, "pve-url", daemon.DefaultPVEURL, "base URL of the Proxmox API")
	flags.StringVar(&pveCA, "pve-ca-file", "", "CA bundle to verify the Proxmox API with; not needed for a loopback URL")
	flags.StringVar(&node, "node", defaultNode(), "name of this node in Proxmox")
	flags.StringVar(&logLevel, "log-level", "info", "log level: trace, debug, info, warn or error")
	flags.StringVar(&clusterDir, "cluster-dir", "", "directory of the state the cluster shares (default "+daemon.StorePaths("", "", "").Cluster+")")
	flags.StringVar(&privateDir, "private-dir", "", "directory of the secrets the cluster shares (default "+daemon.StorePaths("", "", "").Private+")")
	flags.StringVar(&local, "local-dir", "", "directory of the state of this node (default "+daemon.StorePaths("", "", "").Local+")")
	return cmd
}

// defaultNode is the name Proxmox gives this node: the host name up to its
// first dot.
func defaultNode() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return shortHostname(host)
}

func shortHostname(host string) string {
	short, _, _ := strings.Cut(host, ".")
	return short
}
