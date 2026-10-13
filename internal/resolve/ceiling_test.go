package resolve

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// observedOnPconet is the ceiling of the appliance: a proof made through the
// managed leg stands for observed, which the engine raises to filtered while
// the firewall of the vnet's guests is proven.
func observedOnPconet(ifc HostIface) Level {
	if ifc.Via == "pconet" {
		return LevelObserved
	}
	return ""
}

func TestACeilingCapsTheProofAndSkipsTheForwardingTable(t *testing.T) {
	s := managedScenario(t)
	s.settings.Ceiling = observedOnPconet

	res := s.resolve(t, routeFor(netip.Addr{}, "managed"), nil)

	require.Equal(t, ip("10.203.118.5"), res.Target.Addr)
	require.Equal(t, LevelObserved, res.Level)
	require.Equal(t, LevelObserved, res.Binding.Level)
	require.Empty(t, res.Binding.Bridge)
	require.Empty(t, s.prober.ops("fdb"), "no forwarding-table probe where it cannot raise the proof")

	t.Run("an interface above its ceiling is not touched", func(t *testing.T) {
		s := newScenario(t)
		s.settings.Ceiling = observedOnPconet
		res := s.resolve(t, webRoute(), nil)
		require.Equal(t, LevelPort, res.Level)
		require.Len(t, s.prober.ops("fdb"), 1)
	})

	t.Run("without a ceiling the managed card is proven at port", func(t *testing.T) {
		s := managedScenario(t)
		res := s.resolve(t, routeFor(netip.Addr{}, "managed"), nil)
		require.Equal(t, LevelPort, res.Level)
	})
}

// A minimum of filtered asks a card on pconet for observed: a young binding
// there settles, and the call looks no further for a level it cannot prove
// on that card.
func TestACeilingLowersWhatABindingMustBeProvenAtToSettle(t *testing.T) {
	bound := func() *Binding {
		b := boundTo("10.203.118.5")
		b.MAC, b.Level, b.Via, b.Segment = mac1, LevelObserved, "pconet", Segment{Bridge: "pconet"}
		b.Since = t0.Add(-time.Second)
		return b
	}

	s := managedScenario(t)
	s.settings.Ceiling = observedOnPconet
	s.required = LevelFiltered
	res := s.resolve(t, webRoute(), bound())
	require.Equal(t, ip("10.203.118.5"), res.Target.Addr)
	require.Empty(t, s.prober.ops("dial")[1:], "one dial, of the bound card")

	s = managedScenario(t)
	s.settings.Ceiling = observedOnPconet
	s.settings.NoForwardingTable = false
	s.required = LevelPort
	_ = s.resolve(t, webRoute(), bound())
	require.Len(t, s.prober.ops("dial"), 1, "port is above the ceiling too")

	s = managedScenario(t)
	s.required = LevelFiltered
	s.prober.fdb[fdbKey("pconet", 0, mac1)] = nil
	res = s.resolve(t, webRoute(), bound())
	require.Equal(t, ip("10.20.0.10"), res.Target.Addr, "without the ceiling the call looks for a higher proof elsewhere")
}

func TestACeilingCapsTheProofABindingCarries(t *testing.T) {
	s := managedScenario(t)
	s.settings.Ceiling = observedOnPconet
	s.prober.arpErr[arpKey("leg3", "10.203.118.5")] = errors.New("no buffer space")
	b := boundTo("10.203.118.5")
	b.MAC, b.Level, b.Via = mac1, LevelFiltered, "pconet"

	res := s.resolve(t, routeFor(netip.Addr{}, "managed"), b)

	require.Equal(t, ip("10.203.118.5"), res.Target.Addr)
	require.Equal(t, LevelObserved, res.Level, "the level the engine raised it to last time does not carry over")
}
