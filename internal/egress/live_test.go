package egress

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadLiveReturnsTheSetsAndTheCounters(t *testing.T) {
	n := &fakeNft{}
	n.setLive(realListing(t, "1.1.3").edit(t, func(l listing) listing {
		l.object(t, "counter", counterLocal)["packets"] = 3
		l.object(t, "counter", counterLocal)["bytes"] = 180
		l.object(t, "counter", counterOther)["packets"] = 12
		l.object(t, "counter", counterOther)["bytes"] = 720
		return l
	}).with(t, targets("10.0.0.6:443", "[fd00::5]:80", "10.0.0.5:80"), []netip.Addr{addr("fd00::53"), addr("192.168.1.1")}).bytes(t))

	live, err := ReadLive(t.Context(), n, testUID)

	require.NoError(t, err)
	require.Equal(t, Live{
		Targets:       targets("10.0.0.5:80", "10.0.0.6:443", "[fd00::5]:80"),
		Resolvers:     []netip.Addr{addr("192.168.1.1"), addr("fd00::53")},
		RejectedLocal: Counter{Packets: 3, Bytes: 180},
		Rejected:      Counter{Packets: 12, Bytes: 720},
	}, live)
}

func TestReadLiveSaysWhatIsNotAsItShouldBe(t *testing.T) {
	n := &fakeNft{}
	n.setLive(realListing(t, "1.1.3").bytes(t))
	live, err := ReadLive(t.Context(), n, testUID)
	require.NoError(t, err)
	require.Empty(t, live.Differences)

	for _, version := range []string{"1.1.3", "1.0.6"} {
		n.setLive(dormant(t, version).bytes(t))
		live, err = ReadLive(t.Context(), n, testUID)
		require.NoError(t, err)
		require.Len(t, live.Differences, 1, version)
		require.Contains(t, live.Differences[0], "the table has flags", version)
	}

	live, err = ReadLive(t.Context(), n, 4242)
	require.NoError(t, err)
	require.NotEmpty(t, live.Differences, "a table for another user")
}

func TestReadLiveOfAMissingTable(t *testing.T) {
	n := &fakeNft{listErr: ErrNotLoaded}

	_, err := ReadLive(t.Context(), n, testUID)

	require.ErrorIs(t, err, ErrNotLoaded)
}

func TestReadLiveRefusesAnElementItCannotRead(t *testing.T) {
	n := &fakeNft{}
	n.setLive(realListing(t, "1.1.3").edit(t, func(l listing) listing {
		l.object(t, "set", setResolvers4)["elem"] = []any{map[string]any{"prefix": map[string]any{"addr": "10.0.0.0", "len": 8}}}
		return l
	}).bytes(t))

	_, err := ReadLive(t.Context(), n, testUID)

	require.ErrorContains(t, err, "set resolvers4 holds an element pco cannot read")
}

func TestBlockLiveTakesAnAddressOutOfEverySetAndBlocksItInOneTransaction(t *testing.T) {
	n := &fakeNft{}
	n.setLive(realListing(t, "1.1.3").with(t,
		targets("10.0.0.5:80", "10.0.0.5:8080", "10.0.0.6:443"),
		[]netip.Addr{addr("10.0.0.5"), addr("192.168.1.1")}).bytes(t))

	removed, err := BlockLive(t.Context(), n, addr("::ffff:10.0.0.5"))

	require.NoError(t, err)
	require.Equal(t, 3, removed)
	require.Equal(t, []string{
		"delete element inet pco_egress targets4 { 10.0.0.5 . 80, 10.0.0.5 . 8080 }\n" +
			"delete element inet pco_egress resolvers4 { 10.0.0.5 }\n" +
			"add element inet pco_egress blocked4 { 10.0.0.5 }\n",
	}, n.applied())
}

func TestBlockLiveOfAnAddressTheSetsDoNotHoldStillBlocksIt(t *testing.T) {
	n := &fakeNft{}
	n.setLive(realListing(t, "1.1.3").bytes(t))

	removed, err := BlockLive(t.Context(), n, addr("fd00::99"))

	require.NoError(t, err)
	require.Zero(t, removed)
	require.Equal(t, []string{"add element inet pco_egress blocked6 { fd00::99 }\n"}, n.applied())
}

func TestBlockLiveWithoutATable(t *testing.T) {
	n := &fakeNft{listErr: ErrNotLoaded}

	_, err := BlockLive(t.Context(), n, addr("10.0.0.5"))

	require.ErrorIs(t, err, ErrNotLoaded)
	require.Empty(t, n.applied())
}

func TestBlockLiveReturnsAFailedApply(t *testing.T) {
	n := &fakeNft{apply: func(string) error { return errBoom }}
	n.setLive(realListing(t, "1.1.3").bytes(t))

	_, err := BlockLive(t.Context(), n, addr("10.0.0.5"))

	require.ErrorIs(t, err, errBoom)
}

func TestUnblockLiveTakesTheAddressOutOfTheBlockedSet(t *testing.T) {
	n := &fakeNft{}
	n.setLive(realListing(t, "1.1.3").withBlocked(t, addr("10.0.0.9"), addr("fd00::9")).bytes(t))

	removed, err := UnblockLive(t.Context(), n, addr("fd00::9"))
	require.NoError(t, err)
	require.True(t, removed)
	require.Equal(t, []string{"delete element inet pco_egress blocked6 { fd00::9 }\n"}, n.applied())

	removed, err = UnblockLive(t.Context(), n, addr("10.0.0.10"))
	require.NoError(t, err)
	require.False(t, removed)
	require.Len(t, n.applied(), 1, "nothing to take out")
}

func TestLoadAppliesBaseWhenThereIsNoTable(t *testing.T) {
	n := &fakeNft{listErr: ErrNotLoaded}

	loaded, err := Load(t.Context(), n, testUID, nil, nil)

	require.NoError(t, err)
	require.True(t, loaded)
	require.Equal(t, []string{Base(testUID, nil, nil)}, n.applied())
}

func TestLoadKeepsAnIntactTableWithItsSets(t *testing.T) {
	n := &fakeNft{}
	n.setLive(realListing(t, "1.0.6").bytes(t))

	loaded, err := Load(t.Context(), n, testUID, nil, nil)

	require.NoError(t, err)
	require.False(t, loaded)
	require.Empty(t, n.applied())
}

func TestLoadReplacesATableThatIsNotIntact(t *testing.T) {
	for name, l := range map[string]func(t *testing.T) listing{
		"flushed": func(t *testing.T) listing {
			return realListing(t, "1.1.3").edit(t, func(l listing) listing {
				var out listing
				for _, e := range l {
					if _, rule := e["rule"]; !rule {
						out = append(out, e)
					}
				}
				return out
			})
		},
		"another user":      func(t *testing.T) listing { return realListing(t, "1.1.3") },
		"dormant":           func(t *testing.T) listing { return dormant(t, "1.1.3") },
		"dormant, as 1.0.6": func(t *testing.T) listing { return dormant(t, "1.0.6") },
	} {
		t.Run(name, func(t *testing.T) {
			n := &fakeNft{}
			n.setLive(l(t).bytes(t))
			uid := uint32(testUID)
			if name == "another user" {
				uid = 4242
			}

			loaded, err := Load(t.Context(), n, uid, nil, nil)

			require.NoError(t, err)
			require.True(t, loaded)
			require.Equal(t, []string{Base(uid, nil, nil)}, n.applied())
		})
	}
}

func TestLoadLoadsTheResolversItIsGivenLessTheBlocked(t *testing.T) {
	n := &fakeNft{listErr: ErrNotLoaded}
	resolvers := []netip.Addr{addr("192.168.1.1"), addr("10.0.0.53")}
	blocked := []netip.Addr{addr("10.0.0.53")}

	loaded, err := Load(t.Context(), n, testUID, resolvers, blocked)

	require.NoError(t, err)
	require.True(t, loaded)
	require.Equal(t, []string{Base(testUID, resolvers, blocked)}, n.applied())
	require.Equal(t, []string{"192.168.1.1"}, elementsOf(t, n.applied()[0], setResolvers4))
}

func TestLoadReplacesATableWhoseListingCannotBeRead(t *testing.T) {
	// What nft 1.0.6 can print for a table with flags.
	n := &fakeNft{live: []byte(`{"nftables": [{"table": {"family": "inet", "name": "pco_egress", "handle": 1, "flags": `)}

	loaded, err := Load(t.Context(), n, testUID, nil, nil)

	require.NoError(t, err)
	require.True(t, loaded)
	require.Equal(t, []string{Base(testUID, nil, nil)}, n.applied())
}

func TestLoadReturnsAFailureToList(t *testing.T) {
	n := &fakeNft{listErr: errBoom}

	_, err := Load(t.Context(), n, testUID, nil, nil)

	require.ErrorIs(t, err, errBoom)
	require.Empty(t, n.applied())
}

func TestUnloadRemovesTheTableWhetherOrNotItIsThere(t *testing.T) {
	n := &fakeNft{}

	require.NoError(t, Unload(t.Context(), n))

	require.Equal(t, []string{"add table inet pco_egress\ndelete table inet pco_egress\n"}, n.applied())
}
