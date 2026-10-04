package connector

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// What cloudflared logs when the edge refuses its token, as after the secret
// of the tunnel was rotated, and when a connection comes up.
const (
	logStarting   = `2026-10-03T12:00:00Z INF Starting tunnel tunnelID=00000000-0000-4000-8000-000000000001`
	logRefused    = `2026-10-03T12:00:01Z ERR Register tunnel error from server side error="Unauthorized: Invalid tunnel secret" connIndex=0 event=0 ip=198.41.192.7`
	logRegistered = `2026-10-03T12:00:01Z INF Registered tunnel connection connIndex=0 connection=8c1b event=0 ip=198.41.192.7 location=prg01 protocol=quic`
	logRetrying   = `2026-10-03T12:00:02Z INF Retrying connection in up to 2s connIndex=0 event=0 ip=198.41.192.7`
)

// journalOf makes a manager read lines as the journal of every unit.
func journalOf(t *testing.T, m *Manager, lines []string, err error) *[]string {
	t.Helper()
	var asked []string
	m.journal = func(_ context.Context, unit string, n int) ([]string, error) {
		asked = append(asked, unit)
		require.Equal(t, journalLines, n)
		return lines, err
	}
	return &asked
}

func TestARefusedTokenIsTheLastWordOfTheJournal(t *testing.T) {
	for _, tt := range []struct {
		name  string
		lines []string
		want  bool
	}{
		{"refused", []string{logStarting, logRefused, logRetrying}, true},
		{"refused after a restart", []string{logRegistered, logStarting, logRefused}, true},
		{"registered since", []string{logRefused, logStarting, logRegistered}, false},
		{"nothing of either", []string{logStarting, logRetrying}, false},
		{"no lines", nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newStatusFixture(t, answer(http.StatusServiceUnavailable, `{"status":503,"readyConnections":0}`))
			asked := journalOf(t, f.m, tt.lines, nil)

			got, err := f.m.Status(t.Context(), idA)

			require.NoError(t, err)
			require.Equal(t, tt.want, got.TokenRefused)
			require.Equal(t, []string{unitA}, *asked)
		})
	}
}

// A connector that is ready or does not run has no journal read: it is read
// only for one that runs and is not connected.
func TestTheJournalIsReadOnlyForAConnectorThatIsNotReady(t *testing.T) {
	f := newStatusFixture(t, answer(http.StatusOK, `{"readyConnections":4,"connectorId":"c1"}`))
	asked := journalOf(t, f.m, []string{logRefused}, nil)

	got, err := f.m.Status(t.Context(), idA)
	require.NoError(t, err)
	require.False(t, got.TokenRefused)

	f.sd.active[unitA] = false
	got, err = f.m.Status(t.Context(), idA)
	require.NoError(t, err)
	require.False(t, got.TokenRefused)
	require.Empty(t, *asked)
}

func TestAJournalThatCannotBeReadSaysNothing(t *testing.T) {
	f := newStatusFixture(t, answer(http.StatusServiceUnavailable, ``))
	journalOf(t, f.m, []string{logRefused}, errors.New("journalctl: no journal"))

	got, err := f.m.Status(t.Context(), idA)

	require.NoError(t, err, "the status is there without it")
	require.False(t, got.TokenRefused)
}

func TestTheManagerReadsTheJournalThroughSystemctl(t *testing.T) {
	m, _, _ := newTestManager(t)
	require.Nil(t, m.journal, "the fake has no journal")

	m = NewManager(NewSystemctl(), t.TempDir(), nil, m.log)
	require.NotNil(t, m.journal)
}

// fakeJournalctl stands in for /usr/bin/journalctl: it logs its arguments and
// prints two lines, or fails for a unit named bad.service.
const fakeJournalctl = `#!/bin/sh
printf '%s\n' "$*" >> "$0.log"
for last; do :; done
if [ "$last" = --unit=bad.service ]; then
	echo "  no such journal  " >&2
	exit 1
fi
printf '%s\n' 'first line' 'second line'
`

func TestJournalctlArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journalctl")
	require.NoError(t, os.WriteFile(path, []byte(fakeJournalctl), 0o755))
	ctl := systemctl{journal: path}

	lines, err := ctl.Journal(t.Context(), "up.service", 50)
	require.NoError(t, err)
	require.Equal(t, []string{"first line", "second line"}, lines)

	_, err = ctl.Journal(t.Context(), "bad.service", 50)
	require.ErrorContains(t, err, "no such journal")

	b, err := os.ReadFile(path + ".log")
	require.NoError(t, err)
	require.Equal(t, "--quiet --no-pager --output=cat --lines=50 --unit=up.service\n"+
		"--quiet --no-pager --output=cat --lines=50 --unit=bad.service\n", string(b))
	require.False(t, strings.Contains(string(b), " -f"), "never follows")
}
