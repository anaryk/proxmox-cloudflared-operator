//go:build linux

package gateway

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The table loads in the map form, which takes range data for the SNAT of
// each leg, and its listing is the table: that is the form the gateway loads
// it in. The form with a rule per leg loads too.
func TestLinuxTheTableLoadsWithItsSNATMap(t *testing.T) {
	l := newLabNet(t)
	for _, form := range []snatForm{snatMap, snatRules} {
		script := renderGateway(form, elementsOf(testLegs, testServices), testLegs)
		require.NoError(t, l.hostNft.Apply(t.Context(), script), "form %d", form)
		raw, err := l.hostNft.List(t.Context(), gwTable)
		require.NoError(t, err)
		listed, err := parseListing(raw)
		require.NoError(t, err)
		require.Empty(t, gwDifferences(listed, form, testLegs), "form %d", form)
		require.Equal(t, sortedElements(inForm(form, elementsOf(testLegs, testServices))), listedElements(listed), "form %d", form)
	}
	require.Equal(t, snatMap, theSNATForm, "the map loads, so the table is loaded in the map form")

	// Elements change in one transaction each way, and back.
	have := sortedElements(elementsOf(testLegs, testServices))
	want := sortedElements(elementsOf(testLegs[1:], testServices[:1]))
	require.NoError(t, l.hostNft.Apply(t.Context(), renderGateway(snatMap, have, testLegs)))
	require.NoError(t, l.hostNft.Apply(t.Context(), elementScript(have, want)))
	raw, err := l.hostNft.List(t.Context(), gwTable)
	require.NoError(t, err)
	listed, err := parseListing(raw)
	require.NoError(t, err)
	require.Equal(t, want, listedElements(listed))
	require.NoError(t, l.hostNft.Apply(t.Context(), elementScript(want, have)))
}

func TestLinuxTheHostTableLoads(t *testing.T) {
	l := newLabNet(t)
	require.NoError(t, l.hostNft.Apply(t.Context(), renderHost()))
	raw, err := l.hostNft.List(t.Context(), hostTable)
	require.NoError(t, err)
	listed, err := parseListing(raw)
	require.NoError(t, err)
	require.Empty(t, hostDifferences(listed))
}

// nftRuleset is the ruleset of the namespace the test runs in.
func nftRuleset(t *testing.T) string {
	t.Helper()
	// Stateless: the counters of rules the machine keeps for itself, such
	// as the ones of a cloud runner, move on their own between two reads.
	out, err := exec.Command(nftPath, "--stateless", "list", "ruleset").CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func requireLab(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("PCO_REQUIRE_LINUX_TESTS") == "1" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}
