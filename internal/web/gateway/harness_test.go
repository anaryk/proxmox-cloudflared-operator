package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/auth"
)

var update = flag.Bool("update", false, "write the golden files of the tests")

const (
	testHost   = "pve1:8643"
	testOrigin = "https://" + testHost
	admin      = "alice@pve"
	reader     = "bob@pve"
	reader2    = "carol@pve"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// clock is the time of a test, moved on by the test only.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// call is a request the fake daemon got.
type call struct {
	Method, Path, RawQuery string
	Header                 http.Header
	Body                   []byte
}

func (c call) route() string { return c.Method + " " + c.Path }

// fakeDaemon answers on a unix socket and records every request. A route
// without an answer of its own gets 200 and {}.
type fakeDaemon struct {
	socket string
	srv    *http.Server

	mu      sync.Mutex
	calls   []call
	answers map[string]http.HandlerFunc
	stream  fakeStream
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{
		socket:  filepath.Join(testutil.ShortDir(t), "pco.sock"),
		answers: map[string]http.HandlerFunc{},
		stream:  fakeStream{conns: make(chan *streamConn, 64)},
	}
	d.stream.hello = engine.Hello{Boot: bootA, Version: "v1.3.0", Seq: 0, Digest: "d1", PollInterval: "10s"}
	ln, err := net.Listen("unix", d.socket)
	require.NoError(t, err)
	d.srv = &http.Server{Handler: d, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = d.srv.Serve(ln) }()
	t.Cleanup(d.close)
	return d
}

// close stops the daemon: nothing answers on its socket any more.
func (d *fakeDaemon) close() {
	_ = d.srv.Close()
	_ = os.Remove(d.socket)
}

func (d *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	d.mu.Lock()
	d.calls = append(d.calls, call{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
	h := d.answers[r.Method+" "+r.URL.Path]
	d.mu.Unlock()
	r.Body = io.NopCloser(bytes.NewReader(body))
	switch {
	case h != nil:
		h(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/stream":
		d.stream.serve(w, r)
	default:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte("{}"))
	}
}

// on answers route ("GET /v1/state") with h.
func (d *fakeDaemon) on(route string, h http.HandlerFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.answers[route] = h
}

// answer answers route with v as JSON.
func (d *fakeDaemon) answer(route string, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	d.on(route, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(data)
	})
}

// serveState answers GET /v1/state with st as the daemon does: its digest is
// its ETag, and a matching If-None-Match gets a 304.
func (d *fakeDaemon) serveState(st engine.State) {
	data, err := json.Marshal(st)
	if err != nil {
		panic(err)
	}
	d.on("GET /v1/state", func(w http.ResponseWriter, r *http.Request) {
		if st.Digest != "" {
			tag := `"` + st.Digest + `"`
			w.Header().Set("ETag", tag)
			if r.Header.Get("If-None-Match") == tag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(data)
	})
}

func (d *fakeDaemon) requests() []call {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

// count is how many requests of route came; "" counts them all.
func (d *fakeDaemon) count(route string) int {
	n := 0
	for _, c := range d.requests() {
		if route == "" || c.route() == route {
			n++
		}
	}
	return n
}

// last is the last request of route.
func (d *fakeDaemon) last(t *testing.T, route string) call {
	t.Helper()
	calls := d.requests()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].route() == route {
			return calls[i]
		}
	}
	t.Fatalf("the daemon got no %s", route)
	return call{}
}

func (d *fakeDaemon) forget() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = nil
}

// fakeStream is the daemon's /v1/stream: every connection is handed to the
// test, which sends it messages and drops it.
type fakeStream struct {
	mu     sync.Mutex
	hello  engine.Hello
	refuse int // the next connections that are answered 503
	conns  chan *streamConn
}

type streamConn struct {
	lastEventID string
	send        chan string
	drop        chan struct{}
}

func (s *fakeStream) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	refused := s.refuse > 0
	if refused {
		s.refuse--
	}
	hello := s.hello
	s.mu.Unlock()
	if refused {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"16 streams are open already; try again later","code":"unavailable"}`))
		return
	}
	conn := &streamConn{lastEventID: r.Header.Get("Last-Event-ID"), send: make(chan string, 256), drop: make(chan struct{})}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, sse("", "hello", hello))
	_ = http.NewResponseController(w).Flush()
	s.conns <- conn
	for {
		select {
		case m := <-conn.send:
			_, _ = io.WriteString(w, m)
			_ = http.NewResponseController(w).Flush()
		case <-conn.drop:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *fakeStream) setHello(h engine.Hello) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hello = h
}

func (s *fakeStream) refuseNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse = n
}

// next waits for the next connection of the gateway to the stream.
func (s *fakeStream) next(t *testing.T) *streamConn {
	t.Helper()
	select {
	case c := <-s.conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway did not connect to the stream of the daemon")
	}
	return nil
}

// sse is one message of a stream as the daemon writes it.
func sse(id, event string, v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var b strings.Builder
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	b.WriteString("event: " + event + "\ndata: " + string(data) + "\n\n")
	return b.String()
}

func eventMessage(ev engine.Event) string {
	return sse(fmt.Sprintf("%s:%d", ev.Boot, ev.Seq), "event", ev)
}

func gapMessage(g engine.GapNotice) string {
	return sse(fmt.Sprintf("%s:%d", g.Boot, g.To), "gap", g)
}

const (
	bootA = "9f2c4e1a0b7d3c55"
	bootB = "1b2c3d4e5f607182"
)

// fakePVE is Proxmox VE as auth asks it: the privileges and the guests of
// the user a ticket names.
type fakePVE struct {
	mu    sync.Mutex
	users map[string]*pveUser
}

type pveUser struct {
	privs map[string]bool
	vmids []int
}

func newFakePVE() *fakePVE {
	return &fakePVE{users: map[string]*pveUser{
		admin:   {privs: map[string]bool{"Sys.Audit": true, "Sys.Modify": true}},
		reader:  {privs: map[string]bool{"Sys.Audit": true}},
		reader2: {privs: map[string]bool{"Sys.Audit": true}},
	}}
}

func (f *fakePVE) user(c auth.Credential) (*pveUser, error) {
	parts := strings.SplitN(c.Ticket, ":", 3)
	if len(parts) < 3 {
		return nil, auth.ErrRefused
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[parts[1]]
	if !ok {
		return nil, auth.ErrRefused
	}
	return u, nil
}

func (f *fakePVE) Privileges(_ context.Context, c auth.Credential, _ string) (map[string]bool, error) {
	u, err := f.user(c)
	if err != nil {
		return nil, err
	}
	return u.privs, nil
}

func (f *fakePVE) VisibleVMIDs(_ context.Context, c auth.Credential) ([]int, error) {
	u, err := f.user(c)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if u.privs["Sys.Modify"] {
		return nil, errors.New("an admin's guests are never asked for")
	}
	return slices.Clone(u.vmids), nil
}

// sees sets the guests user may see.
func (f *fakePVE) sees(user string, vmids ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[user].vmids = vmids
}

// grant sets the privileges user has on /.
func (f *fakePVE) grant(user string, privs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[user].privs = map[string]bool{}
	for _, p := range privs {
		f.users[user].privs[p] = true
	}
}

// ticks makes the streams' ticker one the test drives: a stream checks its
// session and pings on every tick it is sent.
func (s *testServer) ticks() chan<- time.Time {
	tick := make(chan time.Time)
	s.gw.ticker = func(time.Duration) (<-chan time.Time, func()) { return tick, func() {} }
	return tick
}

// testServer is pco web with the sessions and the gateway, in front of a
// fake daemon.
type testServer struct {
	t      *testing.T
	daemon *fakeDaemon
	pve    *fakePVE
	clock  *clock
	gw     *Gateway
	h      http.Handler
	logs   *testutil.SyncBuffer
	// sleeps are the waits of the gateway before it connects again, and
	// resume lets it go on.
	sleeps chan time.Duration
	resume chan struct{}
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	s := &testServer{
		t: t, daemon: newFakeDaemon(t), pve: newFakePVE(), clock: &clock{now: t0}, logs: &testutil.SyncBuffer{},
		sleeps: make(chan time.Duration), resume: make(chan struct{}),
	}
	log := zerolog.New(s.logs)
	a := auth.New(s.pve, auth.Config{
		Now:     s.clock.Now,
		Hosts:   func() []string { return []string{testHost} },
		Profile: "host",
		Node:    "pve1",
		Version: "v1.3.0",
		Log:     log,
	})
	s.gw = New(s.daemon.socket, a, log)
	s.gw.now = s.clock.Now
	s.gw.ticker = func(time.Duration) (<-chan time.Time, func()) { return nil, func() {} }
	s.gw.sleep = func(ctx context.Context, d time.Duration) bool {
		select {
		case s.sleeps <- d:
		case <-ctx.Done():
			return false
		}
		select {
		case <-s.resume:
			return true
		case <-ctx.Done():
			return false
		}
	}
	srv, err := web.New(web.Config{
		Assets: fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>pco</title>")}},
		Now:    s.clock.Now,
		Log:    log,
	}, a, s.gw)
	require.NoError(t, err)
	s.h = srv.Handler()
	return s
}

// run starts the shared stream of the gateway until the test ends.
func (s *testServer) run() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.gw.Run(ctx)
	}()
	s.t.Cleanup(func() {
		cancel()
		<-done
	})
}

// browser keeps the cookies and the token of one signed-in browser.
type browser struct {
	s       *testServer
	ticket  string
	session string
	csrf    string
}

func (s *testServer) signIn(user string) *browser {
	s.t.Helper()
	b := &browser{s: s, ticket: "PVE:" + user + ":6700F2A0::c2lnbmF0dXJl"}
	rec := b.send(b.request(http.MethodPost, "/api/session/ticket", "{}"))
	require.Equal(s.t, http.StatusOK, rec.Code, rec.Body.String())
	var answer struct {
		CSRF string `json:"csrf"`
	}
	require.NoError(s.t, json.Unmarshal(rec.Body.Bytes(), &answer))
	b.csrf = answer.CSRF
	require.NotEmpty(s.t, b.session)
	return b
}

// request is what the page sends: a mutating request with its JSON, Origin
// and token.
func (b *browser) request(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, "https://"+testHost+path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.7:51234"
	if method != http.MethodGet && method != http.MethodHead {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		if b.csrf != "" {
			r.Header.Set("Pco-Csrf", b.csrf)
		}
	}
	r.AddCookie(&http.Cookie{Name: "PVEAuthCookie", Value: b.ticket})
	if b.session != "" {
		r.AddCookie(&http.Cookie{Name: "__Host-pco-session", Value: b.session})
	}
	return r
}

func (b *browser) send(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	b.s.h.ServeHTTP(rec, r)
	for _, c := range rec.Result().Cookies() {
		if c.Name == "__Host-pco-session" {
			b.session = c.Value
		}
	}
	return rec
}

func (b *browser) do(method, path, body string) *httptest.ResponseRecorder {
	return b.send(b.request(method, path, body))
}

// requireError checks that rec is the JSON error status with code.
func requireError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	require.Equal(t, code, body.Code)
	require.NotEmpty(t, body.Error)
}

// innermost matches an object or a list that holds no other: the goldens put
// each on one line, so that a list of a thousand routes reads as one.
var innermost = regexp.MustCompile(`[{\[]\n[^{}\[\]]*?\n\s*[}\]]`)

var indentation = regexp.MustCompile(`\n\s*`)

func goldenJSON(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, json.Indent(&buf, data, "", "  "))
	out := innermost.ReplaceAllFunc(buf.Bytes(), func(m []byte) []byte {
		inner := indentation.ReplaceAll(m[1:len(m)-1], []byte(" "))
		return append(append([]byte{m[0]}, bytes.TrimSpace(inner)...), m[len(m)-1])
	})
	return append(out, '\n')
}

// requireGolden compares got with the file of testdata, or writes it with
// -update.
func requireGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, got, 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "run the test with -update to write %s", path)
	require.Equal(t, string(want), string(got), "%s differs; run with -update after checking the change", path)
}

func requireGoldenJSON(t *testing.T, name string, data []byte) {
	t.Helper()
	requireGolden(t, name, goldenJSON(t, data))
}

// populated is the engine's populated state.
func populated(t *testing.T) engine.State {
	t.Helper()
	data, err := os.ReadFile("../../engine/testdata/state_populated.json")
	require.NoError(t, err)
	var st engine.State
	require.NoError(t, json.Unmarshal(data, &st))
	return st
}

// sees is a Visible of the given guests, by VMID as auth makes it.
func sees(vmids ...int) auth.Visible {
	return func(g model.GuestRef) bool { return slices.Contains(vmids, g.VMID) }
}

func guestRoute(host string, vmid int, state planner.RouteState) engine.RouteView {
	ref := model.GuestRef{Kind: model.KindQEMU, VMID: vmid}
	return engine.RouteView{
		RouteStatus: planner.RouteStatus{Hostname: host, Owner: ref.String(), State: state},
		Guest:       &engine.GuestView{GuestRef: ref, Name: fmt.Sprintf("vm-%d", vmid)},
	}
}

func manualRoute(host, id string) engine.RouteView {
	return engine.RouteView{RouteStatus: planner.RouteStatus{Hostname: host, Owner: model.ManualPrefix + id, State: planner.StateActive}}
}

var zoneNames = []string{"example.com", "example.net", "example.org", "example.io", "example.dev", "example.app"}

// largeState is the shape of the large scenario: 1000 routes over 6 zones of
// the guests 1000 to 2999, a route on every second one, 40 of them not
// active, ten hostnames that one guest holds and the next one wants as well,
// and two manual routes.
func largeState() engine.State {
	st := engine.State{Node: "pve1", Digest: "1a2b3c4d5e6f7081", Mode: engine.ModeEnforce, Complete: true}
	for i := range 1000 {
		host := fmt.Sprintf("r%04d.%s", i, zoneNames[i%len(zoneNames)])
		state := planner.StateActive
		if i%25 == 3 {
			state = planner.StateUnreachable
		}
		rt := guestRoute(host, 1000+2*i, state)
		rt.Zone = zoneNames[i%len(zoneNames)]
		st.Routes = append(st.Routes, rt)
		if i%100 == 7 {
			loser := guestRoute(host, 1001+2*i, planner.StateConflict)
			loser.Reason = "hostname is held by " + rt.Owner
			st.Routes = append(st.Routes, loser)
		}
	}
	st.Routes = append(st.Routes, manualRoute("status.example.com", "status"), manualRoute("api.example.net", "api"))
	sortRoutes(st.Routes)
	return st
}

// outageState is the shape of the outage scenario: 1000 routes, 600 of them
// unreachable for one of three reasons.
func outageState() engine.State {
	reasons := []string{"no candidate answers", "the guest is stopped", "the address is not verified"}
	st := engine.State{Node: "pve1", Digest: "0f1e2d3c4b5a6978", Mode: engine.ModeEnforce, Complete: true}
	for i := range 1000 {
		host := fmt.Sprintf("o%04d.%s", i, zoneNames[i%3])
		state := planner.StateActive
		rt := guestRoute(host, 5000+i, state)
		if i < 600 {
			rt.State, rt.Reason = planner.StateUnreachable, reasons[i%3]
		}
		st.Routes = append(st.Routes, rt)
	}
	sortRoutes(st.Routes)
	return st
}

func sortRoutes(routes []engine.RouteView) {
	slices.SortFunc(routes, func(a, b engine.RouteView) int {
		return strings.Compare(a.Hostname+" "+a.Owner, b.Hostname+" "+b.Owner)
	})
}

// largeTraffic is the per-route traffic of the large state: a figure for every
// active route that holds its hostname, and every fiftieth target shared by
// two routes three apart, whose guests are a reader's both or neither.
func largeTraffic(st engine.State) engine.TrafficView {
	tv := engine.TrafficView{At: t0, Interval: "5s", Tunnels: []engine.TunnelTraffic{{
		TunnelID: "00000000-0000-4000-8000-000000000001", Node: "pve1", HAConnections: 4,
		Edges: []connector.Edge{}, RTTMillis: []float64{}, Samples: []engine.TrafficSample{{At: t0, RPS: 38.2}},
	}}, Routes: []engine.RouteTraffic{}}
	n := 0
	for _, r := range st.Routes {
		if r.State != planner.StateActive || !strings.HasPrefix(r.Owner, "qemu/") {
			continue
		}
		target, flows := fmt.Sprintf("10.%d.%d.%d:8080", n/65536, n/256%256, n%256), float64(n%7)/2
		if n%50 == 3 {
			partner := tv.Routes[len(tv.Routes)-3]
			target, flows = partner.Target, partner.FlowsPerSec
		}
		tv.Routes = append(tv.Routes, engine.RouteTraffic{Hostname: r.Hostname, Owner: r.Owner, Target: target, FlowsPerSec: flows})
		n++
	}
	shared := map[string]int{}
	for _, r := range tv.Routes {
		shared[r.Target]++
	}
	for i := range tv.Routes {
		tv.Routes[i].Shared = shared[tv.Routes[i].Target] - 1
	}
	tv.RoutesTotal = len(tv.Routes)
	return tv
}

// readMessage is one message a browser read from its stream.
type readMessage struct {
	id, event, data string
}

func (m readMessage) String() string {
	var b strings.Builder
	if m.id != "" {
		b.WriteString("id: " + m.id + "\n")
	}
	b.WriteString("event: " + m.event + "\ndata: " + m.data + "\n\n")
	return b.String()
}

// streamReader reads the stream of a browser, message by message; a stream
// that was refused has its status and the body of the refusal.
type streamReader struct {
	t        *testing.T
	status   int
	header   http.Header
	refusal  []byte
	body     io.ReadCloser
	messages chan readMessage
	ended    chan struct{}
}

// openStream opens the stream of b over a real connection to srv.
func (b *browser) openStream(srv *httptest.Server, lastEventID string) *streamReader {
	b.s.t.Helper()
	r := b.request(http.MethodGet, auth.StreamPath, "")
	req, err := http.NewRequest(http.MethodGet, srv.URL+auth.StreamPath, nil)
	require.NoError(b.s.t, err)
	req.Host = testHost
	req.Header = r.Header.Clone()
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.s.t.Cleanup(cancel)
	res, err := srv.Client().Do(req.WithContext(ctx))
	require.NoError(b.s.t, err)
	b.s.t.Cleanup(func() { _ = res.Body.Close() })
	sr := &streamReader{t: b.s.t, status: res.StatusCode, header: res.Header, body: res.Body}
	if res.StatusCode != http.StatusOK {
		sr.refusal, err = io.ReadAll(res.Body)
		require.NoError(b.s.t, err)
		return sr
	}
	sr.messages, sr.ended = make(chan readMessage, 4096), make(chan struct{})
	go sr.read()
	return sr
}

func (r *streamReader) close() { _ = r.body.Close() }

func (r *streamReader) read() {
	defer close(r.ended)
	br := bufio.NewReader(r.body)
	var m readMessage
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			if m.event != "" {
				r.messages <- m
			}
			m = readMessage{}
		case strings.HasPrefix(line, ":"):
			r.messages <- readMessage{event: "comment", data: strings.TrimSpace(line[1:])}
		case strings.HasPrefix(line, "id: "):
			m.id = line[len("id: "):]
		case strings.HasPrefix(line, "event: "):
			m.event = line[len("event: "):]
		case strings.HasPrefix(line, "data: "):
			m.data = line[len("data: "):]
		}
	}
}

// next is the next message; the test fails when none comes within 5 s.
func (r *streamReader) next() readMessage {
	r.t.Helper()
	select {
	case m := <-r.messages:
		return m
	case <-time.After(5 * time.Second):
		r.t.Fatal("no message on the stream")
	}
	return readMessage{}
}

// take reads n messages.
func (r *streamReader) take(n int) []readMessage {
	r.t.Helper()
	out := make([]readMessage, 0, n)
	for range n {
		out = append(out, r.next())
	}
	return out
}

// quiet checks that nothing comes within a short while.
func (r *streamReader) quiet() {
	r.t.Helper()
	select {
	case m := <-r.messages:
		r.t.Fatalf("unexpected message %q: %s", m.event, m.data)
	case <-time.After(150 * time.Millisecond):
	}
}

// open says whether the stream is still open.
func (r *streamReader) open() bool {
	select {
	case <-r.ended:
		return false
	default:
		return true
	}
}

func transcript(ms []readMessage) []byte {
	var b strings.Builder
	for _, m := range ms {
		b.WriteString(m.String())
	}
	return []byte(b.String())
}

// eventually waits for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
