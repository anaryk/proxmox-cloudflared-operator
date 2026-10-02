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
	return State{
		At:       t0,
		Mode:     "enforce",
		Complete: true,
		Routes: []RouteView{
			{
				RouteStatus: planner.RouteStatus{
					Hostname: "www.example.com", Owner: "qemu/101", State: planner.StateActive,
					Service: "http://10.0.0.11:8080", Zone: "example.com", Warnings: []string{"a warning"},
				},
				Guest:      &GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Name: "web-1"},
				Candidates: []resolve.CandidateResult{{Addr: netip.MustParseAddr("10.0.0.11"), Source: resolve.FromStatic, OK: true}},
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
		Profile:       "host",
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
}

func TestAStateComesBackFromItsJSON(t *testing.T) {
	st := populatedState().normalized()
	b, err := json.Marshal(st)
	require.NoError(t, err)
	var back State
	require.NoError(t, json.Unmarshal(b, &back))
	require.Equal(t, st, back)
}
