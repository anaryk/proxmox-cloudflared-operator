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
		At:       t0,
		Mode:     "enforce",
		Complete: true,
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
		Connectors: []connector.Status{{TunnelID: "00000000-0000-4000-8000-000000000001", Active: true, Ready: true, Connections: 4, MetricsAddr: "127.0.0.1:20300"}},
		Credentials: []CredentialView{
			{
				ID: "cred1", Label: "main", Kind: "scoped", Checked: true,
				Report: credentials.Report{
					Token:     cfapi.TokenStatus{ID: "token-1", Status: "active", ExpiresOn: &expires},
					Accounts:  []cfapi.Account{{ID: "acc1", Name: "Main"}},
					Zones:     []cfapi.Zone{{ID: "zone1", Name: "example.com", Status: "active", AccountID: "acc1"}},
					Checks:    []credentials.Check{{Capability: credentials.CapToken, OK: true}, {Capability: credentials.CapDNSRead, Scope: "example.com", ScopeID: "zone1", Detail: "grant Zone > DNS > Read on example.com"}},
					Deep:      true,
					Usable:    true,
					Leftovers: []string{"_pco-probe-x.example.com"},
					CheckedAt: t0,
				},
			},
			{ID: "cred2", Label: "spare", Kind: "scoped"},
		},
		Actions: []reconcile.Action{
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
		Unapproved: []UnapprovedGuest{{
			GuestView: GuestView{GuestRef: model.GuestRef{Kind: model.KindLXC, VMID: 201}, Name: "new-1"},
			Identity:  "uuid:201", Hostnames: []string{"new.example.com"},
		}},
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
	requireGolden(t, "events.json", []Event{{Seq: 7, At: t0, Level: "warn", Kind: "route", Subject: "www.example.com", Message: "qemu/101: unreachable"}})
	requireGolden(t, "apply_result.json", ApplyResult{LeftObserveOnly: true, Accepted: populatedWaiting()[:1]})
	requireGolden(t, "apply_result_empty.json", ApplyResult{Accepted: []Waiting{}})
	requireGolden(t, "claims.json", populatedClaims())
	requireGolden(t, "approvals.json", populatedApprovals())
	requireGolden(t, "approval.json", Approval{Owner: "qemu/101", Guest: &GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Name: "web-1"},
		Identity: "uuid:101", Mode: "approve"})
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
		{Owner: "lxc/300", Guest: &GuestView{GuestRef: model.GuestRef{Kind: model.KindLXC, VMID: 300}}, Identity: "uuid:300"},
	}
}

// A copy of a state shares nothing with it, down to the items of what waits.
func TestACloneOfTheStateOwnsWhatWaits(t *testing.T) {
	st := populatedState()
	c := st.clone()

	c.Waiting[0].Items[0] = "changed"
	c.Waiting[1].Detail = "changed"

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
