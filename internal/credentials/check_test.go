package credentials

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
)

const (
	install     = "abc"
	probeRecord = "_pco-probe-r4nd.example.com"
	probeTunnel = "pco-abc-probe-r4nd"
)

var t0 = time.Date(2026, time.March, 4, 5, 6, 7, 0, time.UTC)

func newChecker() *Checker {
	return NewChecker(install, func() time.Time { return t0 }, func() string { return "r4nd" })
}

// newFake returns a fake with one account, Acme, that owns one zone,
// example.com.
func newFake() *cffake.Fake {
	f := cffake.New()
	f.AddAccount("acct1", "Acme")
	f.AddZone("zone1", "example.com", "acct1")
	return f
}

func ok(c Capability, scope string) Check {
	return Check{Capability: c, Scope: scope, OK: true}
}

func failed(c Capability, scope, detail string) Check {
	return Check{Capability: c, Scope: scope, Detail: detail}
}

// find returns the one check of a capability and scope.
func find(t *testing.T, r Report, c Capability, scope string) Check {
	t.Helper()
	var found []Check
	for _, check := range r.Checks {
		if check.Capability == c && check.Scope == scope {
			found = append(found, check)
		}
	}
	require.Len(t, found, 1, "checks of %s for %q in %v", c, scope, r.Checks)
	return found[0]
}

// failures returns the checks that did not pass.
func failures(r Report) []Check {
	var out []Check
	for _, check := range r.Checks {
		if !check.OK {
			out = append(out, check)
		}
	}
	return out
}

// callsTo returns the calls that start with one of the methods.
func callsTo(f *cffake.Fake, methods ...string) []string {
	var out []string
	for _, call := range f.Calls() {
		if slices.ContainsFunc(methods, func(m string) bool { return strings.HasPrefix(call, m+" ") || call == m }) {
			out = append(out, call)
		}
	}
	return out
}

func requireNothingLeft(t *testing.T, f *cffake.Fake, zoneIDs, accountIDs []string) {
	t.Helper()
	for _, id := range zoneIDs {
		require.Empty(t, f.RecordsIn(id), "records left in %s", id)
	}
	for _, id := range accountIDs {
		require.Empty(t, f.TunnelsIn(id), "tunnels left in %s", id)
	}
}

// spyAPI wraps an API to change or record what a test needs and the fake
// cannot do: a zone that is not active, a refusal in one zone only, a delete
// that fails after the create worked.
type spyAPI struct {
	cfapi.API

	zoneStatus   map[string]string // zone id -> status to report
	denyCreateIn map[string]bool   // zone id -> CreateRecord answers 403
	deleteErr    error             // what DeleteRecord and DeleteTunnel answer

	afterCreateRecord func()
	afterCreateTunnel func()

	filters []cfapi.RecordFilter
	records []cfapi.Record
	tunnels []string
}

func (s *spyAPI) Zones(ctx context.Context) ([]cfapi.Zone, error) {
	zones, err := s.API.Zones(ctx)
	for i := range zones {
		if status, ok := s.zoneStatus[zones[i].ID]; ok {
			zones[i].Status = status
		}
	}
	return zones, err
}

func (s *spyAPI) Records(ctx context.Context, zoneID string, f cfapi.RecordFilter) ([]cfapi.Record, error) {
	s.filters = append(s.filters, f)
	return s.API.Records(ctx, zoneID, f)
}

func (s *spyAPI) CreateRecord(ctx context.Context, zoneID string, r cfapi.Record) (cfapi.Record, error) {
	if s.denyCreateIn[zoneID] {
		return cfapi.Record{}, &cfapi.Error{Status: 403, Message: "denied"}
	}
	s.records = append(s.records, r)
	got, err := s.API.CreateRecord(ctx, zoneID, r)
	if err == nil && s.afterCreateRecord != nil {
		s.afterCreateRecord()
	}
	return got, err
}

func (s *spyAPI) DeleteRecord(ctx context.Context, zoneID, recordID string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.API.DeleteRecord(ctx, zoneID, recordID)
}

func (s *spyAPI) CreateTunnel(ctx context.Context, accountID, name string) (cfapi.Tunnel, error) {
	s.tunnels = append(s.tunnels, name)
	got, err := s.API.CreateTunnel(ctx, accountID, name)
	if err == nil && s.afterCreateTunnel != nil {
		s.afterCreateTunnel()
	}
	return got, err
}

func (s *spyAPI) DeleteTunnel(ctx context.Context, accountID, tunnelID string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.API.DeleteTunnel(ctx, accountID, tunnelID)
}

func TestCapableTokenIsUsable(t *testing.T) {
	f := newFake()

	got := newChecker().Run(t.Context(), f, true)

	require.Equal(t, []Check{
		ok(CapToken, ""),
		ok(CapAccounts, ""),
		ok(CapZones, ""),
		ok(CapDNSRead, "example.com"),
		ok(CapDNSWrite, "example.com"),
		ok(CapTunnelRead, "Acme"),
		ok(CapTunnelWrite, "Acme"),
	}, got.Checks)
	require.True(t, got.Usable)
	require.Equal(t, t0, got.CheckedAt)
	require.Equal(t, cfapi.TokenStatus{ID: "token-1", Status: "active"}, got.Token)
	require.Equal(t, []cfapi.Account{{ID: "acct1", Name: "Acme"}}, got.Accounts)
	require.Equal(t, []cfapi.Zone{{ID: "zone1", Name: "example.com", Status: "active", AccountID: "acct1"}}, got.Zones)
	requireNothingLeft(t, f, []string{"zone1"}, []string{"acct1"})
	require.Equal(t, []string{
		"VerifyToken",
		"Accounts",
		"Zones",
		"Records zone1",
		"CreateRecord zone1 " + probeRecord,
		"DeleteRecord zone1 rec-1",
		"FindTunnel acct1 pco-abc",
		"CreateTunnel acct1 " + probeTunnel,
		"DeleteTunnel acct1 00000000-0000-4000-8000-000000000001",
	}, f.Calls())
}

func TestProbesAreMarkedObjects(t *testing.T) {
	spy := &spyAPI{API: newFake()}

	newChecker().Run(t.Context(), spy, true)

	require.Equal(t, []cfapi.RecordFilter{{CommentPrefix: "pco:abc"}}, spy.filters)
	require.Equal(t, []cfapi.Record{{
		Type:    "TXT",
		Name:    probeRecord,
		Content: "pco permission probe",
		Comment: "pco:abc probe",
		Proxied: false,
	}}, spy.records)
	require.Equal(t, []string{probeTunnel}, spy.tunnels)
}

func TestShallowRunMakesNoWriteCall(t *testing.T) {
	f := newFake()
	var randCalls atomic.Int32
	checker := NewChecker(install, func() time.Time { return t0 }, func() string {
		randCalls.Add(1)
		return "r4nd"
	})

	got := checker.Run(t.Context(), f, false)

	require.Equal(t, []Check{
		ok(CapToken, ""),
		ok(CapAccounts, ""),
		ok(CapZones, ""),
		ok(CapDNSRead, "example.com"),
		ok(CapTunnelRead, "Acme"),
	}, got.Checks)
	require.True(t, got.Usable)
	require.Equal(t, []string{
		"VerifyToken",
		"Accounts",
		"Zones",
		"Records zone1",
		"FindTunnel acct1 pco-abc",
	}, f.Calls())
	require.Zero(t, randCalls.Load())
}

func TestDeepRunDrawsOneRandomValue(t *testing.T) {
	f := newFake()
	f.AddAccount("acct2", "Beta")
	f.AddZone("zone2", "example.org", "acct2")
	var randCalls atomic.Int32
	checker := NewChecker(install, func() time.Time { return t0 }, func() string {
		randCalls.Add(1)
		return "r4nd"
	})

	got := checker.Run(t.Context(), f, true)

	require.True(t, got.Usable)
	require.Equal(t, int32(1), randCalls.Load())
	require.Equal(t, []string{
		"CreateRecord zone1 _pco-probe-r4nd.example.com",
		"CreateRecord zone2 _pco-probe-r4nd.example.org",
	}, callsTo(f, "CreateRecord"))
}

func TestDeniedWriteNamesThePermission(t *testing.T) {
	tests := []struct {
		name   string
		op     string
		failed Check
	}{
		{
			name:   "dns write",
			op:     "dns.write",
			failed: failed(CapDNSWrite, "example.com", "grant Zone > DNS > Edit on example.com"),
		},
		{
			name:   "tunnel write",
			op:     "tunnel.write",
			failed: failed(CapTunnelWrite, "Acme", "grant Account > Cloudflare Tunnel > Edit on Acme"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.Deny(tc.op)

			got := newChecker().Run(t.Context(), f, true)

			require.Equal(t, []Check{tc.failed}, failures(got))
			require.Len(t, got.Checks, 7)
			require.False(t, got.Usable)
			requireNothingLeft(t, f, []string{"zone1"}, []string{"acct1"})
		})
	}
}

func TestDeniedWriteInOneZoneNamesThatZone(t *testing.T) {
	f := newFake()
	f.AddAccount("acct2", "Beta")
	f.AddZone("zone2", "example.org", "acct2")
	spy := &spyAPI{API: f, denyCreateIn: map[string]bool{"zone2": true}}

	got := newChecker().Run(t.Context(), spy, true)

	require.Equal(t, []Check{
		failed(CapDNSWrite, "example.org", "grant Zone > DNS > Edit on example.org"),
	}, failures(got))
	require.True(t, find(t, got, CapDNSWrite, "example.com").OK)
	require.False(t, got.Usable)
}

func TestDeniedReadSkipsTheMatchingWriteProbe(t *testing.T) {
	tests := []struct {
		name    string
		op      string
		failed  Check
		missing Capability
		writes  string
	}{
		{
			name:    "dns read",
			op:      "dns.read",
			failed:  failed(CapDNSRead, "example.com", "grant Zone > DNS > Read on example.com"),
			missing: CapDNSWrite,
			writes:  "CreateRecord",
		},
		{
			name:    "tunnel read",
			op:      "tunnel.read",
			failed:  failed(CapTunnelRead, "Acme", "grant Account > Cloudflare Tunnel > Read on Acme"),
			missing: CapTunnelWrite,
			writes:  "CreateTunnel",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.Deny(tc.op)

			got := newChecker().Run(t.Context(), f, true)

			require.Equal(t, []Check{tc.failed}, failures(got))
			require.Len(t, got.Checks, 6)
			for _, check := range got.Checks {
				require.NotEqual(t, tc.missing, check.Capability)
			}
			require.Empty(t, callsTo(f, tc.writes))
			require.False(t, got.Usable)
		})
	}
}

func TestDeniedZoneListing(t *testing.T) {
	f := newFake()
	f.Deny("zones")

	got := newChecker().Run(t.Context(), f, true)

	require.Equal(t, []Check{
		ok(CapToken, ""),
		ok(CapAccounts, ""),
		failed(CapZones, "", "grant Zone > Zone > Read on the zones to manage"),
	}, got.Checks)
	require.Empty(t, got.Zones)
	require.False(t, got.Usable)
	require.Equal(t, []string{"VerifyToken", "Accounts", "Zones"}, f.Calls())
}

func TestDeniedAccountListing(t *testing.T) {
	f := newFake()
	f.Deny("accounts")

	got := newChecker().Run(t.Context(), f, true)

	require.Equal(t, []Check{
		ok(CapToken, ""),
		failed(CapAccounts, "", "grant Account > Cloudflare Tunnel > Read on the account"),
		ok(CapZones, ""),
		ok(CapDNSRead, "example.com"),
		ok(CapDNSWrite, "example.com"),
		ok(CapTunnelRead, "acct1"),
		ok(CapTunnelWrite, "acct1"),
	}, got.Checks)
	require.Empty(t, got.Accounts)
	require.False(t, got.Usable)
}

func TestNoZonesIsNotUsable(t *testing.T) {
	f := cffake.New()
	f.AddAccount("acct1", "Acme")

	got := newChecker().Run(t.Context(), f, true)

	require.Equal(t, []Check{
		ok(CapToken, ""),
		ok(CapAccounts, ""),
		failed(CapZones, "", "token sees no zones; grant Zone > Zone > Read on the zones to manage"),
	}, got.Checks)
	require.Equal(t, []cfapi.Account{{ID: "acct1", Name: "Acme"}}, got.Accounts)
	require.False(t, got.Usable)
	require.Equal(t, []string{"VerifyToken", "Accounts", "Zones"}, f.Calls())
}

func TestNoAccountsIsReported(t *testing.T) {
	f := cffake.New()

	got := newChecker().Run(t.Context(), f, true)

	require.Equal(t, failed(CapAccounts, "", "token sees no accounts; grant Account > Cloudflare Tunnel > Read on the account"),
		find(t, got, CapAccounts, ""))
	require.False(t, got.Usable)
}

func TestPendingZoneFailsAloneAndLeavesTheTokenUsable(t *testing.T) {
	f := newFake()
	f.AddAccount("acct2", "Beta")
	f.AddZone("zone2", "example.org", "acct2")
	spy := &spyAPI{API: f, zoneStatus: map[string]string{"zone2": "pending"}}

	got := newChecker().Run(t.Context(), spy, true)

	require.Equal(t, []Check{
		ok(CapToken, ""),
		ok(CapAccounts, ""),
		ok(CapZones, ""),
		failed(CapZones, "example.org", "zone is pending at Cloudflare"),
		ok(CapDNSRead, "example.com"),
		ok(CapDNSWrite, "example.com"),
		ok(CapTunnelRead, "Acme"),
		ok(CapTunnelWrite, "Acme"),
	}, got.Checks)
	require.True(t, got.Usable)
	require.Equal(t, []cfapi.Zone{
		{ID: "zone1", Name: "example.com", Status: "active", AccountID: "acct1"},
		{ID: "zone2", Name: "example.org", Status: "pending", AccountID: "acct2"},
	}, got.Zones)
	require.Equal(t, []cfapi.Account{{ID: "acct1", Name: "Acme"}, {ID: "acct2", Name: "Beta"}}, got.Accounts)
	for _, call := range f.Calls() {
		require.NotContains(t, call, "zone2")
		require.NotContains(t, call, "acct2")
	}
}

func TestOnlyPendingZonesIsNotUsable(t *testing.T) {
	f := newFake()
	spy := &spyAPI{API: f, zoneStatus: map[string]string{"zone1": "pending"}}

	got := newChecker().Run(t.Context(), spy, true)

	require.Equal(t, []Check{
		ok(CapToken, ""),
		ok(CapAccounts, ""),
		ok(CapZones, ""),
		failed(CapZones, "example.com", "zone is pending at Cloudflare"),
	}, got.Checks)
	require.False(t, got.Usable)
	require.Equal(t, []string{"VerifyToken", "Accounts", "Zones"}, f.Calls())
}

func TestAccountWithoutZonesIsListedNotProbed(t *testing.T) {
	f := newFake()
	f.AddAccount("acct2", "Idle")

	got := newChecker().Run(t.Context(), f, true)

	require.Equal(t, []cfapi.Account{{ID: "acct1", Name: "Acme"}, {ID: "acct2", Name: "Idle"}}, got.Accounts)
	require.True(t, got.Usable)
	for _, call := range f.Calls() {
		require.NotContains(t, call, "acct2")
	}
	for _, check := range got.Checks {
		require.NotEqual(t, "Idle", check.Scope)
	}
}

func TestTokenThatIsNotActive(t *testing.T) {
	expired := time.Date(2026, time.February, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		setup  func(f *cffake.Fake)
		detail string
		token  cfapi.TokenStatus
	}{
		{
			name:   "disabled",
			setup:  func(f *cffake.Fake) { f.SetTokenStatus("disabled", nil) },
			detail: "token is disabled",
			token:  cfapi.TokenStatus{ID: "token-1", Status: "disabled"},
		},
		{
			name:   "expired with a date",
			setup:  func(f *cffake.Fake) { f.SetTokenStatus("expired", &expired) },
			detail: "token expired on 2026-02-01",
			token:  cfapi.TokenStatus{ID: "token-1", Status: "expired", ExpiresOn: &expired},
		},
		{
			name:   "expired without a date",
			setup:  func(f *cffake.Fake) { f.SetTokenStatus("expired", nil) },
			detail: "token is expired",
			token:  cfapi.TokenStatus{ID: "token-1", Status: "expired"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			tc.setup(f)

			got := newChecker().Run(t.Context(), f, true)

			require.Equal(t, []Check{failed(CapToken, "", tc.detail)}, got.Checks)
			require.Equal(t, tc.token, got.Token)
			require.False(t, got.Usable)
			require.Equal(t, t0, got.CheckedAt)
			require.Empty(t, got.Accounts)
			require.Empty(t, got.Zones)
			require.Equal(t, []string{"VerifyToken"}, f.Calls())
		})
	}
}

func TestActiveTokenKeepsItsExpiry(t *testing.T) {
	f := newFake()
	expires := time.Date(2027, time.January, 5, 0, 0, 0, 0, time.UTC)
	f.SetTokenStatus("active", &expires)

	got := newChecker().Run(t.Context(), f, false)

	require.True(t, got.Usable)
	require.Equal(t, cfapi.TokenStatus{ID: "token-1", Status: "active", ExpiresOn: &expires}, got.Token)
}

func TestFailingVerificationStopsEarly(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *cffake.Fake)
		want  string
	}{
		{
			name:  "network error",
			setup: func(f *cffake.Fake) { f.FailNext("verify", 1, errors.New("dial tcp: connection refused")) },
			want:  "dial tcp: connection refused",
		},
		{
			name:  "rejected token",
			setup: func(f *cffake.Fake) { f.Deny("verify") },
			want:  "HTTP 403",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			tc.setup(f)

			got := newChecker().Run(t.Context(), f, true)

			require.Len(t, got.Checks, 1)
			require.Equal(t, CapToken, got.Checks[0].Capability)
			require.False(t, got.Checks[0].OK)
			require.Contains(t, got.Checks[0].Detail, tc.want)
			require.False(t, got.Usable)
			require.Equal(t, []string{"VerifyToken"}, f.Calls())
		})
	}
}

func TestErrorsThatAreNotAuthorisationCarryNoHint(t *testing.T) {
	tests := []struct {
		name  string
		op    string
		check func(*testing.T, Report) Check
	}{
		{"accounts", "accounts", func(t *testing.T, r Report) Check { return find(t, r, CapAccounts, "") }},
		{"zones", "zones", func(t *testing.T, r Report) Check { return find(t, r, CapZones, "") }},
		{"dns read", "dns.read", func(t *testing.T, r Report) Check { return find(t, r, CapDNSRead, "example.com") }},
		{"dns write", "dns.write", func(t *testing.T, r Report) Check { return find(t, r, CapDNSWrite, "example.com") }},
		{"tunnel read", "tunnel.read", func(t *testing.T, r Report) Check { return find(t, r, CapTunnelRead, "Acme") }},
		{"tunnel write", "tunnel.write", func(t *testing.T, r Report) Check { return find(t, r, CapTunnelWrite, "Acme") }},
	}
	errs := []error{
		errors.New("connection reset by peer"),
		&cfapi.Error{Status: 429, Message: "rate limited", RetryAfter: 5 * time.Second},
		&cfapi.Error{Status: 500, Message: "internal error"},
	}
	for _, tc := range tests {
		for _, injected := range errs {
			t.Run(tc.name+"/"+injected.Error(), func(t *testing.T) {
				f := newFake()
				f.FailNext(tc.op, 1, injected)

				got := newChecker().Run(t.Context(), f, true)

				check := tc.check(t, got)
				require.False(t, check.OK)
				require.Equal(t, injected.Error(), check.Detail)
				require.NotContains(t, check.Detail, "grant")
				require.False(t, got.Usable)
				requireNothingLeft(t, f, []string{"zone1"}, []string{"acct1"})
			})
		}
	}
}

func TestFailedDeleteReportsTheLeftoverProbe(t *testing.T) {
	tests := []struct {
		name      string
		deleteErr error
		cause     string
	}{
		{
			name:      "refused",
			deleteErr: &cfapi.Error{Status: 403, Message: "denied"},
		},
		{
			name:      "unreachable",
			deleteErr: errors.New("connection reset"),
			cause:     " (connection reset)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			spy := &spyAPI{API: f, deleteErr: tc.deleteErr}

			got := newChecker().Run(t.Context(), spy, true)

			require.Equal(t,
				failed(CapDNSWrite, "example.com",
					"probe record left behind: "+probeRecord+tc.cause+"; grant Zone > DNS > Edit on example.com"),
				find(t, got, CapDNSWrite, "example.com"))
			require.Equal(t,
				failed(CapTunnelWrite, "Acme",
					"probe tunnel left behind: "+probeTunnel+tc.cause+"; grant Account > Cloudflare Tunnel > Edit on Acme"),
				find(t, got, CapTunnelWrite, "Acme"))
			require.False(t, got.Usable)

			records := f.RecordsIn("zone1")
			require.Len(t, records, 1)
			require.Equal(t, probeRecord, records[0].Name)
			require.True(t, cfapi.RecordFilter{CommentPrefix: "pco:abc"}.Matches(records[0]), "leftover must carry the marker")
			tunnels := f.TunnelsIn("acct1")
			require.Len(t, tunnels, 1)
			require.Equal(t, probeTunnel, tunnels[0].Name)
		})
	}
}

func TestOnlyObjectsItCreatedAreTouched(t *testing.T) {
	f := newFake()
	f.SeedRecord("zone1", cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: "x.cfargotunnel.com", Proxied: true, Comment: "pco:abc"})
	f.SeedRecord("zone1", cfapi.Record{Type: "TXT", Name: "_pco-probe-old.example.com", Content: "pco permission probe", Comment: "pco:abc probe"})
	f.SeedRecord("zone1", cfapi.Record{Type: "A", Name: "www.example.com", Content: "192.0.2.1"})
	f.SeedTunnel("acct1", "pco-abc", nil)
	f.SeedTunnel("acct1", "pco-abc-probe-old", nil)
	f.SeedTunnel("acct1", "someone-elses", nil)
	records, tunnels := f.RecordsIn("zone1"), f.TunnelsIn("acct1")

	got := newChecker().Run(t.Context(), f, true)

	require.True(t, got.Usable)
	require.Equal(t, records, f.RecordsIn("zone1"))
	require.Equal(t, tunnels, f.TunnelsIn("acct1"))
	require.Len(t, callsTo(f, "DeleteRecord"), 1)
	require.Len(t, callsTo(f, "DeleteTunnel"), 1)
}

func TestProbeNameTakenByAnotherObjectFailsWithoutDeleting(t *testing.T) {
	f := newFake()
	f.SeedRecord("zone1", cfapi.Record{Type: "TXT", Name: probeRecord, Content: "pco permission probe", Comment: "pco:abc probe"})
	f.SeedTunnel("acct1", probeTunnel, nil)
	records, tunnels := f.RecordsIn("zone1"), f.TunnelsIn("acct1")

	got := newChecker().Run(t.Context(), f, true)

	for _, check := range []Check{find(t, got, CapDNSWrite, "example.com"), find(t, got, CapTunnelWrite, "Acme")} {
		require.False(t, check.OK, "%v", check)
		require.NotContains(t, check.Detail, "grant")
		require.NotContains(t, check.Detail, "left behind")
	}
	require.False(t, got.Usable)
	require.Equal(t, records, f.RecordsIn("zone1"))
	require.Equal(t, tunnels, f.TunnelsIn("acct1"))
	require.Empty(t, callsTo(f, "DeleteRecord", "DeleteTunnel"))
}

func TestCancelledRunStillRemovesItsProbes(t *testing.T) {
	tests := []struct {
		name  string
		hook  func(s *spyAPI, cancel context.CancelFunc)
		check Check
	}{
		{
			name:  "record",
			hook:  func(s *spyAPI, cancel context.CancelFunc) { s.afterCreateRecord = cancel },
			check: ok(CapDNSWrite, "example.com"),
		},
		{
			name:  "tunnel",
			hook:  func(s *spyAPI, cancel context.CancelFunc) { s.afterCreateTunnel = cancel },
			check: ok(CapTunnelWrite, "Acme"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			spy := &spyAPI{API: f}
			tc.hook(spy, cancel)

			got := newChecker().Run(ctx, spy, true)

			require.Equal(t, tc.check, find(t, got, tc.check.Capability, tc.check.Scope))
			requireNothingLeft(t, f, []string{"zone1"}, []string{"acct1"})
		})
	}
}

func TestChecksAndListsAreSorted(t *testing.T) {
	f := cffake.New()
	f.AddAccount("acct-b", "Beta")
	f.AddAccount("acct-a", "Acme")
	f.AddZone("zone-z", "zulu.org", "acct-b")
	f.AddZone("zone-m", "mike.net", "acct-a")
	f.AddZone("zone-a", "alpha.com", "acct-a")
	f.AddZone("zone-d", "dead.example", "acct-b")
	spy := &spyAPI{API: f, zoneStatus: map[string]string{"zone-d": "pending"}}

	got := newChecker().Run(t.Context(), spy, true)

	var order []string
	for _, check := range got.Checks {
		order = append(order, string(check.Capability)+"@"+check.Scope)
	}
	require.Equal(t, []string{
		"token@",
		"accounts@",
		"zones@",
		"zones@dead.example",
		"dns.read@alpha.com",
		"dns.read@mike.net",
		"dns.read@zulu.org",
		"dns.write@alpha.com",
		"dns.write@mike.net",
		"dns.write@zulu.org",
		"tunnel.read@Acme",
		"tunnel.read@Beta",
		"tunnel.write@Acme",
		"tunnel.write@Beta",
	}, order)
	require.Equal(t, []cfapi.Account{{ID: "acct-a", Name: "Acme"}, {ID: "acct-b", Name: "Beta"}}, got.Accounts)
	var zones []string
	for _, z := range got.Zones {
		zones = append(zones, z.Name)
	}
	require.Equal(t, []string{"alpha.com", "dead.example", "mike.net", "zulu.org"}, zones)
	require.True(t, got.Usable)
}

func TestConcurrentRunsForDifferentCredentials(t *testing.T) {
	checker := newChecker()
	const n = 8
	reports := make([]Report, n)
	fakes := make([]*cffake.Fake, n)
	var wg sync.WaitGroup
	for i := range n {
		fakes[i] = newFake()
		if i%2 == 1 {
			fakes[i].Deny("dns.write")
		}
		wg.Go(func() { reports[i] = checker.Run(t.Context(), fakes[i], true) })
	}
	wg.Wait()

	for i := range n {
		require.Equal(t, i%2 == 0, reports[i].Usable, "report %d: %v", i, reports[i].Checks)
		requireNothingLeft(t, fakes[i], []string{"zone1"}, []string{"acct1"})
	}
}
