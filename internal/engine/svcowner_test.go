package engine

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// requireNeverForwarded fails when a sync of the gateway from the one at
// index from on had service forward to addr.
func (e *legEnv) requireNeverForwarded(from int, service, addr netip.Addr) {
	e.t.Helper()
	e.gw.mu.Lock()
	defer e.gw.mu.Unlock()
	for _, s := range e.gw.syncs[from:] {
		for _, svc := range s.services {
			if svc.Service == service {
				require.NotEqual(e.t, addr, svc.Addr, "%s forwards to %s", service, addr)
			}
		}
	}
}

// web-1 drops its route, and its binding goes, but the rule stays at
// Cloudflare, since every write to the account fails, and with it the
// service address. web-1 is deleted, and another guest, of another owner,
// takes 10.50.0.10: it must
// get a service address of its own, and the one web-1's rule still sends a
// connector to must forward to nothing.
func TestAServiceAddressIsNeverHandedToAnotherOwner(t *testing.T) {
	e := newLegEnv(t)
	e.warm()
	st := e.next()
	require.Equal(t, "http://198.18.1.0:8080", route(st, legHost).Service)
	e.cf.FailNext("tunnel.write", 1000, errors.New("the account is frozen"))
	e.inv.set(snapshot(guest(101, "web-1")))
	e.next()
	e.next()
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.NotContains(t, bindings, legHost, "no binding names the old service address any more")
	require.Equal(t, map[string]netip.Addr{"vmbr1 10.50.0.10": svc0}, e.mappings(), "kept: the verified configuration still sends a connector there")

	e.inv.set(snapshot(guest(102, "x", "x.example.com -> :8080")))
	e.res.goVia("x.example.com", "vmbr1")
	e.res.moveTo("x.example.com", legAddr)
	e.res.answerFrom("x.example.com", dbMAC)
	synced := e.gw.syncCount()

	e.next()
	st = e.next()

	require.Contains(t, e.rules(), hostRuleAt(legHost, "http://198.18.1.0:8080"), "the write failed: the old rule is still there")
	require.NotEqual(t, "http://198.18.1.0:8080", route(st, "x.example.com").Service)
	require.NotEqual(t, svc0, e.mappings()["vmbr1 10.50.0.10"])
	e.requireNeverForwarded(synced, svc0, legAddr)
	got, _ := e.egr.last()
	for _, tg := range got {
		require.NotEqual(t, svc0, tg.Addr)
	}
	require.Equal(t, planner.StateActive, route(st, "x.example.com").State)
	require.Equal(t, "http://198.18.1.1:8080", route(st, "x.example.com").Service, "a service address of its own")
	require.Equal(t, map[string]netip.Addr{"vmbr1 10.50.0.10": svc1}, e.mappings())
	require.Equal(t, []gateway.Service{{Leg: "vmbr1", Service: svc1, Addr: legAddr, Ports: []uint16{8080}}}, e.gw.last(t).services)
}

// web-1 is deleted and a new guest gets its VMID and its address, while the
// old rule stays at Cloudflare: the same owner and guest by name, but not the
// same guest, so it gets a service address of its own.
func TestAServiceAddressIsNeverHandedToANewGuestWithTheSameVMID(t *testing.T) {
	e := newLegEnv(t)
	e.warm()
	e.next()
	e.cf.FailNext("tunnel.write", 1000, errors.New("the account is frozen"))
	reborn := guest(101, "web-1", "www.example.com -> :8080")
	reborn.Identity = "uuid:another"
	e.inv.set(snapshot(reborn))
	synced := e.gw.syncCount()

	st := e.next()

	require.Equal(t, "http://198.18.1.1:8080", route(st, legHost).Service)
	require.Equal(t, map[string]netip.Addr{"vmbr1 10.50.0.10": svc1}, e.mappings())
	e.requireNeverForwarded(synced, svc0, legAddr)
	require.Contains(t, unnumbered(e.eng.Events(time.Time{})), Event{At: st.At, Level: levelInfo, Kind: kindNetwork, Subject: "vmbr1", Route: legHost,
		Message: "service address 198.18.1.0 of 10.50.0.10 on network vmbr1 released: it was made for qemu/101, and qemu/101 is at the address now"})
	all, err := e.store.ServiceAddrs()
	require.NoError(t, err)
	require.Equal(t, store.ServiceOwner{Owner: "qemu/101", Guest: "qemu/101", Identity: "uuid:another"}, all[0].ServiceOwner)
}

// x.example.com of another guest is proven at the address of web-1 while
// web-1 is served there: it is never given web-1's service address. When
// web-1 moves, the service address follows it, and x gets one of its own;
// when x moves, web-1 keeps it, and x gets one at its new address.
func TestARouteOfAnotherGuestNeverSharesAServiceAddress(t *testing.T) {
	for _, tc := range []struct {
		name, mover   string
		svc0At, newAt netip.Addr
	}{
		{"its owner moves", legHost, netip.MustParseAddr("10.50.0.11"), legAddr},
		{"the other one moves", "x.example.com", legAddr, netip.MustParseAddr("10.50.0.11")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLegEnv(t)
			e.warm()
			e.next()
			e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), guest(102, "x", "x.example.com -> :8080")))
			e.res.goVia("x.example.com", "vmbr1")
			e.res.moveTo("x.example.com", legAddr)
			st := e.next()
			require.Equal(t, "service address not allocated: service address 198.18.1.0 of this address was made for qemu/101, which is proven there too",
				route(st, "x.example.com").Reason)
			require.Equal(t, map[string]netip.Addr{"vmbr1 10.50.0.10": svc0}, e.mappings())
			synced := e.gw.syncCount()

			e.res.moveTo(tc.mover, netip.MustParseAddr("10.50.0.11"))
			st = e.next()

			require.Equal(t, "http://198.18.1.0:8080", route(st, legHost).Service, "web-1 keeps its service address")
			require.Equal(t, "http://198.18.1.1:8080", route(st, "x.example.com").Service, "x gets one of its own")
			require.Equal(t, map[string]netip.Addr{"vmbr1 " + tc.svc0At.String(): svc0, "vmbr1 " + tc.newAt.String(): svc1}, e.mappings())
			e.requireNeverForwarded(synced, svc0, tc.newAt)
		})
	}
}

// A manual route to web-1 shares its service address while web-1's own route
// is served: both reach the one guest. Once web-1's route is gone, the
// manual route gets one of its own, and web-1's forwards nothing.
func TestAManualRouteToTheSameGuestSharesItsServiceAddress(t *testing.T) {
	e := newLegEnv(t)
	e.warm()
	e.next()
	require.NoError(t, e.store.SaveManualRoute(model.Route{
		Hostname: "wiki.example.com", ManualID: "wiki", Source: model.SourceManual,
		Guest:  &model.GuestRef{Kind: model.KindQEMU, VMID: 101},
		Target: model.Target{Scheme: model.SchemeHTTP, Port: 8080},
	}))
	e.res.goVia("wiki.example.com", "vmbr1")
	e.res.moveTo("wiki.example.com", legAddr)

	st := e.next()
	e.next()

	require.Equal(t, "http://198.18.1.0:8080", route(st, "wiki.example.com").Service)
	require.Equal(t, map[string]netip.Addr{"vmbr1 10.50.0.10": svc0}, e.mappings())
	for _, ev := range e.eng.Events(time.Time{}) {
		require.NotContains(t, ev.Message, "released")
	}

	e.inv.set(snapshot(guest(101, "web-1")))
	synced := e.gw.syncCount()
	st = e.next()

	require.Equal(t, "http://198.18.1.1:8080", route(st, "wiki.example.com").Service)
	require.Equal(t, map[string]netip.Addr{"vmbr1 10.50.0.10": svc1}, e.mappings())
	e.requireNeverForwarded(synced, svc0, legAddr)
}

// A mapping stored before owners were kept belongs to the routes whose
// stored bindings were published under it: web-1 keeps it without a write,
// and the next guest at its address does not get it.
func TestAServiceAddressStoredWithoutAnOwner(t *testing.T) {
	e := newLegEnv(t)
	e.warm()
	e.next()
	require.NoError(t, e.store.ReleaseServiceAddr("vmbr1", legAddr))
	require.NoError(t, e.store.SaveServiceAddr(store.ServiceAddr{Network: "vmbr1", Addr: legAddr, Service: svc0, AllocatedAt: t0}))
	writes := len(e.writes())

	st := e.next()
	e.next()

	require.Equal(t, "http://198.18.1.0:8080", route(st, legHost).Service)
	require.Equal(t, map[string]netip.Addr{"vmbr1 10.50.0.10": svc0}, e.mappings())
	require.Len(t, e.writes(), writes, "nothing changes for its own route")
	for _, ev := range e.eng.Events(time.Time{}) {
		require.NotContains(t, ev.Message, "released")
	}

	e.cf.FailNext("tunnel.write", 1000, errors.New("the account is frozen"))
	e.inv.set(snapshot(guest(102, "x", "x.example.com -> :8080")))
	e.res.goVia("x.example.com", "vmbr1")
	e.res.moveTo("x.example.com", legAddr)
	e.res.answerFrom("x.example.com", dbMAC)
	synced := e.gw.syncCount()

	e.next()
	st = e.next()

	require.Equal(t, "http://198.18.1.1:8080", route(st, "x.example.com").Service)
	require.Equal(t, map[string]netip.Addr{"vmbr1 10.50.0.10": svc1}, e.mappings())
	e.requireNeverForwarded(synced, svc0, legAddr)
	require.Contains(t, unnumbered(e.eng.Events(time.Time{})), Event{At: st.At.Add(-20 * time.Second), Level: levelInfo, Kind: kindNetwork,
		Subject: "vmbr1", Route: "x.example.com",
		Message: "service address 198.18.1.0 of 10.50.0.10 on network vmbr1 released: it was made for a route of an earlier version, and qemu/102 is at the address now"})

	t.Run("that no stored binding was published under", func(t *testing.T) {
		e := newLegEnv(t)
		require.NoError(t, e.store.SaveServiceAddr(store.ServiceAddr{Network: "vmbr1", Addr: legAddr, Service: svc0, AllocatedAt: t0}))
		require.NoError(t, e.store.SaveServicePool(svc1))
		e.warm()

		st := e.next()

		require.Equal(t, "http://198.18.1.1:8080", route(st, legHost).Service, "it is made anew")
		require.Equal(t, map[string]netip.Addr{"vmbr1 10.50.0.10": svc1}, e.mappings())
	})
}
