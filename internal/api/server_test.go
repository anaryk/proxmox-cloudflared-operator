package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const (
	testToken = "s3cr3t-token-value-0123456789"
	testBoot  = "9f2c4e1a0b7d3c55"
)

var testUID = uint32(os.Getuid())

// syncBuffer is a log destination that handler goroutines may write to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeEngine records what the server asks of it and answers from its fields.
type fakeEngine struct {
	mu       sync.Mutex
	state    engine.State
	events   []engine.Event
	view     engine.CredentialView
	applied  engine.ApplyResult // what Apply answers
	err      error
	hook     func(ctx context.Context) error // runs first in Apply
	calls    []string
	since    time.Time
	ctx      context.Context
	triggers int
	panics   bool
	addPanic string            // AddCredential panics with this text
	interval time.Duration     // the poll interval; 10s when zero
	query    engine.EventQuery // of the last request for the events

	// Subscribe answers with notices and hello, or fails with subErr.
	notices chan engine.Notice
	hello   engine.Hello
	subErr  error

	creds     []engine.CredentialView
	claims    []engine.ClaimView
	approvals []engine.ApprovalView
	steps     []doctor.Step
	findings  []doctor.Finding
}

func (f *fakeEngine) record(ctx context.Context, call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	f.ctx = ctx
}

func (f *fakeEngine) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeEngine) State() engine.State {
	if f.panics {
		panic("state exploded")
	}
	f.record(context.Background(), "state")
	return f.state
}

func (f *fakeEngine) lastSince() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.since
}

func (f *fakeEngine) triggered() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.triggers
}

func (f *fakeEngine) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeEngine) setApplied(res engine.ApplyResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = res
}

func (f *fakeEngine) failure() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// QueryEvents answers with the events that match the query; the limit and
// the seq are the engine's to apply.
func (f *fakeEngine) QueryEvents(q engine.EventQuery) ([]engine.Event, error) {
	f.record(context.Background(), "events")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.since, f.query = q.Since, q
	var out []engine.Event
	for _, ev := range f.events {
		if q.Match(ev) {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (f *fakeEngine) lastQuery() engine.EventQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.query
}

func (f *fakeEngine) Boot() string { return testBoot }

func (f *fakeEngine) Subscribe(ctx context.Context, boot string, after uint64) (<-chan engine.Notice, engine.Hello, error) {
	f.record(ctx, fmt.Sprintf("subscribe:%s:%d", boot, after))
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subErr != nil {
		return nil, engine.Hello{}, f.subErr
	}
	return f.notices, f.hello, nil
}

func (f *fakeEngine) Trigger() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggers++
}

func (f *fakeEngine) Apply(ctx context.Context, confirmDeletes bool, offer string) (engine.ApplyResult, error) {
	f.record(ctx, fmt.Sprintf("apply:%t:%s", confirmDeletes, offer))
	if f.hook != nil {
		if err := f.hook(ctx); err != nil {
			return engine.ApplyResult{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applied, f.err
}

func (f *fakeEngine) Adopt(ctx context.Context, name string) error {
	f.record(ctx, "adopt:"+name)
	return f.failure()
}

func (f *fakeEngine) RotateTunnel(ctx context.Context, account string) (engine.TunnelRotation, error) {
	f.record(ctx, "rotate:"+account)
	if err := f.failure(); err != nil {
		return engine.TunnelRotation{}, err
	}
	return engine.TunnelRotation{Tunnel: "pco-abc123", TunnelID: "00000000-0000-4000-8000-000000000001", Account: "acc1"}, nil
}

func (f *fakeEngine) AddCredential(ctx context.Context, label, token string) (engine.CredentialView, error) {
	f.record(ctx, fmt.Sprintf("add:%s:%s", label, token))
	if f.addPanic != "" {
		panic(f.addPanic)
	}
	return f.view, f.failure()
}

func (f *fakeEngine) CheckCredential(ctx context.Context, id string, deep bool) (engine.CredentialView, error) {
	f.record(ctx, fmt.Sprintf("check:%s:%t", id, deep))
	return f.view, f.failure()
}

func (f *fakeEngine) RemoveCredential(ctx context.Context, id string) error {
	f.record(ctx, "remove:"+id)
	return f.failure()
}

func (f *fakeEngine) Credentials() ([]engine.CredentialView, error) {
	f.record(context.Background(), "credentials")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creds, f.err
}

func (f *fakeEngine) Claims() ([]engine.ClaimView, error) {
	f.record(context.Background(), "claims")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims, f.err
}

func (f *fakeEngine) ResolveClaim(ctx context.Context, hostname, owner string) error {
	f.record(ctx, "resolve:"+hostname+":"+owner)
	return f.failure()
}

func (f *fakeEngine) Approvals() ([]engine.ApprovalView, error) {
	f.record(context.Background(), "approvals")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.approvals, f.err
}

func (f *fakeEngine) ApproveGuest(ctx context.Context, owner, identity string) (engine.Approval, error) {
	f.record(ctx, "approve:"+owner+":"+identity)
	if err := f.failure(); err != nil {
		return engine.Approval{}, err
	}
	return engine.Approval{Owner: owner, Identity: "uuid:1", Mode: "approve"}, nil
}

func (f *fakeEngine) RevokeGuest(ctx context.Context, owner string) error {
	f.record(ctx, "revoke:"+owner)
	return f.failure()
}

func (f *fakeEngine) Diagnose(ctx context.Context, hostname string) ([]doctor.Step, error) {
	f.record(ctx, "diagnose:"+hostname)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.steps, f.err
}

func (f *fakeEngine) Doctor(ctx context.Context) []doctor.Finding {
	f.record(ctx, "doctor")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.findings
}

func (f *fakeEngine) PollInterval() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.interval == 0 {
		return 10 * time.Second
	}
	return f.interval
}

func (f *fakeEngine) lastCtx() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ctx
}

// newServer returns a server that checks peers as the platform does.
func newServer(e *fakeEngine) *Server {
	return New(e, "1.2.3", []uint32{testUID}, zerolog.Nop())
}

// requestFrom builds a request that came in on the socket from the peer with
// uid. A POST carries the JSON content type.
func requestFrom(uid uint32, method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	return req.WithContext(withPeerUID(req.Context(), uid))
}

// request builds a request from an allowed peer.
func request(method, target, body string) *http.Request {
	return requestFrom(testUID, method, target, body)
}

func send(s *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func do(s *Server, method, target, body string) *httptest.ResponseRecorder {
	return send(s, request(method, target, body))
}

// errorAnswer is the body of an error answer.
type errorAnswer struct {
	Message    string
	Code       string
	Credential json.RawMessage // nil when the body has none
}

// parseError reads an error answer strictly: it carries the message and the
// code, may carry a credential, and nothing else.
func parseError(t *testing.T, rec *httptest.ResponseRecorder) errorAnswer {
	t.Helper()
	require.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json"), rec.Header().Get("Content-Type"))
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw), rec.Body.String())
	for key := range raw {
		require.Contains(t, []string{"error", "code", "credential"}, key, rec.Body.String())
	}
	require.Contains(t, raw, "error", rec.Body.String())
	require.Contains(t, raw, "code", rec.Body.String())
	var a errorAnswer
	require.NoError(t, json.Unmarshal(raw["error"], &a.Message))
	require.NoError(t, json.Unmarshal(raw["code"], &a.Code))
	a.Credential = raw["credential"]
	return a
}

func errorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return parseError(t, rec).Message
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return parseError(t, rec).Code
}

func testState() engine.State {
	return engine.State{
		At:            time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
		Digest:        "5e0c1f7a92b4d3e8",
		Mode:          "enforce",
		Complete:      true,
		Problems:      []string{"something is off"},
		WriterVerdict: "ok",
		Credentials: []engine.CredentialView{
			{ID: "abc12345", Label: "main", Kind: "scoped"},
			{ID: "def67890", Label: "spare", Kind: "scoped"},
		},
	}
}

func testClaims() []engine.ClaimView {
	return []engine.ClaimView{{
		Hostname: "www.example.com", Holder: "qemu/101", Since: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), State: "conflict",
		Waiting: []engine.ClaimantView{{Owner: "qemu/102", Since: time.Date(2026, 3, 4, 5, 7, 0, 0, time.UTC)}},
	}}
}

// shortDir returns a directory whose path is short enough for a unix socket.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pco")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// needRoot skips a test that only root can run, or fails it where
// PCO_REQUIRE_LINUX_TESTS=1 says the environment exists to run it.
func needRoot(t *testing.T, why string) {
	t.Helper()
	if os.Getuid() == 0 {
		return
	}
	if os.Getenv("PCO_REQUIRE_LINUX_TESTS") == "1" {
		t.Fatalf("%s: this environment is meant to run the test as root", why)
	}
	t.Skip(why)
}

// socketPath returns where a test's socket goes: in a directory named pco,
// which Serve makes, inside a directory of the test's own.
func socketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(shortDir(t), "pco", "pco.sock")
}

// pcoDir makes the socket directory ahead of Serve and returns it.
func pcoDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(shortDir(t), "pco")
	require.NoError(t, os.Mkdir(dir, 0o750))
	return dir
}

// serve runs Serve until the returned stop function is called, which returns
// the error Serve ended with.
func serve(t *testing.T, s *Server, socket string) (stop func() error) {
	t.Helper()
	return serveAs(t, s, socket, os.Getgid())
}

func serveAs(t *testing.T, s *Server, socket string, gid int) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan struct{})
	s.onListening = func() { close(ready) }
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, socket, gid) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("Serve ended before it listened: %v", err)
	}
	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() {
			cancel()
			result = <-done
		})
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

// serveFails runs Serve where it is expected to give up before it listens, and
// returns its error. A server that got to listen would end at once instead of
// hanging.
func serveFails(t *testing.T, s *Server, socket string) error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.onListening = cancel
	err := s.Serve(ctx, socket, os.Getgid())
	require.Error(t, err, "Serve was expected to fail")
	return err
}

func TestEveryClientMethodOverTheSocket(t *testing.T) {
	st := testState()
	view := engine.CredentialView{ID: "abc12345", Label: "main", Kind: "scoped"}
	events := []engine.Event{{At: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), Level: "info", Kind: "route", Subject: "a.example.com", Message: "up"}}
	f := &fakeEngine{
		state: st, view: view, events: events,
		claims:    testClaims(),
		approvals: []engine.ApprovalView{{Owner: "qemu/101", Identity: "uuid:101", Current: "uuid:101", Matches: true}},
		steps:     []doctor.Step{{Name: "route", Level: doctor.LevelOK, Detail: "qemu/101 holds it"}},
		findings:  []doctor.Finding{{Check: "mode", Level: doctor.LevelWarn, Detail: "observe-only", Fix: "pco apply"}},
	}
	socket := socketPath(t)
	stop := serve(t, newServer(f), socket)
	c := apiclient.New(socket)
	ctx := t.Context()

	v, err := c.Version(ctx)
	require.NoError(t, err)
	require.Equal(t, "1.2.3", v)

	got, err := c.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, st, got)

	since := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
	evs, err := c.Events(ctx, since)
	require.NoError(t, err)
	require.Equal(t, events, evs)
	require.True(t, since.Equal(f.lastSince()))
	_, err = c.Events(ctx, time.Time{})
	require.NoError(t, err)
	require.True(t, f.lastSince().IsZero())

	require.NoError(t, c.Sync(ctx))
	require.Equal(t, 1, f.triggered())
	accepted := engine.ApplyResult{LeftObserveOnly: true, Accepted: []engine.Waiting{
		{Kind: "stale-zone", Subject: "example.net", Detail: "zone example.net is no longer listed by credential cred1", Items: []string{}},
	}}
	f.setApplied(accepted)
	applied, err := c.Apply(ctx, true, "0123456789abcdef")
	require.NoError(t, err)
	require.Equal(t, accepted, applied)
	require.NoError(t, c.Adopt(ctx, "www.example.com"))

	added, err := c.AddCredential(ctx, "main", testToken)
	require.NoError(t, err)
	require.Equal(t, view, added)
	checked, err := c.CheckCredential(ctx, "abc12345", true)
	require.NoError(t, err)
	require.Equal(t, view, checked)
	require.NoError(t, c.RemoveCredential(ctx, "abc12345"))

	gotClaims, err := c.Claims(ctx)
	require.NoError(t, err)
	require.Equal(t, f.claims, gotClaims)
	require.NoError(t, c.ResolveClaim(ctx, "www.example.com", "qemu/102"))
	gotApprovals, err := c.Approvals(ctx)
	require.NoError(t, err)
	require.Equal(t, f.approvals, gotApprovals)
	approved, err := c.ApproveGuest(ctx, "qemu/101", "uuid:101")
	require.NoError(t, err)
	require.Equal(t, engine.Approval{Owner: "qemu/101", Identity: "uuid:1", Mode: "approve"}, approved)
	require.NoError(t, c.RevokeGuest(ctx, "qemu/101"))
	steps, err := c.Diagnose(ctx, "www.example.com")
	require.NoError(t, err)
	require.Equal(t, f.steps, steps)
	findings, err := c.Doctor(ctx)
	require.NoError(t, err)
	require.Equal(t, f.findings, findings)

	require.Equal(t, []string{
		"state", "state", "events", "events", "apply:true:0123456789abcdef", "adopt:www.example.com",
		"add:main:" + testToken, "check:abc12345:true", "remove:abc12345",
		"claims", "resolve:www.example.com:qemu/102", "approvals", "approve:qemu/101:uuid:101", "revoke:qemu/101",
		"diagnose:www.example.com", "doctor",
	}, f.called())

	// The sentinels survive the trip.
	f.setErr(fmt.Errorf("%w: credential abc12345 still manages 1 record", engine.ErrRefused))
	err = c.RemoveCredential(ctx, "abc12345")
	require.ErrorIs(t, err, engine.ErrRefused)
	require.EqualError(t, err, "refused: credential abc12345 still manages 1 record")

	require.NoError(t, stop())
}

func TestARefusedTokenReachesTheClientWithItsReport(t *testing.T) {
	f := &fakeEngine{
		view: refusedView(),
		err:  fmt.Errorf("%w: the token cannot be used: dns.write on example.com: grant Zone > DNS > Edit on example.com", engine.ErrInvalid),
	}
	socket := socketPath(t)
	serve(t, newServer(f), socket)

	view, err := apiclient.New(socket).AddCredential(t.Context(), "main", testToken)

	require.ErrorIs(t, err, engine.ErrInvalid)
	require.Contains(t, err.Error(), "grant Zone > DNS > Edit on example.com")
	require.NotContains(t, err.Error(), testToken)
	require.Equal(t, f.view.Label, view.Label)
	require.Equal(t, f.view.Report.Checks, view.Report.Checks)
	require.False(t, view.Report.Usable)
}

func TestServeCreatesTheDirectory(t *testing.T) {
	socket := socketPath(t)
	serve(t, newServer(&fakeEngine{}), socket)

	info, err := os.Stat(filepath.Dir(socket))
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.Equal(t, fs.FileMode(0o750), info.Mode().Perm())
}

func TestTheSocketIsGroupWritableOnly(t *testing.T) {
	socket := socketPath(t)
	serve(t, newServer(&fakeEngine{}), socket)

	info, err := os.Lstat(socket)
	require.NoError(t, err)
	require.Equal(t, fs.ModeSocket, info.Mode().Type())
	require.Equal(t, fs.FileMode(0o660), info.Mode().Perm())
}

func TestShutdownIsCleanAndRemovesTheSocket(t *testing.T) {
	socket := socketPath(t)
	stop := serve(t, newServer(&fakeEngine{}), socket)

	require.NoError(t, stop())

	_, err := os.Lstat(socket)
	require.ErrorIs(t, err, fs.ErrNotExist)
	_, err = apiclient.New(socket).Version(t.Context())
	require.ErrorContains(t, err, "cannot reach the pco daemon")
}

func TestShutdownLetsARunningRequestFinish(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	shuttingDown := make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	f := &fakeEngine{hook: func(context.Context) error {
		close(started)
		<-release
		return nil
	}}
	s := newServer(f)
	s.onShutdown = func() { close(shuttingDown) }
	socket := socketPath(t)
	stop := serve(t, s, socket)
	t.Cleanup(letGo)

	applied := make(chan error, 1)
	go func() {
		_, err := apiclient.New(socket).Apply(t.Context(), false, "")
		applied <- err
	}()
	select {
	case <-started:
	case err := <-applied:
		t.Fatalf("the request ended before the engine saw it: %v", err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()

	// The request is released only once the shutdown has begun, so that a
	// server that closed its connections instead of waiting would fail here.
	select {
	case <-shuttingDown:
	case err := <-stopped:
		t.Fatalf("Serve returned while a request was running: %v", err)
	}
	letGo()

	require.NoError(t, <-applied)
	require.NoError(t, <-stopped)
}

func TestShutdownGivesUpOnAStuckRequest(t *testing.T) {
	started := make(chan struct{})
	f := &fakeEngine{hook: func(ctx context.Context) error {
		close(started)
		select { // the first ends only when the connection is closed
		case <-ctx.Done():
		case <-t.Context().Done():
		}
		return ctx.Err()
	}}
	s := newServer(f)
	s.shutdownTimeout = 20 * time.Millisecond
	socket := socketPath(t)
	stop := serve(t, s, socket)

	applied := make(chan error, 1)
	go func() {
		_, err := apiclient.New(socket).Apply(t.Context(), false, "")
		applied <- err
	}()
	select {
	case <-started:
	case err := <-applied:
		t.Fatalf("the request ended before the engine saw it: %v", err)
	}

	err := stop()

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Error(t, <-applied)
	_, statErr := os.Lstat(socket)
	require.ErrorIs(t, statErr, fs.ErrNotExist)
}

func TestServeTimeouts(t *testing.T) {
	srv := newServer(&fakeEngine{}).httpServer()

	require.Equal(t, 5*time.Second, srv.ReadHeaderTimeout)
	require.GreaterOrEqual(t, srv.WriteTimeout, 70*time.Second)
	require.Equal(t, time.Minute, srv.IdleTimeout)
	require.Zero(t, srv.ReadTimeout, "a read timeout would cancel the context of a long apply")
}

func TestTheHTTPServerLogsThroughTheLogger(t *testing.T) {
	var logged syncBuffer
	s := New(&fakeEngine{}, "v", []uint32{testUID}, zerolog.New(&logged))

	s.httpServer().ErrorLog.Printf("http: Accept error: %v", "boom")

	require.Contains(t, logged.String(), "http: Accept error: boom")
	require.Contains(t, logged.String(), `"level":"warn"`)
}

func TestServeSaysWhenPeersAreNotChecked(t *testing.T) {
	for _, tt := range []struct {
		name  string
		check bool
		want  int
	}{
		{"checked", true, 0},
		{"unchecked", false, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logged syncBuffer
			s := New(&fakeEngine{}, "v", []uint32{testUID}, zerolog.New(&logged))
			s.checkPeers = tt.check
			stop := serve(t, s, socketPath(t))
			require.NoError(t, stop())

			require.Equal(t, tt.want, strings.Count(logged.String(), "peer credentials are not checked"))
			if tt.want > 0 {
				require.Contains(t, logged.String(), `"level":"warn"`)
			}
		})
	}
}
