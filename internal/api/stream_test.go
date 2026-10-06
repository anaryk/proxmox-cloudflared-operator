package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// fakeTicker is the clock of the pings, which only a test moves.
type fakeTicker struct {
	mu    sync.Mutex
	every []time.Duration
	c     chan time.Time
}

func (f *fakeTicker) start(d time.Duration) (<-chan time.Time, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.every = append(f.every, d)
	return f.c, func() {}
}

func (f *fakeTicker) asked() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.every...)
}

// streamServer is a server of an engine whose stream the test feeds, with
// pings on fake time.
func streamServer() (*fakeEngine, *fakeTicker, *Server) {
	f := &fakeEngine{
		notices: make(chan engine.Notice),
		hello:   engine.Hello{Boot: testBoot, Seq: 812, Digest: "5e0c1f7a92b4d3e8", PollInterval: "10s"},
	}
	tick := &fakeTicker{c: make(chan time.Time)}
	s := newServer(f)
	s.ticker = tick.start
	return f, tick, s
}

// openStream asks for the stream on the socket, and returns the status and
// the header of the answer and a reader of its body.
func openStream(t *testing.T, socket, target string) (int, http.Header, *bufio.Reader) {
	t.Helper()
	var d net.Dialer
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return d.DialContext(ctx, "unix", socket)
	}}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://pco"+target, nil)
	require.NoError(t, err)
	res, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = res.Body.Close() })
	return res.StatusCode, res.Header, bufio.NewReader(res.Body)
}

// nextMessage reads a message of the stream, up to the blank line that ends
// it.
func nextMessage(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	var b strings.Builder
	for {
		line, err := r.ReadString('\n')
		require.NoError(t, err)
		b.WriteString(line)
		if line == "\n" {
			return b.String()
		}
	}
}

func receiveNotice(t *testing.T, ch <-chan engine.Notice) engine.Notice {
	t.Helper()
	select {
	case n, ok := <-ch:
		require.True(t, ok, "the stream ended")
		return n
	case <-time.After(10 * time.Second):
		t.Fatal("no notice came")
	}
	return engine.Notice{}
}

// requireText checks text against the golden file name.
func requireText(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), got, "the stream changed; run the test with -update when that is intended")
}

// streamNotices holds one notice of each kind.
func streamNotices() []engine.Notice {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return []engine.Notice{
		{Kind: engine.NoticeState, State: &engine.StateNotice{At: at, FinishedAt: at.Add(2 * time.Second), Digest: "77a1c0de5f3b2a90"}},
		{Kind: engine.NoticeEvent, Event: &engine.Event{
			Seq: 813, Boot: testBoot, At: at, Level: "warn", Kind: "route", Subject: "www.example.com",
			Message: "qemu/101: unreachable (connection refused)", Route: "www.example.com", Guest: "qemu/101", Account: "acc1",
		}},
		{Kind: engine.NoticeGap, Gap: &engine.GapNotice{Boot: testBoot, From: 814, To: 1826, Count: 1013, Level: "warn"}},
		{Kind: engine.NoticeTraffic, Traffic: &engine.TrafficNotice{At: at.Add(5 * time.Second), Tunnels: []engine.TunnelNotice{
			{TunnelID: "00000000-0000-4000-8000-000000000001", RPS: 38.2, ErrorsPerSec: 0.1, Concurrent: 3, HAConnections: 4},
		}, Routes: []engine.RouteTraffic{
			{Hostname: "www.example.com", Owner: "qemu/101", Target: "10.0.0.11:8080", FlowsPerSec: 2.4},
		}, RoutesTotal: 1}},
		{Kind: engine.NoticeEvent, Event: &engine.Event{
			Seq: 1827, Boot: testBoot, At: at, Level: "info", Kind: "admin", Subject: "www.example.com",
			Message: "adoption requested for the next run", Actor: "alice@pve (ticket)",
		}},
		{Kind: engine.NoticeReset, Reason: "boot changed"},
	}
}

func TestTheWireFormatOfTheStream(t *testing.T) {
	f, tick, s := streamServer()
	socket := socketPath(t)
	serve(t, s, socket)

	status, header, r := openStream(t, socket, "/v1/stream")

	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "text/event-stream", header.Get("Content-Type"))
	require.Equal(t, "no-store", header.Get("Cache-Control"))
	require.Equal(t, "no", header.Get("X-Accel-Buffering"))
	require.Equal(t, testBoot, header.Get("Pco-Boot"))
	hello := nextMessage(t, r)
	require.Equal(t, "event: hello\n"+
		`data: {"boot":"9f2c4e1a0b7d3c55","version":"1.2.3","seq":812,"digest":"5e0c1f7a92b4d3e8","pollInterval":"10s"}`+"\n\n",
		hello, "before any notice")
	// The ping's ticker may be made just after hello is written.
	require.Eventually(t, func() bool { return len(tick.asked()) > 0 }, 5*time.Second, time.Millisecond)
	require.Equal(t, []time.Duration{15 * time.Second}, tick.asked())

	text := hello
	for _, n := range streamNotices() {
		f.notices <- n
		text += nextMessage(t, r)
	}
	tick.c <- time.Time{}
	close(f.notices)
	rest, err := io.ReadAll(r)
	require.NoError(t, err)
	requireText(t, "stream.txt", text+string(rest))
}

func TestTheStreamResumesAfterTheLastEventID(t *testing.T) {
	for _, tt := range []struct {
		name, target, header, want string
	}{
		{"from now", "/v1/stream", "", "subscribe::0"},
		{"the header", "/v1/stream", testBoot + ":812", "subscribe:9f2c4e1a0b7d3c55:812"},
		{"the query", "/v1/stream?lastEventId=" + testBoot + ":40", "", "subscribe:9f2c4e1a0b7d3c55:40"},
		{"the header before the query", "/v1/stream?lastEventId=0123456789abcdef:1", testBoot + ":812", "subscribe:9f2c4e1a0b7d3c55:812"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, _, s := streamServer()
			close(f.notices)
			req := request(http.MethodGet, tt.target, "")
			if tt.header != "" {
				req.Header.Set("Last-Event-ID", tt.header)
			}

			rec := send(s, req)

			require.Equal(t, http.StatusOK, rec.Code)
			require.True(t, strings.HasPrefix(rec.Body.String(), "event: hello\n"), rec.Body.String())
			require.Equal(t, []string{tt.want}, f.called())
		})
	}

	for _, bad := range []string{"nonsense", testBoot, testBoot + ":", testBoot + ":x", testBoot + ":-1", "9F2C4E1A0B7D3C55:1", "9f2c:1", ":5"} {
		t.Run("malformed "+bad, func(t *testing.T) {
			f, _, s := streamServer()
			req := request(http.MethodGet, "/v1/stream", "")
			req.Header.Set("Last-Event-ID", bad)

			rec := send(s, req)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, "invalid", errorCode(t, rec))
			require.Contains(t, errorMessage(t, rec), "Last-Event-ID")
			require.Empty(t, f.called())
		})
	}
}

func TestAStreamTheEngineHasNoRoomForIsUnavailable(t *testing.T) {
	f, _, s := streamServer()
	f.subErr = fmt.Errorf("16 streams are open already: %w", engine.ErrBusy)

	rec := do(s, http.MethodGet, "/v1/stream", "")

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "unavailable", errorCode(t, rec))
	require.Equal(t, testBoot, rec.Header().Get("Pco-Boot"))
}

func TestTheClientReadsTheStream(t *testing.T) {
	f, _, s := streamServer()
	socket := socketPath(t)
	serve(t, s, socket)

	notices, hello, err := apiclient.New(socket).Stream(t.Context(), testBoot, 811)
	require.NoError(t, err)

	require.Equal(t, engine.Hello{Boot: testBoot, Version: "1.2.3", Seq: 812, Digest: "5e0c1f7a92b4d3e8", PollInterval: "10s"}, hello)
	require.Equal(t, []string{"subscribe:9f2c4e1a0b7d3c55:811"}, f.called())
	for _, n := range streamNotices() {
		f.notices <- n
		require.Equal(t, n, receiveNotice(t, notices))
	}
	close(f.notices)
	_, open := <-notices
	require.False(t, open, "the stream ended")
}

func TestTheStreamEndsCleanlyOnShutdown(t *testing.T) {
	_, _, s := streamServer()
	socket := socketPath(t)
	stop := serve(t, s, socket)
	notices, _, err := apiclient.New(socket).Stream(t.Context(), "", 0)
	require.NoError(t, err)

	require.NoError(t, stop(), "the stream does not hold the shutdown up")

	select {
	case _, open := <-notices:
		require.False(t, open)
	case <-time.After(10 * time.Second):
		t.Fatal("the stream did not end")
	}
}

func TestAStreamIsGivenUpBeforeItsAnswerEnds(t *testing.T) {
	f, _, s := streamServer()
	closing := make(chan struct{})
	close(closing)
	req := request(http.MethodGet, "/v1/stream", "")
	req = req.WithContext(context.WithValue(req.Context(), closingKey{}, (<-chan struct{})(closing)))

	rec := send(s, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Zero(t, f.streams(), "a client that comes back at once finds its place free")
}

func TestAStreamOutlivesTheWriteTimeout(t *testing.T) {
	f, _, s := streamServer()
	s.writeTimeout = time.Second
	socket := socketPath(t)
	serve(t, s, socket)
	notices, _, err := apiclient.New(socket).Stream(t.Context(), "", 0)
	require.NoError(t, err)

	// The write timeout is the connection's, on the wall clock.
	time.Sleep(3 * time.Second)
	n := streamNotices()[0]
	f.notices <- n

	require.Equal(t, n, receiveNotice(t, notices))
}
