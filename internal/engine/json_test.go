package engine

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

var update = flag.Bool("update", false, "write the golden files of the tests")

// populatedState holds every type the state can carry, each field set.
func populatedState() State {
	expires := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	waiting := populatedWaiting()
	return State{
		At:         t0,
		FinishedAt: t0.Add(2 * time.Second),
		Node:       "pve1",
		Digest:     "5e0c1f7a92b4d3e8",
		Mode:       "enforce",
		Complete:   true,
		Routes: []RouteView{
			{
				RouteStatus: planner.RouteStatus{
					Hostname: "www.example.com", Owner: "qemu/101", State: planner.StateActive, Level: "port",
					Service: "http://10.0.0.11:8080", Zone: "example.com", Warnings: []string{"a warning"},
				},
				Guest:      &GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Name: "web-1"},
				Candidates: []resolve.CandidateResult{{Addr: netip.MustParseAddr("10.0.0.11"), Source: resolve.FromStatic, OK: true, Level: "port"}},
				Account:    "acc1",
				Rule:       &planner.IngressRule{Hostname: "www.example.com", Service: "http://10.0.0.11:8080", HTTPHostHeader: "intranet"},
				Path: &PathView{
					Node: "pve1", Bridge: "vmbr0", VLAN: 20, Port: "tap101i0", MAC: "bc:24:11:5e:7a:01",
					VerifiedAt: t0, Since: t0.Add(-28 * time.Hour),
				},
			},
			{
				RouteStatus: planner.RouteStatus{
					Hostname: "www.example.com", Owner: "qemu/102", State: planner.StateConflict, Reason: "hostname is held by qemu/101",
				},
				Guest: &GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 102}, Name: "web-2"},
			},
		},
		Issues: []planner.Issue{
			{Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 103}, Line: 2, Col: 5, Msg: "a broken entry"},
			{Msg: "an issue of the settings"},
		},
		Tunnels: []TunnelView{
			{TunnelState: reconcile.TunnelState{AccountID: "acc1", CredentialID: "cred1", Name: "pco-abc123", ID: "00000000-0000-4000-8000-000000000001", Version: 3, Exists: true, Verified: true}},
			{TunnelState: reconcile.TunnelState{AccountID: "acc2", CredentialID: "cred1", Name: "pco-abc123", Unknown: true}},
			{
				TunnelState: reconcile.TunnelState{AccountID: "acc3", CredentialID: "cred1", Name: "pco-abc123", ID: "00000000-0000-4000-8000-000000000003", Exists: true},
				Held:        "account frozen: zone example.info is no longer listed by credential cred1",
			},
			{
				TunnelState: reconcile.TunnelState{AccountID: "acc4", CredentialID: "cred2", Name: "pco-abc123", ID: "00000000-0000-4000-8000-000000000004", Version: 2, Exists: true},
				Held:        "not checked in the last cycle: no writer identity; run pco setup",
				Unchecked:   true,
			},
		},
		Connectors: []connector.Status{{
			TunnelID: "00000000-0000-4000-8000-000000000001", Active: true, Ready: true, Connections: 4,
			ConnectorID: "6b1f0e4c-29a4-4c43-9d2c-0f3a8c1b7d11", MetricsAddr: "127.0.0.1:20300", Install: "abc123",
		}},
		Credentials: []CredentialView{
			{
				ID: "cred1", Label: "main", Kind: "scoped", Checked: true,
				Report: credentials.Report{
					Token:    cfapi.TokenStatus{ID: "token-1", Status: "active", ExpiresOn: &expires},
					Accounts: []cfapi.Account{{ID: "acc1", Name: "Main"}},
					Zones: []cfapi.Zone{
						{ID: "zone1", Name: "example.com", Status: "active", AccountID: "acc1"},
						{ID: "zone2", Name: "example.org", Status: "active", AccountID: "acc1"},
					},
					Checks: []credentials.Check{{Capability: credentials.CapToken, OK: true}, {Capability: credentials.CapDNSRead, Scope: "example.com", ScopeID: "zone1", Detail: "grant Zone > DNS > Read on example.com"}},
					Excluded: []credentials.Exclusion{
						{Zone: "example.org", ZoneID: "zone2", Reason: "no DNS read", Detail: "grant Zone > DNS > Edit on example.org"},
					},
					Deep:      true,
					Usable:    true,
					Leftovers: []string{"_pco-probe-x.example.com"},
					CheckedAt: t0,
				},
			},
			{ID: "cred2", Label: "spare", Kind: "scoped"},
		},
		Zones: populatedZones(),
		Actions: []reconcile.Action{
			{Kind: reconcile.PutConfig, Credential: "cred1", AccountID: "acc1", Target: "pco-abc123", Detail: "in account acc1: 3 rules replace version 2", Applied: true},
			{Kind: reconcile.CreateRecord, Credential: "cred1", Target: "www.example.com", Detail: "in zone example.com", Applied: true},
			{Kind: reconcile.DeleteRecord, Credential: "cred1", Target: "old.example.com", Detail: "in zone example.com", Destructive: true, Held: "grace period: 1m0s left"},
		},
		Conflicts:     []reconcile.Conflict{{Zone: "example.com", Name: "api.example.com", Type: "A", Content: "192.0.2.10"}},
		Lost:          []string{"lost.example.com"},
		Problems:      []string{"a problem"},
		WriterVerdict: "ok",
		Hold:          "no writer identity; run pco setup",
		Profile:       "host",
		Waiting:       waiting,
		Offer:         offerOf(waiting),
		Unapproved: []UnapprovedGuest{
			{
				GuestView: GuestView{GuestRef: model.GuestRef{Kind: model.KindLXC, VMID: 201}, Name: "new-1"},
				Identity:  "uuid:201", Hostnames: []string{"new.example.com"}, Why: []string{"admission mode approve"},
			},
			{
				GuestView: GuestView{GuestRef: model.GuestRef{Kind: model.KindLXC, VMID: 202}, Name: "dns-1"},
				Identity:  "uuid:202", Hostnames: []string{"dns.example.com"},
				Why:  []string{"delegated: alice@pve holds VM.Config.Network", "address 10.0.0.1 is the gateway of node pve1"},
				MACs: []string{"bc:24:11:00:02:02"}, Addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1")},
			},
		},
		Segments: []SegmentView{
			{Bridge: "vmbr1", Acknowledged: true, AcknowledgedAt: t0, Routes: 2},
			{Bridge: "vmbr1", VLAN: 20, Routes: 1},
		},
		Egress:     EgressView{State: EgressOff, Since: t0.Add(-time.Hour)},
		Admission:  "approve",
		GateTagged: 2,
		RogueConnectors: []RogueConnector{{
			Tunnel: "pco-abc123", TunnelID: "00000000-0000-4000-8000-000000000001", Account: "acc1",
			ID: "0d5e9a77-3b1c-4f2e-8a6d-5c4b3a291807", OriginIP: "198.51.100.7", Version: "2026.8.0", Since: t0.Add(-time.Minute),
		}},
	}
}

// populatedZones holds a zone in each state.
func populatedZones() []ZoneView {
	return []ZoneView{
		{
			Name: "example.com", ID: "zone1", Status: "active", AccountID: "acc1", State: ZoneServed,
			Credentials: []string{"cred1", "cred2"}, ServedBy: "cred1", Pinned: "cred1", Stale: []string{}, Excluded: []string{},
		},
		{
			Name: "example.info", ID: "zone3", Status: "active", AccountID: "acc3", State: ZoneFrozen,
			Credentials: []string{"cred1"}, Stale: []string{"cred1"}, Excluded: []string{},
			FrozenWhy: "zone example.info is no longer listed by credential cred1",
		},
		{
			Name: "example.net", ID: "zone4", Status: "active", AccountID: "acc2", State: ZoneNotServed,
			Credentials: []string{"cred1", "cred2"}, Pinned: "cred2", Stale: []string{}, Excluded: []string{"cred2"},
		},
		{
			Name: "example.org", ID: "zone2", Status: "active", AccountID: "acc1", State: ZoneLeftOut,
			Credentials: []string{"cred1"}, Stale: []string{}, Excluded: []string{"cred1"},
		},
	}
}

// populatedWaiting holds one of each kind of what waits for a confirmation.
func populatedWaiting() []Waiting {
	return []Waiting{
		{
			Kind:   "dns-removals",
			Detail: "mass delete guard: 7 of 9 records are being removed (1 in zones that could not be listed); confirm to proceed",
			Items:  []string{"a.example.com", "b.example.com"},
		},
		{
			Kind: "stale-zone", Subject: "example.info",
			Detail: "zone example.info is no longer listed by credential cred1; a confirmation takes it as gone, and its hostnames are taken off the tunnel",
			Items:  []string{},
		},
		{
			Kind: "unseen-tunnel", Subject: "pco-abc123",
			Detail: "tunnel pco-abc123 (00000000-0000-4000-8000-000000000002) in account acc2 is not visible through any credential; " +
				"a confirmation takes it as gone and removes its connector",
			Items: []string{},
		},
		{
			Kind: "vanished-guests",
			Detail: "2 guests that hold a hostname are no longer listed by Proxmox; " +
				"a confirmation takes them as removed, and their hostnames are released after the grace period",
			Items: []string{"qemu/104 db-1", "lxc/200"},
		},
	}
}

func requireGolden(t *testing.T, name string, v any) {
	t.Helper()
	// The escaping of < and > is the encoder's business, not the contract's.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(v))
	got := buf.Bytes()
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(path, got, 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), string(got), "the JSON of the state changed; run the test with -update when that is intended")
}

// The state is what the API serves to the CLI and the web UI: its JSON is a
// contract.
func TestTheJSONOfTheState(t *testing.T) {
	requireGolden(t, "state_populated.json", populatedState().normalized())
	requireGolden(t, "state_empty.json", emptyState())
	requireGolden(t, "events.json", []Event{
		{Seq: 7, Boot: testBoot, At: t0, Level: "warn", Kind: "route", Subject: "www.example.com", Message: "qemu/101: unreachable",
			Route: "www.example.com", Guest: "qemu/101", Account: "acc1"},
		{Seq: 8, Boot: testBoot, At: t0, Level: "info", Kind: "rollout", Subject: "pco-abc123", Message: "configuration version 3 runs on 2 connectors in account acc1",
			Tunnel: "pco-abc123", Account: "acc1"},
		numbered(9, actionEvent(t0, populatedState().Actions[0])),
		{Seq: 10, Boot: testBoot, At: t0, Level: "info", Kind: "admin", Subject: "www.example.com", Message: "adoption requested for the next run",
			Actor: "alice@pve (ticket)"},
	})
	requireGolden(t, "apply_result.json", ApplyResult{LeftObserveOnly: true, Accepted: populatedWaiting()[:1]})
	requireGolden(t, "apply_result_empty.json", ApplyResult{Accepted: []Waiting{}})
	requireGolden(t, "claims.json", populatedClaims())
	requireGolden(t, "approvals.json", populatedApprovals())
	requireGolden(t, "approval.json", Approval{Owner: "qemu/101", Guest: &GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Name: "web-1"},
		Identity: "uuid:101", Mode: "approve"})
	requireGolden(t, "approval_observed.json", Approval{
		Owner: "lxc/202", Guest: &GuestView{GuestRef: model.GuestRef{Kind: model.KindLXC, VMID: 202}, Name: "dns-1"},
		Identity: "uuid:202", Mode: "tag", MACs: []string{"bc:24:11:00:02:02"}, Addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1")},
	})
	requireGolden(t, "segments.json", populatedState().Segments)
}

const testBoot = "9f2c4e1a0b7d3c55"

func numbered(seq uint64, ev Event) Event {
	ev.Seq, ev.Boot = seq, testBoot
	return ev
}

func populatedClaims() []ClaimView {
	missing := t0.Add(time.Minute)
	web := model.GuestRef{Kind: model.KindQEMU, VMID: 101}
	clone := model.GuestRef{Kind: model.KindQEMU, VMID: 102}
	return []ClaimView{
		{Hostname: "api.example.com", Holder: "manual/api", Since: t0, State: ClaimServing, Waiting: []ClaimantView{}},
		{Hostname: "new.example.com", Holder: "qemu/102", Guest: &GuestView{GuestRef: clone, Name: "web-2"}, Since: t0, State: ClaimPending, Waiting: []ClaimantView{}},
		{
			Hostname: "old.example.com", Holder: "qemu/101", Guest: &GuestView{GuestRef: web, Name: "web-1"},
			Since: t0, MissingSince: &missing, State: ClaimHeld, Waiting: []ClaimantView{},
		},
		{Hostname: "shop.example.com", Holder: "lxc/200", Since: t0, State: ClaimUnknown, Waiting: []ClaimantView{}},
		{
			Hostname: "www.example.com", Holder: "qemu/101", Guest: &GuestView{GuestRef: web, Name: "web-1"},
			Since: t0, State: ClaimConflict,
			Waiting: []ClaimantView{{Owner: "qemu/102", Guest: &GuestView{GuestRef: clone, Name: "web-2"}, Since: missing}},
		},
	}
}

func populatedApprovals() []ApprovalView {
	return []ApprovalView{
		{Owner: "qemu/101", Guest: &GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Name: "web-1"}, Identity: "uuid:101", Current: "uuid:101", Matches: true},
		{
			Owner: "lxc/300", Guest: &GuestView{GuestRef: model.GuestRef{Kind: model.KindLXC, VMID: 300}}, Identity: "uuid:300",
			MACs: []string{"bc:24:11:00:03:00", "bc:24:11:00:03:01"}, Addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1")},
		},
	}
}

// A copy of a state shares nothing with it, down to the items of what waits.
func TestACloneOfTheStateOwnsWhatWaits(t *testing.T) {
	st := populatedState()
	c := st.clone()

	c.Waiting[0].Items[0] = "changed"
	c.Waiting[1].Detail = "changed"
	c.Unapproved[1].Why[0] = "changed"
	c.Unapproved[1].MACs[0] = "changed"
	c.Unapproved[1].Addresses[0] = netip.MustParseAddr("10.9.9.9")
	c.Segments[0].Routes = 9

	require.Equal(t, populatedState(), st)
}

func TestAStateComesBackFromItsJSON(t *testing.T) {
	st := populatedState().normalized()
	b, err := json.Marshal(st)
	require.NoError(t, err)
	var back State
	require.NoError(t, json.Unmarshal(b, &back))
	require.Equal(t, st, back)
}
