package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const cfToken = "cf-test-token-0123456789"

// withWrites is a credential whose deep check passed.
func withWrites(v engine.CredentialView) engine.CredentialView {
	v.Report.Deep = true
	v.Report.Checks = append(v.Report.Checks,
		credentials.Check{Capability: credentials.CapDNSWrite, Scope: "example.com", ScopeID: "zone1", OK: true},
		credentials.Check{Capability: credentials.CapTunnelWrite, Scope: "Main", ScopeID: "acc1", OK: true},
	)
	v.Report.Leftovers = []string{"_pco-probe-x.example.com"}
	return v
}

// leavingOut is a credential whose token also lists zones whose DNS it may not
// read.
func leavingOut(v engine.CredentialView, zones ...string) engine.CredentialView {
	for i, name := range zones {
		id := fmt.Sprintf("zone%d", i+2)
		v.Report.Zones = append(v.Report.Zones, cfapi.Zone{ID: id, Name: name, Status: "active", AccountID: "acc1"})
		v.Report.Excluded = append(v.Report.Excluded, credentials.Exclusion{
			Zone: name, ZoneID: id, Reason: "no DNS read", Detail: "grant Zone > DNS > Edit on " + name,
		})
	}
	return v
}

func credentialsState() engine.State {
	st := healthyState()
	st.Credentials = []engine.CredentialView{
		usableCredential("a1b2c3d4", "main", 12*24*time.Hour),
		{ID: "e5f6a7b8", Label: "spare", Kind: "scoped"},
		failingCredential("c9d0e1f2", "readonly"),
		leavingOut(usableCredential("f0e1d2c3", "narrow", 365*24*time.Hour), "example.net", "example.org"),
	}
	return st
}

func TestCredentialListGolden(t *testing.T) {
	r, _ := daemonWith(t, credentialsState())

	res := r.run("", "credential", "list")

	require.NoError(t, res.err)
	require.Empty(t, res.errOut)
	requireGolden(t, "credential_list.golden", res.out)
}

func TestCredentialListWithNone(t *testing.T) {
	r, _ := daemonWith(t, freshState())

	res := r.run("", "credential", "list")

	require.NoError(t, res.err)
	require.Equal(t, "No credentials.\n", res.out)
}

func TestCredentialListJSONIsThePayloadOfTheDaemon(t *testing.T) {
	const payload = `[{"label":"main","id":"abc12345","kind":"scoped","checked":false,"extra":{"b":1,"a":2}}]`
	r := newRunner(t, serveRaw(t, map[string]rawReply{"GET /v1/credentials": {200, payload}}))

	res := r.run("", "credential", "list", "--json")

	require.NoError(t, res.err)
	require.Equal(t, indented(t, payload), res.out)
}

func TestCredentialAddReadsTheTokenFromStandardInput(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   string
	}{
		{"with a newline", cfToken + "\n"},
		{"with a carriage return and a newline", cfToken + "\r\n"},
		{"without a newline", cfToken},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWith(t, freshState())
			e.addView = usableCredential("a1b2c3d4", "main", 12*24*time.Hour)

			res := r.run(tt.in, "credential", "add", "--label", "main")

			require.NoError(t, res.err)
			require.Equal(t, []string{"add main " + cfToken}, e.called(), "the token as typed, without the newline")
			require.Empty(t, res.errOut, "stdin is no terminal: no prompt")
			require.NotContains(t, res.out, cfToken)
		})
	}
}

func TestCredentialAddGolden(t *testing.T) {
	r, e := daemonWith(t, freshState())
	e.addView = usableCredential("a1b2c3d4", "main", 12*24*time.Hour)

	res := r.run(cfToken+"\n", "credential", "add", "--label", "main")

	require.NoError(t, res.err)
	requireGolden(t, "credential_add.golden", res.out)
}

func TestCredentialAddReadsTheTokenFromAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(file, []byte(cfToken+"\n"), 0o600))
	r, e := daemonWith(t, freshState())
	e.addView = usableCredential("a1b2c3d4", "main", 12*24*time.Hour)

	// Standard input is not read when there is a file.
	res := r.run("not-the-token-0123456789\n", "credential", "add", "--label", "main", "--token-file", file)

	require.NoError(t, res.err)
	require.Equal(t, []string{"add main " + cfToken}, e.called())
}

func TestCredentialAddWarnsOfATokenFileOthersCanRead(t *testing.T) {
	for _, tt := range []struct {
		mode os.FileMode
		warn bool
	}{
		{0o600, false},
		{0o400, false},
		{0o640, true},
		{0o604, true},
		{0o644, true},
	} {
		t.Run(tt.mode.String(), func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "token")
			require.NoError(t, os.WriteFile(file, []byte(cfToken+"\n"), 0o600))
			require.NoError(t, os.Chmod(file, tt.mode))
			r, e := daemonWith(t, freshState())
			e.addView = usableCredential("a1b2c3d4", "main", 12*24*time.Hour)

			res := r.run("", "credential", "add", "--label", "main", "--token-file", file)

			require.NoError(t, res.err, "a warning is no reason to refuse the token")
			require.Equal(t, []string{"add main " + cfToken}, e.called())
			if tt.warn {
				require.Equal(t, "warning: "+file+" can be read by others; restrict it with chmod 600\n", res.errOut)
			} else {
				require.Empty(t, res.errOut)
			}
		})
	}
}

func TestCredentialAddNeverEchoesAStrayArgument(t *testing.T) {
	r, e := daemonWith(t, freshState())

	// A token typed in the place of a flag value ends up as an argument.
	res := r.run("", "credential", "add", "--label", "main", cfToken)

	require.EqualError(t, res.err, `"pco credential add" takes no arguments: the token is read from standard input or --token-file`)
	require.NotContains(t, res.err.Error(), cfToken)
	require.Empty(t, e.called())
}

func TestCredentialAddNeverTakesTheTokenFromAFlag(t *testing.T) {
	r, e := daemonWith(t, freshState())

	res := r.run("", "credential", "add", "--label", "main", "--token", cfToken)

	require.ErrorContains(t, res.err, "unknown flag: --token")
	require.Empty(t, e.called())
}

func TestCredentialAddAsksForTheTokenOnATerminalWithoutEcho(t *testing.T) {
	r, e := daemonWith(t, freshState())
	e.addView = usableCredential("a1b2c3d4", "main", 12*24*time.Hour)
	var readFrom int
	r.env.stdinTerminal = func(io.Reader) (int, bool) { return 7, true }
	r.env.readPassword = func(fd int) ([]byte, error) {
		readFrom = fd
		return []byte(cfToken), nil
	}

	res := r.run("", "credential", "add", "--label", "main")

	require.NoError(t, res.err)
	require.Equal(t, 7, readFrom)
	require.Equal(t, "Cloudflare API token: \n", res.errOut, "the prompt, and the newline the admin typed that was not echoed")
	require.Equal(t, []string{"add main " + cfToken}, e.called())
	require.NotContains(t, res.out, cfToken)
}

func TestCredentialAddWhenTheTerminalCannotBeRead(t *testing.T) {
	r, e := daemonWith(t, freshState())
	r.env.stdinTerminal = func(io.Reader) (int, bool) { return 7, true }
	r.env.readPassword = func(int) ([]byte, error) { return nil, fmt.Errorf("inappropriate ioctl") }

	res := r.run("", "credential", "add", "--label", "main")

	require.ErrorContains(t, res.err, "reading the token: inappropriate ioctl")
	require.Empty(t, e.called())
}

func TestCredentialAddRefusesWhatCannotBeAToken(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   string
		args []string
		want string
	}{
		{"no label", cfToken, nil, "--label is required"},
		{"a blank label", cfToken, []string{"--label", "  "}, "--label is required"},
		{"no token", "", []string{"--label", "main"}, "the token is empty"},
		{"only a newline", "\n", []string{"--label", "main"}, "the token is empty"},
		{"a file that is not there", "", []string{"--label", "main", "--token-file", "/nonexistent/token"}, "reading the token"},
		{"too long", strings.Repeat("a", maxTokenInput+1), []string{"--label", "main"}, "too long for a token"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWith(t, freshState())

			res := r.run(tt.in, append([]string{"credential", "add"}, tt.args...)...)

			require.ErrorContains(t, res.err, tt.want)
			require.Empty(t, e.called())
			require.NotContains(t, fmt.Sprint(res.err), cfToken)
		})
	}
}

func TestCredentialAddPrintsTheChecklistOfARefusedTokenAndThenFails(t *testing.T) {
	r, e := daemonWith(t, freshState())
	e.addView = failingCredential("", "main")
	e.addErr = fmt.Errorf("%w: the token cannot be used: dns.read on example.com: grant Zone > DNS > Read on example.com", engine.ErrInvalid)

	res := r.run(cfToken+"\n", "credential", "add", "--label", "main")

	require.ErrorIs(t, res.err, engine.ErrInvalid)
	require.ErrorContains(t, res.err, "the token cannot be used")
	requireGolden(t, "credential_refused.golden", res.out)
	require.NotContains(t, res.out, cfToken)
	require.NotContains(t, res.err.Error(), cfToken)
}

func TestCredentialAddWithATokenThatIsRefusedBeforeACheck(t *testing.T) {
	// The daemon refuses what has not the shape of a token before it asks
	// Cloudflare: there is no report to show.
	r, e := daemonWith(t, freshState())

	res := r.run("short\n", "credential", "add", "--label", "main")

	require.ErrorContains(t, res.err, "the token is not a Cloudflare API token")
	require.Empty(t, res.out)
	require.Empty(t, e.called())
}

func TestCredentialAddJSON(t *testing.T) {
	r, e := daemonWith(t, freshState())
	e.addView = usableCredential("a1b2c3d4", "main", 12*24*time.Hour)

	res := r.run(cfToken, "credential", "add", "--label", "main", "--json")

	require.NoError(t, res.err)
	require.JSONEq(t, mustJSON(t, e.addView), res.out)
	require.True(t, strings.HasPrefix(res.out, "{\n  \"id\": \"a1b2c3d4\",\n"), res.out)
}

func TestCredentialCheckGolden(t *testing.T) {
	r, e := daemonWith(t, credentialsState())
	e.checkView = withWrites(usableCredential("a1b2c3d4", "main", 12*24*time.Hour))

	res := r.run("", "credential", "check", "a1b2c3d4", "--deep", "--yes")

	require.NoError(t, res.err)
	require.Empty(t, res.errOut)
	requireGolden(t, "credential_check_deep.golden", res.out)
	require.Equal(t, []string{"check a1b2c3d4 deep=true"}, e.called())
}

func TestCredentialCheckWithoutDeepSaysWriteAccessWasNotTried(t *testing.T) {
	r, e := daemonWith(t, credentialsState())
	e.checkView = failingCredential("c9d0e1f2", "readonly")

	res := r.run("", "credential", "check", "c9d0e1f2")

	require.NoError(t, res.err)
	require.Empty(t, res.errOut, "only a deep check asks")
	requireGolden(t, "credential_check.golden", res.out)
	require.Equal(t, []string{"check c9d0e1f2 deep=false"}, e.called())
}

func TestTheZonesATokenLeavesOutAreNoFailure(t *testing.T) {
	r, e := daemonWith(t, credentialsState())
	e.checkView = leavingOut(usableCredential("f0e1d2c3", "narrow", 365*24*time.Hour), "example.net", "example.org")

	res := r.run("", "credential", "check", "f0e1d2c3")

	require.NoError(t, res.err)
	requireGolden(t, "credential_check_excluded.golden", res.out)
	require.NotContains(t, res.out, "✗")
}

func TestAddingATokenThatLeavesZonesOutSaysSo(t *testing.T) {
	r, e := daemonWith(t, freshState())
	e.addView = leavingOut(usableCredential("a1b2c3d4", "main", 12*24*time.Hour), "example.org")

	res := r.run(cfToken+"\n", "credential", "add", "--label", "main")

	require.NoError(t, res.err)
	require.Contains(t, res.out, "  - example.org left out: no DNS read\n      grant Zone > DNS > Edit on example.org\n")
	require.Contains(t, res.out, "Usable:    yes")
	require.Contains(t, res.out, "Added credential a1b2c3d4 (main).")
}

func TestDeepCheckAsksFirst(t *testing.T) {
	const question = "A deep check creates and deletes a test DNS record and a test tunnel. Continue? [y/N] "
	for _, tt := range []struct {
		name  string
		in    string
		check bool
	}{
		{"yes", "y\n", true},
		{"no", "n\n", false},
		{"nothing", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWith(t, credentialsState())
			e.checkView = withWrites(usableCredential("a1b2c3d4", "main", 12*24*time.Hour))

			res := r.tty().run(tt.in, "credential", "check", "a1b2c3d4", "--deep")

			require.Contains(t, res.errOut, question)
			if tt.check {
				require.NoError(t, res.err)
				require.Equal(t, []string{"check a1b2c3d4 deep=true"}, e.called())
			} else {
				require.ErrorIs(t, res.err, errAborted)
				require.Empty(t, e.called())
				require.Empty(t, res.out)
			}
		})
	}
}

func TestCredentialCheckOfAnUnknownCredential(t *testing.T) {
	r, e := daemonWith(t, credentialsState())
	e.checkErr = fmt.Errorf("%w: no credential %q", engine.ErrNotFound, "nope")

	res := r.run("", "credential", "check", "nope")

	require.ErrorIs(t, res.err, engine.ErrNotFound)
}

func TestCredentialRemove(t *testing.T) {
	r, e := daemonWith(t, credentialsState())

	res := r.run("", "credential", "remove", "a1b2c3d4")

	require.NoError(t, res.err)
	require.Equal(t, "Removed credential a1b2c3d4.\n", res.out)
	require.Equal(t, []string{"remove a1b2c3d4"}, e.called())

	e.removeErr = fmt.Errorf("%w: credential a1b2c3d4 still manages record www.example.com in zone example.com", engine.ErrRefused)
	res = r.run("", "credential", "remove", "a1b2c3d4")
	require.ErrorIs(t, res.err, engine.ErrRefused)
	require.Empty(t, res.out)
}

func TestExpiryNotes(t *testing.T) {
	r, _ := daemonWith(t, freshState())
	a := &app{env: r.env}

	for _, tt := range []struct {
		name    string
		expires *time.Duration // from now
		want    string
	}{
		{"does not expire", nil, ""},
		{"far away", after(31 * 24 * time.Hour), ""},
		{"a month", after(30*24*time.Hour - time.Second), "token expires 2026-10-31T13:59:59+02:00 (in 29 days)"},
		{"a day and a bit", after(36 * time.Hour), "token expires 2026-10-03T02:00:00+02:00 (in 1 day)"},
		{"hours", after(5 * time.Hour), "token expires 2026-10-01T19:00:00+02:00 (in less than a day)"},
		{"just now", after(0), "token expired 2026-10-01T14:00:00+02:00"},
		{"long ago", after(-90 * 24 * time.Hour), "token expired 2026-07-03T14:00:00+02:00"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var at *time.Time
			if tt.expires != nil {
				v := t0.Add(*tt.expires)
				at = &v
			}
			require.Equal(t, tt.want, a.expiryNote(at))
		})
	}
}

func after(d time.Duration) *time.Duration { return &d }
