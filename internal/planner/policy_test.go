package planner

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

const (
	apexReason     = `the apex of zone example.com is published only when allowHosts names it: add "example.com" to allowHosts`
	wildcardReason = `a wildcard is published only when an allowHosts pattern names it: add "*.example.com" to allowHosts`
)

var policyZones = []Zone{{ID: "z1", Name: "example.com", AccountID: "acc1", CredentialID: "cred1"}}

// planOf collects the Notes of one tagged guest with the settings, takes out
// what the policy refuses, settles the claims and plans them in the zone
// example.com, every hostname verified.
func planOf(t *testing.T, notes string, s Settings) (Collected, Plan) {
	t.Helper()
	g := model.Guest{
		Ref: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Running: true, Tags: []string{"cf-tunnel"}, Description: notes,
	}
	col := Collect([]model.Guest{g}, nil, s)
	routes, refused := RefuseUnnamed(col.Routes, policyZones, s.AllowHosts)
	claims := ResolveClaims(ClaimInput{
		Routes: routes, Held: col.Held, Claims: map[string]Claim{},
		Now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC), Grace: time.Minute,
	})
	targets := map[string]ResolvedTarget{}
	for _, rt := range claims.Winners {
		targets[rt.Hostname] = ResolvedTarget{Addr: netip.MustParseAddr("10.0.0.11"), Reachable: true, Owner: rt.Owner(), Level: "port"}
	}
	plan := Build(BuildInput{
		Winners: claims.Winners,
		Claims:  claims.Claims,
		Targets: targets,
		Zones:   policyZones,
		Writer:  Writer{InstallID: "abc123", Generation: 1, Nonce: "n1"},
		Refused: refused,
	})
	return col, plan
}

func statesOf(plan Plan) map[string]string {
	out := map[string]string{}
	for _, rs := range plan.Routes {
		out[rs.Hostname] = string(rs.State) + ": " + rs.Reason
	}
	return out
}

// With the default settings whoever may edit the Notes of one tagged guest
// could claim the apex of every zone the credential sees, and the wildcard
// below it, which serves every name of the zone that has no record of its
// own, with a valid certificate. Neither is published unless allowHosts
// names it.
func TestTheApexAndAWildcardAreRejectedUnlessAllowHostsNamesThem(t *testing.T) {
	col, plan := planOf(t, "```cf-tunnel\nexample.com *.example.com www.example.com -> :80\n```", Settings{})

	require.Empty(t, col.Issues)
	require.Len(t, col.Routes, 3)
	require.Equal(t, map[string]string{
		"example.com":     "rejected: " + apexReason,
		"*.example.com":   "rejected: " + wildcardReason,
		"www.example.com": "active: ",
	}, statesOf(plan))
	var names []string
	for _, r := range plan.Records {
		names = append(names, r.Name)
	}
	require.Equal(t, []string{"www.example.com"}, names, "no record for the apex or the wildcard")
	require.Len(t, plan.Tunnels, 1)
	require.Equal(t, []IngressRule{
		{Hostname: "www.example.com", Service: "http://10.0.0.11:80"}, SentinelRule(Writer{InstallID: "abc123", Generation: 1, Nonce: "n1"}), CatchAllRule(),
	}, plan.Tunnels[0].Rules, "no rule either: nothing at Cloudflare is theirs")
}

func TestWhatAnAllowHostsPatternNames(t *testing.T) {
	const notes = "```cf-tunnel\nexample.com *.example.com *.shop.example.com www.example.com -> :80\n```"
	for _, tt := range []struct {
		name  string
		allow []string
		want  map[string]string
	}{
		{"everything allowed is not naming them", []string{"*"}, map[string]string{
			"example.com": "rejected: " + apexReason, "*.example.com": "rejected: " + wildcardReason,
			"*.shop.example.com": `rejected: a wildcard is published only when an allowHosts pattern names it: add "*.shop.example.com" to allowHosts`,
			"www.example.com":    "active: ",
		}},
		{"the apex", []string{"example.com", "www.example.com"}, map[string]string{
			"example.com": "active: ", "www.example.com": "active: ",
		}},
		{"a wildcard and those below it", []string{"*.example.com"}, map[string]string{
			"*.example.com": "active: ", "*.shop.example.com": "active: ", "www.example.com": "active: ",
		}},
		{"a wildcard further down", []string{"*.shop.example.com", "*"}, map[string]string{
			"example.com": "rejected: " + apexReason, "*.example.com": "rejected: " + wildcardReason,
			"*.shop.example.com": "active: ", "www.example.com": "active: ",
		}},
		{"an exact wildcard", []string{"www.example.com", "*.example.com", "example.com"}, map[string]string{
			"example.com": "active: ", "*.example.com": "active: ", "*.shop.example.com": "active: ", "www.example.com": "active: ",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, plan := planOf(t, notes, Settings{AllowHosts: tt.allow})

			require.Equal(t, tt.want, statesOf(plan))
		})
	}
}

// Manual routes are the admin's own: root wrote them.
func TestAManualRouteMayBeTheApexOrAWildcard(t *testing.T) {
	manual := []model.Route{
		{Hostname: "example.com", Source: model.SourceManual, ManualID: "apex", Target: model.Target{Scheme: model.SchemeHTTP, Port: 80}},
		{Hostname: "*.example.com", Source: model.SourceManual, ManualID: "all", Target: model.Target{Scheme: model.SchemeHTTP, Port: 80}},
	}
	kept, refused := RefuseUnnamed(manual, policyZones, nil)
	require.Equal(t, manual, kept)
	require.Empty(t, refused)
	plan := Build(BuildInput{
		Winners: kept,
		Targets: map[string]ResolvedTarget{
			"example.com":   {Addr: netip.MustParseAddr("10.0.0.11"), Reachable: true},
			"*.example.com": {Addr: netip.MustParseAddr("10.0.0.11"), Reachable: true},
		},
		Zones:  []Zone{{ID: "z1", Name: "example.com", AccountID: "acc1", CredentialID: "cred1"}},
		Writer: Writer{InstallID: "abc123", Generation: 1, Nonce: "n1"},
	})

	require.Equal(t, map[string]string{"example.com": "active: ", "*.example.com": "active: "}, statesOf(plan))
}

func hostnames(n int) string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("h%d.example.com", i+1)
	}
	return "```cf-tunnel\n" + strings.Join(names, " ") + " -> :80\n```"
}

func TestAGuestOverTheCapPublishesNoneOfItsRoutes(t *testing.T) {
	for _, tt := range []struct {
		name   string
		count  int
		limit  int
		routes int
	}{
		{"at the default cap", 32, 0, 32},
		{"over the default cap", 33, 0, 0},
		{"at a cap of its own", 2, 2, 2},
		{"over a cap of its own", 3, 2, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			col, plan := planOf(t, hostnames(tt.count), Settings{MaxHostnamesPerGuest: tt.limit})

			require.Len(t, col.Routes, tt.routes)
			if tt.routes > 0 {
				require.Empty(t, col.Issues)
				return
			}
			limit := tt.limit
			if limit == 0 {
				limit = DefaultMaxHostnamesPerGuest
			}
			require.Equal(t, []Issue{{Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Msg: fmt.Sprintf(
				"the Notes name %d hostnames, more than maxHostnamesPerGuest allows (%d); none of them is published until they name at most %d",
				tt.count, limit, limit)}}, col.Issues)
			require.Len(t, col.Held, tt.count, "its hostnames stay its own, and serve nothing")
			require.Empty(t, plan.Records)
		})
	}
}

// A hostname the policy refuses does not count.
func TestTheCapCountsTheHostnamesThePolicyLetsThrough(t *testing.T) {
	notes := "```cf-tunnel\na.example.com b.example.com c.other.org -> :80\n```"

	col := Collect([]model.Guest{{
		Ref: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Tags: []string{"cf-tunnel"}, Description: notes,
	}}, nil, Settings{AllowHosts: []string{"*.example.com"}, MaxHostnamesPerGuest: 2})

	require.Equal(t, []string{"a.example.com qemu/101", "b.example.com qemu/101"}, routeKeys(col.Routes))
}

// settleWith collects the Notes of the guests, takes out what the policy
// refuses and settles the claims on top of stored, at now.
func settleWith(guests []model.Guest, allow []string, stored map[string]Claim, now time.Time) (ClaimResult, []RouteStatus) {
	col := Collect(guests, nil, Settings{AllowHosts: allow})
	routes, refused := RefuseUnnamed(col.Routes, policyZones, allow)
	return ResolveClaims(ClaimInput{Routes: routes, Held: col.Held, Claims: stored, Now: now, Grace: time.Minute}), refused
}

func apexGuest(vmid int) model.Guest {
	return model.Guest{
		Ref: model.GuestRef{Kind: model.KindQEMU, VMID: vmid}, Running: true, Tags: []string{"cf-tunnel"},
		Description: "```cf-tunnel\nexample.com -> :80\n```",
	}
}

// A guest that names the apex while the policy refuses it takes no claim and
// holds none, however long it names it. Once allowHosts names it, it goes by
// the rule for a hostname nobody holds: to the first of its claimants in owner
// order, from that moment, as if the refused route had never been there.
func TestARefusedNameTakesNoClaim(t *testing.T) {
	t1 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name           string
		squat, second  int
		winner, waiter string
	}{
		{"squatted by the guest first in order", 101, 200, "qemu/101", "qemu/200"},
		{"squatted by the guest second in order", 300, 200, "qemu/200", "qemu/300"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res, refused := settleWith([]model.Guest{apexGuest(tt.squat)}, nil, map[string]Claim{}, t1)
			require.Empty(t, res.Claims)
			require.Len(t, refused, 1)
			require.Equal(t, StateRejected, refused[0].State)

			res, _ = settleWith([]model.Guest{apexGuest(tt.squat), apexGuest(tt.second)}, nil, res.Claims, t1.Add(time.Hour))
			require.Empty(t, res.Claims, "nor while another names it too")

			allowed := t1.Add(2 * time.Hour)
			res, refused = settleWith([]model.Guest{apexGuest(tt.squat), apexGuest(tt.second)}, []string{"example.com"}, res.Claims, allowed)
			require.Empty(t, refused)
			require.Equal(t, Claim{
				Hostname: "example.com", Owner: tt.winner, Since: allowed, Waiting: []Waiter{{Owner: tt.waiter, FirstSeen: allowed}},
			}, res.Claims["example.com"])
		})
	}
}
