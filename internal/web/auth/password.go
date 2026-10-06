package auth

import (
	"context"
	"errors"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/wire"
)

// The sign-in of the appliance with the user and password of Proxmox VE. The
// password is read from the request, handed to Proxmox VE and dropped when
// the call returns: it is never logged, kept or put in the session. When
// Proxmox VE asks for a second factor, the partial ticket it gave waits in
// memory for two minutes, bound to the browser by a cookie of its own.

const (
	stepCookieName  = "__Host-pco-second-factor"
	stepLife        = 2 * time.Minute
	maxWrongCodes   = 3
	maxPendingSteps = 1000
	realmsAge       = 5 * time.Minute
	realmsRetry     = 30 * time.Second
)

var (
	// userForm is a user name pco can record as the actor of a change; one
	// that does not fit would be refused after Proxmox VE took the password.
	userForm = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)
	// realmForm is a realm id as Proxmox VE allows them.
	realmForm = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{1,31}$`)
	// totpForm is the code of an authenticator app; anything else is taken
	// for a recovery code.
	totpForm = regexp.MustCompile(`^[0-9]{6,8}$`)
)

// keySentence is the refusal of an account whose only second factor is a
// security key: WebAuthn binds it to Proxmox VE's own origin.
const keySentence = "This account's second factor is a security key, which works only on Proxmox VE's own page. " +
	"Use a TOTP or recovery code, or an API token."

// rootRefused is the refusal of root@pam: the appliance is reachable from more
// places than the node's own page, and root's password is the one most worth
// guessing. PCO_WEB_ALLOW_ROOT=1 allows it.
const rootRefused = "Sign in with a user that has Sys.Modify on /, or with an API token"

func (a *Auth) signInPassword(c *gin.Context) {
	if !a.admit(c) {
		return
	}
	p := Principal{Method: MethodPassword}
	if !a.allowAttempt(c, p) {
		return
	}
	var body struct {
		User     string `json:"user"`
		Realm    string `json:"realm"`
		Password string `json:"password"`
	}
	if !readJSON(c, &body) {
		return
	}
	user, realm := strings.TrimSpace(body.User), strings.TrimSpace(body.Realm)
	if userForm.MatchString(user) && realmForm.MatchString(realm) {
		user, realm = ownRealm(user, realm, a.realms.get(c.Request.Context(), a.cfg.Log))
	}
	switch {
	case !userForm.MatchString(user):
		a.signInRefused(c, p, "a user name pco does not record", http.StatusBadRequest, wire.Error{
			Error: "pco cannot record changes under this user name: it allows letters, digits and . _ - @ only; " +
				"sign in with another user or an API token",
			Code:  wire.CodeInvalid,
			Field: "user",
		})
		return
	case !realmForm.MatchString(realm):
		a.signInRefused(c, p, "no realm", http.StatusBadRequest, wire.Error{Error: "choose the realm of the user", Code: wire.CodeInvalid, Field: "realm"})
		return
	case body.Password == "":
		a.signInRefused(c, p, "no password", http.StatusBadRequest, wire.Error{Error: "the password is empty", Code: wire.CodeInvalid, Field: "password"})
		return
	case !a.cfg.AllowRoot && strings.EqualFold(user, "root") && strings.EqualFold(realm, "pam"):
		// Refused before Proxmox VE is asked: root's password never leaves
		// this request.
		a.signInRefused(c, p, "root@pam on the password form", http.StatusForbidden, wire.Error{Error: rootRefused, Code: wire.CodeForbidden})
		return
	}
	account := accountKey(user, realm)
	if a.refuseLocked(c, p, account) {
		return
	}
	ticket, ch, err := a.login.Password(c.Request.Context(), user, realm, body.Password)
	body.Password = ""
	switch {
	case errors.Is(err, ErrRefused):
		a.failed(c, p, account, "Proxmox VE did not accept the user and password")
		refuse(c, http.StatusUnauthorized, wire.Error{Error: "Proxmox VE did not accept the user and password", Code: wire.CodeTicketInvalid})
	case errors.Is(err, ErrUnknownChallenge):
		a.unknownChallenge(c, p, err)
	case err != nil:
		a.signInUnreachable(c, p, err)
	case ch != nil:
		a.askSecondFactor(c, account, ch)
	default:
		a.lockout.clear(account)
		a.startPassword(c, ticket)
	}
}

// askSecondFactor keeps the challenge for the code, or refuses an account
// whose second factor cannot work here.
func (a *Auth) askSecondFactor(c *gin.Context, account string, ch *Challenge) {
	p := Principal{User: ch.User, Method: MethodPassword}
	if !ch.Typed() {
		a.signInRefused(c, p, "the second factor is a security key", http.StatusUnauthorized, wire.Error{
			Error: keySentence, Code: wire.CodeSecondFactorKey, Kinds: ch.Kinds,
		})
		return
	}
	id, err := secret(a.cfg.Rand)
	if err != nil {
		a.internal(c, err)
		return
	}
	now := a.cfg.Now()
	a.pending.add(&pendingStep{id: id, account: account, challenge: ch, created: now}, now)
	http.SetCookie(c.Writer, stepCookie(id))
	a.cfg.Log.Info().Str("user", ch.User).Str("method", MethodPassword).Str("client", client(c.Request)).
		Str("result", "second factor asked").Msg("sign-in")
	refuse(c, http.StatusUnauthorized, wire.Error{
		Error: "Proxmox VE asks for the second factor of this account", Code: wire.CodeSecondFactor, Kinds: ch.Kinds,
	})
}

func (a *Auth) signInSecondFactor(c *gin.Context) {
	if !a.admit(c) {
		return
	}
	p := Principal{Method: MethodPassword}
	if !a.allowAttempt(c, p) {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if !readJSON(c, &body) {
		return
	}
	step, state := a.pending.take(cookieValue(c.Request, stepCookieName), a.cfg.Now())
	switch state {
	case stepBusy:
		// Another code of this step is with Proxmox VE, sent twice or from
		// another tab: this one waits for its answer instead of ending it.
		c.Header("Retry-After", "1")
		a.signInRefused(c, p, "a code of this step is being checked", http.StatusTooManyRequests, wire.Error{
			Error: "a code of this sign-in is with Proxmox VE now; wait for its answer", Code: wire.CodeRateLimited, RetryAfter: 1,
		})
		return
	case stepMissing:
		a.signInRefused(c, p, "no second factor pending", http.StatusUnauthorized, wire.Error{
			Error: "Start again with the password: the step of the second factor is over", Code: wire.CodeTicketInvalid,
		})
		return
	}
	p.User = step.challenge.User
	code := strings.Join(strings.Fields(body.Code), "")
	kind := codeKind(code, step.challenge.Kinds)
	if kind == "" {
		a.pending.put(step)
		a.signInRefused(c, p, "a code of another kind", http.StatusBadRequest, wire.Error{
			Error: "this is not a code this account takes: type the digits of the authenticator app", Code: wire.CodeInvalid, Field: "code",
		})
		return
	}
	if a.refuseLocked(c, p, step.account) {
		a.pending.put(step)
		return
	}
	ticket, err := a.login.SecondFactor(c.Request.Context(), step.challenge, kind+":"+code)
	switch {
	case errors.Is(err, ErrRefused):
		step.wrong++
		if step.wrong < maxWrongCodes {
			a.pending.put(step)
			a.signInRefused(c, p, "Proxmox VE did not accept the code", http.StatusUnauthorized, wire.Error{
				Error: "Proxmox VE did not accept the code", Code: wire.CodeSecondFactor, Kinds: step.challenge.Kinds,
			})
			return
		}
		a.pending.remove(step)
		http.SetCookie(c.Writer, clearedStepCookie())
		a.failed(c, p, step.account, "Proxmox VE did not accept the code three times")
		refuse(c, http.StatusUnauthorized, wire.Error{
			Error: "Proxmox VE did not accept the code three times. Start again with the password", Code: wire.CodeTicketInvalid,
		})
	case errors.Is(err, ErrUnknownChallenge):
		a.pending.remove(step)
		http.SetCookie(c.Writer, clearedStepCookie())
		a.unknownChallenge(c, p, err)
	case err != nil:
		a.pending.put(step)
		a.signInUnreachable(c, p, err)
	default:
		a.pending.remove(step)
		http.SetCookie(c.Writer, clearedStepCookie())
		a.lockout.clear(step.account)
		a.startPassword(c, ticket)
	}
}

// unknownChallenge refuses a sign-in whose second factor Proxmox VE asks for
// in a form pco does not read: closed, and said as what it is, not as a
// Proxmox VE out of reach.
func (a *Auth) unknownChallenge(c *gin.Context, p Principal, err error) {
	a.cfg.Log.Warn().Err(err).Msg("reading the second factor Proxmox VE asks for")
	a.signInRefused(c, p, "a second factor pco does not read", http.StatusUnauthorized, wire.Error{
		Error: "Proxmox VE asks this account for a second factor in a form pco does not read. " +
			"Sign in on Proxmox VE's own page, or with an API token.",
		Code: wire.CodeSecondFactorKey,
	})
}

// ownRealm takes the realm a user field names itself, as Proxmox VE's own
// page does: alice@pve is alice in pve whichever realm is chosen, when pve is
// the chosen realm or one of the node's. A name with an @ of its own, as
// john@example.com of a directory, keeps the chosen realm.
func ownRealm(user, realm string, realms []string) (string, string) {
	i := strings.LastIndex(user, "@")
	if i <= 0 {
		return user, realm
	}
	name, named := user[:i], user[i+1:]
	switch {
	case strings.EqualFold(named, realm):
		return name, realm
	case slices.Contains(realms, named):
		return name, named
	}
	return user, realm
}

// codeKind is what a code is to Proxmox VE: the digits of an authenticator
// app, or a recovery code; empty when the account takes neither as typed.
func codeKind(code string, kinds []string) string {
	switch {
	case code == "":
		return ""
	case totpForm.MatchString(code) && slices.Contains(kinds, KindTOTP):
		return KindTOTP
	case slices.Contains(kinds, KindRecovery):
		return KindRecovery
	}
	return ""
}

// startPassword makes the session of a ticket a sign-in obtained, with the
// role its privileges at / give.
func (a *Auth) startPassword(c *gin.Context, ticket string) {
	p := Principal{Method: MethodPassword}
	user, ok := ticketUser(ticket)
	if !ok {
		a.signInUnreachable(c, p, errors.New("the ticket it gave names no user"))
		return
	}
	p.User = user
	privs, err := a.pve.Privileges(c.Request.Context(), LoginTicket(ticket), "/")
	p.Role = roleOf(privs)
	switch {
	case errors.Is(err, ErrRefused):
		a.signInRefused(c, p, "Proxmox VE did not accept the ticket it gave", http.StatusUnauthorized, wire.Error{
			Error: "Proxmox VE did not accept the ticket it gave; sign in again", Code: wire.CodeTicketInvalid,
		})
		return
	case err != nil:
		a.signInUnreachable(c, p, err)
		return
	}
	now := a.cfg.Now()
	a.start(c, p, func(s *Session) { s.ticket, s.renewed = ticket, now })
}

// allowAttempt counts a sign-in of the client address, and answers 429 to
// the eleventh within a minute.
func (a *Auth) allowAttempt(c *gin.Context, p Principal) bool {
	ok, wait := a.limit.allow(bucket(client(c.Request)), a.cfg.Now())
	if ok {
		return true
	}
	after := int(math.Ceil(wait.Seconds()))
	c.Header("Retry-After", strconv.Itoa(after))
	a.signInRefused(c, p, "too many attempts", http.StatusTooManyRequests, wire.Error{
		Error:      "too many sign-ins from this address; try again in " + strconv.Itoa(after) + " s",
		Code:       wire.CodeRateLimited,
		RetryAfter: after,
	})
	return false
}

// refuseLocked answers 429 while the account is locked at pco, before
// Proxmox VE is asked.
func (a *Auth) refuseLocked(c *gin.Context, p Principal, account string) bool {
	locked, wait := a.lockout.locked(account, a.cfg.Now())
	if !locked {
		return false
	}
	after := int(math.Ceil(wait.Seconds()))
	c.Header("Retry-After", strconv.Itoa(after))
	// Whoever keeps an account locked sends a refusal at least every 15
	// minutes and may send many more: the log names the account once a
	// minute, with how many were refused since.
	if n, now := a.lockout.refused(account, a.cfg.Now()); now {
		ev := a.cfg.Log.Warn().Str("account", account)
		if p.User != "" {
			ev = ev.Str("user", p.User)
		}
		ev.Str("method", p.Method).Str("client", client(c.Request)).Int("refused", n).Dur("lockedFor", wait).
			Str("result", "refused: the account is locked at pco").Msg("sign-in")
	}
	refuse(c, http.StatusTooManyRequests, wire.Error{
		Error: "too many failed sign-ins of this account; try again in " + strconv.Itoa(after) +
			" s, or sign in with an API token",
		Code:       wire.CodeRateLimited,
		RetryAfter: after,
	})
	return true
}

// failed counts a failure of the account and logs it with the account, as
// the user typed it in lower case, so that the admin sees whom a lock keeps
// out; the user Proxmox VE named is there too once it accepted the password.
// Neither the password nor a code is ever in the line.
func (a *Auth) failed(c *gin.Context, p Principal, account, why string) {
	if d := a.lockout.fail(account, a.cfg.Now()); d > 0 {
		why += "; the account is locked at pco for " + d.String()
	}
	ev := a.cfg.Log.Warn().Str("account", account)
	if p.User != "" {
		ev = ev.Str("user", p.User)
	}
	ev.Str("method", p.Method).Str("client", client(c.Request)).Str("result", "refused: "+why).Msg("sign-in")
}

// stepCookie binds a pending second factor to the browser for its two
// minutes, as the session's cookie binds a session.
func stepCookie(id string) *http.Cookie {
	return &http.Cookie{
		Name: stepCookieName, Value: id, Path: "/", MaxAge: int(stepLife / time.Second),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	}
}

func clearedStepCookie() *http.Cookie {
	c := stepCookie("")
	c.MaxAge = -1
	return c
}

// pendingStep is a password Proxmox VE accepted, whose second factor is due.
type pendingStep struct {
	id        string
	account   string
	challenge *Challenge
	created   time.Time
	wrong     int
}

// pendingSteps keep the second factors that are due, each for two minutes
// and at most max of them, the oldest out first. A step is taken while its
// code is with Proxmox VE, so that one step has one code in flight.
type pendingSteps struct {
	max int

	mu   sync.Mutex
	byID map[string]*pendingStep
	busy map[string]bool // the steps whose code is with Proxmox VE
}

// What take found of a step.
type stepState int

const (
	stepMissing stepState = iota // not there, or two minutes old
	stepBusy                     // a code of it is with Proxmox VE
	stepTaken
)

func newPendingSteps(max int) *pendingSteps {
	return &pendingSteps{max: max, byID: map[string]*pendingStep{}, busy: map[string]bool{}}
}

func (p *pendingSteps) add(s *pendingStep, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, o := range p.byID {
		if now.Sub(o.created) >= stepLife && !p.busy[id] {
			delete(p.byID, id)
		}
	}
	for len(p.byID) >= p.max {
		var oldest *pendingStep
		for _, o := range p.byID {
			if oldest == nil || o.created.Before(oldest.created) {
				oldest = o
			}
		}
		delete(p.byID, oldest.id)
		delete(p.busy, oldest.id)
	}
	p.byID[s.id] = s
}

// take takes the step id for a code, unless it is not there, two minutes old,
// or taken by a code that is with Proxmox VE now.
func (p *pendingSteps) take(id string, now time.Time) (*pendingStep, stepState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.byID[id]
	switch {
	case !ok:
		return nil, stepMissing
	case p.busy[id]:
		return nil, stepBusy
	case now.Sub(s.created) >= stepLife:
		delete(p.byID, id)
		return nil, stepMissing
	}
	p.busy[id] = true
	return s, stepTaken
}

// put gives a step taken back, for the next code.
func (p *pendingSteps) put(s *pendingStep) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.busy, s.id)
}

// remove ends a step taken: it was answered, or voided.
func (p *pendingSteps) remove(s *pendingStep) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.busy, s.id)
	delete(p.byID, s.id)
}

// realmList keeps the realms of the node for five minutes, and a failure to
// read them for 30 s, so that the answers without a session do not each ask
// Proxmox VE.
type realmList struct {
	login Login
	now   func() time.Time

	mu     sync.Mutex
	list   []string
	at     time.Time
	ok     bool
	asking flight[struct{}, []string]
}

func (r *realmList) get(ctx context.Context, log zerolog.Logger) []string {
	if list, fresh := r.cached(); fresh {
		return list
	}
	list, _ := r.asking.do(ctx, struct{}{}, func(ctx context.Context) ([]string, error) {
		if list, fresh := r.cached(); fresh {
			return list, nil
		}
		list, err := r.login.Realms(ctx)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.at, r.ok = r.now(), err == nil
		if err != nil {
			log.Warn().Err(err).Msg("reading the realms of Proxmox VE")
			return r.list, err
		}
		r.list = list
		return list, nil
	})
	return slices.Clone(list)
}

func (r *realmList) cached() ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	age := r.now().Sub(r.at)
	fresh := !r.at.IsZero() && (r.ok && age < realmsAge || !r.ok && age < realmsRetry)
	return slices.Clone(r.list), fresh
}
