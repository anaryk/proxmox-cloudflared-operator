package auth

import (
	"context"
	"errors"
	"net/http"
)

// Live says whether the session id is there and not over, without asking
// Proxmox VE and without being activity: what a stream asks before every
// write, since an idle or old session may not be sent more.
func (a *Auth) Live(id string) bool {
	s, ok := a.sessions.get(id)
	return ok && s.live(a.cfg.Now())
}

// Recheck says whether the session id still holds, and its role now, as a
// request of the session would find them: the ticket r carries through the
// cache of 30 s, or the token once a minute. RoleNone is a session that is
// over, signed out, refused by Proxmox VE or of another user's ticket. It is
// no activity and ends no session, so that a stream, which asks on its own,
// ends itself when the answer changed; Proxmox VE out of reach is an error,
// and the caller keeps what it has, as a request keeps its session.
func (a *Auth) Recheck(ctx context.Context, r *http.Request, id string) (Role, error) {
	s, ok := a.sessions.get(id)
	if !ok || !s.live(a.cfg.Now()) {
		return RoleNone, nil
	}
	var (
		role Role
		err  error
	)
	switch s.Principal.Method {
	case MethodTicket, MethodPassword:
		ticket := cookieValue(r, ticketCookieName)
		if s.Principal.Method == MethodPassword {
			ticket = LoginTicket(s.ticket).Ticket
		}
		if ticket == "" {
			return RoleNone, nil
		}
		var got ticketCheck
		got, err = a.tickets.check(ctx, ticket)
		if err == nil && got.user != s.Principal.User {
			return RoleNone, nil
		}
		role = got.role
	case MethodToken:
		if a.cfg.Now().Sub(s.checked) < tokenCheckAge {
			return s.Principal.Role, nil
		}
		role, err = a.tokenChecks.do(ctx, s.ID, func(ctx context.Context) (Role, error) {
			return a.checkToken(ctx, s)
		})
	default:
		return RoleNone, nil
	}
	switch {
	case errors.Is(err, ErrRefused):
		return RoleNone, nil
	case err != nil:
		return RoleNone, err
	}
	return role, nil
}

// OnEnd calls fn with the id of every session that ends: signed out,
// replaced by a new sign-in, evicted, expired, or refused by Proxmox VE. The
// streams of the session end with it.
func (a *Auth) OnEnd(fn func(id string)) {
	a.endsMu.Lock()
	defer a.endsMu.Unlock()
	a.ends = append(a.ends, fn)
}

func (a *Auth) ended(id string) {
	a.endsMu.Lock()
	ends := a.ends
	a.endsMu.Unlock()
	for _, fn := range ends {
		fn(id)
	}
}
