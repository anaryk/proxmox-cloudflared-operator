package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Login is the appliance's sign-in: user, realm and password, then, when
// Proxmox VE asks for it, the second factor. It is the node's access/ticket,
// called as Proxmox VE's own page calls it.
type Login interface {
	// Password returns a full ticket, or a challenge when a second factor is
	// due. A wrong user or password is ErrRefused.
	Password(ctx context.Context, user, realm, password string) (ticket string, challenge *Challenge, err error)
	// SecondFactor completes a challenge with "totp:<code>" or
	// "recovery:<code>". A wrong code is ErrRefused.
	SecondFactor(ctx context.Context, c *Challenge, code string) (ticket string, err error)
	// Renew renews a ticket as Proxmox VE's own page does: the ticket in
	// place of the password.
	Renew(ctx context.Context, user, ticket string) (string, error)
	// Realms lists the realms of access/domains, which Proxmox VE gives
	// anyone who asks; the default realm first.
	Realms(ctx context.Context) ([]string, error)
}

// Challenge is a pending second factor: the partial ticket Proxmox VE gave
// for the password, and the kinds of second factor it takes.
type Challenge struct {
	User    string
	partial string   // the partial ticket of the challenge
	Kinds   []string // "totp", "recovery", "webauthn", ...
}

// The kinds of second factor that work through pco's page.
const (
	KindTOTP     = "totp"
	KindRecovery = "recovery"
)

// Typed says whether the challenge takes a code one can type here: a TOTP
// or a recovery code. A security key is bound to Proxmox VE's own origin.
func (c *Challenge) Typed() bool {
	return slices.Contains(c.Kinds, KindTOTP) || slices.Contains(c.Kinds, KindRecovery)
}

type loginClient struct{ apiConn }

// NewLogin returns the sign-in at the API at baseURL, whose answers are
// checked as t says. A call that does not answer within timeout fails.
func NewLogin(baseURL string, t Trust, timeout time.Duration) Login {
	return &loginClient{apiConn: newAPIConn(baseURL, t, timeout)}
}

// ticketAnswer is the answer of access/ticket. NeedTFA is set, and the
// ticket is a partial one, PVE:!tfa!<challenge>:<time>::<signature>, when a
// second factor is due.
type ticketAnswer struct {
	Username string `json:"username"`
	Ticket   string `json:"ticket"`
	NeedTFA  any    `json:"NeedTFA"`
}

func (l *loginClient) ticket(ctx context.Context, form url.Values) (ticketAnswer, error) {
	var a ticketAnswer
	if err := l.call(ctx, http.MethodPost, "access/ticket", form, nil, &a); err != nil {
		return ticketAnswer{}, err
	}
	if a.Ticket == "" || a.Username == "" {
		return ticketAnswer{}, fmt.Errorf("%w: its answer holds no ticket", ErrUnreachable)
	}
	return a, nil
}

func (l *loginClient) Password(ctx context.Context, user, realm, password string) (string, *Challenge, error) {
	a, err := l.ticket(ctx, url.Values{"username": {user + "@" + realm}, "password": {password}})
	if err != nil {
		return "", nil, err
	}
	data, partial := partialData(a.Ticket)
	switch {
	case !partial && !truthy(a.NeedTFA):
		return a.Ticket, nil, nil
	case !partial:
		return "", nil, fmt.Errorf("%w: it asked for a second factor without a challenge", ErrUnknownChallenge)
	}
	kinds, err := challengeKinds(data)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %w", ErrUnknownChallenge, err)
	}
	return "", &Challenge{User: a.Username, partial: a.Ticket, Kinds: kinds}, nil
}

func (l *loginClient) SecondFactor(ctx context.Context, c *Challenge, code string) (string, error) {
	if c == nil || c.partial == "" {
		return "", errors.New("there is no challenge to answer")
	}
	a, err := l.ticket(ctx, url.Values{"username": {c.User}, "tfa-challenge": {c.partial}, "password": {code}})
	if err != nil {
		return "", err
	}
	if _, partial := partialData(a.Ticket); partial {
		return "", fmt.Errorf("%w: it asked for yet another factor", ErrUnknownChallenge)
	}
	return a.Ticket, nil
}

func (l *loginClient) Renew(ctx context.Context, user, ticket string) (string, error) {
	a, err := l.ticket(ctx, url.Values{"username": {user}, "password": {ticket}})
	if err != nil {
		return "", err
	}
	if _, partial := partialData(a.Ticket); partial {
		return "", fmt.Errorf("%w: it asked for a second factor to renew a ticket", ErrRefused)
	}
	return a.Ticket, nil
}

func (l *loginClient) Realms(ctx context.Context) ([]string, error) {
	var rows []struct {
		Realm   string `json:"realm"`
		Default any    `json:"default"`
	}
	if err := l.call(ctx, http.MethodGet, "access/domains", nil, nil, &rows); err != nil {
		return nil, fmt.Errorf("listing the realms: %w", err)
	}
	var out []string
	for _, r := range rows {
		switch {
		case !realmForm.MatchString(r.Realm) || slices.Contains(out, r.Realm):
		case truthy(r.Default):
			out = slices.Insert(out, 0, r.Realm)
		default:
			out = append(out, r.Realm)
		}
	}
	return out, nil
}

// partialData is the data of a partial ticket, PVE:!tfa!<challenge>:..., and
// whether the ticket is one. The challenge is escaped, so that its colons do
// not end the field; the time and the signature follow the last single colon.
func partialData(ticket string) (string, bool) {
	rest, ok := strings.CutPrefix(ticket, "PVE:!tfa!")
	if !ok {
		return "", false
	}
	if i := strings.LastIndex(rest, "::"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		rest = rest[:i]
	}
	if s, err := url.PathUnescape(rest); err == nil {
		rest = s
	}
	return rest, true
}

// challengeKinds are the kinds of second factor a challenge offers: every
// field of it that is set, as "totp": true, "recovery": "available" or
// "webauthn": {...}. A field that is false, empty or "unavailable" is not.
func challengeKinds(data string) ([]string, error) {
	var fields map[string]any
	if err := json.Unmarshal([]byte(data), &fields); err != nil {
		return nil, errors.New("the challenge of its second factor is not JSON")
	}
	var kinds []string
	for k, v := range fields {
		if truthy(v) && v != "unavailable" {
			kinds = append(kinds, k)
		}
	}
	slices.Sort(kinds)
	return kinds, nil
}

// truthy reads a flag of Proxmox VE, which says 1, true or a value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != "" && x != "0"
	case map[string]any:
		return len(x) > 0
	case []any:
		return len(x) > 0
	}
	return true
}
