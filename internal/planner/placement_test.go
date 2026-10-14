package planner

import (
	"math/rand/v2"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

const pinnedTo2 = "pco-3f9a2c1e77b0-pve2"

func pinned(node string) PlacementInput { return PlacementInput{Kind: PlacementPinned, Node: node} }

// pinnedRecordFor is the record of a hostname that points at the tunnel
// pinned to a node.
func pinnedRecordFor(z Zone, name, node string) RecordPlan {
	rp := recordFor(z, name)
	rp.TunnelName = tunnelName + "-" + node
	return rp
}

func TestPinnedTunnelName(t *testing.T) {
	longest := strings.Repeat("n", maxLabelLen-len(tunnelName)-1)
	tests := []struct {
		node string
		want string
		err  string
	}{
		{node: "pve2", want: pinnedTo2},
		{node: "pve-node-1", want: tunnelName + "-pve-node-1"},
		{node: "7", want: tunnelName + "-7"},
		{node: longest, want: tunnelName + "-" + longest},
		{node: longest + "n", err: "is longer than 63 characters"},
		{node: "", err: `node name ""`},
		{node: "PVE2", err: `node name "PVE2"`},
		{node: "pve_2", err: `node name "pve_2"`},
		{node: "pve.2", err: `node name "pve.2"`},
		{node: "-pve2", err: `node name "-pve2"`},
		{node: "pve2-", err: `node name "pve2-"`},
	}
	for _, tt := range tests {
		t.Run(tt.node, func(t *testing.T) {
			got, err := PinnedTunnelName(buildWriter.InstallID, tt.node)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				require.Empty(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.LessOrEqual(t, len(got), maxLabelLen)
			node, ok := PinnedTunnelNode(buildWriter.InstallID, got)
			require.True(t, ok)
			require.Equal(t, tt.node, node)
		})
	}
}

func TestPinnedTunnelNodeOfOtherNames(t *testing.T) {
	require.Equal(t, tunnelName+"-", PinnedTunnelPrefix(buildWriter.InstallID))
	for _, name := range []string{
		tunnelName,
		tunnelName + "-",
		tunnelName + "-PVE2",
		tunnelName + "-pve.2",
		tunnelName + "-pve2-",
		tunnelName + "_probe_x",
		tunnelName + "-" + strings.Repeat("n", maxLabelLen),
		"pco-3f9a2c1e77b0a-pve2",
		"pco-3f9a2c1e77b-pve2",
		"pco-xyz-pve2",
		"",
	} {
		node, ok := PinnedTunnelNode(buildWriter.InstallID, name)
		require.False(t, ok, "%q is not a tunnel pinned to a node of this install", name)
		require.Empty(t, node)
	}
}

// The hostnames of one guest are placed together: a pinned placement of
// each sends their rules and records to the same tunnel of the node, and the
// shared tunnel serves none of them.
func TestBuildPlacesTheHostnamesOfAGuestInOnePinnedTunnel(t *testing.T) {
	plan := Build(BuildInput{
		Winners: []model.Route{
			winner(t, "app.shop.cz", "qemu/1"),
			winner(t, "*.app.shop.cz", "qemu/1"),
			winner(t, "web.shop.cz", "qemu/2"),
		},
		Targets:    allVerified("app.shop.cz", "*.app.shop.cz", "web.shop.cz"),
		Placements: map[string]PlacementInput{"app.shop.cz": pinned("pve2"), "*.app.shop.cz": pinned("pve2")},
		Zones:      []Zone{shopZone},
		Writer:     buildWriter,
	})

	require.Equal(t, []TunnelPlan{
		{AccountID: "acc1", CredentialID: "cred1", Name: tunnelName, Rules: withSentinel(httpRule("web.shop.cz", originAddr))},
		{
			AccountID: "acc1", CredentialID: "cred1", Name: pinnedTo2, Kind: PlacementPinned, Node: "pve2",
			Rules: withSentinel(httpRule("app.shop.cz", originAddr), httpRule("*.app.shop.cz", originAddr)),
		},
	}, plan.Tunnels)
	require.Equal(t, []RecordPlan{
		pinnedRecordFor(shopZone, "*.app.shop.cz", "pve2"),
		pinnedRecordFor(shopZone, "app.shop.cz", "pve2"),
		recordFor(shopZone, "web.shop.cz"),
	}, plan.Records)
	for _, rt := range plan.Routes {
		require.Equal(t, StateActive, rt.State, rt.Hostname)
	}
}

// Every tunnel of an account ends with the sentinel and the catch-all, and
// carries the account's blocks: a wildcard of another owner may be placed in
// any of them. An account whose routes are all pinned has no shared tunnel.
func TestBuildPinnedPlanPerNode(t *testing.T) {
	plan := Build(BuildInput{
		Winners: []model.Route{
			winner(t, "one.shop.cz", "qemu/1"),
			winner(t, "two.shop.cz", "qemu/2"),
			winner(t, "*.shop.cz", "qemu/3"),
		},
		Claims:  claimed(map[string]string{"held.shop.cz": "qemu/4"}),
		Targets: allVerified("one.shop.cz", "two.shop.cz", "*.shop.cz"),
		Placements: map[string]PlacementInput{
			"one.shop.cz": pinned("pve1"),
			"two.shop.cz": pinned("pve2"),
			"*.shop.cz":   pinned("pve1"),
		},
		Zones:  []Zone{shopZone},
		Writer: buildWriter,
	})

	require.Equal(t, []TunnelPlan{
		{
			AccountID: "acc1", CredentialID: "cred1", Name: tunnelName + "-pve1", Kind: PlacementPinned, Node: "pve1",
			Rules: withSentinel(blockRule("held.shop.cz"), httpRule("one.shop.cz", originAddr), httpRule("*.shop.cz", originAddr)),
		},
		{
			AccountID: "acc1", CredentialID: "cred1", Name: pinnedTo2, Kind: PlacementPinned, Node: "pve2",
			Rules: withSentinel(blockRule("held.shop.cz"), httpRule("two.shop.cz", originAddr)),
		},
	}, plan.Tunnels)
}

// A shared placement, given or absent, keeps a route in the shared tunnel,
// and a withdrawn pinned route keeps its record at its node's tunnel behind
// a block.
func TestBuildSharedPlacementsAndWithdrawnPinnedRoutes(t *testing.T) {
	plan := Build(BuildInput{
		Winners: []model.Route{
			winner(t, "a.shop.cz", "qemu/1"),
			winner(t, "b.shop.cz", "qemu/2"),
			winner(t, "gone.shop.cz", "qemu/3"),
		},
		Targets: map[string]ResolvedTarget{
			"a.shop.cz":    verified(originAddr),
			"b.shop.cz":    verified(originAddr),
			"gone.shop.cz": {Addr: netip.MustParseAddr(originAddr), Withdrawn: true},
		},
		Placements: map[string]PlacementInput{
			"a.shop.cz":    {Kind: PlacementShared},
			"gone.shop.cz": pinned("pve2"),
		},
		Zones:  []Zone{shopZone},
		Writer: buildWriter,
	})

	require.Equal(t, []TunnelPlan{
		{
			AccountID: "acc1", CredentialID: "cred1", Name: tunnelName,
			Rules: withSentinel(httpRule("a.shop.cz", originAddr), httpRule("b.shop.cz", originAddr), blockRule("gone.shop.cz")),
		},
		{
			AccountID: "acc1", CredentialID: "cred1", Name: pinnedTo2, Kind: PlacementPinned, Node: "pve2",
			Rules: withSentinel(blockRule("gone.shop.cz")),
		},
	}, plan.Tunnels)
	require.Equal(t, []RecordPlan{
		recordFor(shopZone, "a.shop.cz"),
		recordFor(shopZone, "b.shop.cz"),
		pinnedRecordFor(shopZone, "gone.shop.cz", "pve2"),
	}, plan.Records)
}

// A placement that names no tunnel this install can have blocks the route
// and says why; nothing is served or published for it.
func TestBuildRefusesAPlacementWithoutATunnel(t *testing.T) {
	tests := []struct {
		name      string
		placement PlacementInput
		reason    string
	}{
		{"node name", pinned("PVE2"), `cannot be pinned to node "PVE2": node name "PVE2": want lower-case letters, digits and hyphens, not at the ends`},
		{"no node", PlacementInput{Kind: PlacementPinned}, `cannot be pinned to node "": node name "": want lower-case letters, digits and hyphens, not at the ends`},
		{"kind", PlacementInput{Kind: "floating", Node: "pve2"}, `placement kind "floating" is not known`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := Build(BuildInput{
				Winners:    []model.Route{winner(t, "app.shop.cz", "qemu/1"), winner(t, "web.shop.cz", "qemu/2")},
				Targets:    allVerified("app.shop.cz", "web.shop.cz"),
				Placements: map[string]PlacementInput{"app.shop.cz": tt.placement},
				Zones:      []Zone{shopZone},
				Writer:     buildWriter,
			})

			require.Equal(t, []TunnelPlan{{
				AccountID: "acc1", CredentialID: "cred1", Name: tunnelName,
				Rules: withSentinel(blockRule("app.shop.cz"), httpRule("web.shop.cz", originAddr)),
			}}, plan.Tunnels)
			require.Equal(t, []RecordPlan{recordFor(shopZone, "web.shop.cz")}, plan.Records)
			require.Equal(t, RouteStatus{
				Hostname: "app.shop.cz", Owner: "qemu/1", State: StateUnreachable, Zone: "shop.cz", Reason: tt.reason,
			}, plan.Routes[0])
		})
	}
}

// The order of the tunnels, and of the rules in each, does not depend on
// the order the placements are given in.
func TestBuildPlacementsAreDeterministic(t *testing.T) {
	hosts := []string{"a.shop.cz", "b.shop.cz", "c.shop.cz", "d.shop.cz", "e.shop.cz", "*.shop.cz"}
	nodes := []string{"pve3", "pve1", "", "pve2", "pve1", "pve3"}
	in := BuildInput{
		Targets:    allVerified(hosts...),
		Placements: map[string]PlacementInput{},
		Zones:      []Zone{shopZone},
		Writer:     buildWriter,
	}
	for i, h := range hosts {
		in.Winners = append(in.Winners, winner(t, h, "qemu/1"))
		if nodes[i] != "" {
			in.Placements[h] = pinned(nodes[i])
		}
	}

	first := Build(in)
	rng := rand.New(rand.NewPCG(2, 7))
	for range 20 {
		shuffle(rng, in.Winners)
		require.Equal(t, first, Build(in))
	}
	require.Equal(t, []string{tunnelName, tunnelName + "-pve1", pinnedTo2, tunnelName + "-pve3"}, tunnelNames(first))
	require.Equal(t, []string{"b.shop.cz", "e.shop.cz", SentinelHostname(buildWriter), ""}, ruleHosts(first.Tunnels[1].Rules))
}

func tunnelNames(plan Plan) []string {
	var out []string
	for _, tp := range plan.Tunnels {
		out = append(out, tp.Name)
	}
	return out
}

// Each tunnel of a move serves what its own nodes judge of the target: the
// tunnel moved to gets its rule while the one the record points at cannot
// serve it yet, and answers for the hostname instead of the block every
// other tunnel has; a second tunnel whose nodes cannot serve it blocks it,
// and both tunnels of a move stay in the plan, so that neither is emptied
// while a record may point at it.
func TestBuildServesEachTunnelOfAMoveByItsOwnTarget(t *testing.T) {
	plan := Build(BuildInput{
		Winners: []model.Route{
			winner(t, "app.shop.cz", "qemu/1"),
			winner(t, "db.shop.cz", "qemu/2"),
		},
		Targets: map[string]ResolvedTarget{
			"app.shop.cz": {Reason: "waiting for pve1 to verify it"},
			"db.shop.cz":  verified(originAddr),
		},
		AlsoTargets: map[string]ResolvedTarget{
			"app.shop.cz": verified(originAddr),
			"db.shop.cz":  {Reason: "waiting for pve1 to verify it"},
		},
		Placements: map[string]PlacementInput{"db.shop.cz": pinned("pve3")},
		Also:       map[string]PlacementInput{"app.shop.cz": pinned("pve3"), "db.shop.cz": {Kind: PlacementShared}},
		Zones:      []Zone{shopZone},
		Writer:     buildWriter,
	})

	require.Equal(t, []TunnelPlan{
		{
			AccountID: "acc1", CredentialID: "cred1", Name: tunnelName,
			Rules: withSentinel(blockRule("app.shop.cz"), blockRule("db.shop.cz")),
		},
		{
			AccountID: "acc1", CredentialID: "cred1", Name: tunnelName + "-pve3", Kind: PlacementPinned, Node: "pve3",
			Rules: withSentinel(httpRule("app.shop.cz", originAddr), httpRule("db.shop.cz", originAddr)),
		},
	}, plan.Tunnels)
	require.Equal(t, []RecordPlan{pinnedRecordFor(shopZone, "db.shop.cz", "pve3")}, plan.Records)
	require.Equal(t, StateUnreachable, plan.Routes[0].State)
	require.Equal(t, "waiting for pve1 to verify it", plan.Routes[0].Reason)
	require.Equal(t, StateActive, plan.Routes[1].State)
}

// Neither tunnel of a move is emptied while the route is served in none:
// the record may point at either, and each answers with the block.
func TestBuildKeepsBothTunnelsOfAMoveThatServesNothing(t *testing.T) {
	plan := Build(BuildInput{
		Winners:     []model.Route{winner(t, "app.shop.cz", "qemu/1")},
		Targets:     map[string]ResolvedTarget{"app.shop.cz": {Reason: "waiting for pve2 to verify it"}},
		AlsoTargets: map[string]ResolvedTarget{"app.shop.cz": {Reason: "waiting for pve3 to verify it"}},
		Placements:  map[string]PlacementInput{"app.shop.cz": pinned("pve2")},
		Also:        map[string]PlacementInput{"app.shop.cz": pinned("pve3")},
		Zones:       []Zone{shopZone},
		Writer:      buildWriter,
	})

	require.Equal(t, []string{tunnelName + "-pve2", tunnelName + "-pve3"}, tunnelNames(plan))
	for _, tp := range plan.Tunnels {
		require.Equal(t, withSentinel(blockRule("app.shop.cz")), tp.Rules, tp.Name)
	}
	require.Empty(t, plan.Records)
}

// A hostname that moves between tunnels has its rule in the tunnel it moves
// from and in the one it moves to, while its record points at its
// placement. A route that is not served has no rule to keep; its block is in
// every tunnel of the account anyway.
func TestBuildKeepsTheRuleOfAMoveInBothTunnels(t *testing.T) {
	plan := Build(BuildInput{
		Winners: []model.Route{
			winner(t, "app.shop.cz", "qemu/1"),
			winner(t, "gone.shop.cz", "qemu/2"),
		},
		Targets: map[string]ResolvedTarget{
			"app.shop.cz":  verified(originAddr),
			"gone.shop.cz": {Addr: netip.MustParseAddr(originAddr), Withdrawn: true},
		},
		Placements: map[string]PlacementInput{"app.shop.cz": pinned("pve3")},
		Also:       map[string]PlacementInput{"app.shop.cz": {Kind: PlacementShared}, "gone.shop.cz": pinned("pve3")},
		Zones:      []Zone{shopZone},
		Writer:     buildWriter,
	})

	require.Equal(t, []TunnelPlan{
		{
			AccountID: "acc1", CredentialID: "cred1", Name: tunnelName,
			Rules: withSentinel(httpRule("app.shop.cz", originAddr), blockRule("gone.shop.cz")),
		},
		{
			AccountID: "acc1", CredentialID: "cred1", Name: tunnelName + "-pve3", Kind: PlacementPinned, Node: "pve3",
			Rules: withSentinel(httpRule("app.shop.cz", originAddr), blockRule("gone.shop.cz")),
		},
	}, plan.Tunnels)
	require.Equal(t, []RecordPlan{
		pinnedRecordFor(shopZone, "app.shop.cz", "pve3"),
		recordFor(shopZone, "gone.shop.cz"),
	}, plan.Records)
}
