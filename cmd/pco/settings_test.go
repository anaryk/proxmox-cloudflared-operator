package main

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// someSettings is revision 7 of the settings, as a daemon shows them.
func someSettings() engine.SettingsView {
	s := store.DefaultSettings()
	s.ObserveOnly = false
	s.DenyHosts = []string{"admin.example.com", "*.internal.example.com"}
	s.TrustedCIDRs = []netip.Prefix{netip.MustParsePrefix("10.0.5.0/24")}
	s.ZonePins = map[string]string{"example.com": "a1b2c3d4"}
	s.ReverifyInterval = store.Duration(2 * time.Minute)
	limits := map[string]engine.Limit{}
	for name, l := range store.Limits() {
		limits[name] = engine.Limit{Min: l.Min, Max: l.Max}
	}
	return engine.SettingsView{
		Rev: 7, Settings: s, Limits: limits,
		ReadAtStart: []string{"cloudflareBudget", "gateTag", "trustStatic", "trustedCIDRs"},
		Notes:       []string{},
	}
}

func daemonWithSettings(t *testing.T) (*runner, *fakeEngine) {
	t.Helper()
	e := &fakeEngine{state: healthyState(), settings: someSettings()}
	return newRunner(t, serveFake(t, e)), e
}

func TestSettingsShowGolden(t *testing.T) {
	r, e := daemonWithSettings(t)

	res := r.run("", "settings", "show")

	require.NoError(t, res.err)
	require.Empty(t, res.errOut)
	requireGolden(t, "settings_show.golden", res.out)
	require.Empty(t, e.called())
}

func TestSettingsShowSaysWhatTheDaemonNoted(t *testing.T) {
	e := &fakeEngine{state: healthyState(), settings: someSettings()}
	e.settings.Notes = []string{"settings: pollInterval is 1s in /etc/pve/pco/meta/settings.json, below the minimum of 5s; 5s is used until it is raised there"}
	r := newRunner(t, serveFake(t, e))

	res := r.run("", "settings", "show")

	require.NoError(t, res.err)
	require.True(t, strings.HasSuffix(res.out, "\n\n"+e.settings.Notes[0]+"\n"), res.out)
}

func TestSettingsShowJSONIsWhatTheDaemonSent(t *testing.T) {
	const raw = `{"rev":7,"settings":{"gateTag":"cf-tunnel"},"readAtStart":[],"limits":{},"notes":[]}`
	r := newRunner(t, serveRaw(t, map[string]rawReply{"GET /v1/settings": {200, raw}}))

	res := r.run("", "--json", "settings", "show")

	require.NoError(t, res.err)
	require.Equal(t, indented(t, raw), res.out)
}

// settingsFile writes what settings show --json printed, with the settings
// edited, and returns its path.
func settingsFile(t *testing.T, r *runner, edit func(string) string) string {
	t.Helper()
	res := r.run("", "--json", "settings", "show")
	require.NoError(t, res.err)
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(edit(res.out)), 0o600))
	return path
}

func TestSettingsApplySendsTheRevisionTheyWereReadAt(t *testing.T) {
	r, e := daemonWithSettings(t)
	e.restartNeeded = []string{"gateTag", "cloudflareBudget"}
	path := settingsFile(t, r, func(s string) string {
		s = strings.Replace(s, `"gateTag": "cf-tunnel"`, `"gateTag": "publish"`, 1)
		return strings.Replace(s, `"cloudflareBudget": 1000`, `"cloudflareBudget": 600`, 1)
	})

	res := r.run("", "settings", "apply", path)

	require.NoError(t, res.err)
	require.Equal(t, []string{"save settings rev=7 gateTag=publish cloudflareBudget=600"}, e.called())
	requireGolden(t, "settings_apply.golden", res.out)

	res = r.run("", "--json", "settings", "apply", path)
	require.NoError(t, res.err)
	require.Contains(t, res.out, `"restartNeeded": [`)
	require.Contains(t, res.out, `"rev": 8`)
}

func TestSettingsApplyThatNeedsNoRestart(t *testing.T) {
	r, _ := daemonWithSettings(t)
	path := settingsFile(t, r, func(s string) string { return s })

	res := r.run("", "settings", "apply", path)

	require.NoError(t, res.err)
	require.Equal(t, "Saved the settings; they are at revision 8.\n", res.out)
}

func TestSettingsApplyRefusedByTheDaemon(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"a stale revision", fmt.Errorf("%w: the settings changed since they were read at revision 7 and are at revision 8 now; read them again", engine.ErrRefused),
			"the settings changed since they were read at revision 7 and are at revision 8 now; read them again"},
		{"a setting out of its range", &engine.FieldError{Field: "pollInterval", Err: errors.New("pollInterval 1s: at least 5s")}, "pollInterval 1s: at least 5s"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWithSettings(t)
			e.configErr = tt.err
			path := settingsFile(t, r, func(s string) string { return s })

			res := r.run("", "settings", "apply", path)

			require.Error(t, res.err)
			require.Contains(t, res.err.Error(), tt.want)
			require.Equal(t, 1, exitCode(res.err, &strings.Builder{}))
			require.Empty(t, res.out)
		})
	}
}

// A key the settings have no field for is refused by the daemon, which reads
// them as strictly as the store does: it is not dropped on the way.
func TestSettingsApplyPassesTheSettingsOnAsTheyAre(t *testing.T) {
	r, e := daemonWithSettings(t)
	path := settingsFile(t, r, func(s string) string {
		return strings.Replace(s, `"denyHosts": [`, `"denyhost": [`, 1)
	})

	res := r.run("", "settings", "apply", path)

	require.Error(t, res.err)
	require.Contains(t, res.err.Error(), `unknown field "denyhost"`)
	require.Empty(t, e.called())
}

func TestSettingsApplyRefusesAFileThatIsNotTheSettings(t *testing.T) {
	for _, tt := range []struct {
		name, content, want string
	}{
		{"not JSON", "gateTag: publish", "is not what pco settings show --json prints"},
		{"no revision", `{"settings":{"gateTag":"publish"}}`, "has no rev"},
		{"no settings", `{"rev":7}`, "has no settings"},
		{"a key of no answer", `{"rev":7,"setting":{"gateTag":"publish"}}`, `unknown field "setting"`},
		{"two documents", `{"rev":7,"settings":{}} {"rev":8}`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWithSettings(t)
			path := filepath.Join(t.TempDir(), "settings.json")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))

			res := r.run("", "settings", "apply", path)

			require.Error(t, res.err)
			require.Contains(t, res.err.Error(), tt.want)
			require.Empty(t, e.called())
		})
	}

	r, _ := daemonWithSettings(t)
	res := r.run("", "settings", "apply", filepath.Join(t.TempDir(), "missing.json"))
	require.ErrorIs(t, res.err, os.ErrNotExist)
}
