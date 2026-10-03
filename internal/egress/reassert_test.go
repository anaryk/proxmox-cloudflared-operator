package egress

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// pco egress unblock takes the address out of the blocked set and puts
// nothing back: until the next Set, the table lacks what the block took out,
// which is no change made outside pco.
func TestVerifyAfterABlockAndAnUnblock(t *testing.T) {
	f, n, r, ov := newTestFilter(t)
	r.set(addr("192.168.1.1"))
	all := targets("10.0.0.5:80", "10.0.0.5:8080", "10.0.0.6:443", "10.0.0.7:22")
	require.NoError(t, f.Set(t.Context(), all))
	_, err := ov.Block(addr("10.0.0.7"))
	require.NoError(t, err)
	n.setLive(realListing(t, "1.1.3").withBlocked(t, addr("10.0.0.7")).bytes(t))
	require.NoError(t, f.Verify(t.Context()))

	_, err = ov.Unblock(addr("10.0.0.7"))
	require.NoError(t, err)
	n.setLive(realListing(t, "1.1.3").bytes(t))

	require.NoError(t, f.Verify(t.Context()))

	n.reset()
	require.NoError(t, f.Set(t.Context(), all))
	require.Len(t, n.applied(), 1, "the next Set puts it back")
	require.Contains(t, elementsOf(t, n.applied()[0], setTargets4), "10.0.0.7 . 22")
}

// While the filter is off, pco egress on may load the table anew, without
// targets: that is no change made outside pco, and the next Set loads them.
func TestVerifyWhileTheFilterIsOffForgetsWhatWasApplied(t *testing.T) {
	f, n, r, ov := newTestFilter(t)
	r.set(addr("192.168.1.1"))
	all := targets("10.0.0.5:80", "10.0.0.5:8080", "10.0.0.6:443")
	require.NoError(t, f.Set(t.Context(), all))
	require.NoError(t, ov.SwitchOff(time.Now()))

	require.ErrorIs(t, f.Verify(t.Context()), ErrOff)

	_, err := ov.SwitchOn()
	require.NoError(t, err)
	n.setLive(realListing(t, "1.1.3").with(t, nil, []netip.Addr{addr("192.168.1.1")}).bytes(t))
	require.NoError(t, f.Verify(t.Context()))
	n.reset()
	require.NoError(t, f.Set(t.Context(), all))
	require.Len(t, n.applied(), 1)
}

func TestReapplyLoadsTheTableAgain(t *testing.T) {
	f, n, _, ov := newTestFilter(t)
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80")))
	n.listErr = ErrNotLoaded
	require.ErrorIs(t, f.Verify(t.Context()), ErrChanged)
	n.reset()

	require.NoError(t, f.Reapply(t.Context()))

	require.Equal(t, []string{render(testUID, contents{targets: targets("10.0.0.5:80")})}, n.applied())

	t.Run("with nothing changed", func(t *testing.T) {
		n.reset()
		require.NoError(t, f.Reapply(t.Context()))
		require.Len(t, n.applied(), 1)
	})
	t.Run("but not while the filter is off", func(t *testing.T) {
		require.NoError(t, ov.SwitchOff(time.Now()))
		n.reset()
		require.NoError(t, f.Reapply(t.Context()))
		require.Empty(t, n.applied())
	})
}

// A table loaded while the resolvers could not be read lets the connectors
// resolve nothing: the next load that has resolvers replaces it.
func TestLoadReplacesAnIntactTableWithoutResolversWhenItIsGivenSome(t *testing.T) {
	resolvers := []netip.Addr{addr("192.168.1.1")}
	n := &fakeNft{}
	n.setLive(realListing(t, "1.1.3").with(t, targets("10.0.0.5:80"), nil).bytes(t))

	loaded, err := Load(t.Context(), n, testUID, resolvers, nil)

	require.NoError(t, err)
	require.True(t, loaded)
	require.Equal(t, []string{Base(testUID, resolvers, nil)}, n.applied())

	t.Run("and keeps it without", func(t *testing.T) {
		n.reset()
		loaded, err := Load(t.Context(), n, testUID, nil, nil)
		require.NoError(t, err)
		require.False(t, loaded)
		require.Empty(t, n.applied())
	})
	t.Run("and keeps one with resolvers", func(t *testing.T) {
		n.reset()
		n.setLive(realListing(t, "1.1.3").bytes(t))
		loaded, err := Load(t.Context(), n, testUID, resolvers, nil)
		require.NoError(t, err)
		require.False(t, loaded)
	})
}

// The node's only resolver is blocked: a table without resolvers is what
// pco loads, and its targets stay.
func TestLoadKeepsATableWithoutResolversWhenTheGivenOnesAreBlocked(t *testing.T) {
	resolvers := []netip.Addr{addr("192.168.1.1")}
	n := &fakeNft{}
	n.setLive(realListing(t, "1.1.3").with(t, targets("10.0.0.5:80"), nil).withBlocked(t, addr("192.168.1.1")).bytes(t))

	loaded, err := Load(t.Context(), n, testUID, resolvers, resolvers)

	require.NoError(t, err)
	require.False(t, loaded)
	require.Empty(t, n.applied())
}

// The connector user was deleted and made again, with another uid: the
// table must confine the new one.
func TestRebindConfinesTheNewUID(t *testing.T) {
	f, n, r, _ := newTestFilter(t)
	r.set(addr("192.168.1.1"))
	require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.5:8080", "10.0.0.6:443")))
	n.setLive(realListing(t, "1.1.3").bytes(t))
	require.NoError(t, f.Verify(t.Context()))
	n.reset()

	require.False(t, f.Rebind(testUID))
	require.True(t, f.Rebind(testUID+1))

	require.ErrorIs(t, f.Verify(t.Context()), ErrChanged, "the table confines the old uid")
	require.NoError(t, f.Reapply(t.Context()))
	require.Len(t, n.applied(), 1)
	require.Contains(t, n.applied()[0], "meta skuid 1000 jump connector")
	require.Contains(t, n.applied()[0], "10.0.0.6 . 443")

	t.Run("also by the next Set of the same targets", func(t *testing.T) {
		require.True(t, f.Rebind(testUID+2))
		n.reset()
		require.NoError(t, f.Set(t.Context(), targets("10.0.0.5:80", "10.0.0.5:8080", "10.0.0.6:443")))
		require.Len(t, n.applied(), 1)
		require.Contains(t, n.applied()[0], "meta skuid 1001 jump connector")
	})
}

func TestReadLiveWithoutTheConnectorUserComparesTheRest(t *testing.T) {
	n := &fakeNft{}
	n.setLive(realListing(t, "1.1.3").bytes(t))

	live, err := ReadLive(t.Context(), n, 0)

	require.NoError(t, err)
	require.Empty(t, live.Differences)
	require.Len(t, live.Targets, 3)

	n.setLive(dormant(t, "1.1.3").bytes(t))
	live, err = ReadLive(t.Context(), n, 0)
	require.NoError(t, err)
	require.NotEmpty(t, live.Differences)
}
