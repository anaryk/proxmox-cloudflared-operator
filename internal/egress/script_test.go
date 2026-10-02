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

// TestTheTableOfTheListingsGolden writes the table the listings of testdata
// were printed for.
func TestTheTableOfTheListingsGolden(t *testing.T) {
	requireGolden(t, "listed.nft", render(testUID, contents{
		targets:   targets("10.0.0.5:80", "10.0.0.5:8080", "10.0.0.6:443"),
		resolvers: []netip.Addr{addr("192.168.1.1")},
	}))
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

// rulesOf returns the rules of the connector chain of a script, in order.
func rulesOf(t *testing.T, script string) []string {
	t.Helper()
	_, chain, ok := strings.Cut(script, "\tchain connector {\n")
	require.True(t, ok)
	chain, _, _ = strings.Cut(chain, "\n\t}\n")
	var rules []string
	for line := range strings.Lines(chain) {
		rules = append(rules, strings.TrimSpace(line))
	}
	return rules
}

func TestOnlyLocalTCPGetsAnswersThroughTheReplyRule(t *testing.T) {
	rules := rulesOf(t, Base(testUID, nil, nil))

	require.Equal(t, "ct direction reply meta l4proto tcp fib daddr type local accept", rules[1],
		"answers of the metrics scrape, and no flow that a datagram from elsewhere seeded")
	require.Equal(t, 1, strings.Count(strings.Join(rules, "\n"), "ct direction"))
}

func TestTheEdgeRulesAdmitOnlyUnicastToThePublicInternet(t *testing.T) {
	var edge []string
	for _, r := range rulesOf(t, Base(testUID, nil, nil)) {
		if strings.Contains(r, "dport 7844") {
			edge = append(edge, r)
		}
	}

	require.Len(t, edge, 2)
	for _, r := range edge {
		require.Contains(t, r, " fib daddr type unicast meta l4proto { tcp, udp } th dport 7844 accept")
	}
	excluded := func(r string) []string {
		set, _, _ := strings.Cut(strings.SplitN(r, "{ ", 2)[1], " }")
		return strings.Split(set, ", ")
	}
	require.ElementsMatch(t, []string{
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10", "127.0.0.0/8",
		"198.18.0.0/15", "0.0.0.0/8", "192.0.0.0/24", "192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3",
	}, excluded(edge[0]))
	require.ElementsMatch(t, []string{
		"fc00::/7", "fe80::/10", "::1", "64:ff9b::/96", "64:ff9b:1::/48", "2002::/16", "2001::/32", "ff00::/8",
	}, excluded(edge[1]))
	require.True(t, strings.HasPrefix(edge[0], "ip daddr != { "))
	require.True(t, strings.HasPrefix(edge[1], "ip6 daddr != { "))
}

func TestABlockedAddressIsRejectedForEveryPortRightAfterTheReplyRule(t *testing.T) {
	script := Base(testUID, []netip.Addr{addr("10.0.0.9"), addr("192.168.1.1")}, []netip.Addr{addr("fd00::9"), addr("10.0.0.9")})

	require.Equal(t, []string{"10.0.0.9"}, elementsOf(t, script, setBlocked4))
	require.Equal(t, []string{"fd00::9"}, elementsOf(t, script, setBlocked6))
	require.Equal(t, []string{"192.168.1.1"}, elementsOf(t, script, setResolvers4), "and out of the other sets")
	rules := rulesOf(t, script)
	require.Equal(t, []string{
		"ip daddr @blocked4 reject with icmpx admin-prohibited",
		"ip6 daddr @blocked6 reject with icmpx admin-prohibited",
	}, rules[2:4], "before any rule that accepts, the edge and DNS over TLS among them")
}

func TestTheDeleteScriptNamesEachElement(t *testing.T) {
	got := deleteScript(targets("10.0.0.5:80", "10.0.0.5:443", "[fd00::5]:80"), nil)

	require.Equal(t, "delete element inet pco_egress targets4 { 10.0.0.5 . 80, 10.0.0.5 . 443 }\n"+
		"delete element inet pco_egress targets6 { fd00::5 . 80 }\n", got)
	require.Equal(t, "delete element inet pco_egress resolvers4 { 10.0.0.53 }\n",
		deleteScript(nil, []netip.Addr{addr("10.0.0.53")}))
}
