package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/wire"
)

// streamServer serves the test server over real connections, as streams
// need.
func (s *testServer) streamServer() *httptest.Server {
	srv := httptest.NewServer(s.h)
	s.t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv
}

// connect starts the shared stream and waits until the gateway is up.
func (s *testServer) connect() *streamConn {
	s.t.Helper()
	s.run()
	conn := s.daemon.stream.next(s.t)
	eventually(s.t, "the upstream subscription", func() bool { return s.gw.hub.status().Up })
	return conn
}

// waitSleep waits for the gateway to wait before its next connection, and
// says how long it waits; wake lets it go on.
func (s *testServer) waitSleep() time.Duration {
	s.t.Helper()
	select {
	case d := <-s.sleeps:
		return d
	case <-time.After(5 * time.Second):
		s.t.Fatal("the gateway did not back off")
	}
	return 0
}

func (s *testServer) wake() { s.resume <- struct{}{} }

// backoff lets the gateway go on after its wait, and says how long it was.
func (s *testServer) backoff() time.Duration {
	s.t.Helper()
	d := s.waitSleep()
	s.wake()
	return d
}

// serveEvents answers GET /v1/events from evs, as the daemon reads its ring
// with boot, after and limit.
func (d *fakeDaemon) serveEvents(evs []engine.Event) {
	d.on("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		after, _ := strconv.ParseUint(q.Get("after"), 10, 64)
		limit, err := strconv.Atoi(q.Get("limit"))
		if err != nil {
			limit = 1000
		}
		out := []engine.Event{}
		for _, ev := range evs {
			if (q.Get("boot") == "" || ev.Boot == q.Get("boot")) && ev.Seq > after && len(out) < limit {
				out = append(out, ev)
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(out)
	})
}

func routeEvent(seq uint64, level, guest string) engine.Event {
	return engine.Event{Seq: seq, Boot: bootA, At: t0, Level: level, Kind: "route", Subject: "www.example.com", Message: guest + ": active", Route: "www.example.com", Guest: guest}
}

func TestAStreamBeginsWithHelloThenUpstream(t *testing.T) {
	s := newTestServer(t)
	s.daemon.stream.setHello(engine.Hello{Boot: bootA, Version: "v1.3.0", Seq: 12, Digest: "5e0c1f7a92b4d3e8", PollInterval: "10s"})
	s.daemon.serveState(populated(t))
	s.connect()
	srv := s.streamServer()

	sr := s.signIn(admin).openStream(srv, "")
	require.Equal(t, http.StatusOK, sr.status)
	require.Equal(t, "text/event-stream", sr.header.Get("Content-Type"))
	require.Empty(t, sr.header.Get("Content-Encoding"))
	require.Equal(t, "no-store", sr.header.Get("Cache-Control"))
	requireGolden(t, "stream/hello.txt", transcript(sr.take(2)))
	sr.quiet()
}

func TestAStreamSaysWhenTheDaemonIsOutOfReach(t *testing.T) {
	s := newTestServer(t)
	s.daemon.stream.refuseNext(1)
	s.run()
	require.Equal(t, time.Second, s.waitSleep())
	srv := s.streamServer()
	sr := s.signIn(reader).openStream(srv, "")
	got := sr.take(2)
	require.Equal(t, "hello", got[0].event)
	require.Equal(t, "upstream", got[1].event)
	require.JSONEq(t, `{"up":false,"since":"2026-10-01T12:00:00Z"}`, got[1].data)
	s.wake()

	s.daemon.stream.next(t)
	m := sr.next()
	require.Equal(t, "upstream", m.event)
	require.JSONEq(t, `{"up":true,"since":"2026-10-01T12:00:00Z"}`, m.data)
	m = sr.next()
	require.Equal(t, "reset", m.event, "the stream began without a boot")
	require.JSONEq(t, `{"reason":"boot changed"}`, m.data)
}

// filterScenario sends what the daemon sends of one cycle and what follows it:
// events of a visible and a hidden guest, a gap of both, a gap of the hidden
// one only, traffic, an approval of a hidden guest, a rollout.
func filterScenario(t *testing.T, s *testServer, conn *streamConn) {
	t.Helper()
	gapped := []engine.Event{
		routeEvent(16, "error", "qemu/102"),
		routeEvent(17, "info", "qemu/101"),
		{Seq: 18, Boot: bootA, At: t0, Level: "info", Kind: "action", Subject: "old.example.com", Message: "delete-record in zone example.com", Route: "old.example.com"},
		{Seq: 19, Boot: bootA, At: t0, Level: "warn", Kind: "claim", Subject: "new.example.com", Message: "pending qemu/102", Route: "new.example.com", Guest: "qemu/102"},
		routeEvent(20, "warn", "qemu/102"),
		routeEvent(21, "warn", "qemu/102"),
	}
	s.daemon.serveEvents(gapped)
	notice := engine.TrafficNotice{At: t0, Tunnels: []engine.TunnelNotice{{TunnelID: "00000000-0000-4000-8000-000000000001", RPS: 38.2, HAConnections: 4, Sampled: true}},
		Routes: []engine.RouteTraffic{
			{Hostname: "www.example.com", Owner: "qemu/101", Target: "10.0.0.11:8080", FlowsPerSec: 1.5, Shared: 2},
			{Hostname: "shop.example.com", Owner: "lxc/200", Target: "10.0.0.11:8080", FlowsPerSec: 1.5, Shared: 2},
		}, RoutesTotal: 4}
	s.daemon.answer("GET /v1/traffic", http.StatusOK, trafficAnswer)
	for _, m := range []string{
		sse("", "state", engine.StateNotice{At: t0, FinishedAt: t0.Add(2 * time.Second), Digest: "5e0c1f7a92b4d3e8"}),
		eventMessage(routeEvent(13, "info", "qemu/101")),
		eventMessage(routeEvent(14, "warn", "qemu/102")),
		eventMessage(engine.Event{Seq: 15, Boot: bootA, At: t0, Level: "warn", Kind: "problem", Message: "a problem"}),
		gapMessage(engine.GapNotice{Boot: bootA, From: 16, To: 19, Count: 4, Level: "error"}),
		gapMessage(engine.GapNotice{Boot: bootA, From: 20, To: 21, Count: 2, Level: "warn"}),
		sse("", "traffic", notice),
		eventMessage(engine.Event{Seq: 22, Boot: bootA, At: t0, Level: "info", Kind: "admin", Subject: "lxc/202", Message: "lxc/202 (dns-1) is approved in identity uuid:202"}),
		eventMessage(engine.Event{Seq: 23, Boot: bootA, At: t0, Level: "info", Kind: "rollout", Subject: "pco-abc123", Message: "version 3 runs on 1 connector", Tunnel: "pco-abc123"}),
	} {
		conn.send <- m
	}
}

// until reads messages up to the one with id.
func (r *streamReader) until(id string) []readMessage {
	r.t.Helper()
	var out []readMessage
	for {
		m := r.next()
		out = append(out, m)
		if m.id == id {
			return out
		}
	}
}

func TestStreamsAreFilteredForTheirSession(t *testing.T) {
	s := newTestServer(t)
	s.daemon.stream.setHello(engine.Hello{Boot: bootA, Version: "v1.3.0", Seq: 12, Digest: "5e0c1f7a92b4d3e8", PollInterval: "10s"})
	s.daemon.serveState(populated(t))
	conn := s.connect()
	srv := s.streamServer()
	s.pve.sees(reader, 101, 201)
	bob := s.signIn(reader).openStream(srv, "")
	alice := s.signIn(admin).openStream(srv, "")
	bob.take(2)
	alice.take(2)

	filterScenario(t, s, conn)
	requireGolden(t, "stream/reader.txt", transcript(bob.until(bootA+":23")))
	requireGolden(t, "stream/admin.txt", transcript(alice.until(bootA+":23")))
	bob.quiet()
	require.Equal(t, 1, s.daemon.count("GET /v1/traffic"), "one fetch of the figures for every reader")

	// A browser that comes back resumes from the web's ring, filtered.
	again := s.signIn(reader).openStream(srv, bootA+":14")
	requireGolden(t, "stream/resume.txt", transcript(again.until(bootA+":23")))
	again.quiet()

	// One whose last event the ring does not hold starts over, and so does
	// one of another boot; the stream stays open.
	for i, c := range []struct{ last, reason string }{{bootA + ":2", "too far behind"}, {bootB + ":30", "boot changed"}} {
		sr := s.signIn(reader).openStream(srv, c.last)
		got := sr.take(3)
		require.Equal(t, "reset", got[2].event, c.last)
		require.JSONEq(t, `{"reason":"`+c.reason+`"}`, got[2].data)
		seq := uint64(24 + i)
		conn.send <- eventMessage(engine.Event{Seq: seq, Boot: bootA, At: t0, Level: "warn", Kind: "problem", Message: "another problem"})
		require.Equal(t, fmt.Sprintf("%s:%d", bootA, seq), sr.next().id)
		require.True(t, sr.open())
	}
}

func TestAMalformedLastEventIDIsRefused(t *testing.T) {
	s := newTestServer(t)
	srv := s.streamServer()
	sr := s.signIn(reader).openStream(srv, "not-an-id")
	require.Equal(t, http.StatusBadRequest, sr.status)
}

func TestABootChangeResetsTheStreamWithoutClosingIt(t *testing.T) {
	s := newTestServer(t)
	s.daemon.stream.setHello(engine.Hello{Boot: bootA, Version: "v1.3.0", Seq: 12, Digest: "5e0c1f7a92b4d3e8", PollInterval: "10s"})
	s.daemon.serveState(populated(t))
	conn := s.connect()
	srv := s.streamServer()
	sr := s.signIn(reader).openStream(srv, "")
	got := sr.take(2)
	conn.send <- eventMessage(engine.Event{Seq: 13, Boot: bootA, At: t0, Level: "warn", Kind: "problem", Message: "a problem"})
	got = append(got, sr.next())

	s.clock.advance(90 * time.Second)
	s.daemon.stream.setHello(engine.Hello{Boot: bootB, Version: "v1.3.1", Seq: 0, Digest: "5e0c1f7a92b4d3e8", PollInterval: "10s"})
	close(conn.drop)
	got = append(got, sr.next())
	require.Equal(t, time.Second, s.waitSleep())
	s.clock.advance(time.Second)
	s.wake()
	conn = s.daemon.stream.next(t)
	require.Equal(t, bootA+":13", conn.lastEventID, "the gateway resumes after the last event it holds")
	got = append(got, sr.take(2)...)
	conn.send <- eventMessage(engine.Event{Seq: 1, Boot: bootB, At: t0, Level: "info", Kind: "writer", Message: "this node writes"})
	got = append(got, sr.next())
	require.True(t, sr.open())
	requireGolden(t, "stream/boot_change.txt", transcript(got))
}

func TestTheUpstreamBacksOff(t *testing.T) {
	s := newTestServer(t)
	s.daemon.stream.setHello(engine.Hello{Boot: bootA, Version: "v1.3.0", Seq: 12, Digest: "5e0c1f7a92b4d3e8", PollInterval: "10s"})
	s.daemon.serveState(populated(t))
	s.daemon.stream.refuseNext(5)
	s.run()
	var waits []time.Duration
	for range 5 {
		waits = append(waits, s.backoff())
	}
	require.Equal(t, []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 10 * time.Second}, waits)
	conn := s.daemon.stream.next(t)
	require.Empty(t, conn.lastEventID, "the gateway held no boot yet")
	eventually(t, "up", func() bool { return s.gw.hub.status().Up })

	srv := s.streamServer()
	sr := s.signIn(reader).openStream(srv, "")
	sr.take(2)
	s.clock.advance(time.Minute)
	close(conn.drop)
	m := sr.next()
	require.Equal(t, "upstream", m.event)
	require.JSONEq(t, `{"up":false,"since":"2026-10-01T12:01:00Z"}`, m.data)
	require.Equal(t, time.Second, s.backoff(), "a lost stream is tried again after a second")
	conn = s.daemon.stream.next(t)
	require.Equal(t, bootA+":12", conn.lastEventID)
	m = sr.next()
	require.Equal(t, "upstream", m.event)
	require.JSONEq(t, `{"up":true,"since":"2026-10-01T12:01:00Z"}`, m.data)
	sr.quiet()
}

func TestStreamsArePinged(t *testing.T) {
	s := newTestServer(t)
	tick := make(chan time.Time)
	s.gw.ticker = func(every time.Duration) (<-chan time.Time, func()) {
		require.Equal(t, 15*time.Second, every)
		return tick, func() {}
	}
	s.connect()
	sr := s.signIn(reader).openStream(s.streamServer(), "")
	sr.take(2)
	tick <- t0
	m := sr.next()
	require.Equal(t, "comment", m.event)
	require.Equal(t, "ping", m.data)
}

func TestStreamsAreLimitedPerSessionAndInAll(t *testing.T) {
	s := newTestServer(t)
	s.connect()
	srv := s.streamServer()
	b := s.signIn(reader)
	var open []*streamReader
	for range 3 {
		sr := b.openStream(srv, "")
		require.Equal(t, http.StatusOK, sr.status)
		sr.take(2)
		open = append(open, sr)
	}
	requireStreamRefused(t, b.openStream(srv, ""))
	other := s.signIn(reader).openStream(srv, "")
	other.take(2)

	// A stream that ends gives its place back.
	open[0].close()
	eventually(t, "the place of the closed stream", func() bool {
		sr := b.openStream(srv, "")
		if sr.status == http.StatusOK {
			sr.take(2)
			return true
		}
		return false
	})

	// 200 streams in all.
	for i := range maxStreams - s.gw.slots.open() {
		require.True(t, s.gw.slots.take(fmt.Sprintf("session-%d", i)))
	}
	requireStreamRefused(t, s.signIn(reader2).openStream(srv, ""))
}

func requireStreamRefused(t *testing.T, sr *streamReader) {
	t.Helper()
	require.Equal(t, http.StatusTooManyRequests, sr.status)
	require.Equal(t, "30", sr.header.Get("Retry-After"))
	var body wire.Error
	require.NoError(t, json.Unmarshal(sr.refusal, &body))
	require.Equal(t, wire.CodeRateLimited, body.Code)
	require.Equal(t, 30, body.RetryAfter)
}

func hvEvent(seq uint64, level string) queued {
	ev := engine.Event{Seq: seq, Boot: bootA, Level: level, Kind: "route", Subject: "www.example.com", Route: "www.example.com", Guest: "qemu/101"}
	return eventQueued(&ev)
}

func lowEvent(seq uint64, message string) queued {
	ev := engine.Event{Seq: seq, Boot: bootA, Level: "warn", Kind: "problem", Message: message}
	return eventQueued(&ev)
}

func stateQueued(digest string) queued {
	return noticeQueued("state", &engine.StateNotice{At: t0, Digest: digest})
}

func kinds(qs []queued) string {
	var parts []string
	for _, q := range qs {
		switch {
		case q.gap != nil:
			parts = append(parts, fmt.Sprintf("gap %d-%d %d %s", q.gap.From, q.gap.To, q.gap.Count, q.gap.Level))
		case q.event != nil:
			parts = append(parts, fmt.Sprintf("event %d", q.event.Seq))
		default:
			parts = append(parts, q.kind)
		}
	}
	return strings.Join(parts, ", ")
}

// A browser that does not keep up has its route events collapsed into a gap,
// keeps every other event and only the newest state, and is not cut off.
func TestASlowBrowserIsCoalescedAndNotCut(t *testing.T) {
	q := newQueue()
	q.push(stateQueued("d1"))
	for seq := uint64(1); seq <= 300; seq++ {
		if seq == 150 {
			q.push(lowEvent(seq, "a problem"))
			q.push(stateQueued("d2"))
			continue
		}
		level := "info"
		if seq == 77 {
			level = "error"
		}
		q.push(hvEvent(seq, level))
	}
	items, closed := q.take()
	require.False(t, closed)
	want := []string{"event 150", "state", "gap 1-255 254 error"}
	for seq := 256; seq <= 300; seq++ {
		want = append(want, fmt.Sprintf("event %d", seq))
	}
	require.Equal(t, strings.Join(want, ", "), kinds(items))
	var gap engine.GapNotice
	for _, it := range items {
		if it.gap != nil {
			gap = *it.gap
			require.True(t, strings.HasPrefix(string(it.msg), "id: "+bootA+":255\nevent: gap\ndata: "), string(it.msg))
		}
	}
	require.Equal(t, 254, gap.Count)
	require.Contains(t, string(items[1].msg), `"digest":"d2"`, "the newest state is the one kept")
}

func TestTheQueueIsBoundInBytes(t *testing.T) {
	q := newQueue()
	big := strings.Repeat("x", 20<<10)
	for seq := uint64(1); seq <= 100; seq++ {
		ev := engine.Event{Seq: seq, Boot: bootA, Level: "info", Kind: "action", Subject: big, Route: "www.example.com"}
		q.push(eventQueued(&ev))
		require.LessOrEqual(t, q.bytes, queueBytes+len(eventQueued(&ev).msg))
	}
	items, closed := q.take()
	require.False(t, closed)
	require.Less(t, len(items), 60)
	require.NotNil(t, items[0].gap)
}

// One that is far behind even so, with more than 4096 events that do not
// collapse, is told to start over and its stream ends.
func TestABrowserFarBehindStartsOver(t *testing.T) {
	q := newQueue()
	for seq := uint64(1); seq <= maxNotices+1; seq++ {
		q.push(lowEvent(seq, "a problem"))
	}
	q.push(lowEvent(5000, "after the reset"))
	items, closed := q.take()
	require.True(t, closed)
	require.Equal(t, "reset", kinds(items))
	require.Contains(t, string(items[0].msg), `"reason":"too far behind"`)
}

func TestAReplayIsBatchedAsTheDaemonDoes(t *testing.T) {
	var qs []queued
	for seq := uint64(1); seq <= 40; seq++ {
		qs = append(qs, hvEvent(seq, "info"))
		if seq == 35 {
			qs = append(qs, lowEvent(100, "a problem"))
		}
	}
	got := batched(qs)
	var want []string
	for seq := 1; seq <= 32; seq++ {
		want = append(want, fmt.Sprintf("event %d", seq))
	}
	want = append(want, "event 100", "gap 33-40 8 info")
	require.Equal(t, strings.Join(want, ", "), kinds(got))
	require.Equal(t, "event 1", kinds(batched(qs[:1])))
}
