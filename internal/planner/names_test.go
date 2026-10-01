package planner

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTheRulesEveryTunnelEndsWith(t *testing.T) {
	w := Writer{InstallID: "abc", Generation: 5, Nonce: "n5"}

	require.Equal(t, IngressRule{Service: "http_status:404"}, CatchAllRule())
	require.Equal(t, IngressRule{Hostname: "g5.n5.pco-abc.invalid", Service: "http_status:404"}, SentinelRule(w))
}

func TestProbeNames(t *testing.T) {
	require.Equal(t, "_pco-probe-", ProbeRecordPrefix)
	require.Equal(t, "pco:abc probe", ProbeRecordComment("abc"))
	require.True(t, strings.HasPrefix(ProbeRecordComment("abc"), DNSMarker("abc")), "the marker comes first")
	require.Equal(t, "pco-abc_probe_r4nd", ProbeTunnelName("abc", "r4nd"))
}

func TestIsProbeTunnel(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"pco-abc_probe_r4nd", true},
		{"pco-abc_probe_0123456789abcdef", true},
		{"pco-abc", false},
		{"pco-abc-node1", false},
		{"pco-abc-probe-r4nd", false},
		{"pco-abc_probe_", false},
		{"pco-abc_probe_R4ND", false},
		{"pco-abc_probe_r4nd-x", false},
		{"pco-abc_probe_r4nd_x", false},
		{"pco-abcd_probe_r4nd", false},
		{"pco-ab_probe_r4nd", false},
		{"PCO-abc_probe_r4nd", false},
		{"x-pco-abc_probe_r4nd", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, IsProbeTunnel("abc", tt.name))
		})
	}
}

func TestProbeTunnelsNeverLookLikeTunnelsOfNodes(t *testing.T) {
	// A tunnel of a node is "pco-<id>-<node>", and a node name has no
	// underscore.
	name := ProbeTunnelName("abc", "node1")
	require.False(t, strings.HasPrefix(name, TunnelName("abc")+"-"))
	require.True(t, IsProbeTunnel("abc", name))
	require.False(t, IsProbeTunnel("abc", TunnelName("abc")+"-node1"))
}
