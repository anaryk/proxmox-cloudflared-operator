package planner

import (
	"encoding/json"
	"maps"
	"net/netip"
	"slices"
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
	res := ResolveClaims(ClaimInput{
		Routes: col.Routes, Held: col.Held, Claims: stored, Identity: identity, Now: now, Grace: grace,
	})
	plan := Build(BuildInput{
		Winners:   res.Winners,
		Conflicts: res.Conflicts,
		Claims:    res.Claims,
		Targets:   p.targets,
		Zones:     p.zones,
		Writer:    buildWriter,
	})
	return cycle{collected: col, claims: res, plan: plan}
}

// persist sends claims through JSON, as the operator stores them between
// cycles.
func persist(t *testing.T, claims map[string]Claim) map[string]Claim {
	t.Helper()
	data, err := json.Marshal(claims)
	require.NoError(t, err)
	var stored map[string]Claim
	require.NoError(t, json.Unmarshal(data, &stored))
	return stored
}

// identified is a tagged guest with an identity of its own.
func identified(kind model.GuestKind, vmid int, notes string) model.Guest {
	g := tagged(kind, vmid, notes)
	g.Identity = "id-" + g.Ref.String()
	return g
}

func TestPipeline(t *testing.T) {
	shop := identified(model.KindQEMU, 101, block(
		"shop.example.com www.example.com -> :8080",
		"*.example.com -> https://:8443 no-tls-verify",
	))
	clone := identified(model.KindQEMU, 102, block("www.example.com -> :8080"))
	api := identified(model.KindLXC, 200, block("api.example.com -> :3000"))
	// Points the tunnel at the Proxmox host's own web interface.
	hostile := identified(model.KindQEMU, 666, block("admin.example.com -> https://127.0.0.1:8006 no-tls-verify"))
	// Names its own address, which nobody has verified.
	pinned := identified(model.KindQEMU, 300, block("db.example.com -> 10.20.0.99:5432"))
	untagged := identified(model.KindQEMU, 400, block("other.example.com -> :80"))
	untagged.Tags = nil
	shopRef := shop.Ref

	p := pipeline{
		guests: []model.Guest{shop, clone, api, hostile, pinned, untagged},
		manual: []model.Route{
			{
				Hostname: "grafana.example.com",
				Target:   model.Target{Scheme: model.SchemeHTTPS, Addr: netip.MustParseAddr("10.20.0.50"), Port: 3000},
				Source:   model.SourceManual,
				ManualID: "grafana",
				Guest:    &shopRef,
			},
			// The admin's catch-all for the zone. It must never serve a name
			// that a guest holds.
			{
				Hostname: "*.example.com",
				Target:   model.Target{Scheme: model.SchemeHTTP, Addr: netip.MustParseAddr("10.20.0.60"), Port: 80},
				Source:   model.SourceManual,
				ManualID: "fallback",
			},
		},
		settings: Settings{DenyHosts: []string{"secret.example.com"}},
		targets: map[string]ResolvedTarget{
			"shop.example.com":    verified("10.20.0.11"),
			"www.example.com":     verified("10.20.0.11"),
			"api.example.com":     verified("10.20.0.20"),
			"grafana.example.com": verified("10.20.0.50"),
			"other.example.com":   verified("10.20.0.40"),
			"*.example.com":       verified("10.20.0.60"),
		},
		zones: []Zone{exampleZone},
	}
	overlaps := func(host string) []string {
		return []string{host + " overlaps *.example.com owned by manual/fallback"}
	}
	catchAll := func(covered ...string) RouteStatus {
		st := RouteStatus{
			Hostname: "*.example.com", Owner: "manual/fallback", State: StateActive,
			Service: "http://10.20.0.60:80", Zone: "example.com",
		}
		for _, c := range covered {
			st.Warnings = append(st.Warnings, "*.example.com overlaps "+c)
		}
		return st
	}
	catchAllRule := IngressRule{Hostname: "*.example.com", Service: "http://10.20.0.60:80"}

	first := p.run(nil, t0)

	require.Equal(t, []Issue{
		{Guest: shop.Ref, Line: 3, Col: 1, Msg: `hostname "*.example.com" is not allowed by policy`},
		{
			Guest: hostile.Ref, Line: 2, Col: 22,
			Msg: `invalid target "https://127.0.0.1:8006": address is not routable to a guest`,
		},
	}, first.collected.Issues)
	// The hostile entry is broken, so its hostname is held, but nobody holds
	// a claim on it and none is made.
	require.Equal(t, []HeldName{{Hostname: "admin.example.com", Owner: "qemu/666"}}, first.collected.Held)
	require.NotContains(t, first.claims.Claims, "admin.example.com")
	require.Equal(t, []string{
		"claimed *.example.com manual/fallback",
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
				catchAllRule,
			),
		}},
		Records: []RecordPlan{
			record("*.example.com"),
			record("api.example.com"),
			record("grafana.example.com"),
			record("shop.example.com"),
			record("www.example.com"),
		},
		Routes: []RouteStatus{
			catchAll(
				"api.example.com owned by lxc/200",
				"db.example.com owned by qemu/300",
				"grafana.example.com owned by manual/grafana",
				"shop.example.com owned by qemu/101",
				"www.example.com owned by qemu/101",
			),
			{
				Hostname: "api.example.com", Owner: "lxc/200", State: StateActive,
				Service: "http://10.20.0.20:3000", Zone: "example.com", Warnings: overlaps("api.example.com"),
			},
			{
				Hostname: "db.example.com", Owner: "qemu/300", State: StateUnreachable, Reason: noAddrReason,
				Zone: "example.com", Warnings: overlaps("db.example.com"),
			},
			{
				Hostname: "grafana.example.com", Owner: "manual/grafana", State: StateActive,
				Service: "https://10.20.0.50:3000", Zone: "example.com", Warnings: overlaps("grafana.example.com"),
			},
			{
				Hostname: "shop.example.com", Owner: "qemu/101", State: StateActive,
				Service: "http://10.20.0.11:8080", Zone: "example.com", Warnings: overlaps("shop.example.com"),
			},
			{
				Hostname: "www.example.com", Owner: "qemu/101", State: StateActive,
				Service: "http://10.20.0.11:8080", Zone: "example.com", Warnings: overlaps("www.example.com"),
			},
			{Hostname: "www.example.com", Owner: "qemu/102", State: StateConflict, Reason: "hostname is held by qemu/101"},
		},
	}, first.plan)

	stored := persist(t, first.claims.Claims)

	second := p.run(stored, at(time.Minute))

	require.Empty(t, second.claims.Events)
	require.Equal(t, first.plan, second.plan)
	require.Equal(t, stored, second.claims.Claims)
	require.Equal(t, first.collected, second.collected)

	// Third: a fence in a comment breaks the shop's block for longer than the
	// grace while its clone waits for www.example.com. The shop keeps its
	// claims and nothing else serves them: not the clone, not the catch-all.
	broken := p
	broken.guests = slices.Clone(p.guests)
	broken.guests[0].Description = block(
		"shop.example.com www.example.com -> :8080",
		"*.example.com -> https://:8443 no-tls-verify # was ```:443```",
	)

	third := broken.run(stored, at(2*time.Minute))
	later := broken.run(persist(t, third.claims.Claims), at(2*time.Minute+2*grace))

	for _, c := range []cycle{third, later} {
		require.Equal(t, []Issue{
			{Guest: shop.Ref, Line: 3, Col: 52, Msg: "the closing fence is hidden by a comment; put the comment on its own line"},
			first.collected.Issues[1],
		}, c.collected.Issues)
		// The broken block still names the wildcard, but the deny pattern
		// rules it out, so it is not held.
		require.Equal(t, []HeldName{
			{Hostname: "admin.example.com", Owner: "qemu/666"},
			{Hostname: "shop.example.com", Owner: "qemu/101"},
			{Hostname: "www.example.com", Owner: "qemu/101"},
		}, c.collected.Held)
		require.Empty(t, c.claims.Events)
		require.Equal(t, stored, c.claims.Claims)
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
					blockRule("shop.example.com"),
					blockRule("www.example.com"),
					catchAllRule,
				),
			}},
			Records: []RecordPlan{
				record("*.example.com"),
				record("api.example.com"),
				record("grafana.example.com"),
			},
			Routes: []RouteStatus{
				catchAll(
					"api.example.com owned by lxc/200",
					"db.example.com owned by qemu/300",
					"grafana.example.com owned by manual/grafana",
				),
				first.plan.Routes[1],
				first.plan.Routes[2],
				first.plan.Routes[3],
				heldStatus("shop.example.com", "qemu/101", "example.com"),
				heldStatus("www.example.com", "qemu/101", "example.com"),
				{Hostname: "www.example.com", Owner: "qemu/102", State: StateConflict, Reason: "hostname is held by qemu/101"},
			},
		}, c.plan)
	}

	// Fourth: the block is repaired and the shop serves again, as before.
	fourth := p.run(persist(t, later.claims.Claims), at(5*time.Minute))

	require.Empty(t, fourth.claims.Events)
	require.Equal(t, stored, fourth.claims.Claims)
	require.Equal(t, first.collected, fourth.collected)
	require.Equal(t, first.plan, fourth.plan)
}

// holderAndClone runs one cycle with a guest that takes www.example.com and
// a clone of it, made later, that carries the same Notes and so waits. The
// address was verified for the holder.
func holderAndClone(t *testing.T) (pipeline, map[string]Claim) {
	t.Helper()
	notes := block("www.example.com -> :8080")
	p := pipeline{
		guests: []model.Guest{identified(model.KindQEMU, 101, notes), identified(model.KindQEMU, 102, notes)},
		targets: map[string]ResolvedTarget{
			"www.example.com": {Addr: netip.MustParseAddr("10.20.0.11"), Reachable: true, Owner: "qemu/101"},
		},
		zones: []Zone{exampleZone},
	}

	first := p.run(nil, t0)

	require.Equal(t, []string{
		"claimed www.example.com qemu/101",
		"conflict www.example.com qemu/102",
	}, eventKeys(t, first.claims.Events))
	require.Equal(t, []string{"www.example.com"}, recordNames(first.plan.Records))
	return p, persist(t, first.claims.Claims)
}

func TestPipelineHolderRemovingTheHostnameHandsItOver(t *testing.T) {
	p, stored := holderAndClone(t)
	// Nothing in the Notes names the hostname any more, not even prose.
	p.guests[0].Description = "Web server, retired. Its site moved to the clone."

	gone := p.run(stored, at(time.Minute))

	require.Empty(t, gone.collected.Held)
	require.Empty(t, gone.claims.Events)
	require.Empty(t, gone.claims.Winners)
	require.Equal(t, at(time.Minute), *gone.claims.Claims["www.example.com"].MissingSince)
	require.Equal(t, []RouteStatus{
		{
			Hostname: "www.example.com", Owner: "qemu/101", State: StateHeld, Zone: "example.com",
			Reason: "no longer claimed by qemu/101 since 2026-03-01T12:01:00Z; released after the grace period",
		},
		{Hostname: "www.example.com", Owner: "qemu/102", State: StateConflict, Reason: "hostname is held by qemu/101"},
	}, gone.plan.Routes)

	after := p.run(persist(t, gone.claims.Claims), at(time.Minute+grace))

	require.Equal(t, []string{"transferred www.example.com qemu/102"}, eventKeys(t, after.claims.Events))
	require.Equal(t, []string{"qemu/102"}, ownersOf(after.claims.Winners))
	// The address is still the one verified for the old holder, so the clone
	// is not served until resolution verifies one for it.
	require.Equal(t, Plan{
		Tunnels: []TunnelPlan{},
		Records: []RecordPlan{},
		Routes: []RouteStatus{{
			Hostname: "www.example.com", Owner: "qemu/102", State: StateUnreachable,
			Reason: "address was verified for another owner", Zone: "example.com",
		}},
	}, after.plan)
}

func TestPipelineRouteAfterAStrayClosingFenceKeepsItsClaim(t *testing.T) {
	p := pipeline{
		guests: []model.Guest{
			identified(model.KindQEMU, 101, block("api.example.com -> :3000", "www.example.com -> :8080")),
			identified(model.KindQEMU, 102, block("www.example.com -> :8080")),
		},
		targets: map[string]ResolvedTarget{
			"api.example.com": verified("10.20.0.11"),
			"www.example.com": verified("10.20.0.11"),
		},
		zones: []Zone{exampleZone},
	}
	first := p.run(nil, t0)
	require.Equal(t, []string{
		"claimed api.example.com qemu/101",
		"claimed www.example.com qemu/101",
		"conflict www.example.com qemu/102",
	}, eventKeys(t, first.claims.Events))
	stored := persist(t, first.claims.Claims)

	// The stray fence closes the block, so the www line is prose now and the
	// last fence opens a block of its own.
	broken := p
	broken.guests = slices.Clone(p.guests)
	broken.guests[0].Description = "```cf-tunnel\napi.example.com -> :3000\n```\nwww.example.com -> :8080\n```"

	third := broken.run(stored, at(time.Minute))
	later := broken.run(persist(t, third.claims.Claims), at(time.Minute+2*grace))

	for _, c := range []cycle{third, later} {
		require.Equal(t, []string{"api.example.com qemu/101", "www.example.com qemu/102"}, routeKeys(c.collected.Routes))
		require.Equal(t, []HeldName{{Hostname: "www.example.com", Owner: "qemu/101"}}, c.collected.Held)
		require.Empty(t, c.claims.Events)
		require.Equal(t, stored, c.claims.Claims)
		require.Equal(t, []TunnelPlan{{
			AccountID: "acc2", CredentialID: "cred2", Name: tunnelName,
			Rules: withSentinel(
				IngressRule{Hostname: "api.example.com", Service: "http://10.20.0.11:3000"},
				blockRule("www.example.com"),
			),
		}}, c.plan.Tunnels)
		require.Equal(t, []string{"api.example.com"}, recordNames(c.plan.Records))
		require.Equal(t, []RouteStatus{
			{
				Hostname: "api.example.com", Owner: "qemu/101", State: StateActive,
				Service: "http://10.20.0.11:3000", Zone: "example.com",
			},
			heldStatus("www.example.com", "qemu/101", "example.com"),
			{Hostname: "www.example.com", Owner: "qemu/102", State: StateConflict, Reason: "hostname is held by qemu/101"},
		}, c.plan.Routes)
	}

	repaired := p.run(persist(t, later.claims.Claims), at(time.Minute+3*grace))

	require.Empty(t, repaired.claims.Events)
	require.Equal(t, first.plan, repaired.plan)
}

func TestPipelineHostnameInProseClaimsNothing(t *testing.T) {
	p := pipeline{
		guests: []model.Guest{identified(model.KindQEMU, 300, block("db.example.com -> :5432")+
			"\nReplica of docs.example.com; see (wiki.example.com)")},
		targets: map[string]ResolvedTarget{"db.example.com": verified("10.20.0.30")},
		zones:   []Zone{exampleZone},
	}

	c := p.run(nil, t0)

	require.Equal(t, []HeldName{
		{Hostname: "docs.example.com", Owner: "qemu/300"},
		{Hostname: "wiki.example.com", Owner: "qemu/300"},
	}, c.collected.Held)
	require.Equal(t, []string{"claimed db.example.com qemu/300"}, eventKeys(t, c.claims.Events))
	require.Equal(t, []string{"db.example.com"}, slices.Sorted(maps.Keys(c.claims.Claims)))
	require.Equal(t, Plan{
		Tunnels: []TunnelPlan{{
			AccountID: "acc2", CredentialID: "cred2", Name: tunnelName,
			Rules: withSentinel(IngressRule{Hostname: "db.example.com", Service: "http://10.20.0.30:5432"}),
		}},
		Records: []RecordPlan{recordFor(exampleZone, "db.example.com")},
		Routes: []RouteStatus{{
			Hostname: "db.example.com", Owner: "qemu/300", State: StateActive,
			Service: "http://10.20.0.30:5432", Zone: "example.com",
		}},
	}, c.plan)
}

func TestPipelineInvalidDenyPatternKeepsClaims(t *testing.T) {
	p, stored := holderAndClone(t)
	p.settings = Settings{DenyHosts: []string{"secret.example.com/"}}

	broken := p.run(stored, at(time.Minute))
	later := p.run(persist(t, broken.claims.Claims), at(time.Minute+2*grace))

	// Holder and clone both only name the hostname now. The holder keeps the
	// claim and the clone its place in line.
	for _, c := range []cycle{broken, later} {
		require.True(t, c.collected.PolicyInvalid)
		require.Equal(t, []HeldName{
			{Hostname: "www.example.com", Owner: "qemu/101"},
			{Hostname: "www.example.com", Owner: "qemu/102"},
		}, c.collected.Held)
		require.Empty(t, c.claims.Winners)
		require.Empty(t, c.claims.Conflicts)
		require.Empty(t, c.claims.Events)
		require.Equal(t, stored, c.claims.Claims)
		require.Equal(t, Plan{
			Tunnels: []TunnelPlan{},
			Records: []RecordPlan{},
			Routes:  []RouteStatus{heldStatus("www.example.com", "qemu/101", "example.com")},
		}, c.plan)
	}

	p.settings = Settings{DenyHosts: []string{"secret.example.com"}}

	fixed := p.run(persist(t, later.claims.Claims), at(time.Minute+3*grace))

	require.False(t, fixed.collected.PolicyInvalid)
	require.Empty(t, fixed.claims.Events)
	require.Equal(t, []string{"qemu/101"}, ownersOf(fixed.claims.Winners))
	require.Equal(t, stored, fixed.claims.Claims, "same holder, same waiter, same first seen")
	require.Equal(t, []Waiter{{Owner: "qemu/102", FirstSeen: t0}}, fixed.claims.Claims["www.example.com"].Waiting)
	require.Equal(t, []TunnelPlan{{
		AccountID: "acc2", CredentialID: "cred2", Name: tunnelName,
		Rules: withSentinel(httpRule("www.example.com", "10.20.0.11")),
	}}, fixed.plan.Tunnels)
}

func TestPipelineValidDenyPatternReleasesTheHostname(t *testing.T) {
	p, stored := holderAndClone(t)
	p.settings = Settings{DenyHosts: []string{"*.example.com"}}

	denied := p.run(stored, at(time.Minute))

	require.False(t, denied.collected.PolicyInvalid)
	require.Empty(t, denied.collected.Held)
	require.Empty(t, denied.claims.Events)
	require.Equal(t, at(time.Minute), *denied.claims.Claims["www.example.com"].MissingSince)
	require.Equal(t, []RouteStatus{{
		Hostname: "www.example.com", Owner: "qemu/101", State: StateHeld, Zone: "example.com",
		Reason: "no longer claimed by qemu/101 since 2026-03-01T12:01:00Z; released after the grace period",
	}}, denied.plan.Routes)

	after := p.run(persist(t, denied.claims.Claims), at(time.Minute+grace))

	require.Equal(t, []string{"released www.example.com qemu/101"}, eventKeys(t, after.claims.Events))
	require.Empty(t, after.claims.Claims)
	require.Empty(t, after.claims.Winners, "the clone is denied too")
	require.Equal(t, Plan{Tunnels: []TunnelPlan{}, Records: []RecordPlan{}, Routes: []RouteStatus{}}, after.plan)
}
