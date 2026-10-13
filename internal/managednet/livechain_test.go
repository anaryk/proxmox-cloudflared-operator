package managednet

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

func liveFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "iptables-save-managed.txt"))
	require.NoError(t, err)
	return string(b)
}

var liveCard = FirewallCard{NIC: 1, Leg: netip.MustParseAddr("10.203.118.1"), Lease: netip.MustParseAddr("10.203.118.2"), Ports: []uint16{443}}

func TestCardDeviceIsTheDeviceOfTheGuestsCard(t *testing.T) {
	require.Equal(t, "tap9320i1", CardDevice(model.GuestRef{Kind: model.KindQEMU, VMID: 9320}, 1))
	require.Equal(t, "veth9321i0", CardDevice(model.GuestRef{Kind: model.KindLXC, VMID: 9321}, 0))
}

func TestParseIPTablesSaveReadsTheFilterTable(t *testing.T) {
	chains := ParseIPTablesSave(liveFixture(t))
	require.NotContains(t, chains, "PREROUTING", "the raw table is left out")
	require.Contains(t, chains, "tap9320i0-IN")
	require.Len(t, chains["tap9320i1-IN"], 7)
	require.Equal(t, LiveRule{Source: "10.203.118.1/32", Proto: "udp", SPort: "67", DPort: "68", Target: "ACCEPT"}, chains["tap9320i1-IN"][2])
	require.Equal(t, LiveRule{Other: []string{"-m", "mac", "!", "--mac-source", "06:50:43:4f:76:02"}, Target: "DROP"}, chains["tap9320i1-OUT"][1])
	require.Equal(t, LiveRule{}, chains["tap9320i1-IN"][6], "a comment matches nothing")
	require.Contains(t, chains, "veth9321i0-OUT")
}

func TestLiveDriftFindsPcosRulesInstalled(t *testing.T) {
	chains := ParseIPTablesSave(liveFixture(t))
	want := WantRules(liveCard, "pco:"+installID)
	require.Empty(t, LiveDrift(chains, "tap9320i1", want))

	other := liveCard
	other.Ports = []uint16{8443}
	require.Equal(t, []string{"the chain tap9320i1-IN lacks pco's rule IN ACCEPT -source 10.203.118.1 -p tcp -dport 8443"},
		LiveDrift(chains, "tap9320i1", WantRules(other, "pco:"+installID)), "a port pco opens that is not installed")
}

func TestLiveDriftFindsPcosRulesMissing(t *testing.T) {
	want := WantRules(liveCard, "pco:"+installID)
	require.Equal(t, []string{
		"the chain tap9320i0-IN lacks pco's rule IN ACCEPT -source 10.203.118.1 -p udp -sport 67 -dport 68",
		"the chain tap9320i0-OUT lacks pco's rule OUT ACCEPT -source 10.203.118.2 -dest 10.203.118.1",
	}, LiveDrift(ParseIPTablesSave(liveFixture(t)), "tap9320i0", want), "a chain that is the configuration without pco's rules")

	require.Equal(t, []string{"the chain tap9399i1-IN is not loaded", "the chain tap9399i1-OUT is not loaded"},
		LiveDrift(ParseIPTablesSave(liveFixture(t)), "tap9399i1", want))

	// The configured rules with the drop before the accepts: they are all
	// there, but not in pco's order.
	text := strings.Replace(liveFixture(t),
		"-A tap9320i1-IN -s 10.203.118.1/32 -p udp -m udp --sport 67 --dport 68 -j ACCEPT\n", "", 1)
	text = strings.Replace(text, "-A tap9320i1-IN -j DROP\n",
		"-A tap9320i1-IN -j DROP\n-A tap9320i1-IN -s 10.203.118.1/32 -p udp -m udp --sport 67 --dport 68 -j ACCEPT\n", 1)
	require.Equal(t, []string{"the chain tap9320i1-IN lacks pco's rule IN ACCEPT -source 10.203.118.1 -p tcp -dport 443"},
		LiveDrift(ParseIPTablesSave(text), "tap9320i1", want))

	// A rule that matches more than pco's does not stand for it.
	text = strings.Replace(liveFixture(t), "-A tap9320i1-IN -j DROP\n", "-A tap9320i1-IN -m limit --limit 1/s -j DROP\n", 1)
	require.Equal(t, []string{"the chain tap9320i1-IN lacks pco's rule IN DROP"}, LiveDrift(ParseIPTablesSave(text), "tap9320i1", want))
}

func TestFieldsKeepAQuotedWord(t *testing.T) {
	require.Equal(t, []string{"-A", "x", "-m", "comment", "--comment", `a "b" c`, "-j", "ACCEPT"},
		fields(`-A x -m comment --comment "a \"b\" c" -j ACCEPT`))
}
