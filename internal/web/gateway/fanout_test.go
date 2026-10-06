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

// throughScenario reads messages up to the last event of filterScenario.
func (r *streamReader) throughScenario() []readMessage {
	r.t.Helper()
	var out []readMessage
	for {
		m := r.next()
		out = append(out, m)
		if m.id == bootA+":23" {
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
	requireGolden(t, "stream/reader.txt", transcript(bob.throughScenario()))
	requireGolden(t, "stream/admin.txt", transcript(alice.throughScenario()))
	bob.quiet()
	require.Equal(t, 1, s.daemon.count("GET /v1/traffic"), "one fetch of the figures for every reader")

	// A browser that comes back resumes from the web's ring, filtered.
	again := s.signIn(reader).openStream(srv, bootA+":14")
	requireGolden(t, "stream/resume.txt", transcript(again.throughScenario()))
	again.quiet()

	// One whose last event is older than the ring gets a gap for what the
	// ring no longer holds, as the daemon's replay has it, then the ring.
	old := s.signIn(reader).openStream(srv, bootA+":2")
	requireGolden(t, "stream/resume_old.txt", transcript(old.throughScenario()))
	old.quiet()

	// One of another boot starts over; the stream stays open.
	sr := s.signIn(reader).openStream(srv, bootB+":30")
	got := sr.take(3)
	require.Equal(t, "reset", got[2].event)
	require.JSONEq(t, `{"reason":"boot changed"}`, got[2].data)
	conn.send <- eventMessage(engine.Event{Seq: 24, Boot: bootA, At: t0, Level: "warn", Kind: "problem", Message: "another problem"})
	require.Equal(t, bootA+":24", sr.next().id)
	require.True(t, sr.open())
}

// A browser that comes back after events the web never held, as after a
// start of pco web, gets a gap for them.
func TestAResumeBeforeTheRingIsAGap(t *testing.T) {
	s := newTestServer(t)
	s.daemon.stream.setHello(engine.Hello{Boot: bootA, Version: "v1.3.0", Seq: 12, Digest: "5e0c1f7a92b4d3e8", PollInterval: "10s"})
	s.daemon.serveState(populated(t))
	s.connect()
	sr := s.signIn(reader).openStream(s.streamServer(), bootA+":5")
	got := sr.take(3)
	require.Equal(t, "gap", got[2].event)
	require.Equal(t, bootA+":12", got[2].id)
	require.JSONEq(t, `{"boot":"`+bootA+`","from":6,"to":12,"count":7,"level":"warn"}`, got[2].data)
	sr.quiet()
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
		require.LessOrEqual(t, q.bytes, queueBytes)
	}
	items, closed := q.take()
	require.False(t, closed, "events that collapse are no reason to cut")
	require.Less(t, len(items), 60)
	require.NotNil(t, items[0].gap)
}

// One that is behind even so, with more than 256 notices or 1 MiB that do
// not collapse, is told to start over and its stream ends: no stream holds
// more than that.
func TestABrowserFarBehindStartsOver(t *testing.T) {
	q := newQueue()
	for seq := uint64(1); seq <= queueNotices; seq++ {
		q.push(lowEvent(seq, "a problem"))
	}
	items, closed := q.take()
	require.False(t, closed)
	require.Len(t, items, queueNotices)

	for seq := uint64(1); seq <= queueNotices+1; seq++ {
		q.push(lowEvent(seq, "a problem"))
	}
	q.push(lowEvent(5000, "after the reset"))
	items, closed = q.take()
	require.True(t, closed)
	require.Equal(t, "reset", kinds(items))
	require.Contains(t, string(items[0].msg), `"reason":"too far behind"`)

	q = newQueue()
	big := strings.Repeat("x", 20<<10)
	for seq := uint64(1); seq <= 60; seq++ {
		q.push(lowEvent(seq, big))
		require.LessOrEqual(t, q.bytes, queueBytes)
	}
	_, closed = q.take()
	require.True(t, closed, "1 MiB that does not collapse")
}

// After a collapse the ids of the stream still only grow: the gap goes where
// its last event was, after the events that stay before it.
func TestACollapseKeepsTheIdsInOrder(t *testing.T) {
	q := newQueue()
	var seq uint64
	for range 3 {
		for range 100 {
			seq++
			q.push(hvEvent(seq, "info"))
		}
		seq++
		q.push(lowEvent(seq, "a problem"))
		q.push(stateQueued(fmt.Sprint(seq)))
	}
	items, closed := q.take()
	require.False(t, closed)
	var last uint64
	for _, it := range items {
		var id uint64
		switch {
		case it.gap != nil:
			id = it.gap.To
		case it.event != nil:
			id = it.event.Seq
		default:
			continue
		}
		require.Greater(t, id, last, kinds(items))
		last = id
	}
	require.Equal(t, uint64(303), last)
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

// streamOf opens a stream of a new session of user and reads its hello and
// whether the daemon is reached.
func (s *testServer) streamOf(srv *httptest.Server, user string) *streamReader {
	s.t.Helper()
	sr := s.signIn(user).openStream(srv, "")
	require.Equal(s.t, http.StatusOK, sr.status)
	sr.take(2)
	return sr
}

// requireEnds checks that the stream ends, now or after the tick it is
// sent.
func requireEnds(t *testing.T, sr *streamReader) {
	t.Helper()
	select {
	case <-sr.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end")
	}
}

// A stream is checked with every ping: when its reader may see less, it
// ends, and the stream the page opens again resumes from the ring for what
// the reader may see now.
func TestAStreamEndsWhenItsReaderSeesLess(t *testing.T) {
	s := newTestServer(t)
	tick := s.ticks()
	s.daemon.stream.setHello(engine.Hello{Boot: bootA, Version: "v1.3.0", Seq: 12, Digest: "5e0c1f7a92b4d3e8", PollInterval: "10s"})
	s.daemon.serveState(populated(t))
	conn := s.connect()
	srv := s.streamServer()
	s.pve.sees(reader, 101, 102)
	b := s.signIn(reader)
	sr := b.openStream(srv, "")
	sr.take(2)
	conn.send <- eventMessage(routeEvent(13, "info", "qemu/101"))
	conn.send <- eventMessage(routeEvent(14, "warn", "qemu/102"))
	require.Equal(t, bootA+":13", sr.next().id)
	require.Equal(t, bootA+":14", sr.next().id)

	// Nothing changed: the tick is a ping.
	tick <- t0
	require.Equal(t, "comment", sr.next().event)

	// qemu/102 taken away: the set is asked for again after a minute.
	s.pve.sees(reader, 101)
	s.clock.advance(61 * time.Second)
	tick <- t0
	requireEnds(t, sr)

	again := b.openStream(srv, bootA+":12")
	require.Equal(t, http.StatusOK, again.status)
	got := again.take(3)
	require.Equal(t, bootA+":13", got[2].id)
	again.quiet()
}

func TestAStreamEndsWhenTheRoleChanges(t *testing.T) {
	s := newTestServer(t)
	tick := s.ticks()
	s.connect()
	srv := s.streamServer()
	sr := s.streamOf(srv, admin)
	s.pve.grant(admin, "Sys.Audit")
	s.clock.advance(31 * time.Second)
	tick <- t0
	requireEnds(t, sr)
}

func TestAStreamEndsWithItsSession(t *testing.T) {
	s := newTestServer(t)
	tick := s.ticks()
	s.connect()
	srv := s.streamServer()

	// Signed out: at once.
	b := s.signIn(reader)
	sr := b.openStream(srv, "")
	sr.take(2)
	require.Equal(t, http.StatusNoContent, b.do(http.MethodDelete, "/api/session", "").Code)
	requireEnds(t, sr)

	// Replaced by a new sign-in of the same browser: at once.
	b = s.signIn(reader)
	sr = b.openStream(srv, "")
	sr.take(2)
	b.do(http.MethodPost, "/api/session/ticket", "{}")
	requireEnds(t, sr)

	// Idle for half an hour: the stream is no activity, and the next tick
	// ends it.
	sr = s.streamOf(srv, reader)
	s.clock.advance(31 * time.Minute)
	tick <- t0
	requireEnds(t, sr)
}

// No events of a gap are read while no reader would be told of them.
func TestGapsAreReadOnlyForReaders(t *testing.T) {
	s := newTestServer(t)
	s.daemon.stream.setHello(engine.Hello{Boot: bootA, Version: "v1.3.0", Seq: 12, Digest: "5e0c1f7a92b4d3e8", PollInterval: "10s"})
	s.daemon.serveState(populated(t))
	s.daemon.serveEvents([]engine.Event{routeEvent(13, "info", "qemu/101"), routeEvent(14, "info", "qemu/101")})
	conn := s.connect()
	srv := s.streamServer()
	alice := s.streamOf(srv, admin)
	conn.send <- gapMessage(engine.GapNotice{Boot: bootA, From: 13, To: 14, Count: 2, Level: "info"})
	require.Equal(t, "gap", alice.next().event)
	require.Zero(t, s.daemon.count("GET /v1/events"))

	s.pve.sees(reader, 101)
	bob := s.streamOf(srv, reader)
	conn.send <- gapMessage(engine.GapNotice{Boot: bootA, From: 15, To: 16, Count: 2, Level: "info"})
	require.Equal(t, "gap", alice.next().event)
	require.Equal(t, 1, s.daemon.count("GET /v1/events"))
	bob.quiet()
}

// A daemon that is slow to answer a read does not hold up the subscription:
// what comes meanwhile is taken off the stream and handed on in order once
// the read is done.
func TestASlowReadDoesNotHoldTheSubscription(t *testing.T) {
	s := newTestServer(t)
	s.daemon.stream.setHello(engine.Hello{Boot: bootA, Version: "v1.3.0", Seq: 12, Digest: "5e0c1f7a92b4d3e8", PollInterval: "10s"})
	s.daemon.serveState(populated(t))
	release := make(chan struct{})
	s.daemon.on("GET /v1/events", func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode([]engine.Event{routeEvent(13, "info", "qemu/101")})
	})
	conn := s.connect()
	srv := s.streamServer()
	s.pve.sees(reader, 101)
	bob := s.streamOf(srv, reader)
	conn.send <- gapMessage(engine.GapNotice{Boot: bootA, From: 13, To: 13, Count: 1, Level: "info"})
	for seq := uint64(14); seq <= 33; seq++ {
		conn.send <- eventMessage(engine.Event{Seq: seq, Boot: bootA, At: t0, Level: "warn", Kind: "problem", Message: "a problem"})
	}
	eventually(t, "the subscription to take every notice", func() bool { return len(s.gw.inbox) == 20 })
	bob.quiet()
	close(release)
	got := bob.take(21)
	require.Equal(t, "gap", got[0].event)
	for i, m := range got[1:] {
		require.Equal(t, fmt.Sprintf("%s:%d", bootA, 14+i), m.id)
	}
}

// A reader's count of routes with a figure follows the figures, also when
// their number stays the same.
func TestAReadersRoutesTotalFollowsTheFigures(t *testing.T) {
	s := newTestServer(t)
	s.daemon.stream.setHello(engine.Hello{Boot: bootA, Version: "v1.3.0", Seq: 12, Digest: "5e0c1f7a92b4d3e8", PollInterval: "10s"})
	s.daemon.serveState(populated(t))
	conn := s.connect()
	srv := s.streamServer()
	s.pve.sees(reader, 101)
	bob := s.streamOf(srv, reader)
	figures := func(routes ...engine.RouteTraffic) {
		s.daemon.answer("GET /v1/traffic", http.StatusOK, engine.TrafficView{At: t0, Interval: "5s", Tunnels: []engine.TunnelTraffic{}, Routes: routes, RoutesTotal: len(routes)})
	}
	www := engine.RouteTraffic{Hostname: "www.example.com", Owner: "qemu/101", Target: "10.0.0.11:8080", FlowsPerSec: 1}
	shop := engine.RouteTraffic{Hostname: "shop.example.com", Owner: "lxc/200", Target: "10.0.0.12:8080", FlowsPerSec: 1}
	web := engine.RouteTraffic{Hostname: "web.example.com", Owner: "qemu/101", Target: "10.0.0.13:8080", FlowsPerSec: 1}
	total := func() int {
		m := bob.next()
		require.Equal(t, "traffic", m.event)
		var n engine.TrafficNotice
		require.NoError(t, json.Unmarshal([]byte(m.data), &n))
		return n.RoutesTotal
	}

	figures(www, shop)
	conn.send <- sse("", "traffic", engine.TrafficNotice{At: t0, Tunnels: []engine.TunnelNotice{}, Routes: []engine.RouteTraffic{shop}, RoutesTotal: 2})
	require.Equal(t, 1, total())

	// shop stopped and web started: two figures still.
	figures(www, web)
	conn.send <- sse("", "traffic", engine.TrafficNotice{At: t0, Tunnels: []engine.TunnelNotice{}, Routes: []engine.RouteTraffic{web}, RoutesTotal: 2})
	require.Equal(t, 2, total())
	require.Equal(t, 2, s.daemon.count("GET /v1/traffic"))

	// The same figures: they are not read again for every notice.
	conn.send <- sse("", "traffic", engine.TrafficNotice{At: t0, Tunnels: []engine.TunnelNotice{}, Routes: []engine.RouteTraffic{web}, RoutesTotal: 2})
	require.Equal(t, 2, total())
	require.Equal(t, 2, s.daemon.count("GET /v1/traffic"))
}
