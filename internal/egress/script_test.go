package egress

import (
	"net/netip"
	"slices"
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
		"ip daddr @blocked4 meta l4proto tcp reject with tcp reset",
		"ip daddr @blocked4 reject with icmpx admin-prohibited",
		"ip6 daddr @blocked6 meta l4proto tcp reject with tcp reset",
		"ip6 daddr @blocked6 reject with icmpx admin-prohibited",
	}, rules[2:6], "before any rule that accepts, the edge and DNS over TLS among them")
}

func TestEveryRejectAnswersTCPWithAReset(t *testing.T) {
	rules := rulesOf(t, Base(testUID, nil, nil))

	var rejects []string
	for _, r := range rules {
		if strings.Contains(r, "reject") {
			rejects = append(rejects, r)
		}
	}
	require.Equal(t, []string{
		"ip daddr @blocked4 meta l4proto tcp reject with tcp reset",
		"ip daddr @blocked4 reject with icmpx admin-prohibited",
		"ip6 daddr @blocked6 meta l4proto tcp reject with tcp reset",
		"ip6 daddr @blocked6 reject with icmpx admin-prohibited",
		`fib daddr type local meta l4proto tcp counter name "rejected_local" reject with tcp reset`,
		`fib daddr type local counter name "rejected_local" reject with icmpx admin-prohibited`,
		`meta l4proto tcp counter name "rejected" reject with tcp reset`,
		`counter name "rejected" reject with icmpx admin-prohibited`,
	}, rejects, "a reset fails a connect at once, for IPv6 too; both counters count both")
	require.Equal(t, `counter name "rejected" reject with icmpx admin-prohibited`, rules[len(rules)-1])
}

func TestTheDeleteScriptNamesEachElement(t *testing.T) {
	tg := targets("10.0.0.5:80", "10.0.0.5:443", "[fd00::5]:80")
	got := deleteScript(tg, tg, nil)

	require.Equal(t, "delete element inet pco_egress targets4 { 10.0.0.5 . 80, 10.0.0.5 . 443 }\n"+
		"delete element inet pco_egress targets6 { fd00::5 . 80 }\n"+
		"delete element inet pco_egress flows4 { 10.0.0.5 . 80, 10.0.0.5 . 443 }\n"+
		"delete element inet pco_egress flows6 { fd00::5 . 80 }\n", got)
	require.Equal(t, "delete element inet pco_egress resolvers4 { 10.0.0.53 }\n",
		deleteScript(nil, nil, []netip.Addr{addr("10.0.0.53")}))
	require.Equal(t, "delete element inet pco_egress targets4 { 10.0.0.5 . 80 }\n",
		deleteScript(targets("10.0.0.5:80"), nil, nil), "what the counting sets do not hold is not deleted from them")
}

// The counting sets hold every target, of allowNode or not, and count the
// connections opened to each: the statement counter in their body gives every
// element a counter of its own.
func TestTheCountingSetsHoldEveryTargetWithACounter(t *testing.T) {
	script := render(testUID, contents{targets: []Target{
		nodeTarget("10.0.0.2:8006"), target("10.0.0.5:80"), target("10.0.0.5:443"), nodeTarget("[fd00::2]:8006"), target("[fd00::5]:80"),
	}})

	require.Contains(t, script, "\tset flows4 {\n\t\ttype ipv4_addr . inet_service\n\t\tcounter\n\t\telements = {\n"+
		"\t\t\t10.0.0.2 . 8006,\n\t\t\t10.0.0.5 . 80,\n\t\t\t10.0.0.5 . 443\n\t\t}\n\t}\n")
	require.Contains(t, script, "\tset flows6 {\n\t\ttype ipv6_addr . inet_service\n\t\tcounter\n\t\telements = {\n"+
		"\t\t\tfd00::2 . 8006,\n\t\t\tfd00::5 . 80\n\t\t}\n\t}\n")
	require.Equal(t, []string{"10.0.0.5 . 80", "10.0.0.5 . 443"}, elementsOf(t, script, setTargets4), "the target sets are as they were")
	require.Equal(t, []string{"10.0.0.2 . 8006"}, elementsOf(t, script, setAllowNode4))
	require.Contains(t, Base(testUID, nil, nil), "\tset flows4 {\n\t\ttype ipv4_addr . inet_service\n\t\tcounter\n\t}\n")
	require.Contains(t, Base(testUID, nil, nil), "\tset flows6 {\n\t\ttype ipv6_addr . inet_service\n\t\tcounter\n\t}\n")
	require.Equal(t, 2, strings.Count(script, "\t\tcounter\n"), "no other set counts")
}

// Only the first packet of a connection is new, so the counting rules count
// connections; they come before every accept of a target and decide nothing,
// and the rules that decide are the ones the chain had without them, in their
// order.
func TestTheCountingRulesComeRightAfterTheBlockedRulesAndDecideNothing(t *testing.T) {
	rules := rulesOf(t, Base(testUID, nil, nil))

	counting := []string{
		"meta l4proto tcp ct state new ip daddr . tcp dport @flows4",
		"meta l4proto tcp ct state new ip6 daddr . tcp dport @flows6",
	}
	require.Equal(t, counting, rules[6:8])
	require.Equal(t, verdictRules, slices.Concat(rules[:6], rules[8:]))
	for _, r := range counting {
		for _, verdict := range []string{"accept", "drop", "reject", "jump", "goto", "return", "queue"} {
			require.NotContains(t, r, verdict)
		}
	}
}

// verdictRules are the rules of the connector chain that decide, in order.
var verdictRules = []string{
	"ct state invalid drop",
	"ct direction reply meta l4proto tcp fib daddr type local accept",
	"ip daddr @blocked4 meta l4proto tcp reject with tcp reset",
	"ip daddr @blocked4 reject with icmpx admin-prohibited",
	"ip6 daddr @blocked6 meta l4proto tcp reject with tcp reset",
	"ip6 daddr @blocked6 reject with icmpx admin-prohibited",
	"ip daddr @resolvers4 meta l4proto { tcp, udp } th dport 53 accept",
	"ip6 daddr @resolvers6 meta l4proto { tcp, udp } th dport 53 accept",
	"ip daddr . tcp dport @allownode4 accept",
	"ip6 daddr . tcp dport @allownode6 accept",
	`fib daddr type local meta l4proto tcp counter name "rejected_local" reject with tcp reset`,
	`fib daddr type local counter name "rejected_local" reject with icmpx admin-prohibited`,
	"ip daddr . tcp dport @targets4 accept",
	"ip6 daddr . tcp dport @targets6 accept",
	"ip daddr != { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16, 100.64.0.0/10, 127.0.0.0/8, 198.18.0.0/15, 0.0.0.0/8, 192.0.0.0/24, 192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, 224.0.0.0/3 } fib daddr type unicast meta l4proto { tcp, udp } th dport 7844 accept",
	"ip6 daddr != { fc00::/7, fe80::/10, ::1, 64:ff9b::/96, 64:ff9b:1::/48, 2002::/16, 2001::/32, ff00::/8 } fib daddr type unicast meta l4proto { tcp, udp } th dport 7844 accept",
	"ip daddr { 1.1.1.1, 1.0.0.1 } tcp dport 853 accept",
	`meta l4proto tcp counter name "rejected" reject with tcp reset`,
	`counter name "rejected" reject with icmpx admin-prohibited`,
}
