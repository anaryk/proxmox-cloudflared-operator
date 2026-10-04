package connector

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// What cloudflared logs when it cannot listen on its metrics address, before
// it exits, and systemd starts it again.
const (
	logPortHeld   = `2026-10-03T12:00:00Z ERR Error opening metrics server listener error="listen tcp 127.0.0.1:20300: bind: address already in use"`
	logPortHeldBy = `Error opening metrics server listener: listen tcp 127.0.0.1:20300: bind: address already in use`
	logOtherPort  = `2026-10-03T12:00:00Z ERR Error opening metrics server listener error="listen tcp 127.0.0.1:20301: bind: address already in use"`
)

// A local user who listens on the metrics port of a connector before it
// starts keeps it from starting: it exits, and systemd starts it again, for
// ever. The journal says so; the next Ensure moves it to another port.
func TestAMetricsPortHeldByAnotherProcessIsReportedAndMovedOff(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-a"))
	require.Equal(t, 20300, portOf(t, dir, idA))
	m.httpc = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.Canceled })}
	journalOf(t, m, []string{logStarting, logPortHeld, logPortHeldBy}, nil)

	st, err := m.Status(t.Context(), idA)

	require.NoError(t, err)
	require.True(t, st.MetricsPortHeld)
	require.False(t, st.TokenRefused)
	sd.reset()

	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-a"))

	require.Equal(t, 20301, portOf(t, dir, idA))
	require.Equal(t, []string{"Restart " + unitA}, sd.changes(), "it reads the env file only when it starts")

	require.NoError(t, m.Ensure(t.Context(), testInstall, idB, "token-b"))
	require.Equal(t, 20302, portOf(t, dir, idB), "the port held is not given to another tunnel")
}

func TestOnlyABindFailureOnItsOwnPortIsAPortHeld(t *testing.T) {
	for _, tt := range []struct {
		name  string
		lines []string
		want  bool
	}{
		{"its own port", []string{logStarting, logPortHeld}, true},
		{"another port", []string{logStarting, logOtherPort}, false},
		{"connected since", []string{logPortHeld, logStarting, logRegistered}, false},
		{"nothing", []string{logStarting, logRetrying}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newStatusFixture(t, answer(http.StatusServiceUnavailable, ``))
			writeFile(t, f.dir, idA+".env", "METRICS_ADDR=127.0.0.1:20300\n")
			f.m.httpc = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.Canceled })}
			journalOf(t, f.m, tt.lines, nil)

			st, err := f.m.Status(t.Context(), idA)

			require.NoError(t, err)
			require.Equal(t, tt.want, st.MetricsPortHeld)
		})
	}
}

// A port that is not held keeps its connector where it is.
func TestEnsureKeepsAPortNobodyElseHolds(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-a"))
	m.httpc = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.Canceled })}
	journalOf(t, m, []string{logStarting, logOtherPort}, nil)
	_, err := m.Status(t.Context(), idA)
	require.NoError(t, err)
	sd.reset()

	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-a"))

	require.Equal(t, 20300, portOf(t, dir, idA))
	require.Empty(t, sd.changes())
}
