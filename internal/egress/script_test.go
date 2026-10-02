package egress

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBaseGolden(t *testing.T) {
	requireGolden(t, "base.nft", Base(testUID, nil, nil))
}

func TestTheBootTableHoldsTheResolversLessTheBlocked(t *testing.T) {
	resolvers := []netip.Addr{addr("192.168.1.1"), addr("fd00::53"), addr("10.0.0.9"), addr("192.168.1.1"), addr("fe80::1%vmbr0")}

	requireGolden(t, "boot.nft", Base(testUID, resolvers, []netip.Addr{addr("10.0.0.9")}))
}

func TestScriptGolden(t *testing.T) {
	tests := []struct {
		name      string
		golden    string
		targets   []Target
		resolvers []netip.Addr
		blocked   []netip.Addr
	}{
		{name: "no target", golden: "none.nft", resolvers: []netip.Addr{addr("192.168.1.1")}},
		{name: "one target", golden: "one.nft", targets: targets("10.0.0.5:80")},
		{
			name:   "several targets",
			golden: "several.nft",
			// Out of order, twice the same, and one that is blocked.
			targets:   targets("10.0.0.6:443", "10.0.0.5:8080", "10.0.0.9:22", "10.0.0.5:80", "10.0.0.6:443"),
			resolvers: []netip.Addr{addr("192.168.1.1")},
			blocked:   []netip.Addr{addr("10.0.0.9")},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, n, r, ov := newTestFilter(t)
			r.set(tc.resolvers...)
			for _, a := range tc.blocked {
				_, err := ov.Block(a)
				require.NoError(t, err)
			}

			require.NoError(t, f.Set(t.Context(), tc.targets))

			require.Len(t, n.applied(), 1)
			requireGolden(t, tc.golden, n.applied()[0])
		})
	}
}

func TestEveryScriptReplacesTheWholeTableInOneTransaction(t *testing.T) {
	for _, script := range []string{Base(testUID, nil, nil), render(testUID, contents{targets: targets("10.0.0.5:80")})} {
		lines := strings.SplitN(script, "\n", 4)
		// add makes the delete work when there is no table yet; the delete
		// takes whatever is there, chains, sets and elements of others
		// included, which a flush would leave.
		require.Equal(t, []string{"add table inet pco_egress", "delete table inet pco_egress", "table inet pco_egress {"}, lines[:3])
		require.Equal(t, 1, strings.Count(script, "delete table"))
	}
}

func TestAnIPv6TargetGoesIntoItsOwnSet(t *testing.T) {
	script := render(testUID, contents{
		targets:   targets("10.0.0.5:80", "[fd00::5]:8080"),
		resolvers: []netip.Addr{addr("fd00::53")},
	})

	require.Contains(t, script, "\tset targets6 {\n\t\ttype ipv6_addr . inet_service\n\t\telements = {\n\t\t\tfd00::5 . 8080\n\t\t}\n\t}\n")
	require.Contains(t, script, "\tset resolvers6 {\n\t\ttype ipv6_addr\n\t\telements = {\n\t\t\tfd00::53\n\t\t}\n\t}\n")
	require.Contains(t, script, "\tset targets4 {\n\t\ttype ipv4_addr . inet_service\n\t\telements = {\n\t\t\t10.0.0.5 . 80\n\t\t}\n\t}\n")
}

func TestOnlyTheConnectorUserIsSentToTheFilter(t *testing.T) {
	script := Base(4242, nil, nil)

	require.Contains(t, script, "\t\ttype filter hook output priority filter - 10; policy accept;\n\t\tmeta skuid 4242 jump connector\n\t}\n")
	require.Equal(t, 1, strings.Count(script, "skuid"))
}

func TestTheDeleteScriptNamesEachElement(t *testing.T) {
	got := deleteScript(targets("10.0.0.5:80", "10.0.0.5:443", "[fd00::5]:80"), nil)

	require.Equal(t, "delete element inet pco_egress targets4 { 10.0.0.5 . 80, 10.0.0.5 . 443 }\n"+
		"delete element inet pco_egress targets6 { fd00::5 . 80 }\n", got)
	require.Equal(t, "delete element inet pco_egress resolvers4 { 10.0.0.53 }\n",
		deleteScript(nil, []netip.Addr{addr("10.0.0.53")}))
}
