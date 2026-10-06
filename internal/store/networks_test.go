package store

import (
	"encoding/json"
	"errors"
	"fmt"
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

// Each leg's source ports for SNAT are a block of 4096 from 1024 up; the
// block of mark 15 ends at 62463, and a 16th would pass 65535.
func TestTheMarksAreThoseOfTheFifteenNetworksOfANode(t *testing.T) {
	s, _ := openStore(t)
	require.NoError(t, s.SaveNetwork(attachNetwork("vmbr1", 0, 1), 0))
	require.NoError(t, s.SaveNetwork(attachNetwork("vmbr2", 0, 15), 0))
	err := s.SaveNetwork(attachNetwork("vmbr3", 0, 16), 0)
	require.ErrorContains(t, err, "network vmbr3: mark 16 is not 1 to 15")
	require.ErrorContains(t, err, "a block of 4096 source ports", "it says why")
	require.ErrorContains(t, s.SaveNetwork(managedNetwork(16), 0), "mark 16 is not 1 to 15")
	require.ErrorContains(t, s.SaveNetwork(attachNetwork("vmbr3", 0, 0), 0), "mark 0 is not 1 to 15")
}

// A 16th network is refused whatever mark it asks for, with the limit and the
// reason for it: the 15 blocks of source ports.
func TestTheSixteenthNetworkIsRefusedWithTheLimitAndWhy(t *testing.T) {
	s, p := openStore(t)
	for mark := 1; mark <= 15; mark++ {
		require.NoError(t, s.SaveNetwork(attachNetwork(fmt.Sprintf("vmbr%d", mark), 0, mark), 0), "network %d", mark)
	}
	have, err := s.Networks()
	require.NoError(t, err)
	require.Len(t, have, 15)
	before := stored(t, p.Cluster)

	for name, n := range map[string]Network{
		"a mark another network holds": attachNetwork("vmbr16", 0, 3),
		"a mark past the blocks":       attachNetwork("vmbr16", 0, 16),
		"the managed network":          managedNetwork(15),
	} {
		t.Run(name, func(t *testing.T) {
			err := s.SaveNetwork(n, 0)
			require.ErrorContains(t, err, "at most 15 networks")
			require.ErrorContains(t, err, "a block of 4096 source ports")
			require.ErrorContains(t, err, "end at port 62463")
			require.Equal(t, before, stored(t, p.Cluster), "nothing is written")
		})
	}

	changed := have["vmbr7"]
	changed.Uplink = true
	require.NoError(t, s.SaveNetwork(changed, changed.Rev), "a network of the 15 is saved again")

	require.NoError(t, s.DeleteNetwork("vmbr7", changed.Rev+1))
	require.NoError(t, s.SaveNetwork(attachNetwork("vmbr16", 0, 7), 0), "a network that left makes room")
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
	require.Equal(t, "02:50:43:4f:00:0f", LegMACFor(NetworkAttach, 15))
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
		"a mark over 15":                        edit(attach, func(n *Network) { n.Mark, n.LegMAC = 16, "02:50:43:4f:00:10" }),
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

// previousNetwork is vmbr1 as the previous release writes it: it knows
// neither the leg MACs of the nodes nor the scope, and drops them whenever it
// writes the network.
const previousNetwork = `{"id":"vmbr1","kind":"attach","bridge":"vmbr1","prefix":"10.20.0.0/24",` +
	`"legs":{"pve-a":"10.20.0.250","pve-b":"10.20.0.251"},"legMAC":"02:50:43:4f:00:05","mark":5,"createdAt":"2026-10-01T12:00:00Z"}`

func twoLegs(mark int) Network {
	n := attachNetwork("vmbr1", 0, mark)
	n.Legs = map[string]netip.Addr{"pve-a": netip.MustParseAddr("10.20.0.250"), "pve-b": netip.MustParseAddr("10.20.0.251")}
	return n
}

var twoIndexes = map[string]int{"pve-a": 0, "pve-b": 3}

func TestANetworkWithoutTheFieldsOfTheClusterReadsAsBefore(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "networks", "vmbr1.json"), envelopeJSON("vmbr1",
		`{"id":"vmbr1","kind":"attach","bridge":"vmbr1","prefix":"10.20.0.0/24","legs":{"pve1":"10.20.0.250"},`+
			`"legMAC":"02:50:43:4f:00:01","mark":1,"createdAt":"2026-10-01T12:00:00Z"}`))

	got, err := s.Networks()

	require.NoError(t, err)
	n := got["vmbr1"]
	require.Nil(t, n.LegMACs)
	require.Empty(t, n.Scope)
	require.Equal(t, ScopeCluster, n.ScopeName())
	require.Equal(t, map[string]netip.Addr{"pve1": netip.MustParseAddr("10.20.0.250")}, n.Legs)
	mac, err := n.LegMACOn("pve1", map[string]int{"pve1": 0})
	require.NoError(t, err)
	require.Equal(t, "02:50:43:4f:00:01", mac, "the node registered first has index 0, and the MAC its leg had")
	_, err = n.LegMACOn("pve1", nil)
	require.ErrorContains(t, err, "index", "without an index the MAC of a node's leg is not known")

	n.Scope = ScopeNode
	n.Rev = 0
	require.Equal(t, ScopeNode, n.ScopeName())
	require.NoError(t, s.SaveNetwork(n, 1))
	var previous struct {
		Legs   map[string]netip.Addr `json:"legs"`
		LegMAC string                `json:"legMAC"`
	}
	require.NoError(t, json.Unmarshal(readRaw(t, filepath.Join(p.Cluster, "networks", "vmbr1.json")).Data, &previous))
	require.Equal(t, n.Legs, previous.Legs, "the legs stay where the previous release reads them")
}

func TestALegMACPerNodeCarriesTheIndexOfTheNode(t *testing.T) {
	require.Equal(t, "02:50:43:4f:03:05", NodeLegMAC(3, 5))
	require.Equal(t, "02:50:43:4f:00:05", NodeLegMAC(0, 5), "the first node keeps the MAC its legs had")

	s, _ := openStore(t)
	n := twoLegs(5)
	n.Scope = ScopeCluster
	n.LegMACs = map[string]string{"pve-b": "02:50:43:4F:03:05"}
	require.NoError(t, s.SaveNetworkIndexed(n, 0, twoIndexes))

	got, err := s.Networks()
	require.NoError(t, err)
	require.Equal(t, map[string]string{"pve-a": "02:50:43:4f:00:05", "pve-b": "02:50:43:4f:03:05"}, got["vmbr1"].LegMACs,
		"every leg has its MAC, in normal form")
	require.Equal(t, map[string]int{"pve-a": 0, "pve-b": 3}, got["vmbr1"].LegIndexes(), "the MACs tell the indexes again")
	mac, err := got["vmbr1"].LegMACOn("pve-b", twoIndexes)
	require.NoError(t, err)
	require.Equal(t, "02:50:43:4f:03:05", mac)
	require.Equal(t, ScopeCluster, got["vmbr1"].Scope)

	computed := twoLegs(6)
	computed.ID, computed.Bridge = "vmbr2", "vmbr2"
	require.NoError(t, s.SaveNetworkIndexed(computed, 0, twoIndexes))
	got, err = s.Networks()
	require.NoError(t, err)
	require.Equal(t, map[string]string{"pve-a": "02:50:43:4f:00:06", "pve-b": "02:50:43:4f:03:06"}, got["vmbr2"].LegMACs,
		"the store makes them from the indexes")
}

// The previous release drops the leg MACs when it writes a network. Read as
// the network's one MAC, every node's leg on an uplinked bridge would then
// answer with node 0's: the indexes give them back instead.
func TestLegMACsThePreviousReleaseDroppedComeBackFromTheIndexes(t *testing.T) {
	s, p := openStore(t)
	n := twoLegs(5)
	require.NoError(t, s.SaveNetworkIndexed(n, 0, twoIndexes))
	writeFile(t, filepath.Join(p.Cluster, "networks", "vmbr1.json"),
		`{"schemaVersion":1,"rev":2,"id":"vmbr1","data":`+previousNetwork+`}`)

	got, err := s.Networks()
	require.NoError(t, err)
	back := got["vmbr1"]
	require.Nil(t, back.LegMACs)
	mac, err := back.LegMACOn("pve-b", twoIndexes)
	require.NoError(t, err)
	require.Equal(t, "02:50:43:4f:03:05", mac, "not node 0's")
	_, err = back.LegMACOn("pve-b", nil)
	require.ErrorContains(t, err, "pve-b")

	back.Rev = 0
	require.NoError(t, s.SaveNetworkIndexed(back, 2, twoIndexes))
	got, err = s.Networks()
	require.NoError(t, err)
	require.Equal(t, map[string]string{"pve-a": "02:50:43:4f:00:05", "pve-b": "02:50:43:4f:03:05"}, got["vmbr1"].LegMACs,
		"saved again with the indexes, the field is whole again")

	require.ErrorContains(t, s.SaveNetwork(back, 3), "same MAC",
		"without indexes, two legs without a MAC of their own would share the network's")
}

func TestALegMACThatIsNotTheNodesIsRefused(t *testing.T) {
	edit := func(f func(*Network)) Network {
		m := twoLegs(6)
		m.LegMACs = map[string]string{"pve-a": "02:50:43:4f:00:06", "pve-b": "02:50:43:4f:03:06"}
		f(&m)
		return m
	}
	threeLegs := func(m *Network) { m.Legs["pve-c"] = netip.MustParseAddr("10.20.0.252") }
	for name, tc := range map[string]struct {
		n       Network
		indexes map[string]int
		want    string
	}{
		"another node's index": {edit(func(m *Network) { m.LegMACs["pve-b"] = "02:50:43:4f:04:06" }), twoIndexes,
			"is 02:50:43:4f:04:06, want 02:50:43:4f:03:06"},
		"another mark":       {edit(func(m *Network) { m.LegMACs["pve-b"] = "02:50:43:4f:03:05" }), twoIndexes, "leg MAC"},
		"a node with no leg": {edit(func(m *Network) { m.LegMACs["pve-c"] = "02:50:43:4f:05:06" }), twoIndexes, "pve-c"},
		"no MAC":             {edit(func(m *Network) { m.LegMACs["pve-b"] = "zz" }), twoIndexes, "pve-b"},
		"another scope":      {edit(func(m *Network) { m.Scope = "datacenter" }), twoIndexes, `"datacenter"`},
		"the managed leg's index for a node without one": {edit(func(m *Network) {
			threeLegs(m)
			m.LegMACs["pve-c"] = "02:50:43:4f:ff:06"
		}), twoIndexes, "255"},
		"a leg of a node without an index and without a MAC": {edit(threeLegs), twoIndexes, "pve-c"},
		"two legs with one MAC": {edit(func(m *Network) {
			threeLegs(m)
			m.LegMACs["pve-c"] = "02:50:43:4f:03:06"
		}), twoIndexes, "same MAC"},
		"two legs with one MAC, without indexes":    {edit(func(m *Network) { m.LegMACs["pve-b"] = "02:50:43:4f:00:06" }), nil, "same MAC"},
		"without indexes the mark is still checked": {edit(func(m *Network) { m.LegMACs["pve-b"] = "02:50:43:4f:03:05" }), nil, "mark 5"},
	} {
		t.Run(name, func(t *testing.T) {
			s, p := openStore(t)
			require.ErrorContains(t, s.SaveNetworkIndexed(tc.n, 0, tc.indexes), tc.want)
			require.Empty(t, stored(t, p.Cluster))
		})
	}

	s, _ := openStore(t)
	unknown := edit(func(m *Network) {
		threeLegs(m)
		m.LegMACs["pve-c"] = "02:50:43:4f:07:06"
	})
	require.NoError(t, s.SaveNetworkIndexed(unknown, 0, twoIndexes), "a node the registry gave no index yet is checked by the form alone")
}

func TestTheManagedLegHasOneMACOnEveryNode(t *testing.T) {
	s, _ := openStore(t)
	managed := managedNetwork(9)
	managed.Scope = ScopeNode
	managed.Legs["pve-b"] = netip.MustParseAddr("10.203.118.1")
	managed.LegMACs = map[string]string{"pve-b": "02:50:43:4f:03:09"}
	require.ErrorContains(t, s.SaveNetworkIndexed(managed, 0, twoIndexes), "want 02:50:43:4f:ff:01")

	managed.LegMACs = map[string]string{"pve-b": "02:50:43:4f:ff:01"}
	require.NoError(t, s.SaveNetworkIndexed(managed, 0, twoIndexes))
	got, err := s.Networks()
	require.NoError(t, err)
	require.Nil(t, got["pconet"].LegMACs, "one MAC for every node needs none of its own")
	mac, err := got["pconet"].LegMACOn("pve-b", nil)
	require.NoError(t, err)
	require.Equal(t, "02:50:43:4f:ff:01", mac)
}
