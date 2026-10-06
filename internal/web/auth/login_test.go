package auth

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/pvefake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/wire"
)

// The passwords of the appliance's users; each is distinctive, so that a test
// finds it wherever it leaks.
const (
	doraPassword = "pw-dora-Zebra-7731"
	erinPassword = "pw-erin-Quokka-1904"
	fredPassword = "pw-fred-Narwhal-5512"
	ginaPassword = "pw-gina-Ibis-6620"
	rootPassword = "pw-root-Okapi-3307"
	fredTOTP     = "424242"
	fredRecovery = "a1b2-c3d4-e5f6-a7b8"
)

// applianceUsers are dora, an admin with a token, erin, a reader, fred, an
// admin with TOTP and recovery codes, gina, whose second factor is a security
// key, and root@pam.
func applianceUsers() pvefake.Users {
	admin := map[string][]string{"/": {"Sys.Audit", "Sys.Modify", "VM.Audit"}}
	return pvefake.Users{Users: []pvefake.User{
		{ID: "dora@pve", Password: doraPassword, Privileges: admin, Guests: []string{"qemu/101", "qemu/102"},
			Tokens: []pvefake.Token{{ID: "pco", Secret: tokenSecret}}},
		{ID: "erin@pve", Password: erinPassword, Privileges: map[string][]string{"/": {"Sys.Audit"}}, Guests: []string{"qemu/102"}},
		{ID: "fred@pve", Password: fredPassword, Privileges: admin, TOTP: fredTOTP, Recovery: []string{fredRecovery}},
		{ID: "gina@pve", Password: ginaPassword, Privileges: admin, WebAuthn: true},
		{ID: "root@pam", Password: rootPassword, Privileges: admin},
	}}
}

// newApplianceHarness is pco web of the appliance: the sign-in with a
// password at the fake, whose certificate it pins.
func newApplianceHarness(t *testing.T, allowRoot bool) *harness {
	t.Helper()
	fake, srv := fakePVE(t, applianceUsers())
	trust := Trust{Pin: srv.Certificate()}
	h := &harness{
		t: t, fake: fake, pve: &switchPVE{PVE: NewTrustedPVE(apiURL(srv), trust, 5*time.Second)},
		clock: newClock(), log: &testutil.SyncBuffer{}, reached: map[string]int{},
	}
	h.auth = New(h.pve, Config{
		Now:       h.clock.Now,
		Rand:      rand.Reader,
		Hosts:     func() []string { return AllowedHosts("192.0.2.10:8643", "pve1", "pve1.example.lan") },
		Profile:   "appliance",
		Node:      "pve1",
		Version:   "0.3.0",
		Log:       zerolog.New(h.log),
		Login:     NewLogin(apiURL(srv), trust, 5*time.Second),
		AllowRoot: allowRoot,
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

func (b *browser) password(user, realm, password string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"user": user, "realm": realm, "password": password})
	return b.do(http.MethodPost, "/api/session/password", string(body))
}

func (b *browser) code(code string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"code": code})
	return b.do(http.MethodPost, "/api/session/second-factor", string(body))
}

func sessionOf(t *testing.T, rec *httptest.ResponseRecorder) wire.Session {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var s wire.Session
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &s))
	return s
}

func TestLoginAtTheFake(t *testing.T) {
	_, srv := fakePVE(t, applianceUsers())
	l := NewLogin(apiURL(srv), Trust{Pin: srv.Certificate()}, 5*time.Second)
	ctx := context.Background()

	ticket, ch, err := l.Password(ctx, "dora", "pve", doraPassword)
	require.NoError(t, err)
	require.Nil(t, ch)
	user, ok := ticketUser(ticket)
	require.True(t, ok)
	require.Equal(t, "dora@pve", user)

	_, _, err = l.Password(ctx, "dora", "pve", "wrong")
	require.ErrorIs(t, err, ErrRefused)
	_, _, err = l.Password(ctx, "nobody", "pve", "wrong")
	require.ErrorIs(t, err, ErrRefused)

	_, ch, err = l.Password(ctx, "fred", "pve", fredPassword)
	require.NoError(t, err)
	require.Equal(t, "fred@pve", ch.User)
	require.Equal(t, []string{"recovery", "totp"}, ch.Kinds)
	require.True(t, ch.Typed())
	_, err = l.SecondFactor(ctx, ch, "totp:000000")
	require.ErrorIs(t, err, ErrRefused)
	ticket, err = l.SecondFactor(ctx, ch, "totp:"+fredTOTP)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(ticket, "PVE:fred@pve:"))
	_, err = l.SecondFactor(ctx, ch, "recovery:"+fredRecovery)
	require.NoError(t, err)
	_, err = l.SecondFactor(ctx, ch, "recovery:"+fredRecovery)
	require.ErrorIs(t, err, ErrRefused, "a recovery code is good once")

	_, ch, err = l.Password(ctx, "gina", "pve", ginaPassword)
	require.NoError(t, err)
	require.Equal(t, []string{"webauthn"}, ch.Kinds)
	require.False(t, ch.Typed())

	renewed, err := l.Renew(ctx, "fred@pve", ticket)
	require.NoError(t, err)
	require.NotEqual(t, ticket, renewed)
	_, err = l.Renew(ctx, "fred@pve", "PVE:fred@pve:66F00000::bm90LWlzc3VlZA==")
	require.ErrorIs(t, err, ErrRefused)

	realms, err := l.Realms(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"pam", "pve"}, realms)
}

func TestLoginTrust(t *testing.T) {
	_, srv := fakePVE(t, applianceUsers())
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	cases := []struct {
		name  string
		trust Trust
		ok    bool
	}{
		{"the pin", Trust{Pin: srv.Certificate()}, true},
		{"another pin", Trust{Pin: otherCertificate(t)}, false},
		{"a CA and the name the certificate is for", Trust{Roots: roots, ServerName: "example.com"}, true},
		{"a CA and an address the certificate is for", Trust{Roots: roots, ServerName: "127.0.0.1"}, true},
		{"a CA and a name the certificate is not for", Trust{Roots: roots, ServerName: "pve1"}, false},
		{"another CA", Trust{Roots: x509.NewCertPool(), ServerName: "example.com"}, false},
		{"nothing", Trust{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := NewLogin(apiURL(srv), c.trust, 5*time.Second).Password(context.Background(), "dora", "pve", doraPassword)
			if c.ok {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrUnreachable)
			require.NotContains(t, err.Error(), doraPassword)
		})
	}
}

func TestPartialTickets(t *testing.T) {
	cases := []struct {
		ticket string
		kinds  []string
	}{
		{"PVE:!tfa!%7B%22totp%22%3Atrue%2C%22recovery%22%3A%22available%22%7D:66F20001::c2lnbmF0dXJl", []string{"recovery", "totp"}},
		{`PVE:!tfa!{"recovery":"unavailable","webauthn":{"publicKey":{"challenge":"x"}}}:66F20001::c2ln`, []string{"webauthn"}},
		{`PVE:!tfa!{"totp":false,"yubico":true,"u2f":null}:66F20001::c2ln`, []string{"yubico"}},
	}
	for _, c := range cases {
		data, ok := partialData(c.ticket)
		require.True(t, ok, c.ticket)
		kinds, err := challengeKinds(data)
		require.NoError(t, err, c.ticket)
		require.Equal(t, c.kinds, kinds, c.ticket)
	}
	_, ok := partialData("PVE:dora@pve:66F20001::c2ln")
	require.False(t, ok)
}

func TestSignInWithAPassword(t *testing.T) {
	cases := []struct {
		name, user, password string
		role                 string
		visible              []int
	}{
		{"an admin", "dora", doraPassword, "admin", []int{101, 102, 200, 300}},
		{"a reader", "erin", erinPassword, "reader", []int{102}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newApplianceHarness(t, false)
			b := h.browser("")
			s := sessionOf(t, b.password(c.user, "pve", c.password))
			require.Equal(t, c.user+"@pve", s.User)
			require.Equal(t, "password", s.Method)
			require.Equal(t, c.role, s.Role)
			require.Equal(t, "appliance", s.Profile)
			require.Equal(t, t0.Add(12*time.Hour), s.ExpiresAt)

			require.Equal(t, http.StatusOK, b.do(http.MethodGet, "/api/v1/state", "").Code)
			rec := b.do(http.MethodGet, "/api/v1/visible", "")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var got struct{ VMIDs []int }
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			require.Equal(t, c.visible, got.VMIDs)
			require.Equal(t, http.StatusOK, b.do(http.MethodGet, "/api/session", "").Code)
		})
	}
}

func TestAWrongPassword(t *testing.T) {
	h := newApplianceHarness(t, false)
	rec := h.browser("").password("dora", "pve", "not-the-password")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	e := errorOf(t, rec)
	require.Equal(t, wire.CodeTicketInvalid, e.Code)
	require.Equal(t, "Proxmox VE did not accept the user and password", e.Error)
	require.Empty(t, rec.Result().Cookies())
}

func TestASignInWithTOTPAndARecoveryCode(t *testing.T) {
	for _, code := range []string{"424 242", fredRecovery} {
		t.Run(code, func(t *testing.T) {
			h := newApplianceHarness(t, false)
			b := h.browser("")
			rec := b.password("fred", "pve", fredPassword)
			require.Equal(t, http.StatusUnauthorized, rec.Code)
			e := errorOf(t, rec)
			require.Equal(t, wire.CodeSecondFactor, e.Code)
			require.Equal(t, []string{"recovery", "totp"}, e.Kinds)
			require.NotEmpty(t, b.step)
			require.Empty(t, b.session, "no session before the second factor")

			s := sessionOf(t, b.code(code))
			require.Equal(t, "fred@pve", s.User)
			require.Equal(t, "admin", s.Role)
			require.Empty(t, b.step, "the step is over")
			require.Equal(t, http.StatusOK, b.do(http.MethodPost, "/api/v1/sync", "{}").Code)
		})
	}
}

func TestASecurityKeyCannotWorkHere(t *testing.T) {
	h := newApplianceHarness(t, false)
	b := h.browser("")
	rec := b.password("gina", "pve", ginaPassword)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	e := errorOf(t, rec)
	require.Equal(t, wire.CodeSecondFactorKey, e.Code)
	require.Equal(t, "This account's second factor is a security key, which works only on Proxmox VE's own page. "+
		"Use a TOTP or recovery code, or an API token.", e.Error)
	require.Empty(t, b.step)
	require.Empty(t, b.session)
}

func TestTheSecondFactorStepLastsTwoMinutes(t *testing.T) {
	h := newApplianceHarness(t, false)
	b := h.browser("")
	require.Equal(t, http.StatusUnauthorized, b.password("fred", "pve", fredPassword).Code)
	h.clock.Add(2 * time.Minute)

	rec := b.code(fredTOTP)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	e := errorOf(t, rec)
	require.Equal(t, wire.CodeTicketInvalid, e.Code)
	require.Contains(t, e.Error, "Start again with the password")
	require.Zero(t, h.fake.Calls("access/ticket")-1, "the code never reached Proxmox VE")
}

func TestThreeWrongCodesVoidTheStep(t *testing.T) {
	h := newApplianceHarness(t, false)
	b := h.browser("")
	require.Equal(t, http.StatusUnauthorized, b.password("fred", "pve", fredPassword).Code)
	for i := 1; i <= 2; i++ {
		rec := b.code("000000")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		require.Equal(t, wire.CodeSecondFactor, errorOf(t, rec).Code, "wrong code %d", i)
	}
	rec := b.code("000000")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	e := errorOf(t, rec)
	require.Equal(t, wire.CodeTicketInvalid, e.Code)
	require.Contains(t, e.Error, "Start again with the password")
	require.Empty(t, b.step)

	b.step = cookieOfLastStep(t, h)
	rec = b.code(fredTOTP)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "the right code is too late")
	require.Equal(t, wire.CodeTicketInvalid, errorOf(t, rec).Code)

	h.auth.lockout.mu.Lock()
	n := h.auth.lockout.byAccount["fred@pve"].Value.(*failures).n
	h.auth.lockout.mu.Unlock()
	require.Equal(t, 1, n, "a voided step is one failure of the account")
}

// cookieOfLastStep is a step id as an attacker who kept the cookie would send
// it: one that no longer is.
func cookieOfLastStep(t *testing.T, h *harness) string {
	t.Helper()
	h.auth.pending.mu.Lock()
	defer h.auth.pending.mu.Unlock()
	require.Empty(t, h.auth.pending.byID)
	return "gone-step-id"
}

func TestTheTicketIsRenewedEvery15Minutes(t *testing.T) {
	h := newApplianceHarness(t, false)
	b := h.browser("")
	sessionOf(t, b.password("dora", "pve", doraPassword))
	first := h.fake.Tickets("dora@pve")
	require.Len(t, first, 1)
	calls := h.fake.Calls("access/ticket")

	h.clock.Add(14 * time.Minute)
	require.Equal(t, http.StatusOK, b.do(http.MethodGet, "/api/v1/state", "").Code)
	require.Equal(t, calls, h.fake.Calls("access/ticket"), "not yet")

	h.clock.Add(time.Minute)
	require.Equal(t, http.StatusOK, b.do(http.MethodGet, "/api/v1/state", "").Code)
	require.Equal(t, calls+1, h.fake.Calls("access/ticket"))
	renewed := h.fake.Tickets("dora@pve")
	require.Len(t, renewed, 2)
	s, ok := h.auth.sessions.get(b.session)
	require.True(t, ok)
	require.Equal(t, renewed[1], s.ticket, "the session holds the new ticket")

	// Proxmox VE refuses the next renewal: the session ends.
	h.fake.RemoveTicket(renewed[1])
	h.clock.Add(15 * time.Minute)
	rec := b.do(http.MethodGet, "/api/v1/state", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	_, ok = h.auth.sessions.get(b.session)
	require.False(t, ok)
	require.Contains(t, h.log.String(), "Proxmox VE did not renew the ticket")
}

func TestARenewalProxmoxDoesNotAnswerKeepsTheSession(t *testing.T) {
	h := newApplianceHarness(t, false)
	b := h.browser("")
	sessionOf(t, b.password("dora", "pve", doraPassword))
	h.clock.Add(15 * time.Minute)
	h.auth.login = downLogin{}

	rec := b.do(http.MethodGet, "/api/v1/state", "")

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	_, ok := h.auth.sessions.get(b.session)
	require.True(t, ok, "the ticket is good for two hours: the next request tries again")
}

type downLogin struct{ Login }

func (downLogin) Renew(context.Context, string, string) (string, error) { return "", ErrUnreachable }

func TestThePasswordGoesNowhere(t *testing.T) {
	h := newApplianceHarness(t, true)
	h.auth.cfg.Log = h.auth.cfg.Log.Level(zerolog.TraceLevel)
	var answers []string
	for _, c := range []struct{ user, password string }{
		{"dora", doraPassword},
		{"dora", "wrong-" + doraPassword},
		{"fred", fredPassword},
		{"gina", ginaPassword},
		{"root", rootPassword},
		{doraPassword, doraPassword},
	} {
		b := h.browser("")
		rec := b.password(c.user, "pve", c.password)
		answers = append(answers, rec.Body.String())
		if b.step != "" {
			answers = append(answers, b.code("000000").Body.String())
		}
	}
	for _, secret := range []string{doraPassword, fredPassword, ginaPassword, rootPassword} {
		require.NotContains(t, h.log.String(), secret, "the log")
		for _, a := range answers {
			require.NotContains(t, a, secret, "an answer")
		}
		h.auth.sessions.mu.Lock()
		for _, s := range h.auth.sessions.byID {
			require.NotContains(t, fmt.Sprintf("%#v", *s), secret, "a session")
		}
		h.auth.sessions.mu.Unlock()
		h.auth.pending.mu.Lock()
		for _, s := range h.auth.pending.byID {
			require.NotContains(t, fmt.Sprintf("%#v %#v", *s, *s.challenge), secret, "a pending step")
		}
		h.auth.pending.mu.Unlock()
	}
}

func TestTheRoutesOfEachProfile(t *testing.T) {
	host := newHarness(t)
	for _, path := range []string{"/api/session/password", "/api/session/second-factor"} {
		require.Equal(t, http.StatusNotFound, host.browser("").do(http.MethodPost, path, "{}").Code, path)
	}
	appliance := newApplianceHarness(t, false)
	require.Equal(t, http.StatusNotFound, appliance.browser(aliceTicket).do(http.MethodPost, "/api/session/ticket", "{}").Code)

	rec := appliance.browser("").do(http.MethodGet, "/api/session", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	var u wire.Unauthenticated
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &u))
	require.Equal(t, wire.Unauthenticated{Code: wire.CodeUnauthenticated, Methods: []string{"password", "token"}, Realms: []string{"pam", "pve"}}, u)
	appliance.browser("").do(http.MethodGet, "/api/session", "")
	require.Equal(t, 1, appliance.fake.Calls("access/domains"), "the realms are kept")
}

func TestRootOnThePasswordForm(t *testing.T) {
	h := newApplianceHarness(t, false)
	for _, user := range []string{"root", "Root"} {
		rec := h.browser("").password(user, "pam", rootPassword)
		require.Equal(t, http.StatusForbidden, rec.Code)
		e := errorOf(t, rec)
		require.Equal(t, wire.CodeForbidden, e.Code)
		require.Equal(t, "Sign in with a user that has Sys.Modify on /, or with an API token", e.Error)
	}
	require.Zero(t, h.fake.Calls("access/ticket"), "root's password never left the request")

	allowed := newApplianceHarness(t, true)
	s := sessionOf(t, allowed.browser("").password("root", "pam", rootPassword))
	require.Equal(t, "root@pam", s.User)
}

func TestTheAccountLock(t *testing.T) {
	h := newApplianceHarness(t, false)
	try := func(i int, user, password string) *httptest.ResponseRecorder {
		b := h.browser("")
		// Each from an address of its own: the limit of an address is not
		// what this test is about.
		b.peer = "192.0.2." + strconv.Itoa(100+i) + ":40000"
		return b.password(user, "pve", password)
	}
	for i := range 5 {
		require.Equal(t, http.StatusUnauthorized, try(i, "dora", "wrong").Code)
	}
	calls := h.fake.Calls("access/ticket")
	rec := try(5, "dora", doraPassword)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	e := errorOf(t, rec)
	require.Equal(t, wire.CodeRateLimited, e.Code)
	require.Equal(t, 60, e.RetryAfter)
	require.Equal(t, "60", rec.Header().Get("Retry-After"))
	require.Equal(t, calls, h.fake.Calls("access/ticket"), "a locked account is refused before Proxmox VE is asked")
	require.Equal(t, 429, try(6, "DORA", doraPassword).Code, "in any spelling")

	sessionOf(t, try(7, "erin", erinPassword))
	tok := h.browser("")
	tok.peer = "192.0.2.50:40000"
	require.Equal(t, http.StatusOK, tok.signInToken("dora@pve!pco="+tokenSecret).Code, "the token goes past the lock")

	h.clock.Add(time.Minute)
	require.Equal(t, http.StatusUnauthorized, try(8, "dora", "wrong").Code)
	rec = try(9, "dora", doraPassword)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, 120, errorOf(t, rec).RetryAfter, "the sixth failure doubles the lock")

	h.clock.Add(2 * time.Minute)
	sessionOf(t, try(10, "dora", doraPassword))
	require.Equal(t, http.StatusUnauthorized, try(11, "dora", "wrong").Code, "a success cleared the count")
	require.Equal(t, http.StatusUnauthorized, try(12, "dora", "wrong").Code)
}

func TestTheSignInsOfAnAddressAreLimited(t *testing.T) {
	h := newApplianceHarness(t, false)
	for i := range 10 {
		b := h.browser("")
		require.NotEqual(t, http.StatusTooManyRequests, b.password("user"+strconv.Itoa(i), "pve", "wrong").Code)
	}
	rec := h.browser("").password("dora", "pve", doraPassword)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, wire.CodeRateLimited, errorOf(t, rec).Code)
	rec = h.browser("").code(fredTOTP)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "the codes count with them")
}

func TestAMalformedPasswordSignIn(t *testing.T) {
	h := newApplianceHarness(t, false)
	cases := []struct {
		user, realm, password, field string
	}{
		{"", "pve", "x", "user"},
		{"do ra", "pve", "x", "user"},
		{"dora", "", "x", "realm"},
		{"dora", "p ve", "x", "realm"},
		{"dora", "pve", "", "password"},
	}
	for i, c := range cases {
		b := h.browser("")
		b.peer = "192.0.2." + strconv.Itoa(100+i) + ":40000"
		rec := b.password(c.user, c.realm, c.password)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Equal(t, c.field, errorOf(t, rec).Field)
	}
	require.Zero(t, h.fake.Calls("access/ticket"))
}
