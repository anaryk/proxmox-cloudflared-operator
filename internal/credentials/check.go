// Package credentials tells an admin what a Cloudflare API token can do before
// pco stores and uses it.
package credentials

import (
	"cmp"
	"context"
	"slices"
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
	Capability Capability
	Scope      string // account or zone name; empty for token-wide checks
	OK         bool
	Detail     string // on failure: what to grant, e.g. "grant Zone > DNS > Edit on example.com"
}

// Report is what a token can do.
type Report struct {
	Token cfapi.TokenStatus // including the expiry date, if the token has one

	// Accounts and Zones are everything the token sees, by name, whether or
	// not it was probed.
	Accounts []cfapi.Account
	Zones    []cfapi.Zone

	// Checks are sorted by capability, then scope.
	Checks []Check

	// Usable is true when the token is active, at least one zone is active
	// and every check that concerns an active zone or its account passed. A
	// zone that is not active fails its own check and does not make the
	// token unusable while another zone is active.
	Usable bool

	CheckedAt time.Time
}

// The Cloudflare permissions a failed check asks the admin to grant, as the
// dashboard names them.
const (
	permTunnelRead = "Account > Cloudflare Tunnel > Read"
	permTunnelEdit = "Account > Cloudflare Tunnel > Edit"
	permZoneRead   = "Zone > Zone > Read"
	permDNSRead    = "Zone > DNS > Read"
	permDNSEdit    = "Zone > DNS > Edit"
)

// cleanupTimeout bounds the removal of a probe object.
const cleanupTimeout = 30 * time.Second

// Checker probes Cloudflare tokens. It keeps nothing between runs.
type Checker struct {
	installID string
	now       func() time.Time
	rand      func() string
}

// NewChecker returns a checker for one install. rand returns lower-case
// letters and digits that make the names of probe objects unique; it is called
// once per deep run and must be safe for concurrent use when runs overlap.
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
// Run is safe to call concurrently, for different credentials.
func (c *Checker) Run(ctx context.Context, api cfapi.API, deep bool) Report {
	r := &run{checker: c, api: api, deep: deep}
	r.report.CheckedAt = c.now()
	if r.verify(ctx) {
		r.probe(ctx)
	}
	slices.SortStableFunc(r.report.Checks, func(a, b Check) int {
		return cmp.Or(cmp.Compare(a.Capability.rank(), b.Capability.rank()), cmp.Compare(a.Scope, b.Scope))
	})
	r.report.Usable = !r.blocked && r.active > 0
	return r.report
}

// run is the state of one Run.
type run struct {
	checker *Checker
	api     cfapi.API
	deep    bool
	suffix  string // makes the names of this run's probes unique

	report  Report
	active  int  // zones that are active
	blocked bool // a check failed that makes the token unusable
}

func (r *run) pass(c Capability, scope string) {
	r.report.Checks = append(r.report.Checks, Check{Capability: c, Scope: scope, OK: true})
}

// note adds a failed check that does not by itself make the token unusable.
func (r *run) note(c Capability, scope, detail string) {
	r.report.Checks = append(r.report.Checks, Check{Capability: c, Scope: scope, Detail: detail})
}

func (r *run) fail(c Capability, scope, detail string) {
	r.blocked = true
	r.note(c, scope, detail)
}

// refused records why a probe call failed: an authorisation error says what to
// grant, any other error is shown as it is.
func (r *run) refused(c Capability, scope string, err error, hint string) {
	if cfapi.IsAuth(err) {
		r.fail(c, scope, hint)
		return
	}
	r.fail(c, scope, err.Error())
}

// result records the outcome of a probe call and reports whether it passed.
func (r *run) result(c Capability, scope string, err error, hint string) bool {
	if err != nil {
		r.refused(c, scope, err, hint)
		return false
	}
	r.pass(c, scope)
	return true
}

func grant(permission, scope string) string { return "grant " + permission + " on " + scope }

// verify checks the token itself and reports whether the probes can go on.
func (r *run) verify(ctx context.Context) bool {
	status, err := r.api.VerifyToken(ctx)
	if err != nil {
		r.fail(CapToken, "", err.Error())
		return false
	}
	r.report.Token = status
	if status.Status != "active" {
		r.fail(CapToken, "", inactiveDetail(status))
		return false
	}
	r.pass(CapToken, "")
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
			r.note(CapZones, z.Name, "zone is "+z.Status+" at Cloudflare")
			continue
		}
		active = append(active, z)
	}
	r.active = len(active)
	for _, z := range active {
		r.probeZone(ctx, z)
	}
	for _, a := range r.probedAccounts(active) {
		r.probeAccount(ctx, a)
	}
}

func (r *run) listAccounts(ctx context.Context) {
	hint := grant(permTunnelRead, "the account")
	accounts, err := r.api.Accounts(ctx)
	if err == nil && len(accounts) == 0 {
		r.fail(CapAccounts, "", "token sees no accounts; "+hint)
		return
	}
	if !r.result(CapAccounts, "", err, hint) {
		return
	}
	accounts = slices.Clone(accounts)
	slices.SortFunc(accounts, func(a, b cfapi.Account) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	r.report.Accounts = accounts
}

// listZones returns the zones the token sees, which are also in the report.
func (r *run) listZones(ctx context.Context) []cfapi.Zone {
	hint := grant(permZoneRead, "the zones to manage")
	zones, err := r.api.Zones(ctx)
	if err == nil && len(zones) == 0 {
		r.fail(CapZones, "", "token sees no zones; "+hint)
		return nil
	}
	if !r.result(CapZones, "", err, hint) {
		return nil
	}
	zones = slices.Clone(zones)
	slices.SortFunc(zones, func(a, b cfapi.Zone) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	r.report.Zones = zones
	return zones
}

// probedAccounts returns the accounts that own an active zone. The zone names
// its account, so an account the listing lacks is still probed, by id.
func (r *run) probedAccounts(active []cfapi.Zone) []cfapi.Account {
	var out []cfapi.Account
	for _, z := range active {
		if slices.ContainsFunc(out, func(a cfapi.Account) bool { return a.ID == z.AccountID }) {
			continue
		}
		a := cfapi.Account{ID: z.AccountID, Name: z.AccountID}
		if i := slices.IndexFunc(r.report.Accounts, func(x cfapi.Account) bool { return x.ID == z.AccountID }); i >= 0 {
			a = r.report.Accounts[i]
		}
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b cfapi.Account) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return out
}

func (r *run) probeZone(ctx context.Context, z cfapi.Zone) {
	filter := cfapi.RecordFilter{CommentPrefix: planner.DNSMarker(r.checker.installID)}
	_, err := r.api.Records(ctx, z.ID, filter)
	if r.result(CapDNSRead, z.Name, err, grant(permDNSRead, z.Name)) && r.deep {
		r.probeDNSWrite(ctx, z)
	}
}

func (r *run) probeDNSWrite(ctx context.Context, z cfapi.Zone) {
	hint := grant(permDNSEdit, z.Name)
	name := "_pco-probe-" + r.suffix + "." + z.Name
	created, err := r.api.CreateRecord(ctx, z.ID, cfapi.Record{
		Type:    "TXT",
		Name:    name,
		Content: "pco permission probe",
		Comment: planner.DNSMarker(r.checker.installID) + " probe",
	})
	if err != nil {
		r.refused(CapDNSWrite, z.Name, err, hint)
		return
	}
	cleanup, cancel := cleanupContext(ctx)
	defer cancel()
	if err := r.api.DeleteRecord(cleanup, z.ID, created.ID); err != nil {
		r.fail(CapDNSWrite, z.Name, leftBehind("record", name, err, hint))
		return
	}
	r.pass(CapDNSWrite, z.Name)
}

func (r *run) probeAccount(ctx context.Context, a cfapi.Account) {
	_, _, err := r.api.FindTunnel(ctx, a.ID, planner.TunnelName(r.checker.installID))
	if r.result(CapTunnelRead, a.Name, err, grant(permTunnelRead, a.Name)) && r.deep {
		r.probeTunnelWrite(ctx, a)
	}
}

func (r *run) probeTunnelWrite(ctx context.Context, a cfapi.Account) {
	hint := grant(permTunnelEdit, a.Name)
	name := planner.TunnelName(r.checker.installID) + "-probe-" + r.suffix
	created, err := r.api.CreateTunnel(ctx, a.ID, name)
	if err != nil {
		r.refused(CapTunnelWrite, a.Name, err, hint)
		return
	}
	cleanup, cancel := cleanupContext(ctx)
	defer cancel()
	if err := r.api.DeleteTunnel(cleanup, a.ID, created.ID); err != nil {
		r.fail(CapTunnelWrite, a.Name, leftBehind("tunnel", name, err, hint))
		return
	}
	r.pass(CapTunnelWrite, a.Name)
}

// cleanupContext returns the context a probe object is deleted with. A run
// that was cancelled after it created the object must still remove it.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// leftBehind is the detail of a probe that was created and could not be
// deleted. The admin has to remove it, and the hint says which permission
// covers deleting.
func leftBehind(kind, name string, err error, hint string) string {
	detail := "probe " + kind + " left behind: " + name
	if !cfapi.IsAuth(err) {
		detail += " (" + err.Error() + ")"
	}
	return detail + "; " + hint
}
