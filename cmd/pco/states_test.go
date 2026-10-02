package main

import (
	"fmt"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

const (
	tunnelA = "0a1b2c3d-0000-4000-8000-000000000001"
	tunnelB = "9f8e7d6c-0000-4000-8000-000000000002"
	tunnelC = "5e5e5e5e-0000-4000-8000-000000000003"
)

func routeView(host, owner string, state planner.RouteState, service, zone, reason string, warnings ...string) engine.RouteView {
	return engine.RouteView{RouteStatus: planner.RouteStatus{
		Hostname: host, Owner: owner, State: state, Service: service, Zone: zone, Reason: reason, Warnings: warnings,
	}}
}

// usableCredential is a credential whose last check passed and whose token
// expires in expires.
func usableCredential(id, label string, expires time.Duration) engine.CredentialView {
	on := t0.Add(expires)
	return engine.CredentialView{
		ID: id, Label: label, Kind: "scoped", Checked: true,
		Report: credentials.Report{
			Token:    cfapi.TokenStatus{ID: "tok-" + id, Status: "active", ExpiresOn: &on},
			Accounts: []cfapi.Account{{ID: "acc1", Name: "Main"}},
			Zones:    []cfapi.Zone{{ID: "zone1", Name: "example.com", Status: "active", AccountID: "acc1"}},
			Checks: []credentials.Check{
				{Capability: credentials.CapToken, OK: true},
				{Capability: credentials.CapAccounts, OK: true},
				{Capability: credentials.CapZones, OK: true},
				{Capability: credentials.CapDNSRead, Scope: "example.com", ScopeID: "zone1", OK: true},
				{Capability: credentials.CapTunnelRead, Scope: "Main", ScopeID: "acc1", OK: true},
			},
			Usable:    true,
			CheckedAt: t0,
		},
	}
}

// failingCredential is a credential whose last check found that its token
// cannot read DNS.
func failingCredential(id, label string) engine.CredentialView {
	v := usableCredential(id, label, 365*24*time.Hour)
	v.Report.Usable = false
	v.Report.Checks[3] = credentials.Check{
		Capability: credentials.CapDNSRead, Scope: "example.com", ScopeID: "zone1",
		Detail: "grant Zone > DNS > Read on example.com",
	}
	return v
}

func healthyState() engine.State {
	return engine.State{
		At:       t0,
		Mode:     "enforce",
		Profile:  "host",
		Complete: true,
		Routes: []engine.RouteView{
			routeView("www.example.com", "qemu/101", planner.StateActive, "http://10.0.0.11:8080", "example.com", ""),
			routeView("api.example.com", "qemu/102", planner.StateActive, "http://10.0.0.12:3000", "example.com", ""),
			routeView("blog.example.com", "lxc/200", planner.StateActive, "http://10.0.0.20:80", "example.com", ""),
			routeView("db.example.org", "qemu/103", planner.StateUnreachable, "", "example.org", "connection refused"),
		},
		Tunnels: []engine.TunnelView{
			{TunnelState: reconcile.TunnelState{AccountID: "acc1", CredentialID: "cred1", Name: "pco-abc123", ID: tunnelA, Version: 3, Exists: true, Verified: true}},
			{TunnelState: reconcile.TunnelState{AccountID: "acc2", CredentialID: "cred1", Name: "pco-abc123", ID: tunnelB, Version: 1, Exists: true}},
			{TunnelState: reconcile.TunnelState{AccountID: "acc3", CredentialID: "cred1", Name: "pco-abc123", Unknown: true}},
		},
		Connectors: []connector.Status{
			{TunnelID: tunnelA, Active: true, Ready: true, Connections: 4, MetricsAddr: "127.0.0.1:20300"},
			{TunnelID: tunnelB, Active: true, MetricsAddr: "127.0.0.1:20301"},
		},
		Credentials:   []engine.CredentialView{usableCredential("cred1", "main", 12*24*time.Hour)},
		WriterVerdict: "ok",
	}
}

func observeState() engine.State {
	st := healthyState()
	st.Mode = "observe"
	st.Tunnels, st.Connectors = nil, nil
	st.Credentials = []engine.CredentialView{usableCredential("cred1", "main", 365*24*time.Hour)}
	st.Credentials[0].Checked = false
	st.Credentials[0].Report = credentials.Report{}
	return st
}

func freshState() engine.State {
	return engine.State{
		At:            t0,
		Mode:          "observe",
		Profile:       "host",
		Complete:      true,
		WriterVerdict: "ok",
		Problems:      []string{"no Cloudflare credential; add one with pco credential add"},
	}
}

func problemState() engine.State {
	st := healthyState()
	st.Complete = false
	st.Profile = "appliance"
	st.WriterVerdict = "stale"
	st.Credentials = []engine.CredentialView{
		failingCredential("cred1", "main"),
		{ID: "cred2", Label: "spare", Kind: "scoped"},
		usableCredential("cred3", "old", -48*time.Hour),
	}
	st.Problems = []string{
		"cluster status: proxmox api: HTTP 500: no quorum",
		"the inventory is incomplete; claims, bindings, tunnels, DNS and connectors are left as they are",
	}
	// An account the last cycle found frozen: its route and its tunnel are
	// left as they are.
	frozen := "account frozen: zone example.net is no longer listed by credential cred1"
	st.Routes = append(st.Routes, routeView("shop.example.net", "qemu/104", engine.RouteFrozen, "", "example.net", frozen))
	st.Tunnels = append(st.Tunnels, engine.TunnelView{
		TunnelState: reconcile.TunnelState{AccountID: "acc4", CredentialID: "cred1", Name: "pco-abc123", ID: tunnelC, Exists: true},
		Held:        frozen,
	})
	st.Connectors = append(st.Connectors, connector.Status{TunnelID: tunnelC, Active: true, Ready: true, Connections: 2, MetricsAddr: "127.0.0.1:20302"})
	return st
}

// mixedRoutes has a route in every state, out of order.
func mixedRoutes() []engine.RouteView {
	return []engine.RouteView{
		routeView("www.example.com", "qemu/102", planner.StateConflict, "", "", "hostname is held by qemu/101"),
		routeView("www.example.com", "qemu/101", planner.StateActive, "http://10.0.0.11:8080", "example.com", "", "the guest answers slowly", "another warning"),
		routeView("shop.example.net", "qemu/104", planner.StateNoZone, "", "", "no zone for example.net"),
		routeView("api.example.com", "lxc/200", planner.StateUnreachable, "", "example.com", "connection refused"),
		routeView("old.example.com", "qemu/105", planner.StateWithdrawn, "http://10.0.0.15:80", "example.com", "guest is not running"),
		routeView("new.example.com", "qemu/106", planner.StateHeld, "", "example.com", "claimed by qemu/105"),
	}
}

func planState() engine.State {
	st := healthyState()
	st.Actions = []reconcile.Action{
		{Kind: reconcile.CreateRecord, Target: "www.example.com", Detail: "in zone example.com", Applied: true},
		{Kind: reconcile.CreateRecord, Target: "blog.example.com", Detail: "in zone example.com: tunnel pco-abc123 has no known id", Held: "tunnel not created yet"},
		{Kind: reconcile.UpdateRecord, Target: "api.example.com", Detail: "in zone example.com"},
		{Kind: reconcile.DeleteRecord, Target: "old.example.com", Detail: "in zone example.com", Destructive: true, Held: "grace period: 1m0s left"},
		{Kind: reconcile.DeleteRecord, Target: "gone.example.com", Detail: "in zone example.com", Destructive: true, Held: "mass delete guard: 6 of 9 records are being removed; confirm to proceed"},
	}
	st.Conflicts = []reconcile.Conflict{{Zone: "example.com", Name: "shop.example.com", Type: "A", Content: "192.0.2.10"}}
	st.Lost = []string{"lost.example.com"}
	// The guard counts the removal in its grace too, and a confirmation lets
	// it through.
	st.Waiting = []engine.Waiting{{
		Kind:   engine.WaitingRemovals,
		Detail: "mass delete guard: 6 of 9 records are being removed; confirm to proceed",
		Items:  []string{"gone.example.com", "old.example.com"},
	}}
	st.Offer = "1a2b3c4d5e6f7a8b"
	return st
}

// guardState is a daemon whose mass delete guard holds two removals while
// four more are in their grace.
func guardState() engine.State {
	st := healthyState()
	guard := "mass delete guard: 6 of 16 records are being removed; confirm to proceed"
	names := []string{"a.example.com", "b.example.com", "c.example.com", "d.example.com", "e.example.com", "f.example.com"}
	for i, name := range names {
		held := guard
		if i >= 2 {
			held = "grace period: 29s left"
		}
		st.Actions = append(st.Actions, reconcile.Action{Kind: reconcile.DeleteRecord, Target: name, Detail: "in zone example.com: CNAME", Destructive: true, Held: held})
	}
	st.Problems = []string{guard}
	st.Waiting = []engine.Waiting{{Kind: engine.WaitingRemovals, Detail: guard, Items: names}}
	st.Offer = "0f1e2d3c4b5a6978"
	return st
}

// vanishedGuests is what a vanish hold offers: n guests from qemu/101 on.
func vanishedGuests(n int) engine.Waiting {
	w := engine.Waiting{
		Kind: engine.WaitingVanished,
		Detail: fmt.Sprintf("%d guests that hold a hostname are no longer listed by Proxmox; "+
			"a confirmation takes them as removed, and their hostnames are released after the grace period", n),
	}
	for i := range n {
		w.Items = append(w.Items, fmt.Sprintf("qemu/%d vm-%d", 101+i, 101+i))
	}
	return w
}

// zoneAndTunnelState is a daemon that waits for the confirmation of a zone
// that left its listing and of a tunnel no credential sees; a removal in its
// grace is pending besides.
func zoneAndTunnelState() engine.State {
	st := healthyState()
	st.Actions = []reconcile.Action{
		{Kind: reconcile.DeleteRecord, Target: "old.example.com", Detail: "in zone example.com", Destructive: true, Held: "grace period: 1m0s left"},
	}
	st.Problems = []string{
		"zone example.net is no longer listed by credential cred1; account acc4 is left as it is " +
			"until the zone is listed again or pco apply --confirm-deletes confirms it is gone",
		"tunnel pco-abc123 in account acc3 is not visible through any credential; its connector is kept " +
			"until a credential sees the account again or pco apply --confirm-deletes confirms the tunnel is gone",
	}
	st.Waiting = []engine.Waiting{
		{
			Kind: engine.WaitingZone, Subject: "example.net",
			Detail: "zone example.net is no longer listed by credential cred1; a confirmation takes it as gone, " +
				"and its hostnames are taken off the tunnel",
			Items: []string{},
		},
		{
			Kind: engine.WaitingTunnel, Subject: "pco-abc123",
			Detail: "tunnel pco-abc123 (" + tunnelC + ") in account acc3 is not visible through any credential; " +
				"a confirmation takes it as gone and removes its connector",
			Items: []string{},
		},
	}
	st.Offer = "9a8b7c6d5e4f3a2b"
	return st
}

// graceState waits for nothing: a removal is in its grace and an adoption
// waits for its tunnel, and a confirmation affects neither.
func graceState() engine.State {
	st := healthyState()
	st.Actions = []reconcile.Action{
		{Kind: reconcile.DeleteRecord, Target: "old.example.com", Detail: "in zone example.com", Destructive: true, Held: "grace period: 1m0s left"},
		{Kind: reconcile.UpdateRecord, Target: "shop.example.com", Detail: "in zone example.com: A 192.0.2.10", Destructive: true, Held: "tunnel configuration not verified"},
	}
	st.Waiting = []engine.Waiting{}
	return st
}

// issues returns n problems found in guest notes: the first of the settings,
// the others of guests.
func issues(n int) []planner.Issue {
	var out []planner.Issue
	for i := range n {
		if i == 0 {
			out = append(out, planner.Issue{Msg: "allowHosts[0]: not a valid pattern"})
			continue
		}
		out = append(out, planner.Issue{
			Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 100 + i},
			Line:  i, Col: 2 * i,
			Msg: fmt.Sprintf("entry %d is broken", i),
		})
	}
	return out
}
