package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTheAppliancesEmptyTableGolden(t *testing.T) {
	requireGolden(t, "container-base.nft", renderContainer(ctElementsOf(nil, nil)))
}

// Two legs into identical subnets, three services, one with two ports, and
// the managed leg.
func TestTheAppliancesTableWithLegsAndServicesGolden(t *testing.T) {
	requireGolden(t, "container-legs.nft", renderContainer(ctElementsOf(testLegs, testServices)))
}

// The hooks of the appliance's table: the DNAT in the output path at the
// NAT hook, before the service prefix's table and the egress filter; the
// zones and marks before conntrack; the guards in postrouting and input.
func TestTheHooksOfTheAppliancesTable(t *testing.T) {
	script := renderContainer(nil)
	for chain, hook := range map[string]string{
		"out_raw":     "type filter hook output priority raw; policy accept;",
		"out_nat":     "type nat hook output priority dstnat; policy accept;",
		"out_guard":   "type filter hook output priority filter; policy accept;",
		"post_filter": "type filter hook postrouting priority filter; policy accept;",
		"post_nat":    "type nat hook postrouting priority srcnat; policy accept;",
		"pre_raw":     "type filter hook prerouting priority raw; policy accept;",
		"input":       "type filter hook input priority filter; policy accept;",
		"forward":     "type filter hook forward priority filter; policy drop;",
	} {
		require.Equal(t, hook, chainHook(t, script, chain), chain)
	}
	require.Equal(t, []string{
		"ip daddr 198.18.0.0/16 ct zone set ip daddr map @svc_zone",
		"ip daddr 198.18.0.0/16 meta mark set ip daddr map @svc_mark",
		"oifname @legs ct zone set oifname map @leg_zone",
	}, chainRules(t, script, "out_raw"))
	require.Equal(t, []string{"ip daddr 198.18.0.0/16 meta l4proto tcp dnat ip to ip daddr . tcp dport map @svc_dnat"}, chainRules(t, script, "out_nat"))
	require.Equal(t, []string{"meta l4proto tcp oifname @legs snat ip to oifname map @leg_snat"}, chainRules(t, script, "post_nat"))
	require.Empty(t, chainRules(t, script, "forward"))
	require.NotContains(t, script, setAllowed, "nothing is forwarded")
}

// The guards of the appliance's table, each where it holds: a DNATed flow to
// an address of the appliance itself is dropped; a DNATed flow leaves only
// through the leg of its zone; a leg's ingress gets the leg's zone; the input
// chain drops what a leg sends but replies and DHCP on the managed leg, in
// that order, and leaves net0 alone.
func TestTheGuardsOfTheAppliancesTable(t *testing.T) {
	script := renderContainer(nil)
	require.Equal(t, []string{`ct status dnat fib daddr type local counter name "dropped_local" drop`}, chainRules(t, script, "out_guard"))
	require.Equal(t, []string{`ct status dnat oifname . ct zone != @leg_ok counter name "dropped_oif" drop`}, chainRules(t, script, "post_filter"))
	require.Equal(t, []string{`iifname @legs ct zone set iifname map @leg_zone`}, chainRules(t, script, "pre_raw"))
	require.Equal(t, []string{
		`iif "lo" accept`,
		`ct state established,related accept`,
		`iifname @legs ct state invalid drop`,
		`iifname @dhcp_legs udp dport 67 udp sport 68 accept`,
		`iifname @legs counter name "dropped_leg_in" drop`,
	}, chainRules(t, script, "input"))
}

// Each leg SNATs from its own block of ports in the appliance too, TCP only.
func TestEachLegOfTheApplianceSNATsFromItsOwnBlock(t *testing.T) {
	e := ctElementsOf(testLegs, testServices)
	require.Equal(t, []string{snatElement(legA), snatElement(legB), snatElement(legM)}, e[mapLegSNAT])
	for m := 1; m < MaxMark; m++ {
		_, hi := PortBlock(m)
		lo, _ := PortBlock(m + 1)
		require.Less(t, hi, lo, "the blocks of marks %d and %d are disjoint", m, m+1)
	}
	require.NotContains(t, e, setAllowed)
	require.Equal(t, []string{`"leg3"`}, e[setDHCPLegs])
	require.Contains(t, renderContainer(e), "\t\ttype ifname : interval ipv4_addr . inet_service\n\t\tflags interval\n")
}

func TestTheListingsOfTheAppliancesTableAreTheTable(t *testing.T) {
	l := readListing(t, "container-listing-legs-1.1.3.json")
	require.Empty(t, ctDifferences(l))
	require.Equal(t, sortedElements(ctElementsOf(testLegs, testServices)), listedElements(l))

	base := readListing(t, "container-listing-1.1.3.json")
	require.Empty(t, ctDifferences(base))
	require.Empty(t, listedElements(base))

	require.NotEmpty(t, ctDifferences(readListing(t, "listing-1.1.3.json")), "the host's table is not the appliance's")
	require.NotEmpty(t, gwDifferences(base, snatMap, nil), "nor the other way")
}

// A listing without one guard, or with a hook, set or counter changed, is a
// difference that names it.
func TestAListingOfTheAppliancesTableThatDiffersNamesWhat(t *testing.T) {
	for _, tt := range []struct {
		name, from, to, want string
	}{
		{"the local guard", `{"match": {"op": "==", "left": {"fib": {"result": "type", "flags": ["daddr"]}}, "right": "local"}}, `, ``,
			"chain out_guard of table inet pco_gw: rule 1 is not the one pco loads"},
		{"the oif guard", `{"match": {"op": "!=", "left": {"concat": [{"meta": {"key": "oifname"}}, {"ct": {"key": "zone"}}]}, "right": "@leg_ok"}}, `, ``,
			"chain post_filter of table inet pco_gw: rule 1 is not the one pco loads"},
		{"the zone of a leg's ingress", `"data": "@leg_zone"}}}}]}}, {"rule": {"family": "inet", "table": "pco_gw", "chain": "input"`,
			`"data": "@svc_zone"}}}}]}}, {"rule": {"family": "inet", "table": "pco_gw", "chain": "input"`,
			"chain pre_raw of table inet pco_gw: rule 1 is not the one pco loads"},
		{"the drop of input from a leg", `{"counter": "dropped_leg_in"}, {"drop": null}`, `{"counter": "dropped_leg_in"}, {"accept": null}`,
			"chain input of table inet pco_gw: rule 5 is not the one pco loads"},
		{"the DNAT after the egress filter", `"hook": "output", "prio": -100`, `"hook": "output", "prio": 10`,
			"chain out_nat of table inet pco_gw is nat hook output priority 10 policy accept, want nat hook output priority -100 policy accept"},
		{"an input that drops what net0 sends", `"hook": "input", "prio": 0, "policy": "accept"`, `"hook": "input", "prio": 0, "policy": "drop"`,
			"chain input of table inet pco_gw is filter hook input priority 0 policy drop, want filter hook input priority 0 policy accept"},
		{"the SNAT map", `"name": "leg_snat"`, `"name": "leg_snat2"`, "table inet pco_gw has no set leg_snat"},
		{"a counter", `"name": "dropped_local"`, `"name": "dropped_other"`, "table inet pco_gw has no counter dropped_local"},
		{"a dormant table", `"name": "pco_gw", "handle"`, `"name": "pco_gw", "flags": ["dormant"], "handle"`, "table inet pco_gw has flags dormant"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "container-listing-1.1.3.json"))
			require.NoError(t, err)
			changed := strings.Replace(string(raw), tt.from, tt.to, 1)
			require.NotEqual(t, string(raw), changed, "the listing has no %s", tt.from)
			l, err := parseListing([]byte(changed))
			require.NoError(t, err)
			require.Contains(t, ctDifferences(l), tt.want)
		})
	}
}
