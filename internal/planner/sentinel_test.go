package planner

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSentinelRoundTrip(t *testing.T) {
	w := Writer{InstallID: "3f9a2c1e77b0", Generation: 12, Nonce: "k3x9q1"}

	host := SentinelHostname(w)

	require.Equal(t, "g12.k3x9q1.pco-3f9a2c1e77b0.invalid", host)
	got, ok := ParseSentinel(host)
	require.True(t, ok)
	require.Equal(t, w, got)
}

func TestSentinelParsesEveryGeneration(t *testing.T) {
	for _, gen := range []int{0, 1, 9, 10, 4096, math.MaxInt} {
		t.Run(strconv.Itoa(gen), func(t *testing.T) {
			w := Writer{InstallID: "ab12", Generation: gen, Nonce: "n0nce"}

			got, ok := ParseSentinel(SentinelHostname(w))

			require.True(t, ok)
			require.Equal(t, w, got)
		})
	}
}

func TestSentinelRejectsOtherHostnames(t *testing.T) {
	tests := []struct {
		name string
		host string
	}{
		{"empty", ""},
		{"ordinary hostname", "app.example.com"},
		{"missing generation", "g12.pco-x.invalid"},
		{"generation not a number", "gx.n.pco-x.invalid"},
		{"generation without g", "12.n.pco-x.invalid"},
		{"generation empty", "g.n.pco-x.invalid"},
		{"generation with leading zero", "g012.n.pco-x.invalid"},
		{"generation with sign", "g+1.n.pco-x.invalid"},
		{"negative generation", "g-1.n.pco-x.invalid"},
		{"generation overflows int", "g99999999999999999999.n.pco-x.invalid"},
		{"nonce empty", "g1..pco-x.invalid"},
		{"nonce upper case", "g1.N.pco-x.invalid"},
		{"nonce with hyphen", "g1.n-n.pco-x.invalid"},
		{"install label without prefix", "g1.n.x.invalid"},
		{"install id empty", "g1.n.pco-.invalid"},
		{"install id upper case", "g1.n.pco-X.invalid"},
		{"install id with hyphen", "g1.n.pco-a-b.invalid"},
		{"extra label", "g1.n.m.pco-x.invalid"},
		{"another top level domain", "g1.n.pco-x.example"},
		{"suffix only partly matches", "g1.n.pco-x.xinvalid"},
		{"trailing dot", "g1.n.pco-x.invalid."},
		{"upper case suffix", "g1.n.pco-x.INVALID"},
		{"only the suffix", ".invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, ok := ParseSentinel(tt.host)

			require.False(t, ok)
			require.Equal(t, Writer{}, w)
		})
	}
}

func TestSentinelLabelsFitDNS(t *testing.T) {
	w := Writer{
		InstallID:  strings.Repeat("f", 12),
		Generation: math.MaxInt,
		Nonce:      strings.Repeat("z", 16),
	}

	host := SentinelHostname(w)

	for _, label := range strings.Split(host, ".") {
		require.LessOrEqual(t, len(label), 63, label)
	}
	require.LessOrEqual(t, len(host), 253)
	_, ok := ParseSentinel(host)
	require.True(t, ok)
}

func TestNamesDerivedFromInstallID(t *testing.T) {
	require.Equal(t, "pco-3f9a2c1e77b0", TunnelName("3f9a2c1e77b0"))
	require.Equal(t, "pco:3f9a2c1e77b0", DNSMarker("3f9a2c1e77b0"))
}
