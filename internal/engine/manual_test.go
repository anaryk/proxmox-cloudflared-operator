package engine

import (
	"net/netip"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// withAllowList lets manual routes point at addresses in 10.0.5.0/24 and at
// the node's own network.
func withAllowList(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.settings(func(s *store.Settings) {
		s.TrustedCIDRs = []netip.Prefix{netip.MustParsePrefix("10.0.5.0/24"), netip.MustParsePrefix("10.0.0.0/30")}
	})
	return e
}

func statusRoute() ManualRouteView {
	return ManualRouteView{
		ID:       "status",
		Hostname: "Status.Example.com",
		Target:   ManualTarget{Kind: "address", Scheme: "http", Addr: netip.MustParseAddr("10.0.5.20"), Port: 9000},
	}
}

func TestAManualRouteIsMadeWithTheIDItIsGiven(t *testing.T) {
	e := withAllowList(t)

	got, err := e.eng.CreateManualRoute(WithActor(t.Context(), "alice@pve (ticket)"), statusRoute())

	require.NoError(t, err)
	want := statusRoute()
	want.Rev, want.Hostname = 1, "status.example.com"
	require.Equal(t, want, got)
	routes, err := e.eng.ManualRoutes()
	require.NoError(t, err)
	require.Equal(t, []ManualRouteView{want}, routes)

	events := e.eng.Events(time.Time{})
	ev := events[len(events)-1]
	require.Equal(t, kindAdmin, ev.Kind)
	require.Equal(t, "alice@pve (ticket)", ev.Actor)
	require.Equal(t, "status.example.com", ev.Route)
	require.Equal(t, "manual route manual/status is made: status.example.com -> http://10.0.5.20:9000", ev.Message)
}

func TestAManualRouteWithoutAnIDGetsOne(t *testing.T) {
	e := withAllowList(t)
	v := statusRoute()
	v.ID = ""

	got, err := e.eng.CreateManualRoute(t.Context(), v)

	require.NoError(t, err)
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{8}$`), got.ID)
	routes, err := e.eng.ManualRoutes()
	require.NoError(t, err)
	require.Len(t, routes, 1)
	require.Equal(t, got.ID, routes[0].ID)
}

func TestAManualRouteToAGuest(t *testing.T) {
	e := withAllowList(t)
	v := ManualRouteView{
		ID: "wiki", Hostname: "wiki.example.com",
		Target:  ManualTarget{Kind: "guest", Guest: "qemu/101", Scheme: "https", Port: 8443},
		Options: model.RouteOptions{NoTLSVerify: true, Via: "NET1"},
	}

	got, err := e.eng.CreateManualRoute(t.Context(), v)

	require.NoError(t, err)
	require.Equal(t, "net1", got.Options.Via)
	routes, err := e.store.ManualRoutes()
	require.NoError(t, err)
	require.Equal(t, []model.Route{{
		Hostname: "wiki.example.com", ManualID: "wiki", Source: model.SourceManual,
		Guest:   &model.GuestRef{Kind: model.KindQEMU, VMID: 101},
		Target:  model.Target{Scheme: model.SchemeHTTPS, Port: 8443},
		Options: model.RouteOptions{NoTLSVerify: true, Via: "net1"},
	}}, routes)
}

func TestManualRoutesThatAreNotValidAreRefusedWithTheirField(t *testing.T) {
	for _, tt := range []struct {
		name, field string
		change      func(*ManualRouteView)
	}{
		{"an id with capitals", "id", func(v *ManualRouteView) { v.ID = "Status" }},
		{"an id too long", "id", func(v *ManualRouteView) { v.ID = "a123456789012345678901234567890123" }},
		{"an id with a slash", "id", func(v *ManualRouteView) { v.ID = "a/b" }},
		{"a bad hostname", "hostname", func(v *ManualRouteView) { v.Hostname = "not a host" }},
		{"no hostname", "hostname", func(v *ManualRouteView) { v.Hostname = "" }},
		{"an unknown kind", "target.kind", func(v *ManualRouteView) { v.Target.Kind = "vm" }},
		{"an unknown scheme", "target.scheme", func(v *ManualRouteView) { v.Target.Scheme = "tcp" }},
		{"no port", "target.port", func(v *ManualRouteView) { v.Target.Port = 0 }},
		{"no address", "target.addr", func(v *ManualRouteView) { v.Target.Addr = netip.Addr{} }},
		{"an IPv6 address", "target.addr", func(v *ManualRouteView) { v.Target.Addr = netip.MustParseAddr("fd00::1") }},
		{"an address outside the allow-list", "target.addr", func(v *ManualRouteView) { v.Target.Addr = netip.MustParseAddr("192.168.1.10") }},
		{"an address target with a guest", "target.guest", func(v *ManualRouteView) { v.Target.Guest = "qemu/101" }},
		{"a guest that is no guest", "target.guest", func(v *ManualRouteView) {
			v.Target = ManualTarget{Kind: "guest", Guest: "vm/1", Scheme: "http", Port: 80}
		}},
		{"a guest target with an address", "target.addr", func(v *ManualRouteView) {
			v.Target = ManualTarget{Kind: "guest", Guest: "qemu/101", Scheme: "http", Addr: netip.MustParseAddr("10.0.5.20"), Port: 80}
		}},
		{"allowNode for a guest", "options.allowNode", func(v *ManualRouteView) {
			v.Target = ManualTarget{Kind: "guest", Guest: "qemu/101", Scheme: "http", Port: 80}
			v.Options.AllowNode = true
		}},
		{"via with an address", "options.via", func(v *ManualRouteView) { v.Options.Via = "net0" }},
		{"no-tls-verify on http", "options.noTLSVerify", func(v *ManualRouteView) { v.Options.NoTLSVerify = true }},
		{"a bad host header", "options.hostHeader", func(v *ManualRouteView) { v.Options.HostHeader = "a b" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := withAllowList(t)
			v := statusRoute()
			tt.change(&v)

			_, err := e.eng.CreateManualRoute(t.Context(), v)

			var fe *FieldError
			require.ErrorAs(t, err, &fe)
			require.Equal(t, tt.field, fe.Field, err.Error())
			require.ErrorIs(t, err, ErrInvalid)
			routes, err := e.store.ManualRoutes()
			require.NoError(t, err)
			require.Empty(t, routes)
		})
	}
}

func TestAManualRouteToANodeNeedsAllowNode(t *testing.T) {
	e := withAllowList(t)
	e.cycle() // the node's address is known from the listing
	v := statusRoute()
	v.Target.Addr = nodeAddr

	_, err := e.eng.CreateManualRoute(t.Context(), v)
	var fe *FieldError
	require.ErrorAs(t, err, &fe)
	require.Equal(t, "target.addr", fe.Field)
	require.Contains(t, err.Error(), "allowNode")

	v.Options.AllowNode = true
	got, err := e.eng.CreateManualRoute(t.Context(), v)
	require.NoError(t, err)
	require.True(t, got.Options.AllowNode)
}

func TestAManualRouteIDIsTakenOnce(t *testing.T) {
	e := withAllowList(t)
	_, err := e.eng.CreateManualRoute(t.Context(), statusRoute())
	require.NoError(t, err)

	again := statusRoute()
	again.Hostname = "other.example.com"
	_, err = e.eng.CreateManualRoute(t.Context(), again)

	require.ErrorIs(t, err, ErrRefused)
	require.Contains(t, err.Error(), "manual/status")
	routes, err := e.eng.ManualRoutes()
	require.NoError(t, err)
	require.Len(t, routes, 1)
	require.Equal(t, "status.example.com", routes[0].Hostname)
}

func TestAManualRouteIsChangedAndRemovedAtItsRevision(t *testing.T) {
	e := withAllowList(t)
	made, err := e.eng.CreateManualRoute(t.Context(), statusRoute())
	require.NoError(t, err)

	moved := made
	moved.Target.Port = 9001
	got, err := e.eng.UpdateManualRoute(t.Context(), "status", made.Rev, moved)
	require.NoError(t, err)
	require.Equal(t, 2, got.Rev)
	require.Equal(t, uint16(9001), got.Target.Port)

	_, err = e.eng.UpdateManualRoute(t.Context(), "status", made.Rev, moved)
	require.ErrorIs(t, err, ErrRefused, "a stale revision")
	require.ErrorIs(t, e.eng.DeleteManualRoute(t.Context(), "status", made.Rev), ErrRefused)

	missing := moved
	missing.ID = ""
	_, err = e.eng.UpdateManualRoute(t.Context(), "missing", 1, missing)
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, e.eng.DeleteManualRoute(t.Context(), "missing", 1), ErrNotFound)

	other := got
	other.ID = "renamed"
	_, err = e.eng.UpdateManualRoute(t.Context(), "status", got.Rev, other)
	var fe *FieldError
	require.ErrorAs(t, err, &fe, "the id of a route is not changed")
	require.Equal(t, "id", fe.Field)

	require.NoError(t, e.eng.DeleteManualRoute(WithActor(t.Context(), "root (cli)"), "status", got.Rev))
	routes, err := e.eng.ManualRoutes()
	require.NoError(t, err)
	require.Empty(t, routes)
	events := e.eng.Events(time.Time{})
	require.Equal(t, "manual route manual/status is removed: status.example.com -> http://10.0.5.20:9001", events[len(events)-1].Message)
	require.Equal(t, "root (cli)", events[len(events)-1].Actor)
}

func TestAManualRouteIsARouteOfTheNextCycle(t *testing.T) {
	e := withAllowList(t)
	_, err := e.eng.CreateManualRoute(t.Context(), statusRoute())
	require.NoError(t, err)

	st := e.cycle()

	r := route(st, "status.example.com")
	require.Equal(t, "manual/status", r.Owner)
}
