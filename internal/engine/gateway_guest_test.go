package engine

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

const otherHost = "other.example.com"

// A route withdrawn from its guest keeps its service address for the
// guest's return, and that address forwards nothing meanwhile. Another
// guest proven at the same address never takes it over: the withdrawn
// route's rule may still send a connector there, and its visitors would
// reach the other guest. The other guest gets an address of its own, and
// the kept one is released, so that the stale rule reaches nothing.
func TestAnotherGuestAtTheAddressOfAWithdrawnRouteGetsAServiceAddressOfItsOwn(t *testing.T) {
	e := newLegEnv(t)
	e.warm()
	st := e.next()
	require.Equal(t, "http://198.18.1.0:8080", route(st, legHost).Service)

	e.res.stop(legHost, "10.50.0.10 answered by bc:24:11:ff:ff:01, which is not this guest")
	st = e.next()
	require.Equal(t, planner.StateWithdrawn, route(st, legHost).State)
	require.Empty(t, e.gw.last(t).services, "nothing forwards while the route is withdrawn")

	// Another guest now holds the address, and is proven there for its own
	// route.
	e.res.goVia(otherHost, "vmbr1")
	e.res.moveTo(otherHost, legAddr)
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), guest(102, "web-2", otherHost+" -> :8080")))
	st = e.next()

	require.Equal(t, planner.StateActive, route(st, otherHost).State)
	require.Equal(t, "http://198.18.1.1:8080", route(st, otherHost).Service, "never the withdrawn route's address")
	for _, s := range e.gw.last(t).services {
		require.NotEqual(t, svc0, s.Service, "the withdrawn route's address forwards nothing")
	}
	require.Equal(t, []gateway.Service{{Leg: "vmbr1", Service: svc1, Addr: legAddr, Ports: []uint16{8080}}}, e.gw.last(t).services)
	require.Equal(t, map[string]netip.Addr{"vmbr1 10.50.0.10": svc1}, e.mappings())
	for _, target := range e.eng.egress {
		require.NotEqual(t, svc0, target.Addr, "the withdrawn route's address is out of the egress filter")
	}
	st = e.next()
	require.Equal(t, "http://198.18.1.1:8080", route(st, otherHost).Service, "and keeps it")
}
