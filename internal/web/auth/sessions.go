package auth

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Role is what a session may do.
type Role int

const (
	RoleNone Role = iota
	RoleReader
	RoleAdmin
)

func (r Role) String() string {
	switch r {
	case RoleAdmin:
		return "admin"
	case RoleReader:
		return "reader"
	}
	return "none"
}

// The methods of a sign-in.
const (
	MethodTicket = "ticket"
	MethodToken  = "token"
)

// Principal is who a session is.
type Principal struct {
	User   string // "alice@pve", "alice@pve!pco" for a token
	Method string // "ticket", "token" ("password" in the appliance)
	Role   Role
}

// Actor is the principal as the daemon records it with a change, in the
// Pco-Actor header: "alice@pve (ticket)".
func (p Principal) Actor() string { return p.User + " (" + p.Method + ")" }

// actorForm is what the daemon accepts as Pco-Actor. A user whose name does
// not fit is refused at sign-in, so that every change a session makes can be
// recorded as its own.
var actorForm = regexp.MustCompile(`^[A-Za-z0-9._@!:() -]{1,128}$`)

const (
	idleTimeout    = 30 * time.Minute
	ticketLifetime = 12 * time.Hour
	tokenLifetime  = 8 * time.Hour
	tokenCheckAge  = time.Minute
	perUser        = 5
	maxSessions    = 500
)

// Session is a signed-in browser. It lives in memory only.
type Session struct {
	ID, CSRF  string
	Principal Principal

	token   string    // token sessions: the token
	checked time.Time // token sessions: when Proxmox VE last accepted it

	visible   map[int]bool // readers: the VMIDs they may see
	visibleOf string       // the hash of visible
	visibleAt time.Time

	created  time.Time
	lastSeen time.Time
}

func (s *Session) idleExpiresAt() time.Time { return s.lastSeen.Add(idleTimeout) }

func (s *Session) expiresAt() time.Time {
	if s.Principal.Method == MethodToken {
		return s.created.Add(tokenLifetime)
	}
	return s.created.Add(ticketLifetime)
}

func (s *Session) live(now time.Time) bool {
	return now.Before(s.idleExpiresAt()) && now.Before(s.expiresAt())
}

type lookupState int

const (
	missing lookupState = iota
	expired
	found
)

var errNoSession = errors.New("no such session")

// store holds the sessions. Its methods hand out copies, so that a request
// reads its session without a lock.
type store struct {
	mu   sync.Mutex
	byID map[string]*Session
}

func newStore() *store { return &store{byID: map[string]*Session{}} }

// add puts s in, and returns the sessions it evicted: the user's oldest
// beyond five, and the oldest of all beyond 500. Sessions that are over are
// dropped first and do not count.
func (st *store) add(s *Session, now time.Time) []Session {
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, old := range st.byID {
		if !old.live(now) {
			delete(st.byID, id)
		}
	}
	var evicted []Session
	evict := func(user string) {
		var oldest *Session
		for _, o := range st.byID {
			if (user == "" || o.Principal.User == user) && (oldest == nil || o.created.Before(oldest.created)) {
				oldest = o
			}
		}
		delete(st.byID, oldest.ID)
		evicted = append(evicted, *oldest)
	}
	for st.count(s.Principal.User) >= perUser {
		evict(s.Principal.User)
	}
	for len(st.byID) >= maxSessions {
		evict("")
	}
	st.byID[s.ID] = s
	return evicted
}

func (st *store) count(user string) int {
	n := 0
	for _, s := range st.byID {
		if s.Principal.User == user {
			n++
		}
	}
	return n
}

// lookup finds the session id; one that is over is dropped.
func (st *store) lookup(id string, now time.Time) (Session, lookupState) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.byID[id]
	switch {
	case !ok:
		return Session{}, missing
	case !s.live(now):
		delete(st.byID, id)
		return *s, expired
	}
	return *s, found
}

// get is the session id as it is, over or not.
func (st *store) get(id string) (Session, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.byID[id]
	if !ok {
		return Session{}, false
	}
	return *s, true
}

// touch is activity: the idle timer starts again.
func (st *store) touch(id string, now time.Time) (Session, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.byID[id]
	if !ok || !s.live(now) {
		delete(st.byID, id)
		return Session{}, errNoSession
	}
	s.lastSeen = now
	return *s, nil
}

// update changes the session id with fn, when it is still there.
func (st *store) update(id string, fn func(*Session)) (Session, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.byID[id]
	if !ok {
		return Session{}, false
	}
	fn(s)
	return *s, true
}

func (st *store) remove(id string) (Session, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.byID[id]
	if !ok {
		return Session{}, false
	}
	delete(st.byID, id)
	return *s, true
}

const (
	cookieName       = "__Host-pco-session"
	ticketCookieName = "PVEAuthCookie"
)

// cookie is the session's: the __Host- prefix makes the browser refuse it
// with a Domain, without Secure or on another path, and without Max-Age it
// dies with the browser.
func cookie(id string) *http.Cookie {
	return &http.Cookie{Name: cookieName, Value: id, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode}
}

func clearedCookie() *http.Cookie {
	c := cookie("")
	c.MaxAge = -1
	return c
}

func cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

// session finds the session of the request without asking Proxmox VE, and
// answers 401 when there is none.
func (a *Auth) session(c *gin.Context) (Session, bool) {
	s, ok := a.lookup(c)
	if !ok {
		a.unauthenticated(c)
	}
	return s, ok
}

// lookup finds the session of the request; one that has just ended is logged.
func (a *Auth) lookup(c *gin.Context) (Session, bool) {
	s, state := a.sessions.lookup(cookieValue(c.Request, cookieName), a.cfg.Now())
	if state == expired {
		a.signedOut(c, s, "expired")
	}
	if state != found {
		return Session{}, false
	}
	return s, true
}

// check asks Proxmox VE whether the session still holds, as its method
// says: the ticket of the request, through the cache of 30 s, or the token
// once a minute, one call at a time for each session. Either sets the role
// again. A ticket that is gone, refused or another user's ends the session;
// Proxmox VE out of reach refuses the request and keeps the session. The
// session is bound to its user: a renewed ticket of the same user is the
// session's from then on.
func (a *Auth) check(c *gin.Context, s Session) (Session, bool) {
	var role Role
	switch s.Principal.Method {
	case MethodTicket:
		ticket := cookieValue(c.Request, ticketCookieName)
		if ticket == "" {
			return a.end(c, s, "the browser has no Proxmox VE ticket any more")
		}
		got, err := a.tickets.check(c.Request.Context(), ticket)
		switch {
		case errors.Is(err, ErrRefused):
			return a.end(c, s, "the ticket is no longer valid")
		case err != nil:
			a.unreachable(c, err)
			return Session{}, false
		case got.user != s.Principal.User:
			return a.end(c, s, "the ticket is another user's")
		case got.role == RoleNone:
			return a.end(c, s, "no Sys.Audit on /")
		}
		role = got.role
	case MethodToken:
		if a.cfg.Now().Sub(s.checked) < tokenCheckAge {
			return s, true
		}
		var err error
		role, err = a.tokenChecks.do(c.Request.Context(), s.ID, func(ctx context.Context) (Role, error) {
			return a.checkToken(ctx, s)
		})
		switch {
		case errors.Is(err, ErrRefused):
			return a.end(c, s, "the token is no longer valid")
		case err != nil:
			a.unreachable(c, err)
			return Session{}, false
		case role == RoleNone:
			return a.end(c, s, "no Sys.Audit on /")
		}
	}
	s, ok := a.sessions.update(s.ID, func(x *Session) { x.Principal.Role = role })
	if !ok {
		a.unauthenticated(c)
	}
	return s, ok
}

// checkToken asks Proxmox VE about the token of s and keeps the answer in the
// session, unless a check that ended after the request read its session has
// done so already.
func (a *Auth) checkToken(ctx context.Context, s Session) (Role, error) {
	now := a.cfg.Now()
	if cur, ok := a.sessions.get(s.ID); ok && now.Sub(cur.checked) < tokenCheckAge {
		return cur.Principal.Role, nil
	}
	privs, err := a.pve.Privileges(ctx, Credential{Token: s.token}, "/")
	if err != nil {
		return RoleNone, err
	}
	role := roleOf(privs)
	if role != RoleNone {
		a.sessions.update(s.ID, func(x *Session) { x.checked, x.Principal.Role = now, role })
	}
	return role, nil
}

// end ends the session s, and answers 401.
func (a *Auth) end(c *gin.Context, s Session, why string) (Session, bool) {
	if _, ok := a.sessions.remove(s.ID); ok {
		a.signedOut(c, s, "ended: "+why)
	}
	a.unauthenticated(c)
	return Session{}, false
}

// seen is the activity of a request the user caused: not one the page makes
// by itself (Pco-Background: 1), nor the stream.
func (a *Auth) seen(c *gin.Context, s Session) Session {
	if c.GetHeader(headerBackground) == "1" || c.Request.URL.Path == StreamPath {
		return s
	}
	if t, err := a.sessions.touch(s.ID, a.cfg.Now()); err == nil {
		return t
	}
	return s
}
