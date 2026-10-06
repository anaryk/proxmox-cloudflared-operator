package planner

import (
	"encoding/json"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

const (
	realAddr    = "10.50.0.10"
	serviceAddr = "198.18.1.5"
)

func onLeg(via string) ResolvedTarget {
	t := verified(realAddr)
	t.Leg = true
	if via != "" {
		t.Via = netip.MustParseAddr(via)
	}
	return t
}

func TestBuildSendsALegTargetToItsServiceAddress(t *testing.T) {
	plan := Build(BuildInput{
		Winners: []model.Route{winner(t, "a.shop.cz", "qemu/1"), winner(t, "b.shop.cz", "qemu/2")},
		Targets: map[string]ResolvedTarget{"a.shop.cz": onLeg(serviceAddr), "b.shop.cz": verified(realAddr)},
		Zones:   []Zone{shopZone},
		Writer:  buildWriter,
	})

	require.Equal(t, withSentinel(httpRule("a.shop.cz", serviceAddr), httpRule("b.shop.cz", realAddr)), plan.Tunnels[0].Rules)
	require.Equal(t, "http://198.18.1.5:8080", plan.Routes[0].Service)
}

// A target on a leg has no address the connector may dial until it has a
// service address: it is blocked as one never verified, and its real
// address is in no rule.
func TestBuildBlocksALegTargetWithoutAServiceAddress(t *testing.T) {
	target := onLeg("")
	target.Reason = "service address not allocated: no cycle has run since the start"
	plan := Build(BuildInput{
		Winners: []model.Route{winner(t, "a.shop.cz", "qemu/1")},
		Targets: map[string]ResolvedTarget{"a.shop.cz": target},
		Zones:   []Zone{shopZone},
		Writer:  buildWriter,
	})

	require.Empty(t, plan.Tunnels, "a tunnel of nothing but 503 rules is reached by no record")
	require.Empty(t, plan.Records)
	require.Equal(t, []RouteStatus{{
		Hostname: "a.shop.cz", Owner: "qemu/1", State: StateUnreachable, Zone: "shop.cz",
		Reason: "service address not allocated: no cycle has run since the start",
	}}, plan.Routes)
}

func TestBuildSaysWhyALegTargetWithoutAReasonIsNotServed(t *testing.T) {
	plan := Build(BuildInput{
		Winners: []model.Route{winner(t, "a.shop.cz", "qemu/1")},
		Targets: map[string]ResolvedTarget{"a.shop.cz": onLeg("")},
		Zones:   []Zone{shopZone},
		Writer:  buildWriter,
	})

	require.Equal(t, StateUnreachable, plan.Routes[0].State)
	require.Equal(t, "service address not allocated", plan.Routes[0].Reason)
}

func TestNewRuleRefusesALegTargetWithoutAServiceAddress(t *testing.T) {
	rt := winner(t, "a.shop.cz", "qemu/1")

	_, err := newRule(rt, onLeg(""))
	require.Error(t, err)

	rule, err := newRule(rt, onLeg(serviceAddr))
	require.NoError(t, err)
	require.Equal(t, httpRule("a.shop.cz", serviceAddr), rule)

	rule, err = newRule(rt, verified(realAddr))
	require.NoError(t, err)
	require.Equal(t, httpRule("a.shop.cz", realAddr), rule)
}

func TestTheDialedAddressOfATarget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target ResolvedTarget
		want   string
	}{
		{"a direct target is dialed at its address", verified(realAddr), realAddr},
		{"a leg target at its service address", onLeg(serviceAddr), serviceAddr},
		{"a leg target without one at none", onLeg(""), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.target.Dialed()
			require.Equal(t, tc.want != "", ok)
			if ok {
				require.Equal(t, netip.MustParseAddr(tc.want), got)
			}
		})
	}
}

func TestALegTargetInJSON(t *testing.T) {
	for _, tc := range []struct {
		target ResolvedTarget
		want   string
	}{
		{onLeg(serviceAddr), `{"addr": "10.50.0.10", "reachable": true, "leg": true, "via": "198.18.1.5"}`},
		{verified(realAddr), `{"addr": "10.50.0.10", "reachable": true}`},
	} {
		data, err := json.Marshal(tc.target)
		require.NoError(t, err)
		require.JSONEq(t, tc.want, string(data))
	}
}

// A fallback from the service address to the real one would send a
// connector past the gateway to an address the node does not reach. No
// source of the module takes one for the other.
func TestNothingFallsBackFromTheServiceAddressToTheRealOne(t *testing.T) {
	fallback := regexp.MustCompile(`cmp\.Or\([^()]*\bVia\b[^()]*\bAddr\b|cmp\.Or\([^()]*\bAddr\b[^()]*\bVia\b`)
	root := filepath.Join("..", "..")
	searched := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")) && path != root:
			return filepath.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go"):
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		searched++
		require.NotRegexp(t, fallback, string(data), "%s takes the real address of a target for its service address", path)
		return nil
	})
	require.NoError(t, err)
	require.Greater(t, searched, 100, "the search must cover the module")
}
