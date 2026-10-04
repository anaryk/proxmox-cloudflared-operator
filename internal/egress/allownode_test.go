package egress

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// nodeTarget is the target of a manual route with allowNode: an address of
// the node.
func nodeTarget(s string) Target {
	t := target(s)
	t.AllowNode = true
	return t
}

func ruleIndex(t *testing.T, rules []string, rule string) int {
	t.Helper()
	i := slices.Index(rules, rule)
	require.GreaterOrEqual(t, i, 0, "no rule %q", rule)
	return i
}

// A verified target that becomes an address of the node, as when a virtual
// address fails over to it, must not be reachable: the addresses of the node
// are rejected before the targets are accepted. Only the targets of manual
// routes with allowNode, which root wrote, and the resolvers come before.
func TestTheAddressesOfTheNodeAreRejectedBeforeTheTargets(t *testing.T) {
	rules := rulesOf(t, render(testUID, contents{
		targets:   []Target{target("127.0.0.1:8006"), nodeTarget("10.0.0.2:8006")},
		resolvers: []netip.Addr{addr("127.0.0.53")},
	}))

	local := ruleIndex(t, rules, `fib daddr type local meta l4proto tcp counter name "rejected_local" reject with tcp reset`)
	require.Equal(t, local+1, ruleIndex(t, rules, `fib daddr type local counter name "rejected_local" reject with icmpx admin-prohibited`))
	for _, after := range []string{"ip daddr . tcp dport @targets4 accept", "ip6 daddr . tcp dport @targets6 accept"} {
		require.Greater(t, ruleIndex(t, rules, after), local, after)
	}
	for _, before := range []string{
		"ip daddr . tcp dport @allownode4 accept", "ip6 daddr . tcp dport @allownode6 accept",
		"ip daddr @resolvers4 meta l4proto { tcp, udp } th dport 53 accept", "ip6 daddr @resolvers6 meta l4proto { tcp, udp } th dport 53 accept",
	} {
		require.Less(t, ruleIndex(t, rules, before), local, before)
	}
	for _, blocked := range []string{"ip daddr @blocked4 meta l4proto tcp reject with tcp reset", "ip6 daddr @blocked6 reject with icmpx admin-prohibited"} {
		require.Less(t, ruleIndex(t, rules, blocked), ruleIndex(t, rules, "ip daddr . tcp dport @allownode4 accept"), "a block is absolute")
	}
}

func TestATargetOfAllowNodeGoesIntoASetOfItsOwn(t *testing.T) {
	f, n, _, _ := newTestFilter(t)

	require.NoError(t, f.Set(t.Context(), []Target{target("10.0.0.5:80"), nodeTarget("10.0.0.2:8006"), nodeTarget("[fd00::2]:8006")}))

	require.Len(t, n.applied(), 1)
	script := n.applied()[0]
	require.Equal(t, []string{"10.0.0.5 . 80"}, elementsOf(t, script, setTargets4))
	require.Equal(t, []string{"10.0.0.2 . 8006"}, elementsOf(t, script, "allownode4"))
	require.Equal(t, []string{"fd00::2 . 8006"}, elementsOf(t, script, "allownode6"))
	require.Empty(t, elementsOf(t, script, setTargets6))
}

// Given both ways, an address and port is a target of allowNode: one
// element of one set.
func TestATargetGivenBothWaysIsOneOfAllowNode(t *testing.T) {
	f, n, _, _ := newTestFilter(t)

	require.NoError(t, f.Set(t.Context(), []Target{target("10.0.0.2:8006"), nodeTarget("10.0.0.2:8006")}))

	script := n.applied()[0]
	require.Empty(t, elementsOf(t, script, setTargets4))
	require.Equal(t, []string{"10.0.0.2 . 8006"}, elementsOf(t, script, "allownode4"))
}

func TestRemoveTakesATargetOfAllowNodeOutOfItsSet(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	require.NoError(t, f.Set(t.Context(), []Target{target("10.0.0.2:80"), nodeTarget("10.0.0.2:8006")}))
	n.reset()

	require.NoError(t, f.Remove(t.Context(), addr("10.0.0.2")))

	require.Equal(t, []string{"delete element inet pco_egress targets4 { 10.0.0.2 . 80 }\n" +
		"delete element inet pco_egress allownode4 { 10.0.0.2 . 8006 }\n"}, n.applied())
}

// A table with a target of allowNode besides the others reads back as what
// it was given.
func TestVerifyReadsTheTargetsOfAllowNodeBack(t *testing.T) {
	for _, version := range []string{"1.0.6", "1.1.3"} {
		t.Run(version, func(t *testing.T) {
			f, n, r, _ := newTestFilter(t)
			r.set(addr("192.168.1.1"))
			tg := append(targets("10.0.0.5:80", "10.0.0.5:8080", "10.0.0.6:443"), nodeTarget("10.0.0.2:8006"))
			require.NoError(t, f.Set(t.Context(), tg))
			n.setLive(realListing(t, version).with(t, tg, []netip.Addr{addr("192.168.1.1")}).bytes(t))

			require.NoError(t, f.Verify(t.Context()))

			live, err := ReadLive(t.Context(), n, testUID)
			require.NoError(t, err)
			require.Equal(t, []Target{nodeTarget("10.0.0.2:8006"), target("10.0.0.5:80"), target("10.0.0.5:8080"), target("10.0.0.6:443")}, live.Targets)
		})
	}
}

func TestVerifyNoticesATargetInTheWrongSet(t *testing.T) {
	f, n, r, _ := newTestFilter(t)
	r.set(addr("192.168.1.1"))
	tg := targets("10.0.0.5:80", "10.0.0.5:8080", "10.0.0.6:443")
	require.NoError(t, f.Set(t.Context(), append(tg, target("10.0.0.2:8006"))))
	n.setLive(realListing(t, "1.1.3").with(t, append(tg, nodeTarget("10.0.0.2:8006")), []netip.Addr{addr("192.168.1.1")}).bytes(t))

	err := f.Verify(t.Context())

	require.ErrorIs(t, err, ErrChanged)
	require.ErrorContains(t, err, "set targets4 lacks 10.0.0.2:8006; set allownode4 holds 10.0.0.2:8006")
}
