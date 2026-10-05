package main

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// setupFlags are the flags of pco setup.
type setupFlags struct {
	yes, noTags, skipCloudflared bool
	repair, recover, newInstall  bool
	verbose                      bool
	tokenFile                    string
	tokenStdin                   bool
	installID                    string

	webCert, webCertFile, webKeyFile, webListen string
	noWeb                                       bool
}

func (a *app) setupCmd() *cobra.Command {
	var f setupFlags
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Prepare this Proxmox VE node for pco, repair it, or recover a lost store",
		Long: "Prepare this Proxmox VE node for pco: role PCO, user pco@pve and its API token, the gate\n" +
			"tags as registered tags, the cloudflared package, a first Cloudflare token, the identity of\n" +
			"the install and the daemon. Every step looks first: running setup again finishes a run that\n" +
			"stopped, and changes nothing on a node that is set up. What setup creates is noted in the\n" +
			"manifest that pco uninstall follows. It runs as root on the node.\n\n" +
			"The Cloudflare token is read from --cf-token-file, from standard input with --cf-token-stdin,\n" +
			"or asked for without being shown; never from an argument or the environment. It is stored\n" +
			"only when it can do what pco needs, and only while the daemon is stopped.\n\n" +
			"--repair re-asserts the role, the user, the token and the tags, as after a restore of the\n" +
			"node. --recover adopts the install whose tunnels the Cloudflare token sees, after its store\n" +
			"was lost; when the token sees several, --install-id chooses.\n\n" +
			"A node whose store holds no install but that runs connectors of one is refused, as a new\n" +
			"install would never prune them: pco setup --recover adopts that install, and pco uninstall\n" +
			"--keep-cloudflare removes pco from the node so that setup can start over. --new-install\n" +
			"starts a new install beside them regardless.\n\n" +
			"The web interface, pco-web.service, listens on the node's address in the cluster status,\n" +
			"port 8643, or on --web-listen; --no-web leaves it out. Its certificate, by --web-cert:\n" +
			"  ca        a key of its own and a certificate signed by the cluster CA for 90 days, which\n" +
			"            the daemon renews; browsers that trust the cluster CA, as for port 8006, take it\n" +
			"            (the default)\n" +
			"  own       the certificate and key of --web-cert-file and --web-key-file, copied in\n" +
			"  pveproxy  the certificate and key pveproxy serves: the web interface then holds\n" +
			"            pveproxy's own key\n" +
			"--repair keeps the mode chosen before unless --web-cert says otherwise.",
		// A token typed where a flag value belongs is an argument: the error
		// must not repeat it.
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("%q takes no arguments: the Cloudflare token is read from --cf-token-file, --cf-token-stdin or a prompt", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			p, err := a.prompter(cmd, f.yes)
			if err != nil {
				return err
			}
			o, err := a.setupOptions(cmd, f)
			if err != nil {
				return err
			}
			s, err := a.nodeSetup(p)
			if err != nil {
				return err
			}
			return s.Run(cmd.Context(), o)
		},
	}
	flags := cmd.Flags()
	flags.BoolVarP(&f.yes, "yes", "y", false, "take the default answers and ask nothing (needed without a terminal)")
	flags.StringVar(&f.tokenFile, "cf-token-file", "", "read the Cloudflare API token from this file")
	flags.BoolVar(&f.tokenStdin, "cf-token-stdin", false, "read the Cloudflare API token from standard input, which must not be a terminal")
	flags.BoolVar(&f.noTags, "no-registered-tags", false, "do not register the gate tags, which lets whoever may edit a guest set them")
	flags.BoolVar(&f.skipCloudflared, "skip-cloudflared", false, "do not install cloudflared when it is missing")
	flags.BoolVar(&f.repair, "repair", false, "re-assert the role, the user, the token and the tags in Proxmox, and start the daemon")
	flags.BoolVar(&f.verbose, "verbose", false, "name every zone the Cloudflare token leaves out, not only those this install served")
	flags.BoolVar(&f.recover, "recover", false, "adopt the install whose tunnels the Cloudflare token sees, after the store was lost")
	flags.StringVar(&f.installID, "install-id", "", "with --recover: the install to adopt, when the token sees several")
	flags.BoolVar(&f.newInstall, "new-install", false, "start a new install on a node that runs connectors of another install: "+
		"the connectors of the other install keep running and are never pruned by the new one; pco status reports them")
	flags.StringVar(&f.webCert, "web-cert", "", "the certificate of the web interface: ca (the default), own or pveproxy")
	flags.StringVar(&f.webCertFile, "web-cert-file", "", "with --web-cert own: the certificate, PEM")
	flags.StringVar(&f.webKeyFile, "web-key-file", "", "with --web-cert own: the key of the certificate, PEM")
	flags.StringVar(&f.webListen, "web-listen", "", "the address the web interface listens on, with or without a port "+
		"(default: the node's address in the cluster status, port 8643)")
	flags.BoolVar(&f.noWeb, "no-web", false, "do not set up the web interface")
	cmd.MarkFlagsMutuallyExclusive("cf-token-file", "cf-token-stdin")
	cmd.MarkFlagsMutuallyExclusive("repair", "recover")
	cmd.MarkFlagsMutuallyExclusive("new-install", "repair")
	cmd.MarkFlagsMutuallyExclusive("new-install", "recover")
	return cmd
}

func (a *app) setupOptions(cmd *cobra.Command, f setupFlags) (setup.Options, error) {
	o := setup.Options{
		Yes: f.yes, Repair: f.repair, Recover: f.recover, InstallID: f.installID, NewInstall: f.newInstall, Verbose: f.verbose,
		WebCert: f.webCert, WebCertFile: f.webCertFile, WebKeyFile: f.webKeyFile, WebListen: f.webListen, NoWeb: f.noWeb,
	}
	no := false
	if f.noTags {
		o.RegisterTags = &no
	}
	if f.skipCloudflared {
		o.InstallCloudflared = &no
	}
	token, err := a.setupToken(cmd, f)
	if err != nil {
		return setup.Options{}, err
	}
	o.CloudflareToken = token
	return o, nil
}

// setupToken reads the Cloudflare token given in advance: from the file, or
// piped into standard input. Without either, setup asks for it, or does
// without. A trailing newline is not part of the token.
func (a *app) setupToken(cmd *cobra.Command, f setupFlags) (string, error) {
	var raw []byte
	var err error
	switch in := cmd.InOrStdin(); {
	case f.tokenFile != "":
		var loose bool
		if raw, loose, err = readTokenFile(f.tokenFile); loose {
			s := &screen{w: cmd.ErrOrStderr()}
			s.printf("warning: %s can be read by others; restrict it with chmod 600\n", f.tokenFile)
		}
	case f.tokenStdin:
		if _, terminal := a.stdinTerminal(in); terminal {
			return "", errors.New("--cf-token-stdin reads a token piped in; on a terminal, setup asks for it without --cf-token-stdin")
		}
		raw, err = io.ReadAll(io.LimitReader(in, maxTokenInput+1))
		if err == nil && len(raw) > maxTokenInput {
			err = errors.New("the input is too long for a token")
		}
	default:
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the Cloudflare token: %w", err)
	}
	token := strings.TrimRight(string(raw), "\r\n")
	if strings.TrimSpace(token) == "" {
		return "", errors.New("the Cloudflare token is empty")
	}
	return token, nil
}

// prompter is the operator of pco setup and pco uninstall: the terminal, or
// with --yes nobody, as --yes takes the default of every question no flag
// answers.
func (a *app) prompter(cmd *cobra.Command, yes bool) (*cliPrompter, error) {
	fd, terminal := a.stdinTerminal(cmd.InOrStdin())
	switch {
	case yes:
		return a.newPrompter(cmd, fd, false), nil
	case terminal:
		return a.newPrompter(cmd, fd, true), nil
	}
	return nil, errNoTerminal
}

// nodeSetup returns the setup of this node, which works on its store
// directly and not through the daemon.
func (a *app) nodeSetup(p setup.Prompter) (*setup.Setup, error) {
	override, err := a.cloudflareOverride()
	if err != nil {
		return nil, err
	}
	if os.Geteuid() != 0 {
		return nil, errors.New("this command changes the node: run it as root")
	}
	if override != "" {
		p.Warn("%s", overrideLine(override))
	}
	st, err := store.Open(store.DefaultPaths())
	if err != nil {
		return nil, fmt.Errorf("opening the store: %w", err)
	}
	return setup.New(setup.NewHostRunner(), p, st, cloudflareClients(override), a.now, rand.Reader), nil
}

// cliPrompter is the operator at the terminal. What it prints is cleaned as
// everything the commands print, as it carries the output of commands and
// the messages of Cloudflare. One that is not interactive asks nothing and
// takes the default answers.
type cliPrompter struct {
	a           *app
	interactive bool
	fd          int
	in          *bufio.Reader
	out, errOut io.Writer
}

func (a *app) newPrompter(cmd *cobra.Command, fd int, interactive bool) *cliPrompter {
	return &cliPrompter{
		a: a, interactive: interactive, fd: fd,
		in: bufio.NewReader(cmd.InOrStdin()), out: cmd.OutOrStdout(), errOut: cmd.ErrOrStderr(),
	}
}

// Confirm asks until the answer is yes or no; an empty one is the default.
func (p *cliPrompter) Confirm(question string, def bool) (bool, error) {
	if !p.interactive {
		return def, nil
	}
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	for {
		s := &screen{w: p.errOut}
		s.printf("%s %s ", question, hint)
		if err := s.done(); err != nil {
			return false, err
		}
		line, err := p.in.ReadString('\n')
		if err != nil {
			s.println("")
			return false, errors.New("no answer: the input ended")
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
	}
}

// Secret asks for a secret without showing what is typed.
func (p *cliPrompter) Secret(question string) (string, error) {
	if !p.interactive {
		return "", nil
	}
	s := &screen{w: p.errOut}
	s.printf("%s", question)
	if err := s.done(); err != nil {
		return "", err
	}
	raw, err := p.a.readPassword(p.fd)
	// The newline the operator typed was not echoed.
	s.println("")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func (p *cliPrompter) Info(format string, args ...any) {
	s := &screen{w: p.out}
	s.printf(format+"\n", args...)
}

func (p *cliPrompter) Warn(format string, args ...any) {
	s := &screen{w: p.errOut}
	s.printf("warning: "+format+"\n", args...)
}
