package engine

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNoteWebIsAnEvent(t *testing.T) {
	e := published(t)
	before := e.eng.Events(t0.Add(-1))

	e.eng.NoteWeb("web certificate renewed: it does not name 192.0.2.11, fingerprint AB:CD")

	events := e.eng.Events(t0.Add(-1))
	require.Len(t, events, len(before)+1)
	last := events[len(events)-1]
	require.Equal(t, "web", last.Kind)
	require.Equal(t, "info", last.Level)
	require.Equal(t, "web certificate", last.Subject)
	require.Equal(t, "web certificate renewed: it does not name 192.0.2.11, fingerprint AB:CD", last.Message)
}

func TestNoteWebListenIsAWarning(t *testing.T) {
	e := published(t)

	e.eng.NoteWebListen("net0's address changed from 192.0.2.150 to 192.0.2.160: pco-web listens there now")

	events := e.eng.Events(t0.Add(-1))
	last := events[len(events)-1]
	require.Equal(t, "web", last.Kind)
	require.Equal(t, "warn", last.Level)
	require.Equal(t, "web listen", last.Subject)
}
