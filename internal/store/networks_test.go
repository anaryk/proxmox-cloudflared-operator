package store

import (
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func attachNetwork(bridge string, vlan, mark int) Network {
	return Network{
		ID:        SegmentID(bridge, vlan),
		Kind:      NetworkAttach,
		Bridge:    bridge,
		VLAN:      vlan,
		Prefix:    netip.MustParsePrefix("10.20.0.0/24"),
		Legs:      map[string]netip.Addr{"pve1": netip.MustParseAddr("10.20.0.250")},
		LegMAC:    LegMACFor(NetworkAttach, mark),
		Mark:      mark,
		CreatedAt: t0,
	}
}

func managedNetwork(mark int) Network {
	return Network{
		ID:        "pconet",
		Kind:      NetworkManaged,
		Bridge:    "pconet",
		Prefix:    netip.MustParsePrefix("10.203.118.0/24"),
		Legs:      map[string]netip.Addr{"pve1": netip.MustParseAddr("10.203.118.1")},
		LegMAC:    "02:50:43:4f:ff:01",
		Mark:      mark,
		CreatedAt: t0,
	}
}

func TestNetworksRoundTrip(t *testing.T) {
	s, p := openStore(t)
	got, err := s.Networks()
	require.NoError(t, err)
	require.Empty(t, got)

	attach := attachNetwork("vmbr1", 20, 1)
	attach.Uplink, attach.OverlapOK = true, true
	untagged := attachNetwork("vmbr2", 0, 3)
	managed := managedNetwork(2)
	for _, n := range []Network{attach, untagged, managed} {
		require.NoError(t, s.SaveNetwork(n, 0))
	}

	got, err = s.Networks()
	require.NoError(t, err)
	attach.Rev, untagged.Rev, managed.Rev = 1, 1, 1
	require.Equal(t, map[string]Network{"vmbr1.20": attach, "vmbr2": untagged, "pconet": managed}, got)
	require.Equal(t, []string{"networks/pconet.json", "networks/vmbr1.20.json", "networks/vmbr2.json"}, stored(t, p.Cluster))
	require.NotContains(t, string(readRaw(t, filepath.Join(p.Cluster, "networks", "pconet.json")).Data), `"rev"`,
		"the revision is the envelope's")
}

func TestSaveNetworkComparesAndSwaps(t *testing.T) {
	s, p := openStore(t)
	path := filepath.Join(p.Cluster, "networks", "vmbr1.20.json")
	n := attachNetwork("vmbr1", 20, 1)
	require.NoError(t, s.SaveNetwork(n, 0))

	err := s.SaveNetwork(n, 0)
	require.ErrorIs(t, err, ErrRevision, "a network that exists is no new one")

	n.Uplink = true
	require.NoError(t, s.SaveNetwork(n, 1))
	require.EqualValues(t, 2, revOf(t, path))

	before := readRaw(t, path)
	stale := n
	stale.OverlapOK = true
	require.ErrorIs(t, s.SaveNetwork(stale, 1), ErrRevision)
	require.Equal(t, before, readRaw(t, path), "nothing is written")

	n.Rev = 2
	got, err := s.Networks()
	require.NoError(t, err)
	require.Equal(t, n, got["vmbr1.20"])

	require.ErrorIs(t, s.DeleteNetwork("vmbr1.20", 1), ErrRevision)
	require.Equal(t, before, readRaw(t, path))
	require.NoError(t, s.DeleteNetwork("vmbr1.20", 2))
	requireMissing(t, path)
	require.ErrorIs(t, s.DeleteNetwork("vmbr1.20", 2), ErrRevision, "it is gone")

	require.ErrorIs(t, s.SaveNetwork(attachNetwork("vmbr3", 0, 4), 5), ErrRevision, "a new network has no revision yet")
	require.Empty(t, stored(t, p.Cluster))
}

func TestSaveNetworkRefusesAMarkAnotherNetworkHolds(t *testing.T) {
	s, p := openStore(t)
	first := attachNetwork("vmbr1", 0, 7)
	require.NoError(t, s.SaveNetwork(first, 0))

	err := s.SaveNetwork(attachNetwork("vmbr2", 0, 7), 0)
	require.ErrorContains(t, err, "mark 7 is the mark of network vmbr1")
	require.False(t, errors.Is(err, ErrRevision))
	require.Equal(t, []string{"networks/vmbr1.json"}, stored(t, p.Cluster))
	require.Error(t, s.SaveNetwork(managedNetwork(7), 0))

	first.Uplink = true
	require.NoError(t, s.SaveNetwork(first, 1), "a network keeps its own mark")
	moved := attachNetwork("vmbr1", 0, 8)
	require.NoError(t, s.SaveNetwork(moved, 2))
	require.NoError(t, s.SaveNetwork(attachNetwork("vmbr2", 0, 7), 0), "a mark given up is free")
}

func TestTheLegMACFollowsTheKindAndTheMark(t *testing.T) {
	require.Equal(t, "02:50:43:4f:00:01", LegMACFor(NetworkAttach, 1))
	require.Equal(t, "02:50:43:4f:00:fa", LegMACFor(NetworkAttach, 250))
	require.Equal(t, "02:50:43:4f:ff:01", LegMACFor(NetworkManaged, 9))

	s, p := openStore(t)
	upper := attachNetwork("vmbr1", 0, 10)
	upper.LegMAC = "02:50:43:4F:00:0A"
	require.NoError(t, s.SaveNetwork(upper, 0))
	got, err := s.Networks()
	require.NoError(t, err)
	require.Equal(t, "02:50:43:4f:00:0a", got["vmbr1"].LegMAC, "stored in normal form")

	for name, n := range map[string]Network{
		"an attach with the mark written in decimal": func() Network {
			n := attachNetwork("vmbr2", 0, 10)
			n.LegMAC = "02:50:43:4f:00:10"
			return n
		}(),
		"an attach with the managed leg's MAC": func() Network {
			n := attachNetwork("vmbr2", 0, 11)
			n.LegMAC = "02:50:43:4f:ff:01"
			return n
		}(),
		"the managed network with an attach leg's MAC": func() Network {
			n := managedNetwork(12)
			n.LegMAC = LegMACFor(NetworkAttach, 12)
			return n
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, s.SaveNetwork(n, 0), "leg MAC")
			require.Equal(t, []string{"networks/vmbr1.json"}, stored(t, p.Cluster))
		})
	}
}

func TestSaveNetworkRefusesWhatDoesNotFit(t *testing.T) {
	edit := func(n Network, f func(*Network)) Network {
		f(&n)
		return n
	}
	attach, managed := attachNetwork("vmbr1", 20, 1), managedNetwork(2)
	tests := map[string]Network{
		"an id that is not the bridge and VLAN": edit(attach, func(n *Network) { n.ID = "vmbr1" }),
		"no bridge":                             edit(attach, func(n *Network) { n.Bridge, n.ID = "", ".20" }),
		"a bridge with a dot":                   edit(attach, func(n *Network) { n.Bridge, n.ID = "vmbr1.5", "vmbr1.5.20" }),
		"a VLAN out of range":                   edit(attach, func(n *Network) { n.VLAN, n.ID = 4095, "vmbr1.4095" }),
		"an attach to the managed vnet":         edit(attach, func(n *Network) { n.Bridge, n.VLAN, n.ID = "pconet", 0, "pconet" }),
		"another kind":                          edit(attach, func(n *Network) { n.Kind = "direct" }),
		"the managed network under another id":  edit(managed, func(n *Network) { n.ID = "vmbr9" }),
		"the managed network on a bridge":       edit(managed, func(n *Network) { n.Bridge = "vmbr9" }),
		"the managed network on a VLAN":         edit(managed, func(n *Network) { n.VLAN = 5 }),
		"no mark":                               edit(attach, func(n *Network) { n.Mark, n.LegMAC = 0, LegMACFor(NetworkAttach, 0) }),
		"a mark over 250":                       edit(attach, func(n *Network) { n.Mark, n.LegMAC = 251, "02:50:43:4f:00:fb" }),
		"no time it was made at":                edit(attach, func(n *Network) { n.CreatedAt = time.Time{} }),
		"no prefix":                             edit(attach, func(n *Network) { n.Prefix = netip.Prefix{} }),
		"an IPv6 prefix":                        edit(attach, func(n *Network) { n.Prefix = netip.MustParsePrefix("fd00::/64") }),
		"a prefix that is not its network":      edit(attach, func(n *Network) { n.Prefix = netip.MustParsePrefix("10.20.0.7/24") }),
		"a prefix in the service prefix":        edit(attach, func(n *Network) { n.Prefix = netip.MustParsePrefix("198.18.4.0/24") }),
		"a prefix over the service prefix":      edit(attach, func(n *Network) { n.Prefix = netip.MustParsePrefix("198.0.0.0/8") }),
		"a leg outside the prefix": edit(attach, func(n *Network) {
			n.Legs = map[string]netip.Addr{"pve1": netip.MustParseAddr("10.21.0.5")}
		}),
		"a leg on the network address": edit(attach, func(n *Network) {
			n.Legs = map[string]netip.Addr{"pve1": netip.MustParseAddr("10.20.0.0")}
		}),
		"a leg on the broadcast address": edit(attach, func(n *Network) {
			n.Legs = map[string]netip.Addr{"pve1": netip.MustParseAddr("10.20.0.255")}
		}),
		"a leg of no node": edit(attach, func(n *Network) {
			n.Legs = map[string]netip.Addr{"": netip.MustParseAddr("10.20.0.250")}
		}),
		"a managed leg that is not the first address": edit(managed, func(n *Network) {
			n.Legs = map[string]netip.Addr{"pve1": netip.MustParseAddr("10.203.118.2")}
		}),
		"a leg MAC that is no MAC": edit(attach, func(n *Network) { n.LegMAC = "02:50:43:4f:00" }),
	}
	for name, n := range tests {
		t.Run(name, func(t *testing.T) {
			s, p := openStore(t)
			require.Error(t, s.SaveNetwork(n, 0))
			require.Empty(t, stored(t, p.Cluster))
		})
	}
}

func TestANetworkStoredUnderAnotherIDIsAnError(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.SaveNetwork(attachNetwork("vmbr1", 0, 1), 0))
	raw := readRaw(t, filepath.Join(p.Cluster, "networks", "vmbr1.json"))
	writeFile(t, filepath.Join(p.Cluster, "networks", "vmbr2.json"), envelopeJSON("vmbr2", string(raw.Data)))
	_, err := s.Networks()
	require.ErrorContains(t, err, `holds the network "vmbr1", which is stored as "vmbr2"`)
}
