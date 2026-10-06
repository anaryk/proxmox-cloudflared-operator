package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTheHostTableGolden(t *testing.T) {
	requireGolden(t, "host.nft", renderHost())
}

// The node takes nothing from the managed bridge and nothing from the
// transit link but the replies to its own flows, forwards nothing a managed guest routes through it but what
// it bridges between two of them, and forwards nothing into the transit link.
func TestTheRulesOfTheHostTable(t *testing.T) {
	script := renderHost()
	require.Equal(t, []string{
		`iifname "pconet" counter name "from_pconet" drop`,
		`iifname "pcotr0" ct state invalid,new counter name "from_transit" drop`,
	}, chainRules(t, script, "input"))
	require.Equal(t, []string{
		`iifname "pconet" oifname != "pconet" counter name "routed_from_pconet" drop`,
		`oifname "pcotr0" counter name "to_transit" drop`,
	}, chainRules(t, script, "forward"))
	require.Equal(t, "type filter hook input priority filter - 10; policy accept;", chainHook(t, script, "input"))
	require.Equal(t, "type filter hook forward priority filter - 10; policy accept;", chainHook(t, script, "forward"))
}

func TestTheListingsOfTheHostTableOfBothVersionsAreTheTable(t *testing.T) {
	for _, v := range []string{"1.0.6", "1.1.3"} {
		l := readListing(t, "host-"+v+".json")
		require.Empty(t, hostDifferences(l), v)
	}
}

func TestAListingOfTheHostTableWithoutARuleNamesIt(t *testing.T) {
	for _, tt := range []struct {
		name, from, to, want string
	}{
		{"input from the managed bridge", `{"counter": "from_pconet"}, {"drop": null}`, `{"counter": "from_pconet"}, {"accept": null}`,
			"chain input of table inet pco_host: rule 1 is not the one pco loads"},
		{"new flows from the transit link", `{"match": {"op": "in", "left": {"ct": {"key": "state"}}, "right": ["invalid", "new"]}}, `, ``,
			"chain input of table inet pco_host: rule 2 is not the one pco loads"},
		{"what is of no flow from the transit link", `"right": ["invalid", "new"]`, `"right": "new"`,
			"chain input of table inet pco_host: rule 2 is not the one pco loads"},
		{"what is bridged between managed guests", `"op": "!=", "left": {"meta": {"key": "oifname"}}`, `"op": "==", "left": {"meta": {"key": "oifname"}}`,
			"chain forward of table inet pco_host: rule 1 is not the one pco loads"},
		{"forwarding into the transit link", `"right": "pcotr0"}}, {"counter": "to_transit"}`, `"right": "pcotr1"}}, {"counter": "to_transit"}`,
			"chain forward of table inet pco_host: rule 2 is not the one pco loads"},
		{"the priority", `"hook": "input", "prio": -10`, `"hook": "input", "prio": 0`,
			"chain input of table inet pco_host is filter hook input priority 0 policy accept, want filter hook input priority -10 policy accept"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "host-1.1.3.json"))
			require.NoError(t, err)
			changed := strings.Replace(string(raw), tt.from, tt.to, 1)
			require.NotEqual(t, string(raw), changed, "the listing has no %s", tt.from)
			l, err := parseListing([]byte(changed))
			require.NoError(t, err)
			require.Contains(t, hostDifferences(l), tt.want)
		})
	}
}
