package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/daemon"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
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
			"or an unusual install, all three or none: a store of its own, without the check for the\n" +
			"cluster filesystem. Moving only some of them would mix it with the tokens, the connectors\n" +
			"and the lock of the node.\n\n" +
			"In a terminal the log is written as lines to read, with control characters replaced;\n" +
			"otherwise, as for the journal, as JSON lines.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			paths, err := daemon.StorePaths(clusterDir, privateDir, local)
			if err != nil {
				return err
			}
			level, err := zerolog.ParseLevel(logLevel)
			if err != nil || level == zerolog.NoLevel {
				return fmt.Errorf("unknown log level %q: want trace, debug, info, warn or error", logLevel)
			}
			override, err := a.cloudflareOverride()
			if err != nil {
				return err
			}
			log := a.daemonLog(cmd.ErrOrStderr(), level)
			cfg := daemon.Config{
				Version:    a.version,
				PVEURL:     pveURL,
				PVECAFile:  pveCA,
				Node:       node,
				SocketPath: a.socket,
				Paths:      paths,
				Log:        log,
			}
			deps := a.daemon
			if override != "" {
				line := overrideLine(override)
				log.Warn().Msg(line)
				cfg.Problems = append(cfg.Problems, line)
				deps.CloudflareURL = override
			}
			ctx, stop := signalContext(cmd.Context(), osSignals{}, os.Interrupt, syscall.SIGTERM)
			defer stop()
			return daemon.Run(ctx, cfg, deps)
		},
	}
	defaults := store.DefaultPaths()
	flags := cmd.Flags()
	flags.StringVar(&pveURL, "pve-url", daemon.DefaultPVEURL, "base URL of the Proxmox API")
	flags.StringVar(&pveCA, "pve-ca-file", "", "CA bundle to verify the Proxmox API with; not needed for a loopback URL")
	flags.StringVar(&node, "node", defaultNode(), "name of this node in Proxmox")
	flags.StringVar(&logLevel, "log-level", "info", "log level: trace, debug, info, warn or error")
	flags.StringVar(&clusterDir, "cluster-dir", "", "directory of the state the cluster shares; only with the two others (default "+defaults.Cluster+")")
	flags.StringVar(&privateDir, "private-dir", "", "directory of the secrets the cluster shares; only with the two others (default "+defaults.Private+")")
	flags.StringVar(&local, "local-dir", "", "directory of the state of this node and of the lock of the daemon; only with the two others (default "+defaults.Local+")")
	return cmd
}

// daemonLog is the log of the daemon, written to w: JSON lines for the
// journal, or lines to read when w is a terminal. A message or a field may
// carry text of a guest or of Cloudflare, and on a terminal it is cleaned as
// everything printed is.
func (a *app) daemonLog(w io.Writer, level zerolog.Level) zerolog.Logger {
	if a.stderrTerminal(w) {
		w = zerolog.ConsoleWriter{Out: w, NoColor: true, TimeFormat: time.RFC3339, FormatPrepare: cleanEvent}
	}
	return zerolog.New(w).Level(level).With().Timestamp().Logger()
}

// cleanEvent replaces what a terminal would act on in every text of a log
// event, also inside the lists and objects of a field.
func cleanEvent(event map[string]any) error {
	for k, v := range event {
		event[k] = cleanValue(v)
	}
	return nil
}

func cleanValue(v any) any {
	switch v := v.(type) {
	case string:
		return printable(v)
	case []any:
		for i := range v {
			v[i] = cleanValue(v[i])
		}
	case map[string]any:
		for k, x := range v {
			v[k] = cleanValue(x)
		}
	}
	return v
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
