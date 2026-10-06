package store

import (
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPlacementsRoundTrip(t *testing.T) {
	s, p := openStore(t)
	got, err := s.Placements()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)

	shared := Placement{Hostname: "app.example.com", Tunnel: "pco-0123456789ab", Kind: PlacementShared,
		Addr: netip.MustParseAddr("192.168.4.50"), ResolvingNode: "pve-a", Since: t0}
	pinned := Placement{Hostname: "*.shop.example.com", Tunnel: "pco-0123456789ab-pve-b", Kind: PlacementPinned, Node: "pve-b",
		Addr: netip.MustParseAddr("10.203.118.7"), Service: netip.MustParseAddr("198.18.1.4"), ResolvingNode: "pve-b", Since: t0.Add(time.Minute)}
	next := map[string]Placement{shared.Hostname: shared, pinned.Hostname: pinned}
	require.NoError(t, s.SavePlacements(next))

	got, err = s.Placements()
	require.NoError(t, err)
	require.Equal(t, next, got)
	require.Equal(t, []string{"placements/_wildcard.shop.example.com.json", "placements/app.example.com.json"}, stored(t, p.Cluster))

	require.NoError(t, s.SavePlacements(next))
	require.EqualValues(t, 1, revOf(t, filepath.Join(p.Cluster, "placements", "app.example.com.json")), "unchanged ones are not written")

	require.NoError(t, s.SavePlacements(map[string]Placement{shared.Hostname: shared}))
	got, err = s.Placements()
	require.NoError(t, err)
	require.Equal(t, map[string]Placement{shared.Hostname: shared}, got, "the ones left out are deleted")
}

func TestSavePlacementsRefusesWhatIsNoPlacement(t *testing.T) {
	ok := Placement{Hostname: "a.example.com", Tunnel: "pco-0123456789ab", Kind: PlacementShared, Since: t0}
	for _, tt := range []struct {
		name string
		edit func(*Placement)
		want string
	}{
		{"no tunnel", func(p *Placement) { p.Tunnel = "" }, "tunnel"},
		{"another kind", func(p *Placement) { p.Kind = "sticky" }, `"sticky"`},
		{"a pinned one without its node", func(p *Placement) { p.Kind = PlacementPinned }, "node"},
		{"a shared one with a node", func(p *Placement) { p.Node = "pve1" }, "node"},
		{"no time", func(p *Placement) { p.Since = time.Time{} }, "since"},
		{"another hostname", func(p *Placement) { p.Hostname = "b.example.com" }, "b.example.com"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, p := openStore(t)
			bad := ok
			tt.edit(&bad)
			good := ok
			good.Hostname = "c.example.com"
			require.ErrorContains(t, s.SavePlacements(map[string]Placement{"a.example.com": bad, "c.example.com": good}), tt.want)
			require.Empty(t, stored(t, p.Cluster), "a bad placement changes nothing")
		})
	}
}
