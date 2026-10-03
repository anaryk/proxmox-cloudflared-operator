package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// fakeEnv answers from its fields and remembers what it was asked.
type fakeEnv struct {
	version    string
	versionErr error
	inactive   map[string]bool // units that are not active
	unitErr    error
	enabled    map[string]bool // units that start at boot
	enabledErr error
	dialErr    error
	dialed     []string
	pve        string
	pveErr     error
	storeErr   error
	lockErr    error
	interval   time.Duration
}

func healthyEnv() *fakeEnv {
	return &fakeEnv{version: "cloudflared version 2026.9.0 (built 2026-09-10-1200 UTC)", pve: "9.0", interval: 10 * time.Second}
}

func (f *fakeEnv) CloudflaredVersion(context.Context) (string, error) { return f.version, f.versionErr }

func (f *fakeEnv) UnitActive(_ context.Context, unit string) (bool, error) {
	return !f.inactive[unit], f.unitErr
}

func (f *fakeEnv) UnitEnabled(_ context.Context, unit string) (bool, error) {
	return f.enabled[unit], f.enabledErr
}

func (f *fakeEnv) CanDial(_ context.Context, network, addr string) error {
	f.dialed = append(f.dialed, network+" "+addr)
	return f.dialErr
}

func (f *fakeEnv) PVEVersion(context.Context) (string, error) { return f.pve, f.pveErr }
func (f *fakeEnv) Store(context.Context) error                { return f.storeErr }
func (f *fakeEnv) NodeLock(context.Context) error             { return f.lockErr }
func (f *fakeEnv) PollInterval() time.Duration                { return f.interval }
func (f *fakeEnv) Now() time.Time                             { return now }

// holdOver makes st the state of a cycle that held: its tunnels and
// connectors are those an earlier cycle found.
func holdOver(st *engine.State) {
	const why = "no writer identity; run pco setup"
	st.Hold = why
	st.Problems = []string{why}
	for i := range st.Tunnels {
		t := &st.Tunnels[i]
		t.Held, t.Unchecked, t.Verified = "not checked in the last cycle: "+why, true, false
	}
}

func healthyState() engine.State {
	expires := now.Add(90 * 24 * time.Hour)
	return engine.State{
		At: now.Add(-6 * time.Second), FinishedAt: now.Add(-5 * time.Second), Mode: "enforce", Complete: true, WriterVerdict: "ok",
		Tunnels: []engine.TunnelView{{TunnelState: reconcile.TunnelState{
			AccountID: "acc1", CredentialID: "cred1", Name: "pco-abc123", ID: tunnelID, Version: 3, Exists: true, Verified: true,
		}}},
		Connectors: []connector.Status{{TunnelID: tunnelID, Active: true, Ready: true, Connections: 4}},
		Credentials: []engine.CredentialView{{
			ID: "cred1", Label: "main", Kind: "scoped", Checked: true,
			Report: credentials.Report{Token: cfapi.TokenStatus{Status: "active", ExpiresOn: &expires}, Usable: true},
		}},
		Waiting:    []engine.Waiting{},
		Unapproved: []engine.UnapprovedGuest{},
		Egress:     engine.EgressView{State: engine.EgressOn},
	}
}

func leftOut(zone string) credentials.Exclusion {
	return credentials.Exclusion{Zone: zone, Reason: "no DNS read", Detail: "grant Zone > DNS > Edit on " + zone}
}

func TestAHealthyInstallation(t *testing.T) {
	env := healthyEnv()

	findings := Run(t.Context(), healthyState(), env)

	require.Equal(t, []Finding{
		{Check: "approval", Level: LevelOK, Detail: "no guest waits for approval"},
		{Check: "cloudflared", Level: LevelOK, Detail: "cloudflared 2026.9.0"},
		{Check: "conflicts", Level: LevelOK, Detail: "no record of someone else stands in the way"},
		{Check: "connector pco-abc123 in account acc1", Level: LevelOK, Detail: "active, ready, 4 connections"},
		{Check: "credential cred1", Level: LevelOK, Detail: "usable; the token expires 2026-12-30T12:00:00Z"},
		{Check: "cycle", Level: LevelOK, Detail: "the last cycle ran 5s ago"},
		{Check: "egress", Level: LevelOK, Detail: "the egress filter confines the connectors"},
		{Check: "inventory", Level: LevelOK, Detail: "every guest is listed"},
		{Check: "lost markers", Level: LevelOK, Detail: "no record of this install lost its marker"},
		{Check: "mode", Level: LevelOK, Detail: "enforce: changes are applied"},
		{Check: "nftables", Level: LevelOK, Detail: "nftables.service is not enabled"},
		{Check: "node lock", Level: LevelOK, Detail: "this daemon holds the lock of the node"},
		{Check: "outbound", Level: LevelOK, Detail: "region1.v2.argotunnel.com:7844 answers over TCP"},
		{Check: "problems", Level: LevelOK, Detail: "the last cycle found no problem"},
		{Check: "proxmox", Level: LevelOK, Detail: "Proxmox VE 9.0"},
		{Check: "rogue connectors", Level: LevelOK, Detail: "every connector Cloudflare lists on the tunnels is one pco runs on this node"},
		{Check: "store", Level: LevelOK, Detail: "the store is mounted and set up"},
		{Check: "tunnel pco-abc123 in account acc1", Level: LevelOK, Detail: "configuration version 3 is verified"},
		{Check: "waiting", Level: LevelOK, Detail: "nothing waits for a confirmation"},
		{Check: "writer", Level: LevelOK, Detail: "this daemon writes the tunnel configuration"},
	}, findings)
	require.Equal(t, []string{"tcp region1.v2.argotunnel.com:7844"}, env.dialed)
}

func TestWhatTheDoctorFinds(t *testing.T) {
	for _, tt := range []struct {
		name   string
		state  func(st *engine.State)
		env    func(env *fakeEnv)
		expect Finding
	}{
		{"observe-only mode", func(st *engine.State) { st.Mode = "observe" }, nil,
			Finding{Check: "mode", Level: LevelWarn, Detail: "observe-only: nothing is changed at Cloudflare", Fix: "pco apply"}},
		{"a cycle three intervals old", func(st *engine.State) { st.FinishedAt = now.Add(-30 * time.Second) }, nil,
			Finding{Check: "cycle", Level: LevelOK, Detail: "the last cycle ran 30s ago"}},
		{"a cycle older than three intervals", func(st *engine.State) { st.FinishedAt = now.Add(-31 * time.Second) }, nil,
			Finding{Check: "cycle", Level: LevelWarn, Detail: "the last cycle ran 31s ago, more than three poll intervals of 10s",
				Fix: "journalctl -u pco says what holds the cycles up"}},
		{"a cycle six intervals old", func(st *engine.State) { st.FinishedAt = now.Add(-60 * time.Second) }, nil,
			Finding{Check: "cycle", Level: LevelWarn, Detail: "the last cycle ran 1m0s ago, more than three poll intervals of 10s",
				Fix: "journalctl -u pco says what holds the cycles up"}},
		{"a cycle older than six intervals", func(st *engine.State) { st.FinishedAt = now.Add(-61 * time.Second) }, nil,
			Finding{Check: "cycle", Level: LevelFail, Detail: "the last cycle ran 1m1s ago, more than six poll intervals of 10s",
				Fix: "journalctl -u pco says what holds the cycles up"}},
		{"a daemon that holds", func(st *engine.State) {
			st.Problems = []string{
				"reading what the engine remembered: unexpected end of JSON input; nothing is changed at Cloudflare until it can be read",
				"the inventory is incomplete",
			}
		}, nil, Finding{Check: "problems", Level: LevelFail,
			Detail: "2 problems; the first: reading what the engine remembered: unexpected end of JSON input; " +
				"nothing is changed at Cloudflare until it can be read",
			Fix: "pco status"}},
		{"the egress filter switched off", func(st *engine.State) {
			st.Egress = engine.EgressView{State: engine.EgressOff, Since: now.Add(-2 * time.Hour)}
		}, nil, Finding{Check: "egress", Level: LevelFail,
			Detail: "the egress filter is switched off since 2026-10-01T10:00:00Z: the connectors are not confined", Fix: "pco egress on"}},
		{"the egress filter switched off at a time not known", func(st *engine.State) { st.Egress = engine.EgressView{State: engine.EgressOff} }, nil,
			Finding{Check: "egress", Level: LevelFail,
				Detail: "the egress filter is switched off since an unknown time: the connectors are not confined", Fix: "pco egress on"}},
		{"an egress table not loaded", func(st *engine.State) { st.Egress = engine.EgressView{State: engine.EgressNotLoaded} }, nil,
			Finding{Check: "egress", Level: LevelFail,
				Detail: "the egress table is not loaded: the connectors are not confined",
				Fix:    "pco status says why; pco egress show shows the table"}},
		{"an egress table changed", func(st *engine.State) { st.Egress = engine.EgressView{State: engine.EgressChanged} }, nil,
			Finding{Check: "egress", Level: LevelFail,
				Detail: "the egress table is not the one pco loads: the connectors may not be confined",
				Fix:    "pco status says why; pco egress show shows the table"}},
		{"an egress table not checked yet", func(st *engine.State) { st.Egress = engine.EgressView{} }, nil,
			Finding{Check: "egress", Level: LevelWarn, Detail: "the daemon has not checked the egress table yet", Fix: "wait half a minute"}},
		{"nftables.service enabled", nil, func(env *fakeEnv) { env.enabled = map[string]bool{"nftables.service": true} },
			Finding{Check: "nftables", Level: LevelWarn,
				Detail: "nftables.service is enabled: when it starts or restarts, the ruleset it loads flushes the egress table with the rest, " +
					"and the connectors are not confined until pco loads it again, within 30 seconds",
				Fix: "systemctl disable nftables.service, or keep the rules it loads from flushing the whole ruleset"}},
		{"systemd that does not say", nil, func(env *fakeEnv) { env.enabledErr = errors.New("Failed to connect to bus") },
			Finding{Check: "nftables", Level: LevelWarn, Detail: "systemd did not say whether nftables.service is enabled: Failed to connect to bus",
				Fix: "systemctl is-enabled nftables.service"}},
		{"one problem", func(st *engine.State) { st.Problems = []string{"saving the claims: disk full"} }, nil,
			Finding{Check: "problems", Level: LevelFail, Detail: "1 problem: saving the claims: disk full", Fix: "pco status"}},
		{"a long poll interval", func(st *engine.State) { st.FinishedAt = now.Add(-2 * time.Minute) }, func(env *fakeEnv) { env.interval = time.Minute },
			Finding{Check: "cycle", Level: LevelOK, Detail: "the last cycle ran 2m0s ago"}},
		{"a cycle that took long and ended a moment ago", func(st *engine.State) { st.At = now.Add(-90 * time.Second) }, nil,
			Finding{Check: "cycle", Level: LevelOK, Detail: "the last cycle ran 5s ago"}},
		{"an incomplete inventory", func(st *engine.State) { st.Complete = false }, nil,
			Finding{Check: "inventory", Level: LevelFail, Detail: "the inventory is incomplete: nothing is changed until it is complete",
				Fix: "pco status lists the problems that say why"}},
		{"no credential", func(st *engine.State) { st.Credentials = nil }, nil,
			Finding{Check: "credentials", Level: LevelFail, Detail: "no Cloudflare credential", Fix: "pco credential add --label <label>"}},
		{"a credential not checked", func(st *engine.State) {
			st.Credentials[0].Checked, st.Credentials[0].Report = false, credentials.Report{}
		}, nil, Finding{Check: "credential cred1", Level: LevelWarn, Detail: "not checked yet", Fix: "pco credential check cred1"}},
		{"a credential that cannot be used", func(st *engine.State) {
			st.Credentials[0].Report.Usable = false
			st.Credentials[0].Report.Checks = []credentials.Check{
				{Capability: credentials.CapToken, OK: true},
				{Capability: credentials.CapDNSWrite, Scope: "example.com", Detail: "grant Zone > DNS > Edit on example.com"},
			}
		}, nil, Finding{Check: "credential cred1", Level: LevelFail, Detail: "the token cannot be used: dns.write on example.com: grant Zone > DNS > Edit on example.com",
			Fix: "grant what is missing, then pco credential check cred1"}},
		{"a credential whose check got no answer", func(st *engine.State) {
			st.Credentials[0].Report.Usable = false
			st.Credentials[0].Report.Checks = []credentials.Check{
				{Capability: credentials.CapToken, OK: true},
				{Capability: credentials.CapZones, Detail: "cloudflare api: HTTP 503: unavailable", Unanswered: true},
			}
		}, nil, Finding{Check: "credential cred1", Level: LevelWarn,
			Detail: "the token could not be checked: zones: Cloudflare did not answer (cloudflare api: HTTP 503: unavailable)",
			Fix:    "once Cloudflare answers, run pco credential check cred1"}},
		{"a credential with a refusal beside a missing answer", func(st *engine.State) {
			st.Credentials[0].Report.Usable = false
			st.Credentials[0].Report.Checks = []credentials.Check{
				{Capability: credentials.CapDNSRead, Scope: "example.com", Detail: "grant Zone > DNS > Read on example.com"},
				{Capability: credentials.CapTunnelRead, Scope: "Main", Detail: "cloudflare api: HTTP 429: slow down", Unanswered: true},
			}
		}, nil, Finding{Check: "credential cred1", Level: LevelFail,
			Detail: "the token cannot be used: dns.read on example.com: grant Zone > DNS > Read on example.com; " +
				"tunnel.read on Main: Cloudflare did not answer (cloudflare api: HTTP 429: slow down)",
			Fix: "grant what is missing, then pco credential check cred1"}},
		{"an expired token", func(st *engine.State) {
			expired := now.Add(-time.Hour)
			st.Credentials[0].Report.Token.ExpiresOn = &expired
		}, nil, Finding{Check: "credential cred1", Level: LevelFail, Detail: "the token expired at 2026-10-01T11:00:00Z",
			Fix: "add a new token with pco credential add, then remove this one"}},
		{"a token that expires within 30 days", func(st *engine.State) {
			soon := now.Add(29 * 24 * time.Hour)
			st.Credentials[0].Report.Token.ExpiresOn = &soon
		}, nil, Finding{Check: "credential cred1", Level: LevelWarn, Detail: "the token expires at 2026-10-30T12:00:00Z, in 29 days",
			Fix: "add a new token with pco credential add, then remove this one"}},
		{"a token that expires in 30 days", func(st *engine.State) {
			later := now.Add(30 * 24 * time.Hour)
			st.Credentials[0].Report.Token.ExpiresOn = &later
		}, nil, Finding{Check: "credential cred1", Level: LevelOK, Detail: "usable; the token expires 2026-10-31T12:00:00Z"}},
		{"a token that does not expire", func(st *engine.State) { st.Credentials[0].Report.Token.ExpiresOn = nil }, nil,
			Finding{Check: "credential cred1", Level: LevelOK, Detail: "usable"}},
		{"a credential that leaves zones out", func(st *engine.State) {
			st.Credentials[0].Report.Excluded = []credentials.Exclusion{leftOut("example.net"), leftOut("example.org")}
		}, nil, Finding{Check: "credential cred1", Level: LevelOK,
			Detail: "usable; example.net, example.org left out: no DNS read; the token expires 2026-12-30T12:00:00Z"}},
		{"a credential that leaves zones out and does not expire", func(st *engine.State) {
			st.Credentials[0].Report.Token.ExpiresOn = nil
			st.Credentials[0].Report.Excluded = []credentials.Exclusion{leftOut("example.org")}
		}, nil, Finding{Check: "credential cred1", Level: LevelOK, Detail: "usable; example.org left out: no DNS read"}},
		{"a credential that leaves zones out and expires soon", func(st *engine.State) {
			soon := now.Add(29 * 24 * time.Hour)
			st.Credentials[0].Report.Token.ExpiresOn = &soon
			st.Credentials[0].Report.Excluded = []credentials.Exclusion{leftOut("example.org")}
		}, nil, Finding{Check: "credential cred1", Level: LevelWarn,
			Detail: "the token expires at 2026-10-30T12:00:00Z, in 29 days; example.org left out: no DNS read",
			Fix:    "add a new token with pco credential add, then remove this one"}},
		{"a credential that can read the DNS of no zone", func(st *engine.State) {
			st.Credentials[0].Report.Usable = false
			st.Credentials[0].Report.Checks = []credentials.Check{
				{Capability: credentials.CapToken, OK: true},
				{Capability: credentials.CapDNSRead, Detail: "token can read the DNS of no zone it lists; grant Zone > DNS > Edit on the zones to manage"},
			}
			st.Credentials[0].Report.Excluded = []credentials.Exclusion{leftOut("example.com")}
		}, nil, Finding{Check: "credential cred1", Level: LevelFail,
			Detail: "the token cannot be used: dns.read: token can read the DNS of no zone it lists; grant Zone > DNS > Edit on the zones to manage; " +
				"example.com left out: no DNS read",
			Fix: "grant what is missing, then pco credential check cred1"}},
		{"no cloudflared", nil, func(env *fakeEnv) { env.versionErr = errors.New(`exec: "/usr/bin/cloudflared": file does not exist`) },
			Finding{Check: "cloudflared", Level: LevelFail, Detail: `cloudflared does not run: exec: "/usr/bin/cloudflared": file does not exist`,
				Fix: "install cloudflared from the package repository of Cloudflare"}},
		{"a cloudflared that does not answer in time", nil, func(env *fakeEnv) {
			env.versionErr = fmt.Errorf("cloudflared --version: %w", context.DeadlineExceeded)
		}, Finding{Check: "cloudflared", Level: LevelWarn, Detail: "cloudflared --version did not answer in time",
			Fix: "run cloudflared --version by hand to see what holds it up"}},
		{"a version that cannot be read", nil, func(env *fakeEnv) { env.version = "cloudflared version DEV" },
			Finding{Check: "cloudflared", Level: LevelWarn, Detail: `cannot tell the version from "cloudflared version DEV"`, Fix: "update cloudflared"}},
		{"a cloudflared more than ten months old", nil, func(env *fakeEnv) { env.version = "cloudflared version 2025.11.2 (built 2025-11-30)" },
			Finding{Check: "cloudflared", Level: LevelWarn, Detail: "cloudflared 2025.11.2 is more than ten months old", Fix: "update cloudflared"}},
		{"a cloudflared less than ten months old", nil, func(env *fakeEnv) { env.version = "cloudflared version 2026.1.0" },
			Finding{Check: "cloudflared", Level: LevelOK, Detail: "cloudflared 2026.1.0"}},
		{"a connector unit that is not running", nil, func(env *fakeEnv) {
			env.inactive = map[string]bool{connector.UnitName(tunnelID): true}
		}, Finding{Check: "connector pco-abc123 in account acc1", Level: LevelFail, Detail: "pco-cloudflared@" + tunnelID + ".service is not running",
			Fix: "systemctl status pco-cloudflared@" + tunnelID + ".service"}},
		{"a connector that is not connected", func(st *engine.State) { st.Connectors[0].Ready = false }, nil,
			Finding{Check: "connector pco-abc123 in account acc1", Level: LevelFail, Detail: "not connected to Cloudflare",
				Fix: "journalctl -u pco-cloudflared@" + tunnelID + ".service"}},
		{"a connector the last cycle did not see", func(st *engine.State) { st.Connectors = nil }, nil,
			Finding{Check: "connector pco-abc123 in account acc1", Level: LevelFail, Detail: "the last cycle found no connector",
				Fix: "pco status lists the problems that say why"}},
		{"systemd that does not answer", nil, func(env *fakeEnv) { env.unitErr = errors.New("systemctl: timeout") },
			Finding{Check: "connector pco-abc123 in account acc1", Level: LevelWarn, Detail: "systemd did not say whether it runs: systemctl: timeout",
				Fix: "systemctl status pco-cloudflared@" + tunnelID + ".service"}},
		{"no way out over TCP while every connector is connected", nil, func(env *fakeEnv) { env.dialErr = errors.New("dial tcp: i/o timeout") },
			Finding{Check: "outbound", Level: LevelWarn,
				Detail: "region1.v2.argotunnel.com:7844 cannot be reached over TCP: dial tcp: i/o timeout; every connector is connected all the same, over QUIC perhaps",
				Fix:    "allow outbound TCP and UDP to port 7844"}},
		{"a stale writer", func(st *engine.State) { st.WriterVerdict = "stale" }, nil,
			Finding{Check: "writer", Level: LevelFail, Detail: "a newer generation of this install writes the tunnel configuration",
				Fix: "run pco setup --recover on the node that should write"}},
		{"a foreign writer", func(st *engine.State) { st.WriterVerdict = "foreign" }, nil,
			Finding{Check: "writer", Level: LevelFail, Detail: "another installation writes the tunnel configuration",
				Fix: "stop the other installation, or give this one an install of its own with pco setup"}},
		{"an unknown writer", func(st *engine.State) { st.WriterVerdict = "unknown" }, nil,
			Finding{Check: "writer", Level: LevelFail, Detail: "leader.json could not be used", Fix: "pco setup --recover"}},
		{"records of someone else", func(st *engine.State) {
			st.Conflicts = []reconcile.Conflict{
				{Zone: "example.com", Name: "www.example.com", Type: "A", Content: "192.0.2.10"},
				{Zone: "example.com", Name: "api.example.com", Type: "CNAME", Content: "x.example.net"},
			}
		}, nil, Finding{Check: "conflicts", Level: LevelWarn, Detail: "2 records of someone else stand in the way: www.example.com, api.example.com",
			Fix: "pco adopt <name>"}},
		{"records that lost their marker", func(st *engine.State) { st.Lost = []string{"old.example.com"} }, nil,
			Finding{Check: "lost markers", Level: LevelWarn, Detail: "1 record points at the tunnel but lost the marker of this install: old.example.com",
				Fix: "pco adopt <name>"}},
		{"an old Proxmox", nil, func(env *fakeEnv) { env.pve = "8.3" },
			Finding{Check: "proxmox", Level: LevelFail, Detail: "Proxmox VE 8.3 is not supported", Fix: "upgrade to Proxmox VE 8.4 or later"}},
		{"the oldest Proxmox supported", nil, func(env *fakeEnv) { env.pve = "8.4" },
			Finding{Check: "proxmox", Level: LevelOK, Detail: "Proxmox VE 8.4"}},
		{"a Proxmox that does not answer", nil, func(env *fakeEnv) { env.pveErr = errors.New("proxmox api: HTTP 401") },
			Finding{Check: "proxmox", Level: LevelFail, Detail: "the Proxmox API does not answer: proxmox api: HTTP 401",
				Fix: "check the Proxmox API token of pco"}},
		{"a Proxmox version that cannot be read", nil, func(env *fakeEnv) { env.pve = "nine" },
			Finding{Check: "proxmox", Level: LevelWarn, Detail: `cannot tell the version from "nine"`, Fix: "pveversion says which it is"}},
		{"a store that is not mounted", nil, func(env *fakeEnv) { env.storeErr = fmt.Errorf("%w: /etc/pve/.version", store.ErrNotMounted) },
			Finding{Check: "store", Level: LevelFail, Detail: "the cluster filesystem is not mounted: /etc/pve/.version",
				Fix: "systemctl status pve-cluster"}},
		{"a store that is not set up", nil, func(env *fakeEnv) { env.storeErr = errors.New("pco is not set up on this node; run pco setup") },
			Finding{Check: "store", Level: LevelFail, Detail: "pco is not set up on this node; run pco setup", Fix: "pco setup"}},
		{"a lock that is not held", nil, func(env *fakeEnv) { env.lockErr = errors.New("the lock file was replaced") },
			Finding{Check: "node lock", Level: LevelFail, Detail: "the lock file was replaced",
				Fix: "systemctl restart pco, so that no second daemon can start"}},
		{"a held tunnel", func(st *engine.State) {
			st.Tunnels[0].Held = "account frozen: zone example.com is no longer listed by credential cred1"
		}, nil,
			Finding{Check: "tunnel pco-abc123 in account acc1", Level: LevelWarn,
				Detail: "left as it is: account frozen: zone example.com is no longer listed by credential cred1",
				Fix:    "pco status lists the problems that say why"}},
		{"a configuration that is not verified", func(st *engine.State) { st.Tunnels[0].Verified = false }, nil,
			Finding{Check: "tunnel pco-abc123 in account acc1", Level: LevelWarn, Detail: "its configuration is not verified", Fix: "pco plan"}},
		{"a tunnel not created yet", func(st *engine.State) {
			st.Tunnels[0] = engine.TunnelView{TunnelState: reconcile.TunnelState{AccountID: "acc1", CredentialID: "cred1", Name: "pco-abc123"}}
		}, nil, Finding{Check: "tunnel pco-abc123 in account acc1", Level: LevelWarn, Detail: "it does not exist yet", Fix: "pco plan"}},
		{"what waits for a confirmation", func(st *engine.State) {
			st.Waiting = []engine.Waiting{
				{Kind: engine.WaitingZone, Subject: "example.net", Detail: "zone example.net is no longer listed by credential cred1"},
				{Kind: engine.WaitingRemovals, Detail: "mass delete guard: 6 of 6 records are being removed; confirm to proceed"},
			}
		}, nil, Finding{Check: "waiting", Level: LevelWarn,
			Detail: "2 things wait for a confirmation: zone example.net is no longer listed by credential cred1; " +
				"mass delete guard: 6 of 6 records are being removed; confirm to proceed",
			Fix: "pco apply --confirm-deletes"}},
		{"a guest that waits for approval", func(st *engine.State) {
			st.Unapproved = []engine.UnapprovedGuest{{GuestView: engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindLXC, VMID: 200}, Name: "db"}}}
		}, nil, Finding{Check: "approval lxc/200", Level: LevelWarn, Detail: "lxc/200 (db) waits for approval", Fix: "pco guest approve lxc/200"}},
		{"a connector that is not ours", func(st *engine.State) {
			st.RogueConnectors = []engine.RogueConnector{{
				Tunnel: "pco-abc123", TunnelID: tunnelID, Account: "acc1", ID: "attacker-elsewhere", OriginIP: "198.51.100.7", Version: "2026.8.0",
				Since: now.Add(-time.Minute),
			}}
		}, nil, Finding{Check: "rogue connectors", Level: LevelFail,
			Detail: "1 connector that pco does not run on this node serves its tunnels: " +
				"attacker-elsewhere from 198.51.100.7 (cloudflared 2026.8.0) on tunnel pco-abc123 in account acc1, since 2026-10-01T11:59:00Z",
			Fix: "unless you run it, rotate the tunnel secret: pco tunnel rotate --account acc1"}},
		{"connectors that are not ours in two accounts", func(st *engine.State) {
			st.RogueConnectors = []engine.RogueConnector{
				{Tunnel: "pco-abc123", TunnelID: tunnelID, Account: "acc1", ID: "r1", Since: now},
				{Tunnel: "pco-abc123", TunnelID: tunnelID, Account: "acc2", ID: "r2", OriginIP: "2001:db8::7", Version: "2026.9.1", Since: now},
			}
		}, nil, Finding{Check: "rogue connectors", Level: LevelFail,
			Detail: "2 connectors that pco does not run on this node serve its tunnels: " +
				"r1 from an unknown address (cloudflared of an unknown version) on tunnel pco-abc123 in account acc1, since 2026-10-01T12:00:00Z; " +
				"r2 from 2001:db8::7 (cloudflared 2026.9.1) on tunnel pco-abc123 in account acc2, since 2026-10-01T12:00:00Z",
			Fix: "unless you run them, rotate the tunnel secret of each: pco tunnel rotate --account acc1, pco tunnel rotate --account acc2"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, env := healthyState(), healthyEnv()
			if tt.state != nil {
				tt.state(&st)
			}
			if tt.env != nil {
				tt.env(env)
			}

			findings := Run(t.Context(), st, env)

			require.Contains(t, findings, tt.expect)
			require.Len(t, slices.DeleteFunc(slices.Clone(findings), func(f Finding) bool { return f.Check != tt.expect.Check }), 1,
				"one finding for the check")
			for _, f := range findings {
				if f.Check != tt.expect.Check && !strings.HasPrefix(f.Check, "approval") {
					require.Equal(t, LevelOK, f.Level, "%s: %s", f.Check, f.Detail)
				}
			}
		})
	}
}

// The way out over TCP is a failure only when a connector is not connected:
// connected ones may be over QUIC.
func TestTheWayOutIsAFailureWhenAConnectorIsNotConnected(t *testing.T) {
	for _, tt := range []struct {
		name  string
		state func(st *engine.State)
	}{
		{"a connector not ready", func(st *engine.State) { st.Connectors[0].Ready = false }},
		{"no connector", func(st *engine.State) { st.Connectors, st.Tunnels = nil, nil }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, env := healthyState(), healthyEnv()
			tt.state(&st)
			env.dialErr = errors.New("dial tcp: i/o timeout")

			findings := Run(t.Context(), st, env)

			require.Contains(t, findings, Finding{Check: "outbound", Level: LevelFail,
				Detail: "region1.v2.argotunnel.com:7844 cannot be reached over TCP: dial tcp: i/o timeout",
				Fix:    "allow outbound TCP and UDP to port 7844"})
		})
	}
}

// Before the first cycle the state says nothing yet: what is read from it is
// not known, and no failure.
func TestBeforeTheFirstCycleTheStateTellsNothing(t *testing.T) {
	findings := Run(t.Context(), engine.State{Mode: "observe", WriterVerdict: "ok"}, healthyEnv())

	byCheck := map[string]Finding{}
	for _, f := range findings {
		byCheck[f.Check] = f
		require.NotEqual(t, LevelFail, f.Level, "%s: %s", f.Check, f.Detail)
	}
	for _, check := range []string{"approval", "conflicts", "credentials", "inventory", "lost markers", "mode", "problems", "rogue connectors", "waiting", "writer"} {
		require.Equal(t, Finding{Check: check, Level: LevelWarn, Detail: "not known until the first cycle", Fix: "wait for the first cycle"},
			byCheck[check], check)
	}
	require.Equal(t, Finding{Check: "cycle", Level: LevelWarn, Detail: "no cycle has run yet",
		Fix: "wait for the first cycle; journalctl -u pco says why it does not come"}, byCheck["cycle"])
	for _, check := range []string{"cloudflared", "node lock", "outbound", "proxmox", "store"} {
		require.Equal(t, LevelOK, byCheck[check].Level, check)
	}
}

func TestFindingsAreSortedAndOnlyOkHasNoFix(t *testing.T) {
	st := healthyState()
	st.Mode, st.Complete, st.WriterVerdict = "observe", false, "foreign"
	st.Unapproved = []engine.UnapprovedGuest{
		{GuestView: engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 101}}},
		{GuestView: engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 20}}},
	}
	env := healthyEnv()
	env.dialErr = errors.New("refused")

	findings := Run(t.Context(), st, env)

	require.True(t, slices.IsSortedFunc(findings, compareChecks))
	require.Equal(t, "approval qemu/20", findings[0].Check, "owners in their natural order")
	require.Equal(t, "approval qemu/101", findings[1].Check)
	for _, f := range findings {
		require.Contains(t, []Level{LevelOK, LevelWarn, LevelFail}, f.Level)
		require.Equal(t, f.Level == LevelOK, f.Fix == "", "%s", f.Check)
	}
	require.True(t, Failed(findings))
	require.False(t, Failed(Run(t.Context(), healthyState(), healthyEnv())))
}

func TestTheRunnerAsksAboutTheStateOfNow(t *testing.T) {
	o := newOrigin(t, false, nil)
	st := servedBy(t, o.Server, "http")
	calls := 0
	r := NewRunner(func() engine.State { calls++; return st }, healthyEnv(), nil, (&testClock{t: now}).now, zerolog.Nop())

	steps, err := r.Diagnose(t.Context(), www)
	require.NoError(t, err)
	require.Len(t, steps, len(stepNames))
	require.NotEmpty(t, r.Doctor(t.Context()))

	require.Equal(t, 2, calls)
	_, err = r.Diagnose(t.Context(), "nope.example.com")
	require.ErrorIs(t, err, engine.ErrNotFound)
	_, err = r.Diagnose(t.Context(), "not a host")
	require.ErrorIs(t, err, engine.ErrInvalid)
}

// testClock is a time that moves when a test moves it.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// A diagnosis of a hostname runs once at a time, and its result serves for
// five seconds: whoever may use the socket cannot make the daemon hammer an
// origin.
func TestOneDiagnosisOfAHostnameAtATime(t *testing.T) {
	arrived, release := make(chan struct{}, 4), make(chan struct{})
	o := newOrigin(t, false, func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		<-release
	})
	st := servedBy(t, o.Server, "http")
	clock := &testClock{t: now}
	r := NewRunner(func() engine.State { return st }, healthyEnv(), nil, clock.now, zerolog.Nop())

	results := make(chan []Step, 2)
	go func() { steps, _ := r.Diagnose(t.Context(), www); results <- steps }()
	<-arrived
	go func() { steps, _ := r.Diagnose(t.Context(), "WWW.example.com"); results <- steps }()
	close(release)
	first, second := <-results, <-results

	require.Equal(t, first, second)
	require.Len(t, o.requests(), 1, "one request for both")
	clock.advance(4 * time.Second)
	_, err := r.Diagnose(t.Context(), www)
	require.NoError(t, err)
	require.Len(t, o.requests(), 1, "the result is kept for five seconds")
	clock.advance(time.Second)
	_, err = r.Diagnose(t.Context(), www)
	require.NoError(t, err)
	require.Len(t, o.requests(), 2, "and asked again after")
}

// blockingEnv is an Env whose cloudflared answers when the test lets it.
type blockingEnv struct {
	*fakeEnv
	mu      sync.Mutex
	runs    int
	arrived chan struct{}
	release chan struct{}
}

func (b *blockingEnv) CloudflaredVersion(ctx context.Context) (string, error) {
	b.mu.Lock()
	b.runs++
	b.mu.Unlock()
	b.arrived <- struct{}{}
	<-b.release
	return b.fakeEnv.CloudflaredVersion(ctx)
}

func (b *blockingEnv) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runs
}

func TestOneDoctorAtATime(t *testing.T) {
	env := &blockingEnv{fakeEnv: healthyEnv(), arrived: make(chan struct{}, 4), release: make(chan struct{})}
	clock := &testClock{t: now}
	r := NewRunner(healthyState, env, nil, clock.now, zerolog.Nop())

	results := make(chan []Finding, 2)
	go func() { results <- r.Doctor(t.Context()) }()
	<-env.arrived
	go func() { results <- r.Doctor(t.Context()) }()
	close(env.release)
	first, second := <-results, <-results

	require.Equal(t, first, second)
	require.Equal(t, 1, env.count(), "cloudflared was run once")
	clock.advance(4 * time.Second)
	r.Doctor(t.Context())
	require.Equal(t, 1, env.count())
	clock.advance(time.Second)
	r.Doctor(t.Context())
	require.Equal(t, 2, env.count())
}

// After a cycle that held, the connectors the state shows are those of an
// earlier cycle: only what systemd says now counts.
func TestTheDoctorReadsOnlyWhatTheLastCycleChecked(t *testing.T) {
	for _, tt := range []struct {
		name   string
		state  func(st *engine.State)
		env    func(env *fakeEnv)
		expect Finding
	}{
		{"the tunnel", nil, nil, Finding{Check: "tunnel pco-abc123 in account acc1", Level: LevelWarn,
			Detail: "not checked in the last cycle: no writer identity; run pco setup", Fix: "pco status lists the problems that say why"}},
		{"the tunnel, without a reason", func(st *engine.State) { st.Tunnels[0].Held = "" }, nil,
			Finding{Check: "tunnel pco-abc123 in account acc1", Level: LevelWarn,
				Detail: "not checked in the last cycle", Fix: "pco status lists the problems that say why"}},
		{"a connector that runs", nil, nil, Finding{Check: "connector pco-abc123 in account acc1", Level: LevelWarn,
			Detail: "pco-cloudflared@" + tunnelID + ".service is running; whether it is connected was not checked in the last cycle",
			Fix:    "pco status lists the problems that say why"}},
		{"a connector that does not run", nil, func(env *fakeEnv) { env.inactive = map[string]bool{connector.UnitName(tunnelID): true} },
			Finding{Check: "connector pco-abc123 in account acc1", Level: LevelFail, Detail: "pco-cloudflared@" + tunnelID + ".service is not running",
				Fix: "systemctl status pco-cloudflared@" + tunnelID + ".service"}},
		{"no way out over TCP", nil, func(env *fakeEnv) { env.dialErr = errors.New("dial tcp: i/o timeout") },
			Finding{Check: "outbound", Level: LevelFail,
				Detail: "region1.v2.argotunnel.com:7844 cannot be reached over TCP: dial tcp: i/o timeout",
				Fix:    "allow outbound TCP and UDP to port 7844"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, env := healthyState(), healthyEnv()
			holdOver(&st)
			if tt.state != nil {
				tt.state(&st)
			}
			if tt.env != nil {
				tt.env(env)
			}

			findings := Run(t.Context(), st, env)

			require.Contains(t, findings, tt.expect)
		})
	}
}
