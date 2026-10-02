package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// These tests stop before setup reaches the node: none of them may open the
// store of the machine that runs them, nor run a command on it.

// commandWith is an app on a machine whose stdin is in, and a command that
// reads it and writes to out and errOut.
func commandWith(in io.Reader, terminal bool) (*app, *cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	e := testEnv()
	e.stdinTerminal = func(io.Reader) (int, bool) { return 7, terminal }
	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetIn(in)
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	return &app{env: e}, cmd, &out, &errOut
}

func TestSetupOptionsFromFlags(t *testing.T) {
	no := false
	for _, tt := range []struct {
		name  string
		flags setupFlags
		want  setup.Options
	}{
		{"none", setupFlags{}, setup.Options{}},
		{"declined", setupFlags{yes: true, noTags: true, skipCloudflared: true},
			setup.Options{Yes: true, RegisterTags: &no, InstallCloudflared: &no}},
		{"repair", setupFlags{repair: true}, setup.Options{Repair: true}},
		{"recover", setupFlags{recover: true, installID: "0123456789ab"}, setup.Options{Recover: true, InstallID: "0123456789ab"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, cmd, _, _ := commandWith(unreadable{t}, false)

			o, err := a.setupOptions(cmd, tt.flags)

			require.NoError(t, err)
			require.Equal(t, tt.want, o)
		})
	}
}

func TestSetupReadsTheTokenFromAFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), mode))
		require.NoError(t, os.Chmod(path, mode))
		return path
	}

	a, cmd, _, errOut := commandWith(unreadable{t}, false)
	o, err := a.setupOptions(cmd, setupFlags{tokenFile: write("private", "token-from-file\n", 0o600)})
	require.NoError(t, err)
	require.Equal(t, "token-from-file", o.CloudflareToken)
	require.Empty(t, errOut.String())

	loose := write("loose", "token-from-file\r\n", 0o644)
	_, err = a.setupOptions(cmd, setupFlags{tokenFile: loose})
	require.NoError(t, err)
	require.Equal(t, "warning: "+loose+" can be read by others; restrict it with chmod 600\n", errOut.String())

	_, err = a.setupOptions(cmd, setupFlags{tokenFile: write("empty", "\n", 0o600)})
	require.EqualError(t, err, "the Cloudflare token is empty")

	_, err = a.setupOptions(cmd, setupFlags{tokenFile: filepath.Join(dir, "missing")})
	require.ErrorContains(t, err, "reading the Cloudflare token")
}

func TestSetupReadsTheTokenFromStdin(t *testing.T) {
	a, cmd, _, _ := commandWith(strings.NewReader("token-from-stdin\n"), false)
	o, err := a.setupOptions(cmd, setupFlags{yes: true, tokenStdin: true})
	require.NoError(t, err)
	require.Equal(t, "token-from-stdin", o.CloudflareToken)

	a, cmd, _, _ = commandWith(unreadable{t}, true)
	_, err = a.setupOptions(cmd, setupFlags{tokenStdin: true})
	require.ErrorContains(t, err, "piped in", "a terminal is asked through the prompt, which hides the token")

	a, cmd, _, _ = commandWith(strings.NewReader(strings.Repeat("x", maxTokenInput+1)), false)
	_, err = a.setupOptions(cmd, setupFlags{tokenStdin: true})
	require.ErrorContains(t, err, "too long")

	a, cmd, _, _ = commandWith(unreadable{t}, false)
	o, err = a.setupOptions(cmd, setupFlags{yes: true})
	require.NoError(t, err)
	require.Empty(t, o.CloudflareToken, "stdin is not read without --cf-token-stdin")
}

func TestSetupAndUninstallNeedATerminalOrYes(t *testing.T) {
	r := newRunner(t, "/nonexistent/pco/pco.sock")
	for _, args := range [][]string{{"setup"}, {"setup", "--cf-token-stdin"}, {"uninstall"}, {"uninstall", "--purge-cloudflare"}} {
		res := r.runReader(unreadable{t}, args...)
		require.ErrorIs(t, res.err, errNoTerminal, args)
	}
}

func TestSetupTakesNoTokenAsAnArgument(t *testing.T) {
	r := newRunner(t, "/nonexistent/pco/pco.sock")

	res := r.run("", "setup", "--yes", "secret-typed-as-an-argument")

	require.ErrorContains(t, res.err, "takes no arguments")
	require.NotContains(t, res.err.Error(), "secret-typed-as-an-argument")
}

func TestSetupFlagsThatDoNotGoTogether(t *testing.T) {
	r := newRunner(t, "/nonexistent/pco/pco.sock")
	for _, args := range [][]string{
		{"setup", "--yes", "--repair", "--recover"},
		{"setup", "--yes", "--cf-token-file", "/nonexistent", "--cf-token-stdin"},
		{"setup", "--yes", "--json"},
		{"uninstall", "--yes", "--json"},
	} {
		res := r.runReader(unreadable{t}, args...)
		require.Error(t, res.err, args)
	}
}

func TestThePrompterOnATerminal(t *testing.T) {
	a, cmd, out, errOut := commandWith(strings.NewReader("maybe\ny\n\nno\n"), true)
	a.readPassword = func(fd int) ([]byte, error) {
		require.Equal(t, 7, fd, "the secret is read from the terminal")
		return []byte("typed-secret\n"), nil
	}
	p, err := a.setupPrompter(cmd, false)
	require.NoError(t, err)

	yes, err := p.Confirm("Register the gate tags?", false)
	require.NoError(t, err)
	require.True(t, yes, "an answer that is neither yes nor no is asked again")
	def, err := p.Confirm("Install cloudflared?", true)
	require.NoError(t, err)
	require.True(t, def, "an empty answer is the default")
	no, err := p.Confirm("Install cloudflared?", true)
	require.NoError(t, err)
	require.False(t, no)
	_, err = p.Confirm("Again?", true)
	require.ErrorContains(t, err, "input ended")

	secret, err := p.Secret("Cloudflare API token: ")
	require.NoError(t, err)
	require.Equal(t, "typed-secret", secret)

	p.Info("role %s: added %s", "PCO", "\x1b[2Jsneaky\u202e")
	p.Warn("%s is kept", "a\nb")

	require.Equal(t, "role PCO: added ?[2Jsneaky?\n", out.String(), "what others wrote is cleaned")
	require.Equal(t,
		"Register the gate tags? [y/N] Register the gate tags? [y/N] Install cloudflared? [Y/n] "+
			"Install cloudflared? [Y/n] Again? [Y/n] \nCloudflare API token: \nwarning: a?b is kept\n",
		errOut.String())
	require.NotContains(t, out.String()+errOut.String(), "typed-secret")
}

func TestThePrompterWithYesAsksNothing(t *testing.T) {
	a, cmd, _, errOut := commandWith(unreadable{t}, true)
	a.readPassword = func(int) ([]byte, error) { panic("no secret is read") }
	p, err := a.setupPrompter(cmd, true)
	require.NoError(t, err)

	yes, err := p.Confirm("Install cloudflared?", true)
	require.NoError(t, err)
	require.True(t, yes)
	no, err := p.Confirm("Delete at Cloudflare?", false)
	require.NoError(t, err)
	require.False(t, no)
	secret, err := p.Secret("Cloudflare API token: ")
	require.NoError(t, err)
	require.Empty(t, secret)
	require.Empty(t, errOut.String(), "nothing is asked")
}

func TestTheUninstallPrompter(t *testing.T) {
	for _, tt := range []struct {
		name                string
		terminal, yes       bool
		interactive, refuse bool
	}{
		{"a terminal asks what no flag answers, even with --yes", true, true, true, false},
		{"a terminal without --yes", true, false, true, false},
		{"--yes without a terminal", false, true, false, false},
		{"neither", false, false, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, cmd, _, _ := commandWith(unreadable{t}, tt.terminal)

			p, err := a.uninstallPrompter(cmd, tt.yes)

			if tt.refuse {
				require.ErrorIs(t, err, errNoTerminal)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.interactive, p.interactive)
		})
	}
}
