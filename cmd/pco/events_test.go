package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func someEvents() []engine.Event {
	return []engine.Event{
		{Seq: 1, At: t0.Add(-2 * time.Hour), Level: "info", Kind: "admin", Message: "observe-only mode ended; changes are applied from now on"},
		{Seq: 2, At: t0.Add(-time.Hour), Level: "info", Kind: "route", Subject: "www.example.com", Message: "qemu/101: active",
			Route: "www.example.com", Guest: "qemu/101", Account: "acc1"},
		{Seq: 3, At: t0.Add(-time.Minute), Level: "warn", Kind: "conflict", Subject: "evil\x1b[31m.example.com",
			Message: "A 192.0.2.1 in zone example.com is not ours; the hostname is not published\u202e"},
	}
}

func daemonWithEvents(t *testing.T, events []engine.Event) (*runner, *fakeEngine) {
	t.Helper()
	e := &fakeEngine{state: healthyState(), events: events}
	return newRunner(t, serveFake(t, e)), e
}

func TestEventsGolden(t *testing.T) {
	r, _ := daemonWithEvents(t, someEvents())

	res := r.run("", "events")

	require.NoError(t, res.err)
	require.Empty(t, res.errOut)
	requireGolden(t, "events.golden", res.out)
}

func TestEventsWithNone(t *testing.T) {
	r, _ := daemonWithEvents(t, nil)

	res := r.run("", "events")

	require.NoError(t, res.err)
	require.Equal(t, "No events.\n", res.out)
}

func TestEventsJSONIsWhatTheDaemonSent(t *testing.T) {
	r, _ := daemonWithEvents(t, someEvents()[2:])

	res := r.run("", "events", "--json")

	require.NoError(t, res.err)
	require.Contains(t, res.out, `"subject": "evil\u001b[31m.example.com"`)
	require.Contains(t, res.out, `"seq": 3`)
}

func TestEventsSince(t *testing.T) {
	for _, tt := range []struct {
		name  string
		args  []string
		since time.Time
	}{
		{"every event", nil, time.Time{}},
		{"a duration", []string{"--since", "10m"}, t0.Add(-10 * time.Minute)},
		{"a time in RFC 3339 format", []string{"--since", "2026-10-01T13:30:00+02:00"}, time.Date(2026, 10, 1, 11, 30, 0, 0, time.UTC)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWithEvents(t, someEvents())

			res := r.run("", append([]string{"events"}, tt.args...)...)

			require.NoError(t, res.err)
			require.True(t, tt.since.Equal(e.lastSince()), "asked since %s", e.lastSince())
		})
	}
}

func TestEventsRefusesASinceItCannotRead(t *testing.T) {
	r, e := daemonWithEvents(t, someEvents())

	for _, bad := range []string{"yesterday", "-5m", "0s", "2026-10-01"} {
		res := r.run("", "events", "--since", bad)

		require.EqualError(t, res.err, `--since "`+bad+`": want a duration such as 10m or a time in RFC 3339 format such as 2026-10-01T12:00:00Z`)
	}
	require.Empty(t, e.called())
}
