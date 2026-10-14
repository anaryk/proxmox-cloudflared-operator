package store

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

func TestBlocksRoundTrip(t *testing.T) {
	s, p := openStore(t)
	got, err := s.Blocks()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)

	four := Block{Addr: netip.MustParseAddr("10.0.0.22"), Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 102}, MAC: "bc:24:11:00:02:01",
		Node: "pve3", Port: "tap140i0", Why: "conflict", Since: t0, LastReport: t0}
	six := Block{Addr: netip.MustParseAddr("fd00::22"), Guest: model.GuestRef{Kind: model.KindLXC, VMID: 103}, MAC: "bc:24:11:00:03:01",
		Node: "pve2", Port: "eno1", Why: "moved", Since: t0.Add(time.Minute), LastReport: t0.Add(2 * time.Minute)}
	require.NoError(t, s.SaveBlock(four))
	require.NoError(t, s.SaveBlock(six))

	got, err = s.Blocks()
	require.NoError(t, err)
	require.Equal(t, map[netip.Addr]Block{four.Addr: four, six.Addr: six}, got)
	require.Equal(t, []string{"blocks/10.0.0.22.json", "blocks/fd00--22.json"}, stored(t, p.Cluster))

	four.LastReport = t0.Add(time.Hour)
	require.NoError(t, s.SaveBlock(four))
	require.NoError(t, s.DeleteBlock(six.Addr))
	require.NoError(t, s.DeleteBlock(six.Addr), "a missing one is no error")

	got, err = s.Blocks()
	require.NoError(t, err)
	require.Equal(t, map[netip.Addr]Block{four.Addr: four}, got)
}

func TestSaveBlockRefusesWhatIsNoBlock(t *testing.T) {
	ok := Block{Addr: netip.MustParseAddr("10.0.0.22"), Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 102}, MAC: "bc:24:11:00:02:01",
		Node: "pve3", Port: "tap140i0", Why: "conflict", Since: t0, LastReport: t0}
	for _, tt := range []struct {
		name string
		edit func(*Block)
		want string
	}{
		{"no address", func(b *Block) { b.Addr = netip.Addr{} }, "address"},
		{"no MAC", func(b *Block) { b.MAC = "" }, "MAC"},
		{"no node", func(b *Block) { b.Node = "" }, "node"},
		{"no reason", func(b *Block) { b.Why = "" }, "reason"},
		{"no time", func(b *Block) { b.LastReport = time.Time{} }, "time"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, p := openStore(t)
			b := ok
			tt.edit(&b)
			require.ErrorContains(t, s.SaveBlock(b), tt.want)
			require.Empty(t, stored(t, p.Cluster))
		})
	}
}
