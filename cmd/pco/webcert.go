package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

// webCertModes says what each mode of the certificate is.
var webCertModes = map[string]string{
	webcert.ModeCA:       "a key of its own and a certificate signed by the cluster CA, renewed by pco",
	webcert.ModeOwn:      "the admin's own certificate and key",
	webcert.ModePVEProxy: "the certificate and the key pveproxy serves",
}

func (a *app) webCertCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cert",
		Short: "Show the certificate of the web interface",
		Long: "Show the certificate the web interface serves: its mode, the names it is for, until when it\n" +
			"is valid and its SHA-256 fingerprint, which a browser shows to compare. It runs as root.\n\n" +
			"In mode ca, pco setup's default, the key is the web interface's own and the certificate is\n" +
			"signed by the cluster CA for 90 days; the daemon makes a new one 30 days before it expires,\n" +
			"and when the node's names or addresses or the listen address change.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
	return &cobra.Command{
		Use:   "renew",
		Short: "Make a new key and certificate of the cluster CA for the web interface now",
		Long: "Make a new key and certificate signed by the cluster CA for the web interface now, and\n" +
			"restart pco-web.service so that it serves them, as after a suspected leak of its key.\n" +
			"Only a certificate of mode ca is renewed. It runs as root.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
}

func (a *app) webCertImportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import <crt> <key>",
		Short: "Serve a certificate and key of your own in the web interface",
		Long: "Check a certificate and its key, both PEM, and put them in place of the ones the web\n" +
			"interface serves, which makes its mode own, then restart pco-web.service. The key must be\n" +
			"the certificate's, the certificate valid now and for one of the node's names or addresses.\n" +
			"pco does not renew it; pco doctor warns 30 days before it expires. It runs as root.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
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

// webCertSetup is the setup of the node the certificate commands work on.
func (a *app) webCertSetup(cmd *cobra.Command) (*setup.Setup, error) {
	if err := a.noJSON(cmd); err != nil {
		return nil, err
	}
	if os.Geteuid() != 0 {
		return nil, errors.New("the certificate of the web interface is root's: run this command as root")
	}
	return a.nodeSetup(a.newPrompter(cmd, 0, false))
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
