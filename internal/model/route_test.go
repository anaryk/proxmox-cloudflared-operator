package model

import (
	"encoding/json"
	"net/netip"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRouteOwner(t *testing.T) {
	tests := []struct {
		name  string
		route Route
		want  string
	}{
		{
			name:  "qemu guest",
			route: Route{Source: SourceAnnotation, Guest: &GuestRef{Kind: KindQEMU, VMID: 101}},
			want:  "qemu/101",
		},
		{
			name:  "lxc guest",
			route: Route{Source: SourceAnnotation, Guest: &GuestRef{Kind: KindLXC, VMID: 200}},
			want:  "lxc/200",
		},
		{
			name:  "manual without guest",
			route: Route{Source: SourceManual, ManualID: "grafana"},
			want:  "manual/grafana",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.route.Owner())
		})
	}
}

func TestCompareOwners(t *testing.T) {
	owners := []string{"manual/b", "lxc/5", "qemu/20", "qemu/3", "manual/a"}
	slices.SortFunc(owners, CompareOwners)
	require.Equal(t, []string{"qemu/3", "qemu/20", "lxc/5", "manual/a", "manual/b"}, owners)
}

func TestCompareOwnersSign(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"equal", "qemu/1", "qemu/1", 0},
		{"lower vmid first", "qemu/3", "qemu/20", -1},
		{"higher vmid last", "qemu/20", "qemu/3", 1},
		{"qemu before lxc", "qemu/900", "lxc/1", -1},
		{"lxc after qemu", "lxc/1", "qemu/900", 1},
		{"guest before manual", "lxc/5", "manual/a", -1},
		{"manual after guest", "manual/a", "lxc/5", 1},
		{"manual ids lexically", "manual/a", "manual/b", -1},
		{"equal manual", "manual/a", "manual/a", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, CompareOwners(tt.a, tt.b))
		})
	}
}

func TestRouteJSONOmitsZeroAddr(t *testing.T) {
	r := Route{
		Hostname: "app.example.com",
		Target:   Target{Scheme: SchemeHTTP, Port: 8080},
		Source:   SourceAnnotation,
		Guest:    &GuestRef{Kind: KindLXC, VMID: 200},
	}

	data, err := json.Marshal(r)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	target, ok := raw["target"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, target, "addr")

	var got Route
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, r, got)
}

func TestRouteJSONKeepsAddr(t *testing.T) {
	r := Route{
		Hostname: "grafana.example.com",
		Target:   Target{Scheme: SchemeHTTPS, Addr: netip.MustParseAddr("10.0.0.9"), Port: 3000},
		Options:  RouteOptions{NoTLSVerify: true, HostHeader: "grafana.internal"},
		Source:   SourceManual,
		ManualID: "grafana",
	}

	data, err := json.Marshal(r)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	target, ok := raw["target"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "10.0.0.9", target["addr"])

	var got Route
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, r, got)
}
