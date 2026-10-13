package gateway

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var guardCards = []Sticky{
	{MAC: mac("06:50:43:4f:76:02"), Bridge: ManagedBridge, Port: "veth101i1", Addr: netip.MustParseAddr("10.203.118.2")},
	{MAC: mac("06:50:43:4f:76:03"), Bridge: ManagedBridge, Port: "tap102i1", Addr: netip.MustParseAddr("10.203.118.3")},
}

func TestTheGuardTable(t *testing.T) {
	requireGolden(t, "guard.nft", renderGuard(guardElements(guardCards)))
}

// A card with no port or no address, or on another bridge, has no element:
// with no address it may send from none.
func TestTheGuardHoldsTheCardsWithTheirPortsAndAddresses(t *testing.T) {
	cards := append([]Sticky{
		{MAC: mac("06:50:43:4f:76:04"), Bridge: ManagedBridge, Addr: netip.MustParseAddr("10.203.118.4")},
		{MAC: mac("06:50:43:4f:76:05"), Bridge: ManagedBridge, Port: "tap105i1"},
		{MAC: mac("06:50:43:4f:76:06"), Bridge: "vmbr1", Port: "tap106i0", Addr: netip.MustParseAddr("10.203.118.6")},
	}, guardCards...)
	require.Equal(t, elements{
		setCards:    {`"tap102i1" . 06:50:43:4f:76:03 . 10.203.118.3`, `"veth101i1" . 06:50:43:4f:76:02 . 10.203.118.2`},
		setCardMACs: {`"tap102i1" . 06:50:43:4f:76:03`, `"veth101i1" . 06:50:43:4f:76:02`},
	}, guardElements(cards))
}

func TestTheListingsOfTheGuardAreTheTable(t *testing.T) {
	for _, v := range []string{"1.0.6", "1.1.3"} {
		t.Run(v, func(t *testing.T) {
			l := readListing(t, "guard-"+v+".json")
			require.Empty(t, guardDifferences(l, guardElements(guardCards)))
			require.Equal(t, []string{
				`cards of pco_guard holds "veth101i1" . 06:50:43:4f:76:02 . 10.203.118.2`,
				`card_macs of pco_guard holds "veth101i1" . 06:50:43:4f:76:02`,
			}, guardDifferences(l, guardElements(guardCards[1:])))
		})
	}
}

func TestAGuardThatDiffersNamesWhat(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "guard-1.1.3.json"))
	require.NoError(t, err)
	changed := strings.Replace(string(raw), `{"counter": "dropped_spoofed"}, {"drop": null}`, `{"counter": "dropped_spoofed"}, {"accept": null}`, 1)
	require.NotEqual(t, string(raw), changed)
	l, err := parseListing([]byte(changed))
	require.NoError(t, err)
	require.Contains(t, guardDifferences(l, guardElements(guardCards)),
		"chain prerouting of table bridge pco_guard: rule 7 is not the one pco loads")
}

// The guard is loaded with the managed leg and the cards of the sticky
// entries, follows them, and goes with the managed leg.
func TestTheGuardFollowsTheCards(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sync(t, []Leg{legA}, nil, nil, nil)
	require.Nil(t, h.hostNft.tables[guardFamilyName], "no managed leg, no guard")

	h.sync(t, []Leg{legA, legM}, nil, nil, guardCards)
	tb := h.hostNft.tables[guardFamilyName]
	require.NotNil(t, tb)
	require.Equal(t, guardElements(guardCards)[setCards], tb.elems[setCards])
	require.NoError(t, h.b.Verify(t.Context()))

	h.reset()
	h.sync(t, []Leg{legA, legM}, nil, nil, guardCards)
	require.Empty(t, h.hostNft.scripts, "nothing changes")

	require.NoError(t, ApplySticky(t.Context(), h.b, guardCards[:1]))
	require.Equal(t, guardElements(guardCards[:1])[setCards], h.hostNft.tables[guardFamilyName].elems[setCards])

	h.sync(t, []Leg{legA}, nil, nil, nil)
	require.Nil(t, h.hostNft.tables[guardFamilyName], "the managed leg went, and the guard with it")
}

func TestVerifyNamesAMissingGuard(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sync(t, []Leg{legM}, nil, nil, guardCards)
	delete(h.hostNft.tables, guardFamilyName)
	err := h.b.Verify(t.Context())
	var changed *ChangedError
	require.True(t, errors.As(err, &changed), "%v", err)
	require.Contains(t, changed.Differences, "table bridge pco_guard is missing")
}

func TestUpLoadsTheGuardWithTheCachedCards(t *testing.T) {
	h := newHarness(t).node()
	Expect(h.b, Cache{Legs: []Leg{legM}, Sticky: guardCards})
	h.b.seed([]Leg{legM}, nil, guardCards)
	require.NoError(t, h.b.Up(t.Context()))
	require.Equal(t, guardElements(guardCards)[setCards], h.hostNft.tables[guardFamilyName].elems[setCards])
}

func TestDownRemovesTheGuard(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sync(t, []Leg{legM}, nil, nil, guardCards)
	require.NoError(t, h.b.Down(t.Context()))
	require.Nil(t, h.hostNft.tables[guardFamilyName])
}
