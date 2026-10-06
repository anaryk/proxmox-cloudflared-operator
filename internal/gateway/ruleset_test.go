package gateway

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	testLegs     = []Leg{legA, legB, legM}
	testServices = []Service{svcB, svcA, svcM}
)

func TestTheBaseTableGolden(t *testing.T) {
	requireGolden(t, "base.nft", renderGateway(snatMap, elementsOf(nil, nil), nil))
}

// Two legs into identical subnets, three services, one with two ports, and
// the managed leg.
func TestTheTableWithLegsAndServicesGolden(t *testing.T) {
	requireGolden(t, "legs.nft", renderGateway(snatMap, elementsOf(testLegs, testServices), testLegs))
}

func TestTheTableWithARuleOfSNATPerLegGolden(t *testing.T) {
	requireGolden(t, "legs-rules.nft", renderGateway(snatRules, elementsOf(testLegs, testServices), testLegs))
}

func TestTheTableIsLoadedInTheMapForm(t *testing.T) {
	require.Equal(t, snatMap, theSNATForm)
}

// Each leg SNATs to its address from its own block of source ports, TCP
// only: the DHCP server's replies keep port 67.
func TestEachLegSNATsFromItsOwnBlockOfPorts(t *testing.T) {
	e := elementsOf(testLegs, testServices)
	require.Equal(t, []string{
		`"leg1" : 10.50.0.250 . 1024-2047`,
		`"leg2" : 10.50.0.250 . 2048-3071`,
		`"leg3" : 10.203.118.1 . 3072-4095`,
	}, e[mapLegSNAT])
	script := renderGateway(snatMap, e, testLegs)
	require.Contains(t, script, "\t\ttype ifname : interval ipv4_addr . inet_service\n\t\tflags interval\n")
	require.Equal(t, []string{"meta l4proto tcp oifname @legs snat ip to oifname map @leg_snat"}, chainRules(t, script, "post_nat"))

	perLeg := renderGateway(snatRules, e, testLegs)
	require.NotContains(t, perLeg, mapLegSNAT)
	require.Equal(t, []string{
		`meta l4proto tcp oifname "leg1" snat ip to 10.50.0.250:1024-2047`,
		`meta l4proto tcp oifname "leg2" snat ip to 10.50.0.250:2048-3071`,
		`meta l4proto tcp oifname "leg3" snat ip to 10.203.118.1:3072-4095`,
	}, chainRules(t, perLeg, "post_nat"))
}

func TestTheElementsOfLegsAndServices(t *testing.T) {
	e := elementsOf(testLegs, append(testServices, Service{Leg: "gone", Service: addr("198.18.1.9"), Addr: addr("10.9.9.9"), Ports: []uint16{80}}))
	require.Equal(t, []string{`"leg1"`, `"leg2"`, `"leg3"`}, e[setLegs])
	require.Equal(t, []string{`"leg3"`}, e[setDHCPLegs], "only the managed leg takes DHCP")
	require.Equal(t, []string{`"leg1" : 1`, `"leg2" : 2`, `"leg3" : 3`}, e[mapLegZone])
	require.Equal(t, []string{`"leg1" . 1`, `"leg2" . 2`, `"leg3" . 3`}, e[setLegOK])
	require.Equal(t, []string{"198.18.1.0 : 1", "198.18.1.1 : 2", "198.18.1.2 : 3"}, e[mapSvcZone])
	require.Equal(t, []string{"198.18.1.0 : 1", "198.18.1.1 : 2", "198.18.1.2 : 3"}, e[mapSvcMark])
	require.Equal(t, []string{
		"198.18.1.0 . 80 : 10.50.0.10 . 80",
		"198.18.1.0 . 443 : 10.50.0.10 . 443",
		"198.18.1.1 . 80 : 10.50.0.10 . 80",
		"198.18.1.2 . 8080 : 10.203.118.5 . 8080",
	}, e[mapSvcDNAT], "one element per port; a service on a leg that is not given has none")
	require.Equal(t, []string{
		`"leg1" . 10.50.0.10 . 80`,
		`"leg1" . 10.50.0.10 . 443`,
		`"leg2" . 10.50.0.10 . 80`,
		`"leg3" . 10.203.118.5 . 8080`,
	}, e[setAllowed])
}

// The guards of the table, each where it holds: the transit link takes only
// the host's address, a leg's ingress gets the leg's zone, a DNATed flow
// leaves only through the leg of its zone, and the input chain drops what a
// leg sends but DHCP on the managed leg, in that order.
func TestTheGuardsOfTheTable(t *testing.T) {
	script := renderGateway(snatMap, nil, nil)
	require.Equal(t, `iifname "transit0" ip saddr != 198.18.0.1 counter name "dropped_transit" drop`, chainRules(t, script, "pre_raw")[0])
	require.Equal(t, `iifname @legs ct zone set iifname map @leg_zone`, chainRules(t, script, "pre_raw")[3])
	require.Equal(t, []string{`ct status dnat ct direction original oifname . ct zone != @leg_ok counter name "dropped_oif" drop`}, chainRules(t, script, "post_filter"))
	require.Equal(t, []string{
		`iif "lo" accept`,
		`ct state established,related accept`,
		`ct state invalid drop`,
		`iifname @dhcp_legs udp dport 67 udp sport 68 accept`,
		`iifname @legs counter name "dropped_leg_in" drop`,
		`counter name "dropped_input"`,
	}, chainRules(t, script, "input"))
	require.Equal(t, `type filter hook forward priority filter; policy drop;`, chainHook(t, script, "forward"))
	require.Equal(t, `type filter hook input priority filter; policy drop;`, chainHook(t, script, "input"))
}

// What is new is added before anything is removed, all in one script; a
// value that changes is deleted and added in the same script.
func TestTheElementScriptAddsBeforeItRemoves(t *testing.T) {
	moved := legB
	moved.Prefix = prefix("10.50.0.251/24")
	have := sortedElements(elementsOf([]Leg{legA, legB}, []Service{svcA, svcB}))
	want := sortedElements(elementsOf([]Leg{moved, legM}, []Service{svcB, svcM}))
	script := elementScript(have, want)
	lines := strings.Split(strings.TrimSpace(script), "\n")
	lastAdd, firstDelete := -1, len(lines)
	for i, l := range lines {
		require.True(t, strings.HasPrefix(l, "add element inet pco_gw ") || strings.HasPrefix(l, "delete element inet pco_gw "), l)
		if strings.HasPrefix(l, "add element") && !strings.Contains(l, mapLegSNAT) {
			lastAdd = i
		}
		if strings.HasPrefix(l, "delete element") && !strings.Contains(l, mapLegSNAT) && firstDelete == len(lines) {
			firstDelete = i
		}
	}
	require.Less(t, lastAdd, firstDelete, script)
	require.Contains(t, script, "add element inet pco_gw legs { \"leg3\" }\n")
	require.Contains(t, script, "delete element inet pco_gw leg_snat { \"leg2\" }\nadd element inet pco_gw leg_snat { \"leg2\" : 10.50.0.251 . 2048-3071 }\n")
	require.Contains(t, script, "delete element inet pco_gw svc_dnat { 198.18.1.0 . 443, 198.18.1.0 . 80 }\n")
	require.Contains(t, script, "delete element inet pco_gw legs { \"leg1\" }\n")
	require.Empty(t, elementScript(want, want), "nothing changes")
}

func TestTheListingsOfBothVersionsAreTheTable(t *testing.T) {
	for _, v := range []string{"1.0.6", "1.1.3"} {
		t.Run(v, func(t *testing.T) {
			l := readListing(t, "listing-legs-"+v+".json")
			require.Empty(t, gwDifferences(l, snatMap, testLegs))
			require.Equal(t, sortedElements(elementsOf(testLegs, testServices)), listedElements(l))
			require.Empty(t, elementDifferences(listedElements(l), sortedElements(elementsOf(testLegs, testServices))))

			base := readListing(t, "listing-"+v+".json")
			require.Empty(t, gwDifferences(base, snatMap, nil))
			require.Empty(t, listedElements(base))
		})
	}
}

func TestAListingOfTheRuleOfSNATPerLegIsTheTable(t *testing.T) {
	for _, v := range []string{"1.0.6", "1.1.3"} {
		l := readListing(t, "listing-rules-"+v+".json")
		require.Empty(t, gwDifferences(l, snatRules, testLegs), v)
		require.NotEmpty(t, gwDifferences(l, snatRules, testLegs[:2]), v)
		require.NotEmpty(t, gwDifferences(l, snatMap, testLegs), v)
	}
}

// A listing without one guard, or with a chain, set or counter changed, is a
// difference that names it.
func TestAListingThatDiffersNamesWhat(t *testing.T) {
	for _, tt := range []struct {
		name, from, to, want string
	}{
		{"the oif guard", `{"match": {"op": "!=", "left": {"concat": [{"meta": {"key": "oifname"}}, {"ct": {"key": "zone"}}]}, "right": "@leg_ok"}}, `, ``,
			"chain post_filter of table inet pco_gw: rule 1 is not the one pco loads"},
		{"the zone of a leg's ingress", `"data": "@leg_zone"`, `"data": "@svc_zone"`,
			"chain pre_raw of table inet pco_gw: rule 4 is not the one pco loads"},
		{"the drop of input from a leg", `{"counter": "dropped_leg_in"}, {"drop": null}`, `{"counter": "dropped_leg_in"}, {"accept": null}`,
			"chain input of table inet pco_gw: rule 5 is not the one pco loads"},
		{"the source check of the transit link", `"right": "198.18.0.1"}}, {"counter": "dropped_transit"}`, `"right": "198.18.0.9"}}, {"counter": "dropped_transit"}`,
			"chain pre_raw of table inet pco_gw: rule 1 is not the one pco loads"},
		{"a policy", `"hook": "forward", "prio": 0, "policy": "drop"`, `"hook": "forward", "prio": 0, "policy": "accept"`,
			"chain forward of table inet pco_gw is filter hook forward priority 0 policy accept, want filter hook forward priority 0 policy drop"},
		{"the SNAT map", `"name": "leg_snat"`, `"name": "leg_snat2"`, "table inet pco_gw has no set leg_snat"},
		{"a counter", `"name": "dropped_oif"`, `"name": "dropped_other"`, "table inet pco_gw has no counter dropped_oif"},
		{"a dormant table", `"name": "pco_gw", "handle"`, `"name": "pco_gw", "flags": ["dormant"], "handle"`, "table inet pco_gw has flags dormant"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "listing-1.1.3.json"))
			require.NoError(t, err)
			changed := strings.Replace(string(raw), tt.from, tt.to, 1)
			require.NotEqual(t, string(raw), changed, "the listing has no %s", tt.from)
			l, err := parseListing([]byte(changed))
			require.NoError(t, err)
			require.Contains(t, gwDifferences(l, snatMap, nil), tt.want)
		})
	}
}

func TestAnElementOfAListingThatIsNotWantedIsADifference(t *testing.T) {
	l := readListing(t, "listing-legs-1.1.3.json")
	d := elementDifferences(listedElements(l), sortedElements(elementsOf(testLegs, []Service{svcA, svcB})))
	require.Equal(t, []string{
		"svc_zone of pco_gw holds 198.18.1.2 : 3",
		"svc_mark of pco_gw holds 198.18.1.2 : 3",
		"svc_dnat of pco_gw holds 198.18.1.2 . 8080 : 10.203.118.5 . 8080",
		`allowed of pco_gw holds "leg3" . 10.203.118.5 . 8080`,
	}, d)
}

func TestAListingThatIsNoListingIsUnreadable(t *testing.T) {
	for _, raw := range []string{"", "{}", `{"nftables": [{"map": {"name": "x", "elem": [{"prefix": {"addr": "10.0.0.0", "len": 8}}]}}]}`} {
		_, err := parseListing([]byte(raw))
		require.ErrorIs(t, err, errUnreadable, raw)
	}
}

func readListing(t *testing.T, name string) *listing {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	l, err := parseListing(raw)
	require.NoError(t, err)
	return l
}

// chainRules returns the rules of a chain of a script, without the hook.
func chainRules(t *testing.T, script, chain string) []string {
	t.Helper()
	_, block, ok := strings.Cut(script, "\tchain "+chain+" {\n")
	require.True(t, ok, "no chain %s", chain)
	block, _, _ = strings.Cut(block, "\n\t}\n")
	var out []string
	for i, line := range strings.Split(block, "\n") {
		if i > 0 {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return slices.DeleteFunc(out, func(s string) bool { return s == "" })
}

func chainHook(t *testing.T, script, chain string) string {
	t.Helper()
	_, block, ok := strings.Cut(script, "\tchain "+chain+" {\n")
	require.True(t, ok, "no chain %s", chain)
	line, _, _ := strings.Cut(block, "\n")
	return strings.TrimSpace(line)
}
