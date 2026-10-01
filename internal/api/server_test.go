package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
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
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const testToken = "s3cr3t-token-value"

var testUID = uint32(os.Getuid())

// fakeEngine records what the server asks of it and answers from its fields.
type fakeEngine struct {
	mu       sync.Mutex
	state    engine.State
	events   []engine.Event
	view     engine.CredentialView
	err      error
	hook     func(ctx context.Context) error // runs first in Apply
	calls    []string
	since    time.Time
	ctx      context.Context
	triggers int
	panics   bool
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

func (f *fakeEngine) failure() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fakeEngine) Events(since time.Time) []engine.Event {
	f.record(context.Background(), "events")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.since = since
	return f.events
}

func (f *fakeEngine) Trigger() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggers++
}

func (f *fakeEngine) Apply(ctx context.Context, confirmDeletes bool) error {
	f.record(ctx, fmt.Sprintf("apply:%t", confirmDeletes))
	if f.hook != nil {
		if err := f.hook(ctx); err != nil {
			return err
		}
	}
	return f.failure()
}

func (f *fakeEngine) Adopt(ctx context.Context, name string) error {
	f.record(ctx, "adopt:"+name)
	return f.failure()
}

func (f *fakeEngine) AddCredential(ctx context.Context, label, token string) (engine.CredentialView, error) {
	f.record(ctx, fmt.Sprintf("add:%s:%s", label, token))
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

func (f *fakeEngine) lastCtx() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ctx
}

func newServer(e *fakeEngine) *Server {
	return New(e, "1.2.3", []uint32{testUID}, zerolog.Nop())
}

// request builds a request that came in on the socket from an allowed peer.
// A POST carries the JSON content type.
func request(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	return req.WithContext(withPeerUID(req.Context(), testUID))
}

func send(s *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func do(s *Server, method, target, body string) *httptest.ResponseRecorder {
	return send(s, request(method, target, body))
}

func errorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	require.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json"), rec.Header().Get("Content-Type"))
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	require.Len(t, body, 1, rec.Body.String())
	require.Contains(t, body, "error")
	return body["error"]
}

func testState() engine.State {
	return engine.State{
		At:            time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
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

func TestVersion(t *testing.T) {
	rec := do(newServer(&fakeEngine{}), http.MethodGet, "/v1/version", "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"version":"1.2.3"}`, rec.Body.String())
}

func TestStateIsTheEngineState(t *testing.T) {
	st := testState()
	rec := do(newServer(&fakeEngine{state: st}), http.MethodGet, "/v1/state", "")

	require.Equal(t, http.StatusOK, rec.Code)
	want, err := json.Marshal(st)
	require.NoError(t, err)
	require.JSONEq(t, string(want), rec.Body.String())
}

func TestEvents(t *testing.T) {
	events := []engine.Event{
		{At: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), Level: "info", Kind: "route", Subject: "a.example.com", Message: "up"},
	}
	since := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)

	for _, tt := range []struct {
		name      string
		query     string
		wantSince time.Time
	}{
		{"no since means everything", "", time.Time{}},
		{"utc", "?since=2026-03-04T05:00:00Z", since},
		{"fraction", "?since=2026-03-04T05:00:00.5Z", since.Add(500 * time.Millisecond)},
		{"offset", "?since=2026-03-04T07:00:00%2B02:00", since},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{events: events}
			rec := do(newServer(f), http.MethodGet, "/v1/events"+tt.query, "")

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			want, err := json.Marshal(events)
			require.NoError(t, err)
			require.JSONEq(t, string(want), rec.Body.String())
			require.True(t, tt.wantSince.Equal(f.lastSince()), "since was %v, want %v", f.lastSince(), tt.wantSince)
		})
	}

	t.Run("no events is an empty list", func(t *testing.T) {
		rec := do(newServer(&fakeEngine{}), http.MethodGet, "/v1/events", "")

		require.Equal(t, http.StatusOK, rec.Code)
		require.JSONEq(t, `[]`, rec.Body.String())
	})

	for _, bad := range []string{"yesterday", "2026-03-04", "1709528400", "", "2026-03-04T05:00:00"} {
		t.Run("malformed "+bad, func(t *testing.T) {
			f := &fakeEngine{events: events}
			rec := do(newServer(f), http.MethodGet, "/v1/events?since="+bad, "")

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, errorMessage(t, rec), "since")
			require.Empty(t, f.called())
		})
	}
}

func TestSyncTriggersACycle(t *testing.T) {
	f := &fakeEngine{}
	rec := do(newServer(f), http.MethodPost, "/v1/sync", "")

	require.Equal(t, http.StatusAccepted, rec.Code)
	require.JSONEq(t, `{}`, rec.Body.String())
	require.Equal(t, 1, f.triggered())
}

func TestApply(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		call string
	}{
		{"confirming", `{"confirmDeletes":true}`, "apply:true"},
		{"not confirming", `{"confirmDeletes":false}`, "apply:false"},
		{"empty object", `{}`, "apply:false"},
		{"no body", ``, "apply:false"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{}
			rec := do(newServer(f), http.MethodPost, "/v1/apply", tt.body)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.JSONEq(t, `{}`, rec.Body.String())
			require.Equal(t, []string{tt.call}, f.called())
		})
	}
}

func TestAdopt(t *testing.T) {
	f := &fakeEngine{}
	rec := do(newServer(f), http.MethodPost, "/v1/adopt", `{"name":"www.example.com"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{}`, rec.Body.String())
	require.Equal(t, []string{"adopt:www.example.com"}, f.called())
}

func TestCredentialsAreListedFromTheState(t *testing.T) {
	st := testState()
	rec := do(newServer(&fakeEngine{state: st}), http.MethodGet, "/v1/credentials", "")

	require.Equal(t, http.StatusOK, rec.Code)
	want, err := json.Marshal(st.Credentials)
	require.NoError(t, err)
	require.JSONEq(t, string(want), rec.Body.String())

	t.Run("none is an empty list", func(t *testing.T) {
		rec := do(newServer(&fakeEngine{}), http.MethodGet, "/v1/credentials", "")

		require.Equal(t, http.StatusOK, rec.Code)
		require.JSONEq(t, `[]`, rec.Body.String())
	})
}

func TestAddCredentialAnswersWithTheViewOnly(t *testing.T) {
	view := engine.CredentialView{ID: "abc12345", Label: "main", Kind: "scoped"}
	f := &fakeEngine{view: view}
	rec := do(newServer(f), http.MethodPost, "/v1/credentials", `{"label":"main","token":"`+testToken+`"}`)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	want, err := json.Marshal(view)
	require.NoError(t, err)
	require.JSONEq(t, string(want), rec.Body.String())
	require.NotContains(t, rec.Body.String(), testToken)
	require.Equal(t, []string{"add:main:" + testToken}, f.called())
}

func TestTheTokenIsReadFromTheBodyOnly(t *testing.T) {
	f := &fakeEngine{}
	rec := do(newServer(f), http.MethodPost, "/v1/credentials?token="+testToken, `{"label":"main"}`)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Equal(t, []string{"add:main:"}, f.called())
}

func TestCheckCredential(t *testing.T) {
	view := engine.CredentialView{ID: "abc12345", Label: "main", Kind: "scoped"}
	for _, tt := range []struct {
		name string
		body string
		call string
	}{
		{"deep", `{"deep":true}`, "check:abc12345:true"},
		{"shallow", `{"deep":false}`, "check:abc12345:false"},
		{"no body", ``, "check:abc12345:false"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{view: view}
			rec := do(newServer(f), http.MethodPost, "/v1/credentials/abc12345/check", tt.body)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			want, err := json.Marshal(view)
			require.NoError(t, err)
			require.JSONEq(t, string(want), rec.Body.String())
			require.Equal(t, []string{tt.call}, f.called())
		})
	}
}

func TestRemoveCredential(t *testing.T) {
	f := &fakeEngine{}
	rec := do(newServer(f), http.MethodDelete, "/v1/credentials/abc12345", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{}`, rec.Body.String())
	require.Equal(t, []string{"remove:abc12345"}, f.called())
}

var engineRoutes = []struct{ name, method, target, body string }{
	{"apply", http.MethodPost, "/v1/apply", `{}`},
	{"adopt", http.MethodPost, "/v1/adopt", `{"name":"www.example.com"}`},
	{"add credential", http.MethodPost, "/v1/credentials", `{"label":"main","token":"tok"}`},
	{"check credential", http.MethodPost, "/v1/credentials/abc12345/check", `{}`},
	{"remove credential", http.MethodDelete, "/v1/credentials/abc12345", ""},
}

func TestEngineErrorsAreMapped(t *testing.T) {
	for _, tt := range []struct {
		name    string
		err     error
		status  int
		message string
	}{
		{"invalid", fmt.Errorf("%w: the label is empty", engine.ErrInvalid), http.StatusBadRequest, "invalid request: the label is empty"},
		{"not found", fmt.Errorf("%w: no credential %q", engine.ErrNotFound, "abc12345"), http.StatusNotFound, `not found: no credential "abc12345"`},
		{"refused", fmt.Errorf("%w: credential abc12345 still manages 2 records", engine.ErrRefused), http.StatusConflict, "refused: credential abc12345 still manages 2 records"},
		{"deadline", fmt.Errorf("checking the token: %w", context.DeadlineExceeded), http.StatusServiceUnavailable, "the operation timed out"},
		{"cancelled", fmt.Errorf("checking the token: %w", context.Canceled), http.StatusServiceUnavailable, "the operation was cancelled"},
		{"anything else", errors.New("reading the settings: disk gone"), http.StatusInternalServerError, "reading the settings: disk gone"},
	} {
		for _, rt := range engineRoutes {
			t.Run(tt.name+" on "+rt.name, func(t *testing.T) {
				rec := do(newServer(&fakeEngine{err: tt.err}), rt.method, rt.target, rt.body)

				require.Equal(t, tt.status, rec.Code, rec.Body.String())
				require.Equal(t, tt.message, errorMessage(t, rec))
			})
		}
	}
}

func TestErrorsDoNotEchoTheSubmittedToken(t *testing.T) {
	const padded = "  " + testToken + "\n"
	for _, tt := range []struct {
		name   string
		err    error
		status int
	}{
		{"invalid", fmt.Errorf("%w: %s is not a token", engine.ErrInvalid, testToken), http.StatusBadRequest},
		{"refused", fmt.Errorf("%w: cloudflare rejected %q", engine.ErrRefused, testToken), http.StatusConflict},
		{"anything else", fmt.Errorf("calling cloudflare with %s: boom", testToken), http.StatusInternalServerError},
		{"as submitted", fmt.Errorf("calling cloudflare with %s: boom", padded), http.StatusInternalServerError},
		{"twice", fmt.Errorf("%s then %s", testToken, testToken), http.StatusInternalServerError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logged bytes.Buffer
			s := New(&fakeEngine{err: tt.err}, "v", []uint32{testUID}, zerolog.New(&logged).Level(zerolog.DebugLevel))
			body, err := json.Marshal(map[string]string{"label": "main", "token": padded})
			require.NoError(t, err)

			rec := send(s, request(http.MethodPost, "/v1/credentials", string(body)))

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			require.NotContains(t, rec.Body.String(), testToken)
			require.Contains(t, errorMessage(t, rec), "[redacted]")
			require.NotContains(t, logged.String(), testToken)
		})
	}
}

func TestUnknownRoutesAndMethods(t *testing.T) {
	f := &fakeEngine{}
	s := newServer(f)

	for _, tt := range []struct {
		name, method, target string
		status               int
		allow                string
	}{
		{"unknown path", http.MethodGet, "/v1/nope", http.StatusNotFound, ""},
		{"root", http.MethodGet, "/", http.StatusNotFound, ""},
		{"trailing slash", http.MethodGet, "/v1/state/", http.StatusNotFound, ""},
		{"unknown post", http.MethodPost, "/v1/nope", http.StatusNotFound, ""},
		{"post to a get route", http.MethodPost, "/v1/state", http.StatusMethodNotAllowed, "GET"},
		{"get to a post route", http.MethodGet, "/v1/apply", http.StatusMethodNotAllowed, "POST"},
		{"put on credentials", http.MethodPut, "/v1/credentials", http.StatusMethodNotAllowed, ""},
		{"delete on the collection", http.MethodDelete, "/v1/credentials", http.StatusMethodNotAllowed, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.target, nil) // no content type: the route is not found first
			rec := send(s, req.WithContext(withPeerUID(req.Context(), testUID)))

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			require.NotEmpty(t, errorMessage(t, rec))
			if tt.allow != "" {
				require.Equal(t, tt.allow, rec.Header().Get("Allow"))
			}
		})
	}
	require.Empty(t, f.called())
}

func TestRequestBodies(t *testing.T) {
	big := `{"label":"` + strings.Repeat("a", 1<<20) + `"}`
	for _, tt := range []struct {
		name        string
		target      string
		contentType string
		body        string
		status      int
		contains    string
	}{
		{"no content type", "/v1/apply", "", `{}`, http.StatusUnsupportedMediaType, "application/json"},
		{"wrong content type", "/v1/apply", "text/plain", `{}`, http.StatusUnsupportedMediaType, "application/json"},
		{"form content type", "/v1/apply", "application/x-www-form-urlencoded", `confirmDeletes=true`, http.StatusUnsupportedMediaType, "application/json"},
		{"charset is fine", "/v1/apply", "application/json; charset=utf-8", `{}`, http.StatusOK, ""},
		{"upper case is fine", "/v1/apply", "Application/JSON", `{}`, http.StatusOK, ""},
		{"unknown field", "/v1/apply", "application/json", `{"confirmDeletes":true,"force":true}`, http.StatusBadRequest, "force"},
		{"not json", "/v1/apply", "application/json", `confirmDeletes`, http.StatusBadRequest, "JSON"},
		{"truncated", "/v1/adopt", "application/json", `{"name":`, http.StatusBadRequest, "JSON"},
		{"wrong type", "/v1/apply", "application/json", `{"confirmDeletes":"yes"}`, http.StatusBadRequest, "confirmDeletes"},
		{"not an object", "/v1/apply", "application/json", `[true]`, http.StatusBadRequest, "object"},
		{"two objects", "/v1/apply", "application/json", `{} {}`, http.StatusBadRequest, "after"},
		{"trailing junk", "/v1/apply", "application/json", `{}x`, http.StatusBadRequest, "after"},
		{"adopt needs a body", "/v1/adopt", "application/json", ``, http.StatusBadRequest, "empty"},
		{"add needs a body", "/v1/credentials", "application/json", ``, http.StatusBadRequest, "empty"},
		{"too large", "/v1/credentials", "application/json", big, http.StatusRequestEntityTooLarge, "too large"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{}
			req := httptest.NewRequest(http.MethodPost, tt.target, strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			rec := send(newServer(f), req.WithContext(withPeerUID(req.Context(), testUID)))

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			if tt.status == http.StatusOK {
				return
			}
			require.Contains(t, errorMessage(t, rec), tt.contains)
			require.Empty(t, f.called())
		})
	}

	t.Run("a body just under the limit is read", func(t *testing.T) {
		f := &fakeEngine{}
		pad := strings.Repeat(" ", 1<<20-len(`{"deep":true}`))
		rec := do(newServer(f), http.MethodPost, "/v1/credentials/abc12345/check", `{"deep":true}`+pad)

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, []string{"check:abc12345:true"}, f.called())
	})

	t.Run("get and delete need no content type", func(t *testing.T) {
		s := newServer(&fakeEngine{})
		for _, tt := range []struct{ method, target string }{
			{http.MethodGet, "/v1/version"},
			{http.MethodGet, "/v1/state"},
			{http.MethodDelete, "/v1/credentials/abc12345"},
		} {
			req := httptest.NewRequest(tt.method, tt.target, nil)
			rec := send(s, req.WithContext(withPeerUID(req.Context(), testUID)))
			require.Equal(t, http.StatusOK, rec.Code, tt.method+" "+tt.target)
		}
	})
}

func TestTheRequestContextReachesTheEngine(t *testing.T) {
	type key struct{}
	f := &fakeEngine{}
	for _, rt := range engineRoutes {
		t.Run(rt.name, func(t *testing.T) {
			req := request(rt.method, rt.target, rt.body)
			req = req.WithContext(context.WithValue(req.Context(), key{}, "marker"))

			rec := send(newServer(f), req)

			require.Less(t, rec.Code, 300, rec.Body.String())
			require.Equal(t, "marker", f.lastCtx().Value(key{}))
		})
	}
}

func TestRequestLogCarriesNoBodiesOrQueries(t *testing.T) {
	var logged bytes.Buffer
	s := New(&fakeEngine{}, "v", []uint32{testUID}, zerolog.New(&logged).Level(zerolog.DebugLevel))

	rec := send(s, request(http.MethodPost, "/v1/credentials?token=querysecret&x=1", `{"label":"main","token":"bodysecret"}`))

	require.Equal(t, http.StatusCreated, rec.Code)
	var line map[string]any
	require.NoError(t, json.Unmarshal(logged.Bytes(), &line), logged.String())
	require.Equal(t, "debug", line["level"])
	require.Equal(t, "POST", line["method"])
	require.Equal(t, "/v1/credentials", line["path"])
	require.EqualValues(t, http.StatusCreated, line["status"])
	require.Contains(t, line, "duration")
	for _, secret := range []string{"querysecret", "bodysecret", "token=", "x=1"} {
		require.NotContains(t, logged.String(), secret)
	}
}

func TestRequestLogIsDebugOnly(t *testing.T) {
	var logged bytes.Buffer
	s := New(&fakeEngine{}, "v", []uint32{testUID}, zerolog.New(&logged).Level(zerolog.InfoLevel))

	do(s, http.MethodGet, "/v1/version", "")

	require.Empty(t, logged.String())
}

func TestAPanicIsLoggedAndAnswered(t *testing.T) {
	var logged bytes.Buffer
	s := New(&fakeEngine{panics: true}, "v", []uint32{testUID}, zerolog.New(&logged).Level(zerolog.DebugLevel))

	rec := send(s, request(http.MethodGet, "/v1/state?q=querysecret", ""))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "internal error", errorMessage(t, rec))
	require.Contains(t, logged.String(), "state exploded")
	require.Contains(t, logged.String(), `"level":"error"`)
	require.NotContains(t, logged.String(), "querysecret")

	t.Run("and the server goes on", func(t *testing.T) {
		rec := send(s, request(http.MethodGet, "/v1/version", ""))
		require.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestServerErrorsAreLogged(t *testing.T) {
	var logged bytes.Buffer
	s := New(&fakeEngine{err: errors.New("disk gone")}, "v", []uint32{testUID}, zerolog.New(&logged).Level(zerolog.InfoLevel))

	rec := do(s, http.MethodPost, "/v1/apply", `{}`)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, logged.String(), "disk gone")
	require.Contains(t, logged.String(), `"level":"error"`)
}

// shortDir returns a directory whose path is short enough for a unix socket.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pco")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// serve runs Serve until the returned stop function is called, which returns
// the error Serve ended with.
func serve(t *testing.T, s *Server, socket string) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan struct{})
	s.onListening = func() { close(ready) }
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, socket, os.Getgid()) }()
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

func TestEveryClientMethodOverTheSocket(t *testing.T) {
	st := testState()
	view := engine.CredentialView{ID: "abc12345", Label: "main", Kind: "scoped"}
	events := []engine.Event{{At: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), Level: "info", Kind: "route", Subject: "a.example.com", Message: "up"}}
	f := &fakeEngine{state: st, view: view, events: events}
	socket := filepath.Join(shortDir(t), "pco.sock")
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
	require.NoError(t, c.Apply(ctx, true))
	require.NoError(t, c.Adopt(ctx, "www.example.com"))

	added, err := c.AddCredential(ctx, "main", testToken)
	require.NoError(t, err)
	require.Equal(t, view, added)
	checked, err := c.CheckCredential(ctx, "abc12345", true)
	require.NoError(t, err)
	require.Equal(t, view, checked)
	require.NoError(t, c.RemoveCredential(ctx, "abc12345"))

	require.Equal(t, []string{
		"state", "events", "events", "apply:true", "adopt:www.example.com",
		"add:main:" + testToken, "check:abc12345:true", "remove:abc12345",
	}, f.called())

	// The sentinels survive the trip.
	f.setErr(fmt.Errorf("%w: credential abc12345 still manages 1 record", engine.ErrRefused))
	err = c.RemoveCredential(ctx, "abc12345")
	require.ErrorIs(t, err, engine.ErrRefused)
	require.EqualError(t, err, "refused: credential abc12345 still manages 1 record")

	require.NoError(t, stop())
}

func TestServeCreatesTheDirectory(t *testing.T) {
	socket := filepath.Join(shortDir(t), "run", "pco", "pco.sock")
	serve(t, newServer(&fakeEngine{}), socket)

	info, err := os.Stat(filepath.Dir(socket))
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.Equal(t, fs.FileMode(0o750), info.Mode().Perm())
}

func TestTheSocketIsGroupWritableOnly(t *testing.T) {
	socket := filepath.Join(shortDir(t), "pco.sock")
	serve(t, newServer(&fakeEngine{}), socket)

	info, err := os.Lstat(socket)
	require.NoError(t, err)
	require.Equal(t, fs.ModeSocket, info.Mode().Type())
	require.Equal(t, fs.FileMode(0o660), info.Mode().Perm())
}

func TestAStaleSocketIsReplaced(t *testing.T) {
	socket := filepath.Join(shortDir(t), "pco.sock")
	old, err := net.Listen("unix", socket)
	require.NoError(t, err)
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, old.Close())
	info, err := os.Lstat(socket)
	require.NoError(t, err, "the dead listener leaves its file behind")
	require.Equal(t, fs.ModeSocket, info.Mode().Type())

	serve(t, newServer(&fakeEngine{}), socket)

	v, err := apiclient.New(socket).Version(t.Context())
	require.NoError(t, err)
	require.Equal(t, "1.2.3", v)
}

func TestServeRefusesToRemoveWhatIsNotASocket(t *testing.T) {
	dir := shortDir(t)
	for name, create := range map[string]func(path string) error{
		"file":    func(path string) error { return os.WriteFile(path, []byte("keep me"), 0o600) },
		"symlink": func(path string) error { return os.Symlink(dir, path) },
		"dir":     func(path string) error { return os.Mkdir(path, 0o700) },
	} {
		t.Run(name, func(t *testing.T) {
			socket := filepath.Join(dir, name+".sock")
			require.NoError(t, create(socket))
			before, err := os.Lstat(socket)
			require.NoError(t, err)

			// A server that got to listen would end at once instead of hanging.
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s := newServer(&fakeEngine{})
			s.onListening = cancel
			err = s.Serve(ctx, socket, os.Getgid())

			require.ErrorContains(t, err, "not a socket")
			after, err := os.Lstat(socket)
			require.NoError(t, err, "the path must still exist")
			require.True(t, os.SameFile(before, after))
		})
	}
}

func TestServeDoesNotStealALiveSocket(t *testing.T) {
	socket := filepath.Join(shortDir(t), "pco.sock")
	stop := serve(t, newServer(&fakeEngine{}), socket)

	// A second server that got to listen would end at once instead of hanging.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	second := newServer(&fakeEngine{})
	second.onListening = cancel
	err := second.Serve(ctx, socket, os.Getgid())

	require.ErrorContains(t, err, "already listening")
	v, err := apiclient.New(socket).Version(t.Context())
	require.NoError(t, err, "the first server still answers")
	require.Equal(t, "1.2.3", v)
	require.NoError(t, stop())
}

func TestShutdownIsCleanAndRemovesTheSocket(t *testing.T) {
	socket := filepath.Join(shortDir(t), "pco.sock")
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
	f := &fakeEngine{hook: func(context.Context) error {
		close(started)
		<-release
		return nil
	}}
	socket := filepath.Join(shortDir(t), "pco.sock")
	stop := serve(t, newServer(f), socket)

	applied := make(chan error, 1)
	go func() { applied <- apiclient.New(socket).Apply(t.Context(), false) }()
	<-started
	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()
	close(release)

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
	socket := filepath.Join(shortDir(t), "pco.sock")
	stop := serve(t, s, socket)

	applied := make(chan error, 1)
	go func() { applied <- apiclient.New(socket).Apply(t.Context(), false) }()
	<-started

	err := stop()

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Error(t, <-applied)
	_, statErr := os.Lstat(socket)
	require.ErrorIs(t, statErr, fs.ErrNotExist)
}

func TestServeTimeouts(t *testing.T) {
	s := newServer(&fakeEngine{})
	srv := s.httpServer()

	require.Equal(t, 5*time.Second, srv.ReadHeaderTimeout)
	require.GreaterOrEqual(t, srv.WriteTimeout, 70*time.Second)
	require.Zero(t, srv.ReadTimeout, "a read timeout would cancel the context of a long apply")
}
