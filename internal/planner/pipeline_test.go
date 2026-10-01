package planner

import (
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// pipeline holds what the operator knows in one cycle and runs Collect,
// ResolveClaims and Build on it the way the operator does.
type pipeline struct {
	guests   []model.Guest
	manual   []model.Route
	settings Settings
	targets  map[string]ResolvedTarget
	zones    []Zone
}

type cycle struct {
	collected Collected
	claims    ClaimResult
	plan      Plan
}

func (p pipeline) run(stored map[string]Claim, now time.Time) cycle {
	col := Collect(p.guests, p.manual, p.settings)
	identity := make(map[string]string, len(p.guests))
	for _, g := range p.guests {
		identity[g.Ref.String()] = g.Identity
	}
	res := ResolveClaims(ClaimInput{Routes: col.Routes, Claims: stored, Identity: identity, Now: now, Grace: grace})
	holders := make(map[string]string, len(res.Claims))
	for host, c := range res.Claims {
		holders[host] = c.Owner
	}
	plan := Build(BuildInput{
		Winners:   res.Winners,
		Conflicts: res.Conflicts,
		Holders:   holders,
		Targets:   p.targets,
		Zones:     p.zones,
		Writer:    buildWriter,
	})
	return cycle{collected: col, claims: res, plan: plan}
}

func TestPipeline(t *testing.T) {
	guest := func(kind model.GuestKind, vmid int, notes string) model.Guest {
		g := tagged(kind, vmid, notes)
		g.Identity = "id-" + g.Ref.String()
		return g
	}
	shop := guest(model.KindQEMU, 101, block(
		"shop.example.com www.example.com -> :8080",
		"*.example.com -> https://:8443 no-tls-verify",
	))
	clone := guest(model.KindQEMU, 102, block("www.example.com -> :8080"))
	api := guest(model.KindLXC, 200, block("api.example.com -> :3000"))
	// Points the tunnel at the Proxmox host's own web interface.
	hostile := guest(model.KindQEMU, 666, block("admin.example.com -> https://127.0.0.1:8006 no-tls-verify"))
	// Names its own address, which nobody has verified.
	pinned := guest(model.KindQEMU, 300, block("db.example.com -> 10.20.0.99:5432"))
	untagged := guest(model.KindQEMU, 400, block("other.example.com -> :80"))
	untagged.Tags = nil
	shopRef := shop.Ref

	p := pipeline{
		guests: []model.Guest{shop, clone, api, hostile, pinned, untagged},
		manual: []model.Route{{
			Hostname: "grafana.example.com",
			Target:   model.Target{Scheme: model.SchemeHTTPS, Addr: netip.MustParseAddr("10.20.0.50"), Port: 3000},
			Source:   model.SourceManual,
			ManualID: "grafana",
			Guest:    &shopRef,
		}},
		settings: Settings{DenyHosts: []string{"secret.example.com"}},
		targets: map[string]ResolvedTarget{
			"shop.example.com":    verified("10.20.0.11"),
			"www.example.com":     verified("10.20.0.11"),
			"api.example.com":     verified("10.20.0.20"),
			"grafana.example.com": verified("10.20.0.50"),
			"other.example.com":   verified("10.20.0.40"),
		},
		zones: []Zone{exampleZone},
	}

	first := p.run(nil, t0)

	require.Equal(t, []Issue{
		{Guest: shop.Ref, Line: 3, Col: 1, Msg: `hostname "*.example.com" is not allowed by policy`},
		{
			Guest: hostile.Ref, Line: 2, Col: 22,
			Msg: `invalid target "https://127.0.0.1:8006": address is not routable to a guest`,
		},
	}, first.collected.Issues)
	require.Equal(t, []string{
		"claimed api.example.com lxc/200",
		"claimed db.example.com qemu/300",
		"claimed grafana.example.com manual/grafana",
		"claimed shop.example.com qemu/101",
		"claimed www.example.com qemu/101",
		"conflict www.example.com qemu/102",
	}, eventKeys(t, first.claims.Events))

	record := func(name string) RecordPlan { return recordFor(exampleZone, name) }
	require.Equal(t, Plan{
		Tunnels: []TunnelPlan{{
			AccountID:    "acc2",
			CredentialID: "cred2",
			Name:         tunnelName,
			Rules: withSentinel(
				IngressRule{Hostname: "api.example.com", Service: "http://10.20.0.20:3000"},
				blockRule("db.example.com"),
				IngressRule{
					Hostname: "grafana.example.com", Service: "https://10.20.0.50:3000", OriginServerName: "grafana.example.com",
				},
				httpRule("shop.example.com", "10.20.0.11"),
				httpRule("www.example.com", "10.20.0.11"),
			),
		}},
		Records: []RecordPlan{
			record("api.example.com"),
			record("grafana.example.com"),
			record("shop.example.com"),
			record("www.example.com"),
		},
		Routes: []RouteStatus{
			{
				Hostname: "api.example.com", Owner: "lxc/200", State: StateActive,
				Service: "http://10.20.0.20:3000", Zone: "example.com",
			},
			{Hostname: "db.example.com", Owner: "qemu/300", State: StateUnreachable, Reason: noAddrReason, Zone: "example.com"},
			{
				Hostname: "grafana.example.com", Owner: "manual/grafana", State: StateActive,
				Service: "https://10.20.0.50:3000", Zone: "example.com",
			},
			{
				Hostname: "shop.example.com", Owner: "qemu/101", State: StateActive,
				Service: "http://10.20.0.11:8080", Zone: "example.com",
			},
			{
				Hostname: "www.example.com", Owner: "qemu/101", State: StateActive,
				Service: "http://10.20.0.11:8080", Zone: "example.com",
			},
			{Hostname: "www.example.com", Owner: "qemu/102", State: StateConflict, Reason: "hostname is held by qemu/101"},
		},
	}, first.plan)

	data, err := json.Marshal(first.claims.Claims)
	require.NoError(t, err)
	var stored map[string]Claim
	require.NoError(t, json.Unmarshal(data, &stored))

	second := p.run(stored, at(time.Minute))

	require.Empty(t, second.claims.Events)
	require.Equal(t, first.plan, second.plan)
	require.Equal(t, stored, second.claims.Claims)
	require.Equal(t, first.collected, second.collected)
}
