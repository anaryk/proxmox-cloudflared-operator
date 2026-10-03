// Package cffake is an in-memory Cloudflare for tests of the code that uses
// cfapi.API. It keeps Cloudflare's conflict rules, so that code that works
// against it does not trip on them in production, and it can be made to fail
// by operation.
package cffake

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// The operations Deny, Allow and FailNext work on. Each call of the API
// belongs to one of them.
const (
	opVerify      = "verify"       // VerifyToken
	opAccounts    = "accounts"     // Accounts
	opZones       = "zones"        // Zones
	opTunnelRead  = "tunnel.read"  // FindTunnel, Tunnels, TunnelToken, TunnelConfig, Connectors
	opTunnelWrite = "tunnel.write" // CreateTunnel, DeleteTunnel, PutTunnelConfig
	opDNSRead     = "dns.read"     // Records
	opDNSWrite    = "dns.write"    // CreateRecord, UpdateRecord, DeleteRecord
)

// Cloudflare codes behind the errors the fake returns.
const (
	codeAuthentication = 10000 // the token may not do this
	codeInvalidToken   = 1000  // the token is not one Cloudflare verifies
	codeTunnelExists   = 1013  // a tunnel of that name exists
	codeNameExists     = 81053 // an A, AAAA or CNAME record for that name conflicts
	codeIdentical      = 81058 // the same record exists
)

var _ cfapi.API = (*Fake)(nil)

// Fake is an in-memory Cloudflare. It is safe for concurrent use.
//
// Ids are deterministic: tunnels get UUID-shaped ids from a counter and
// records get "rec-1", "rec-2", ... Slices go in and out as copies, so a
// caller never changes what the fake holds.
type Fake struct {
	mu  sync.Mutex
	now func() time.Time

	accounts []cfapi.Account
	zones    []cfapi.Zone
	tunnels  []*tunnel                 // oldest first, the ones that were deleted included
	records  map[string][]cfapi.Record // by zone id, oldest first

	tunnelSeq  int
	recordSeq  int
	token      cfapi.TokenStatus
	tokenOwner string // the account that owns the token; empty for a user's token

	calls    []string
	denied   map[string]bool
	failures map[string][]*failure
}

type failure struct {
	n   int
	err error
}

// New returns an empty fake that reads the time from the system clock.
func New() *Fake {
	return &Fake{
		now:      time.Now,
		token:    cfapi.TokenStatus{ID: "token-1", Status: "active"},
		records:  make(map[string][]cfapi.Record),
		denied:   make(map[string]bool),
		failures: make(map[string][]*failure),
	}
}

// SetNow sets the clock behind ModifiedOn of records and CreatedAt of
// tunnels. A nil function means the system clock.
func (f *Fake) SetNow(now func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	f.now = now
}

// SetTokenStatus sets what VerifyToken reports: "active", "disabled" or
// "expired", and when the token expires, if it does. A token is active and
// does not expire until this says otherwise.
func (f *Fake) SetTokenStatus(status string, expiresOn *time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token.Status = status
	f.token.ExpiresOn = nil
	if expiresOn != nil {
		expires := *expiresOn
		f.token.ExpiresOn = &expires
	}
}

// SetTokenOwner makes the token one that an account owns rather than a user;
// an empty id makes it a user's again. VerifyToken answers for it what the
// client finds: the user form refuses it with a 401, and the account form of
// its account verifies it, which the client reaches only while the token sees
// that account. So the token verifies while its account is added, and is
// refused with a 401 otherwise.
func (f *Fake) SetTokenOwner(accountID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenOwner = accountID
}

// AddAccount makes an account visible. Adding an id again renames it.
func (f *Fake) AddAccount(id, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := cfapi.Account{ID: id, Name: name}
	if i := slices.IndexFunc(f.accounts, func(x cfapi.Account) bool { return x.ID == id }); i >= 0 {
		f.accounts[i] = a
		return
	}
	f.accounts = append(f.accounts, a)
}

// AddZone makes an active zone of an account visible. Adding an id again
// replaces the zone.
func (f *Fake) AddZone(id, name, accountID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	z := cfapi.Zone{ID: id, Name: name, Status: "active", AccountID: accountID}
	if i := slices.IndexFunc(f.zones, func(x cfapi.Zone) bool { return x.ID == id }); i >= 0 {
		f.zones[i] = z
		return
	}
	f.zones = append(f.zones, z)
}

// SeedRecord puts a record into a zone without the checks and the change of
// the name that CreateRecord makes, so that a test can start from a state it
// could not reach through the API. An empty ID is replaced by the next free
// "rec-N" and a zero ModifiedOn by the current time. The TTL is stored as
// Cloudflare holds it: 1, automatic, for a proxied record or an unset TTL. A
// record that has the id of one already there replaces it. The zone must have
// been added for the record to be visible through the API.
func (f *Fake) SeedRecord(zoneID string, r cfapi.Record) cfapi.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.ID == "" {
		r.ID = f.newRecordID()
	}
	r.TTL = storedTTL(r)
	if r.ModifiedOn.IsZero() {
		r.ModifiedOn = f.now()
	}
	records := f.records[zoneID]
	if i := slices.IndexFunc(records, func(x cfapi.Record) bool { return x.ID == r.ID }); i >= 0 {
		records[i] = r
		return r
	}
	f.records[zoneID] = append(records, r)
	return r
}

// SeedTunnel puts a tunnel into an account without any check, so a test may
// make two of one name that are not deleted. With rules the tunnel starts with that configuration
// at version 1, whether or not Cloudflare would accept it: a configuration
// edited by hand may lack a catch-all. The account must have been added for the
// tunnel to be visible through the API.
func (f *Fake) SeedTunnel(accountID, name string, rules []planner.IngressRule) cfapi.Tunnel {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.newTunnel(accountID, name)
	if len(rules) > 0 {
		t.ingress = clone(rules)
		t.version = 1
	}
	return t.Tunnel
}

// SeedDeletedTunnel puts a tunnel into an account as one that was deleted, at
// the time of the clock, so that a test may start from a name that was used
// before, or from several tombstones of one name. The account must have been
// added for the tombstone to be visible through the API.
func (f *Fake) SeedDeletedTunnel(accountID, name string) cfapi.Tunnel {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.newTunnel(accountID, name)
	t.deleted, t.deletedAt = true, f.now()
	return t.Tunnel
}

// Deny makes every call of an operation fail with a 403 until Allow. The
// operations are "verify", "accounts", "zones", "tunnel.read", "tunnel.write",
// "dns.read" and "dns.write". Given ids, it denies only the calls about those
// zones or accounts: the zone of a DNS call, the account of a tunnel call, as
// a token whose permission covers some of them only.
func (f *Fake) Deny(op string, ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, key := range denyKeys(op, ids) {
		f.denied[key] = true
	}
}

// Allow lifts a Deny made with the same arguments.
func (f *Fake) Allow(op string, ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, key := range denyKeys(op, ids) {
		delete(f.denied, key)
	}
}

func denyKeys(op string, ids []string) []string {
	if len(ids) == 0 {
		return []string{op}
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = op + " " + id
	}
	return keys
}

// FailNext makes the next n calls of op fail with err, whatever they would
// have done and even when op is denied. Calls to it queue: the failures of an
// earlier call are used up first. A nil err still fails the calls.
func (f *Fake) FailNext(op string, n int, err error) {
	if n <= 0 {
		return
	}
	if err == nil {
		err = errors.New("injected failure")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[op] = append(f.failures[op], &failure{n: n, err: err})
}

// Calls returns the calls made so far, oldest first, as the method name
// followed by its arguments: "FindTunnel acct1 pco-abc", "DeleteTunnel acct1
// <tunnel id>", "Records zone1", "CreateRecord zone1 <name>", "UpdateRecord
// zone1 <record id>". A call refused by Deny, FailNext or because an id is
// unknown is in the list. A call that never reaches Cloudflare is not: one made
// with an ended context, or with an id or name the client would refuse to send
// (see cfapi.CheckID).
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return clone(f.calls)
}

// RecordsIn returns the records of a zone, oldest first.
func (f *Fake) RecordsIn(zoneID string) []cfapi.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return clone(f.records[zoneID])
}

// TunnelsIn returns the tunnels of an account that are not deleted, oldest
// first.
func (f *Fake) TunnelsIn(accountID string) []cfapi.Tunnel {
	return f.tunnelsIn(accountID, false)
}

// DeletedTunnelsIn returns the tunnels of an account that were deleted, oldest
// first: what a listing of the API shows of them, and what a test looks at to
// tell a tunnel that was deleted from one that never was.
func (f *Fake) DeletedTunnelsIn(accountID string) []cfapi.Tunnel {
	return f.tunnelsIn(accountID, true)
}

func (f *Fake) tunnelsIn(accountID string, deleted bool) []cfapi.Tunnel {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []cfapi.Tunnel
	for _, t := range f.tunnels {
		if t.account == accountID && t.deleted == deleted {
			out = append(out, t.Tunnel)
		}
	}
	return out
}

// begin logs a call and decides whether it is refused. It must be called with
// the lock held, before the call does anything.
func (f *Fake) begin(ctx context.Context, op, method string, args ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.calls = append(f.calls, strings.Join(append([]string{method}, args...), " "))

	if queue := f.failures[op]; len(queue) > 0 {
		next := queue[0]
		if next.n--; next.n == 0 {
			f.failures[op] = queue[1:]
		}
		return next.err
	}
	// The zone or the account a call is about comes first.
	if f.denied[op] || len(args) > 0 && f.denied[op+" "+args[0]] {
		return &cfapi.Error{Status: http.StatusForbidden, Codes: []int{codeAuthentication}, Message: "Authentication error"}
	}
	return nil
}

func (f *Fake) VerifyToken(ctx context.Context) (cfapi.TokenStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opVerify, "VerifyToken"); err != nil {
		return cfapi.TokenStatus{}, err
	}
	if f.tokenOwner != "" && f.account(f.tokenOwner) != nil {
		return cfapi.TokenStatus{}, errInvalidToken()
	}
	return f.tokenStatus(), nil
}

// verifyForm answers one of the two forms of the token check that Cloudflare
// has, which the handler serves: the user form, asked for with an empty
// account id, and the form of an account. A token is verified by the form of
// its owner and refused by the other. VerifyToken stands for the two together,
// as the client finds the token: it verifies while the account that owns it is
// one the token sees.
func (f *Fake) verifyForm(ctx context.Context, accountID string) (cfapi.TokenStatus, error) {
	if accountID != "" {
		if err := cfapi.CheckID("account id", accountID); err != nil {
			return cfapi.TokenStatus{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opVerify, "VerifyToken"); err != nil {
		return cfapi.TokenStatus{}, err
	}
	if f.tokenOwner != accountID {
		return cfapi.TokenStatus{}, errInvalidToken()
	}
	return f.tokenStatus(), nil
}

func errInvalidToken() error {
	return &cfapi.Error{Status: http.StatusUnauthorized, Codes: []int{codeInvalidToken}, Message: "Invalid API Token"}
}

// tokenStatus is a copy of what the token reports. The lock must be held.
func (f *Fake) tokenStatus() cfapi.TokenStatus {
	st := f.token
	if st.ExpiresOn != nil {
		expires := *st.ExpiresOn
		st.ExpiresOn = &expires
	}
	return st
}

func (f *Fake) Accounts(ctx context.Context) ([]cfapi.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opAccounts, "Accounts"); err != nil {
		return nil, err
	}
	return clone(f.accounts), nil
}

func (f *Fake) Zones(ctx context.Context) ([]cfapi.Zone, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opZones, "Zones"); err != nil {
		return nil, err
	}
	return clone(f.zones), nil
}

// clone copies s; an empty slice comes out nil, as an empty answer does from
// the real client.
func clone[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return slices.Clone(s)
}

func notFound(what, id string) error {
	return &cfapi.Error{Status: http.StatusNotFound, Message: fmt.Sprintf("%s %q not found", what, id)}
}
