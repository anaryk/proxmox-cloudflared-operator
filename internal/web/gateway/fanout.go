package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/auth"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/wire"
)

const (
	// ringEvents is how many events of the daemon's boot the web keeps, for a
	// browser that comes back.
	ringEvents = 1000
	pingEvery  = 15 * time.Second
	// writeWindow is how long one write to a browser may take; one that
	// stalls longer ends its stream.
	writeWindow = 30 * time.Second
	// singleEvents is how many events of the high-volume kinds of a replay go
	// out one by one; the rest go out as one gap, as the daemon sends them.
	singleEvents = 32
	// A browser with this many notices, or bytes of them, waiting is full,
	// and what waits collapses; one that has as many even so is told to start
	// over, and its stream ends. 200 streams hold 200 MiB at most.
	queueNotices = 256
	queueBytes   = 1 << 20
	// maxGapEvents is the most events of a gap the gateway reads, the most
	// a query of the daemon answers with.
	maxGapEvents = engine.MaxEventLimit
	// inboxSize is how far the reads of the gateway may fall behind the
	// subscription before it waits for them.
	inboxSize = 1024
	// figuresEvery is how many traffic notices the figures of every route
	// answer for at most: they are read again with the next.
	figuresEvery = 12

	resetBootChanged = "boot changed"
	resetBehind      = "too far behind"
	noticeUpstream   = "upstream"
)

// backoff are the waits before the gateway connects to the daemon's stream
// again: after a lost stream the first, after each failed connection the
// next, the last one from then on.
var backoff = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second}

// Run holds the one subscription to the daemon's stream that every browser
// stream is fed from, until ctx ends. It resumes after the last event it
// took, so that the ring has no hole the daemon could fill, and tells the
// browsers whenever the subscription goes down or comes up. What the
// browsers need read from the daemon is read by a worker, so that the
// subscription goes on taking notices meanwhile.
func (g *Gateway) Run(ctx context.Context) {
	g.hub.started(g.now())
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.work(ctx)
	}()
	defer func() { <-done }()
	var (
		boot     string
		after    uint64
		failures int
	)
	for {
		notices, hello, err := g.subscribe(ctx, boot, after)
		if err == nil {
			failures = 0
			if hello.Boot != boot {
				boot, after = hello.Boot, hello.Seq
			}
			if !g.hand(ctx, upstreamItem{hello: &hello}) {
				return
			}
			for n := range notices {
				switch {
				case n.Kind == engine.NoticeEvent && n.Event != nil:
					after = max(after, n.Event.Seq)
				case n.Kind == engine.NoticeGap && n.Gap != nil:
					after = max(after, n.Gap.To)
				}
				if !g.hand(ctx, upstreamItem{notice: &n}) {
					return
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			g.log.Warn().Err(err).Msg("the stream of the daemon cannot be followed")
		} else {
			g.log.Info().Msg("the stream of the daemon ended")
		}
		if !g.hand(ctx, upstreamItem{lost: g.now()}) {
			return
		}
		wait := backoff[min(failures, len(backoff)-1)]
		failures++
		if !g.sleep(ctx, wait) {
			return
		}
	}
}

// upstreamItem is what the subscription hands the worker, in its order: the
// hello of a new subscription, a notice, or the loss of the subscription.
type upstreamItem struct {
	hello  *engine.Hello
	notice *engine.Notice
	lost   time.Time
}

// hand passes an item to the worker; it waits only while the worker is
// inboxSize items behind.
func (g *Gateway) hand(ctx context.Context, it upstreamItem) bool {
	select {
	case g.inbox <- it:
		return true
	case <-ctx.Done():
		return false
	}
}

// work hands what the subscription took on to the browsers, in order, until
// ctx ends.
func (g *Gateway) work(ctx context.Context) {
	for {
		select {
		case it := <-g.inbox:
			switch {
			case it.hello != nil:
				g.connected(ctx, *it.hello)
			case it.notice != nil:
				g.receive(ctx, *it.notice)
			default:
				g.hub.down(it.lost)
			}
		case <-ctx.Done():
			return
		}
	}
}

// connected takes the hello of a new subscription, and the daemon's state
// when it is not the one the gateway holds.
func (g *Gateway) connected(ctx context.Context, hello engine.Hello) {
	if held := g.states.newest(); held == nil || held.digest != hello.Digest {
		g.refreshState(ctx)
	}
	g.hub.up(hello, g.now())
}

// receive hands a notice of the daemon on. What the readers need to have it
// filtered is read first: the state when its digest changed, the events of
// a gap, the figures of every route for a notice that carries only some.
// While no reader's stream is open, nothing but the state is read.
func (g *Gateway) receive(ctx context.Context, n engine.Notice) {
	switch {
	case n.Kind == engine.NoticeState && n.State != nil:
		if g.hub.digestChanged(n.State.Digest) && g.hub.streaming() {
			g.refreshState(ctx)
		}
		g.hub.notice(n, nil)
	case n.Kind == engine.NoticeEvent && n.Event != nil:
		g.hub.event(item{event: n.Event})
	case n.Kind == engine.NoticeGap && n.Gap != nil:
		it := item{gap: n.Gap}
		if g.hub.readers() {
			it.in, it.known = g.gapEvents(ctx, *n.Gap)
		}
		g.hub.event(it)
	case n.Kind == engine.NoticeTraffic && n.Traffic != nil:
		var all []engine.RouteTraffic
		if g.hub.readers() && n.Traffic.RoutesWhy == "" && n.Traffic.RoutesTotal > len(n.Traffic.Routes) {
			all = g.hub.figures(ctx, n.Traffic)
		}
		g.hub.notice(n, all)
	}
	// A reset of the daemon says that the gateway fell behind or that the
	// daemon started again: the subscription ends or begins with another
	// boot, and the browsers are told then.
}

func (g *Gateway) refreshState(ctx context.Context) {
	if _, err := g.current(ctx, ""); err != nil {
		g.log.Warn().Err(err).Msg("reading the state of the daemon for the streams")
	}
}

// gapEvents reads the events of a gap, so that a reader is told of those it
// sees; known is false when they could not be read.
func (g *Gateway) gapEvents(ctx context.Context, gap engine.GapNotice) (in []engine.Event, known bool) {
	limit := min(gap.To-gap.From+1, maxGapEvents)
	q := url.Values{"boot": {gap.Boot}, "after": {strconv.FormatUint(gap.From-1, 10)}, "limit": {strconv.FormatUint(limit, 10)}}
	data, err := g.get(ctx, "", "/v1/events", q)
	if err == nil {
		err = json.Unmarshal(data, &in)
	}
	if err != nil {
		g.log.Warn().Err(err).Msg("reading the events of a gap; readers are told of the whole range")
		return nil, false
	}
	return in, true
}

// item is an event of the daemon or a gap, as the ring keeps it.
type item struct {
	event *engine.Event
	gap   *engine.GapNotice
	in    []engine.Event // the events of the gap
	known bool           // in could be read
}

func (it item) first() uint64 {
	if it.gap != nil {
		return it.gap.From
	}
	return it.event.Seq
}

func (it item) last() uint64 {
	if it.gap != nil {
		return it.gap.To
	}
	return it.event.Seq
}

func (it item) count() int {
	if it.gap != nil {
		return it.gap.Count
	}
	return 1
}

// hub hands the notices of the subscription to the browser streams, each
// filtered for its session.
type hub struct {
	g *Gateway

	mu      sync.Mutex
	streams map[*stream]bool
	// hello is the daemon's, but for Seq, which is that of the last event
	// the hub holds of the boot.
	hello engine.Hello
	link  wire.Upstream // whether the subscription is up, and since when
	ring  []item        // of hello.Boot, oldest first
	held  int           // events in the ring
	// hosts are the hostnames of the routes of each visible set, in the
	// state of hostsOf.
	hosts   map[string]map[string]bool
	hostsOf string
	// all are the figures of every route, as the last read of them gave,
	// read again when their number or the state changes, when a notice has
	// a route they lack, and after figuresEvery notices.
	all       []engine.RouteTraffic
	allTotal  int
	allDigest string
	allRoutes map[string]bool // hostname and owner of each of all
	allUsed   int             // notices all answered for
}

func newHub(g *Gateway) *hub {
	return &hub{g: g, streams: map[*stream]bool{}, hosts: map[string]map[string]bool{}}
}

func (h *hub) started(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.link.Since.IsZero() {
		h.link.Since = now.UTC()
	}
}

func (h *hub) status() wire.Upstream {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.link
}

func (h *hub) streaming() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.streams) > 0
}

func (h *hub) readers() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.streams {
		if s.reader != nil {
			return true
		}
	}
	return false
}

func (h *hub) digestChanged(digest string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	changed := digest != h.hello.Digest
	h.hello.Digest = digest
	return changed
}

// up takes the hello of a new subscription. Another boot empties the ring
// and tells every stream to start over; the same boot resumes where the
// ring ends.
func (h *hub) up(hello engine.Hello, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.link.Up {
		h.link = wire.Upstream{Up: true, Since: now.UTC()}
		h.broadcast(func(*stream) []queued { return []queued{noticeQueued(noticeUpstream, h.link)} })
	}
	if hello.Boot == h.hello.Boot {
		hello.Seq = h.hello.Seq
		h.hello = hello
		return
	}
	h.hello = hello
	h.ring, h.held = nil, 0
	reset := resetQueued(resetBootChanged)
	h.broadcast(func(s *stream) []queued {
		if s.boot == hello.Boot {
			return nil
		}
		s.boot = hello.Boot
		return []queued{reset}
	})
}

// down says that the subscription is lost, when it was up.
func (h *hub) down(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.link.Up {
		return
	}
	h.link = wire.Upstream{Up: false, Since: now.UTC()}
	h.broadcast(func(*stream) []queued { return []queued{noticeQueued(noticeUpstream, h.link)} })
}

// event keeps an event or a gap in the ring and hands it on.
func (h *hub) event(it item) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ring = append(h.ring, it)
	h.held += it.count()
	for len(h.ring) > 1 && h.held > ringEvents {
		h.held -= h.ring[0].count()
		h.ring[0] = item{}
		h.ring = h.ring[1:]
	}
	h.hello.Seq = max(h.hello.Seq, it.last())
	views := map[string][]queued{}
	h.broadcast(func(s *stream) []queued {
		qs, ok := views[s.key]
		if !ok {
			qs = h.render(it, s.reader)
			views[s.key] = qs
		}
		return qs
	})
}

// notice hands on a state or a traffic notice; all are the figures of every
// route for a traffic notice that carries only some.
func (h *hub) notice(n engine.Notice, all []engine.RouteTraffic) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n.Kind == engine.NoticeState {
		q := noticeQueued(engine.NoticeState, n.State)
		h.broadcast(func(*stream) []queued { return []queued{q} })
		return
	}
	views := map[string][]queued{}
	h.broadcast(func(s *stream) []queued {
		qs, ok := views[s.key]
		if !ok {
			t := *n.Traffic
			if s.reader != nil {
				t = trafficNotice(t, s.reader.Visible, all)
			}
			qs = []queued{noticeQueued(engine.NoticeTraffic, t)}
			views[s.key] = qs
		}
		return qs
	})
}

// figures are the figures of every route for a notice that carries some of
// them; nil when they cannot be read. They are read again when the number
// of routes with a figure or the state changed, when the notice has a route
// they lack, and after figuresEvery notices, so that a reader's count of
// its routes with a figure follows a route that stops and another that
// starts.
func (h *hub) figures(ctx context.Context, n *engine.TrafficNotice) []engine.RouteTraffic {
	h.mu.Lock()
	digest := h.hello.Digest
	fresh := h.all != nil && h.allTotal == n.RoutesTotal && h.allDigest == digest && h.allUsed < figuresEvery
	for _, r := range n.Routes {
		fresh = fresh && h.allRoutes[r.Hostname+" "+r.Owner]
	}
	if fresh {
		h.allUsed++
		all := h.all
		h.mu.Unlock()
		return all
	}
	h.mu.Unlock()
	data, err := h.g.get(ctx, "", "/v1/traffic", nil)
	var tv engine.TrafficView
	if err == nil {
		err = json.Unmarshal(data, &tv)
	}
	if err != nil {
		h.g.log.Warn().Err(err).Msg("reading the figures of the routes for the readers' streams")
		return nil
	}
	routes := make(map[string]bool, len(tv.Routes))
	for _, r := range tv.Routes {
		routes[r.Hostname+" "+r.Owner] = true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.all, h.allTotal, h.allDigest, h.allRoutes, h.allUsed = tv.Routes, n.RoutesTotal, digest, routes, 1
	return tv.Routes
}

// broadcast queues for every stream what fn gives it. The caller holds mu.
func (h *hub) broadcast(fn func(*stream) []queued) {
	for s := range h.streams {
		if qs := fn(s); len(qs) > 0 {
			s.q.push(qs...)
		}
	}
}

// render is what a stream gets of an item of the ring: an admin all of it,
// a reader the event when it sees it, of a gap the events it sees.
func (h *hub) render(it item, r *Reader) []queued {
	if it.event != nil {
		if r != nil && !keepEvent(*it.event, r.Visible, h.hostsFor(r)) {
			return nil
		}
		return []queued{eventQueued(it.event)}
	}
	if r == nil {
		return []queued{gapQueued(it.gap)}
	}
	g, ok := readerGap(*it.gap, it.in, it.known, r.Visible, h.hostsFor(r))
	if !ok {
		return nil
	}
	return []queued{gapQueued(&g)}
}

// hostsFor are the hostnames of the routes a reader sees in the newest
// state the gateway holds; none without one. The caller holds mu.
func (h *hub) hostsFor(r *Reader) map[string]bool {
	s := h.g.states.newest()
	if s == nil {
		return nil
	}
	if s.digest != h.hostsOf || s.digest == "" {
		h.hosts, h.hostsOf = map[string]map[string]bool{}, s.digest
	}
	if hosts, ok := h.hosts[r.Hash]; ok {
		return hosts
	}
	st, err := s.state()
	if err != nil {
		return nil
	}
	hosts := visibleHosts(st, r.Visible)
	h.hosts[r.Hash] = hosts
	return hosts
}

// join adds a stream: its hello, then whether the daemon is reached, then
// what it missed of the boot it names, as the daemon replays its ring: a
// gap for the events the ring no longer holds, then those it holds, for
// what the session may see. Of another boot it gets a reset.
func (h *hub) join(s *stream, boot string, after uint64) engine.Hello {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.streams[s] = true
	hello := h.hello
	s.boot = hello.Boot
	s.q.push(noticeQueued(noticeUpstream, h.link))
	switch {
	case boot == "":
	case boot != hello.Boot:
		s.q.push(resetQueued(resetBootChanged))
	case after < hello.Seq:
		s.q.push(h.replay(after, s.reader)...)
	}
	return hello
}

// replay is what a stream that saw the events up to after is sent of the
// ring. The levels of the events the ring no longer holds are not known; the
// gap for them says warn, so that the page looks. A replay that would fill
// the stream's queue is one such gap: the page reads the events it wants.
func (h *hub) replay(after uint64, r *Reader) []queued {
	first := h.hello.Seq + 1
	if len(h.ring) > 0 {
		first = min(first, h.ring[0].first())
	}
	unknown := func(from, to uint64) queued {
		g := unseenGap(h.hello.Boot, from, to)
		return gapQueued(&g)
	}
	var qs []queued
	if first > after+1 {
		qs = append(qs, unknown(after+1, first-1))
	}
	var held []queued
	for _, it := range h.ring {
		if it.last() > after {
			held = append(held, h.render(it, r)...)
		}
	}
	qs = append(qs, batched(held)...)
	size := 0
	for _, q := range qs {
		size += len(q.msg)
	}
	if len(qs) >= queueNotices-1 || size >= queueBytes/2 {
		return []queued{unknown(after+1, h.hello.Seq)}
	}
	return qs
}

// endSession ends the streams of a session that ended.
func (h *hub) endSession(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.streams {
		if s.session == id {
			s.q.end()
		}
	}
}

func (h *hub) leave(s *stream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.streams, s)
}

// stream is a browser's stream: the notices that wait for it.
type stream struct {
	session string
	role    auth.Role
	reader  *Reader // nil for an admin
	key     string  // of what it gets: "" for admins, the visible set for a reader
	boot    string  // of the notices it was sent
	q       *queue
}

// stream answers GET /api/v1/stream: the hello, whether the daemon is
// reached, then the notices of the shared subscription for this session, and
// a comment every 15 s, until the browser goes, the server stops or the
// session no longer holds as it did. Last-Event-ID, or the query
// lastEventId, resumes after an event.
func (g *Gateway) stream(c *gin.Context, _ Rule, r *Reader) {
	boot, after, err := resumeFrom(c)
	if err != nil {
		refuse(c, http.StatusBadRequest, wire.Error{Error: err.Error(), Code: wire.CodeInvalid})
		return
	}
	session := auth.SessionOf(c)
	if !g.slots.take(session.ID) {
		refuse(c, http.StatusTooManyRequests, wire.Error{
			Error: "too many streams are open: 3 for each session and 200 in all", Code: wire.CodeRateLimited, RetryAfter: streamRetryAfter,
		})
		return
	}
	defer g.slots.give(session.ID)
	s := &stream{session: session.ID, role: session.Principal.Role, reader: r, q: newQueue()}
	if r != nil {
		// What the reader may see of the events comes from the state, which
		// the gateway reads anew on every state notice from now on.
		s.key = "r:" + r.Hash
		g.refreshState(c.Request.Context())
	}
	hello := g.hub.join(s, boot, after)
	defer g.hub.leave(s)

	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	out := sseWriter{w: c.Writer, rc: http.NewResponseController(c.Writer)}
	if out.send(sseMessage("", "hello", hello)) != nil {
		return
	}
	ping, stop := g.ticker(pingEvery)
	defer stop()
	ctx := c.Request.Context()
	for {
		items, closed := s.q.take()
		for _, q := range items {
			if out.send(q.msg) != nil {
				return
			}
		}
		if closed {
			return
		}
		select {
		case <-s.q.wake:
		case <-ping:
			if !g.holds(c, s) || out.send([]byte(": ping\n\n")) != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// holds says whether the session of a stream still holds as it did when
// the stream opened: it is there, its role is the same, and so is the set of
// guests a reader sees. A stream whose session does not hold ends, and the
// page opens another, which is checked as any request is and resumes from
// the ring with what the session may see now. Proxmox VE out of reach keeps
// the stream, as it keeps the session.
func (g *Gateway) holds(c *gin.Context, s *stream) bool {
	role, err := g.auth.Recheck(c.Request.Context(), c.Request, s.session)
	if err != nil {
		g.log.Warn().Err(err).Msg("checking the session of a stream")
		return true
	}
	if role != s.role {
		return false
	}
	if s.reader == nil {
		return true
	}
	_, hash, err := g.auth.Visible(c)
	if err != nil {
		g.log.Warn().Err(err).Msg("listing the guests of a reader's stream")
		return true
	}
	return hash == s.reader.Hash
}

var errBadResume = errors.New("Last-Event-ID must be <boot>:<seq>, as the id of an event of the stream has it")

// resumeFrom reads where a browser resumes: the boot and the seq of the last
// event it saw, or nothing.
func resumeFrom(c *gin.Context) (string, uint64, error) {
	id := c.GetHeader("Last-Event-ID")
	if id == "" {
		id = c.Query("lastEventId")
	}
	if id == "" {
		return "", 0, nil
	}
	boot, seq, _ := strings.Cut(id, ":")
	after, err := strconv.ParseUint(seq, 10, 64)
	if err != nil || len(boot) != 16 || strings.Trim(boot, "0123456789abcdef") != "" {
		return "", 0, errBadResume
	}
	return boot, after, nil
}

// sseWriter writes the messages of a stream, each at once.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (s sseWriter) send(msg []byte) error {
	// A writer that cannot take a deadline has no write timeout either.
	_ = s.rc.SetWriteDeadline(time.Now().Add(writeWindow))
	if _, err := s.w.Write(msg); err != nil {
		return err
	}
	return s.rc.Flush()
}

// sseMessage is one message of a stream, with a blank line after it. The
// JSON of data has no line break, so one data line carries it.
func sseMessage(id, event string, data any) []byte {
	payload, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	var b bytes.Buffer
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	b.WriteString("event: " + event + "\ndata: ")
	b.Write(payload)
	b.WriteString("\n\n")
	return b.Bytes()
}

// queued is a notice ready to send, with what coalescing needs of it.
type queued struct {
	kind  string
	event *engine.Event
	gap   *engine.GapNotice
	msg   []byte
}

func eventQueued(ev *engine.Event) queued {
	return queued{kind: engine.NoticeEvent, event: ev, msg: sseMessage(fmt.Sprintf("%s:%d", ev.Boot, ev.Seq), "event", ev)}
}

func gapQueued(g *engine.GapNotice) queued {
	return queued{kind: engine.NoticeGap, gap: g, msg: sseMessage(fmt.Sprintf("%s:%d", g.Boot, g.To), "gap", g)}
}

func noticeQueued(kind string, v any) queued {
	return queued{kind: kind, msg: sseMessage("", kind, v)}
}

func resetQueued(reason string) queued {
	return noticeQueued(engine.NoticeReset, struct {
		Reason string `json:"reason"`
	}{reason})
}

// highVolume says whether the events of a kind come by the thousand in one
// cycle, as the daemon has it.
func highVolume(kind string) bool {
	switch kind {
	case "route", "action", "claim":
		return true
	}
	return false
}

func gapOf(ev engine.Event) engine.GapNotice {
	return engine.GapNotice{Boot: ev.Boot, From: ev.Seq, To: ev.Seq, Count: 1, Level: ev.Level}
}

// joined is one gap that stands for the events of both, a new one.
func joined(a *engine.GapNotice, b engine.GapNotice) *engine.GapNotice {
	if a == nil {
		return &b
	}
	g := *a
	g.From, g.To = min(g.From, b.From), max(g.To, b.To)
	g.Count += b.Count
	if levelRank(b.Level) > levelRank(g.Level) {
		g.Level = b.Level
	}
	if g.Boot == "" {
		g.Boot = b.Boot
	}
	return &g
}

func levelRank(level string) int {
	switch level {
	case "error":
		return 2
	case "warn":
		return 1
	}
	return 0
}

// batched is a replay as the daemon sends a batch: the events of the
// high-volume kinds beyond the first 32, and the gaps after them, as one gap
// where the last of them would be, so that the ids only grow.
func batched(qs []queued) []queued {
	var gap *engine.GapNotice
	member := make([]bool, len(qs))
	last, high := -1, 0
	for i, q := range qs {
		switch {
		case q.event != nil && highVolume(q.event.Kind):
			if high++; high > singleEvents {
				gap, member[i], last = joined(gap, gapOf(*q.event)), true, i
			}
		case q.gap != nil && high >= singleEvents:
			gap, member[i], last = joined(gap, *q.gap), true, i
		}
	}
	if gap == nil {
		return qs
	}
	out := make([]queued, 0, len(qs))
	for i, q := range qs {
		switch {
		case i == last:
			out = append(out, gapQueued(gap))
		case !member[i]:
			out = append(out, q)
		}
	}
	return out
}

// queue holds what waits for a browser. Past 256 notices or 1 MiB it
// collapses, as the daemon's does: the events of the high-volume kinds and
// the gaps become one gap with their highest level, of the state, traffic
// and upstream notices only the newest stays, and every other event stays.
// One that is past them even so is told to start over, and its stream ends.
type queue struct {
	mu     sync.Mutex
	items  []queued
	bytes  int
	closed bool
	wake   chan struct{}
}

func newQueue() *queue { return &queue{wake: make(chan struct{}, 1)} }

func (q *queue) push(qs ...queued) {
	q.mu.Lock()
	for _, it := range qs {
		if q.closed {
			break
		}
		q.items = append(q.items, it)
		q.bytes += len(it.msg)
		if q.full() {
			q.collapse()
		}
		if q.full() {
			q.items = []queued{resetQueued(resetBehind)}
			q.bytes = len(q.items[0].msg)
			q.closed = true
		}
	}
	q.mu.Unlock()
	q.signal()
}

func (q *queue) full() bool { return len(q.items) > queueNotices || q.bytes > queueBytes }

func (q *queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// end ends the stream once what waits is sent: its session ended.
func (q *queue) end() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

// take hands out everything that waits, and says whether the stream ends
// after it.
func (q *queue) take() ([]queued, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items, q.bytes = nil, 0
	return items, q.closed
}

// collapse makes room. The caller holds mu.
func (q *queue) collapse() {
	newest := map[string]int{}
	for i, it := range q.items {
		if it.event == nil && it.gap == nil && it.kind != engine.NoticeReset {
			newest[it.kind] = i
		}
	}
	var gap *engine.GapNotice
	kept := make([]queued, 0, len(q.items))
	for i, it := range q.items {
		switch {
		case it.gap != nil:
			gap = joined(gap, *it.gap)
		case it.event != nil && highVolume(it.event.Kind):
			gap = joined(gap, gapOf(*it.event))
		case it.event != nil, it.kind == engine.NoticeReset, newest[it.kind] == i:
			kept = append(kept, it)
		}
	}
	if gap != nil {
		at := len(kept)
		if i := slices.IndexFunc(kept, func(it queued) bool { return it.event != nil && it.event.Seq > gap.To }); i >= 0 {
			at = i
		}
		kept = slices.Insert(kept, at, gapQueued(gap))
	}
	q.items, q.bytes = kept, 0
	for _, it := range kept {
		q.bytes += len(it.msg)
	}
}
