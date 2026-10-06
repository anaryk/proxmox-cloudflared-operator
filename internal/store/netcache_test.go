package store

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNetCacheRoundTrip(t *testing.T) {
	s, p := openStore(t)
	_, found, err := s.NetCache()
	require.NoError(t, err)
	require.False(t, found)

	want := NetCache{
		Networks: []Network{attachNetwork("vmbr1", 20, 1), managedNetwork(2)},
		ServiceAddrs: []ServiceAddr{{
			Network: "vmbr1.20", Addr: netip.MustParseAddr("10.20.0.5"), Service: netip.MustParseAddr("198.18.1.0"), AllocatedAt: t0,
		}},
		Leases:    []Lease{leaseOf("10.203.118.5")},
		WrittenAt: t0,
	}
	want.Networks[0].Rev = 4
	require.NoError(t, s.SaveNetCache(want))

	got, found, err := s.NetCache()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, want, got)
	require.Equal(t, []string{"netns/state.json"}, stored(t, p.Local))
	require.Empty(t, stored(t, p.Cluster), "the cache is the node's own")
}

func TestPinsRoundTrip(t *testing.T) {
	s, p := openStore(t)
	pins, at, err := s.Pins()
	require.NoError(t, err)
	require.Empty(t, pins)
	require.True(t, at.IsZero())

	in := []Pin{
		{Network: "vmbr1.20", Addr: netip.MustParseAddr("10.20.0.5"), MAC: "BC:24:11:00:AA:B5"},
		{Network: "pconet", Addr: netip.MustParseAddr("10.203.118.5"), MAC: "06:50:43:4f:76:05"},
	}
	require.NoError(t, s.SavePins(in, t0))
	require.Equal(t, "BC:24:11:00:AA:B5", in[0].MAC, "the caller's pins are not changed")

	pins, at, err = s.Pins()
	require.NoError(t, err)
	require.Equal(t, t0, at)
	require.Equal(t, []Pin{
		{Network: "vmbr1.20", Addr: netip.MustParseAddr("10.20.0.5"), MAC: "bc:24:11:00:aa:b5"},
		{Network: "pconet", Addr: netip.MustParseAddr("10.203.118.5"), MAC: "06:50:43:4f:76:05"},
	}, pins)
	require.Equal(t, []string{"netns/pins.json"}, stored(t, p.Local))

	require.NoError(t, s.SavePins(nil, t0.Add(time.Minute)))
	pins, at, err = s.Pins()
	require.NoError(t, err)
	require.Empty(t, pins)
	require.Equal(t, t0.Add(time.Minute), at)
}

func TestSavePinsRefusesAPinThatNamesNothing(t *testing.T) {
	for name, pin := range map[string]Pin{
		"no network": {Addr: netip.MustParseAddr("10.20.0.5"), MAC: "bc:24:11:00:aa:b5"},
		"no address": {Network: "vmbr1", MAC: "bc:24:11:00:aa:b5"},
		"no MAC":     {Network: "vmbr1", Addr: netip.MustParseAddr("10.20.0.5")},
	} {
		t.Run(name, func(t *testing.T) {
			s, p := openStore(t)
			require.Error(t, s.SavePins([]Pin{pin}, t0))
			require.Empty(t, stored(t, p.Local))
		})
	}
}

func TestPinsStandForFiveMinutes(t *testing.T) {
	require.True(t, FreshPins(t0, t0))
	require.True(t, FreshPins(t0, t0.Add(5*time.Minute-time.Second)))
	require.False(t, FreshPins(t0, t0.Add(5*time.Minute)))
	require.False(t, FreshPins(t0, t0.Add(-time.Second)), "pins from ahead of the clock do not stand")
	require.False(t, FreshPins(time.Time{}, t0), "no pins were saved")
}
