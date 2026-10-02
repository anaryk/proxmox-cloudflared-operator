package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const (
	// maxTokenInput is how much of a token file or of stdin is read: a token
	// has a few dozen characters.
	maxTokenInput = 64 << 10
	tokenPrompt   = "Cloudflare API token: "
)

func (a *app) credentialCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "credential",
		Short: "Manage the Cloudflare tokens of the daemon",
	}
	cmd.AddCommand(a.credentialListCmd(), a.credentialAddCmd(), a.credentialCheckCmd(), a.credentialRemoveCmd())
	return cmd
}

func (a *app) credentialListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the stored credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if a.json {
				raw, err := a.client().CredentialsRaw(ctx)
				if err != nil {
					return a.explain(ctx, err)
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			views, err := a.client().Credentials(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			return a.renderCredentials(cmd.OutOrStdout(), views)
		},
	}
}

func (a *app) renderCredentials(w io.Writer, views []engine.CredentialView) error {
	s := &screen{w: w}
	if len(views) == 0 {
		s.println("No credentials.")
		return s.done()
	}
	t := s.table()
	t.row("ID", "LABEL", "KIND", "STATE", "NOTE")
	for _, v := range views {
		t.row(v.ID, dash(v.Label), dash(v.Kind), credentialState(v), dash(a.credentialNote(v)))
	}
	t.flush()
	return s.done()
}

func (a *app) credentialAddCmd() *cobra.Command {
	var label, tokenFile string
	cmd := &cobra.Command{
		Use:   "add --label <label> [--token-file <file>]",
		Short: "Check a Cloudflare API token and store it",
		Long: "Check what a Cloudflare API token can do and store it when the check passes. The\n" +
			"token is read from the file, or from standard input; on a terminal it is asked for\n" +
			"without being shown. It is never taken from an argument, which others could read.",
		// A token typed where a flag value belongs is an argument: the error
		// must not repeat it.
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("%q takes no arguments: the token is read from standard input or --token-file", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if strings.TrimSpace(label) == "" {
				return errors.New("--label is required")
			}
			token, err := a.readToken(cmd, tokenFile)
			if err != nil {
				return err
			}
			view, err := a.client().AddCredential(ctx, label, token)
			if err != nil {
				// A token the daemon refused comes with what it can do.
				if view.Checked {
					if perr := a.printCredential(cmd, view, ""); perr != nil {
						return perr
					}
				}
				return a.explain(ctx, err)
			}
			return a.printCredential(cmd, view, fmt.Sprintf("Added credential %s (%s).", view.ID, view.Label))
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "name of the credential")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "read the token from this file instead of standard input")
	return cmd
}

// readToken reads the token from the file, or from standard input; a terminal
// is asked for it without echo. A trailing newline is not part of the token.
func (a *app) readToken(cmd *cobra.Command, file string) (string, error) {
	var raw []byte
	var err error
	in := cmd.InOrStdin()
	switch fd, terminal := a.stdinTerminal(in); {
	case file != "":
		var loose bool
		if raw, loose, err = readTokenFile(file); loose {
			s := &screen{w: cmd.ErrOrStderr()}
			s.printf("warning: %s can be read by others; restrict it with chmod 600\n", file)
		}
	case terminal:
		raw, err = a.promptToken(cmd, fd)
	default:
		raw, err = io.ReadAll(io.LimitReader(in, maxTokenInput+1))
		if err == nil && len(raw) > maxTokenInput {
			err = errors.New("the input is too long for a token")
		}
	}
	if err != nil {
		return "", fmt.Errorf("reading the token: %w", err)
	}
	token := strings.TrimRight(string(raw), "\r\n")
	if strings.TrimSpace(token) == "" {
		return "", errors.New("the token is empty")
	}
	return token, nil
}

func (a *app) promptToken(cmd *cobra.Command, fd int) ([]byte, error) {
	s := &screen{w: cmd.ErrOrStderr()}
	s.printf("%s", tokenPrompt)
	if err := s.done(); err != nil {
		return nil, err
	}
	raw, err := a.readPassword(fd)
	// The newline the admin typed was not echoed.
	s.println("")
	return raw, err
}

// readTokenFile reads a file of at most maxTokenInput bytes, and says whether
// group or others can read it.
func readTokenFile(path string) (raw []byte, loose bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err == nil {
		loose = info.Mode().Perm()&0o077 != 0
	}
	raw, err = io.ReadAll(io.LimitReader(f, maxTokenInput+1))
	if err != nil {
		return nil, loose, fmt.Errorf("%s: %w", path, err)
	}
	if len(raw) > maxTokenInput {
		return nil, loose, fmt.Errorf("%s is too long for a token", path)
	}
	return raw, loose, nil
}

func (a *app) credentialCheckCmd() *cobra.Command {
	var deep, yes bool
	cmd := &cobra.Command{
		Use:   "check <id>",
		Short: "Check what the token of a credential can do",
		Long: "Check what the token of a stored credential can do. A deep check proves the write\n" +
			"permissions by creating and deleting a test DNS record and a test tunnel; it asks\n" +
			"first, which needs a terminal, and a script passes --yes.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if deep {
				ok, err := a.confirm(cmd, yes, "A deep check creates and deletes a test DNS record and a test tunnel. Continue? [y/N]")
				if err != nil {
					return err
				}
				if !ok {
					return errAborted
				}
			}
			view, err := a.client().CheckCredential(ctx, args[0], deep)
			if err != nil {
				return a.explain(ctx, err)
			}
			return a.printCredential(cmd, view, "")
		},
	}
	cmd.Flags().BoolVar(&deep, "deep", false, "prove the write permissions with test objects")
	addYesFlag(cmd, &yes)
	return cmd
}

func (a *app) credentialRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a credential, when nothing is left that it manages",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			if err := a.client().RemoveCredential(cmd.Context(), args[0]); err != nil {
				return a.explain(cmd.Context(), err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			s.printf("Removed credential %s.\n", args[0])
			return s.done()
		},
	}
}

// printCredential shows a credential as JSON, or as the checklist of its last
// check followed by closing, which may be empty.
func (a *app) printCredential(cmd *cobra.Command, v engine.CredentialView, closing string) error {
	out := cmd.OutOrStdout()
	if a.json {
		return printCredentialJSON(out, v)
	}
	s := &screen{w: out}
	a.renderChecklist(s, v)
	if closing != "" {
		s.printf("\n%s\n", closing)
	}
	return s.done()
}

// renderChecklist writes the outcome of a check: what the token is, what it
// sees, a line for each capability that was tried with what to grant when it
// failed, and whether the token can be used.
func (a *app) renderChecklist(s *screen, v engine.CredentialView) {
	r := v.Report
	title := fmt.Sprintf("Credential %s", dash(v.Label))
	if v.ID != "" {
		title += " (" + v.ID + ")"
	}
	s.println(title)
	if r.Token.Status != "" {
		s.printf("Token:     %s\n", a.tokenText(r))
	}
	s.printf("Accounts:  %s\n", accountNames(r))
	s.printf("Zones:     %s\n\n", zoneNames(r))

	for _, c := range r.Checks {
		mark := "✗"
		if c.OK {
			mark = "✓"
		}
		s.printf("  %s %s\n", mark, checkName(c))
		if !c.OK && c.Detail != "" {
			s.printf("      %s\n", c.Detail)
		}
	}
	if len(r.Checks) > 0 {
		s.println("")
	}
	if r.Usable {
		s.println("Usable:    yes")
	} else {
		s.println("Usable:    no")
	}
	if !r.Deep && v.ID != "" {
		s.printf("Write access was not tried. Run pco credential check %s --deep to prove it.\n", v.ID)
	}
	if len(r.Leftovers) > 0 {
		s.printf("Probe records left by an earlier check, to be removed by hand: %s\n", strings.Join(r.Leftovers, ", "))
	}
}

func checkName(c credentials.Check) string {
	if c.Scope == "" {
		return string(c.Capability)
	}
	return string(c.Capability) + " on " + c.Scope
}

func (a *app) tokenText(r credentials.Report) string {
	text := r.Token.Status
	if r.Token.ExpiresOn != nil {
		text += ", expires " + a.when(*r.Token.ExpiresOn)
	}
	return text
}

func accountNames(r credentials.Report) string {
	names := make([]string, len(r.Accounts))
	for i, acc := range r.Accounts {
		names[i] = acc.Name
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

func zoneNames(r credentials.Report) string {
	names := make([]string, len(r.Zones))
	for i, z := range r.Zones {
		names[i] = fmt.Sprintf("%s (%s)", z.Name, z.Status)
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// credentialState is "usable" for a credential whose last check passed,
// "problem" for one that failed it, and "unknown" for one that was never
// checked.
func credentialState(v engine.CredentialView) string {
	switch {
	case !v.Checked:
		return "unknown"
	case v.Report.Usable:
		return "usable"
	}
	return "problem"
}

// credentialNote says what is wrong with a credential, if its last check found
// something, and warns when its token is about to expire.
func (a *app) credentialNote(v engine.CredentialView) string {
	if !v.Checked {
		return ""
	}
	var notes []string
	if !v.Report.Usable {
		if i := firstFailed(v.Report); i >= 0 {
			c := v.Report.Checks[i]
			note := checkName(c)
			if c.Detail != "" {
				note += ": " + c.Detail
			}
			notes = append(notes, note)
		}
	}
	if note := a.expiryNote(v.Report.Token.ExpiresOn); note != "" {
		notes = append(notes, note)
	}
	return strings.Join(notes, "; ")
}

func firstFailed(r credentials.Report) int {
	for i, c := range r.Checks {
		if !c.OK {
			return i
		}
	}
	return -1
}

// expiryNote warns of a token that has expired or does within the warning
// time of the engine, which the doctor uses too.
func (a *app) expiryNote(expires *time.Time) string {
	if expires == nil {
		return ""
	}
	left := expires.Sub(a.now())
	switch {
	case left <= 0:
		return "token expired " + a.when(*expires)
	case left >= engine.ExpiryWarning:
		return ""
	case left < 24*time.Hour:
		return "token expires " + a.when(*expires) + " (in less than a day)"
	}
	days := int(left / (24 * time.Hour))
	if days == 1 {
		return "token expires " + a.when(*expires) + " (in 1 day)"
	}
	return fmt.Sprintf("token expires %s (in %d days)", a.when(*expires), days)
}
