package cffake

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

const (
	acct = "acct1"
	zone = "zone1"
)

var (
	ctx         = context.Background()
	t0          = time.Date(2026, time.March, 4, 5, 6, 7, 0, time.UTC)
	uuidPattern = regexp.MustCompile(`^[0-9a-f-]{36}$`)
)

// newFake returns a fake with one account that owns one zone, on a clock that
// stands still at t0.
func newFake() *Fake {
	f := New()
	f.SetNow(func() time.Time { return t0 })
	f.AddAccount(acct, "Acme")
	f.AddZone(zone, "example.com", acct)
	return f
}

func cname(name, content string) cfapi.Record {
	return cfapi.Record{Type: "CNAME", Name: name, Content: content, Proxied: true, Comment: "pco:abc"}
}

func rec(typ, name, content string) cfapi.Record {
	return cfapi.Record{Type: typ, Name: name, Content: content}
}

func TestAccountsAndZones(t *testing.T) {
	f := New()
	f.AddAccount("a1", "First")
	f.AddAccount("a2", "Second")
	f.AddZone("z1", "example.com", "a1")
	f.AddZone("z2", "example.org", "a2")

	accounts, err := f.Accounts(ctx)
	require.NoError(t, err)
	require.Equal(t, []cfapi.Account{{ID: "a1", Name: "First"}, {ID: "a2", Name: "Second"}}, accounts)

	zones, err := f.Zones(ctx)
	require.NoError(t, err)
	require.Equal(t, []cfapi.Zone{
		{ID: "z1", Name: "example.com", Status: "active", AccountID: "a1"},
		{ID: "z2", Name: "example.org", Status: "active", AccountID: "a2"},
	}, zones)
}

func TestAddingAgainReplaces(t *testing.T) {
	f := New()
	f.AddAccount("a1", "First")
	f.AddAccount("a1", "Renamed")
	f.AddZone("z1", "example.com", "a1")
	f.AddZone("z1", "example.net", "a1")

	accounts, err := f.Accounts(ctx)
	require.NoError(t, err)
	require.Equal(t, []cfapi.Account{{ID: "a1", Name: "Renamed"}}, accounts)
	zones, err := f.Zones(ctx)
	require.NoError(t, err)
	require.Len(t, zones, 1)
	require.Equal(t, "example.net", zones[0].Name)
}

func TestEmptyFakeListsNothing(t *testing.T) {
	f := New()
	accounts, err := f.Accounts(ctx)
	require.NoError(t, err)
	require.Empty(t, accounts)
	zones, err := f.Zones(ctx)
	require.NoError(t, err)
	require.Empty(t, zones)
}

func TestVerifyToken(t *testing.T) {
	st, err := New().VerifyToken(ctx)
	require.NoError(t, err)
	require.Equal(t, "active", st.Status)
	require.NotEmpty(t, st.ID)
	require.Nil(t, st.ExpiresOn)
}

func TestTunnelLifecycle(t *testing.T) {
	f := newFake()

	_, found, err := f.FindTunnel(ctx, acct, "pco-abc")
	require.NoError(t, err)
	require.False(t, found)

	created, err := f.CreateTunnel(ctx, acct, "pco-abc")
	require.NoError(t, err)
	require.Regexp(t, uuidPattern, created.ID)
	require.Equal(t, "pco-abc", created.Name)
	require.Equal(t, "inactive", created.Status)
	require.Equal(t, t0, created.CreatedAt)

	got, found, err := f.FindTunnel(ctx, acct, "pco-abc")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, created, got)
	require.Equal(t, []cfapi.Tunnel{created}, f.TunnelsIn(acct))

	token, err := f.TunnelToken(ctx, acct, created.ID)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	conns, err := f.Connectors(ctx, acct, created.ID)
	require.NoError(t, err)
	require.Empty(t, conns)

	require.NoError(t, f.DeleteTunnel(ctx, acct, created.ID))
	_, found, err = f.FindTunnel(ctx, acct, "pco-abc")
	require.NoError(t, err)
	require.False(t, found)
	require.Empty(t, f.TunnelsIn(acct))

	// A deleted tunnel is gone for every call that names it.
	require.True(t, cfapi.IsNotFound(f.DeleteTunnel(ctx, acct, created.ID)))
	_, err = f.TunnelConfig(ctx, acct, created.ID)
	require.True(t, cfapi.IsNotFound(err))
}

func TestTunnelIDsAreDistinctUUIDShapedAndRepeatable(t *testing.T) {
	ids := func() []string {
		f := newFake()
		f.AddAccount("acct2", "Other")
		a := f.SeedTunnel(acct, "pco-a", nil)
		b, err := f.CreateTunnel(ctx, acct, "pco-b")
		require.NoError(t, err)
		c, err := f.CreateTunnel(ctx, "acct2", "pco-a")
		require.NoError(t, err)
		return []string{a.ID, b.ID, c.ID}
	}

	first := ids()
	require.Equal(t, first, ids(), "the same calls give the same ids")
	require.Len(t, map[string]bool{first[0]: true, first[1]: true, first[2]: true}, 3)
	for _, id := range first {
		require.Regexp(t, uuidPattern, id)
	}
}

func TestTunnelTokensDifferPerTunnel(t *testing.T) {
	f := newFake()
	a := f.SeedTunnel(acct, "pco-a", nil)
	b := f.SeedTunnel(acct, "pco-b", nil)

	ta, err := f.TunnelToken(ctx, acct, a.ID)
	require.NoError(t, err)
	tb, err := f.TunnelToken(ctx, acct, b.ID)
	require.NoError(t, err)
	require.NotEqual(t, ta, tb)
	again, err := f.TunnelToken(ctx, acct, a.ID)
	require.NoError(t, err)
	require.Equal(t, ta, again)
}

// A rotated secret makes the tokens handed out from then on; the connectors
// stay until their connections are cleaned up.
func TestARotatedSecretMakesTheTokens(t *testing.T) {
	f := newFake()
	a := f.SeedTunnel(acct, "pco-a", nil)
	b := f.SeedTunnel(acct, "pco-b", nil)
	f.SetConnectors(acct, a.ID, []cfapi.Connector{{ID: "c1", Connections: 4}})
	before, err := f.TunnelToken(ctx, acct, a.ID)
	require.NoError(t, err)
	require.Equal(t, RunToken(acct, a.ID), before)
	secret := []byte("new-secret-of-thirty-two-bytes!!")

	require.NoError(t, f.RotateTunnelSecret(ctx, acct, a.ID, secret))

	after, err := f.TunnelToken(ctx, acct, a.ID)
	require.NoError(t, err)
	require.Equal(t, RunTokenWith(acct, a.ID, secret), after)
	require.NotEqual(t, before, after)
	other, err := f.TunnelToken(ctx, acct, b.ID)
	require.NoError(t, err)
	require.Equal(t, RunToken(acct, b.ID), other)
	conns, err := f.Connectors(ctx, acct, a.ID)
	require.NoError(t, err)
	require.Len(t, conns, 1)

	require.NoError(t, f.CleanUpConnections(ctx, acct, a.ID))
	conns, err = f.Connectors(ctx, acct, a.ID)
	require.NoError(t, err)
	require.Empty(t, conns)
	require.Equal(t, "down", f.TunnelsIn(acct)[0].Status, "it ran and has no connection: down, not inactive, which is never run")
}

func TestCreateTunnelNameConflict(t *testing.T) {
	f := newFake()
	f.AddAccount("acct2", "Other")
	first, err := f.CreateTunnel(ctx, acct, "pco-abc")
	require.NoError(t, err)

	got, err := f.CreateTunnel(ctx, acct, "pco-abc")
	require.True(t, cfapi.IsConflict(err))
	require.Equal(t, cfapi.Tunnel{}, got)
	require.Equal(t, []cfapi.Tunnel{first}, f.TunnelsIn(acct), "a refused create leaves nothing behind")

	// The rule is per account, and a deleted tunnel does not hold its name.
	_, err = f.CreateTunnel(ctx, "acct2", "pco-abc")
	require.NoError(t, err)
	require.NoError(t, f.DeleteTunnel(ctx, acct, first.ID))
	_, err = f.CreateTunnel(ctx, acct, "pco-abc")
	require.NoError(t, err)
}

func TestSeedTunnelBypassesTheNameRule(t *testing.T) {
	// Tests need the state a real account can reach by other means, such as two
	// tunnels of one name made by hand.
	f := newFake()
	f.SeedTunnel(acct, "pco-abc", nil)
	f.SeedTunnel(acct, "pco-abc", nil)
	require.Len(t, f.TunnelsIn(acct), 2)

	got, found, err := f.FindTunnel(ctx, acct, "pco-abc")
	require.Error(t, err, "pco cannot tell which of two tunnels it owns")
	require.False(t, found)
	require.Equal(t, cfapi.Tunnel{}, got)
	require.False(t, cfapi.IsNotFound(err))
}

func TestFindTunnelWantsTheExactName(t *testing.T) {
	f := newFake()
	f.SeedTunnel(acct, "pco-abc-probe-1", nil)
	f.SeedTunnel(acct, "PCO-ABC", nil)

	_, found, err := f.FindTunnel(ctx, acct, "pco-abc")
	require.NoError(t, err)
	require.False(t, found)
}

func TestSeedTunnel(t *testing.T) {
	f := newFake()
	rules := []planner.IngressRule{
		{Hostname: "a.example.com", Service: "http://10.0.0.5:80"},
		{Service: "http_status:404"},
	}

	withConfig := f.SeedTunnel(acct, "pco-a", rules)
	bare := f.SeedTunnel(acct, "pco-b", nil)

	require.Equal(t, "pco-a", withConfig.Name)
	require.Equal(t, t0, withConfig.CreatedAt)
	cfg, err := f.TunnelConfig(ctx, acct, withConfig.ID)
	require.NoError(t, err)
	require.Equal(t, cfapi.TunnelConfig{Version: 1, Ingress: rules}, cfg)

	cfg, err = f.TunnelConfig(ctx, acct, bare.ID)
	require.NoError(t, err)
	require.Equal(t, 0, cfg.Version)
	require.Empty(t, cfg.Ingress)
	require.Equal(t, []cfapi.Tunnel{withConfig, bare}, f.TunnelsIn(acct))
}

func TestPutTunnelConfigCountsVersions(t *testing.T) {
	f := newFake()
	tun, err := f.CreateTunnel(ctx, acct, "pco-abc")
	require.NoError(t, err)
	first := []planner.IngressRule{{Hostname: "a.example.com", Service: "http://10.0.0.5:80"}, {Service: "http_status:404"}}
	second := []planner.IngressRule{{Service: "http_status:404"}}

	cfg, err := f.TunnelConfig(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.Equal(t, cfapi.TunnelConfig{}, cfg, "no configuration yet: version 0, no rules")

	v, err := f.PutTunnelConfig(ctx, acct, tun.ID, first)
	require.NoError(t, err)
	require.Equal(t, 1, v)
	cfg, err = f.TunnelConfig(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.Equal(t, cfapi.TunnelConfig{Version: 1, Ingress: first}, cfg)

	v, err = f.PutTunnelConfig(ctx, acct, tun.ID, second)
	require.NoError(t, err)
	require.Equal(t, 2, v)

	v, err = f.PutTunnelConfig(ctx, acct, tun.ID, second)
	require.NoError(t, err)
	require.Equal(t, 3, v, "writing the same rules again is still a new version")
	cfg, err = f.TunnelConfig(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.Equal(t, cfapi.TunnelConfig{Version: 3, Ingress: second}, cfg)
}

func TestVersionsAreKeptPerTunnel(t *testing.T) {
	f := newFake()
	a := f.SeedTunnel(acct, "pco-a", nil)
	b := f.SeedTunnel(acct, "pco-b", nil)

	_, err := f.PutTunnelConfig(ctx, acct, a.ID, []planner.IngressRule{{Service: "http_status:404"}})
	require.NoError(t, err)

	cfg, err := f.TunnelConfig(ctx, acct, b.ID)
	require.NoError(t, err)
	require.Equal(t, 0, cfg.Version)
}

func TestTunnelsAreScopedToTheirAccount(t *testing.T) {
	f := newFake()
	f.AddAccount("acct2", "Other")
	tun := f.SeedTunnel(acct, "pco-abc", nil)

	_, err := f.TunnelConfig(ctx, "acct2", tun.ID)
	require.True(t, cfapi.IsNotFound(err))
	require.True(t, cfapi.IsNotFound(f.DeleteTunnel(ctx, "acct2", tun.ID)))
	_, found, err := f.FindTunnel(ctx, "acct2", "pco-abc")
	require.NoError(t, err)
	require.False(t, found)
	require.Empty(t, f.TunnelsIn("acct2"))
}

func TestReturnedSlicesAreCopies(t *testing.T) {
	f := newFake()
	tun := f.SeedTunnel(acct, "pco-abc", nil)
	rules := []planner.IngressRule{{Hostname: "a.example.com", Service: "http://10.0.0.5:80"}, {Service: "http_status:404"}}
	_, err := f.PutTunnelConfig(ctx, acct, tun.ID, rules)
	require.NoError(t, err)
	seeded := f.SeedTunnel(acct, "pco-seeded", rules)
	f.SeedRecord(zone, cname("a.example.com", "x"))

	// What went in is not shared with the caller.
	rules[0].Service = "http://evil"
	// Neither is what comes out.
	cfg, err := f.TunnelConfig(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.Equal(t, "http://10.0.0.5:80", cfg.Ingress[0].Service)
	cfg.Ingress[0].Service = "http://evil"
	cfg, err = f.TunnelConfig(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.Equal(t, "http://10.0.0.5:80", cfg.Ingress[0].Service)
	cfg, err = f.TunnelConfig(ctx, acct, seeded.ID)
	require.NoError(t, err)
	require.Equal(t, "http://10.0.0.5:80", cfg.Ingress[0].Service)

	tunnels := f.TunnelsIn(acct)
	tunnels[0].Name = "changed"
	require.Equal(t, "pco-abc", f.TunnelsIn(acct)[0].Name)

	records, err := f.Records(ctx, zone, cfapi.RecordFilter{})
	require.NoError(t, err)
	records[0].Content = "changed"
	require.Equal(t, "x", f.RecordsIn(zone)[0].Content)
	in := f.RecordsIn(zone)
	in[0].Content = "changed"
	require.Equal(t, "x", f.RecordsIn(zone)[0].Content)

	accounts, err := f.Accounts(ctx)
	require.NoError(t, err)
	accounts[0].Name = "changed"
	again, err := f.Accounts(ctx)
	require.NoError(t, err)
	require.Equal(t, "Acme", again[0].Name)
}

func TestRecordIDsAndClock(t *testing.T) {
	f := newFake()
	later := t0.Add(time.Hour)

	first, err := f.CreateRecord(ctx, zone, cname("a.example.com", "x"))
	require.NoError(t, err)
	f.SetNow(func() time.Time { return later })
	second, err := f.CreateRecord(ctx, zone, cname("b.example.com", "x"))
	require.NoError(t, err)

	require.Equal(t, "rec-1", first.ID)
	require.Equal(t, "rec-2", second.ID)
	require.Equal(t, t0, first.ModifiedOn)
	require.Equal(t, later, second.ModifiedOn)
	require.Equal(t, []cfapi.Record{first, second}, f.RecordsIn(zone))
}

func TestCreateRecordIgnoresIDAndModifiedOn(t *testing.T) {
	f := newFake()
	in := cname("a.example.com", "x")
	in.ID = "mine"
	in.ModifiedOn = t0.Add(-time.Hour)

	got, err := f.CreateRecord(ctx, zone, in)

	require.NoError(t, err)
	require.Equal(t, "rec-1", got.ID)
	require.Equal(t, t0, got.ModifiedOn)
	require.Equal(t, "pco:abc", got.Comment)
	require.True(t, got.Proxied)
}

func TestRecordTTL(t *testing.T) {
	f := newFake()
	auto, err := f.CreateRecord(ctx, zone, rec("A", "a.example.com", "192.0.2.0"))
	require.NoError(t, err)
	timed := rec("A", "b.example.com", "192.0.2.1")
	timed.TTL = 300
	timed, err = f.CreateRecord(ctx, zone, timed)
	require.NoError(t, err)
	seeded := f.SeedRecord(zone, rec("A", "c.example.com", "192.0.2.2"))
	seededTimed := rec("A", "d.example.com", "192.0.2.3")
	seededTimed.TTL = 600
	seededTimed = f.SeedRecord(zone, seededTimed)

	require.Equal(t, 1, auto.TTL, "an unset TTL is automatic, as the client sends it")
	require.Equal(t, 300, timed.TTL)
	require.Equal(t, 1, seeded.TTL, "Cloudflare holds no record without a TTL")
	require.Equal(t, 600, seededTimed.TTL)

	timed.TTL = 0
	updated, err := f.UpdateRecord(ctx, zone, timed)
	require.NoError(t, err)
	require.Equal(t, 1, updated.TTL)
	require.Equal(t, []int{1, 1, 1, 600}, []int{f.RecordsIn(zone)[0].TTL, f.RecordsIn(zone)[1].TTL, f.RecordsIn(zone)[2].TTL, f.RecordsIn(zone)[3].TTL})
}

func TestProxiedRecordsHaveAutomaticTTL(t *testing.T) {
	f := newFake()
	proxied := func(name string) cfapi.Record {
		r := cname(name, "x.cfargotunnel.com")
		r.TTL = 300
		return r
	}

	created, err := f.CreateRecord(ctx, zone, proxied("a.example.com"))
	require.NoError(t, err)
	seeded := f.SeedRecord(zone, proxied("b.example.com"))
	plain := rec("A", "c.example.com", "192.0.2.1")
	plain.TTL = 300
	plain, err = f.CreateRecord(ctx, zone, plain)
	require.NoError(t, err)
	plain.Proxied = true
	updated, err := f.UpdateRecord(ctx, zone, plain)
	require.NoError(t, err)

	require.Equal(t, []int{1, 1, 1}, []int{created.TTL, seeded.TTL, updated.TTL})
	for _, r := range f.RecordsIn(zone) {
		require.Equal(t, 1, r.TTL, r.Name)
	}
}

func TestSeedRecord(t *testing.T) {
	f := newFake()
	older := t0.Add(-24 * time.Hour)

	plain := f.SeedRecord(zone, cname("a.example.com", "x"))
	named := f.SeedRecord(zone, cfapi.Record{ID: "mine", Type: "A", Name: "b.example.com", Content: "192.0.2.1", ModifiedOn: older})

	require.Equal(t, "rec-1", plain.ID)
	require.Equal(t, t0, plain.ModifiedOn)
	require.Equal(t, "mine", named.ID)
	require.Equal(t, older, named.ModifiedOn)
	require.Equal(t, []cfapi.Record{plain, named}, f.RecordsIn(zone))

	// New ids step around the ones a test chose.
	f.SeedRecord(zone, cfapi.Record{ID: "rec-2", Type: "TXT", Name: "c.example.com", Content: "x"})
	made, err := f.CreateRecord(ctx, zone, rec("TXT", "d.example.com", "y"))
	require.NoError(t, err)
	require.Equal(t, "rec-3", made.ID)
}

func TestRecordConflicts(t *testing.T) {
	tests := []struct {
		name      string
		existing  string
		new       string
		sameName  bool
		apex      bool // the name is the zone itself
		proxied   bool // the CNAMEs of the pair are proxied
		wantClash bool
	}{
		// Cloudflare answers a proxied name with addresses of its own, and
		// keeps a TXT record beside a proxied CNAME, as a real account showed.
		{name: "txt then proxied cname", existing: "TXT", new: "CNAME", sameName: true, proxied: true},
		{name: "proxied cname then txt", existing: "CNAME", new: "TXT", sameName: true, proxied: true},
		{name: "mx then proxied cname", existing: "MX", new: "CNAME", sameName: true, proxied: true, wantClash: true},
		{name: "proxied cname then mx", existing: "CNAME", new: "MX", sameName: true, proxied: true, wantClash: true},
		{name: "a then proxied cname", existing: "A", new: "CNAME", sameName: true, proxied: true, wantClash: true},
		{name: "proxied cname then aaaa", existing: "CNAME", new: "AAAA", sameName: true, proxied: true, wantClash: true},
		{name: "proxied cname then proxied cname", existing: "CNAME", new: "CNAME", sameName: true, proxied: true, wantClash: true},
		{name: "cname then cname", existing: "CNAME", new: "CNAME", sameName: true, wantClash: true},
		{name: "cname then a", existing: "CNAME", new: "A", sameName: true, wantClash: true},
		{name: "cname then aaaa", existing: "CNAME", new: "AAAA", sameName: true, wantClash: true},
		{name: "a then cname", existing: "A", new: "CNAME", sameName: true, wantClash: true},
		{name: "aaaa then cname", existing: "AAAA", new: "CNAME", sameName: true, wantClash: true},
		{name: "cname then mx", existing: "CNAME", new: "MX", sameName: true, wantClash: true},
		{name: "mx then cname", existing: "MX", new: "CNAME", sameName: true, wantClash: true},
		{name: "a then a", existing: "A", new: "A", sameName: true},
		{name: "a then aaaa", existing: "A", new: "AAAA", sameName: true},
		{name: "aaaa then aaaa", existing: "AAAA", new: "AAAA", sameName: true},
		{name: "a then mx", existing: "A", new: "MX", sameName: true},
		{name: "txt then cname", existing: "TXT", new: "CNAME", sameName: true, wantClash: true},
		{name: "cname then txt", existing: "CNAME", new: "TXT", sameName: true, wantClash: true},
		{name: "txt then txt", existing: "TXT", new: "TXT", sameName: true},
		{name: "txt then a", existing: "TXT", new: "A", sameName: true},
		{name: "cname then cname of another name", existing: "CNAME", new: "CNAME"},
		{name: "txt then cname at the apex", existing: "TXT", new: "CNAME", sameName: true, apex: true},
		{name: "mx then cname at the apex", existing: "MX", new: "CNAME", sameName: true, apex: true},
		{name: "cname then txt at the apex", existing: "CNAME", new: "TXT", sameName: true, apex: true},
		{name: "a then cname at the apex", existing: "A", new: "CNAME", sameName: true, apex: true, wantClash: true},
		{name: "cname then cname at the apex", existing: "CNAME", new: "CNAME", sameName: true, apex: true, wantClash: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			existing := "app.example.com"
			if tt.apex {
				existing = "example.com"
			}
			old := rec(tt.existing, existing, "old")
			old.Proxied = tt.proxied && tt.existing == "CNAME"
			seeded := f.SeedRecord(zone, old)

			name := "other.example.com"
			if tt.sameName {
				name = existing
			}
			made := rec(tt.new, name, "new")
			made.Proxied = tt.proxied && tt.new == "CNAME"
			got, err := f.CreateRecord(ctx, zone, made)

			if tt.wantClash {
				require.True(t, cfapi.IsConflict(err), "got %v", err)
				var apiErr *cfapi.Error
				require.ErrorAs(t, err, &apiErr)
				require.Equal(t, cfapi.Error{
					Status: http.StatusBadRequest, Codes: []int{81053},
					Message: "An A, AAAA, or CNAME record with that host already exists.",
				}, *apiErr, "the same refusal whatever the other record")
				require.Equal(t, cfapi.Record{}, got)
				require.Equal(t, []cfapi.Record{seeded}, f.RecordsIn(zone), "a refused create leaves nothing behind")
				return
			}
			require.NoError(t, err)
			require.Equal(t, name, got.Name)
			require.Contains(t, f.RecordsIn(zone), got)
		})
	}
}

func TestRecordConflictsIgnoreCaseAndSpellingOfTheName(t *testing.T) {
	f := newFake()
	f.SeedRecord(zone, rec("CNAME", "app.example.com", "old"))

	for _, name := range []string{"APP.example.com", "app", "App.Example.Com."} {
		_, err := f.CreateRecord(ctx, zone, rec("A", name, "192.0.2.1"))
		require.True(t, cfapi.IsConflict(err), name)
	}
}

func TestRecordConflictsStayInTheirZone(t *testing.T) {
	f := newFake()
	f.AddZone("zone2", "example.org", acct)
	f.SeedRecord(zone, rec("CNAME", "app.example.com", "old"))

	got, err := f.CreateRecord(ctx, "zone2", rec("CNAME", "app", "new"))

	require.NoError(t, err)
	require.Equal(t, "app.example.org", got.Name)
}

func TestIdenticalRecordIsAConflict(t *testing.T) {
	tests := []struct {
		name      string
		existing  cfapi.Record
		new       cfapi.Record
		wantClash bool
	}{
		{"a twice", rec("A", "a.example.com", "192.0.2.1"), rec("A", "a.example.com", "192.0.2.1"), true},
		{"txt twice", rec("TXT", "a.example.com", "v"), rec("TXT", "a.example.com", "v"), true},
		{"cname twice", rec("CNAME", "a.example.com", "x.example.net"), rec("CNAME", "a.example.com", "x.example.net"), true},
		{"name in other case", rec("A", "a.example.com", "192.0.2.1"), rec("A", "A.Example.com", "192.0.2.1"), true},
		{"short name", rec("A", "a.example.com", "192.0.2.1"), rec("A", "a", "192.0.2.1"), true},
		{"other proxying and comment do not matter", rec("A", "a.example.com", "192.0.2.1"),
			cfapi.Record{Type: "A", Name: "a.example.com", Content: "192.0.2.1", Proxied: true, Comment: "mine"}, true},
		{"other content", rec("A", "a.example.com", "192.0.2.1"), rec("A", "a.example.com", "192.0.2.2"), false},
		{"other txt content", rec("TXT", "a.example.com", "v"), rec("TXT", "a.example.com", "w"), false},
		{"other type", rec("A", "a.example.com", "192.0.2.1"), rec("TXT", "a.example.com", "192.0.2.1"), false},
		{"other name", rec("A", "a.example.com", "192.0.2.1"), rec("A", "b.example.com", "192.0.2.1"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			seeded := f.SeedRecord(zone, tt.existing)

			got, err := f.CreateRecord(ctx, zone, tt.new)

			if !tt.wantClash {
				require.NoError(t, err)
				require.Len(t, f.RecordsIn(zone), 2)
				return
			}
			require.True(t, cfapi.IsConflict(err), "got %v", err)
			var apiErr *cfapi.Error
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, http.StatusBadRequest, apiErr.Status)
			require.Contains(t, apiErr.Codes, 81058)
			require.Equal(t, cfapi.Record{}, got)
			require.Equal(t, []cfapi.Record{seeded}, f.RecordsIn(zone))
		})
	}
}

func TestUpdateToAnIdenticalRecordIsAConflict(t *testing.T) {
	f := newFake()
	a := f.SeedRecord(zone, rec("A", "a.example.com", "192.0.2.1"))
	b := f.SeedRecord(zone, rec("A", "a.example.com", "192.0.2.2"))

	b.Content = "192.0.2.1"
	_, err := f.UpdateRecord(ctx, zone, b)
	require.True(t, cfapi.IsConflict(err))
	require.Equal(t, "192.0.2.2", f.RecordsIn(zone)[1].Content)

	// A record is not identical to itself.
	a.Comment = "changed"
	_, err = f.UpdateRecord(ctx, zone, a)
	require.NoError(t, err)
}

func TestRecordNamesAreLoweredAndQualified(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"qualified", "www.example.com", "www.example.com"},
		{"upper case", "WWW.Example.COM", "www.example.com"},
		{"short name", "www", "www.example.com"},
		{"short name in upper case", "WWW", "www.example.com"},
		{"several labels", "a.b", "a.b.example.com"},
		{"apex", "example.com", "example.com"},
		{"apex in upper case", "EXAMPLE.com", "example.com"},
		{"at sign", "@", "example.com"},
		{"trailing dot", "www.example.com.", "www.example.com"},
		{"look-alike zone", "notexample.com", "notexample.com.example.com"},
		{"zone inside the name", "www.example.com.example.com", "www.example.com.example.com"},
		{"wildcard", "*.example.com", "*.example.com"},
		{"probe", "_pco-probe-x", "_pco-probe-x.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			got, err := f.CreateRecord(ctx, zone, rec("TXT", tt.in, "v"))
			require.NoError(t, err)
			require.Equal(t, tt.want, got.Name)
			require.Equal(t, []cfapi.Record{got}, f.RecordsIn(zone))

			// Asking for the record by its name finds it, however the name is spelled.
			found, err := f.Records(ctx, zone, cfapi.RecordFilter{Name: tt.want})
			require.NoError(t, err)
			require.Equal(t, []cfapi.Record{got}, found)
		})
	}
}

func TestUpdatedRecordNamesAreLoweredAndQualified(t *testing.T) {
	f := newFake()
	r, err := f.CreateRecord(ctx, zone, rec("TXT", "a", "v"))
	require.NoError(t, err)

	r.Name = "B"
	got, err := f.UpdateRecord(ctx, zone, r)

	require.NoError(t, err)
	require.Equal(t, "b.example.com", got.Name)
	require.Equal(t, []cfapi.Record{got}, f.RecordsIn(zone))
}

func TestSeedRecordKeepsTheNameAsGiven(t *testing.T) {
	f := newFake()
	got := f.SeedRecord(zone, rec("A", "Odd.Example.com", "192.0.2.1"))
	require.Equal(t, "Odd.Example.com", got.Name)
}

func TestUpdateRecord(t *testing.T) {
	f := newFake()
	later := t0.Add(time.Hour)
	orig, err := f.CreateRecord(ctx, zone, cname("a.example.com", "old.cfargotunnel.com"))
	require.NoError(t, err)
	f.SetNow(func() time.Time { return later })

	updated := orig
	updated.Content = "new.cfargotunnel.com"
	updated.Comment = "pco:abc moved"
	updated.Proxied = false
	updated.ModifiedOn = time.Time{}
	got, err := f.UpdateRecord(ctx, zone, updated)

	require.NoError(t, err)
	want := cfapi.Record{
		ID: orig.ID, Type: "CNAME", Name: "a.example.com", Content: "new.cfargotunnel.com",
		Proxied: false, TTL: 1, Comment: "pco:abc moved", ModifiedOn: later,
	}
	require.Equal(t, want, got)
	require.Equal(t, []cfapi.Record{want}, f.RecordsIn(zone))
}

func TestUpdateRecordConflicts(t *testing.T) {
	f := newFake()
	cn := f.SeedRecord(zone, cname("a.example.com", "x"))
	addr := f.SeedRecord(zone, rec("A", "b.example.com", "192.0.2.1"))
	other := f.SeedRecord(zone, rec("A", "b.example.com", "192.0.2.2"))

	// Changing the content of a record does not clash with itself.
	cn.Content = "y"
	_, err := f.UpdateRecord(ctx, zone, cn)
	require.NoError(t, err)

	// Turning an A record into a CNAME where an A record of that name remains.
	addr.Type = "CNAME"
	_, err = f.UpdateRecord(ctx, zone, addr)
	require.True(t, cfapi.IsConflict(err))
	require.Contains(t, f.RecordsIn(zone), other)
	require.Equal(t, "A", f.RecordsIn(zone)[1].Type, "a refused update changes nothing")

	// Moving an A record onto the name of a CNAME.
	other.Name = "a.example.com"
	_, err = f.UpdateRecord(ctx, zone, other)
	require.True(t, cfapi.IsConflict(err))

	// A TXT record stands beside a CNAME only while that is proxied.
	f.SeedRecord(zone, rec("TXT", "a.example.com", "v"))
	cn.Proxied = false
	_, err = f.UpdateRecord(ctx, zone, cn)
	require.True(t, cfapi.IsConflict(err))
	txt := f.SeedRecord(zone, rec("TXT", "c.example.com", "w"))
	txt.Name = "a.example.com"
	_, err = f.UpdateRecord(ctx, zone, txt)
	require.NoError(t, err, "moved beside the proxied CNAME")
}

func TestRecordFilters(t *testing.T) {
	f := newFake()
	cn := f.SeedRecord(zone, cname("app.example.com", "x"))
	other := f.SeedRecord(zone, cfapi.Record{Type: "CNAME", Name: "www.example.com", Content: "y", Comment: "pco:other"})
	foreign := f.SeedRecord(zone, cfapi.Record{Type: "A", Name: "mail.example.com", Content: "192.0.2.1", Comment: "mine"})
	bare := f.SeedRecord(zone, cfapi.Record{Type: "A", Name: "app2.example.com", Content: "192.0.2.2"})
	txt := f.SeedRecord(zone, cfapi.Record{Type: "TXT", Name: "app.example.com", Content: "v", Comment: "pco:abc probe"})

	tests := []struct {
		name   string
		filter cfapi.RecordFilter
		want   []cfapi.Record
	}{
		{"no filter", cfapi.RecordFilter{}, []cfapi.Record{cn, other, foreign, bare, txt}},
		{"type", cfapi.RecordFilter{Type: "A"}, []cfapi.Record{foreign, bare}},
		{"name is exact", cfapi.RecordFilter{Name: "app.example.com"}, []cfapi.Record{cn, txt}},
		{"name ignores case", cfapi.RecordFilter{Name: "APP.example.com"}, []cfapi.Record{cn, txt}},
		{"comment prefix", cfapi.RecordFilter{CommentPrefix: "pco:abc"}, []cfapi.Record{cn, txt}},
		{"comment prefix is a prefix", cfapi.RecordFilter{CommentPrefix: "abc"}, nil},
		{"comment prefix ignores case", cfapi.RecordFilter{CommentPrefix: "PCO:"}, []cfapi.Record{cn, other, txt}},
		{"type and name", cfapi.RecordFilter{Type: "TXT", Name: "app.example.com"}, []cfapi.Record{txt}},
		{"all three", cfapi.RecordFilter{Type: "CNAME", Name: "app.example.com", CommentPrefix: "pco:abc"}, []cfapi.Record{cn}},
		{"nothing matches", cfapi.RecordFilter{Name: "none.example.com"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.Records(ctx, zone, tt.filter)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestDeleteRecord(t *testing.T) {
	f := newFake()
	a := f.SeedRecord(zone, cname("a.example.com", "x"))
	b := f.SeedRecord(zone, cname("b.example.com", "x"))

	require.NoError(t, f.DeleteRecord(ctx, zone, a.ID))

	require.Equal(t, []cfapi.Record{b}, f.RecordsIn(zone))
	require.True(t, cfapi.IsNotFound(f.DeleteRecord(ctx, zone, a.ID)))

	// The name is free again.
	_, err := f.CreateRecord(ctx, zone, rec("A", "a.example.com", "192.0.2.1"))
	require.NoError(t, err)
}

func TestUnknownIDsAreNotFound(t *testing.T) {
	f := newFake()
	tun := f.SeedTunnel(acct, "pco-abc", nil)
	r := f.SeedRecord(zone, cname("a.example.com", "x"))
	withID := func(id string) cfapi.Record { c := cname("a.example.com", "x"); c.ID = id; return c }
	catchAll := []planner.IngressRule{{Service: "http_status:404"}}

	tests := []struct {
		name string
		call func() error
	}{
		{"FindTunnel in unknown account", func() error { _, _, err := f.FindTunnel(ctx, "nope", "pco-abc"); return err }},
		{"Tunnels in unknown account", func() error { _, err := f.Tunnels(ctx, "nope", "pco-"); return err }},
		{"CreateTunnel in unknown account", func() error { _, err := f.CreateTunnel(ctx, "nope", "pco-new"); return err }},
		{"DeleteTunnel in unknown account", func() error { return f.DeleteTunnel(ctx, "nope", tun.ID) }},
		{"DeleteTunnel unknown", func() error { return f.DeleteTunnel(ctx, acct, "nope") }},
		{"TunnelToken in unknown account", func() error { _, err := f.TunnelToken(ctx, "nope", tun.ID); return err }},
		{"TunnelToken unknown", func() error { _, err := f.TunnelToken(ctx, acct, "nope"); return err }},
		{"TunnelConfig in unknown account", func() error { _, err := f.TunnelConfig(ctx, "nope", tun.ID); return err }},
		{"TunnelConfig unknown", func() error { _, err := f.TunnelConfig(ctx, acct, "nope"); return err }},
		{"PutTunnelConfig in unknown account", func() error { _, err := f.PutTunnelConfig(ctx, "nope", tun.ID, catchAll); return err }},
		{"PutTunnelConfig unknown", func() error { _, err := f.PutTunnelConfig(ctx, acct, "nope", catchAll); return err }},
		{"Connectors in unknown account", func() error { _, err := f.Connectors(ctx, "nope", tun.ID); return err }},
		{"Connectors unknown", func() error { _, err := f.Connectors(ctx, acct, "nope"); return err }},
		{"Records in unknown zone", func() error { _, err := f.Records(ctx, "nope", cfapi.RecordFilter{}); return err }},
		{"CreateRecord in unknown zone", func() error { _, err := f.CreateRecord(ctx, "nope", cname("a.example.com", "x")); return err }},
		{"UpdateRecord in unknown zone", func() error { _, err := f.UpdateRecord(ctx, "nope", withID(r.ID)); return err }},
		{"UpdateRecord unknown", func() error { _, err := f.UpdateRecord(ctx, zone, withID("nope")); return err }},
		{"DeleteRecord in unknown zone", func() error { return f.DeleteRecord(ctx, "nope", r.ID) }},
		{"DeleteRecord unknown", func() error { return f.DeleteRecord(ctx, zone, "nope") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.True(t, cfapi.IsNotFound(tt.call()))
		})
	}
	require.Equal(t, []cfapi.Record{r}, f.RecordsIn(zone), "failed calls changed nothing")
	require.Equal(t, []cfapi.Tunnel{tun}, f.TunnelsIn(acct))
}

func TestBadArgumentsAreRejectedLocally(t *testing.T) {
	// The same calls the client refuses before it sends anything: not a 404,
	// no call logged, and nothing used up of an injected failure.
	f := newFake()
	boom := errors.New("boom")
	f.FailNext("tunnel.read", 1, boom)
	f.FailNext("tunnel.write", 1, boom)
	f.FailNext("dns.read", 1, boom)
	f.FailNext("dns.write", 1, boom)
	catchAll := []planner.IngressRule{{Service: "http_status:404"}}
	withID := func(id string) cfapi.Record { r := cname("a.example.com", "x"); r.ID = id; return r }

	tests := []struct {
		name string
		call func() error
	}{
		{"FindTunnel without account", func() error { _, _, err := f.FindTunnel(ctx, "", "n"); return err }},
		{"FindTunnel without name", func() error { _, _, err := f.FindTunnel(ctx, acct, ""); return err }},
		{"FindTunnel blank name", func() error { _, _, err := f.FindTunnel(ctx, acct, "  "); return err }},
		{"FindTunnel account with a slash", func() error { _, _, err := f.FindTunnel(ctx, "a/b", "n"); return err }},
		{"Tunnels without account", func() error { _, err := f.Tunnels(ctx, "", "pco-"); return err }},
		{"Tunnels without prefix", func() error { _, err := f.Tunnels(ctx, acct, ""); return err }},
		{"CreateTunnel without account", func() error { _, err := f.CreateTunnel(ctx, "", "n"); return err }},
		{"CreateTunnel without name", func() error { _, err := f.CreateTunnel(ctx, acct, ""); return err }},
		{"DeleteTunnel without account", func() error { return f.DeleteTunnel(ctx, "", "t1") }},
		{"DeleteTunnel without tunnel", func() error { return f.DeleteTunnel(ctx, acct, "") }},
		{"DeleteTunnel dot dot", func() error { return f.DeleteTunnel(ctx, acct, "..") }},
		{"TunnelToken without account", func() error { _, err := f.TunnelToken(ctx, "", "t1"); return err }},
		{"TunnelToken without tunnel", func() error { _, err := f.TunnelToken(ctx, acct, ""); return err }},
		{"TunnelConfig without account", func() error { _, err := f.TunnelConfig(ctx, "", "t1"); return err }},
		{"TunnelConfig without tunnel", func() error { _, err := f.TunnelConfig(ctx, acct, ""); return err }},
		{"PutTunnelConfig without account", func() error { _, err := f.PutTunnelConfig(ctx, "", "t1", catchAll); return err }},
		{"PutTunnelConfig without tunnel", func() error { _, err := f.PutTunnelConfig(ctx, acct, "", catchAll); return err }},
		{"Connectors without account", func() error { _, err := f.Connectors(ctx, "", "t1"); return err }},
		{"Connectors without tunnel", func() error { _, err := f.Connectors(ctx, acct, ""); return err }},
		{"Records without zone", func() error { _, err := f.Records(ctx, "", cfapi.RecordFilter{}); return err }},
		{"CreateRecord without zone", func() error { _, err := f.CreateRecord(ctx, "", cname("a.example.com", "x")); return err }},
		{"CreateRecord without name", func() error { _, err := f.CreateRecord(ctx, zone, rec("A", "", "x")); return err }},
		{"CreateRecord without type", func() error { _, err := f.CreateRecord(ctx, zone, rec("", "a.example.com", "x")); return err }},
		{"UpdateRecord without zone", func() error { _, err := f.UpdateRecord(ctx, "", withID("r1")); return err }},
		{"UpdateRecord without id", func() error { _, err := f.UpdateRecord(ctx, zone, withID("")); return err }},
		{"UpdateRecord id with a slash", func() error { _, err := f.UpdateRecord(ctx, zone, withID("a/b")); return err }},
		{"UpdateRecord without name", func() error {
			r := withID("r1")
			r.Name = ""
			_, err := f.UpdateRecord(ctx, zone, r)
			return err
		}},
		{"DeleteRecord without zone", func() error { return f.DeleteRecord(ctx, "", "r1") }},
		{"DeleteRecord without id", func() error { return f.DeleteRecord(ctx, zone, "") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			require.ErrorIs(t, err, cfapi.ErrInvalidArgument)
			require.False(t, cfapi.IsNotFound(err))
		})
	}

	require.Empty(t, f.Calls())
	require.Empty(t, f.RecordsIn(zone))
	require.Empty(t, f.TunnelsIn(acct))

	// Each injected failure is still there for the first valid call.
	_, _, err := f.FindTunnel(ctx, acct, "n")
	require.Same(t, boom, err)
	_, err = f.CreateTunnel(ctx, acct, "n")
	require.Same(t, boom, err)
	_, err = f.Records(ctx, zone, cfapi.RecordFilter{})
	require.Same(t, boom, err)
	_, err = f.CreateRecord(ctx, zone, cname("a.example.com", "x"))
	require.Same(t, boom, err)
}

func TestPutTunnelConfigRefusesWhatCloudflareRefuses(t *testing.T) {
	host := func(h string) planner.IngressRule {
		return planner.IngressRule{Hostname: h, Service: "http://10.0.0.5:80"}
	}
	catchAll := planner.IngressRule{Service: "http_status:404"}

	refused := []struct {
		name  string
		rules []planner.IngressRule
	}{
		{"nil", nil},
		{"empty", []planner.IngressRule{}},
		{"last rule has a hostname", []planner.IngressRule{host("a.example.com")}},
		{"catch-all missing at the end", []planner.IngressRule{catchAll, host("a.example.com")}},
		{"catch-all first", []planner.IngressRule{catchAll, catchAll}},
		{"catch-all in the middle", []planner.IngressRule{host("a.example.com"), catchAll, host("b.example.com"), catchAll}},
		{"rule without service", []planner.IngressRule{{Hostname: "a.example.com"}, catchAll}},
		{"catch-all without service", []planner.IngressRule{host("a.example.com"), {}}},
		{"only a rule without service", []planner.IngressRule{{}}},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			good := []planner.IngressRule{host("keep.example.com"), catchAll}
			tun := f.SeedTunnel(acct, "pco-abc", good)

			version, err := f.PutTunnelConfig(ctx, acct, tun.ID, tt.rules)

			var apiErr *cfapi.Error
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, http.StatusBadRequest, apiErr.Status)
			require.False(t, cfapi.IsAuth(err) || cfapi.IsNotFound(err) || cfapi.IsConflict(err))
			require.Zero(t, version)
			cfg, err := f.TunnelConfig(ctx, acct, tun.ID)
			require.NoError(t, err)
			require.Equal(t, cfapi.TunnelConfig{Version: 1, Ingress: good}, cfg, "a refused write changes nothing")
			require.Contains(t, f.Calls(), "PutTunnelConfig acct1 "+tun.ID)
		})
	}

	accepted := []struct {
		name  string
		rules []planner.IngressRule
	}{
		{"catch-all only", []planner.IngressRule{catchAll}},
		{"hosts then catch-all", []planner.IngressRule{host("a.example.com"), host("*.example.com"), catchAll}},
		{"with options", []planner.IngressRule{{Hostname: "a.example.com", Service: "https://10.0.0.5:443", NoTLSVerify: true}, catchAll}},
	}
	for _, tt := range accepted {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			version, err := f.PutTunnelConfig(ctx, acct, tun.ID, tt.rules)
			require.NoError(t, err)
			require.Equal(t, 1, version)
		})
	}
}

func TestSeedTunnelDoesNotCheckTheIngress(t *testing.T) {
	// A configuration somebody edited by hand can lack a catch-all.
	f := newFake()
	rules := []planner.IngressRule{{Hostname: "a.example.com", Service: "http://10.0.0.5:80"}}
	tun := f.SeedTunnel(acct, "pco-abc", rules)

	cfg, err := f.TunnelConfig(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.Equal(t, rules, cfg.Ingress)
}

func TestSetForeign(t *testing.T) {
	f := newFake()
	rules := []planner.IngressRule{{Hostname: "a.example.com", Service: "http://10.0.0.5:80"}, {Service: "http_status:404"}}
	tun := f.SeedTunnel(acct, "pco-abc", rules)
	other := f.SeedTunnel(acct, "pco-other", rules)
	read := func(id string) cfapi.TunnelConfig {
		cfg, err := f.TunnelConfig(ctx, acct, id)
		require.NoError(t, err)
		return cfg
	}
	require.False(t, read(tun.ID).Foreign, "no foreign settings to begin with")

	f.SetForeign(acct, tun.ID, true)
	require.Equal(t, cfapi.TunnelConfig{Version: 1, Ingress: rules, Foreign: true}, read(tun.ID))
	require.False(t, read(other.ID).Foreign, "only the tunnel that was named")

	// A write that is refused leaves it in place; one that succeeds replaces
	// the configuration, foreign settings included.
	_, err := f.PutTunnelConfig(ctx, acct, tun.ID, nil)
	require.Error(t, err)
	require.True(t, read(tun.ID).Foreign)
	_, err = f.PutTunnelConfig(ctx, acct, tun.ID, rules)
	require.NoError(t, err)
	require.Equal(t, cfapi.TunnelConfig{Version: 2, Ingress: rules}, read(tun.ID))

	f.SetForeign(acct, tun.ID, true)
	f.SetForeign(acct, tun.ID, false)
	require.False(t, read(tun.ID).Foreign)
}

func TestSetForeignOnNothingChangesNothing(t *testing.T) {
	f := newFake()
	tun := f.SeedTunnel(acct, "pco-abc", nil)
	f.SetForeign("nope", tun.ID, true)
	f.SetForeign(acct, "nope", true)
	cfg, err := f.TunnelConfig(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.False(t, cfg.Foreign)
}

func TestSetTokenStatus(t *testing.T) {
	f := New()
	st, err := f.VerifyToken(ctx)
	require.NoError(t, err)
	require.Equal(t, "active", st.Status)
	require.Nil(t, st.ExpiresOn)

	expires := t0.Add(48 * time.Hour)
	f.SetTokenStatus("expired", &expires)
	st, err = f.VerifyToken(ctx)
	require.NoError(t, err)
	require.Equal(t, "expired", st.Status)
	require.Equal(t, &expires, st.ExpiresOn)
	require.NotSame(t, &expires, st.ExpiresOn, "the fake keeps a copy of the time")

	// Nor does a change to what came back reach the fake.
	*st.ExpiresOn = t0
	again, err := f.VerifyToken(ctx)
	require.NoError(t, err)
	require.Equal(t, expires, *again.ExpiresOn)

	expires = t0 // the caller's own value, after the call
	again, err = f.VerifyToken(ctx)
	require.NoError(t, err)
	require.Equal(t, t0.Add(48*time.Hour), *again.ExpiresOn)

	f.SetTokenStatus("disabled", nil)
	st, err = f.VerifyToken(ctx)
	require.NoError(t, err)
	require.Equal(t, cfapi.TokenStatus{ID: st.ID, Status: "disabled"}, st)
}

func TestAccountOwnedToken(t *testing.T) {
	f := newFake()
	f.SetTokenStatus("expired", nil)

	f.SetTokenOwner(acct)
	st, err := f.VerifyToken(ctx)
	require.NoError(t, err, "found at the account form of its account")
	require.Equal(t, "expired", st.Status, "with the status the token has")

	f.SetTokenOwner("acct9")
	st, err = f.VerifyToken(ctx)
	require.True(t, cfapi.IsAuth(err), "its account is not one the token sees: %v", err)
	require.Equal(t, cfapi.TokenStatus{}, st)
	f.AddAccount("acct9", "Ninth")
	_, err = f.VerifyToken(ctx)
	require.NoError(t, err)

	f.SetTokenOwner("")
	_, err = f.VerifyToken(ctx)
	require.NoError(t, err, "a user's token again")
	require.Equal(t, []string{"VerifyToken", "VerifyToken", "VerifyToken", "VerifyToken"}, f.Calls())
}

func TestSetTokenStatusDoesNotOverrideDeny(t *testing.T) {
	f := New()
	f.SetTokenStatus("active", nil)
	f.Deny("verify")
	_, err := f.VerifyToken(ctx)
	require.True(t, cfapi.IsAuth(err))
}

func TestSetConnectors(t *testing.T) {
	f := newFake()
	tun := f.SeedTunnel(acct, "pco-abc", nil)
	other := f.SeedTunnel(acct, "pco-other", nil)
	set := []cfapi.Connector{
		{ID: "c1", Version: "2026.9.0", ConfigVersion: 3, Connections: 4},
		{ID: "c2", Version: "2026.8.1", ConfigVersion: 2, Connections: 1},
	}

	f.SetConnectors(acct, tun.ID, set)

	got, err := f.Connectors(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.Equal(t, set, got)
	none, err := f.Connectors(ctx, acct, other.ID)
	require.NoError(t, err)
	require.Empty(t, none, "only the tunnel that was named")

	// Copies in and out.
	set[0].Connections = 99
	got[1].Version = "changed"
	again, err := f.Connectors(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.Equal(t, 4, again[0].Connections)
	require.Equal(t, "2026.8.1", again[1].Version)

	// Setting again replaces; nothing clears.
	f.SetConnectors(acct, tun.ID, []cfapi.Connector{{ID: "c3"}})
	again, err = f.Connectors(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.Equal(t, []cfapi.Connector{{ID: "c3"}}, again)
	f.SetConnectors(acct, tun.ID, nil)
	again, err = f.Connectors(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.Empty(t, again)
}

func TestConnectorsMoveTheStatus(t *testing.T) {
	f := newFake()
	tun := f.SeedTunnel(acct, "pco-abc", nil)
	status := func() string {
		got, found, err := f.FindTunnel(ctx, acct, "pco-abc")
		require.NoError(t, err)
		require.True(t, found)
		listed, err := f.Tunnels(ctx, acct, "pco-")
		require.NoError(t, err)
		require.Equal(t, []cfapi.Tunnel{got}, listed)
		require.Equal(t, []cfapi.Tunnel{got}, f.TunnelsIn(acct))
		return got.Status
	}

	require.Equal(t, "inactive", status())
	f.SetConnectors(acct, tun.ID, []cfapi.Connector{{ID: "c1", Connections: 4}})
	require.Equal(t, "healthy", status())
	f.SetConnectors(acct, tun.ID, nil)
	require.Equal(t, "inactive", status())
}

func TestTunnelWithConnectorsIsNotDeleted(t *testing.T) {
	f := newFake()
	tun := f.SeedTunnel(acct, "pco-abc", nil)
	f.SetConnectors(acct, tun.ID, []cfapi.Connector{{ID: "c1", Connections: 1}})

	err := f.DeleteTunnel(ctx, acct, tun.ID)

	var apiErr *cfapi.Error
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusBadRequest, apiErr.Status)
	require.False(t, cfapi.IsAuth(err) || cfapi.IsNotFound(err) || cfapi.IsConflict(err))
	require.Len(t, f.TunnelsIn(acct), 1, "a refused delete leaves the tunnel")
	require.Equal(t, []string{"DeleteTunnel acct1 " + tun.ID}, f.Calls())

	f.SetConnectors(acct, tun.ID, nil)
	require.NoError(t, f.DeleteTunnel(ctx, acct, tun.ID))
	require.Empty(t, f.TunnelsIn(acct))
}

func TestTunnelsByPrefix(t *testing.T) {
	f := newFake()
	f.AddAccount("acct2", "Other")
	probe1 := f.SeedTunnel(acct, "pco-abc_probe_1", nil)
	f.SeedTunnel(acct, "pco-abc", nil)
	f.SeedTunnel(acct, "pco-abc-node1", nil)
	f.SeedTunnel(acct, "PCO-ABC_probe_2", nil)
	probe3 := f.SeedTunnel(acct, "pco-abc_probe_3", nil)
	f.SeedTunnel("acct2", "pco-abc_probe_4", nil)

	got, err := f.Tunnels(ctx, acct, "pco-abc_probe_")
	require.NoError(t, err)
	require.Equal(t, []cfapi.Tunnel{probe1, probe3}, got, "the prefix is matched exactly, in the account asked")

	none, err := f.Tunnels(ctx, acct, "pco-xyz")
	require.NoError(t, err)
	require.Nil(t, none)

	got[0].Name = "changed"
	again, err := f.Tunnels(ctx, acct, "pco-abc_probe_")
	require.NoError(t, err)
	require.Equal(t, "pco-abc_probe_1", again[0].Name, "a copy")
	require.Equal(t, []string{
		"Tunnels acct1 pco-abc_probe_", "Tunnels acct1 pco-xyz", "Tunnels acct1 pco-abc_probe_",
	}, f.Calls())
}

func TestSetConnectorsOnNothingChangesNothing(t *testing.T) {
	f := newFake()
	tun := f.SeedTunnel(acct, "pco-abc", nil)
	f.SetConnectors("nope", tun.ID, []cfapi.Connector{{ID: "c1"}})
	f.SetConnectors(acct, "nope", []cfapi.Connector{{ID: "c1"}})
	got, err := f.Connectors(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSeedsAreNotVisibleOutsideTheirZone(t *testing.T) {
	// A record seeded into a zone nobody added cannot be reached through the
	// API, which says so.
	f := New()
	f.SeedRecord("ghost", cname("a.example.com", "x"))
	_, err := f.Records(ctx, "ghost", cfapi.RecordFilter{})
	require.True(t, cfapi.IsNotFound(err))
	require.Len(t, f.RecordsIn("ghost"), 1)
}

// method is one call of the API with valid arguments, and the operation the
// fake files it under.
type method struct {
	name string
	op   string
	call func(f *Fake, fx fixture) error
	log  func(fx fixture) string
}

type fixture struct {
	tunnel string
	record string
}

func newFixture(t *testing.T) (*Fake, fixture) {
	t.Helper()
	f := newFake()
	tun := f.SeedTunnel(acct, "pco-seeded", nil)
	r := f.SeedRecord(zone, rec("TXT", "seeded.example.com", "x"))
	return f, fixture{tunnel: tun.ID, record: r.ID}
}

func methods() []method {
	return []method{
		{"VerifyToken", "verify",
			func(f *Fake, _ fixture) error { _, err := f.VerifyToken(ctx); return err },
			func(fixture) string { return "VerifyToken" }},
		{"Accounts", "accounts",
			func(f *Fake, _ fixture) error { _, err := f.Accounts(ctx); return err },
			func(fixture) string { return "Accounts" }},
		{"Zones", "zones",
			func(f *Fake, _ fixture) error { _, err := f.Zones(ctx); return err },
			func(fixture) string { return "Zones" }},
		{"FindTunnel", "tunnel.read",
			func(f *Fake, _ fixture) error { _, _, err := f.FindTunnel(ctx, acct, "pco-seeded"); return err },
			func(fixture) string { return "FindTunnel acct1 pco-seeded" }},
		{"Tunnels", "tunnel.read",
			func(f *Fake, _ fixture) error { _, err := f.Tunnels(ctx, acct, "pco-"); return err },
			func(fixture) string { return "Tunnels acct1 pco-" }},
		{"TunnelToken", "tunnel.read",
			func(f *Fake, fx fixture) error { _, err := f.TunnelToken(ctx, acct, fx.tunnel); return err },
			func(fx fixture) string { return "TunnelToken acct1 " + fx.tunnel }},
		{"TunnelConfig", "tunnel.read",
			func(f *Fake, fx fixture) error { _, err := f.TunnelConfig(ctx, acct, fx.tunnel); return err },
			func(fx fixture) string { return "TunnelConfig acct1 " + fx.tunnel }},
		{"Connectors", "tunnel.read",
			func(f *Fake, fx fixture) error { _, err := f.Connectors(ctx, acct, fx.tunnel); return err },
			func(fx fixture) string { return "Connectors acct1 " + fx.tunnel }},
		{"CreateTunnel", "tunnel.write",
			func(f *Fake, _ fixture) error { _, err := f.CreateTunnel(ctx, acct, "pco-new"); return err },
			func(fixture) string { return "CreateTunnel acct1 pco-new" }},
		{"PutTunnelConfig", "tunnel.write",
			func(f *Fake, fx fixture) error {
				_, err := f.PutTunnelConfig(ctx, acct, fx.tunnel, []planner.IngressRule{{Service: "http_status:404"}})
				return err
			},
			func(fx fixture) string { return "PutTunnelConfig acct1 " + fx.tunnel }},
		{"DeleteTunnel", "tunnel.write",
			func(f *Fake, fx fixture) error { return f.DeleteTunnel(ctx, acct, fx.tunnel) },
			func(fx fixture) string { return "DeleteTunnel acct1 " + fx.tunnel }},
		{"Records", "dns.read",
			func(f *Fake, _ fixture) error {
				_, err := f.Records(ctx, zone, cfapi.RecordFilter{Type: "TXT"})
				return err
			},
			func(fixture) string { return "Records zone1" }},
		{"CreateRecord", "dns.write",
			func(f *Fake, _ fixture) error {
				_, err := f.CreateRecord(ctx, zone, cname("new.example.com", "x"))
				return err
			},
			func(fixture) string { return "CreateRecord zone1 new.example.com" }},
		{"UpdateRecord", "dns.write",
			func(f *Fake, fx fixture) error {
				r := rec("TXT", "seeded.example.com", "y")
				r.ID = fx.record
				_, err := f.UpdateRecord(ctx, zone, r)
				return err
			},
			func(fx fixture) string { return "UpdateRecord zone1 " + fx.record }},
		{"DeleteRecord", "dns.write",
			func(f *Fake, fx fixture) error { return f.DeleteRecord(ctx, zone, fx.record) },
			func(fx fixture) string { return "DeleteRecord zone1 " + fx.record }},
	}
}

func TestEveryMethodSucceedsOnAGoodFixture(t *testing.T) {
	for _, m := range methods() {
		t.Run(m.name, func(t *testing.T) {
			f, fx := newFixture(t)
			require.NoError(t, m.call(f, fx))
		})
	}
}

func TestDenyFailsOnlyItsOperation(t *testing.T) {
	ops := []string{"verify", "accounts", "zones", "tunnel.read", "tunnel.write", "dns.read", "dns.write"}
	for _, denied := range ops {
		t.Run(denied, func(t *testing.T) {
			for _, m := range methods() {
				f, fx := newFixture(t)
				f.Deny(denied)

				err := m.call(f, fx)

				if m.op == denied {
					require.True(t, cfapi.IsAuth(err), "%s: %v", m.name, err)
				} else {
					require.NoError(t, err, m.name)
				}
			}
		})
	}
}

func TestDenyChangesNothingAndAllowLiftsIt(t *testing.T) {
	f, fx := newFixture(t)
	f.Deny("dns.write")
	f.Deny("tunnel.write")

	_, err := f.CreateRecord(ctx, zone, cname("new.example.com", "x"))
	require.True(t, cfapi.IsAuth(err))
	require.True(t, cfapi.IsAuth(f.DeleteRecord(ctx, zone, fx.record)))
	require.True(t, cfapi.IsAuth(f.DeleteTunnel(ctx, acct, fx.tunnel)))
	require.Len(t, f.RecordsIn(zone), 1)
	require.Len(t, f.TunnelsIn(acct), 1)

	f.Allow("dns.write")
	_, err = f.CreateRecord(ctx, zone, cname("new.example.com", "x"))
	require.NoError(t, err)
	require.True(t, cfapi.IsAuth(f.DeleteTunnel(ctx, acct, fx.tunnel)), "tunnel.write is still denied")

	f.Allow("tunnel.write")
	require.NoError(t, f.DeleteTunnel(ctx, acct, fx.tunnel))
}

func TestDenyOfAZoneOrAnAccountRefusesOnlyTheCallsAboutIt(t *testing.T) {
	f, fx := newFixture(t)
	f.AddAccount("acct2", "Other")
	f.AddZone("zone2", "example.org", "acct2")
	f.Deny("dns.read", "zone2")
	f.Deny("tunnel.write", "acct2")

	_, err := f.Records(ctx, "zone2", cfapi.RecordFilter{})
	require.True(t, cfapi.IsAuth(err), "%v", err)
	_, err = f.Records(ctx, zone, cfapi.RecordFilter{})
	require.NoError(t, err, "another zone is read")
	_, err = f.CreateRecord(ctx, "zone2", cname("app.example.org", "x"))
	require.NoError(t, err, "another operation in the zone is not refused")
	_, err = f.CreateTunnel(ctx, "acct2", "pco-new")
	require.True(t, cfapi.IsAuth(err), "%v", err)
	require.True(t, cfapi.IsAuth(f.DeleteTunnel(ctx, "acct2", fx.tunnel)), "a call about the account, whatever the tunnel")
	_, err = f.CreateTunnel(ctx, acct, "pco-new")
	require.NoError(t, err, "another account is written")

	f.Allow("dns.read")
	_, err = f.Records(ctx, "zone2", cfapi.RecordFilter{})
	require.True(t, cfapi.IsAuth(err), "lifting the operation leaves the deny of the zone")

	f.Allow("dns.read", "zone2")
	_, err = f.Records(ctx, "zone2", cfapi.RecordFilter{})
	require.NoError(t, err)
	_, err = f.CreateTunnel(ctx, "acct2", "pco-new")
	require.True(t, cfapi.IsAuth(err), "an account stays denied until it is allowed")
}

func TestDenyIsA403(t *testing.T) {
	f := New()
	f.Deny("verify")
	_, err := f.VerifyToken(ctx)
	var apiErr *cfapi.Error
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, 403, apiErr.Status)
}

func TestFailNext(t *testing.T) {
	boom := errors.New("boom")
	f, fx := newFixture(t)
	f.FailNext("dns.write", 2, boom)

	_, err := f.CreateRecord(ctx, zone, cname("a.example.com", "x"))
	require.Same(t, boom, err)
	require.Same(t, boom, f.DeleteRecord(ctx, zone, fx.record))
	require.Len(t, f.RecordsIn(zone), 1, "failed writes changed nothing")

	// The third call is back to normal, and other operations never failed.
	require.NoError(t, f.DeleteRecord(ctx, zone, fx.record))
	f.FailNext("dns.write", 1, boom)
	_, err = f.Records(ctx, zone, cfapi.RecordFilter{})
	require.NoError(t, err)
}

func TestFailNextTakesPrecedenceOverDeny(t *testing.T) {
	boom := errors.New("boom")
	f := newFake()
	f.Deny("zones")
	f.FailNext("zones", 1, boom)

	_, err := f.Zones(ctx)
	require.Same(t, boom, err)
	_, err = f.Zones(ctx)
	require.True(t, cfapi.IsAuth(err), "once the failures are used up the denial applies again")
}

func TestFailNextTakesPrecedenceOverNormalBehaviour(t *testing.T) {
	// The first call would have been a conflict, an unknown id, or a success;
	// the injected error wins in each case.
	boom := errors.New("boom")
	f := newFake()
	f.SeedTunnel(acct, "pco-abc", nil)
	f.FailNext("tunnel.write", 1, boom)
	f.FailNext("tunnel.read", 1, boom)

	_, err := f.CreateTunnel(ctx, acct, "pco-abc")
	require.Same(t, boom, err)
	_, err = f.CreateTunnel(ctx, acct, "pco-abc")
	require.True(t, cfapi.IsConflict(err))
	_, err = f.TunnelConfig(ctx, acct, "unknown")
	require.Same(t, boom, err)
	_, err = f.TunnelConfig(ctx, acct, "unknown")
	require.True(t, cfapi.IsNotFound(err))
}

func TestFailNextQueuesInOrder(t *testing.T) {
	first, second := errors.New("first"), errors.New("second")
	f := newFake()
	f.FailNext("zones", 1, first)
	f.FailNext("zones", 2, second)

	for _, want := range []error{first, second, second} {
		_, err := f.Zones(ctx)
		require.Same(t, want, err)
	}
	_, err := f.Zones(ctx)
	require.NoError(t, err)
}

func TestFailNextCountsEveryMethodOfItsOperation(t *testing.T) {
	boom := errors.New("boom")
	f, fx := newFixture(t)
	f.FailNext("tunnel.read", 3, boom)

	_, _, err := f.FindTunnel(ctx, acct, "pco-seeded")
	require.Same(t, boom, err)
	_, err = f.TunnelToken(ctx, acct, fx.tunnel)
	require.Same(t, boom, err)
	_, err = f.Connectors(ctx, acct, fx.tunnel)
	require.Same(t, boom, err)
	_, err = f.TunnelConfig(ctx, acct, fx.tunnel)
	require.NoError(t, err)
}

func TestFailNextWithoutAnErrorStillFails(t *testing.T) {
	f := newFake()
	f.FailNext("zones", 1, nil)
	zones, err := f.Zones(ctx)
	require.Error(t, err)
	require.Nil(t, zones)
}

func TestFailNextForNothing(t *testing.T) {
	f := newFake()
	f.FailNext("zones", 0, errors.New("boom"))
	f.FailNext("zones", -1, errors.New("boom"))
	_, err := f.Zones(ctx)
	require.NoError(t, err)
}

func TestCallsAreLoggedInOrder(t *testing.T) {
	f, fx := newFixture(t)
	var want []string
	for _, m := range methods() {
		if m.name == "DeleteTunnel" || m.name == "DeleteRecord" {
			continue // the later calls need what these remove
		}
		require.NoError(t, m.call(f, fx), m.name)
		want = append(want, m.log(fx))
	}
	for _, m := range methods() {
		if m.name == "DeleteTunnel" || m.name == "DeleteRecord" {
			require.NoError(t, m.call(f, fx), m.name)
			want = append(want, m.log(fx))
		}
	}

	require.Equal(t, want, f.Calls())
}

func TestCallsAlsoRecordRefusedCalls(t *testing.T) {
	f := newFake()
	f.Deny("zones")
	_, _ = f.Zones(ctx)
	_, _ = f.TunnelConfig(ctx, acct, "unknown")
	require.Equal(t, []string{"Zones", "TunnelConfig acct1 unknown"}, f.Calls())
}

func TestSeedingAndInspectingAreNotCalls(t *testing.T) {
	f := newFake()
	f.SeedTunnel(acct, "pco-abc", nil)
	f.SeedRecord(zone, cname("a.example.com", "x"))
	f.RecordsIn(zone)
	f.TunnelsIn(acct)
	require.Empty(t, f.Calls())
}

func TestCallsAreACopy(t *testing.T) {
	f := newFake()
	_, _ = f.Zones(ctx)
	calls := f.Calls()
	calls[0] = "changed"
	require.Equal(t, []string{"Zones"}, f.Calls())
}

func TestEndedContextIsNotACall(t *testing.T) {
	f, fx := newFixture(t)
	done, cancel := context.WithCancel(ctx)
	cancel()

	_, err := f.Records(done, zone, cfapi.RecordFilter{})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, f.DeleteRecord(done, zone, fx.record), context.Canceled)
	require.Empty(t, f.Calls())
	require.Len(t, f.RecordsIn(zone), 1)
}

func TestDefaultClockIsTheRealOne(t *testing.T) {
	f := New()
	f.AddAccount(acct, "Acme")
	before := time.Now()
	tun, err := f.CreateTunnel(ctx, acct, "pco-abc")
	require.NoError(t, err)
	require.False(t, tun.CreatedAt.Before(before))

	f.SetNow(func() time.Time { return t0 })
	f.SetNow(nil)
	tun, err = f.CreateTunnel(ctx, acct, "pco-def")
	require.NoError(t, err)
	require.False(t, tun.CreatedAt.Before(before), "a nil clock means the real one")
}

func TestSafeForConcurrentUse(t *testing.T) {
	f := newFake()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			name := fmt.Sprintf("host%d.example.com", i)
			r, err := f.CreateRecord(ctx, zone, cname(name, "x"))
			assert.NoError(t, err)
			_, err = f.Records(ctx, zone, cfapi.RecordFilter{Name: name})
			assert.NoError(t, err)
			tun, err := f.CreateTunnel(ctx, acct, fmt.Sprintf("pco-%d", i))
			assert.NoError(t, err)
			_, err = f.PutTunnelConfig(ctx, acct, tun.ID, []planner.IngressRule{{Service: "http_status:404"}})
			assert.NoError(t, err)
			assert.NoError(t, f.DeleteRecord(ctx, zone, r.ID))
			f.Deny("verify")
			f.Allow("verify")
			f.FailNext("accounts", 0, nil)
			_ = f.Calls()
			_ = f.RecordsIn(zone)
			_ = f.TunnelsIn(acct)
		})
	}
	wg.Wait()

	require.Empty(t, f.RecordsIn(zone))
	require.Len(t, f.TunnelsIn(acct), 8)
	require.Len(t, f.Calls(), 8*5)
}

func TestADeletedTunnelLeavesATombstone(t *testing.T) {
	f := newFake()
	old := f.SeedTunnel(acct, "pco-abc", nil)
	f.SeedTunnel(acct, "pco-other", nil)
	require.NoError(t, f.DeleteTunnel(ctx, acct, old.ID))

	got, found, err := f.FindTunnel(ctx, acct, "pco-abc")
	require.NoError(t, err)
	require.False(t, found, "FindTunnel means a tunnel that is not deleted")
	require.Equal(t, cfapi.Tunnel{}, got)
	listed, err := f.Tunnels(ctx, acct, "pco-")
	require.NoError(t, err)
	require.Len(t, listed, 1, "and so does Tunnels")
	require.Equal(t, "pco-other", listed[0].Name)
	require.Len(t, f.TunnelsIn(acct), 1)
	require.Equal(t, []cfapi.Tunnel{old}, f.DeletedTunnelsIn(acct))
	require.Empty(t, f.DeletedTunnelsIn("acct2"))

	// The id is no tunnel for any call, as before.
	require.True(t, cfapi.IsNotFound(f.DeleteTunnel(ctx, acct, old.ID)))
	_, err = f.TunnelToken(ctx, acct, old.ID)
	require.True(t, cfapi.IsNotFound(err))
	_, err = f.TunnelConfig(ctx, acct, old.ID)
	require.True(t, cfapi.IsNotFound(err))
	_, err = f.PutTunnelConfig(ctx, acct, old.ID, []planner.IngressRule{{Service: "http_status:404"}})
	require.True(t, cfapi.IsNotFound(err))
	_, err = f.Connectors(ctx, acct, old.ID)
	require.True(t, cfapi.IsNotFound(err))
	f.SetForeign(acct, old.ID, true)
	f.SetConnectors(acct, old.ID, []cfapi.Connector{{ID: "c1"}})

	// The name is free again, and the new tunnel is another one.
	again, err := f.CreateTunnel(ctx, acct, "pco-abc")
	require.NoError(t, err)
	require.NotEqual(t, old.ID, again.ID)
	got, found, err = f.FindTunnel(ctx, acct, "pco-abc")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, again, got)
	require.Equal(t, []cfapi.Tunnel{old}, f.DeletedTunnelsIn(acct), "the tombstone stays")

	// Seeding the name twice is still a way to make two live tunnels.
	f.SeedTunnel(acct, "pco-abc", nil)
	_, _, err = f.FindTunnel(ctx, acct, "pco-abc")
	require.Error(t, err)
}

func TestTheTombstoneKeepsTheTimeOfTheDeletion(t *testing.T) {
	f := newFake()
	old := f.SeedTunnel(acct, "pco-abc", nil)
	later := t0.Add(36 * time.Hour)
	f.SetNow(func() time.Time { return later })

	require.NoError(t, f.DeleteTunnel(ctx, acct, old.ID))

	all, total, err := f.tunnelListing(ctx, "Tunnels", acct, "pco-", tunnelFilter{prefix: "pco-"})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, all, 1)
	require.Equal(t, old, all[0].Tunnel)
	require.NotNil(t, all[0].DeletedAt)
	require.Equal(t, later, *all[0].DeletedAt)
	require.Equal(t, t0, all[0].CreatedAt)
}

func TestTunnelListingFilters(t *testing.T) {
	f := newFake()
	f.AddAccount("acct2", "Other")
	live := f.SeedTunnel(acct, "pco-live", nil)
	dead := f.SeedTunnel(acct, "pco-dead", nil)
	f.SeedTunnel(acct, "other", nil)
	f.SeedTunnel("acct2", "pco-elsewhere", nil)
	require.NoError(t, f.DeleteTunnel(ctx, acct, dead.ID))

	names := func(flt tunnelFilter) ([]string, int) {
		t.Helper()
		got, total, err := f.tunnelListing(ctx, "Tunnels", acct, "", flt)
		require.NoError(t, err)
		var out []string
		for _, tun := range got {
			out = append(out, tun.Name)
		}
		return out, total
	}
	for _, tc := range []struct {
		name  string
		flt   tunnelFilter
		names []string
	}{
		{"everything", tunnelFilter{}, []string{"pco-live", "pco-dead", "other"}},
		{"live ones", tunnelFilter{deleted: liveOnly}, []string{"pco-live", "other"}},
		{"deleted ones", tunnelFilter{deleted: deletedOnly}, []string{"pco-dead"}},
		{"a prefix", tunnelFilter{prefix: "pco-"}, []string{"pco-live", "pco-dead"}},
		{"a prefix of live ones", tunnelFilter{prefix: "pco-", deleted: liveOnly}, []string{"pco-live"}},
		{"a name", tunnelFilter{name: "pco-dead"}, []string{"pco-dead"}},
		{"a name that is a prefix only", tunnelFilter{name: "pco"}, nil},
		{"a name of a deleted one among live ones", tunnelFilter{name: "pco-dead", deleted: liveOnly}, nil},
		{"a name and a prefix that disagree", tunnelFilter{name: "other", prefix: "pco-"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, total := names(tc.flt)
			require.Equal(t, tc.names, got)
			require.Equal(t, 3, total, "the total is the account's, whatever the filter")
		})
	}
	require.Equal(t, []cfapi.Tunnel{live}, mustTunnels(t, f, acct, "pco-l"))
}

func mustTunnels(t *testing.T, f *Fake, account, prefix string) []cfapi.Tunnel {
	t.Helper()
	got, err := f.Tunnels(ctx, account, prefix)
	require.NoError(t, err)
	return got
}

func TestTheRunTokenIsWhatCloudflaredReads(t *testing.T) {
	f := newFake()
	tun := f.SeedTunnel(acct, "pco-abc", nil)

	token, err := f.TunnelToken(ctx, acct, tun.ID)
	require.NoError(t, err)

	require.Equal(t, RunToken(acct, tun.ID), token)
	raw, err := base64.StdEncoding.DecodeString(token)
	require.NoError(t, err)
	var parsed struct {
		Account string `json:"a"`
		Tunnel  string `json:"t"`
		Secret  []byte `json:"s"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))
	require.Equal(t, acct, parsed.Account)
	require.Equal(t, tun.ID, parsed.Tunnel)
	require.Len(t, parsed.Secret, 32)
	require.Contains(t, string(parsed.Secret), "not-a-secret", "plainly not one")
	var keys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &keys))
	require.Len(t, keys, 3, "a, t and s, and nothing else")

	other := RunToken("acct2", tun.ID)
	require.NotEqual(t, token, other, "it names the account")
}

func TestRecordTTLsAreOneOrWithinCloudflaresRange(t *testing.T) {
	f := newFake()
	r := rec("A", "a.example.com", "192.0.2.1")
	for _, ttl := range []int{0, -5, 1, 30, 31, 3600, 86400} {
		r.TTL = ttl
		r.Content = fmt.Sprintf("192.0.2.%d", ttl&0xff)
		created, err := f.CreateRecord(ctx, zone, r)
		require.NoError(t, err, "ttl %d", ttl)
		require.NoError(t, f.DeleteRecord(ctx, zone, created.ID))
	}
	for _, ttl := range []int{2, 29, 86401, 1 << 30} {
		r.TTL = ttl
		_, err := f.CreateRecord(ctx, zone, r)
		var apiErr *cfapi.Error
		require.ErrorAs(t, err, &apiErr, "ttl %d", ttl)
		require.Equal(t, http.StatusBadRequest, apiErr.Status)
		require.Contains(t, apiErr.Message, "ttl")
		require.False(t, cfapi.IsConflict(err))
	}
	require.Empty(t, f.RecordsIn(zone), "a refused write changes nothing")

	created, err := f.CreateRecord(ctx, zone, withTTL(r, 300))
	require.NoError(t, err)
	_, err = f.UpdateRecord(ctx, zone, withTTL(created, 29))
	require.Error(t, err)
	got, err := f.UpdateRecord(ctx, zone, withTTL(created, 86400))
	require.NoError(t, err)
	require.Equal(t, 86400, got.TTL)
	require.Equal(t, 86400, f.RecordsIn(zone)[0].TTL)

	proxied := cname("p.example.com", "x.example.com")
	proxied.TTL = 7
	_, err = f.CreateRecord(ctx, zone, proxied)
	require.Error(t, err, "the TTL that is sent is held to it whether or not the record is proxied")
}

func withTTL(r cfapi.Record, ttl int) cfapi.Record {
	r.TTL = ttl
	return r
}
