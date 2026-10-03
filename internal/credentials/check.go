// Package credentials tells an admin what a Cloudflare API token can do before
// pco stores and uses it.
package credentials

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// Capability is one thing pco needs from a token.
type Capability string

// The capabilities, in the order a report lists them.
const (
	CapToken       Capability = "token"
	CapAccounts    Capability = "accounts"
	CapZones       Capability = "zones"
	CapDNSRead     Capability = "dns.read"
	CapDNSWrite    Capability = "dns.write"
	CapTunnelRead  Capability = "tunnel.read"
	CapTunnelWrite Capability = "tunnel.write"
)

// rank is the place of a capability in a report.
func (c Capability) rank() int {
	switch c {
	case CapToken:
		return 0
	case CapAccounts:
		return 1
	case CapZones:
		return 2
	case CapDNSRead:
		return 3
	case CapDNSWrite:
		return 4
	case CapTunnelRead:
		return 5
	case CapTunnelWrite:
		return 6
	}
	return 7
}

// Check is the outcome of one probe.
type Check struct {
	Capability Capability `json:"capability"`
	Scope      string     `json:"scope,omitempty"`   // account or zone name; empty for token-wide checks
	ScopeID    string     `json:"scopeId,omitempty"` // account or zone id; empty for token-wide checks
	OK         bool       `json:"ok"`
	Detail     string     `json:"detail,omitempty"` // on failure: what to grant, e.g. "grant Zone > DNS > Edit on example.com"

	// Unanswered marks a failure that says nothing about the token: Cloudflare
	// did not answer, with a network error, a server error or a rate limit.
	Unanswered bool `json:"unanswered,omitempty"`
}

// Report is what a token can do.
type Report struct {
	Token cfapi.TokenStatus `json:"token"` // including the expiry date, if the token has one

	// Accounts and Zones are everything the token sees, by name, whether or
	// not it was probed.
	Accounts []cfapi.Account `json:"accounts"`
	Zones    []cfapi.Zone    `json:"zones"`

	// Checks are sorted by capability, then by scope name and id.
	Checks []Check `json:"checks"`

	// Excluded are the active zones the token lists but whose DNS Cloudflare
	// refuses to show it, sorted by name and id. The credential is used
	// without them, and no check is made of them or of an account that only
	// they are in.
	Excluded []Exclusion `json:"excluded"`

	// Deep is true when the run was asked to probe write access.
	Deep bool `json:"deep"`

	// Usable is true when the token is active, can read the DNS of at least
	// one active zone, every check that concerns such a zone or its account
	// passed, and the run was not cut short by its context. A zone that is
	// not active fails its own check, and one that is excluded fails none;
	// neither makes the token unusable while another zone is served.
	//
	// With Deep false Usable says nothing about write access. A caller that
	// is about to store a credential for use must run a deep check.
	Usable bool `json:"usable"`

	// Leftovers are the names of probe records that were already in the
	// zones before this run, sorted: records whose comment is exactly the
	// one a probe carries. Each is an object left by an earlier run of this
	// checker that could not remove it, or one of a run that is still going
	// on elsewhere. The checker leaves them alone.
	Leftovers []string `json:"leftovers"`

	CheckedAt time.Time `json:"checkedAt,omitzero"`
}

// Exclusion is a zone the token lists that the credential leaves out.
type Exclusion struct {
	Zone   string `json:"zone"`
	ZoneID string `json:"zoneId"`
	Reason string `json:"reason"` // why it is left out, e.g. "no DNS read"
	Detail string `json:"detail"` // what to grant to serve it, e.g. "grant Zone > DNS > Edit on example.com"
}

// reasonNoDNSRead is why a zone whose DNS Cloudflare refuses to show the
// token is left out.
const reasonNoDNSRead = "no DNS read"

// LeftOut names the excluded zones with the reason, as "a.com, b.com left
// out: no DNS read". It is empty when no zone is excluded.
func (r Report) LeftOut() string {
	var reasons []string
	zones := make(map[string][]string)
	for _, x := range r.Excluded {
		if _, seen := zones[x.Reason]; !seen {
			reasons = append(reasons, x.Reason)
		}
		zones[x.Reason] = append(zones[x.Reason], x.Zone)
	}
	parts := make([]string, len(reasons))
	for i, reason := range reasons {
		parts[i] = strings.Join(zones[reason], ", ") + " left out: " + reason
	}
	return strings.Join(parts, "; ")
}

// Reason says why a check failed: what to grant, or that Cloudflare did not
// answer. It is empty for a check that passed.
func (c Check) Reason() string {
	switch {
	case c.OK:
		return ""
	case !c.Unanswered:
		return c.Detail
	case c.Detail == "":
		return "Cloudflare did not answer"
	}
	return "Cloudflare did not answer (" + c.Detail + ")"
}

// Unanswered reports whether the report cannot say what the token can do: it
// is not usable, and every check that failed did so because Cloudflare did not
// answer. Asking again later may find the token fine.
func (r Report) Unanswered() bool {
	if r.Usable {
		return false
	}
	failed := false
	for _, c := range r.Checks {
		switch {
		case c.OK:
		case !c.Unanswered:
			return false
		default:
			failed = true
		}
	}
	return failed
}

// The Cloudflare permissions a failed check asks the admin to grant, as the
// dashboard names them.
const (
	permTunnelRead = "Account > Cloudflare Tunnel > Read"
	permTunnelEdit = "Account > Cloudflare Tunnel > Edit"
	permZoneRead   = "Zone > Zone > Read"
	permDNSEdit    = "Zone > DNS > Edit"
)

// Checker probes Cloudflare tokens. It keeps nothing between runs and is safe
// for concurrent use.
type Checker struct {
	installID string
	now       func() time.Time
	rand      func() string
}

// NewChecker returns a checker for one install. rand makes the names of the
// probe objects of a deep run unique and is called once per deep run: it must
// return a fresh, unpredictable suffix of lower-case letters and digits on
// every call, unique also across restarts of the process, for example 16 hex
// characters from crypto/rand. A suffix that repeats would let one run take
// the probe of another for its own. now and rand must themselves be safe for
// concurrent use.
func NewChecker(installID string, now func() time.Time, rand func() string) *Checker {
	return &Checker{installID: installID, now: now, rand: rand}
}

// Run probes what the token behind api can do. Write permissions cannot be
// read from a token, so with deep set Run proves them by creating and
// deleting probe objects, which carry pco's marker; without it no call
// changes anything and the write capabilities are absent from the report. A
// probe is attempted only where the matching read probe passed. Run deletes
// only what it created, and a probe it cannot delete is named in the check.
//
// A deep run creates and deletes objects at Cloudflare without asking who the
// writer of the install is, so it may run in a process that is not the
// writer. It touches only objects it created itself, each under a name unique
// to the run, never a record or tunnel that serves a hostname.
//
// A deep run makes roughly three calls per zone and three per account, one
// after the other, besides the three that open it. It is meant for adding a
// token or for an explicit action of the admin, and the caller should pass a
// context with a deadline. A run that ctx cut short is never usable; the
// caller should check ctx.Err() and discard its report.
func (c *Checker) Run(ctx context.Context, api cfapi.API, deep bool) Report {
	r := &run{checker: c, api: api, deep: deep}
	r.report.Deep = deep
	r.report.CheckedAt = c.now()
	if r.verify(ctx) {
		r.probe(ctx)
	}
	slices.SortStableFunc(r.report.Checks, func(a, b Check) int {
		return cmp.Or(
			cmp.Compare(a.Capability.rank(), b.Capability.rank()),
			cmp.Compare(a.Scope, b.Scope),
			cmp.Compare(a.ScopeID, b.ScopeID))
	})
	slices.Sort(r.report.Leftovers)
	r.report.Usable = !r.blocked && r.served > 0 && ctx.Err() == nil
	return r.report
}

// run is the state of one Run.
type run struct {
	checker *Checker
	api     cfapi.API
	deep    bool
	suffix  string // makes the names of this run's probes unique

	report  Report
	served  int  // zones that are active and not excluded
	blocked bool // a check failed that makes the token unusable
}

// scope is what a check is about: a zone or an account, or the token as a
// whole when empty.
type scope struct{ name, id string }

func zoneScope(z cfapi.Zone) scope       { return scope{z.Name, z.ID} }
func accountScope(a cfapi.Account) scope { return scope{a.Name, a.ID} }

func (r *run) pass(c Capability, s scope) {
	r.report.Checks = append(r.report.Checks, Check{Capability: c, Scope: s.name, ScopeID: s.id, OK: true})
}

// note adds a failed check that does not by itself make the token unusable.
func (r *run) note(c Capability, s scope, detail string) {
	r.report.Checks = append(r.report.Checks, Check{Capability: c, Scope: s.name, ScopeID: s.id, Detail: detail})
}

func (r *run) fail(c Capability, s scope, detail string) {
	r.failBy(c, s, detail, nil)
}

// failBy adds a failed check that makes the token unusable and that err
// caused; one that left Cloudflare unanswered is marked as such.
func (r *run) failBy(c Capability, s scope, detail string, err error) {
	r.blocked = true
	r.report.Checks = append(r.report.Checks, Check{
		Capability: c, Scope: s.name, ScopeID: s.id, Detail: detail, Unanswered: cfapi.IsUnanswered(err),
	})
}

// result records the outcome of a probe call and reports whether it passed. An
// authorisation error says what to grant, any other error is shown as it is.
func (r *run) result(c Capability, s scope, err error, hint string) bool {
	switch {
	case err == nil:
		r.pass(c, s)
		return true
	case cfapi.IsAuth(err):
		r.fail(c, s, hint)
	default:
		r.failBy(c, s, err.Error(), err)
	}
	return false
}

func grant(permission, where string) string { return "grant " + permission + " on " + where }

// verify checks the token itself and reports whether the probes can go on.
func (r *run) verify(ctx context.Context) bool {
	status, err := r.api.VerifyToken(ctx)
	if err != nil {
		r.failBy(CapToken, scope{}, err.Error(), err)
		return false
	}
	r.report.Token = status
	if status.Status != "active" {
		r.fail(CapToken, scope{}, inactiveDetail(status))
		return false
	}
	r.pass(CapToken, scope{})
	return true
}

func inactiveDetail(s cfapi.TokenStatus) string {
	if s.Status == "expired" && s.ExpiresOn != nil {
		return "token expired on " + s.ExpiresOn.UTC().Format(time.DateOnly)
	}
	return "token is " + s.Status
}

func (r *run) probe(ctx context.Context) {
	if r.deep {
		r.suffix = r.checker.rand()
	}
	r.listAccounts(ctx)
	var active []cfapi.Zone
	for _, z := range r.listZones(ctx) {
		if z.Status != "active" {
			r.note(CapZones, zoneScope(z), "zone is "+z.Status+" at Cloudflare")
			continue
		}
		active = append(active, z)
	}
	var served []cfapi.Zone
	for _, z := range active {
		if r.probeZone(ctx, z) {
			served = append(served, z)
		}
	}
	r.served = len(served)
	if len(active) > 0 && len(served) == 0 {
		r.fail(CapDNSRead, scope{}, "token can read the DNS of no zone it lists; "+grant(permDNSEdit, "the zones to manage"))
	}
	for _, a := range r.probedAccounts(served) {
		r.probeAccount(ctx, a)
	}
}

func (r *run) listAccounts(ctx context.Context) {
	hint := grant(permTunnelRead, "the account")
	accounts, err := r.api.Accounts(ctx)
	if err == nil && len(accounts) == 0 {
		r.fail(CapAccounts, scope{}, "token sees no accounts; "+hint)
		return
	}
	if !r.result(CapAccounts, scope{}, err, hint) {
		return
	}
	accounts = slices.Clone(accounts)
	slices.SortFunc(accounts, compareAccounts)
	r.report.Accounts = accounts
}

// listZones returns the zones the token sees, which are also in the report.
func (r *run) listZones(ctx context.Context) []cfapi.Zone {
	hint := grant(permZoneRead, "the zones to manage")
	zones, err := r.api.Zones(ctx)
	if err == nil && len(zones) == 0 {
		r.fail(CapZones, scope{}, "token sees no zones; "+hint)
		return nil
	}
	if !r.result(CapZones, scope{}, err, hint) {
		return nil
	}
	zones = slices.Clone(zones)
	slices.SortFunc(zones, func(a, b cfapi.Zone) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	r.report.Zones = zones
	return zones
}

func compareAccounts(a, b cfapi.Account) int {
	return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
}

// probedAccounts returns the accounts that own a served zone. The zone names
// its account, so an account the listing lacks is still probed, by id.
func (r *run) probedAccounts(served []cfapi.Zone) []cfapi.Account {
	var out []cfapi.Account
	for _, z := range served {
		if slices.ContainsFunc(out, func(a cfapi.Account) bool { return a.ID == z.AccountID }) {
			continue
		}
		a := cfapi.Account{ID: z.AccountID, Name: z.AccountID}
		if i := slices.IndexFunc(r.report.Accounts, func(x cfapi.Account) bool { return x.ID == z.AccountID }); i >= 0 {
			a = r.report.Accounts[i]
		}
		out = append(out, a)
	}
	slices.SortFunc(out, compareAccounts)
	return out
}

// probeZone probes an active zone and reports whether the credential serves
// it. A zone whose DNS Cloudflare refuses to show the token is excluded; one
// whose read got no answer is not, and fails its check.
func (r *run) probeZone(ctx context.Context, z cfapi.Zone) bool {
	filter := cfapi.RecordFilter{CommentPrefix: planner.DNSMarker(r.checker.installID)}
	owned, err := r.api.Records(ctx, z.ID, filter)
	switch {
	case cfapi.IsAuth(err):
		r.report.Excluded = append(r.report.Excluded, Exclusion{
			Zone: z.Name, ZoneID: z.ID, Reason: reasonNoDNSRead, Detail: grant(permDNSEdit, z.Name),
		})
		return false
	case err != nil:
		r.failBy(CapDNSRead, zoneScope(z), err.Error(), err)
		return true
	}
	r.pass(CapDNSRead, zoneScope(z))
	for _, rec := range owned {
		if rec.Comment == r.probeComment() {
			r.report.Leftovers = append(r.report.Leftovers, rec.Name)
		}
	}
	if r.deep {
		r.probeDNSWrite(ctx, z)
	}
	return true
}

func (r *run) probeAccount(ctx context.Context, a cfapi.Account) {
	_, _, err := r.api.FindTunnel(ctx, a.ID, planner.TunnelName(r.checker.installID))
	if r.result(CapTunnelRead, accountScope(a), err, grant(permTunnelRead, a.Name)) && r.deep {
		r.probeTunnelWrite(ctx, a)
	}
}
