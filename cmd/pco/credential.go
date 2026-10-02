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
	// expiryWarning is how long before its expiry a token is pointed out.
	expiryWarning = 30 * 24 * time.Hour
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
	if len(views) == 0 {
		_, err := fmt.Fprintln(w, "No credentials.")
		return err
	}
	t := newTable(w)
	_, _ = fmt.Fprintln(t, "ID\tLABEL\tKIND\tSTATE\tNOTE")
	for _, v := range views {
		_, _ = fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", v.ID, dash(v.Label), dash(v.Kind), credentialState(v), dash(a.credentialNote(v)))
	}
	return t.Flush()
}

func (a *app) credentialAddCmd() *cobra.Command {
	var label, tokenFile string
	cmd := &cobra.Command{
		Use:   "add --label <label> [--token-file <file>]",
		Short: "Check a Cloudflare API token and store it",
		Long: "Check what a Cloudflare API token can do and store it when the check passes. The\n" +
			"token is read from the file, or from standard input; on a terminal it is asked for\n" +
			"without being shown. It is never taken from an argument, which others could read.",
		Args: cobra.NoArgs,
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
		raw, err = readLimited(file)
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
	if _, err := fmt.Fprint(cmd.ErrOrStderr(), tokenPrompt); err != nil {
		return nil, err
	}
	raw, err := a.readPassword(fd)
	// The newline the admin typed was not echoed.
	_, _ = fmt.Fprintln(cmd.ErrOrStderr())
	return raw, err
}

// readLimited reads a file of at most maxTokenInput bytes.
func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxTokenInput+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(raw) > maxTokenInput {
		return nil, fmt.Errorf("%s is too long for a token", path)
	}
	return raw, nil
}

func (a *app) credentialCheckCmd() *cobra.Command {
	var deep, yes bool
	cmd := &cobra.Command{
		Use:   "check <id>",
		Short: "Check what the token of a credential can do",
		Long: "Check what the token of a stored credential can do. A deep check proves the write\n" +
			"permissions by creating and deleting a test DNS record and a test tunnel.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if deep {
				ok, err := confirm(cmd, yes, "A deep check creates and deletes a test DNS record and a test tunnel. Continue? [y/N]")
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
			if err := a.client().RemoveCredential(cmd.Context(), args[0]); err != nil {
				return a.explain(cmd.Context(), err)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "Removed credential %s.\n", args[0])
			return err
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
	if err := a.renderChecklist(out, v); err != nil {
		return err
	}
	if closing != "" {
		_, err := fmt.Fprintf(out, "\n%s\n", closing)
		return err
	}
	return nil
}

// renderChecklist writes the outcome of a check: what the token is, what it
// sees, a line for each capability that was tried with what to grant when it
// failed, and whether the token can be used.
func (a *app) renderChecklist(w io.Writer, v engine.CredentialView) error {
	r := v.Report
	title := fmt.Sprintf("Credential %s", dash(v.Label))
	if v.ID != "" {
		title += " (" + v.ID + ")"
	}
	_, _ = fmt.Fprintln(w, title)
	if r.Token.Status != "" {
		_, _ = fmt.Fprintf(w, "Token:     %s\n", a.tokenText(r))
	}
	_, _ = fmt.Fprintf(w, "Accounts:  %s\n", accountNames(r))
	_, _ = fmt.Fprintf(w, "Zones:     %s\n\n", zoneNames(r))

	for _, c := range r.Checks {
		mark := "✗"
		if c.OK {
			mark = "✓"
		}
		_, _ = fmt.Fprintf(w, "  %s %s\n", mark, checkName(c))
		if !c.OK && c.Detail != "" {
			_, _ = fmt.Fprintf(w, "      %s\n", c.Detail)
		}
	}
	if len(r.Checks) > 0 {
		_, _ = fmt.Fprintln(w)
	}
	if r.Usable {
		_, _ = fmt.Fprintln(w, "Usable:    yes")
	} else {
		_, _ = fmt.Fprintln(w, "Usable:    no")
	}
	if !r.Deep && v.ID != "" {
		_, _ = fmt.Fprintf(w, "Write access was not tried. Run pco credential check %s --deep to prove it.\n", v.ID)
	}
	if len(r.Leftovers) > 0 {
		_, _ = fmt.Fprintf(w, "Probe records left by an earlier check, to be removed by hand: %s\n", strings.Join(r.Leftovers, ", "))
	}
	return nil
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
// "problem" for one that failed it, and "unknown" for one that was not checked
// since the daemon started.
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

// expiryNote warns of a token that has expired or does within expiryWarning.
func (a *app) expiryNote(expires *time.Time) string {
	if expires == nil {
		return ""
	}
	left := expires.Sub(a.now())
	switch {
	case left <= 0:
		return "token expired " + a.when(*expires)
	case left >= expiryWarning:
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
