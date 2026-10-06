package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Credential is what the browser presented, or what a sign-in with a password
// obtained; exactly one field is set. Ticket is a PVEAuthCookie as the browser
// sends it, escaped as Proxmox VE's page sets it: LoginTicket makes one of a
// ticket from access/ticket, and pveproxy reads both the same way.
type Credential struct{ Ticket, Token string }

// LoginTicket is the credential of a ticket a sign-in obtained, escaped as
// the cookie of Proxmox VE's page: pveproxy unescapes what it is sent.
func LoginTicket(ticket string) Credential { return Credential{Ticket: url.PathEscape(ticket)} }

// PVE is the Proxmox VE API as sign-in asks it, always with the user's own
// credential: pveproxy checks the ticket's signature and expiry, or the token.
type PVE interface {
	// Privileges returns the effective privileges at path.
	Privileges(ctx context.Context, c Credential, path string) (map[string]bool, error)
	// VisibleVMIDs returns the guests /cluster/resources lists for c.
	VisibleVMIDs(ctx context.Context, c Credential) ([]int, error)
}

var (
	// ErrRefused is Proxmox VE's 401: the ticket or the token is not valid,
	// or no longer.
	ErrRefused = errors.New("proxmox ve did not accept the credential")
	// ErrUnreachable is every other failure, a certificate other than the pin
	// among them: sign-in fails closed.
	ErrUnreachable = errors.New("proxmox ve cannot be reached")
	// ErrUnknownChallenge is a second factor Proxmox VE asks for in a form pco
	// does not read, which fails closed as well.
	ErrUnknownChallenge = errors.New("proxmox ve asks for a second factor pco does not read")
)

const maxAnswer = 8 << 20

type pveClient struct{ apiConn }

// Trust is how the answers of Proxmox VE are checked. On a node, Pin is the
// certificate its pveproxy serves, which every answer must present byte for
// byte: port 8006 is no privileged port, and while pveproxy is down any local
// user could listen on it and collect the tickets of everyone who signs in.
// In the appliance, which reaches the node's API over the network, the chain
// must lead to Roots and name ServerName, as for the appliance's daemon.
// Without either every call fails.
type Trust struct {
	Pin        *x509.Certificate
	Roots      *x509.CertPool
	ServerName string
}

// tlsConfig is the configuration of the connection t says: on a node the
// chain is not what is checked, the pin is; in the appliance the default
// verification against Roots under ServerName, as the daemon's client does.
func (t Trust) tlsConfig() (*tls.Config, error) {
	switch {
	case t.Pin != nil:
		pin := t.Pin
		return &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 || !bytes.Equal(cs.PeerCertificates[0].Raw, pin.Raw) {
					return errors.New("what answers presents another certificate than pveproxy's")
				}
				return nil
			},
		}, nil
	case t.Roots != nil && t.ServerName != "":
		return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: t.Roots, ServerName: t.ServerName}, nil
	}
	return nil, errors.New("there is no certificate of pveproxy to check its answer against")
}

// apiConn is the connection to the API at a base URL that every call makes.
type apiConn struct {
	base    *url.URL
	err     error // what is wrong with the base URL
	hc      *http.Client
	timeout time.Duration
}

func newAPIConn(baseURL string, t Trust, timeout time.Duration) apiConn {
	c := apiConn{timeout: timeout}
	c.base, c.err = url.Parse(baseURL)
	switch {
	case c.err != nil:
		// The error of url.Parse repeats the URL; the reason is enough.
		c.err = fmt.Errorf("the address of Proxmox VE is not a URL: %w", errors.Unwrap(c.err))
	case c.base.Scheme != "https" || c.base.Host == "":
		c.err = errors.New("the address of Proxmox VE is not an https URL")
	}
	tc, err := t.tlsConfig()
	if err != nil && c.err == nil {
		c.err = err
	}
	c.hc = &http.Client{
		Transport: &http.Transport{
			// No proxy: the API is the node's own.
			TLSClientConfig:     tc,
			TLSHandshakeTimeout: timeout,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		},
		// The API never redirects; following one would carry the ticket or
		// the password to wherever it points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c
}

// NewPVE returns the API at baseURL, such as https://127.0.0.1:8006/api2/json,
// whose answers must present pin.
func NewPVE(baseURL string, pin *x509.Certificate, timeout time.Duration) PVE {
	return NewTrustedPVE(baseURL, Trust{Pin: pin}, timeout)
}

// NewTrustedPVE returns the API at baseURL, whose answers are checked as t
// says.
func NewTrustedPVE(baseURL string, t Trust, timeout time.Duration) PVE {
	return &pveClient{apiConn: newAPIConn(baseURL, t, timeout)}
}

func (p *pveClient) Privileges(ctx context.Context, c Credential, path string) (map[string]bool, error) {
	var data map[string]map[string]json.RawMessage
	if err := p.get(ctx, c, "access/permissions", url.Values{"path": {path}}, &data); err != nil {
		return nil, fmt.Errorf("reading the privileges at %s: %w", path, err)
	}
	privs := map[string]bool{}
	// The value is the propagate flag; a privilege listed is held.
	for priv := range data[path] {
		privs[priv] = true
	}
	return privs, nil
}

func (p *pveClient) VisibleVMIDs(ctx context.Context, c Credential) ([]int, error) {
	var rows []struct {
		Type string `json:"type"`
		VMID int    `json:"vmid"`
	}
	if err := p.get(ctx, c, "cluster/resources", url.Values{"type": {"vm"}}, &rows); err != nil {
		return nil, fmt.Errorf("listing the guests: %w", err)
	}
	var out []int
	for _, row := range rows {
		if (row.Type == "qemu" || row.Type == "lxc") && row.VMID > 0 {
			out = append(out, row.VMID)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// get calls GET <base>/<endpoint> with c and decodes the data of the answer
// into out. No error carries the credential.
func (p *pveClient) get(ctx context.Context, c Credential, endpoint string, query url.Values, out any) error {
	if (c.Ticket == "") == (c.Token == "") {
		return errors.New("a credential is a ticket or a token")
	}
	return p.call(ctx, http.MethodGet, endpoint, query, func(req *http.Request) {
		if c.Ticket != "" {
			// As the browser sent it: pveproxy unescapes it as it does for
			// its own page.
			req.Header.Set("Cookie", "PVEAuthCookie="+c.Ticket)
		} else {
			req.Header.Set("Authorization", "PVEAPIToken="+c.Token)
		}
	}, out)
}

// call calls <method> <base>/<endpoint>, with values in the query of a GET
// and as a form in the body of a POST, and decodes the data of the answer
// into out. set adds what the call presents. A 401 is ErrRefused, any other
// failure ErrUnreachable; no error carries what was sent.
func (c apiConn) call(ctx context.Context, method, endpoint string, values url.Values, set func(*http.Request), out any) error {
	if c.err != nil {
		return fmt.Errorf("%w: %w", ErrUnreachable, c.err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	u := c.base.JoinPath(endpoint)
	var form io.Reader
	if method == http.MethodGet {
		u.RawQuery = values.Encode()
	} else {
		form = strings.NewReader(values.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), form)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if set != nil {
		set(req)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			// Its text repeats the URL; the cause is enough.
			err = uerr.Err
		}
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return ErrRefused
	default:
		return fmt.Errorf("%w: it answered %s", ErrUnreachable, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	switch {
	case err != nil:
		return fmt.Errorf("%w: reading its answer: %w", ErrUnreachable, err)
	case len(body) > maxAnswer:
		return fmt.Errorf("%w: its answer is longer than %d bytes", ErrUnreachable, maxAnswer)
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("%w: decoding its answer: %w", ErrUnreachable, err)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		// The error of a type that did not fit may quote the answer, which
		// holds a ticket when it is the answer of a sign-in.
		return fmt.Errorf("%w: its answer is not what the call returns", ErrUnreachable)
	}
	return nil
}

// ticketUser is the user a ticket names, PVE:<user>:<time>::<signature>,
// read from the cookie as Proxmox VE's page sets it (escaped) or not. It is
// what the ticket claims, so it is read only once pveproxy accepted it.
func ticketUser(ticket string) (string, bool) {
	if s, err := url.PathUnescape(ticket); err == nil {
		ticket = s
	}
	parts := strings.SplitN(ticket, ":", 3)
	if len(parts) < 3 || parts[0] != "PVE" || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// roleOf is the role the privileges at / give.
func roleOf(privs map[string]bool) Role {
	switch {
	case privs["Sys.Modify"]:
		return RoleAdmin
	case privs["Sys.Audit"]:
		return RoleReader
	}
	return RoleNone
}

const (
	ticketCheckAge  = 30 * time.Second
	maxTicketChecks = 4096
)

// ticketCheck is what Proxmox VE said of a ticket, and when.
type ticketCheck struct {
	user string
	role Role
	at   time.Time
}

// ticketChecks keeps the answers for tickets for 30 s, by the SHA-256 of the
// ticket, so that the requests of a page do not each ask pveproxy, and asks
// once for the requests that find no answer at the same time. Refusals are
// not kept: a refused ticket ends its session.
type ticketChecks struct {
	pve PVE
	now func() time.Time

	mu     sync.Mutex
	byHash map[[sha256.Size]byte]ticketCheck
	asking flight[[sha256.Size]byte, ticketCheck]
}

func newTicketChecks(pve PVE, now func() time.Time) *ticketChecks {
	return &ticketChecks{pve: pve, now: now, byHash: map[[sha256.Size]byte]ticketCheck{}}
}

func (t *ticketChecks) check(ctx context.Context, ticket string) (ticketCheck, error) {
	h := sha256.Sum256([]byte(ticket))
	if got, ok := t.cached(h); ok {
		return got, nil
	}
	return t.asking.do(ctx, h, func(ctx context.Context) (ticketCheck, error) {
		// A call that ended after the look above has the answer.
		if got, ok := t.cached(h); ok {
			return got, nil
		}
		return t.ask(ctx, h, ticket)
	})
}

func (t *ticketChecks) cached(h [sha256.Size]byte) (ticketCheck, bool) {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	got, ok := t.byHash[h]
	return got, ok && now.Sub(got.at) < ticketCheckAge
}

func (t *ticketChecks) ask(ctx context.Context, h [sha256.Size]byte, ticket string) (ticketCheck, error) {
	now := t.now()
	privs, err := t.pve.Privileges(ctx, Credential{Ticket: ticket}, "/")
	if err != nil {
		return ticketCheck{}, err
	}
	user, ok := ticketUser(ticket)
	if !ok {
		return ticketCheck{}, fmt.Errorf("%w: the ticket names no user", ErrRefused)
	}
	got := ticketCheck{user: user, role: roleOf(privs), at: now}

	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.byHash) >= maxTicketChecks {
		for k, v := range t.byHash {
			if now.Sub(v.at) >= ticketCheckAge {
				delete(t.byHash, k)
			}
		}
	}
	if len(t.byHash) >= maxTicketChecks {
		clear(t.byHash)
	}
	t.byHash[h] = got
	return got, nil
}
