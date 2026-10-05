package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/ui"
)

// webFlags are the flags of pco web. What is not given comes from the
// environment of the unit.
type webFlags struct {
	listen, cert, key, hosts string
}

func (a *app) webCmd() *cobra.Command {
	var f webFlags
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Serve the web interface",
		Long: "Serve the web interface over HTTPS, as pco-web.service does. It runs until SIGINT or\n" +
			"SIGTERM.\n\n" +
			"The flags win over the environment, which the unit reads from /etc/default/pco-web:\n" +
			"  PCO_WEB_LISTEN  the address to listen on (default " + web.DefaultListen + ")\n" +
			"  PCO_WEB_HOSTS   more host names the interface is reached by, comma separated\n" +
			"  PCO_WEB_HSTS    1 to send Strict-Transport-Security, which holds for every port of\n" +
			"                  the host name; off by default\n" +
			"Without --cert and --key the certificate and its key are tls.crt and tls.key in\n" +
			"$CREDENTIALS_DIRECTORY, where systemd puts the credentials of the unit.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			cfg, err := a.webConfig(f)
			if err != nil {
				return err
			}
			cfg.Log = a.daemonLog(cmd.ErrOrStderr(), zerolog.InfoLevel)
			if !ui.Built {
				cfg.Log.Warn().Msg("this build of pco has no web interface; it serves a page that says so")
			}
			srv, err := web.New(cfg)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd.Context(), osSignals{}, os.Interrupt, syscall.SIGTERM)
			defer stop()
			return srv.Run(ctx)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.listen, "listen", "", "address to listen on (default $PCO_WEB_LISTEN, else "+web.DefaultListen+")")
	flags.StringVar(&f.cert, "cert", "", "certificate file, PEM (default $CREDENTIALS_DIRECTORY/tls.crt)")
	flags.StringVar(&f.key, "key", "", "key file of the certificate, PEM (default $CREDENTIALS_DIRECTORY/tls.key)")
	flags.StringVar(&f.hosts, "hosts", "", "more host names the interface is reached by, comma separated (default $PCO_WEB_HOSTS)")
	return cmd
}

// webConfig is what pco web serves, from the flags and the environment.
func (a *app) webConfig(f webFlags) (web.Config, error) {
	cfg := web.Config{
		Listen: firstOf(f.listen, a.getenv("PCO_WEB_LISTEN"), web.DefaultListen),
		Hosts:  splitList(firstOf(f.hosts, a.getenv("PCO_WEB_HOSTS"))),
		Assets: ui.Assets(),
		Now:    time.Now,
	}
	switch hsts := a.getenv("PCO_WEB_HSTS"); hsts {
	case "1":
		cfg.HSTS = true
	case "", "0":
	default:
		return web.Config{}, fmt.Errorf("PCO_WEB_HSTS is %q: want 1 or 0", hsts)
	}
	switch creds := a.getenv("CREDENTIALS_DIRECTORY"); {
	case (f.cert == "") != (f.key == ""):
		return web.Config{}, errors.New("--cert and --key go together")
	case f.cert != "":
		cfg.CertFile, cfg.KeyFile = f.cert, f.key
	case creds != "":
		cfg.CertFile, cfg.KeyFile = filepath.Join(creds, "tls.crt"), filepath.Join(creds, "tls.key")
	default:
		return web.Config{}, errors.New("no certificate: give --cert and --key, or run pco web from pco-web.service, which passes them as credentials")
	}
	return cfg, nil
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// splitList splits a comma-separated list, leaving out empty items.
func splitList(s string) []string {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
