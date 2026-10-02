package doctor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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

func (f *fakeEnv) CanDial(_ context.Context, network, addr string) error {
	f.dialed = append(f.dialed, network+" "+addr)
	return f.dialErr
}

func (f *fakeEnv) PVEVersion(context.Context) (string, error) { return f.pve, f.pveErr }
func (f *fakeEnv) Store(context.Context) error                { return f.storeErr }
func (f *fakeEnv) NodeLock(context.Context) error             { return f.lockErr }
func (f *fakeEnv) PollInterval() time.Duration                { return f.interval }
func (f *fakeEnv) Now() time.Time                             { return now }

func healthyState() engine.State {
	expires := now.Add(90 * 24 * time.Hour)
	return engine.State{
		At: now.Add(-5 * time.Second), Mode: "enforce", Complete: true, WriterVerdict: "ok",
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
	}
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
		{Check: "inventory", Level: LevelOK, Detail: "every guest is listed"},
		{Check: "lost markers", Level: LevelOK, Detail: "no record of this install lost its marker"},
		{Check: "mode", Level: LevelOK, Detail: "enforce: changes are applied"},
		{Check: "node lock", Level: LevelOK, Detail: "this daemon holds the lock of the node"},
		{Check: "outbound", Level: LevelOK, Detail: "region1.v2.argotunnel.com:7844 answers over TCP"},
		{Check: "proxmox", Level: LevelOK, Detail: "Proxmox VE 9.0"},
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
		{"no cycle yet", func(st *engine.State) { st.At = time.Time{} }, nil,
			Finding{Check: "cycle", Level: LevelWarn, Detail: "no cycle has run yet", Fix: "wait for the first cycle; journalctl -u pco says why it does not come"}},
		{"a cycle three intervals old", func(st *engine.State) { st.At = now.Add(-30 * time.Second) }, nil,
			Finding{Check: "cycle", Level: LevelOK, Detail: "the last cycle ran 30s ago"}},
		{"a cycle older than three intervals", func(st *engine.State) { st.At = now.Add(-31 * time.Second) }, nil,
			Finding{Check: "cycle", Level: LevelFail, Detail: "the last cycle ran 31s ago, more than three poll intervals of 10s",
				Fix: "journalctl -u pco says what holds the cycles up"}},
		{"a long poll interval", func(st *engine.State) { st.At = now.Add(-2 * time.Minute) }, func(env *fakeEnv) { env.interval = time.Minute },
			Finding{Check: "cycle", Level: LevelOK, Detail: "the last cycle ran 2m0s ago"}},
		{"an incomplete inventory", func(st *engine.State) { st.Complete = false }, nil,
			Finding{Check: "inventory", Level: LevelFail, Detail: "the inventory is incomplete: nothing is changed until it is complete",
				Fix: "pco status lists the problems that say why"}},
		{"no credential", func(st *engine.State) { st.Credentials = nil }, nil,
			Finding{Check: "credentials", Level: LevelFail, Detail: "no Cloudflare credential", Fix: "pco credential add --label <label>"}},
		{"a credential not checked", func(st *engine.State) {
			st.Credentials[0].Checked, st.Credentials[0].Report = false, credentials.Report{}
		}, nil, Finding{Check: "credential cred1", Level: LevelWarn, Detail: "not checked since the daemon started", Fix: "pco credential check cred1"}},
		{"a credential that cannot be used", func(st *engine.State) {
			st.Credentials[0].Report.Usable = false
			st.Credentials[0].Report.Checks = []credentials.Check{
				{Capability: credentials.CapToken, OK: true},
				{Capability: credentials.CapDNSWrite, Scope: "example.com", Detail: "grant Zone > DNS > Edit on example.com"},
			}
		}, nil, Finding{Check: "credential cred1", Level: LevelFail, Detail: "the token cannot be used: dns.write on example.com: grant Zone > DNS > Edit on example.com",
			Fix: "grant what is missing, then pco credential check cred1"}},
		{"an expired token", func(st *engine.State) {
			expired := now.Add(-time.Hour)
			st.Credentials[0].Report.Token.ExpiresOn = &expired
		}, nil, Finding{Check: "credential cred1", Level: LevelFail, Detail: "the token expired at 2026-10-01T11:00:00Z",
			Fix: "add a new token with pco credential add, then remove this one"}},
		{"a token that expires within 14 days", func(st *engine.State) {
			soon := now.Add(13 * 24 * time.Hour)
			st.Credentials[0].Report.Token.ExpiresOn = &soon
		}, nil, Finding{Check: "credential cred1", Level: LevelWarn, Detail: "the token expires at 2026-10-14T12:00:00Z, in 13 days",
			Fix: "add a new token with pco credential add, then remove this one"}},
		{"a token that expires in 14 days", func(st *engine.State) {
			later := now.Add(14 * 24 * time.Hour)
			st.Credentials[0].Report.Token.ExpiresOn = &later
		}, nil, Finding{Check: "credential cred1", Level: LevelOK, Detail: "usable; the token expires 2026-10-15T12:00:00Z"}},
		{"a token that does not expire", func(st *engine.State) { st.Credentials[0].Report.Token.ExpiresOn = nil }, nil,
			Finding{Check: "credential cred1", Level: LevelOK, Detail: "usable"}},
		{"no cloudflared", nil, func(env *fakeEnv) { env.versionErr = errors.New(`exec: "/usr/bin/cloudflared": file does not exist`) },
			Finding{Check: "cloudflared", Level: LevelFail, Detail: `cloudflared does not run: exec: "/usr/bin/cloudflared": file does not exist`,
				Fix: "install cloudflared from the package repository of Cloudflare"}},
		{"a version that cannot be read", nil, func(env *fakeEnv) { env.version = "cloudflared version DEV" },
			Finding{Check: "cloudflared", Level: LevelWarn, Detail: `cannot tell the version from "cloudflared version DEV"`, Fix: "update cloudflared"}},
		{"a cloudflared more than a year old", nil, func(env *fakeEnv) { env.version = "cloudflared version 2025.9.2 (built 2025-09-30)" },
			Finding{Check: "cloudflared", Level: LevelWarn, Detail: "cloudflared 2025.9.2 is more than a year old", Fix: "update cloudflared"}},
		{"a cloudflared less than a year old", nil, func(env *fakeEnv) { env.version = "cloudflared version 2025.11.0" },
			Finding{Check: "cloudflared", Level: LevelOK, Detail: "cloudflared 2025.11.0"}},
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
		{"no way out", nil, func(env *fakeEnv) { env.dialErr = errors.New("dial tcp: i/o timeout") },
			Finding{Check: "outbound", Level: LevelFail, Detail: "region1.v2.argotunnel.com:7844 cannot be reached over TCP: dial tcp: i/o timeout",
				Fix: "allow outbound TCP and UDP to port 7844"}},
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

func TestFindingsAreSortedAndOnlyOkHasNoFix(t *testing.T) {
	st := healthyState()
	st.Mode, st.Complete, st.WriterVerdict = "observe", false, "foreign"
	st.Unapproved = []engine.UnapprovedGuest{
		{GuestView: engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 102}}},
		{GuestView: engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 101}}},
	}
	env := healthyEnv()
	env.dialErr = errors.New("refused")

	findings := Run(t.Context(), st, env)

	require.True(t, slices.IsSortedFunc(findings, func(a, b Finding) int { return strings.Compare(a.Check, b.Check) }))
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
	r := Runner{State: func() engine.State { calls++; return st }, Env: healthyEnv()}

	steps, err := r.Diagnose(t.Context(), www)
	require.NoError(t, err)
	require.Len(t, steps, len(stepNames))
	require.NotEmpty(t, r.Doctor(t.Context()))

	require.Equal(t, 2, calls)
	_, err = r.Diagnose(t.Context(), "nope.example.com")
	require.ErrorIs(t, err, engine.ErrNotFound)
}
