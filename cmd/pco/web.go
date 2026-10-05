package main

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/auth"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/ui"
)

// webFlags are the flags of pco web. What is not given comes from the
// environment of the unit.
type webFlags struct {
	listen, cert, key, hosts, logLevel string
	pin, pveURL                        string
}

// defaultPVEURL is the API of the node's own pveproxy, which checks the
// tickets and tokens of sign-ins.
const defaultPVEURL = "https://127.0.0.1:8006/api2/json"

const pveTimeout = 5 * time.Second

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
			"$CREDENTIALS_DIRECTORY, where systemd puts the credentials of the unit, and without\n" +
			"--pin the certificate pveproxy serves, which its answers to sign-ins must present,\n" +
			"is pveproxy.crt there.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			level, err := logLevel(f.logLevel)
			if err != nil {
				return err
			}
			cfg, err := a.webConfig(f)
			if err != nil {
				return err
			}
			cfg.Log = a.daemonLog(cmd.ErrOrStderr(), level)
			if !ui.Built {
				cfg.Log.Warn().Msg("this build of pco has no web interface; it serves a page that says so")
			}
			sessions, err := a.webAuth(f, cfg)
			if err != nil {
				return err
			}
			srv, err := web.New(cfg, sessions)
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
	flags.StringVar(&f.logLevel, "log-level", "info", "log level: trace, debug, info, warn or error")
	flags.StringVar(&f.pin, "pin", "", "the certificate pveproxy serves, PEM (default $CREDENTIALS_DIRECTORY/pveproxy.crt)")
	flags.StringVar(&f.pveURL, "pve-url", defaultPVEURL, "the Proxmox VE API that checks sign-ins")
	cmd.AddCommand(a.webCertCmd())
	return cmd
}

// logLevel is the level a --log-level names.
func logLevel(name string) (zerolog.Level, error) {
	level, err := zerolog.ParseLevel(name)
	if err != nil || level == zerolog.NoLevel {
		return zerolog.NoLevel, fmt.Errorf("unknown log level %q: want trace, debug, info, warn or error", name)
	}
	return level, nil
}

// webAuth is the sign-in of pco web, checked by the node's pveproxy, which
// must present the certificate of the pin.
func (a *app) webAuth(f webFlags, cfg web.Config) (*auth.Auth, error) {
	file, err := a.webPinFile(f)
	if err != nil {
		return nil, err
	}
	pin, err := readCertificate(file)
	if err != nil {
		return nil, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("reading the host name: %w", err)
	}
	etcHosts, _ := os.ReadFile("/etc/hosts")
	node, fqdn := nodeNames(hostname, etcHosts)
	hosts := auth.AllowedHosts(cfg.Listen, append([]string{node, fqdn}, cfg.Hosts...)...)
	link, linkErr := os.Readlink("/etc/localtime")
	return auth.New(auth.NewPVE(f.pveURL, pin, pveTimeout), auth.Config{
		Now:     a.now,
		Hosts:   func() []string { return hosts },
		Profile: store.ProfileHost,
		Node:    node,
		Zone:    nodeZone(a.getenv("TZ"), link, linkErr),
		Version: a.version,
		Log:     cfg.Log,
	}), nil
}

// webPinFile is the certificate of pveproxy: never tls.crt, which is pco
// web's own.
func (a *app) webPinFile(f webFlags) (string, error) {
	switch creds := a.getenv("CREDENTIALS_DIRECTORY"); {
	case f.pin != "":
		return f.pin, nil
	case creds != "":
		return filepath.Join(creds, "pveproxy.crt"), nil
	}
	return "", errors.New("no certificate of pveproxy to check sign-ins against: give --pin, or run pco web from pco-web.service, which passes it as a credential")
}

// readCertificate reads the first certificate of a PEM file.
func readCertificate(file string) (*x509.Certificate, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("reading the certificate of pveproxy: %w", err)
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		switch {
		case block == nil:
			return nil, fmt.Errorf("%s holds no certificate", file)
		case block.Type == "CERTIFICATE":
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("reading the certificate of pveproxy in %s: %w", file, err)
			}
			return cert, nil
		}
	}
}

// nodeNames are the node's short name and its FQDN. Proxmox VE has the node's
// name mapped to its FQDN in /etc/hosts, which is where hostname -f finds it
// too; no resolver is asked.
func nodeNames(hostname string, etcHosts []byte) (string, string) {
	short := shortHostname(hostname)
	if short != hostname {
		return short, hostname
	}
	for line := range strings.Lines(string(etcHosts)) {
		line, _, _ = strings.Cut(line, "#")
		fields := strings.Fields(line)
		if len(fields) < 2 || !slices.Contains(fields[1:], short) {
			continue
		}
		for _, name := range fields[1:] {
			if strings.HasPrefix(name, short+".") {
				return short, name
			}
		}
	}
	return short, ""
}

// nodeZone is the name of the node's time zone, as the page shows the node's
// times in: TZ when it is set, else what /etc/localtime links to. Without
// /etc/localtime the zone is UTC; a copy of a zone file has no name.
func nodeZone(tz, link string, linkErr error) string {
	if tz != "" {
		return strings.TrimPrefix(tz, ":")
	}
	if errors.Is(linkErr, fs.ErrNotExist) {
		return "UTC"
	}
	if _, zone, ok := strings.Cut(link, "zoneinfo/"); linkErr == nil && ok {
		return zone
	}
	return ""
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
