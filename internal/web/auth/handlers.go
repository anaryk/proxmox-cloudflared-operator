// Package auth signs users of the web interface in with what Proxmox VE
// already knows of them: the ticket of its own page, which the browser sends
// to every port of the node's name, or a pasted API token. The role comes
// from their privileges at /, and readers see the guests they may audit.
// Sessions live in the memory of the web process only.
package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/wire"
)

// Config is what the sessions are made with.
type Config struct {
	Now   func() time.Time
	Rand  io.Reader       // of session ids and CSRF tokens; crypto/rand without it
	Hosts func() []string // the accepted Host headers, name and port: AllowedHosts
	// Profile, Node, Zone (the node's time zone) and Version are told to the
	// page with the session.
	Profile string
	Node    string
	Zone    string
	Version string
	Log     zerolog.Logger
}

// Auth is the sign-in and the sessions of the web interface.
type Auth struct {
	pve      PVE
	cfg      Config
	sessions *store
	tickets  *ticketChecks
	limit    *limiter
}

// New returns the sessions of the web process, checked with p.
func New(p PVE, cfg Config) *Auth {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	return &Auth{
		pve:      p,
		cfg:      cfg,
		sessions: newStore(),
		tickets:  newTicketChecks(p, cfg.Now),
		limit:    newLimiter(signInsPerMinute, limitWindow, maxLimitedPeers),
	}
}

// Mount adds the routes of /api/session. None of their answers is ever
// compressed: they carry the CSRF token, and a compressed secret beside text
// an attacker chooses is what attacks on the length read.
func (a *Auth) Mount(r gin.IRouter) {
	g := r.Group("/api/session")
	g.GET("", a.current)
	g.DELETE("", a.signOut)
	g.POST("/ticket", a.signInTicket)
	g.POST("/token", a.signInToken)
	g.POST("/touch", a.Require(RoleReader), a.touch)
}

func (a *Auth) current(c *gin.Context) {
	if !a.admit(c) {
		return
	}
	s, ok := a.session(c)
	if !ok {
		return
	}
	if s, ok = a.check(c, s); !ok {
		return
	}
	c.JSON(http.StatusOK, a.answer(a.seen(c, s)))
}

// touch is the activity of a user reading the page: it starts the idle timer
// again and does nothing else.
func (a *Auth) touch(c *gin.Context) {
	if _, err := a.sessions.touch(SessionOf(c).ID, a.cfg.Now()); err != nil {
		a.unauthenticated(c)
		return
	}
	c.Status(http.StatusNoContent)
}

// signOut ends the session. Proxmox VE is not asked: signing out works while
// it is out of reach.
func (a *Auth) signOut(c *gin.Context) {
	if !a.admit(c) {
		return
	}
	s, ok := a.session(c)
	if !ok {
		return
	}
	if !csrfOK(c, s) {
		refuse(c, http.StatusForbidden, wire.Error{Error: "the request does not carry the token of this session", Code: wire.CodeForbidden})
		return
	}
	if _, ok := a.sessions.remove(s.ID); ok {
		a.signedOut(c, s, "signed out")
	}
	http.SetCookie(c.Writer, clearedCookie())
	c.Status(http.StatusNoContent)
}

func (a *Auth) signInTicket(c *gin.Context) {
	if !a.admit(c) || !readJSON(c, &struct{}{}) {
		return
	}
	ticket := cookieValue(c.Request, ticketCookieName)
	if ticket == "" {
		a.signInRefused(c, Principal{Method: MethodTicket}, "no ticket", http.StatusUnauthorized, wire.Error{
			Error: "this browser has no Proxmox VE session for this name; sign in to Proxmox VE at the same address first",
			Code:  wire.CodeTicketInvalid,
		})
		return
	}
	got, err := a.tickets.check(c.Request.Context(), ticket)
	p := Principal{User: got.user, Method: MethodTicket, Role: got.role}
	switch {
	case errors.Is(err, ErrRefused):
		a.signInRefused(c, p, "Proxmox VE did not accept the ticket", http.StatusUnauthorized, wire.Error{
			Error: "Proxmox VE did not accept the session of this browser; sign in to Proxmox VE again",
			Code:  wire.CodeTicketInvalid,
		})
		return
	case err != nil:
		a.signInUnreachable(c, p, err)
		return
	}
	a.start(c, p, func(s *Session) { s.binding = sha256.Sum256([]byte(ticket)) })
}

// tokenForm is an API token as Proxmox VE makes them: user@realm!id=uuid.
var tokenForm = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+![A-Za-z0-9._-]+=[0-9a-fA-F-]{36}$`)

func (a *Auth) signInToken(c *gin.Context) {
	if !a.admit(c) {
		return
	}
	p := Principal{Method: MethodToken}
	if ok, wait := a.limit.allow(client(c.Request), a.cfg.Now()); !ok {
		after := int(math.Ceil(wait.Seconds()))
		c.Header("Retry-After", strconv.Itoa(after))
		a.signInRefused(c, p, "too many attempts", http.StatusTooManyRequests, wire.Error{
			Error:      "too many sign-ins from this address; try again in " + strconv.Itoa(after) + " s",
			Code:       wire.CodeRateLimited,
			RetryAfter: after,
		})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if !readJSON(c, &body) {
		return
	}
	if !tokenForm.MatchString(body.Token) {
		a.signInRefused(c, p, "malformed token", http.StatusBadRequest, wire.Error{
			Error: "this is not an API token of the form user@realm!id=secret",
			Code:  wire.CodeInvalid,
			Field: "token",
		})
		return
	}
	p.User, _, _ = strings.Cut(body.Token, "=")
	privs, err := a.pve.Privileges(c.Request.Context(), Credential{Token: body.Token}, "/")
	p.Role = roleOf(privs)
	switch {
	case errors.Is(err, ErrRefused):
		a.signInRefused(c, p, "Proxmox VE did not accept the token", http.StatusUnauthorized, wire.Error{
			Error: "Proxmox VE did not accept the token",
			Code:  wire.CodeTicketInvalid,
		})
		return
	case err != nil:
		a.signInUnreachable(c, p, err)
		return
	}
	now := a.cfg.Now()
	a.start(c, p, func(s *Session) { s.token, s.checked = body.Token, now })
}

// start makes a new session for p, whose role Proxmox VE just gave, unless
// that role is none or the user cannot be named as the actor of a change.
// The session the browser had is over: an id is never kept across a sign-in,
// so one an attacker planted is worth nothing.
func (a *Auth) start(c *gin.Context, p Principal, bind func(*Session)) {
	switch {
	case p.Role == RoleNone:
		a.signInRefused(c, p, "no Sys.Audit on /", http.StatusForbidden, wire.Error{
			Error:   "pco needs Sys.Audit on / to show you anything, and Sys.Modify on / to let you change it",
			Code:    wire.CodeForbidden,
			Missing: "Sys.Audit",
		})
		return
	case !actorForm.MatchString(p.Actor()):
		a.signInRefused(c, p, "the user name has characters pco does not record", http.StatusForbidden, wire.Error{
			Error: "pco cannot record changes under this user name: it allows letters, digits and . _ - @ ! only; sign in with another user or an API token",
			Code:  wire.CodeForbidden,
		})
		return
	}
	id, err := secret(a.cfg.Rand)
	if err != nil {
		a.internal(c, err)
		return
	}
	csrf, err := secret(a.cfg.Rand)
	if err != nil {
		a.internal(c, err)
		return
	}
	if old, ok := a.sessions.remove(cookieValue(c.Request, cookieName)); ok {
		a.signedOut(c, old, "replaced by a new sign-in")
	}
	now := a.cfg.Now()
	s := &Session{ID: id, CSRF: csrf, Principal: p, created: now, lastSeen: now}
	bind(s)
	for _, gone := range a.sessions.add(s, now) {
		a.signedOut(c, gone, "evicted")
	}
	a.cfg.Log.Info().Str("user", p.User).Str("method", p.Method).Str("client", client(c.Request)).
		Str("result", "ok").Str("role", p.Role.String()).Msg("sign-in")
	http.SetCookie(c.Writer, cookie(id))
	c.JSON(http.StatusOK, a.answer(*s))
}

func (a *Auth) answer(s Session) wire.Session {
	return wire.Session{
		User:          s.Principal.User,
		Method:        s.Principal.Method,
		Role:          s.Principal.Role.String(),
		CSRF:          s.CSRF,
		IdleExpiresAt: s.idleExpiresAt().UTC(),
		ExpiresAt:     s.expiresAt().UTC(),
		Profile:       a.cfg.Profile,
		Node:          a.cfg.Node,
		NodeZone:      a.cfg.Zone,
		Version:       a.cfg.Version,
	}
}

const maxBody = 4 << 10

// readJSON decodes the body, a JSON object of exactly the fields of v, and
// answers 400 when it is not.
func readJSON(c *gin.Context, v any) bool {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBody+1))
	if err == nil && len(body) <= maxBody && bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err = dec.Decode(v); err == nil && !dec.More() {
			if _, err = dec.Token(); errors.Is(err, io.EOF) {
				return true
			}
		}
	}
	refuse(c, http.StatusBadRequest, wire.Error{Error: "the request is not the JSON this call takes", Code: wire.CodeInvalid})
	return false
}

func refuse(c *gin.Context, status int, body wire.Error) {
	c.AbortWithStatusJSON(status, body)
}

// unauthenticated is the answer without a session: how to sign in.
func (a *Auth) unauthenticated(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, wire.Unauthenticated{
		Code:    wire.CodeUnauthenticated,
		Methods: []string{MethodTicket, MethodToken},
		Ticket:  cookieValue(c.Request, ticketCookieName) != "",
	})
}

func (a *Auth) unreachable(c *gin.Context, err error) {
	a.cfg.Log.Warn().Err(err).Str("path", c.Request.URL.Path).Msg("checking a session with Proxmox VE")
	refuse(c, http.StatusServiceUnavailable, wire.Error{
		Error: "Proxmox VE on this node does not answer, or presents another certificate than the node's",
		Code:  wire.CodeProxmoxUnreachable,
	})
}

func (a *Auth) signInUnreachable(c *gin.Context, p Principal, err error) {
	a.cfg.Log.Warn().Err(err).Msg("checking a sign-in with Proxmox VE")
	a.signInRefused(c, p, "Proxmox VE cannot be reached", http.StatusServiceUnavailable, wire.Error{
		Error: "Proxmox VE on this node does not answer, or presents another certificate than the node's",
		Code:  wire.CodeProxmoxUnreachable,
	})
}

func (a *Auth) internal(c *gin.Context, err error) {
	a.cfg.Log.Error().Err(err).Msg("making a session")
	refuse(c, http.StatusInternalServerError, wire.Error{Error: "pco web could not make a session", Code: "internal"})
}

// signInRefused logs and answers a refused sign-in. The user is in the line
// only when Proxmox VE accepted the credential, or it is the id of a token.
func (a *Auth) signInRefused(c *gin.Context, p Principal, why string, status int, body wire.Error) {
	ev := a.cfg.Log.Warn()
	if p.User != "" {
		ev = ev.Str("user", p.User)
	}
	ev.Str("method", p.Method).Str("client", client(c.Request)).Str("result", "refused: "+why).Msg("sign-in")
	refuse(c, status, body)
}

func (a *Auth) signedOut(c *gin.Context, s Session, result string) {
	a.cfg.Log.Info().Str("user", s.Principal.User).Str("method", s.Principal.Method).
		Str("client", client(c.Request)).Str("result", result).Msg("sign-out")
}
