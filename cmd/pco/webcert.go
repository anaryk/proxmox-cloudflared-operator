package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

// webCertModes says what each mode of the certificate is.
var webCertModes = map[string]string{
	webcert.ModeCA:         "a key of its own and a certificate signed by the cluster CA, renewed by pco",
	webcert.ModeOwn:        "the admin's own certificate and key",
	webcert.ModePVEProxy:   "the certificate and the key pveproxy serves",
	webcert.ModeSelfSigned: "a key of its own and a self-signed certificate, renewed by pco",
}

func (a *app) webCertCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cert",
		Short: "Show the certificate of the web interface",
		Long: "Show the certificate the web interface serves: its mode, the names it is for, until when it\n" +
			"is valid and its SHA-256 fingerprint, which a browser shows to compare. It runs as root.\n\n" +
			"In mode ca, pco setup's default, the key is the web interface's own and the certificate is\n" +
			"signed by the cluster CA for 90 days; the daemon makes a new one 30 days before it expires,\n" +
			"and when the node's names or addresses or the listen address change.\n\n" +
			"In the appliance the key is its own and the certificate self-signed for 397 days, made by\n" +
			"the daemon at its first start and again 30 days before it expires: a browser trusts it by\n" +
			"the fingerprint this command prints.",
		Example: "  # The certificate the web interface serves, and its fingerprint\n" +
			"  pco web cert\n\n" +
			"  # A new key and certificate of the cluster CA now\n" +
			"  pco web cert renew",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.webCertRoot(cmd); err != nil {
				return err
			}
			appliance, err := a.isAppliance()
			if err != nil {
				return err
			}
			if appliance {
				st, err := a.applianceWebCert()
				if err != nil {
					return err
				}
				return a.printWebCert(cmd.OutOrStdout(), st)
			}
			s, err := a.webCertSetup(cmd)
			if err != nil {
				return err
			}
			st, err := s.WebCert()
			if err != nil {
				return err
			}
			return a.printWebCert(cmd.OutOrStdout(), st)
		},
	}
	cmd.AddCommand(a.webCertRenewCmd(), a.webCertImportCmd())
	return cmd
}

func (a *app) webCertRenewCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "renew",
		Short: "Make a new key and certificate for the web interface now",
		Long: "Make a new key and certificate signed by the cluster CA for the web interface now, and\n" +
			"restart pco-web.service so that it serves them, as after a suspected leak of its key.\n" +
			"Only a certificate of mode ca is renewed. In the appliance it makes a new key and\n" +
			"self-signed certificate; one of your own it replaces only with --force. It runs as root.",
		Example: "  # A new key and certificate of the cluster CA now\n" +
			"  pco web cert renew\n\n" +
			"  # And the fingerprint to compare in the browser\n" +
			"  pco web cert",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.webCertRoot(cmd); err != nil {
				return err
			}
			appliance, err := a.isAppliance()
			if err != nil {
				return err
			}
			if appliance {
				return a.applianceRenew(cmd.Context(), cmd.OutOrStdout(), force)
			}
			s, err := a.webCertSetup(cmd)
			if err != nil {
				return err
			}
			st, err := s.RenewWebCert(cmd.Context())
			if err != nil {
				return err
			}
			return a.printWebCert(cmd.OutOrStdout(), st)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "in the appliance, replace a certificate of your own with a self-signed one")
	return cmd
}

func (a *app) webCertImportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import <crt> <key>",
		Short: "Serve a certificate and key of your own in the web interface",
		Long: "Check a certificate and its key, both PEM, and put them in place of the ones the web\n" +
			"interface serves, which makes its mode own, then restart pco-web.service. The key must be\n" +
			"the certificate's, the certificate valid now and for one of the node's names or addresses\n" +
			"(the appliance's, in the appliance). pco does not renew it; pco doctor warns 30 days before\n" +
			"it expires. It runs as root.",
		Example: "  # Serve the certificate and key of pco.example.com\n" +
			"  pco web cert import /root/pco.example.com.crt /root/pco.example.com.key\n\n" +
			"  # Back to a certificate of the cluster CA, renewed by pco\n" +
			"  pco setup --repair --web-cert ca",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.webCertRoot(cmd); err != nil {
				return err
			}
			appliance, err := a.isAppliance()
			if err != nil {
				return err
			}
			if appliance {
				return a.applianceImport(cmd.Context(), cmd.OutOrStdout(), args[0], args[1])
			}
			s, err := a.webCertSetup(cmd)
			if err != nil {
				return err
			}
			st, err := s.ImportWebCert(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return a.printWebCert(cmd.OutOrStdout(), st)
		},
	}
}

// webCertRoot refuses what the certificate commands cannot do: --json, and a
// user other than root.
func (a *app) webCertRoot(cmd *cobra.Command) error {
	if err := a.noJSON(cmd); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("the certificate of the web interface is root's: run this command as root")
	}
	return nil
}

func (a *app) isAppliance() (bool, error) {
	profile, err := store.DetectProfile(a.profileFile)
	return profile == store.ProfileAppliance, err
}

// webCertSetup is the setup of the node the certificate commands work on.
func (a *app) webCertSetup(cmd *cobra.Command) (*setup.Setup, error) {
	return a.nodeSetup(a.newPrompter(cmd, 0, false))
}

// applianceWebCert is the certificate the appliance's web interface serves,
// which the daemon makes as it starts.
func (a *app) applianceWebCert() (setup.WebCertStatus, error) {
	mode, err := webcert.ReadMode(a.webDir)
	if err != nil {
		return setup.WebCertStatus{}, err
	}
	certPEM, _, err := webcert.ReadPair(a.webDir)
	if errors.Is(err, fs.ErrNotExist) {
		return setup.WebCertStatus{}, errors.New("the web interface has no certificate yet: the daemon makes one as it starts, " +
			"and systemctl status pco says whether it runs")
	}
	if err != nil {
		return setup.WebCertStatus{}, fmt.Errorf("reading the certificate of the web interface: %w", err)
	}
	leaf, err := webcert.ParseCert(certPEM)
	if err != nil {
		return setup.WebCertStatus{}, fmt.Errorf("reading the certificate of the web interface: %w", err)
	}
	return setup.WebCertStatus{Mode: mode, Leaf: leaf}, nil
}

// applianceNames are the names the appliance's certificate is for now.
func (a *app) applianceNames() (webcert.Names, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return webcert.Names{}, fmt.Errorf("reading the host name: %w", err)
	}
	return webcert.ApplianceNamesFrom(hostname, webcert.NodeFQDN(), webcert.EnvFile, a.net0File)
}

// applianceRenew makes a new self-signed pair in the appliance; the admin's
// own only with force, as on a node pco renews none but its own.
func (a *app) applianceRenew(ctx context.Context, w io.Writer, force bool) error {
	mode, err := webcert.ReadMode(a.webDir)
	if err != nil {
		return err
	}
	if mode == webcert.ModeOwn && !force {
		return errors.New("the certificate of the web interface is your own (mode own), which pco does not renew: " +
			"pco web cert import <crt> <key> puts another of yours in place, and pco web cert renew --force a self-signed one")
	}
	names, err := a.applianceNames()
	if err != nil {
		return err
	}
	if _, err := webcert.MakeSelfSigned(a.webDir, names, a.now(), rand.Reader); err != nil {
		return err
	}
	return a.restartApplianceWeb(ctx, w)
}

func (a *app) applianceImport(ctx context.Context, w io.Writer, certFile, keyFile string) error {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return err
	}
	names, err := a.applianceNames()
	if err != nil {
		return err
	}
	if _, err := webcert.ImportOwn(a.webDir, certPEM, keyPEM, names, a.now()); err != nil {
		return fmt.Errorf("%s and %s: %w", certFile, keyFile, err)
	}
	return a.restartApplianceWeb(ctx, w)
}

// restartApplianceWeb restarts a pco-web that runs, so that it serves the
// certificate in place, and prints that certificate.
func (a *app) restartApplianceWeb(ctx context.Context, w io.Writer) error {
	if out, err := exec.CommandContext(ctx, "systemctl", "try-restart", "pco-web.service").CombinedOutput(); err != nil {
		return fmt.Errorf("the certificate is in place, but restarting pco-web.service failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	st, err := a.applianceWebCert()
	if err != nil {
		return err
	}
	return a.printWebCert(w, st)
}

// printWebCert writes the certificate the web interface serves.
func (a *app) printWebCert(w io.Writer, st setup.WebCertStatus) error {
	leaf := st.Leaf
	names := append([]string(nil), leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		names = append(names, ip.String())
	}
	left := leaf.NotAfter.Sub(a.now())
	expiry := a.when(leaf.NotAfter)
	switch days := int(left.Hours() / 24); {
	case left <= 0:
		expiry += " (expired)"
	case days == 1:
		expiry += " (in 1 day)"
	default:
		expiry += fmt.Sprintf(" (in %d days)", days)
	}
	s := &screen{w: w}
	t := s.table()
	t.row("mode", st.Mode+": "+cmp.Or(webCertModes[st.Mode], "unknown"))
	t.row("names", strings.Join(names, ", "))
	t.row("valid until", expiry)
	t.row("SHA-256", webcert.Fingerprint(leaf))
	t.flush()
	return s.done()
}
