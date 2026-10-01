package resolve

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

func TestResolveCancelled(t *testing.T) {
	const reason = "resolve cancelled"
	cancelled := func(t *testing.T) context.Context {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		return ctx
	}

	t.Run("before resolving", func(t *testing.T) {
		stale := provenAt(boundTo("10.20.0.10"), t0.Add(-10*time.Minute))
		staleWithdrawn := *stale
		staleWithdrawn.Withdrawn = true
		tests := []struct {
			name       string
			running    bool
			missing    bool
			incomplete bool
			prev       *Binding
			target     planner.ResolvedTarget
			binding    *Binding
		}{
			{
				name: "fresh binding", running: true, prev: boundTo("10.20.0.10"),
				target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Reason: reason, Owner: webOwner},
				binding: boundTo("10.20.0.10"),
			},
			{
				name: "failing binding", running: true, prev: failingAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
				target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: reason, Owner: webOwner},
				binding: failingAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
			},
			{
				name: "withdrawn binding", running: true, prev: withdrawnAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
				target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: reason, Owner: webOwner},
				binding: withdrawnAt(boundTo("10.20.0.10"), t0.Add(-time.Minute)),
			},
			{
				name: "stale binding", running: true, prev: stale,
				target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: "identity not confirmed for 10m0s", Owner: webOwner},
				binding: &staleWithdrawn,
			},
			{
				name: "stopped guest", prev: boundTo("10.20.0.10"),
				target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: "guest is not running", Owner: webOwner},
				binding: withdrawnAt(boundTo("10.20.0.10"), t0),
			},
			{
				name: "missing guest", missing: true, prev: boundTo("10.20.0.10"),
				target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Withdrawn: true, Reason: "guest not found in inventory", Owner: webOwner},
				binding: withdrawnAt(boundTo("10.20.0.10"), t0),
			},
			{
				name: "missing guest in an incomplete snapshot", missing: true, incomplete: true, prev: boundTo("10.20.0.10"),
				target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reason: "guest state unknown", Owner: webOwner},
				binding: failingAt(boundTo("10.20.0.10"), t0),
			},
			{name: "no binding", running: true, target: planner.ResolvedTarget{Reason: reason}},
			{
				name: "denied binding", running: true, prev: boundTo("10.20.0.2"),
				target: planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				s := newScenario(t)
				s.web().Running = tt.running
				if tt.missing {
					s.guests = nil
				}
				s.complete = !tt.incomplete

				res := s.resolveCtx(cancelled(t), webRoute(), tt.prev)

				require.Equal(t, tt.target, res.Target)
				require.Equal(t, tt.binding, res.Binding)
				require.Empty(t, s.prober.calls)
			})
		}
	})

	for _, op := range []string{"interfaces", "route", "arp", "fdb", "dial"} {
		t.Run("during "+op+" of the binding", func(t *testing.T) {
			s := newScenario(t)
			ctx, cancel := context.WithCancel(t.Context())
			s.prober.cancelOn, s.prober.cancel = op, cancel

			res := s.resolveCtx(ctx, webRoute(), boundTo("10.20.0.10"))

			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Reason: reason, Owner: webOwner}, res.Target)
			require.Equal(t, boundTo("10.20.0.10"), res.Binding, "no FailingSince stamp")
			require.Len(t, s.prober.ops(op), 1, "nothing is asked after the cancel")
			require.Equal(t, op, s.prober.calls[len(s.prober.calls)-1].op)
		})
	}

	t.Run("answer after the end is no proof", func(t *testing.T) {
		for _, op := range []string{"interfaces", "route", "arp", "fdb", "dial"} {
			t.Run(op, func(t *testing.T) {
				s := newScenario(t)
				ctx, cancel := context.WithCancel(t.Context())
				s.prober.cancelOn, s.prober.cancelSilently, s.prober.cancel = op, true, cancel
				prev := withdrawnAt(boundTo("10.20.0.10"), t0.Add(-time.Minute))

				res := s.resolveCtx(ctx, webRoute(), prev)

				requireWithdrawn(t, res, reason, prev)
				require.Equal(t, op, s.prober.calls[len(s.prober.calls)-1].op, "nothing is asked after the end")
			})
		}
	})

	t.Run("negative answer after the end is not used", func(t *testing.T) {
		tests := []struct {
			op    string
			setup func(s *scenario)
		}{
			{"interfaces", func(s *scenario) { s.prober.ifaces = nil }},
			{"route", func(s *scenario) { s.prober.routes["10.20.0.10"] = fakeRoute{iface: "vmbr1", onLink: true} }},
			{"arp", func(s *scenario) { s.prober.arp[arpKey("vmbr0", "10.20.0.10")] = []string{foreignMAC} }},
			{"fdb", func(s *scenario) { s.prober.fdb[fdbKey("vmbr0", 0, mac0)] = []string{"tap102i0"} }},
		}
		for _, tt := range tests {
			t.Run(tt.op, func(t *testing.T) {
				s := newScenario(t)
				tt.setup(s)
				ctx, cancel := context.WithCancel(t.Context())
				s.prober.cancelOn, s.prober.cancelSilently, s.prober.cancel = tt.op, true, cancel

				res := s.resolveCtx(ctx, webRoute(), boundTo("10.20.0.10"))

				require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Reason: reason, Owner: webOwner}, res.Target)
				require.Equal(t, boundTo("10.20.0.10"), res.Binding)
			})
		}
	})

	t.Run("answer after the end without a binding", func(t *testing.T) {
		s := newScenario(t)
		ctx, cancel := context.WithCancel(t.Context())
		s.prober.cancelOn, s.prober.cancelSilently, s.prober.cancel = "dial", true, cancel

		res := s.resolveCtx(ctx, webRoute(), nil)

		requireNotServed(t, res, reason)
	})

	t.Run("while trying other candidates after a dial failure", func(t *testing.T) {
		s, prev := stickyScenario(t)
		prev.FailingSince = timePtr(t0.Add(-time.Hour))
		ctx, cancel := context.WithCancel(t.Context())
		s.prober.cancelOn, s.prober.cancelAddr, s.prober.cancel = "arp", ip("10.20.0.10"), cancel

		res := s.resolveCtx(ctx, webRoute(), prev)

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.11"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)
		require.Equal(t, provenAt(prev, t0), res.Binding)
	})

	t.Run("while trying other candidates after a stale prober error", func(t *testing.T) {
		s := newScenario(t)
		s.web().NICs[0].Static = ips("10.20.0.10", "10.20.0.11")
		s.prober.arpErr[arpKey("vmbr0", "10.20.0.10")] = errors.New("socket closed")
		ctx, cancel := context.WithCancel(t.Context())
		s.prober.cancelOn, s.prober.cancelAddr, s.prober.cancel = "arp", ip("10.20.0.11"), cancel
		prev := provenAt(boundTo("10.20.0.10"), t0.Add(-10*time.Minute))

		res := s.resolveCtx(ctx, webRoute(), prev)

		requireWithdrawn(t, res, "identity not confirmed for 10m0s", withdrawnAt(prev, t0))
	})

	t.Run("without a binding", func(t *testing.T) {
		s := newScenario(t)
		ctx, cancel := context.WithCancel(t.Context())
		s.prober.cancelOn, s.prober.cancel = "dial", cancel

		res := s.resolveCtx(ctx, webRoute(), nil)

		requireNotServed(t, res, reason)
	})
}

func TestResolveApplicabilityBeforeCancel(t *testing.T) {
	const cancelled = "resolve cancelled"
	tests := []struct {
		name    string
		route   model.Route
		setup   func(s *scenario)
		target  planner.ResolvedTarget
		binding func(prev *Binding) *Binding
	}{
		{
			name:    "still applies",
			route:   webRoute(),
			target:  planner.ResolvedTarget{Addr: ip("10.20.0.10"), Reachable: true, Reason: cancelled, Owner: webOwner},
			binding: func(prev *Binding) *Binding { return prev },
		},
		{
			name:   "MAC on no NIC any more",
			route:  webRoute(),
			setup:  func(s *scenario) { s.web().NICs[0].MAC = mac2 },
			target: planner.ResolvedTarget{Reason: cancelled},
		},
		{
			name:   "via names another NIC",
			route:  routeFor(netip.Addr{}, "net1"),
			setup:  func(s *scenario) { s.web().NICs = append(s.web().NICs, nicOn(1, mac1, "vmbr0", 0, "10.20.0.11")) },
			target: planner.ResolvedTarget{Reason: cancelled},
		},
		{
			name:   "route names another address",
			route:  routeFor(ip("10.20.0.11"), ""),
			setup:  func(s *scenario) { s.web().NICs[0].Static = ips("10.20.0.10", "10.20.0.11") },
			target: planner.ResolvedTarget{Reason: cancelled},
		},
		{
			name:   "via names another address",
			route:  routeFor(netip.Addr{}, "10.20.0.11"),
			setup:  func(s *scenario) { s.web().NICs[0].Static = ips("10.20.0.10", "10.20.0.11") },
			target: planner.ResolvedTarget{Reason: cancelled},
		},
		{
			name:   "candidates fail for the route",
			route:  routeFor(netip.Addr{}, "net9"),
			target: planner.ResolvedTarget{Rejected: true, Reason: "guest has no net9"},
		},
		{
			name:  "MAC also on a running guest",
			route: webRoute(),
			setup: func(s *scenario) { s.addDB1(true, nicOn(0, mac0, "vmbr1", 0)) },
			target: planner.ResolvedTarget{
				Addr: ip("10.20.0.10"), Withdrawn: true, Owner: webOwner,
				Reason: "MAC bc:24:11:00:00:01 is also configured on qemu/102",
			},
			binding: func(prev *Binding) *Binding {
				c := *prev
				c.Withdrawn = true
				return &c
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			if tt.setup != nil {
				tt.setup(s)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			prev := boundTo("10.20.0.10")

			res := s.resolveCtx(ctx, tt.route, prev)

			require.Equal(t, tt.target, res.Target)
			if tt.binding == nil {
				require.Nil(t, res.Binding)
			} else {
				require.Equal(t, tt.binding(boundTo("10.20.0.10")), res.Binding)
			}
			require.Empty(t, s.prober.calls)
		})
	}
}
