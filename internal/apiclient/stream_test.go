package apiclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const hello = "event: hello\ndata: {\"boot\":\"9f2c4e1a0b7d3c55\",\"version\":\"1.3.0\",\"seq\":812,\"digest\":\"5e0c1f7a92b4d3e8\",\"pollInterval\":\"10s\"}\n\n"

// streams answers with the text of a stream, and seen gets the request.
func streams(text string, seen chan<- http.Header) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			seen <- r.Header.Clone()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, text)
	}
}

// all reads the notices until the stream ends.
func all(t *testing.T, ch <-chan engine.Notice) []engine.Notice {
	t.Helper()
	var out []engine.Notice
	for {
		select {
		case n, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, n)
		case <-time.After(10 * time.Second):
			t.Fatal("the stream did not end")
		}
	}
}

func TestStreamReadsTheHelloAndTheNotices(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	text := ": a comment first\n\n" + hello +
		"event: state\r\ndata: {\"at\":\"2026-10-01T12:00:00Z\",\r\ndata: \"finishedAt\":\"2026-10-01T12:00:02Z\",\"digest\":\"77a1c0de5f3b2a90\"}\r\n\r\n" +
		": ping\n\n" +
		"id: 9f2c4e1a0b7d3c55:813\nevent: event\ndata: {\"seq\":813,\"boot\":\"9f2c4e1a0b7d3c55\",\"at\":\"2026-10-01T12:00:00Z\",\"level\":\"warn\",\"kind\":\"route\",\"subject\":\"a.example.com\",\"message\":\"down\"}\n\n" +
		"event: upstream\ndata: {\"up\":false}\n\n" +
		"id: 9f2c4e1a0b7d3c55:900\nevent: gap\ndata:{\"boot\":\"9f2c4e1a0b7d3c55\",\"from\":814,\"to\":900,\"count\":87,\"level\":\"error\"}\n\n" +
		"event: traffic\ndata: {\"at\":\"2026-10-01T12:00:05Z\"}\n\n" +
		"event: reset\ndata: {\"reason\":\"boot changed\"}\n\n"
	seen := make(chan http.Header, 1)
	_, socket := fakeDaemon(t, streams(text, seen))

	ch, got, err := New(socket).Stream(t.Context(), "", 0)

	require.NoError(t, err)
	require.Equal(t, engine.Hello{Boot: "9f2c4e1a0b7d3c55", Version: "1.3.0", Seq: 812, Digest: "5e0c1f7a92b4d3e8", PollInterval: "10s"}, got)
	require.Equal(t, []engine.Notice{
		{Kind: engine.NoticeState, State: &engine.StateNotice{At: at, FinishedAt: at.Add(2 * time.Second), Digest: "77a1c0de5f3b2a90"}},
		{Kind: engine.NoticeEvent, Event: &engine.Event{Seq: 813, Boot: "9f2c4e1a0b7d3c55", At: at, Level: "warn", Kind: "route", Subject: "a.example.com", Message: "down"}},
		{Kind: engine.NoticeGap, Gap: &engine.GapNotice{Boot: "9f2c4e1a0b7d3c55", From: 814, To: 900, Count: 87, Level: "error"}},
		{Kind: engine.NoticeTraffic, Traffic: &engine.TrafficNotice{At: at.Add(5 * time.Second)}},
		{Kind: engine.NoticeReset, Reason: "boot changed"},
	}, all(t, ch), "with CRLF, data over two lines, comments and a kind it does not know")
	header := <-seen
	require.Equal(t, "text/event-stream", header.Get("Accept"))
	require.Empty(t, header.Values("Last-Event-ID"))
}

func TestStreamSendsTheLastEventID(t *testing.T) {
	seen := make(chan http.Header, 1)
	_, socket := fakeDaemon(t, streams(hello, seen))

	ch, _, err := New(socket).Stream(t.Context(), "9f2c4e1a0b7d3c55", 812)
	require.NoError(t, err)
	all(t, ch)

	require.Equal(t, "9f2c4e1a0b7d3c55:812", (<-seen).Get("Last-Event-ID"))
}

func TestStreamFailures(t *testing.T) {
	for _, tt := range []struct {
		name     string
		handler  http.HandlerFunc
		contains string
		noAnswer bool
	}{
		{"no hello first", streams("event: state\ndata: {}\n\n", nil), "did not begin with its hello", true},
		{"an empty stream", streams("", nil), "did not begin with its hello", true},
		{"a broken hello", streams("event: hello\ndata: {\n\n", nil), "did not begin with its hello", true},
		{"not a stream", reply(200, `{}`), "is not a stream", true},
		{"busy", reply(503, `{"error":"16 streams are open already; try again later","code":"unavailable"}`), "16 streams are open already", true},
		{"an old daemon", reply(404, `{"error":"no such route","code":"no_route"}`), "does not know this request", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, socket := fakeDaemon(t, tt.handler)

			_, _, err := New(socket).Stream(t.Context(), "", 0)

			require.ErrorContains(t, err, tt.contains)
			require.Equal(t, tt.noAnswer, errorIsNoAnswer(err))
		})
	}
}

func TestAStreamEndsOnABrokenNotice(t *testing.T) {
	_, socket := fakeDaemon(t, streams(hello+"event: event\ndata: {\"seq\":\n\nevent: reset\ndata: {\"reason\":\"x\"}\n\n", nil))

	ch, _, err := New(socket).Stream(t.Context(), "", 0)

	require.NoError(t, err)
	require.Empty(t, all(t, ch), "what follows a notice that cannot be read is not trusted")
}

func TestCommentsDoNotAddUpToATooLargeMessage(t *testing.T) {
	comment := ": " + strings.Repeat("x", 1022) + "\n\n"
	text := hello + strings.Repeat(comment, maxMessage/len(comment)+10) + "event: reset\ndata: {\"reason\":\"boot changed\"}\n\n"
	_, socket := fakeDaemon(t, streams(text, nil))

	ch, _, err := New(socket).Stream(t.Context(), "", 0)

	require.NoError(t, err)
	require.Equal(t, []engine.Notice{{Kind: engine.NoticeReset, Reason: "boot changed"}}, all(t, ch),
		"more comments than one message may hold, each a message of its own")
}

func TestAStreamWaitsForTheHelloOnlySoLong(t *testing.T) {
	release := make(chan struct{})
	_, socket := fakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	})
	t.Cleanup(func() { close(release) }) // before the fake daemon waits for its handler
	c := New(socket)
	c.short = 50 * time.Millisecond

	_, _, err := c.Stream(t.Context(), "", 0)

	require.ErrorContains(t, err, "did not answer within 50ms")
	require.True(t, errorIsNoAnswer(err))
}

func TestAStreamEndsWithItsContext(t *testing.T) {
	release := make(chan struct{})
	_, socket := fakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, hello)
		w.(http.Flusher).Flush()
		<-release
	})
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithCancel(t.Context())

	ch, _, err := New(socket).Stream(ctx, "", 0)
	require.NoError(t, err)
	cancel()

	require.Empty(t, all(t, ch))
}

func errorIsNoAnswer(err error) bool { return errors.Is(err, ErrNoAnswer) }
