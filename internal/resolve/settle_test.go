package resolve

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// boundSince returns a copy of b bound since at.
func boundSince(b *Binding, at time.Time) *Binding {
	c := *b
	c.Since = at
	return &c
}

// A candidate whose identity comes and goes would move a route bound below it
// up and down every cycle. A binding stays where it is, without looking
// further, until it has been bound for StickyFor.
func TestResolveAYoungBindingDoesNotMoveUp(t *testing.T) {
	s := trustedFirst(t)
	prev := boundSince(atLevel(boundTo("10.40.0.10"), LevelObserved), t0.Add(-30*time.Second))

	res := s.resolve(t, webRoute(), prev)

	requireServedAt(t, res, "10.40.0.10", t0, LevelObserved)
	require.Equal(t, []CandidateResult{
		{Addr: ip("10.40.0.10"), Source: FromStatic, OK: true, Level: "observed"},
		{Addr: ip("10.20.0.10"), Source: FromStatic, Reason: "not tried"},
	}, res.Candidates)
	require.Equal(t, t0.Add(-30*time.Second), res.Binding.Since, "a proof of the same candidate keeps when it was bound")

	t.Run("until it has settled", func(t *testing.T) {
		s := trustedFirst(t)
		s.clock.t = t0.Add(90 * time.Second)

		res := s.resolve(t, webRoute(), res.Binding)

		requireServed(t, res, "10.20.0.10", s.clock.t)
		require.Equal(t, s.clock.t, res.Binding.Since, "a new candidate is bound from now")
	})
}

func TestResolveAMoveDownAfterAnIdentityFailureIsImmediate(t *testing.T) {
	s := trustedFirst(t)
	prev := boundSince(atLevel(boundTo("10.20.0.10"), LevelPort), t0.Add(-10*time.Second))
	s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{foreignMAC}

	res := s.resolve(t, webRoute(), prev)

	requireServedAt(t, res, "10.40.0.10", t0, LevelObserved)
	require.Equal(t, t0, res.Binding.Since)
}

func TestResolveASinceInTheFutureSettlesNothing(t *testing.T) {
	s := trustedFirst(t)
	prev := boundSince(atLevel(boundTo("10.40.0.10"), LevelObserved), t0.Add(time.Hour))

	res := s.resolve(t, webRoute(), prev)

	requireServed(t, res, "10.20.0.10", t0)
}

// A binding stored before the time it was bound was kept counts from its last
// proof, and keeps that from then on.
func TestResolveABindingOfAnOlderVersionIsBoundSinceItsLastProof(t *testing.T) {
	s := newScenario(t)
	prev := boundTo("10.20.0.10")

	res := s.resolve(t, webRoute(), prev)

	requireServed(t, res, "10.20.0.10", t0)
	require.Equal(t, prev.VerifiedAt, res.Binding.Since)
}

func TestResolveKeepsWhenAndWhereTheBindingWasProven(t *testing.T) {
	s := newScenario(t)
	prev := boundSince(boundTo("10.20.0.10"), t0.Add(-time.Hour))

	res := s.resolve(t, webRoute(), prev)

	require.Equal(t, &Binding{
		Owner: webOwner, Hostname: webHost, Guest: webOwner, Addr: ip("10.20.0.10"), MAC: mac0,
		VerifiedAt: t0, Level: LevelPort, Since: t0.Add(-time.Hour), Bridge: "vmbr0", Port: "tap101i0",
	}, res.Binding)

	t.Run("and nowhere at a lower level", func(t *testing.T) {
		s.web().Node = "pve2"
		s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"nic3"}

		res := s.resolve(t, webRoute(), res.Binding)

		require.Equal(t, LevelObserved, res.Level)
		require.Empty(t, res.Binding.Bridge)
		require.Empty(t, res.Binding.Port)
	})
}
