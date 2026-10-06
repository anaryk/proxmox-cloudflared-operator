package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/auth"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/ui"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

// webFlags are the flags of pco web. What is not given comes from the
// environment of the unit.
type webFlags struct {
	listen, cert, key, hosts, logLevel string
	pin, pveURL, pveAPI                string
}

// defaultPVEURL is the API of the node's own pveproxy, which checks the
// tickets and tokens of sign-ins.
const defaultPVEURL = "https://127.0.0.1:8006/api2/json"

// pveTimeout bounds a check of a ticket or a token; loginTimeout a sign-in
// with a password, which Proxmox VE answers late when it is wrong.
const (
	pveTimeout   = 5 * time.Second
	loginTimeout = 10 * time.Second
)

func (a *app) webCmd() *cobra.Command {
	var f webFlags
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Serve the web interface",
		Long: "Serve the web interface over HTTPS, as pco-web.service does. It runs until SIGINT or\n" +
			"SIGTERM. The calls of the page go to the daemon on --socket, as those of the other\n" +
			"commands do.\n\n" +
			"The flags win over the environment, which the unit reads from /etc/default/pco-web:\n" +
			"  PCO_WEB_LISTEN  the address to listen on (default " + web.DefaultListen + ")\n" +
			"  PCO_WEB_HOSTS   more host names the interface is reached by, comma separated\n" +
			"  PCO_WEB_HSTS    1 to send Strict-Transport-Security, which holds for every port of\n" +
			"                  the host name; off by default\n" +
			"Without --cert and --key the certificate and its key are tls.crt and tls.key in\n" +
			"$CREDENTIALS_DIRECTORY, where systemd puts the credentials of the unit, and without\n" +
			"--pin the certificate pveproxy serves, which its answers to sign-ins must present,\n" +
			"is pveproxy.crt there.\n\n" +
			"In the appliance users sign in with their user and password of Proxmox VE, at the node's\n" +
			"API as the appliance's daemon reaches it (--pve-api, default pve-api.json in\n" +
			"$CREDENTIALS_DIRECTORY, which the daemon writes). It listens on net0's address only, as\n" +
			"pco appliance install wrote it into " + webcert.Net0File + ", and refuses to start on any\n" +
			"other. root@pam may not sign in with a password unless PCO_WEB_ALLOW_ROOT=1.",
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
			profile, err := store.DetectProfile(a.profileFile)
			if err != nil {
				return err
			}
			webAuth := a.webAuth
			if profile == store.ProfileAppliance {
				if err := a.applianceListen(f, &cfg); err != nil {
					return err
				}
				webAuth = a.applianceAuth
			}
			cfg.Log = a.daemonLog(cmd.ErrOrStderr(), level)
			if !ui.Built {
				cfg.Log.Warn().Msg("this build of pco has no web interface; it serves a page that says so")
			}
			sessions, err := webAuth(f, cfg)
			if err != nil {
				return err
			}
			gw := gateway.New(a.socket, sessions, cfg.Log)
			srv, err := web.New(cfg, sessions, gw)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd.Context(), osSignals{}, os.Interrupt, syscall.SIGTERM)
			defer stop()
			return serveWeb(ctx, srv, gw)
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
	flags.StringVar(&f.pveAPI, "pve-api", "", "in the appliance, the node's API as its daemon writes it (default $CREDENTIALS_DIRECTORY/"+webcert.APIName+")")
	cmd.AddCommand(a.webCertCmd())
	return cmd
}

// serveWeb serves the interface until ctx ends, and follows the daemon's
// stream for as long as it does: the subscription starts before the first
// request and ends after the last.
func serveWeb(ctx context.Context, srv *web.Server, gw *gateway.Gateway) error {
	follow, unfollow := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { gw.Run(follow) })
	err := srv.Run(ctx)
	unfollow()
	wg.Wait()
	return err
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
	c, err := a.authConfig(cfg)
	if err != nil {
		return nil, err
	}
	c.Profile = store.ProfileHost
	return auth.New(auth.NewPVE(f.pveURL, pin, pveTimeout), c), nil
}

// authConfig is what the sessions are made with on a node and in the
// appliance alike: the Host headers of the machine's names, and its zone.
func (a *app) authConfig(cfg web.Config) (auth.Config, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return auth.Config{}, fmt.Errorf("reading the host name: %w", err)
	}
	etcHosts, _ := os.ReadFile("/etc/hosts")
	node, fqdn := nodeNames(hostname, etcHosts)
	hosts := auth.AllowedHosts(cfg.Listen, append([]string{node, fqdn}, cfg.Hosts...)...)
	link, linkErr := os.Readlink("/etc/localtime")
	return auth.Config{
		Now:     a.now,
		Hosts:   func() []string { return hosts },
		Node:    node,
		Zone:    nodeZone(a.getenv("TZ"), link, linkErr),
		Version: a.version,
		Log:     cfg.Log,
	}, nil
}

// applianceListen holds the appliance's web interface to net0's address: the
// other cards are legs into the networks of guests, who would reach the
// sign-in page through them. Without an address given it listens there.
func (a *app) applianceListen(f webFlags, cfg *web.Config) error {
	net0, err := webcert.ReadNet0(a.net0File)
	if err != nil {
		return fmt.Errorf("%w, and its address is not known: %w", webcert.ErrNotNet0, err)
	}
	if f.listen == "" && a.getenv("PCO_WEB_LISTEN") == "" {
		cfg.Listen = net.JoinHostPort(net0.String(), webcert.Port)
	}
	return webcert.CheckListen(cfg.Listen, net0)
}

// applianceAuth is the sign-in of the appliance: the user and password of
// Proxmox VE, or a token, checked at the node's API as the appliance's daemon
// reaches it, verified under its server name against the system roots and
// the CA the installer pushed.
func (a *app) applianceAuth(f webFlags, cfg web.Config) (*auth.Auth, error) {
	file := f.pveAPI
	if file == "" {
		creds := a.getenv("CREDENTIALS_DIRECTORY")
		if creds == "" {
			return nil, errors.New("no API of the node to sign users in at: give --pve-api, or run pco web from pco-web.service, which passes it as a credential")
		}
		file = filepath.Join(creds, webcert.APIName)
	}
	api, err := webcert.ReadAPI(file)
	if err != nil {
		return nil, fmt.Errorf("reading the API of the node: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		cfg.Log.Warn().Err(err).Msg("the system roots cannot be read; the API is verified against its CA alone")
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM([]byte(api.CA)) {
		return nil, fmt.Errorf("%s holds no CA that can be read", file)
	}
	allowRoot := false
	switch v := a.getenv("PCO_WEB_ALLOW_ROOT"); v {
	case "1":
		allowRoot = true
	case "", "0":
	default:
		return nil, fmt.Errorf("PCO_WEB_ALLOW_ROOT is %q: want 1 or 0", v)
	}
	c, err := a.authConfig(cfg)
	if err != nil {
		return nil, err
	}
	trust := auth.Trust{Roots: roots, ServerName: api.ServerName}
	c.Profile, c.Node = store.ProfileAppliance, api.Node
	c.Login, c.AllowRoot = auth.NewLogin(api.URL, trust, loginTimeout), allowRoot
	return auth.New(auth.NewTrustedPVE(api.URL, trust, pveTimeout), c), nil
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

// nodeNames are the node's short name and its FQDN, the second read as the
// leaf of the web interface reads it, so that the names it is made for and the
// Host headers pco web answers to are the same.
func nodeNames(hostname string, etcHosts []byte) (string, string) {
	return shortHostname(hostname), webcert.FQDN(hostname, etcHosts)
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
