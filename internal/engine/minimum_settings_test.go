package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// belowTheMinimums rewrites the stored settings the way a build with lower
// minimums left them, which the store refuses to save now.
func belowTheMinimums(e *env, poll, grace string) string {
	e.t.Helper()
	e.settings(func(*store.Settings) {})
	path := filepath.Join(e.paths.Cluster, "meta", "settings.json")
	raw, err := os.ReadFile(path)
	require.NoError(e.t, err)
	raw = regexp.MustCompile(`"pollInterval": "[^"]*"`).ReplaceAll(raw, []byte(`"pollInterval": "`+poll+`"`))
	raw = regexp.MustCompile(`"grace": "[^"]*"`).ReplaceAll(raw, []byte(`"grace": "`+grace+`"`))
	require.NoError(e.t, os.WriteFile(path, raw, 0o600))
	return path
}

func TestSettingsBelowTheirMinimumsAreRaisedAndOnlyReported(t *testing.T) {
	e := newEnv(t)
	file := belowTheMinimums(e, "1s", "10s")

	st := e.cycle()

	require.True(t, hasProblem(st, "settings: pollInterval is 1s in "+file+", below the minimum of 5s; 5s is used until it is raised there"), "%v", st.Problems)
	require.True(t, hasProblem(st, "settings: grace is 10s in "+file+", below the minimum of 30s; 30s is used until it is raised there"), "%v", st.Problems)
	require.False(t, hasProblem(st, "reading the settings"), "%v", st.Problems)
	require.True(t, st.Complete, "the cycle runs on the raised values")
	require.NotEmpty(t, st.Routes)
	require.Equal(t, 5*time.Second, e.eng.PollInterval())
	require.Equal(t, 30*time.Second, e.eng.dnsSet.Grace)
}

func TestTheReportOfARaisedSettingGoesWhenTheFileIsFixed(t *testing.T) {
	e := newEnv(t)
	belowTheMinimums(e, "10s", "10s")
	require.True(t, hasProblem(e.cycle(), "settings: grace is 10s"))

	e.settings(func(s *store.Settings) { s.Grace = store.Duration(time.Minute) })

	require.False(t, hasProblem(e.cycle(), "settings: grace"))
}

func TestSettingsAtTheirMinimumsAreNotReported(t *testing.T) {
	e := newEnv(t)
	belowTheMinimums(e, "5s", "30s")

	st := e.cycle()

	require.False(t, hasProblem(st, "settings:"), "%v", st.Problems)
	require.Equal(t, 5*time.Second, e.eng.PollInterval())
}
