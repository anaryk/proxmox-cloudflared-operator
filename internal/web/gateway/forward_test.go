package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/wire"
)

// headerNames are the names of the headers a request carried, leaving out
// Content-Length, which frames the body and is no header of the browser's.
func headerNames(h http.Header) []string {
	var names []string
	for name := range h {
		if name != "Content-Length" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func TestTheForwardedRequestCarriesExactlyItsThreeHeaders(t *testing.T) {
	s := newTestServer(t)
	s.daemon.serveState(populated(t))
	b := s.signIn(admin)
	browserHeaders := func(r *http.Request) *http.Request {
		r.Header.Set("Authorization", "PVEAPIToken=root@pam!x=00000000-0000-0000-0000-000000000000")
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		r.Header.Set("Pco-Actor", "root (cli)")
		r.Header.Set("If-None-Match", `"from-the-browser"`)
		r.Header.Set("User-Agent", "Mozilla/5.0")
		r.Header.Set("Accept-Encoding", "gzip")
		r.Header.Set("Pco-Background", "1")
		r.Header.Set("Last-Event-ID", bootA+":3")
		return r
	}

	rec := b.send(browserHeaders(b.request(http.MethodPost, "/api/v1/apply", `{"confirmDeletes":true,"offer":"9298960fb3d77f2d"}`)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := s.daemon.last(t, "POST /v1/apply")
	require.Equal(t, []string{"Content-Type", "Pco-Actor"}, headerNames(got.Header))
	require.Equal(t, "application/json", got.Header.Get("Content-Type"))
	require.Equal(t, "alice@pve (ticket)", got.Header.Get("Pco-Actor"), "the actor is the session's, never the browser's")

	rec = b.send(browserHeaders(b.request(http.MethodGet, "/api/v1/events?after=3", "")))
	require.Equal(t, http.StatusOK, rec.Code)
	got = s.daemon.last(t, "GET /v1/events")
	require.Equal(t, []string{"Pco-Actor"}, headerNames(got.Header))

	// The state goes up with the gateway's own If-None-Match: that of the
	// state it holds, once it holds one.
	b.send(browserHeaders(b.request(http.MethodGet, "/api/v1/state", "")))
	got = s.daemon.last(t, "GET /v1/state")
	require.Equal(t, []string{"Pco-Actor"}, headerNames(got.Header))
	b.send(browserHeaders(b.request(http.MethodGet, "/api/v1/state", "")))
	got = s.daemon.last(t, "GET /v1/state")
	require.Equal(t, []string{"If-None-Match", "Pco-Actor"}, headerNames(got.Header))
	require.Equal(t, `"5e0c1f7a92b4d3e8"`, got.Header.Get("If-None-Match"))

	for _, c := range s.daemon.requests() {
		require.Empty(t, c.Header.Values("Cookie"))
		require.Empty(t, c.Header.Values("Authorization"))
		require.Empty(t, c.Header.Values("X-Forwarded-For"))
	}
}

func TestTheDaemonsAnswerPassesThrough(t *testing.T) {
	s := newTestServer(t)
	b := s.signIn(admin)
	refused := `{"error":"refused: the offer is not the one of the last state","code":"refused"}`
	s.daemon.on("POST /v1/apply", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Pco-Boot", bootA)
		w.Header().Set("X-Other", "1")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, refused)
	})
	rec := b.do(http.MethodPost, "/api/v1/apply", `{"offer":"x"}`)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, refused, rec.Body.String())
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	require.Empty(t, rec.Header().Get("X-Other"))
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func TestADaemonOutOfReachIsSaidSo(t *testing.T) {
	s := newTestServer(t)
	b := s.signIn(admin)
	s.daemon.close()
	requireError(t, b.do(http.MethodGet, "/api/v1/version", ""), http.StatusBadGateway, wire.CodeDaemonUnreachable)
	requireError(t, b.do(http.MethodGet, "/api/v1/state", ""), http.StatusBadGateway, wire.CodeDaemonUnreachable)
	requireError(t, b.do(http.MethodPost, "/api/v1/sync", "{}"), http.StatusBadGateway, wire.CodeDaemonUnreachable)
}

func TestACallWithoutAnAnswerInTimeEnds(t *testing.T) {
	s := newTestServer(t)
	b := s.signIn(admin)
	s.gw.timeoutOf = func(Rule) time.Duration { return 50 * time.Millisecond }
	release := make(chan struct{})
	defer close(release)
	s.daemon.on("POST /v1/apply", func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	rec := b.do(http.MethodPost, "/api/v1/apply", `{}`)
	requireError(t, rec, http.StatusServiceUnavailable, "unavailable")
	require.Contains(t, rec.Body.String(), "unknown")
}

// A failed call is logged with its method, path, status and duration, and
// never with its body or its query: a failed POST /credentials carries a
// token.
func TestTheLogOfAFailedCallHoldsNoBodyAndNoQuery(t *testing.T) {
	s := newTestServer(t)
	b := s.signIn(admin)
	token := "Zq3xV9mK2pL7wR4tY8uN1bC6dF0gH5jS"
	s.daemon.answer("POST /v1/credentials", http.StatusBadRequest, map[string]string{"error": "the token [redacted] was refused", "code": "invalid"})
	rec := b.do(http.MethodPost, "/api/v1/credentials", `{"label":"label-marker-x","token":"`+token+`"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	s.daemon.answer("GET /v1/events", http.StatusBadRequest, map[string]string{"error": "after must be the seq of an event", "code": "invalid"})
	rec = b.do(http.MethodGet, "/api/v1/events?after=marker-in-the-query", "")
	require.Equal(t, http.StatusBadRequest, rec.Code)

	logs := s.logs.String()
	require.Contains(t, logs, `"path":"/v1/credentials"`)
	require.Contains(t, logs, `"method":"POST"`)
	require.Contains(t, logs, `"status":400`)
	require.Contains(t, logs, `"duration"`)
	require.Contains(t, logs, `"path":"/v1/events"`)
	for i := 0; i+8 <= len(token); i++ {
		require.NotContains(t, logs, token[i:i+8], "a piece of the token is in the log")
	}
	require.NotContains(t, logs, "marker-in-the-query")
	require.NotContains(t, logs, "label-marker-x")
}

func TestBodiesAreBounded(t *testing.T) {
	s := newTestServer(t)
	b := s.signIn(admin)
	big := `{"offer":"` + strings.Repeat("a", 64<<10) + `"}`
	requireError(t, b.do(http.MethodPost, "/api/v1/apply", big), http.StatusRequestEntityTooLarge, wire.CodeTooLarge)
	require.Zero(t, s.daemon.count(""))

	// A settings import may be larger, up to 1 MiB.
	settings := `{"rev":7,"settings":{"denyHosts":["` + strings.Repeat("a", 512<<10) + `"]}}`
	rec := b.do(http.MethodPut, "/api/v1/settings", settings)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, s.daemon.last(t, "PUT /v1/settings").Body, len(settings))
	settings = `{"rev":7,"settings":{"denyHosts":["` + strings.Repeat("a", 1<<20) + `"]}}`
	requireError(t, b.do(http.MethodPut, "/api/v1/settings", settings), http.StatusRequestEntityTooLarge, wire.CodeTooLarge)
}

func gunzip(t *testing.T, data []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	return out
}

func TestAnswersOverAKibibyteAreGzipped(t *testing.T) {
	s := newTestServer(t)
	b := s.signIn(admin)
	large := map[string]string{"version": strings.Repeat("v", 2048)}
	s.daemon.answer("GET /v1/version", http.StatusOK, large)
	s.daemon.answer("GET /v1/settings", http.StatusOK, map[string]int{"rev": 7})

	r := b.request(http.MethodGet, "/api/v1/version", "")
	r.Header.Set("Accept-Encoding", "gzip, deflate, br")
	rec := b.send(r)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
	require.Equal(t, "Accept-Encoding", rec.Header().Get("Vary"))
	require.JSONEq(t, `{"version":"`+strings.Repeat("v", 2048)+`"}`, string(gunzip(t, rec.Body.Bytes())))

	rec = b.do(http.MethodGet, "/api/v1/version", "")
	require.Empty(t, rec.Header().Get("Content-Encoding"), "not without Accept-Encoding")
	require.Len(t, rec.Body.String(), len(`{"version":""}`)+2048)

	r = b.request(http.MethodGet, "/api/v1/version", "")
	r.Header.Set("Accept-Encoding", "gzip;q=0, identity")
	require.Empty(t, b.send(r).Header().Get("Content-Encoding"), "not when gzip is refused")

	r = b.request(http.MethodGet, "/api/v1/settings", "")
	r.Header.Set("Accept-Encoding", "gzip")
	rec = b.send(r)
	require.Empty(t, rec.Header().Get("Content-Encoding"), "not under a kibibyte")
	require.JSONEq(t, `{"rev":7}`, rec.Body.String())
}

// The answers of /api/session carry the CSRF token and are never compressed,
// whatever the browser accepts: only the gateway's routes are.
func TestSessionAnswersAreNeverCompressed(t *testing.T) {
	s := newTestServer(t)
	b := s.signIn(admin)
	r := b.request(http.MethodGet, "/api/session", "")
	r.Header.Set("Accept-Encoding", "gzip")
	rec := b.send(r)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Header().Get("Content-Encoding"))
	require.Contains(t, rec.Body.String(), b.csrf)

	r = b.request(http.MethodPost, "/api/session/ticket", "{}")
	r.Header.Set("Accept-Encoding", "gzip")
	rec = b.send(r)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Header().Get("Content-Encoding"))
}
