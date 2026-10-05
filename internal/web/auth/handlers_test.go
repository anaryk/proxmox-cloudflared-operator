package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/pvefake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/wire"
)

var update = flag.Bool("update", false, "write the golden files of the tests")

// harness is pco web with the session mounted, against a fake Proxmox VE
// served over TLS and pinned, and a clock of its own.
type harness struct {
	t     *testing.T
	fake  *pvefake.Fake
	pve   *switchPVE
	clock *clock
	auth  *Auth
	srv   http.Handler
	log   *testutil.SyncBuffer
	// reached counts the calls that got past Require, by path.
	reached map[string]int
	mu      sync.Mutex
}

// switchPVE is a PVE that can be taken down and brought back.
type switchPVE struct {
	PVE
	mu   sync.Mutex
	down bool
}

func (s *switchPVE) setDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

func (s *switchPVE) isDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.down
}

func (s *switchPVE) Privileges(ctx context.Context, c Credential, path string) (map[string]bool, error) {
	if s.isDown() {
		return nil, ErrUnreachable
	}
	return s.PVE.Privileges(ctx, c, path)
}

func (s *switchPVE) VisibleVMIDs(ctx context.Context, c Credential) ([]int, error) {
	if s.isDown() {
		return nil, ErrUnreachable
	}
	return s.PVE.VisibleVMIDs(ctx, c)
}

func newHarness(t *testing.T) *harness {
	return newHarnessWith(t, testUsers(), nil, rand.Reader)
}

// newHarnessWith serves users; pve, when given, makes the client from the
// fake's server instead of pinning its certificate.
func newHarnessWith(t *testing.T, users pvefake.Users, pve func(*httptest.Server) PVE, random io.Reader) *harness {
	t.Helper()
	fake, srv := fakePVE(t, users)
	client := NewPVE(apiURL(srv), srv.Certificate(), 5*time.Second)
	if pve != nil {
		client = pve(srv)
	}
	h := &harness{t: t, fake: fake, pve: &switchPVE{PVE: client}, clock: newClock(), log: &testutil.SyncBuffer{}, reached: map[string]int{}}
	h.auth = New(h.pve, Config{
		Now:     h.clock.Now,
		Rand:    random,
		Hosts:   func() []string { return AllowedHosts("192.0.2.10:8643", "pve1", "pve1.example.lan") },
		Profile: "host",
		Node:    "pve1",
		Zone:    "Europe/Prague",
		Version: "0.3.0",
		Log:     zerolog.New(h.log),
	})
	s, err := web.New(web.Config{
		Assets: fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>pco</title>")}},
		Now:    h.clock.Now,
		Log:    zerolog.New(h.log),
	}, h.auth, testRoutes{h})
	require.NoError(t, err)
	h.srv = s.Handler()
	return h
}

// testRoutes stand in for the gateway.
type testRoutes struct{ h *harness }

func (m testRoutes) Mount(r gin.IRouter) {
	a := m.h.auth
	reached := func(c *gin.Context) {
		m.h.mu.Lock()
		m.h.reached[c.Request.URL.Path]++
		m.h.mu.Unlock()
		s := SessionOf(c)
		c.JSON(http.StatusOK, gin.H{"user": s.Principal.User, "role": s.Principal.Role.String()})
	}
	r.GET("/api/v1/state", a.Require(RoleReader), reached)
	r.GET(StreamPath, a.Require(RoleReader), reached)
	r.POST("/api/v1/doctor", a.Require(RoleReader), reached)
	r.POST("/api/v1/sync", a.Require(RoleAdmin), reached)
	r.GET("/api/v1/visible", a.Require(RoleReader), func(c *gin.Context) {
		visible, hash, err := a.Visible(c)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		seen := []int{}
		for _, ref := range []model.GuestRef{{Kind: model.KindQEMU, VMID: 101}, {Kind: model.KindQEMU, VMID: 102}, {Kind: model.KindLXC, VMID: 200}, {Kind: model.KindQEMU, VMID: 300}} {
			if visible(ref) {
				seen = append(seen, ref.VMID)
			}
		}
		c.JSON(http.StatusOK, gin.H{"vmids": seen, "hash": hash})
	})
}

func (h *harness) reachedCount(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reached[path]
}

// browser keeps the cookies and the CSRF token of one browser, and sends
// what the page would.
type browser struct {
	h       *harness
	ticket  string // PVEAuthCookie
	session string
	csrf    string
	peer    string
}

func (h *harness) browser(ticket string) *browser {
	return &browser{h: h, ticket: ticket, peer: "192.0.2.7:51234"}
}

func (b *browser) request(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, "https://"+testHost+path, strings.NewReader(body))
	r.RemoteAddr = b.peer
	if method != http.MethodGet && method != http.MethodHead {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		if b.csrf != "" {
			r.Header.Set("Pco-Csrf", b.csrf)
		}
	}
	if b.ticket != "" {
		r.AddCookie(&http.Cookie{Name: "PVEAuthCookie", Value: b.ticket})
	}
	if b.session != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: b.session})
	}
	return r
}

func (b *browser) send(r *http.Request) *httptest.ResponseRecorder {
	b.h.t.Helper()
	rec := httptest.NewRecorder()
	b.h.srv.ServeHTTP(rec, r)
	for name := range rec.Header() {
		require.False(b.h.t, strings.HasPrefix(strings.ToLower(name), "access-control-"), "no CORS header, ever: %s", name)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			b.session = c.Value
		}
	}
	if rec.Code == http.StatusOK && strings.HasPrefix(r.URL.Path, "/api/session") {
		var s wire.Session
		require.NoError(b.h.t, json.Unmarshal(rec.Body.Bytes(), &s))
		b.csrf = s.CSRF
	}
	return rec
}

func (b *browser) do(method, path, body string) *httptest.ResponseRecorder {
	return b.send(b.request(method, path, body))
}

func (b *browser) signIn() wire.Session {
	b.h.t.Helper()
	rec := b.do(http.MethodPost, "/api/session/ticket", "{}")
	require.Equal(b.h.t, http.StatusOK, rec.Code, rec.Body.String())
	var s wire.Session
	require.NoError(b.h.t, json.Unmarshal(rec.Body.Bytes(), &s))
	return s
}

func (b *browser) signInToken(token string) *httptest.ResponseRecorder {
	return b.do(http.MethodPost, "/api/session/token", `{"token":"`+token+`"}`)
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) wire.Error {
	t.Helper()
	var e wire.Error
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &e), rec.Body.String())
	return e
}

func TestSignInWithTheTicket(t *testing.T) {
	cases := []struct {
		name    string
		ticket  string
		status  int
		role    string
		code    string
		missing string
	}{
		{"an admin", aliceTicket, 200, "admin", "", ""},
		{"a reader", bobTicket, 200, "reader", "", ""},
		{"without Sys.Audit", carolTicket, 403, "", wire.CodeForbidden, "Sys.Audit"},
		{"an expired ticket", "PVE:alice@pve:66F00000::ZXhwaXJlZA==", 401, "", wire.CodeTicketInvalid, ""},
		{"no ticket", "", 401, "", wire.CodeTicketInvalid, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			b := h.browser(c.ticket)
			rec := b.do(http.MethodPost, "/api/session/ticket", "{}")
			require.Equal(t, c.status, rec.Code, rec.Body.String())
			if c.status == 200 {
				var s wire.Session
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &s))
				require.Equal(t, c.role, s.Role)
				require.Equal(t, "ticket", s.Method)
				require.Equal(t, t0.Add(30*time.Minute), s.IdleExpiresAt)
				require.Equal(t, t0.Add(12*time.Hour), s.ExpiresAt)
				require.Equal(t, http.StatusOK, b.do(http.MethodGet, "/api/v1/state", "").Code)
				return
			}
			e := errorOf(t, rec)
			require.Equal(t, c.code, e.Code)
			require.Equal(t, c.missing, e.Missing)
			require.Empty(t, rec.Result().Cookies(), "a refused sign-in sets no cookie")
		})
	}
}

func TestSignInFailsClosed(t *testing.T) {
	down := httptest.NewTLSServer(http.NotFoundHandler())
	down.Close()
	cases := []struct {
		name string
		pve  func(*httptest.Server) PVE
	}{
		{"Proxmox VE down", func(*httptest.Server) PVE { return NewPVE(apiURL(down), down.Certificate(), time.Second) }},
		{"another certificate than the pin", func(s *httptest.Server) PVE { return NewPVE(apiURL(s), otherCertificate(t), time.Second) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarnessWith(t, testUsers(), c.pve, rand.Reader)
			for _, rec := range []*httptest.ResponseRecorder{
				h.browser(aliceTicket).do(http.MethodPost, "/api/session/ticket", "{}"),
				h.browser("").signInToken(aliceToken),
			} {
				require.Equal(t, http.StatusServiceUnavailable, rec.Code)
				require.Equal(t, wire.CodeProxmoxUnreachable, errorOf(t, rec).Code)
				require.Empty(t, rec.Result().Cookies())
			}
			require.Zero(t, h.fake.Calls("access/permissions"), "nothing reached the fake")
		})
	}
}

func TestProxmoxDownDuringASession(t *testing.T) {
	h := newHarness(t)
	b := h.browser(aliceTicket)
	b.signIn()
	h.clock.Add(time.Minute)
	h.pve.setDown(true)
	rec := b.do(http.MethodGet, "/api/v1/state", "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, wire.CodeProxmoxUnreachable, errorOf(t, rec).Code)

	h.pve.setDown(false)
	require.Equal(t, http.StatusOK, b.do(http.MethodGet, "/api/v1/state", "").Code, "an outage does not end the session")
}

func TestSignInWithAToken(t *testing.T) {
	cases := []struct {
		name   string
		token  string
		status int
		role   string
		user   string
	}{
		{"a token without privilege separation", aliceToken, 200, "admin", "alice@pve!pco"},
		{"a privilege-separated token has what both have", aliceSep, 200, "reader", "alice@pve!sep"},
		{"a revoked token", "alice@pve!gone=" + tokenSecret, 401, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			b := h.browser("")
			rec := b.signInToken(c.token)
			require.Equal(t, c.status, rec.Code, rec.Body.String())
			if c.status != 200 {
				require.Equal(t, wire.CodeTicketInvalid, errorOf(t, rec).Code)
				return
			}
			var s wire.Session
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &s))
			require.Equal(t, wire.Session{
				User: c.user, Method: "token", Role: c.role, CSRF: s.CSRF,
				IdleExpiresAt: t0.Add(30 * time.Minute), ExpiresAt: t0.Add(8 * time.Hour),
				Profile: "host", Node: "pve1", NodeZone: "Europe/Prague", Version: "0.3.0",
			}, s)
			require.Equal(t, http.StatusOK, b.do(http.MethodGet, "/api/v1/state", "").Code)
		})
	}
}

func TestAMalformedToken(t *testing.T) {
	h := newHarness(t)
	bodies := []string{
		`{"token":"alice@pve!pco"}`,
		`{"token":"alice@pve=` + tokenSecret + `"}`,
		`{"token":"alice!pco=` + tokenSecret + `"}`,
		`{"token":"alice@pve!pco=not-a-uuid"}`,
		`{"token":"alice@pve!pco=` + tokenSecret + `\n"}`,
		`{"token":" alice@pve!pco=` + tokenSecret + `"}`,
		`{"token":"alice@pve!p/co=` + tokenSecret + `"}`,
		`{"token":""}`,
	}
	// Each from an address of its own, below the limit of sign-ins.
	attempt := func(i int, body string) *httptest.ResponseRecorder {
		b := h.browser("")
		b.peer = "192.0.2." + strconv.Itoa(i+10) + ":443"
		return b.do(http.MethodPost, "/api/session/token", body)
	}
	for i, body := range bodies {
		rec := attempt(i, body)
		require.Equal(t, http.StatusBadRequest, rec.Code, body)
		e := errorOf(t, rec)
		require.Equal(t, wire.CodeInvalid, e.Code, body)
		require.Equal(t, "token", e.Field, body)
	}
	for i, body := range []string{``, `{}x`, `[]`, `{"token":"` + aliceToken + `","more":1}`, `{"token":1}`} {
		rec := attempt(i+len(bodies), body)
		require.Equal(t, http.StatusBadRequest, rec.Code, body)
		require.Equal(t, wire.CodeInvalid, errorOf(t, rec).Code, body)
	}
	for _, body := range []string{``, `{"ticket":"x"}`, `null`} {
		rec := h.browser(aliceTicket).do(http.MethodPost, "/api/session/ticket", body)
		require.Equal(t, http.StatusBadRequest, rec.Code, body)
	}
	require.Zero(t, h.fake.Calls("access/permissions"), "nothing malformed reaches Proxmox VE")
}

func TestATokenRevokedMidSessionEndsWithinAMinute(t *testing.T) {
	h := newHarness(t)
	b := h.browser("")
	require.Equal(t, http.StatusOK, b.signInToken(aliceToken).Code)
	h.fake.RemoveToken("alice@pve!pco")

	h.clock.Add(59 * time.Second)
	require.Equal(t, http.StatusOK, b.do(http.MethodGet, "/api/v1/state", "").Code)
	require.Equal(t, 1, h.fake.Calls("access/permissions"), "a token is checked once a minute")

	h.clock.Add(time.Second)
	rec := b.do(http.MethodGet, "/api/v1/state", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, wire.CodeUnauthenticated, errorOf(t, rec).Code)
	require.Contains(t, h.log.String(), "the token is no longer valid")
}

func TestARevokedSysModifyDowngrades(t *testing.T) {
	cases := []struct {
		name   string
		signIn func(*browser)
		user   string
		within time.Duration
	}{
		{"a ticket within 30 s", func(b *browser) { b.signIn() }, "alice@pve", 30 * time.Second},
		{"a token within 60 s", func(b *browser) { require.Equal(b.h.t, 200, b.signInToken(aliceToken).Code) }, "alice@pve", time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			b := h.browser(aliceTicket)
			c.signIn(b)
			require.Equal(t, http.StatusOK, b.do(http.MethodPost, "/api/v1/sync", "{}").Code)
			h.fake.SetPrivileges(c.user, "/", "Sys.Audit")

			h.clock.Add(c.within - time.Second)
			require.Equal(t, http.StatusOK, b.do(http.MethodPost, "/api/v1/sync", "{}").Code)
			h.clock.Add(time.Second)
			rec := b.do(http.MethodPost, "/api/v1/sync", "{}")
			require.Equal(t, http.StatusForbidden, rec.Code)
			e := errorOf(t, rec)
			require.Equal(t, wire.CodeForbidden, e.Code)
			require.Equal(t, "Sys.Modify", e.Missing)

			rec = b.do(http.MethodGet, "/api/session", "")
			require.Equal(t, http.StatusOK, rec.Code)
			require.Contains(t, rec.Body.String(), `"role":"reader"`)
			require.Equal(t, http.StatusOK, b.do(http.MethodGet, "/api/v1/state", "").Code, "a reader still reads")

			h.fake.SetPrivileges(c.user, "/", "VM.Audit")
			h.clock.Add(c.within)
			require.Equal(t, http.StatusUnauthorized, b.do(http.MethodGet, "/api/v1/state", "").Code, "without Sys.Audit the session ends")
		})
	}
}

func TestTheCookie(t *testing.T) {
	h := newHarness(t)
	b := h.browser(aliceTicket)
	rec := b.do(http.MethodPost, "/api/session/ticket", "{}")
	require.Equal(t, http.StatusOK, rec.Code)
	set := rec.Header().Values("Set-Cookie")
	require.Len(t, set, 1)
	require.Regexp(t, regexp.MustCompile(`^__Host-pco-session=[A-Za-z0-9_-]{43}; Path=/; HttpOnly; Secure; SameSite=Strict$`), set[0])

	var s wire.Session
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &s))
	require.Regexp(t, regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`), s.CSRF)
	require.NotEqual(t, b.session, s.CSRF)

	rec = b.do(http.MethodDelete, "/api/session", "")
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, []string{"__Host-pco-session=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Strict"}, rec.Header().Values("Set-Cookie"))
}

func TestIdleExpiry(t *testing.T) {
	cases := []struct {
		name string
		// last is the request 29 minutes after the sign-in; it counts as
		// activity or not.
		last     func(*browser) *httptest.ResponseRecorder
		activity bool
	}{
		{"a request of the user", func(b *browser) *httptest.ResponseRecorder { return b.do(http.MethodGet, "/api/v1/state", "") }, true},
		{"a background request", func(b *browser) *httptest.ResponseRecorder {
			r := b.request(http.MethodGet, "/api/v1/state", "")
			r.Header.Set("Pco-Background", "1")
			return b.send(r)
		}, false},
		{"the stream", func(b *browser) *httptest.ResponseRecorder { return b.do(http.MethodGet, StreamPath, "") }, false},
		{"a touch", func(b *browser) *httptest.ResponseRecorder {
			rec := b.do(http.MethodPost, "/api/session/touch", "{}")
			require.Equal(b.h.t, http.StatusNoContent, rec.Code)
			return rec
		}, true},
		{"a background touch is still one", func(b *browser) *httptest.ResponseRecorder {
			r := b.request(http.MethodPost, "/api/session/touch", "{}")
			r.Header.Set("Pco-Background", "1")
			return b.send(r)
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			b := h.browser(aliceTicket)
			b.signIn()
			h.clock.Add(29 * time.Minute)
			rec := c.last(b)
			require.Less(t, rec.Code, 300, rec.Body.String())
			h.clock.Add(2 * time.Minute)
			rec = b.do(http.MethodGet, "/api/session", "")
			if c.activity {
				require.Equal(t, http.StatusOK, rec.Code)
				var s wire.Session
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &s))
				require.Equal(t, t0.Add(61*time.Minute), s.IdleExpiresAt, "the GET of the session counts too")
				return
			}
			require.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}
}

func TestAbsoluteExpiry(t *testing.T) {
	h := newHarness(t)
	ticket := h.browser(aliceTicket)
	ticket.signIn()
	token := h.browser("")
	require.Equal(t, http.StatusOK, token.signInToken(aliceToken).Code)
	touch := func(b *browser) int { return b.do(http.MethodPost, "/api/session/touch", "{}").Code }

	for elapsed := 20 * time.Minute; elapsed < 8*time.Hour; elapsed += 20 * time.Minute {
		h.clock.Add(20 * time.Minute)
		require.Equal(t, http.StatusNoContent, touch(ticket), elapsed)
		require.Equal(t, http.StatusNoContent, touch(token), elapsed)
	}
	h.clock.Add(20 * time.Minute)
	require.Equal(t, http.StatusUnauthorized, token.do(http.MethodGet, "/api/v1/state", "").Code, "8 h for a token")
	for elapsed := 8 * time.Hour; elapsed < 12*time.Hour; elapsed += 20 * time.Minute {
		require.Equal(t, http.StatusNoContent, touch(ticket), elapsed)
		h.clock.Add(20 * time.Minute)
	}
	require.Equal(t, http.StatusUnauthorized, ticket.do(http.MethodGet, "/api/v1/state", "").Code, "12 h for a ticket")
}

func TestSessionAnswersAreNeverCompressed(t *testing.T) {
	h := newHarness(t)
	b := h.browser(aliceTicket)
	gzip := func(method, path, body string) *httptest.ResponseRecorder {
		r := b.request(method, path, body)
		r.Header.Set("Accept-Encoding", "gzip, deflate, br")
		return b.send(r)
	}
	recs := []*httptest.ResponseRecorder{
		gzip(http.MethodGet, "/api/session", ""),
		gzip(http.MethodPost, "/api/session/ticket", "{}"),
		gzip(http.MethodGet, "/api/session", ""),
		gzip(http.MethodPost, "/api/session/touch", "{}"),
		gzip(http.MethodPost, "/api/session/token", `{"token":"`+aliceToken+`"}`),
		gzip(http.MethodDelete, "/api/session", ""),
	}
	for i, rec := range recs {
		require.Less(t, rec.Code, 500, i)
		require.Empty(t, rec.Header().Get("Content-Encoding"), i)
		require.Equal(t, "no-store", rec.Header().Get("Cache-Control"), i)
	}
	require.Contains(t, recs[2].Body.String(), `"csrf":"`, "the answer is plain JSON")
}

func TestTheSixthSignInOfAUserEndsItsOldest(t *testing.T) {
	h := newHarness(t)
	var browsers []*browser
	for range perUser + 1 {
		b := h.browser(aliceTicket)
		b.signIn()
		browsers = append(browsers, b)
		h.clock.Add(time.Second)
	}
	require.Equal(t, http.StatusUnauthorized, browsers[0].do(http.MethodGet, "/api/v1/state", "").Code)
	for _, b := range browsers[1:] {
		require.Equal(t, http.StatusOK, b.do(http.MethodGet, "/api/v1/state", "").Code)
	}
	require.Contains(t, h.log.String(), `"result":"evicted"`)
}

func TestEverySignInMakesANewSession(t *testing.T) {
	h := newHarness(t)
	b := h.browser(aliceTicket)
	first := b.signIn()
	firstID := b.session

	second := b.signIn()
	require.NotEqual(t, firstID, b.session, "the id the browser had is not kept")
	require.NotEqual(t, first.CSRF, second.CSRF)

	old := h.browser(aliceTicket)
	old.session = firstID
	require.Equal(t, http.StatusUnauthorized, old.do(http.MethodGet, "/api/v1/state", "").Code)
	require.Equal(t, http.StatusOK, b.do(http.MethodGet, "/api/v1/state", "").Code)

	// A session id an attacker planted is replaced in the same way.
	planted := h.browser(aliceTicket)
	planted.session = strings.Repeat("A", 43)
	planted.signIn()
	require.NotEqual(t, strings.Repeat("A", 43), planted.session)
}

func TestTheTicketOfTheRequest(t *testing.T) {
	t.Run("a renewed ticket of the same user re-binds", func(t *testing.T) {
		h := newHarness(t)
		b := h.browser(aliceTicket)
		b.signIn()
		renewed := "PVE:alice@pve:66F1EF00::cmVuZXdlZA=="
		h.fake.AddTicket("alice@pve", renewed)
		h.fake.RemoveTicket(aliceTicket)
		b.ticket = renewed
		require.Equal(t, http.StatusOK, b.do(http.MethodGet, "/api/v1/state", "").Code)
		h.clock.Add(time.Minute)
		require.Equal(t, http.StatusOK, b.do(http.MethodGet, "/api/v1/state", "").Code)
	})
	t.Run("another user's ticket ends the session", func(t *testing.T) {
		h := newHarness(t)
		b := h.browser(aliceTicket)
		b.signIn()
		b.ticket = bobTicket
		require.Equal(t, http.StatusUnauthorized, b.do(http.MethodGet, "/api/v1/state", "").Code)
		b.ticket = aliceTicket
		require.Equal(t, http.StatusUnauthorized, b.do(http.MethodGet, "/api/v1/state", "").Code, "the session is over")
		require.Contains(t, h.log.String(), "the ticket is another user's")
	})
	t.Run("no ticket ends the session", func(t *testing.T) {
		h := newHarness(t)
		b := h.browser(aliceTicket)
		b.signIn()
		b.ticket = ""
		require.Equal(t, http.StatusUnauthorized, b.do(http.MethodGet, "/api/v1/state", "").Code)
		b.ticket = aliceTicket
		require.Equal(t, http.StatusUnauthorized, b.do(http.MethodGet, "/api/v1/state", "").Code)
	})
	t.Run("an expired ticket ends the session", func(t *testing.T) {
		h := newHarness(t)
		b := h.browser(aliceTicket)
		b.signIn()
		h.fake.RemoveTicket(aliceTicket)
		h.clock.Add(29 * time.Second)
		require.Equal(t, http.StatusOK, b.do(http.MethodGet, "/api/v1/state", "").Code, "the check is cached for 30 s")
		h.clock.Add(time.Second)
		require.Equal(t, http.StatusUnauthorized, b.do(http.MethodGet, "/api/v1/state", "").Code)
	})
	t.Run("a token session ignores the ticket", func(t *testing.T) {
		h := newHarness(t)
		b := h.browser(bobTicket)
		require.Equal(t, http.StatusOK, b.signInToken(aliceToken).Code)
		rec := b.do(http.MethodGet, "/api/v1/state", "")
		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Body.String(), `"user":"alice@pve!pco"`)
	})
}

func TestCSRF(t *testing.T) {
	h := newHarness(t)
	b := h.browser(aliceTicket)
	b.signIn()
	other := h.browser(bobTicket)
	other.signIn()

	for _, path := range []string{"/api/v1/sync", "/api/v1/doctor", "/api/session/touch"} {
		for name, value := range map[string]string{"missing": "", "wrong": strings.Repeat("x", 43), "another session's": other.csrf, "a prefix": b.csrf[:42]} {
			r := b.request(http.MethodPost, path, "{}")
			r.Header.Del("Pco-Csrf")
			if value != "" {
				r.Header.Set("Pco-Csrf", value)
			}
			rec := b.send(r)
			require.Equal(t, http.StatusForbidden, rec.Code, path+": "+name)
			require.Equal(t, wire.CodeForbidden, errorOf(t, rec).Code)
		}
	}
	r := b.request(http.MethodDelete, "/api/session", "")
	r.Header.Del("Pco-Csrf")
	require.Equal(t, http.StatusForbidden, b.send(r).Code)
	require.Zero(t, h.reachedCount("/api/v1/sync")+h.reachedCount("/api/v1/doctor"))

	require.Equal(t, http.StatusOK, b.do(http.MethodPost, "/api/v1/sync", "{}").Code)
	require.Equal(t, http.StatusOK, b.do(http.MethodPost, "/api/v1/doctor", "{}").Code)
	require.Equal(t, http.StatusOK, b.do(http.MethodGet, "/api/v1/state", "").Code, "a GET needs no CSRF token")
}

func TestTheRulesOfAMutatingRequest(t *testing.T) {
	h := newHarness(t)
	b := h.browser(aliceTicket)
	b.signIn()
	cases := []struct {
		name   string
		change func(*http.Request)
		status int
	}{
		{"a form", func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, 415},
		{"plain text", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"no content type", func(r *http.Request) { r.Header.Del("Content-Type") }, 415},
		{"no Origin", func(r *http.Request) { r.Header.Del("Origin") }, 403},
		{"a foreign Origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example.lan") }, 403},
		{"an Origin of plain HTTP", func(r *http.Request) { r.Header.Set("Origin", "http://pve1.example.lan:8643") }, 403},
		{"same-site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }, 403},
		{"an unknown host", func(r *http.Request) {
			r.Host = "rebound.example:8643"
			r.Header.Set("Origin", "https://rebound.example:8643")
		}, 403},
	}
	for _, path := range []string{"/api/v1/sync", "/api/v1/doctor", "/api/session/touch", "/api/session/ticket", "/api/session/token"} {
		for _, c := range cases {
			r := b.request(http.MethodPost, path, "{}")
			c.change(r)
			require.Equal(t, c.status, b.send(r).Code, path+": "+c.name)
		}
	}
	for _, c := range cases {
		r := b.request(http.MethodDelete, "/api/session", "")
		c.change(r)
		require.Equal(t, c.status, b.send(r).Code, "sign-out: "+c.name)
	}
	require.Zero(t, h.reachedCount("/api/v1/sync")+h.reachedCount("/api/v1/doctor"))
	require.Equal(t, 1, h.fake.Calls("access/permissions"), "only the sign-in reached Proxmox VE")
	require.Equal(t, http.StatusOK, b.do(http.MethodGet, "/api/v1/state", "").Code, "the session is still there")
}

func TestAGetIsANavigation(t *testing.T) {
	h := newHarness(t)
	b := h.browser(aliceTicket)
	b.signIn()
	for _, site := range []string{"same-site", "none", "cross-site"} {
		r := b.request(http.MethodGet, "/api/v1/state", "")
		r.Header.Set("Sec-Fetch-Site", site)
		require.Equal(t, http.StatusOK, b.send(r).Code, site)
	}
	r := b.request(http.MethodGet, "/api/session", "")
	r.Host = "rebound.example:8643"
	require.Equal(t, http.StatusForbidden, b.send(r).Code, "a GET under an unknown name is refused (DNS rebinding)")
	r = b.request(http.MethodGet, "/api/v1/state", "")
	r.Host = "rebound.example:8643"
	require.Equal(t, http.StatusForbidden, b.send(r).Code)
}

func TestOptionsAndNoCORS(t *testing.T) {
	h := newHarness(t)
	b := h.browser(aliceTicket)
	b.signIn()
	for _, path := range []string{"/api/session", "/api/session/ticket", "/api/session/token", "/api/session/touch", "/api/v1/sync"} {
		r := b.request(http.MethodOptions, path, "")
		r.Header.Set("Origin", "https://evil.example")
		r.Header.Set("Access-Control-Request-Method", "POST")
		r.Header.Set("Access-Control-Request-Headers", "pco-csrf, content-type")
		require.Equal(t, http.StatusMethodNotAllowed, b.send(r).Code, path)
	}
}

func TestTheTokenSignInIsRateLimited(t *testing.T) {
	h := newHarness(t)
	attempt := func(peer, forwarded, token string) *httptest.ResponseRecorder {
		b := h.browser("")
		b.peer = peer
		r := b.request(http.MethodPost, "/api/session/token", `{"token":"`+token+`"}`)
		if forwarded != "" {
			r.Header.Set("X-Forwarded-For", forwarded)
			r.Header.Set("X-Real-Ip", forwarded)
		}
		return b.send(r)
	}
	for i := range 10 {
		token := "alice@pve!guess=0b5c3e7e-1d2f-4a6b-9c8d-00000000000" + string(rune('0'+i))
		require.Equal(t, http.StatusUnauthorized, attempt("192.0.2.7:4000"+string(rune('0'+i)), "", token).Code, i)
	}
	rec := attempt("192.0.2.7:50000", "", aliceToken)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "the 11th from the address, whatever its port")
	e := errorOf(t, rec)
	require.Equal(t, wire.CodeRateLimited, e.Code)
	require.Equal(t, 60, e.RetryAfter)
	require.Equal(t, "60", rec.Header().Get("Retry-After"))
	for i := range 5 {
		forwarded := "203.0.113." + string(rune('1'+i))
		require.Equal(t, http.StatusTooManyRequests, attempt("192.0.2.7:50001", forwarded, aliceToken).Code, "X-Forwarded-For is not the client")
	}
	require.Equal(t, 10, h.fake.Calls("access/permissions"), "a refused attempt does not reach Proxmox VE")

	require.Equal(t, http.StatusOK, attempt("192.0.2.8:50000", "", aliceToken).Code, "another address has its own limit")
	h.clock.Add(30 * time.Second)
	rec = attempt("192.0.2.7:50000", "", aliceToken)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, 30, errorOf(t, rec).RetryAfter)
	h.clock.Add(30 * time.Second)
	require.Equal(t, http.StatusOK, attempt("192.0.2.7:50000", "", aliceToken).Code, "a minute later")
	require.Contains(t, h.log.String(), `"client":"192.0.2.7"`)
	require.NotContains(t, h.log.String(), "203.0.113.")
}

func TestTheLogHasNoSecret(t *testing.T) {
	h := newHarness(t)
	admin := h.browser(aliceTicket)
	admin.signIn()
	admin.do(http.MethodPost, "/api/session/touch", "{}")
	admin.do(http.MethodPost, "/api/v1/sync", "{}")
	adminSession, adminCSRF := admin.session, admin.csrf
	admin.do(http.MethodDelete, "/api/session", "")

	tok := h.browser("")
	require.Equal(t, http.StatusOK, tok.signInToken(aliceToken).Code)
	tok.do(http.MethodGet, "/api/v1/state", "")
	tok.signInToken("alice@pve!gone=" + tokenSecret)
	tok.signInToken("alice@pve!pco=" + tokenSecret + "x")
	h.browser("PVE:alice@pve:1::c2VjcmV0LXNpZ25hdHVyZQ==").do(http.MethodPost, "/api/session/ticket", "{}")
	h.browser(carolTicket).do(http.MethodPost, "/api/session/ticket", "{}")
	switched := h.browser(bobTicket)
	switched.signIn()
	switched.ticket = aliceTicket
	switched.do(http.MethodGet, "/api/v1/state", "")

	log := h.log.String()
	for _, secret := range []string{
		aliceTicket, "YWxpY2Utc2lnbmF0dXJl", bobTicket, "Ym9iLXNpZ25hdHVyZQ", carolTicket, "c2VjcmV0LXNpZ25hdHVyZQ",
		tokenSecret, adminSession, adminCSRF, tok.session, tok.csrf, switched.session, switched.csrf,
	} {
		require.NotEmpty(t, secret)
		require.NotContains(t, log, secret)
	}

	var lines []map[string]any
	for line := range strings.Lines(log) {
		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &entry))
		if entry["message"] == "sign-in" || entry["message"] == "sign-out" {
			delete(entry, "level")
			lines = append(lines, entry)
		}
	}
	require.Equal(t, []map[string]any{
		{"message": "sign-in", "user": "alice@pve", "method": "ticket", "client": "192.0.2.7", "result": "ok", "role": "admin"},
		{"message": "sign-out", "user": "alice@pve", "method": "ticket", "client": "192.0.2.7", "result": "signed out"},
		{"message": "sign-in", "user": "alice@pve!pco", "method": "token", "client": "192.0.2.7", "result": "ok", "role": "admin"},
		{"message": "sign-in", "user": "alice@pve!gone", "method": "token", "client": "192.0.2.7", "result": "refused: Proxmox VE did not accept the token"},
		{"message": "sign-in", "method": "token", "client": "192.0.2.7", "result": "refused: malformed token"},
		{"message": "sign-in", "method": "ticket", "client": "192.0.2.7", "result": "refused: Proxmox VE did not accept the ticket"},
		{"message": "sign-in", "user": "carol@pve", "method": "ticket", "client": "192.0.2.7", "result": "refused: no Sys.Audit on /"},
		{"message": "sign-in", "user": "bob@pve", "method": "ticket", "client": "192.0.2.7", "result": "ok", "role": "reader"},
		{"message": "sign-out", "user": "bob@pve", "method": "ticket", "client": "192.0.2.7", "result": "ended: the ticket is another user's"},
	}, lines)
}

func TestTheSessionGolden(t *testing.T) {
	counting := make([]byte, 256)
	for i := range counting {
		counting[i] = byte(i)
	}
	h := newHarnessWith(t, testUsers(), nil, bytes.NewReader(counting))
	b := h.browser(aliceTicket)
	b.signIn()
	h.clock.Add(5 * time.Minute)
	rec := b.do(http.MethodGet, "/api/session", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	requireGolden(t, "session.json", rec.Body.Bytes())

	rec = h.browser(aliceTicket).do(http.MethodGet, "/api/session", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	requireGolden(t, "unauthenticated.json", rec.Body.Bytes())
	rec = h.browser("").do(http.MethodGet, "/api/session", "")
	require.JSONEq(t, `{"code":"unauthenticated","methods":["ticket","token"],"ticket":false}`, rec.Body.String())
}

func TestSignOut(t *testing.T) {
	h := newHarness(t)
	b := h.browser(aliceTicket)
	b.signIn()
	h.clock.Add(time.Minute)
	h.pve.setDown(true)
	id := b.session
	rec := b.do(http.MethodDelete, "/api/session", "")
	require.Equal(t, http.StatusNoContent, rec.Code, "signing out needs no Proxmox VE")
	require.Empty(t, b.session)
	h.pve.setDown(false)

	gone := h.browser(aliceTicket)
	gone.session = id
	require.Equal(t, http.StatusUnauthorized, gone.do(http.MethodGet, "/api/session", "").Code)
	require.Equal(t, http.StatusUnauthorized, b.do(http.MethodDelete, "/api/session", "").Code)
}

func TestVisible(t *testing.T) {
	h := newHarness(t)
	admin := h.browser(aliceTicket)
	admin.signIn()
	rec := admin.do(http.MethodGet, "/api/v1/visible", "")
	require.JSONEq(t, `{"vmids":[101,102,200,300],"hash":""}`, rec.Body.String(), "an admin sees every guest")
	require.Zero(t, h.fake.Calls("cluster/resources"))

	reader := h.browser(bobTicket)
	reader.signIn()
	var got struct {
		VMIDs []int  `json:"vmids"`
		Hash  string `json:"hash"`
	}
	rec = reader.do(http.MethodGet, "/api/v1/visible", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, []int{102, 200}, got.VMIDs)
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), got.Hash)
	first := got.Hash

	h.clock.Add(59 * time.Second)
	reader.do(http.MethodGet, "/api/v1/visible", "")
	require.Equal(t, 1, h.fake.Calls("cluster/resources"), "the set is kept for 60 s")
	h.clock.Add(time.Second)
	rec = reader.do(http.MethodGet, "/api/v1/visible", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, 2, h.fake.Calls("cluster/resources"))
	require.Equal(t, first, got.Hash, "the same set has the same hash")

	tok := h.browser("")
	require.Equal(t, http.StatusOK, tok.signInToken(aliceSep).Code)
	rec = tok.do(http.MethodGet, "/api/v1/visible", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, []int{200}, got.VMIDs, "a privilege-separated token sees what it may")
	require.NotEqual(t, first, got.Hash)
}

func TestVisibleWhenProxmoxFails(t *testing.T) {
	a := New(staticPVE{}, Config{})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	_, _, err := a.Visible(c)
	require.Error(t, err, "no session")

	a = New(staticPVE{err: ErrUnreachable}, Config{})
	c.Set(sessionKey, &Session{ID: "s", Principal: Principal{User: "bob@pve", Method: MethodToken, Role: RoleReader}, token: aliceSep})
	_, _, err = a.Visible(c)
	require.ErrorIs(t, err, ErrUnreachable)

	c.Set(sessionKey, &Session{ID: "s", Principal: Principal{User: "alice@pve", Method: MethodTicket, Role: RoleAdmin}})
	visible, hash, err := a.Visible(c)
	require.NoError(t, err, "an admin needs no list")
	require.Empty(t, hash)
	require.True(t, visible(model.GuestRef{Kind: model.KindLXC, VMID: 999}))
}

func TestTheActorRule(t *testing.T) {
	users := testUsers()
	users.Users = append(users.Users, pvefake.User{
		ID:         "o'neil@pve",
		Privileges: map[string][]string{"/": {"Sys.Audit"}},
		Tickets:    []string{"PVE:o'neil@pve:66F1E2D6::b25laWw="},
	}, pvefake.User{
		ID:         strings.Repeat("a", 116) + "@pve",
		Privileges: map[string][]string{"/": {"Sys.Audit"}},
		Tickets:    []string{"PVE:" + strings.Repeat("a", 116) + "@pve:66F1E2D7::bG9uZw=="},
	})
	h := newHarnessWith(t, users, nil, rand.Reader)
	for _, ticket := range []string{"PVE:o'neil@pve:66F1E2D6::b25laWw=", "PVE:" + strings.Repeat("a", 116) + "@pve:66F1E2D7::bG9uZw=="} {
		rec := h.browser(ticket).do(http.MethodPost, "/api/session/ticket", "{}")
		require.Equal(t, http.StatusForbidden, rec.Code)
		require.Equal(t, wire.CodeForbidden, errorOf(t, rec).Code)
		require.Empty(t, rec.Result().Cookies())
	}
	require.Equal(t, "alice@pve (ticket)", Principal{User: "alice@pve", Method: MethodTicket}.Actor())
	require.Equal(t, "alice@pve!pco (token)", Principal{User: "alice@pve!pco", Method: MethodToken}.Actor())
}

func TestRequireWithoutASessionRoute(t *testing.T) {
	require.Nil(t, SessionOf(&gin.Context{}))
}

// requireGolden checks JSON, indented, against the golden file name.
func requireGolden(t *testing.T, name string, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, json.Indent(&buf, body, "", "  "))
	buf.WriteByte('\n')
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), buf.String(), "the JSON changed; run the test with -update when that is intended")
}
