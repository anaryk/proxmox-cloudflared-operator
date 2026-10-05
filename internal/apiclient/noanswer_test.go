package apiclient

import (
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A command tells a daemon it could not ask from one that answered: the
// errors of the first kind are ErrNoAnswer.
func TestAnErrorWithoutAnAnswerOfTheDaemonIsNoAnswer(t *testing.T) {
	for _, tt := range []struct {
		name     string
		socket   func(t *testing.T) string
		noAnswer bool
	}{
		{"no daemon", func(t *testing.T) string { return filepath.Join(shortDir(t), "nope.sock") }, true},
		{"a socket that refuses the peer", func(t *testing.T) string {
			_, s := fakeDaemon(t, reply(http.StatusForbidden, `{"error":"not allowed","code":"forbidden"}`))
			return s
		}, true},
		{"a daemon that gave up waiting", func(t *testing.T) string {
			_, s := fakeDaemon(t, reply(http.StatusServiceUnavailable,
				`{"error":"a cycle is running and took longer than 45s; try again","code":"unavailable"}`))
			return s
		}, true},
		{"an answer that is not JSON", func(t *testing.T) string {
			_, s := fakeDaemon(t, reply(http.StatusOK, `not json`))
			return s
		}, true},
		{"a daemon of another version", func(t *testing.T) string {
			_, s := fakeDaemon(t, reply(http.StatusNotFound, `{"error":"no such route","code":"no_route"}`))
			return s
		}, true},
		{"a refused request", func(t *testing.T) string {
			_, s := fakeDaemon(t, reply(http.StatusConflict, `{"error":"refused: what waits changed","code":"refused"}`))
			return s
		}, false},
		{"an invalid request", func(t *testing.T) string {
			_, s := fakeDaemon(t, reply(http.StatusBadRequest, `{"error":"invalid request: no","code":"invalid"}`))
			return s
		}, false},
		{"an internal error", func(t *testing.T) string {
			_, s := fakeDaemon(t, reply(http.StatusInternalServerError, `{"error":"disk gone","code":"internal"}`))
			return s
		}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.socket(t)).Status(t.Context())

			require.Error(t, err)
			require.Equal(t, tt.noAnswer, errors.Is(err, ErrNoAnswer), err.Error())
		})
	}
}

func TestADaemonThatGaveUpWaitingSaysWhy(t *testing.T) {
	_, socket := fakeDaemon(t, reply(http.StatusServiceUnavailable,
		`{"error":"a cycle is running and took longer than 45s; try again","code":"unavailable"}`))

	_, err := New(socket).Apply(t.Context(), false, "")

	require.EqualError(t, err, "a cycle is running and took longer than 45s; try again")
}

func TestAStuckDaemonSaysHowLongItWasWaitedFor(t *testing.T) {
	release := make(chan struct{})
	_, socket := fakeDaemon(t, func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(func() { close(release) })
	c := New(socket)
	c.short = 20 * time.Millisecond

	_, err := c.Version(t.Context())

	require.EqualError(t, err, "the pco daemon at "+socket+" did not answer within 20ms")
	require.ErrorIs(t, err, ErrNoAnswer)
}

func TestEventsRawKeepsTheBytesTheDaemonSent(t *testing.T) {
	const body = `[{"seq":1,"level":"info","kind":"admin","subject":"","message":"x"}]`
	d, socket := fakeDaemon(t, reply(http.StatusOK, body))

	raw, err := New(socket).EventsRaw(t.Context(), time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))

	require.NoError(t, err)
	require.Equal(t, body, string(raw))
	require.Equal(t, "/v1/events", d.last(t).Path)
	require.Equal(t, "since=2026-10-01T12%3A00%3A00Z", d.last(t).Query)
}

// Only a daemon that is not there is not running: one that did not answer, or
// whose socket refused the caller, may well be.
func TestOnlyNothingBehindTheSocketIsNotRunning(t *testing.T) {
	missing := filepath.Join(shortDir(t), "nope.sock")
	stale := filepath.Join(shortDir(t), "stale.sock")
	ln, err := net.Listen("unix", stale)
	require.NoError(t, err)
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, ln.Close())

	for _, tt := range []struct {
		name       string
		socket     func(t *testing.T) string
		notRunning bool
	}{
		{"no socket", func(*testing.T) string { return missing }, true},
		{"nothing listens on it", func(*testing.T) string { return stale }, true},
		{"no directory", func(*testing.T) string { return filepath.Join(missing, "deeper.sock") }, true},
		{"a socket that refuses the peer", func(t *testing.T) string {
			_, s := fakeDaemon(t, reply(http.StatusForbidden, `{"error":"not allowed","code":"forbidden"}`))
			return s
		}, false},
		{"a daemon that gave up waiting", func(t *testing.T) string {
			_, s := fakeDaemon(t, reply(http.StatusServiceUnavailable, `{"error":"a cycle took too long","code":"unavailable"}`))
			return s
		}, false},
		{"a daemon of another version", func(t *testing.T) string {
			_, s := fakeDaemon(t, reply(http.StatusNotFound, `{"error":"no such route","code":"no_route"}`))
			return s
		}, false},
		{"a refused request", func(t *testing.T) string {
			_, s := fakeDaemon(t, reply(http.StatusConflict, `{"error":"refused: no","code":"refused"}`))
			return s
		}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.socket(t)).Status(t.Context())

			require.Error(t, err)
			require.Equal(t, tt.notRunning, NotRunning(err), err.Error())
		})
	}
	require.False(t, NotRunning(nil))
	require.False(t, NotRunning(errors.New("cannot reach the pco daemon")), "an error that is not the client's")
}
