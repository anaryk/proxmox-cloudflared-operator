package resolve

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
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

// A binding the minimum holds back is not served: it does not settle, and
// the route moves up as soon as a candidate that meets the minimum passes.
func TestResolveABindingBelowTheMinimumDoesNotSettle(t *testing.T) {
	prev := boundSince(atLevel(boundTo("10.40.0.10"), LevelObserved), t0.Add(-30*time.Second))
	s := trustedFirst(t)
	s.required = LevelPort

	res := s.resolve(t, webRoute(), prev)

	requireServed(t, res, "10.20.0.10", t0)

	t.Run("one that meets it does", func(t *testing.T) {
		s := trustedFirst(t)
		s.required = LevelObserved

		res := s.resolve(t, webRoute(), prev)

		requireServedAt(t, res, "10.40.0.10", t0, LevelObserved)
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

// A guest with two NICs on the bridge answers from both: the port of each is
// kept, for the watch.
func TestResolveKeepsThePortOfEveryMACThatAnswered(t *testing.T) {
	s := newScenario(t)
	s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 0, "10.20.0.10"), nicOn(1, mac1, "vmbr0", 0)}
	s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0, mac1}
	s.prober.fdb[fdbKey("vmbr0", 0, mac1)] = []string{"fwpr101p1"}

	res := s.resolve(t, webRoute(), nil)

	requireServed(t, res, "10.20.0.10", t0)
	require.Equal(t, map[string]string{mac0: "tap101i0", mac1: "fwpr101p1"}, res.Binding.Ports)
	require.Equal(t, "tap101i0", res.Binding.Port)
}

func TestResolveKeepsWhenAndWhereTheBindingWasProven(t *testing.T) {
	s := newScenario(t)
	prev := boundSince(boundTo("10.20.0.10"), t0.Add(-time.Hour))

	res := s.resolve(t, webRoute(), prev)

	require.Equal(t, &Binding{
		Owner: webOwner, Hostname: webHost, Guest: webOwner, Addr: ip("10.20.0.10"), MAC: mac0,
		VerifiedAt: t0, Level: LevelPort, Since: t0.Add(-time.Hour), Bridge: "vmbr0", Port: "tap101i0",
		Ports: map[string]string{mac0: "tap101i0"}, Segment: Segment{Bridge: "vmbr0"},
	}, res.Binding)

	t.Run("and nowhere at a lower level", func(t *testing.T) {
		s.web().Node = "pve2"
		s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"nic3"}

		res := s.resolve(t, webRoute(), res.Binding)

		require.Equal(t, LevelObserved, res.Level)
		require.Empty(t, res.Binding.Bridge)
		require.Empty(t, res.Binding.Port)
		require.Empty(t, res.Binding.Ports)
	})
}
