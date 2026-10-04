package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const testToken = "s3cr3t-token-value"

// seen is what the fake daemon received.
type seen struct {
	Method      string
	Path        string
	Query       string
	ContentType string
	Accept      string
	Body        string
}

type daemon struct {
	mu   sync.Mutex
	reqs []seen
}

func (d *daemon) last(t *testing.T) seen {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	require.NotEmpty(t, d.reqs)
	return d.reqs[len(d.reqs)-1]
}

// shortDir returns a directory whose path is short enough for a unix socket.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pco")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakeDaemon serves handler on a unix socket and records every request.
func fakeDaemon(t *testing.T, handler http.HandlerFunc) (*daemon, string) {
	t.Helper()
	socket := filepath.Join(shortDir(t), "pco.sock")
	ln, err := net.Listen("unix", socket)
	require.NoError(t, err)
	d := &daemon{}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.reqs = append(d.reqs, seen{
			Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery,
			ContentType: r.Header.Get("Content-Type"), Accept: r.Header.Get("Accept"), Body: string(body),
		})
		d.mu.Unlock()
		handler(w, r)
	}))
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	return d, socket
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestRequests(t *testing.T) {
	since := time.Date(2026, 3, 4, 7, 0, 0, 500_000_000, time.FixedZone("CEST", 2*3600))
	view := engine.CredentialView{ID: "abc12345", Label: "main", Kind: "scoped"}
	viewJSON, err := json.Marshal(view)
	require.NoError(t, err)

	for _, tt := range []struct {
		name   string
		reply  string
		status int
		call   func(c *Client) error
		want   seen
	}{
		{
			name: "version", reply: `{"version":"1.2.3"}`, status: 200,
			call: func(c *Client) error {
				v, err := c.Version(t.Context())
				require.Equal(t, "1.2.3", v)
				return err
			},
			want: seen{Method: "GET", Path: "/v1/version"},
		},
		{
			name: "status", reply: `{"mode":"enforce","complete":true}`, status: 200,
			call: func(c *Client) error {
				st, err := c.Status(t.Context())
				require.Equal(t, "enforce", st.Mode)
				require.True(t, st.Complete)
				return err
			},
			want: seen{Method: "GET", Path: "/v1/state"},
		},
		{
			name: "events", reply: `[{"at":"2026-03-04T05:06:07Z","level":"info","kind":"route","subject":"a","message":"up"}]`, status: 200,
			call: func(c *Client) error {
				evs, err := c.Events(t.Context(), since)
				require.Len(t, evs, 1)
				require.Equal(t, "up", evs[0].Message)
				return err
			},
			want: seen{Method: "GET", Path: "/v1/events", Query: "since=2026-03-04T05%3A00%3A00.5Z"},
		},
		{
			name: "events from the start", reply: `[]`, status: 200,
			call: func(c *Client) error {
				evs, err := c.Events(t.Context(), time.Time{})
				require.Empty(t, evs)
				return err
			},
			want: seen{Method: "GET", Path: "/v1/events"},
		},
		{
			name: "sync", reply: `{}`, status: 202,
			call: func(c *Client) error { return c.Sync(t.Context()) },
			want: seen{Method: "POST", Path: "/v1/sync", ContentType: "application/json"},
		},
		{
			name: "apply", reply: `{"leftObserveOnly":true,"accepted":[]}`, status: 200,
			call: func(c *Client) error {
				res, err := c.Apply(t.Context(), false, "")
				require.Equal(t, engine.ApplyResult{LeftObserveOnly: true, Accepted: []engine.Waiting{}}, res)
				return err
			},
			want: seen{Method: "POST", Path: "/v1/apply", ContentType: "application/json", Body: `{"confirmDeletes":false}`},
		},
		{
			name: "apply with a confirmation",
			reply: `{"leftObserveOnly":false,"accepted":[{"kind":"stale-zone","subject":"example.net",` +
				`"detail":"zone example.net is no longer listed by credential cred1","items":[]}]}`,
			status: 200,
			call: func(c *Client) error {
				res, err := c.Apply(t.Context(), true, "0123456789abcdef")
				require.Equal(t, engine.ApplyResult{Accepted: []engine.Waiting{
					{Kind: "stale-zone", Subject: "example.net", Detail: "zone example.net is no longer listed by credential cred1", Items: []string{}},
				}}, res)
				return err
			},
			want: seen{Method: "POST", Path: "/v1/apply", ContentType: "application/json", Body: `{"confirmDeletes":true,"offer":"0123456789abcdef"}`},
		},
		{
			name: "adopt", reply: `{}`, status: 200,
			call: func(c *Client) error { return c.Adopt(t.Context(), "www.example.com") },
			want: seen{Method: "POST", Path: "/v1/adopt", ContentType: "application/json", Body: `{"name":"www.example.com"}`},
		},
		{
			name: "rotate a tunnel", reply: `{"tunnel":"pco-abc123","tunnelId":"00000000-0000-4000-8000-000000000001","accountId":"acc1"}`, status: 200,
			call: func(c *Client) error {
				res, err := c.RotateTunnel(t.Context(), "acc1")
				require.Equal(t, engine.TunnelRotation{Tunnel: "pco-abc123", TunnelID: "00000000-0000-4000-8000-000000000001", Account: "acc1"}, res)
				return err
			},
			want: seen{Method: "POST", Path: "/v1/tunnels/rotate", ContentType: "application/json", Body: `{"account":"acc1"}`},
		},
		{
			name: "add credential", reply: string(viewJSON), status: 201,
			call: func(c *Client) error {
				v, err := c.AddCredential(t.Context(), "main", testToken)
				require.Equal(t, view, v)
				return err
			},
			want: seen{Method: "POST", Path: "/v1/credentials", ContentType: "application/json", Body: `{"label":"main","token":"` + testToken + `"}`},
		},
		{
			name: "check credential", reply: string(viewJSON), status: 200,
			call: func(c *Client) error {
				v, err := c.CheckCredential(t.Context(), "abc12345", true)
				require.Equal(t, view, v)
				return err
			},
			want: seen{Method: "POST", Path: "/v1/credentials/abc12345/check", ContentType: "application/json", Body: `{"deep":true}`},
		},
		{
			name: "remove credential", reply: `{}`, status: 200,
			call: func(c *Client) error { return c.RemoveCredential(t.Context(), "abc12345") },
			want: seen{Method: "DELETE", Path: "/v1/credentials/abc12345"},
		},
		{
			name: "an id is escaped", reply: `{}`, status: 200,
			call: func(c *Client) error { return c.RemoveCredential(t.Context(), "a/b?c") },
			want: seen{Method: "DELETE", Path: "/v1/credentials/a%2Fb%3Fc"},
		},
		{
			name: "claims", reply: `[{"hostname":"www.example.com","holder":"qemu/101","since":"2026-03-04T05:06:07Z","state":"serving","waiting":[]}]`, status: 200,
			call: func(c *Client) error {
				claims, err := c.Claims(t.Context())
				require.Equal(t, []engine.ClaimView{{
					Hostname: "www.example.com", Holder: "qemu/101", Since: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
					State: "serving", Waiting: []engine.ClaimantView{},
				}}, claims)
				return err
			},
			want: seen{Method: "GET", Path: "/v1/claims"},
		},
		{
			name: "resolve a claim", reply: `{}`, status: 200,
			call: func(c *Client) error { return c.ResolveClaim(t.Context(), "www.example.com", "qemu/102") },
			want: seen{Method: "POST", Path: "/v1/claims/resolve", ContentType: "application/json", Body: `{"hostname":"www.example.com","owner":"qemu/102"}`},
		},
		{
			name: "approvals", reply: `[{"owner":"qemu/101","identity":"uuid:1","matches":false}]`, status: 200,
			call: func(c *Client) error {
				approvals, err := c.Approvals(t.Context())
				require.Equal(t, []engine.ApprovalView{{Owner: "qemu/101", Identity: "uuid:1"}}, approvals)
				return err
			},
			want: seen{Method: "GET", Path: "/v1/approvals"},
		},
		{
			name: "approve a guest", reply: `{"owner":"qemu/101","identity":"uuid:101","mode":"tag"}`, status: 200,
			call: func(c *Client) error {
				a, err := c.ApproveGuest(t.Context(), "qemu/101", "")
				require.Equal(t, engine.Approval{Owner: "qemu/101", Identity: "uuid:101", Mode: "tag"}, a)
				return err
			},
			want: seen{Method: "POST", Path: "/v1/guests/approve", ContentType: "application/json", Body: `{"owner":"qemu/101"}`},
		},
		{
			name: "approve a guest as it was shown", reply: `{"owner":"qemu/101","identity":"uuid:101","mode":"approve"}`, status: 200,
			call: func(c *Client) error {
				_, err := c.ApproveGuest(t.Context(), "qemu/101", "uuid:101")
				return err
			},
			want: seen{Method: "POST", Path: "/v1/guests/approve", ContentType: "application/json", Body: `{"owner":"qemu/101","identity":"uuid:101"}`},
		},
		{
			name: "revoke a guest", reply: `{}`, status: 200,
			call: func(c *Client) error { return c.RevokeGuest(t.Context(), "qemu/101") },
			want: seen{Method: "POST", Path: "/v1/guests/revoke", ContentType: "application/json", Body: `{"owner":"qemu/101"}`},
		},
		{
			name: "diagnose", reply: `[{"name":"route","level":"ok","detail":"fine"}]`, status: 200,
			call: func(c *Client) error {
				steps, err := c.Diagnose(t.Context(), "www.example.com")
				require.Equal(t, []doctor.Step{{Name: "route", Level: doctor.LevelOK, Detail: "fine"}}, steps)
				return err
			},
			want: seen{Method: "GET", Path: "/v1/diagnose", Query: "hostname=www.example.com"},
		},
		{
			name: "a hostname is escaped", reply: `[]`, status: 200,
			call: func(c *Client) error {
				_, err := c.Diagnose(t.Context(), "a&b=c d")
				return err
			},
			want: seen{Method: "GET", Path: "/v1/diagnose", Query: "hostname=a%26b%3Dc+d"},
		},
		{
			name: "doctor", reply: `[{"check":"mode","level":"warn","detail":"observe-only","fix":"pco apply"}]`, status: 200,
			call: func(c *Client) error {
				findings, err := c.Doctor(t.Context())
				require.Equal(t, []doctor.Finding{{Check: "mode", Level: doctor.LevelWarn, Detail: "observe-only", Fix: "pco apply"}}, findings)
				return err
			},
			want: seen{Method: "GET", Path: "/v1/doctor"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, socket := fakeDaemon(t, reply(tt.status, tt.reply))

			require.NoError(t, tt.call(New(socket)))

			tt.want.Accept = "application/json"
			require.Equal(t, tt.want, d.last(t))
		})
	}
}

func TestTheTokenTravelsInTheBodyOnly(t *testing.T) {
	d, socket := fakeDaemon(t, reply(201, `{"id":"abc12345"}`))

	_, err := New(socket).AddCredential(t.Context(), "main", testToken)

	require.NoError(t, err)
	got := d.last(t)
	require.Empty(t, got.Query)
	require.NotContains(t, got.Path, testToken)
	require.Contains(t, got.Body, testToken)
}

// result is what one client method returned.
type result struct {
	name string
	err  error
}

// everyCall runs every client method once.
func everyCall(t *testing.T, c *Client) []result {
	t.Helper()
	ctx := t.Context()
	var out []result
	add := func(name string, err error) { out = append(out, result{name, err}) }
	_, err := c.Version(ctx)
	add("version", err)
	_, err = c.Status(ctx)
	add("status", err)
	_, err = c.Events(ctx, time.Time{})
	add("events", err)
	add("sync", c.Sync(ctx))
	_, err = c.Apply(ctx, false, "")
	add("apply", err)
	add("adopt", c.Adopt(ctx, "www.example.com"))
	_, err = c.RotateTunnel(ctx, "acc1")
	add("rotate", err)
	_, err = c.AddCredential(ctx, "main", testToken)
	add("add", err)
	_, err = c.CheckCredential(ctx, "abc", false)
	add("check", err)
	add("remove", c.RemoveCredential(ctx, "abc"))
	_, err = c.Claims(ctx)
	add("claims", err)
	add("resolve", c.ResolveClaim(ctx, "www.example.com", "qemu/102"))
	_, err = c.Approvals(ctx)
	add("approvals", err)
	_, err = c.ApproveGuest(ctx, "qemu/101", "")
	add("approve", err)
	add("revoke", c.RevokeGuest(ctx, "qemu/101"))
	_, err = c.Diagnose(ctx, "www.example.com")
	add("diagnose", err)
	_, err = c.Doctor(ctx)
	add("doctor", err)
	return out
}

func TestErrorsComeBackAsGoErrors(t *testing.T) {
	for _, tt := range []struct {
		name     string
		status   int
		code     string
		message  string
		sentinel error
	}{
		{"invalid", 400, "invalid", "invalid request: the label is empty", engine.ErrInvalid},
		{"not found", 404, "not_found", `not found: no credential "abc"`, engine.ErrNotFound},
		{"refused", 409, "refused", "refused: credential abc still manages 2 records", engine.ErrRefused},
		{"bad body", 400, "invalid", "the request body is not valid JSON", engine.ErrInvalid},
		{"unsupported media", 415, "unsupported_media_type", "the content type must be application/json", nil},
		{"too large", 413, "too_large", "the request body is too large", nil},
		{"server error", 500, "internal", "reading the settings: disk gone", nil},
		{"unavailable", 503, "unavailable", "the operation timed out", nil},
		{"the code decides, not the status", 400, "refused", "refused: a guard said no", engine.ErrRefused},
		{"the code decides, even on a server error", 500, "not_found", "not found: no credential", engine.ErrNotFound},
		{"no code, no sentinel", 400, "", "something is wrong", nil},
		{"no code on a conflict", 409, "", "something is wrong", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			answer := map[string]string{"error": tt.message}
			if tt.code != "" {
				answer["code"] = tt.code
			}
			body, err := json.Marshal(answer)
			require.NoError(t, err)
			_, socket := fakeDaemon(t, reply(tt.status, string(body)))

			for _, r := range everyCall(t, New(socket)) {
				require.EqualError(t, r.err, tt.message, r.name)
				for _, s := range []error{engine.ErrInvalid, engine.ErrNotFound, engine.ErrRefused} {
					if errors.Is(tt.sentinel, s) {
						require.ErrorIs(t, r.err, s, r.name)
					} else {
						require.NotErrorIs(t, r.err, s, r.name)
					}
				}
			}
		})
	}
}

func TestAnUnknownRequestIsAVersionMismatch(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"unknown route", 404, `{"error":"no such route","code":"no_route"}`},
		{"unknown method", 405, `{"error":"method not allowed","code":"method_not_allowed"}`},
		{"a daemon that sends no codes", 404, `{"error":"no such route"}`},
		{"not json at all", 404, "404 page not found"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, socket := fakeDaemon(t, reply(tt.status, tt.body))

			for _, r := range everyCall(t, New(socket)) {
				require.EqualError(t, r.err, "the pco daemon at "+socket+" does not know this request: is it a different version than this pco?", r.name)
				require.ErrorIs(t, r.err, ErrUnknownRequest, r.name)
				require.NotErrorIs(t, r.err, engine.ErrNotFound, r.name)
				require.NotErrorIs(t, r.err, engine.ErrInvalid, r.name)
			}
		})
	}
}

func TestRawAnswersKeepTheBytesTheDaemonSent(t *testing.T) {
	// The key order is not the one of the structs the client decodes into.
	const state = `{"writerVerdict":"ok","mode":"observe","extra":{"b":1,"a":[1,2]}}`
	const creds = `[{"label":"main","id":"abc12345","kind":"scoped","checked":false}]`

	for _, tt := range []struct {
		name  string
		reply string
		path  string
		call  func(c *Client) (json.RawMessage, error)
	}{
		{"status", state, "/v1/state", func(c *Client) (json.RawMessage, error) { return c.StatusRaw(t.Context()) }},
		{"credentials", creds, "/v1/credentials", func(c *Client) (json.RawMessage, error) { return c.CredentialsRaw(t.Context()) }},
		{"claims", `[{"state":"held","hostname":"a"}]`, "/v1/claims", func(c *Client) (json.RawMessage, error) { return c.ClaimsRaw(t.Context()) }},
		{"approvals", `[{"matches":true,"owner":"qemu/1"}]`, "/v1/approvals", func(c *Client) (json.RawMessage, error) { return c.ApprovalsRaw(t.Context()) }},
		{"doctor", `[{"level":"ok","check":"mode"}]`, "/v1/doctor", func(c *Client) (json.RawMessage, error) { return c.DoctorRaw(t.Context()) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, socket := fakeDaemon(t, reply(200, tt.reply))

			got, err := tt.call(New(socket))

			require.NoError(t, err)
			require.JSONEq(t, tt.reply, string(got))
			require.Equal(t, tt.reply, string(got))
			require.Equal(t, seen{Method: "GET", Path: tt.path, Accept: "application/json"}, d.last(t))
		})
	}
}

func TestCredentials(t *testing.T) {
	_, socket := fakeDaemon(t, reply(200, `[{"id":"abc12345","label":"main","kind":"scoped","checked":false}]`))

	got, err := New(socket).Credentials(t.Context())

	require.NoError(t, err)
	require.Equal(t, []engine.CredentialView{{ID: "abc12345", Label: "main", Kind: "scoped"}}, got)
}

func TestAnAnswerWithoutAnErrorBody(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"plain text", 502, "bad gateway\n", "the pco daemon answered 502 Bad Gateway"},
		{"empty", 500, "", "the pco daemon answered 500 Internal Server Error"},
		{"json without error", 500, `{"oops":true}`, "the pco daemon answered 500 Internal Server Error"},
		{"an error without a message", 400, `{"code":"invalid"}`, "the pco daemon answered 400 Bad Request"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, socket := fakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})

			_, err := New(socket).Version(t.Context())

			require.EqualError(t, err, tt.want)
		})
	}
}

func TestAForbiddenAnswerSaysToRunAsRoot(t *testing.T) {
	for _, body := range []string{`{"error":"not allowed","code":"forbidden"}`, `{"error":"not allowed"}`, ``} {
		_, socket := fakeDaemon(t, reply(403, body))

		_, err := New(socket).Apply(t.Context(), false, "")

		require.EqualError(t, err, "permission denied on "+socket+": run as root")
		require.ErrorIs(t, err, fs.ErrPermission)
	}
}

func TestARefusedTokenComesBackWithItsReport(t *testing.T) {
	view := engine.CredentialView{
		Label: "main", Kind: "scoped",
		Report: credentials.Report{Checks: []credentials.Check{
			{Capability: credentials.CapDNSWrite, Scope: "example.com", Detail: "grant Zone > DNS > Edit on example.com"},
		}},
	}
	const message = "invalid request: the token cannot be used: dns.write on example.com: grant Zone > DNS > Edit on example.com"
	body, err := json.Marshal(map[string]any{"error": message, "code": "invalid", "credential": view})
	require.NoError(t, err)
	_, socket := fakeDaemon(t, reply(400, string(body)))

	got, err := New(socket).AddCredential(t.Context(), "main", testToken)

	require.EqualError(t, err, message)
	require.ErrorIs(t, err, engine.ErrInvalid)
	require.Equal(t, view, got)
}

func TestAFailureWithoutAReportReturnsAZeroView(t *testing.T) {
	_, socket := fakeDaemon(t, reply(400, `{"error":"invalid request: the label is empty","code":"invalid"}`))

	got, err := New(socket).AddCredential(t.Context(), "", testToken)

	require.ErrorIs(t, err, engine.ErrInvalid)
	require.Equal(t, engine.CredentialView{}, got)
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	d, socket := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v1/elsewhere", http.StatusFound)
	})
	c := New(socket)

	_, err := c.Version(t.Context())
	require.EqualError(t, err, "the pco daemon answered 302 Found")
	_, err = c.Apply(t.Context(), true, "0123456789abcdef")
	require.EqualError(t, err, "the pco daemon answered 302 Found")

	d.mu.Lock()
	defer d.mu.Unlock()
	require.Len(t, d.reqs, 2, "each call is sent once and its redirect is not followed")
	for _, r := range d.reqs {
		require.NotEqual(t, "/v1/elsewhere", r.Path)
	}
}

func TestSuccessWithAnUnreadableBody(t *testing.T) {
	_, socket := fakeDaemon(t, reply(200, `not json`))

	_, err := New(socket).Status(t.Context())

	require.ErrorContains(t, err, "decoding")
}

func TestAnOversizedAnswerIsRefused(t *testing.T) {
	_, socket := fakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"version":"`)
		chunk := strings.Repeat("a", 1<<20)
		for range maxResponse/len(chunk) + 1 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, `"}`)
	})

	_, err := New(socket).Version(t.Context())

	require.ErrorContains(t, err, "too large")
}

func TestTheDaemonIsNotThere(t *testing.T) {
	missing := filepath.Join(shortDir(t), "nope.sock")
	stale := filepath.Join(shortDir(t), "stale.sock")
	ln, err := net.Listen("unix", stale)
	require.NoError(t, err)
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, ln.Close())

	for _, tt := range []struct{ name, socket string }{
		{"no socket", missing},
		{"nothing listens on it", stale},
		{"no directory", filepath.Join(missing, "deeper.sock")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, r := range everyCall(t, New(tt.socket)) {
				require.EqualError(t, r.err, "cannot reach the pco daemon at "+tt.socket+": is it running?", r.name)
			}
		})
	}
}

func TestASocketNobodyMayOpen(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root opens everything")
	}
	dir := shortDir(t)
	socket := filepath.Join(dir, "pco.sock")
	ln, err := net.Listen("unix", socket)
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	require.NoError(t, os.Chmod(socket, 0))

	_, err = New(socket).Version(t.Context())

	require.EqualError(t, err, "permission denied on "+socket+": run as root")
}

func TestTimeouts(t *testing.T) {
	_, socket := fakeDaemon(t, reply(200, `{"version":"x","id":"a"}`))
	c := New(socket)
	rec := &deadlines{next: c.http.Transport}
	c.http.Transport = rec

	_, _ = c.Version(t.Context())
	require.InDelta(t, 10, rec.last().Seconds(), 1, "version")
	_, _ = c.Status(t.Context())
	require.InDelta(t, 10, rec.last().Seconds(), 1, "status")
	_, _ = c.Events(t.Context(), time.Time{})
	require.InDelta(t, 10, rec.last().Seconds(), 1, "events")
	_ = c.Sync(t.Context())
	require.InDelta(t, 10, rec.last().Seconds(), 1, "sync")

	_, _ = c.Apply(t.Context(), false, "")
	require.InDelta(t, 60, rec.last().Seconds(), 1, "apply")
	_, _ = c.CheckCredential(t.Context(), "a", false)
	require.InDelta(t, 60, rec.last().Seconds(), 1, "check")
	_, _ = c.AddCredential(t.Context(), "l", "t")
	require.InDelta(t, 60, rec.last().Seconds(), 1, "add")
	_ = c.Adopt(t.Context(), "a.example.com")
	require.InDelta(t, 60, rec.last().Seconds(), 1, "adopt")
	_ = c.RemoveCredential(t.Context(), "a")
	require.InDelta(t, 60, rec.last().Seconds(), 1, "remove")

	_, _ = c.Claims(t.Context())
	require.InDelta(t, 10, rec.last().Seconds(), 1, "claims")
	_, _ = c.Approvals(t.Context())
	require.InDelta(t, 10, rec.last().Seconds(), 1, "approvals")
	_ = c.ResolveClaim(t.Context(), "a.example.com", "qemu/1")
	require.InDelta(t, 60, rec.last().Seconds(), 1, "resolve")
	_, _ = c.ApproveGuest(t.Context(), "qemu/1", "")
	require.InDelta(t, 60, rec.last().Seconds(), 1, "approve")
	_ = c.RevokeGuest(t.Context(), "qemu/1")
	require.InDelta(t, 60, rec.last().Seconds(), 1, "revoke")
	_, _ = c.Diagnose(t.Context(), "a.example.com")
	require.InDelta(t, 60, rec.last().Seconds(), 1, "diagnose")
	_, _ = c.Doctor(t.Context())
	require.InDelta(t, 60, rec.last().Seconds(), 1, "doctor")
}

// deadlines records how long each request has to finish.
type deadlines struct {
	next http.RoundTripper
	mu   sync.Mutex
	left []time.Duration
}

func (d *deadlines) RoundTrip(r *http.Request) (*http.Response, error) {
	if dl, ok := r.Context().Deadline(); ok {
		d.mu.Lock()
		d.left = append(d.left, time.Until(dl))
		d.mu.Unlock()
	}
	return d.next.RoundTrip(r)
}

func (d *deadlines) last() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.left) == 0 {
		return 0
	}
	return d.left[len(d.left)-1]
}

func TestAStuckDaemonTimesOut(t *testing.T) {
	release := make(chan struct{})
	_, socket := fakeDaemon(t, func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(func() { close(release) })
	c := New(socket)
	c.short = 20 * time.Millisecond

	_, err := c.Version(t.Context())

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, socket)
}

func TestTheCallersContextEndsTheCall(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	_, socket := fakeDaemon(t, func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- New(socket).Sync(ctx) }()
	<-started

	cancel()

	require.ErrorIs(t, <-result, context.Canceled)
}
