package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

func TestResolveStickyBindingDoesNotFlap(t *testing.T) {
	s, prev := stickyScenario(t)
	const failed = "port 80: connection refused"

	for _, after := range []time.Duration{0, 60 * time.Second} {
		s.clock.t = t0.Add(after)
		s.prober.calls = nil

		res := s.resolve(t, webRoute(), prev)

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.11"), Reason: failed, Owner: webOwner}, res.Target, "after %s", after)
		require.Equal(t, onPort(atLevel(provenAt(failingAt(boundTo("10.20.0.11"), t0), s.clock.t), LevelPort)), res.Binding, "after %s", after)
		require.Equal(t, []CandidateResult{
			{Addr: ip("10.20.0.11"), Source: FromStatic, Reason: failed, Level: "port"},
			{Addr: ip("10.20.0.10"), Source: FromStatic, Reason: "not tried"},
		}, res.Candidates)
		require.False(t, s.prober.touched(ip("10.20.0.10")), "after %s", after)
		prev = res.Binding
	}

	s.clock.t = t0.Add(121 * time.Second)
	res := s.resolve(t, webRoute(), prev)

	requireServed(t, res, "10.20.0.10", t0.Add(121*time.Second))
	require.Equal(t, []CandidateResult{
		{Addr: ip("10.20.0.11"), Source: FromStatic, Reason: failed, Level: "port"},
		{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true, Level: "port"},
	}, res.Candidates)
}

func TestResolveStickyBindingKeptWithoutAlternative(t *testing.T) {
	s, prev := stickyScenario(t)
	s.prober.dialErr[ip("10.20.0.10")] = errDial
	prev.FailingSince = timePtr(t0)
	s.clock.t = t0.Add(10 * time.Minute)

	res := s.resolve(t, webRoute(), prev)

	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.11"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)
	require.Equal(t, onPort(atLevel(provenAt(prev, s.clock.t), LevelPort)), res.Binding)
	require.Equal(t, []CandidateResult{
		{Addr: ip("10.20.0.11"), Source: FromStatic, Reason: "port 80: connection refused", Level: "port"},
		{Addr: ip("10.20.0.10"), Source: FromStatic, Reason: "port 80: connection refused", Level: "port"},
	}, res.Candidates)
}

func TestResolveStickyFor(t *testing.T) {
	s, prev := stickyScenario(t)
	s.settings.StickyFor = 30 * time.Second
	prev.FailingSince = timePtr(t0)
	s.clock.t = t0.Add(30 * time.Second)

	res := s.resolve(t, webRoute(), prev)

	requireServed(t, res, "10.20.0.10", s.clock.t)
}

func TestResolveBindingRecovers(t *testing.T) {
	s := newScenario(t)
	prev := failingAt(boundTo("10.20.0.10"), t0.Add(-time.Minute))

	res := s.resolve(t, webRoute(), prev)

	requireServed(t, res, "10.20.0.10", t0)
	require.Equal(t, timePtr(t0.Add(-time.Minute)), prev.FailingSince, "prev must not change")
}

func TestResolveIdentityLossWithdrawsBinding(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(s *scenario)
		reason string
	}{
		{
			name:   "foreign MAC answers",
			setup:  func(s *scenario) { s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{foreignMAC} },
			reason: "10.20.0.10 answered by bc:24:11:ff:ff:01, which is not this guest",
		},
		{
			name:   "second MAC answers",
			setup:  func(s *scenario) { s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0, foreignMAC} },
			reason: "10.20.0.10 answered by bc:24:11:ff:ff:01, which is not this guest",
		},
		{
			name:   "no ARP answer",
			setup:  func(s *scenario) { delete(s.prober.arp, arpKey("vmbr0", "10.20.0.10")) },
			reason: "no ARP answer on vmbr0",
		},
		{
			name:   "MAC on another port",
			setup:  func(s *scenario) { s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"tap102i0"} },
			reason: "MAC bc:24:11:00:00:01 is on port tap102i0, not on the guest's own port",
		},
		{
			name:   "MAC not learned",
			setup:  func(s *scenario) { delete(s.prober.fdb, fdbKey("vmbr0", 0, mac0)) },
			reason: "MAC bc:24:11:00:00:01 not seen on bridge vmbr0",
		},
		{
			name:   "no longer adjacent",
			setup:  func(s *scenario) { s.prober.ifaces = nil },
			reason: "node has no address on vmbr0 in the guest's network",
		},
		{
			name:   "MAC on several ports",
			setup:  func(s *scenario) { s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"tap101i0", "tap102i0"} },
			reason: "MAC bc:24:11:00:00:01 is on several ports: tap101i0, tap102i0",
		},
		{
			name:   "route through another interface",
			setup:  func(s *scenario) { s.prober.routes["10.20.0.10"] = fakeRoute{iface: "vmbr1", onLink: true} },
			reason: "route to 10.20.0.10 leaves through vmbr1, not vmbr0",
		},
		{
			name:   "route through a gateway",
			setup:  func(s *scenario) { s.prober.routes["10.20.0.10"] = fakeRoute{iface: "vmbr0"} },
			reason: "route to 10.20.0.10 leaves through a gateway",
		},
		{
			name: "no route",
			setup: func(s *scenario) {
				s.prober.routes["10.20.0.10"] = fakeRoute{err: fmt.Errorf("route to 10.20.0.10: %w", ErrNoRoute)}
			},
			reason: "node has no route to 10.20.0.10",
		},
		{
			name: "route that changes with the source address",
			setup: func(s *scenario) {
				s.prober.routes["10.20.0.10"] = fakeRoute{err: fmt.Errorf("route to 10.20.0.10 from 10.20.0.2: %w", ErrRouteDiffers)}
			},
			reason: "route to 10.20.0.10 changes with the source address",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			tt.setup(s)
			prev := boundTo("10.20.0.10")

			res := s.resolve(t, webRoute(), prev)

			requireWithdrawn(t, res, tt.reason, withdrawnAt(boundTo("10.20.0.10"), t0))
			require.Nil(t, prev.FailingSince, "prev must not change")
			require.Empty(t, s.prober.ops("dial"))
		})
	}
}

func TestResolveIdentityLossKeepsFailingSince(t *testing.T) {
	s := newScenario(t)
	s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{foreignMAC}
	prev := failingAt(boundTo("10.20.0.10"), t0.Add(-30*time.Second))

	res := s.resolve(t, webRoute(), prev)

	requireWithdrawn(t, res, "10.20.0.10 answered by bc:24:11:ff:ff:01, which is not this guest",
		withdrawnAt(boundTo("10.20.0.10"), t0.Add(-30*time.Second)))
}

func TestResolveIdentityLossTriesOthersAtOnce(t *testing.T) {
	const foreign = "10.20.0.11 answered by bc:24:11:ff:ff:01, which is not this guest"

	t.Run("an alternative verifies", func(t *testing.T) {
		s := newScenario(t)
		s.web().NICs[0].Static = ips("10.20.0.10", "10.20.0.11")
		s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{foreignMAC}

		res := s.resolve(t, webRoute(), boundTo("10.20.0.11"))

		requireServed(t, res, "10.20.0.10", t0)
		require.Equal(t, []CandidateResult{
			{Addr: ip("10.20.0.11"), Source: FromStatic, Reason: foreign},
			{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true, Level: "port"},
		}, res.Candidates)
	})

	t.Run("nothing else verifies", func(t *testing.T) {
		s := newScenario(t)
		s.web().NICs[0].Static = ips("10.20.0.10", "10.20.0.11")
		s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{foreignMAC}
		s.prober.dialErr[ip("10.20.0.10")] = errDial

		res := s.resolve(t, webRoute(), boundTo("10.20.0.11"))

		requireWithdrawn(t, res, foreign, withdrawnAt(boundTo("10.20.0.11"), t0))
		require.True(t, s.prober.touched(ip("10.20.0.10")))
	})
}

func TestResolveWithdrawnUntilIdentityPasses(t *testing.T) {
	s := newScenario(t)
	const foreign = "10.20.0.10 answered by bc:24:11:ff:ff:01, which is not this guest"

	// Identity lost.
	s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{foreignMAC}
	res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))
	requireWithdrawn(t, res, foreign, withdrawnAt(boundTo("10.20.0.10"), t0))

	// Guest stopped: still withdrawn, nothing probed.
	s.clock.t = t0.Add(10 * time.Second)
	s.web().Running = false
	s.prober.calls = nil
	res = s.resolve(t, webRoute(), res.Binding)
	requireWithdrawn(t, res, "guest is not running", withdrawnAt(boundTo("10.20.0.10"), t0))
	require.Empty(t, s.prober.calls)

	// Running again, but the host cannot be asked: still withdrawn, although
	// the last proof is fresh.
	s.clock.t = t0.Add(20 * time.Second)
	s.web().Running = true
	s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0}
	s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = errors.New("socket closed")
	res = s.resolve(t, webRoute(), res.Binding)
	requireWithdrawn(t, res, "ARP on vmbr0: socket closed", withdrawnAt(boundTo("10.20.0.10"), t0))

	// Identity holds but the port does not answer: no longer withdrawn.
	s.clock.t = t0.Add(30 * time.Second)
	delete(s.prober.arpErr, arpKey("vmbr0", "10.20.0.10"))
	s.prober.dialErr[ip("10.20.0.10")] = errDial
	res = s.resolve(t, webRoute(), res.Binding)
	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)
	require.Equal(t, onPort(atLevel(provenAt(failingAt(boundTo("10.20.0.10"), t0), t0.Add(30*time.Second)), LevelPort)), res.Binding)

	// Fully verified: served and reachable.
	s.clock.t = t0.Add(40 * time.Second)
	delete(s.prober.dialErr, ip("10.20.0.10"))
	res = s.resolve(t, webRoute(), res.Binding)
	requireServed(t, res, "10.20.0.10", t0.Add(40*time.Second))
}

func TestResolveTakeoverDuringLaterCandidate(t *testing.T) {
	s, prev := stickyScenario(t)
	s.prober.dialErr[ip("10.20.0.10")] = errDial

	// The port closes.
	res := s.resolve(t, webRoute(), prev)
	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.11"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)

	// Still closed beyond StickyFor; the other candidate does not answer either.
	s.clock.t = t0.Add(3 * time.Minute)
	res = s.resolve(t, webRoute(), res.Binding)
	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.11"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)
	require.True(t, s.prober.touched(ip("10.20.0.10")))

	// Another machine takes the address over, and the context ends while the
	// other candidate is being tried.
	s.clock.t = t0.Add(4 * time.Minute)
	s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{foreignMAC}
	ctx, cancel := context.WithCancel(t.Context())
	s.prober.cancelOn, s.prober.cancelAddr, s.prober.cancel = "arp", ip("10.20.0.10"), cancel
	res = s.resolveCtx(ctx, webRoute(), res.Binding)

	want := onPort(atLevel(withdrawnAt(provenAt(boundTo("10.20.0.11"), t0.Add(3*time.Minute)), t0), LevelPort))
	requireWithdrawn(t, res, "10.20.0.11 answered by bc:24:11:ff:ff:01, which is not this guest", want)
}

func TestResolveIdentityClearsWithdrawn(t *testing.T) {
	s := newScenario(t)
	s.prober.dialErr[ip("10.20.0.10")] = errDial
	prev := withdrawnAt(boundTo("10.20.0.10"), t0.Add(-time.Minute))

	res := s.resolve(t, webRoute(), prev)

	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)
	require.Equal(t, onPort(atLevel(provenAt(failingAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)), t0), LevelPort)), res.Binding)
}

func TestResolveCapNotReportedWhenStickyKeepsBinding(t *testing.T) {
	s, prev := stickyScenario(t)
	var static []string
	for i := range 20 {
		static = append(static, fmt.Sprintf("10.20.0.%d", 100+i))
	}
	s.web().NICs[0].Static = append(s.web().NICs[0].Static, ips(static...)...)

	res := s.resolve(t, webRoute(), prev)

	require.Equal(t, "port 80: connection refused", res.Target.Reason)
}

func TestResolveGuestNotRunning(t *testing.T) {
	const reason = "guest is not running"
	tests := []struct {
		name    string
		prev    *Binding
		target  planner.ResolvedTarget
		binding *Binding
	}{
		{
			name:    "with a binding",
			prev:    boundTo("10.20.0.10"),
			target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: reason, Owner: webOwner},
			binding: withdrawnAt(boundTo("10.20.0.10"), t0),
		},
		{
			name:    "with a failing binding",
			prev:    failingAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
			target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: reason, Owner: webOwner},
			binding: withdrawnAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
		},
		{
			name:   "without a binding",
			target: planner.ResolvedTarget{Reason: reason},
		},
		{
			name:   "with a binding to a denied address",
			prev:   boundTo("10.20.0.2"),
			target: planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.web().Running = false

			res := s.resolve(t, webRoute(), tt.prev)

			require.Equal(t, tt.target, res.Target)
			require.Equal(t, tt.binding, res.Binding)
			require.Empty(t, s.prober.calls)
		})
	}
}

func TestResolveGuestNotFound(t *testing.T) {
	const reason = "guest not found in inventory"
	tests := []struct {
		name    string
		prev    *Binding
		target  planner.ResolvedTarget
		binding *Binding
	}{
		{
			name:    "with a binding",
			prev:    boundTo("10.20.0.10"),
			target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: reason, Owner: webOwner},
			binding: withdrawnAt(boundTo("10.20.0.10"), t0),
		},
		{
			name:   "without a binding",
			target: planner.ResolvedTarget{Reason: reason},
		},
		{
			name:   "with a binding to a denied address",
			prev:   boundTo("10.20.0.2"),
			target: planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.guests = nil

			res := s.resolve(t, webRoute(), tt.prev)

			require.Equal(t, tt.target, res.Target)
			require.Equal(t, tt.binding, res.Binding)
			require.Empty(t, s.prober.calls)
		})
	}
}

// otherRoutes edit a binding of this route into one of another route.
var otherRoutes = []struct {
	name string
	edit func(b *Binding)
}{
	{"another owner", func(b *Binding) { b.Owner = db1Ref.String() }},
	{"another hostname", func(b *Binding) { b.Hostname = "db.example.com" }},
	{"another guest", func(b *Binding) { b.Guest = db1Ref.String() }},
}

func TestResolveIgnoresBindingOfAnotherRoute(t *testing.T) {
	for _, tt := range otherRoutes {
		t.Run(tt.name+" on a stopped guest", func(t *testing.T) {
			s := newScenario(t)
			s.web().Running = false
			prev := boundTo("10.20.0.10")
			tt.edit(prev)

			res := s.resolve(t, webRoute(), prev)

			requireNotServed(t, res, "guest is not running")
		})
		t.Run(tt.name+" on a running guest", func(t *testing.T) {
			s := newScenario(t)
			s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}
			prev := boundTo("10.20.0.11")
			tt.edit(prev)

			res := s.resolve(t, webRoute(), prev)

			requireServed(t, res, "10.20.0.10", t0)
			require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true, Level: "port"}}, res.Candidates)
			require.False(t, s.prober.touched(ip("10.20.0.11")))
		})
	}
}

func TestResolveBindingOutlivesSources(t *testing.T) {
	t.Run("no source reports the address", func(t *testing.T) {
		s := newScenario(t)
		s.web().NICs[0].Static = nil

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireServed(t, res, "10.20.0.10", t0)
		require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.10"), Source: FromBinding, OK: true, Level: "port"}}, res.Candidates)
	})

	t.Run("verified before the other candidates", func(t *testing.T) {
		s := newScenario(t)
		s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}

		res := s.resolve(t, webRoute(), boundTo("10.20.0.11"))

		requireServed(t, res, "10.20.0.11", t0)
		require.Equal(t, []CandidateResult{
			{Addr: ip("10.20.0.11"), Source: FromBinding, OK: true, Level: "port"},
			{Addr: ip("10.20.0.10"), Source: FromStatic, Reason: "not tried"},
		}, res.Candidates)
	})

	t.Run("on the NIC that has the MAC", func(t *testing.T) {
		s := newScenario(t)
		s.web().NICs = []model.NIC{nicOn(0, mac1, "vmbr1", 0), nicOn(1, mac0, "vmbr0", 0)}
		s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"tap101i1"}

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireServed(t, res, "10.20.0.10", t0)
	})

	t.Run("unreported address that fails identity gives way", func(t *testing.T) {
		s := newScenario(t)

		res := s.resolve(t, webRoute(), boundTo("10.20.0.99"))

		requireServed(t, res, "10.20.0.10", t0)
		require.Equal(t, []CandidateResult{
			{Addr: ip("10.20.0.99"), Source: FromBinding, Reason: "no ARP answer on vmbr0"},
			{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true, Level: "port"},
		}, res.Candidates)
	})

	t.Run("unreported address that fails identity with nothing else", func(t *testing.T) {
		s := newScenario(t)
		s.web().NICs[0].Static = nil

		res := s.resolve(t, webRoute(), boundTo("10.20.0.99"))

		requireWithdrawn(t, res, "no ARP answer on vmbr0", withdrawnAt(boundTo("10.20.0.99"), t0))
	})
}

func TestResolveBindingDropped(t *testing.T) {
	tests := []struct {
		name  string
		route model.Route
		edit  func(s *scenario)
	}{
		{
			name:  "no NIC has the MAC any more",
			route: webRoute(),
			edit: func(s *scenario) {
				s.web().NICs[0].MAC = mac2
				s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac2}
				s.prober.fdb[fdbKey("vmbr0", 0, mac2)] = []string{"tap101i0"}
			},
		},
		{name: "route names another address", route: routeFor(ip("10.20.0.10"), "")},
		{name: "via names another address", route: routeFor(netip.Addr{}, "10.20.0.10")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}
			if tt.edit != nil {
				tt.edit(s)
			}

			res := s.resolve(t, tt.route, boundTo("10.20.0.11"))

			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Owner: webOwner}, res.Target)
			require.Equal(t, ip("10.20.0.10"), res.Binding.Addr)
			require.Equal(t, s.web().NICs[0].MAC, res.Binding.MAC)
			require.Len(t, res.Candidates, 1)
			require.False(t, s.prober.touched(ip("10.20.0.11")))
		})
	}

	t.Run("kept when the route names the bound address", func(t *testing.T) {
		s := newScenario(t)
		s.prober.dialErr[ip("10.20.0.10")] = errDial

		res := s.resolve(t, routeFor(ip("10.20.0.10"), ""), boundTo("10.20.0.10"))

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)
		require.Equal(t, onPort(atLevel(provenAt(failingAt(boundTo("10.20.0.10"), t0), t0), LevelPort)), res.Binding)
		require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.10"), Source: FromVia, Reason: "port 80: connection refused", Level: "port"}}, res.Candidates)
	})

	t.Run("via names another NIC", func(t *testing.T) {
		s := newScenario(t)
		s.web().NICs = append(s.web().NICs, nicOn(1, mac1, "vmbr0", 0, "10.20.0.11"))
		s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac1}
		s.prober.fdb[fdbKey("vmbr0", 0, mac1)] = []string{"tap101i1"}

		res := s.resolve(t, routeFor(netip.Addr{}, "net1"), boundTo("10.20.0.10"))

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.11"), Reachable: true, Owner: webOwner}, res.Target)
		require.Equal(t, mac1, res.Binding.MAC)
		require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.11"), Source: FromStatic, OK: true, Level: "port"}}, res.Candidates)
		require.False(t, s.prober.touched(ip("10.20.0.10")))
	})

	t.Run("kept when via names the NIC that has the MAC", func(t *testing.T) {
		s := newScenario(t)
		s.web().NICs[0].Static = nil

		res := s.resolve(t, routeFor(netip.Addr{}, "net0"), boundTo("10.20.0.10"))

		requireServed(t, res, "10.20.0.10", t0)
	})
}

func TestResolveAddressMovesToAnotherNIC(t *testing.T) {
	s := newScenario(t)
	s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 0), nicOn(1, mac1, "vmbr1", 0, "10.20.0.10")}
	s.prober.ifaces = append(s.prober.ifaces, HostIface{Name: "vmbr1", Addrs: prefixes("10.20.0.3/24")})
	s.prober.routes["10.20.0.10"] = fakeRoute{iface: "vmbr1", onLink: true}
	delete(s.prober.arp, arpKey("vmbr0", "10.20.0.10"))
	s.prober.arp[arpKey("vmbr1", "10.20.0.10")] = []string{mac1}
	s.prober.fdb[fdbKey("vmbr1", 0, mac1)] = []string{"tap101i1"}

	res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Owner: webOwner}, res.Target)
	require.Equal(t, mac1, res.Binding.MAC)
	require.Equal(t, []CandidateResult{
		{Addr: ip("10.20.0.10"), Source: FromBinding, Reason: "route to 10.20.0.10 leaves through vmbr1, not vmbr0"},
		{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true, Level: "port"},
	}, res.Candidates)
}

// proberErrors fail each prober call that comes before the dial.
var proberErrors = []struct {
	name   string
	setup  func(s *scenario)
	reason string
}{
	{
		name:   "interfaces",
		setup:  func(s *scenario) { s.prober.ifacesErr = errors.New("netlink closed") },
		reason: "listing host interfaces: netlink closed",
	},
	{
		name:   "route",
		setup:  func(s *scenario) { s.prober.routes["10.20.0.10"] = fakeRoute{err: errors.New("netlink closed")} },
		reason: "route to 10.20.0.10: netlink closed",
	},
	{
		name:   "ARP",
		setup:  func(s *scenario) { s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = errors.New("socket closed") },
		reason: "ARP on vmbr0: socket closed",
	},
	{
		name:   "forwarding table",
		setup:  func(s *scenario) { s.prober.fdbErr = errors.New("dump interrupted") },
		reason: "forwarding table of vmbr0: dump interrupted",
	},
}

func TestResolveProberErrorIsNotWithdrawal(t *testing.T) {
	for _, tt := range proberErrors {
		t.Run(tt.name+" on the binding", func(t *testing.T) {
			s := newScenario(t)
			tt.setup(s)

			res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: tt.reason, Owner: webOwner}, res.Target)
			require.Equal(t, failingAt(boundTo("10.20.0.10"), t0), res.Binding)
			require.Empty(t, s.prober.ops("dial"))
		})
		t.Run(tt.name+" without a binding", func(t *testing.T) {
			s := newScenario(t)
			tt.setup(s)

			res := s.resolve(t, webRoute(), nil)

			requireNotServed(t, res, tt.reason)
		})
	}
}

func TestResolveProberErrorAndProofAge(t *testing.T) {
	for _, tt := range proberErrors {
		t.Run(tt.name+" with a fresh proof", func(t *testing.T) {
			s := newScenario(t)
			tt.setup(s)
			prev := provenAt(boundTo("10.20.0.10"), t0.Add(-5*time.Minute))

			res := s.resolve(t, webRoute(), prev)

			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: tt.reason, Owner: webOwner}, res.Target)
			require.Equal(t, failingAt(prev, t0), res.Binding)
		})
		t.Run(tt.name+" with a stale proof", func(t *testing.T) {
			s := newScenario(t)
			tt.setup(s)
			prev := provenAt(boundTo("10.20.0.10"), t0.Add(-5*time.Minute-time.Second))

			res := s.resolve(t, webRoute(), prev)

			requireWithdrawn(t, res, "identity not confirmed for 5m1s", withdrawnAt(prev, t0))
		})
	}

	t.Run("custom MaxProofAge", func(t *testing.T) {
		s := newScenario(t)
		s.settings.MaxProofAge = 30 * time.Second
		s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = errors.New("socket closed")

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, "identity not confirmed for 1m0s", withdrawnAt(boundTo("10.20.0.10"), t0))
	})

	t.Run("stale proof gives way to a verified alternative", func(t *testing.T) {
		s := newScenario(t)
		s.web().NICs[0].Static = ips("10.20.0.10", "10.20.0.11")
		s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}
		s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = errors.New("socket closed")

		res := s.resolve(t, webRoute(), provenAt(boundTo("10.20.0.10"), t0.Add(-10*time.Minute)))

		requireServed(t, res, "10.20.0.11", t0)
	})

	t.Run("fresh proof keeps the binding without trying others", func(t *testing.T) {
		s := newScenario(t)
		s.web().NICs[0].Static = ips("10.20.0.10", "10.20.0.11")
		s.prober.arp[arpKey("vmbr0", "10.20.0.11")] = []string{mac0}
		s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = errors.New("socket closed")

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		require.Equal(t, ip("10.20.0.10"), res.Target.Addr)
		require.False(t, res.Target.Withdrawn)
		require.False(t, s.prober.touched(ip("10.20.0.11")))
	})
}

// throughJSON returns b as it comes back from storage, without a monotonic
// clock reading.
func throughJSON(t *testing.T, b *Binding) *Binding {
	t.Helper()
	data, err := json.Marshal(b)
	require.NoError(t, err)
	var back Binding
	require.NoError(t, json.Unmarshal(data, &back))
	return &back
}

func TestResolveProofDatedInFuture(t *testing.T) {
	const reason = "identity proof is dated in the future"

	t.Run("prober error", func(t *testing.T) {
		s := newScenario(t)
		s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = errors.New("socket closed")
		prev := throughJSON(t, provenAt(boundTo("10.20.0.10"), t0.Add(time.Second)))

		res := s.resolve(t, webRoute(), prev)

		requireWithdrawn(t, res, reason, withdrawnAt(prev, t0))
	})

	t.Run("cancelled on entry", func(t *testing.T) {
		s := newScenario(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		prev := throughJSON(t, provenAt(boundTo("10.20.0.10"), t0.Add(time.Minute)))

		res := s.resolveCtx(ctx, webRoute(), prev)

		want := *prev
		want.Withdrawn = true
		requireWithdrawn(t, res, reason, &want)
		require.Empty(t, s.prober.calls)
	})

	t.Run("renewed when identity passes", func(t *testing.T) {
		s := newScenario(t)
		prev := throughJSON(t, provenAt(boundTo("10.20.0.10"), t0.Add(time.Hour)))

		res := s.resolve(t, webRoute(), prev)

		requireServed(t, res, "10.20.0.10", t0)
	})
}

func TestResolveProberErrorWithSharedMAC(t *testing.T) {
	s := newScenario(t)
	s.addDB1(true, nicOn(0, mac0, "vmbr1", 0))
	s.prober.fdbErr = errors.New("dump interrupted")

	res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

	requireWithdrawn(t, res, "MAC bc:24:11:00:00:01 is also configured on qemu/102", withdrawnAt(boundTo("10.20.0.10"), t0))
}

func TestResolveStickyFailingSinceInTheFuture(t *testing.T) {
	s, prev := stickyScenario(t)
	prev = throughJSON(t, failingAt(prev, t0.Add(time.Hour)))

	res := s.resolve(t, webRoute(), prev)

	requireServed(t, res, "10.20.0.10", t0)
	require.Equal(t, []CandidateResult{
		{Addr: ip("10.20.0.11"), Source: FromStatic, Reason: "port 80: connection refused", Level: "port"},
		{Addr: ip("10.20.0.10"), Source: FromStatic, OK: true, Level: "port"},
	}, res.Candidates, "a binding failing since a time still to come is not sticky")
}

func TestResolveGuestAbsentFromIncompleteSnapshot(t *testing.T) {
	const reason = "guest state unknown"
	stale := provenAt(boundTo("10.20.0.10"), t0.Add(-5*time.Minute-time.Second))
	future := throughJSON(t, provenAt(boundTo("10.20.0.10"), t0.Add(time.Second)))
	tests := []struct {
		name    string
		prev    *Binding
		setup   func(s *scenario)
		target  planner.ResolvedTarget
		binding *Binding
	}{
		{
			name:    "fresh proof",
			prev:    boundTo("10.20.0.10"),
			target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: reason, Owner: webOwner},
			binding: failingAt(boundTo("10.20.0.10"), t0),
		},
		{
			name:    "proof exactly MaxProofAge old",
			prev:    provenAt(boundTo("10.20.0.10"), t0.Add(-5*time.Minute)),
			target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: reason, Owner: webOwner},
			binding: failingAt(provenAt(boundTo("10.20.0.10"), t0.Add(-5*time.Minute)), t0),
		},
		{
			name:    "already failing",
			prev:    failingAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
			target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: reason, Owner: webOwner},
			binding: failingAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
		},
		{
			name:    "stale proof",
			prev:    stale,
			target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: "identity not confirmed for 5m1s", Owner: webOwner},
			binding: withdrawnAt(stale, t0),
		},
		{
			name:    "proof dated in the future",
			prev:    future,
			target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: "identity proof is dated in the future", Owner: webOwner},
			binding: withdrawnAt(future, t0),
		},
		{
			name:    "withdrawn binding stays withdrawn",
			prev:    withdrawnAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
			target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: reason, Owner: webOwner},
			binding: withdrawnAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
		},
		{
			name:  "MAC on another running guest",
			prev:  boundTo("10.20.0.10"),
			setup: func(s *scenario) { s.addDB1(true, nicOn(0, mac0, "vmbr1", 0)) },
			target: planner.ResolvedTarget{
				Addr: ip("10.20.0.10"), Withdrawn: true, Owner: webOwner,
				Reason: "MAC bc:24:11:00:00:01 is also configured on qemu/102",
			},
			binding: withdrawnAt(boundTo("10.20.0.10"), t0),
		},
		{
			name:   "no binding",
			target: planner.ResolvedTarget{Reason: reason},
		},
		{
			name:   "denied binding",
			prev:   boundTo("10.20.0.2"),
			target: planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"},
		},
		{
			name:   "binding to an address of a node",
			prev:   boundTo("10.20.0.3"),
			setup:  func(s *scenario) { s.nodes[1].Addr = ip("10.20.0.3") },
			target: planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.guests = nil
			s.complete = false
			if tt.setup != nil {
				tt.setup(s)
			}

			res := s.resolve(t, webRoute(), tt.prev)

			require.Equal(t, tt.target, res.Target)
			require.Equal(t, tt.binding, res.Binding)
			require.Empty(t, s.prober.calls)
		})
	}

	t.Run("absent from a complete snapshot", func(t *testing.T) {
		s := newScenario(t)
		s.guests = nil

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, "guest not found in inventory", withdrawnAt(boundTo("10.20.0.10"), t0))
	})

	t.Run("present in an incomplete snapshot", func(t *testing.T) {
		s := newScenario(t)
		s.complete = false

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireServed(t, res, "10.20.0.10", t0)
	})

	t.Run("binding of another route", func(t *testing.T) {
		s := newScenario(t)
		s.guests = nil
		s.complete = false
		prev := boundTo("10.20.0.10")
		prev.Owner = db1Ref.String()

		res := s.resolve(t, webRoute(), prev)

		requireNotServed(t, res, reason)
	})
}

func TestResolveGuestStatusUnknown(t *testing.T) {
	const reason = "guest state unknown"
	stale := provenAt(boundTo("10.20.0.10"), t0.Add(-5*time.Minute-time.Second))
	tests := []struct {
		name    string
		prev    *Binding
		setup   func(s *scenario)
		target  planner.ResolvedTarget
		binding *Binding
	}{
		{
			name:    "fresh proof",
			prev:    boundTo("10.20.0.10"),
			target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: reason, Owner: webOwner},
			binding: failingAt(boundTo("10.20.0.10"), t0),
		},
		{
			name:    "stale proof",
			prev:    stale,
			target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: "identity not confirmed for 5m1s", Owner: webOwner},
			binding: withdrawnAt(stale, t0),
		},
		{
			name:    "withdrawn binding stays withdrawn",
			prev:    withdrawnAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
			target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: reason, Owner: webOwner},
			binding: withdrawnAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
		},
		{
			name:  "MAC on another running guest",
			prev:  boundTo("10.20.0.10"),
			setup: func(s *scenario) { s.addDB1(true, nicOn(0, mac0, "vmbr1", 0)) },
			target: planner.ResolvedTarget{
				Addr: ip("10.20.0.10"), Withdrawn: true, Owner: webOwner,
				Reason: "MAC bc:24:11:00:00:01 is also configured on qemu/102",
			},
			binding: withdrawnAt(boundTo("10.20.0.10"), t0),
		},
		{
			name:   "binding whose MAC is on no NIC any more",
			prev:   boundTo("10.20.0.10"),
			setup:  func(s *scenario) { s.web().NICs[0].MAC = mac2 },
			target: planner.ResolvedTarget{Reason: reason},
		},
		{
			name:   "no binding",
			target: planner.ResolvedTarget{Reason: reason},
		},
		{
			name:   "denied binding",
			prev:   boundTo("10.20.0.2"),
			target: planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.web().Running = false
			s.web().StatusUnknown = true
			if tt.setup != nil {
				tt.setup(s)
			}

			res := s.resolve(t, webRoute(), tt.prev)

			require.Equal(t, tt.target, res.Target)
			require.Equal(t, tt.binding, res.Binding)
			require.Empty(t, s.prober.calls, "nothing is probed for a guest whose state is unknown")
		})
	}

	t.Run("invalid route is still rejected", func(t *testing.T) {
		s := newScenario(t)
		s.web().Running = false
		s.web().StatusUnknown = true

		res := s.resolve(t, routeFor(netip.Addr{}, "net9"), boundTo("10.20.0.10"))

		require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: "guest has no net9"}, res.Target)
		require.Nil(t, res.Binding)
	})

	t.Run("cancelled", func(t *testing.T) {
		s := newScenario(t)
		s.web().Running = false
		s.web().StatusUnknown = true
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		res := s.resolveCtx(ctx, webRoute(), boundTo("10.20.0.10"))

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: reason, Owner: webOwner}, res.Target)
		require.Equal(t, failingAt(boundTo("10.20.0.10"), t0), res.Binding)
	})

	t.Run("a known stop still withdraws", func(t *testing.T) {
		s := newScenario(t)
		s.web().Running = false

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, "guest is not running", withdrawnAt(boundTo("10.20.0.10"), t0))
	})
}

func TestResolveForwardingTableWithoutNodes(t *testing.T) {
	const unsure = " (no cluster nodes known)"
	stale := provenAt(boundTo("10.20.0.10"), t0.Add(-5*time.Minute-time.Second))
	tests := []struct {
		name      string
		guestNode string
		localNode string
		nodes     []inventory.Node
		ports     []string
		reason    string
		unproven  bool // a prober error rather than lost identity
	}{
		{
			name: "not learned", guestNode: "pve2", localNode: localNode,
			reason: "MAC bc:24:11:00:00:01 not seen on bridge vmbr0" + unsure, unproven: true,
		},
		{
			name: "learned on the uplink", guestNode: "pve2", localNode: localNode, ports: []string{"eno1"},
			reason: "MAC bc:24:11:00:00:01 is on port eno1, not on the guest's own port" + unsure, unproven: true,
		},
		{
			name: "learned on two uplinks", guestNode: "pve2", localNode: localNode, ports: []string{"eno1", "eno2"},
			reason: "MAC bc:24:11:00:00:01 is on several ports: eno1, eno2" + unsure, unproven: true,
		},
		{
			name: "learned on another local guest's port", guestNode: "pve2", localNode: localNode, ports: []string{"tap102i0"},
			reason: "MAC bc:24:11:00:00:01 is on port tap102i0, not on the guest's own port",
		},
		{
			name: "learned on the uplink and a local guest's port", guestNode: "pve2", localNode: localNode, ports: []string{"eno1", "fwpr102p0"},
			reason: "MAC bc:24:11:00:00:01 is on several ports: eno1, fwpr102p0",
		},
		{
			name: "guest on this node", guestNode: localNode, localNode: localNode,
			reason: "MAC bc:24:11:00:00:01 not seen on bridge vmbr0",
		},
		{
			name: "guest node unknown", guestNode: "", localNode: localNode,
			reason: "MAC bc:24:11:00:00:01 not seen on bridge vmbr0",
		},
		{
			name: "local node unknown", guestNode: "pve2", localNode: "",
			reason: "MAC bc:24:11:00:00:01 not seen on bridge vmbr0",
		},
		{
			name: "local node names none of the listed nodes", guestNode: "pve2", localNode: "pve9",
			nodes:  []inventory.Node{{Name: "pve1"}, {Name: "pve2"}},
			reason: "MAC bc:24:11:00:00:01 not seen on bridge vmbr0",
		},
	}
	for _, tt := range tests {
		run := func(t *testing.T, prev *Binding) Result {
			t.Helper()
			s := newScenario(t)
			s.nodes = tt.nodes
			s.web().Node = tt.guestNode
			s.settings.LocalNode = tt.localNode
			s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = tt.ports
			res := s.resolve(t, webRoute(), prev)
			require.Empty(t, s.prober.ops("dial"))
			return res
		}
		t.Run(tt.name+" without a binding", func(t *testing.T) {
			requireNotServed(t, run(t, nil), tt.reason)
		})
		t.Run(tt.name+" with a fresh binding", func(t *testing.T) {
			res := run(t, boundTo("10.20.0.10"))
			if tt.unproven {
				require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: tt.reason, Owner: webOwner}, res.Target)
				require.Equal(t, failingAt(boundTo("10.20.0.10"), t0), res.Binding)
				return
			}
			requireWithdrawn(t, res, tt.reason, withdrawnAt(boundTo("10.20.0.10"), t0))
		})
		if tt.unproven {
			t.Run(tt.name+" with a stale binding", func(t *testing.T) {
				requireWithdrawn(t, run(t, stale), "identity not confirmed for 5m1s", withdrawnAt(stale, t0))
			})
		}
	}

	t.Run("own port still proves identity", func(t *testing.T) {
		s := newScenario(t)
		s.nodes = nil
		s.web().Node = "pve2"

		res := s.resolve(t, webRoute(), nil)

		requireServed(t, res, "10.20.0.10", t0)
	})
}

// TestResolveIdentityFailureOutranksAProberError has two NICs of the guest
// answer ARP. A forwarding-table answer that proves nothing about the first
// MAC must not hide what the table says about the second.
func TestResolveIdentityFailureOutranksAProberError(t *testing.T) {
	const unproven = "forwarding table of vmbr0: dump interrupted"
	errDump := errors.New("dump interrupted")
	scenarios := []struct {
		name  string
		setup func(s *scenario)
		fine  string // a port that proves nothing wrong about mac1
		wrong string // the identity failure for mac1 on another guest's port
	}{
		{
			name:  "no cluster nodes known",
			setup: func(s *scenario) { s.nodes, s.web().Node = nil, "pve2" },
			fine:  "tap101i1",
			wrong: "MAC bc:24:11:00:00:02 is on port tap102i0, not on the guest's own port",
		},
		{
			name:  "guest on this node",
			setup: func(*scenario) {},
			fine:  "tap101i1",
			wrong: "MAC bc:24:11:00:00:02 is on port tap102i0, not on the guest's own port",
		},
		{
			name:  "guest on another node",
			setup: func(s *scenario) { s.web().Node = "pve2" },
			fine:  "eno1",
			wrong: "MAC bc:24:11:00:00:02 is on local port tap102i0 but the guest runs on pve2",
		},
	}
	for _, sc := range scenarios {
		setup := func(t *testing.T, first func(s *scenario), second []string) *scenario {
			s := newScenario(t)
			sc.setup(s)
			s.web().NICs = append(s.web().NICs, nicOn(1, mac1, "vmbr0", 0))
			s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0, mac1}
			first(s)
			s.prober.fdb[fdbKey("vmbr0", 0, mac1)] = second
			return s
		}
		firstErrors := func(s *scenario) { s.prober.fdbErrs[fdbKey("vmbr0", 0, mac0)] = errDump }

		t.Run(sc.name+": first errors, second on another guest's port", func(t *testing.T) {
			for _, prev := range []*Binding{nil, boundTo("10.20.0.10")} {
				s := setup(t, firstErrors, []string{"tap102i0"})

				res := s.resolve(t, webRoute(), prev)

				require.Len(t, s.prober.ops("fdb"), 2, "every answering MAC is looked up")
				if prev == nil {
					requireNotServed(t, res, sc.wrong)
					continue
				}
				requireWithdrawn(t, res, sc.wrong, withdrawnAt(boundTo("10.20.0.10"), t0))
			}
		})

		t.Run(sc.name+": first errors, second fine", func(t *testing.T) {
			s := setup(t, firstErrors, []string{sc.fine})
			res := s.resolve(t, webRoute(), nil)
			requireNotServed(t, res, unproven)

			s = setup(t, firstErrors, []string{sc.fine})
			res = s.resolve(t, webRoute(), boundTo("10.20.0.10"))
			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: unproven, Owner: webOwner}, res.Target)
			require.Equal(t, failingAt(boundTo("10.20.0.10"), t0), res.Binding)

			stale := provenAt(boundTo("10.20.0.10"), t0.Add(-5*time.Minute-time.Second))
			s = setup(t, firstErrors, []string{sc.fine})
			res = s.resolve(t, webRoute(), stale)
			requireWithdrawn(t, res, "identity not confirmed for 5m1s", withdrawnAt(stale, t0))
		})
	}

	t.Run("no cluster nodes known: first proves nothing, second on another guest's port", func(t *testing.T) {
		s := newScenario(t)
		s.nodes, s.web().Node = nil, "pve2"
		s.web().NICs = append(s.web().NICs, nicOn(1, mac1, "vmbr0", 0))
		s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{mac0, mac1}
		s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"eno1"}
		s.prober.fdb[fdbKey("vmbr0", 0, mac1)] = []string{"tap102i0"}

		res := s.resolve(t, webRoute(), boundTo("10.20.0.10"))

		requireWithdrawn(t, res, "MAC bc:24:11:00:00:02 is on port tap102i0, not on the guest's own port",
			withdrawnAt(boundTo("10.20.0.10"), t0))
	})
}
