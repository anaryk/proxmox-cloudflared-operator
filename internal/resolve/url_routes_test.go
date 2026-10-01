package resolve

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

func TestResolveRouteWithoutGuest(t *testing.T) {
	const owner = "manual/app"
	route := func(addr string, allowNode bool) model.Route {
		return model.Route{
			Hostname: "app.example.com",
			Target:   model.Target{Scheme: model.SchemeHTTP, Addr: ip(addr), Port: 8080},
			Options:  model.RouteOptions{AllowNode: allowNode},
			Source:   model.SourceManual,
			ManualID: "app",
		}
	}
	notManual := func(r model.Route) model.Route {
		r.Source = model.SourceAnnotation
		return r
	}
	served := func(addr string) planner.ResolvedTarget {
		return planner.ResolvedTarget{Addr: ip(addr), Reachable: true, Owner: owner}
	}
	rejected := func(reason string) planner.ResolvedTarget {
		return planner.ResolvedTarget{Rejected: true, Reason: reason}
	}
	tests := []struct {
		name   string
		route  model.Route
		target planner.ResolvedTarget
		calls  []string
	}{
		{name: "answering", route: route("10.20.0.50", false), target: served("10.20.0.50"), calls: []string{"interfaces", "dial"}},
		{name: "address of a cluster node", route: route("10.20.0.2", false), target: rejected("address of a cluster node")},
		{
			name: "address of a host interface", route: route("10.30.0.2", false),
			target: rejected("address of this node"), calls: []string{"interfaces"},
		},
		{name: "cluster node allowed", route: route("10.20.0.2", true), target: served("10.20.0.2"), calls: []string{"dial"}},
		{name: "host interface allowed", route: route("10.30.0.2", true), target: served("10.30.0.2"), calls: []string{"dial"}},
		{name: "loopback with allowNode", route: route("127.0.0.1", true), target: rejected("loopback address")},
		{name: "link-local with allowNode", route: route("169.254.0.1", true), target: rejected("link-local address")},
		{name: "reserved node address with allowNode", route: route("10.99.0.1", true), target: rejected("reserved by pco")},
		{name: "no address", route: model.Route{Hostname: "app.example.com", Source: model.SourceManual, ManualID: "app"}, target: rejected("not an IPv4 address")},
		{name: "cluster node allowed outside a manual route", route: notManual(route("10.20.0.2", true)), target: rejected("address of a cluster node")},
		{
			name: "host interface allowed outside a manual route", route: notManual(route("10.30.0.2", true)),
			target: rejected("address of this node"), calls: []string{"interfaces"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.deny = denylist(t, ips("10.20.0.2", "10.99.0.1"), prefixes("10.99.0.0/24"))
			s.prober.ifaces = append(s.prober.ifaces, HostIface{Name: "vmbr1", Addrs: prefixes("10.30.0.2/24")})

			res := s.resolve(t, tt.route, &Binding{Owner: owner, Hostname: "app.example.com", Addr: ip("10.20.0.51")})

			require.Equal(t, tt.target, res.Target)
			require.Nil(t, res.Binding)
			var ops []string
			for _, c := range s.prober.calls {
				ops = append(ops, c.op)
			}
			require.Equal(t, tt.calls, ops)
		})
	}

	t.Run("address of a node in the inventory", func(t *testing.T) {
		for _, addr := range []string{"10.20.0.3", "10.20.0.33"} {
			s := newScenario(t)
			s.deny = denylist(t, nil, nil)
			s.nodes[1].Addr = ip("10.20.0.3")
			s.nodes[1].Ifaces = []pve.NodeIface{{Name: "vmbr1", Addrs: prefixes("10.20.0.33/24")}}

			res := s.resolve(t, route(addr, false), nil)
			require.Equal(t, rejected("address of a cluster node"), res.Target, addr)
			require.Empty(t, s.prober.calls)

			res = s.resolve(t, notManual(route(addr, true)), nil)
			require.Equal(t, rejected("address of a cluster node"), res.Target, addr)

			res = s.resolve(t, route(addr, true), nil)
			require.Equal(t, served(addr), res.Target, "allowNode lifts the node rules: %s", addr)
		}
	})

	t.Run("not answering", func(t *testing.T) {
		s := newScenario(t)
		s.prober.dialErr[ip("10.20.0.50")] = errDial

		res := s.resolve(t, route("10.20.0.50", false), nil)

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.50"), Reason: "port 8080: connection refused", Owner: owner}, res.Target)
		require.Equal(t, []CandidateResult{{Addr: ip("10.20.0.50"), Source: FromVia, Reason: "port 8080: connection refused"}}, res.Candidates)
	})

	t.Run("interfaces not listed", func(t *testing.T) {
		s := newScenario(t)
		s.prober.ifacesErr = errors.New("netlink closed")

		res := s.resolve(t, route("10.20.0.50", false), nil)

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.50"), Withdrawn: true, Reason: "listing host interfaces: netlink closed", Owner: owner}, res.Target)
		require.Empty(t, s.prober.ops("dial"))
	})

	t.Run("cancelled", func(t *testing.T) {
		s := newScenario(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		res := s.resolveCtx(ctx, route("10.20.0.50", false), nil)

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.50"), Withdrawn: true, Reason: "resolve cancelled", Owner: owner}, res.Target)
		require.Empty(t, s.prober.calls)
	})

	t.Run("cancelled during the dial", func(t *testing.T) {
		s := newScenario(t)
		ctx, cancel := context.WithCancel(t.Context())
		s.prober.cancelOn, s.prober.cancelSilently, s.prober.cancel = "dial", true, cancel

		res := s.resolveCtx(ctx, route("10.20.0.50", false), nil)

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.20.0.50"), Withdrawn: true, Reason: "resolve cancelled", Owner: owner}, res.Target)
	})

	t.Run("cancelled with a denied address", func(t *testing.T) {
		s := newScenario(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		res := s.resolveCtx(ctx, route("10.20.0.2", false), nil)

		require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"}, res.Target)
	})
}
