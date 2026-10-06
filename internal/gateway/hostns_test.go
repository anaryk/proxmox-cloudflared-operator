package gateway

import (
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUpOnAFreshNodeMakesTheNamespaceTheTransitLinkAndBothTables(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.b.Up(t.Context()))
	c := h.sys.changes()
	require.Equal(t, "route add host unreachable 198.18.0.0/16 metric 1000", c[0],
		"the fallback route is there before anything it guards")
	require.Equal(t, "create namespace", c[1])
	for _, want := range []string{
		"sysctl ns net.ipv4.ip_forward=1",
		"sysctl ns net.ipv4.conf.all.arp_ignore=1",
		"sysctl ns net.ipv4.conf.all.arp_announce=2",
		"sysctl ns net.ipv6.conf.all.disable_ipv6=1",
		"sysctl ns net.ipv6.conf.default.disable_ipv6=1",
		"up ns lo",
		"veth pcotr0 auto transit0 auto mtu 0",
		"sysctl host net.ipv6.conf.pcotr0.disable_ipv6=1",
		"addr add host pcotr0 198.18.0.1/30",
		"addr add ns transit0 198.18.0.2/30",
		"up host pcotr0",
		"up ns transit0",
		"route add host 198.18.0.0/16 via 198.18.0.2 dev pcotr0 src 198.18.0.1",
	} {
		require.Contains(t, c, want)
	}
	require.Less(t, index(t, c, "sysctl ns net.ipv4.ip_forward"), index(t, c, "veth pcotr0"))
	require.Less(t, index(t, c, "up ns transit0"), index(t, c, "route add host 198.18.0.0/16 via"))
	// Both tables are there before the transit link, so that nothing passes
	// it unfiltered, also after a flush of the ruleset.
	o := h.sys.order
	require.Less(t, index(t, o, "nft pco_host"), index(t, o, "veth pcotr0"))
	require.Less(t, index(t, o, "nft pco_gw"), index(t, o, "veth pcotr0"))
	require.Less(t, index(t, o, "route add host unreachable"), index(t, o, "nft pco_host"))
	require.Len(t, h.nsNft.scripts, 1)
	require.Equal(t, renderGateway(snatMap, elementsOf(nil, nil), nil), h.nsNft.scripts[0], "the table with empty maps")
	require.Equal(t, []string{renderHost()}, h.hostNft.scripts)
	require.NotContains(t, strings.Join(c, "\n"), ManagedBridge, "no managed bridge, no sysctl of it")
}

// A restart of pco-netns.service under traffic runs Up once more: it reads
// everything and changes nothing.
func TestASecondUpChangesNothing(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sync(t, testLegs, testServices, nil, nil)
	h.reset()
	require.NoError(t, h.b.Up(t.Context()))
	require.Empty(t, h.sys.changes())
	require.Empty(t, h.nsNft.scripts)
	require.Empty(t, h.hostNft.scripts)
}

func TestUpSetsIPv6OffOnTheManagedBridge(t *testing.T) {
	h := newHarness(t).node()
	require.NoError(t, h.b.Up(t.Context()))
	require.Contains(t, h.sys.changes(), "sysctl host net.ipv6.conf.pconet.disable_ipv6=1")
	h.reset()
	h.sys.host.sysctls["net.ipv6.conf.pconet.disable_ipv6"] = "0"
	require.NoError(t, h.b.Up(t.Context()))
	require.Equal(t, []string{"sysctl host net.ipv6.conf.pconet.disable_ipv6=1"}, h.sys.changes(),
		"an ifreload gave the bridge IPv6 back")
}

// Where the namespace is missing and this process cannot pin one (the
// daemon), Up says so and changes nothing but the fallback route.
func TestUpWithoutAPinWhereNoneCanBeMadeSaysWhatToDo(t *testing.T) {
	h := newHarness(t)
	h.sys.canPin = false
	err := h.b.Up(t.Context())
	require.ErrorIs(t, err, ErrNotPinned)
	require.Equal(t, "the gateway namespace is not pinned; systemctl restart pco-netns", err.Error())
	require.Equal(t, []string{"route add host unreachable 198.18.0.0/16 metric 1000"}, h.sys.changes())
}

// Up after a restore loads the table with the cached elements in the one
// transaction that loads it, so that the maps are never empty in between.
func TestUpLoadsTheTableWithTheCachedElements(t *testing.T) {
	h := newHarness(t).node()
	h.b.seed(testLegs, testServices, nil)
	require.NoError(t, h.b.Up(t.Context()))
	require.Len(t, h.nsNft.scripts, 1)
	require.Equal(t, renderGateway(snatMap, elementsOf(testLegs, testServices), testLegs), h.nsNft.scripts[0])
}

// A table whose base differs from what this build loads, as after an
// upgrade, is loaded again whole, with the elements of what was applied.
func TestUpLoadsATableThatDiffersAgainWithItsElements(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sync(t, testLegs, testServices, nil, nil)
	h.reset()
	h.nsNft.dropRule(gwTable, "post_filter", 0)
	require.NoError(t, h.b.Up(t.Context()))
	require.Equal(t, []string{renderGateway(snatMap, elementsOf(testLegs, testServices), testLegs)}, h.nsNft.scripts)
	require.Empty(t, h.sys.changes())
}

// A leg is made in this order: the pair with both MACs and the bridge's
// MTU in one call, IPv6 off on both ends, the bridge end enslaved and in its
// VLAN alone before either end is up, the sticky entry of the leg's MAC, the
// address, then the leg's table and its rule.
func TestALegIsMadeInOrder(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	states := h.sync(t, []Leg{legB}, nil, nil, nil)
	require.Equal(t, []LegState{{ID: "vmbr2.20", Up: true, Iface: "leg2", MTU: 9000}}, states)
	c := h.sys.changes()
	order := []string{
		"veth vethpco2 fe:50:43:4f:00:02 leg2 02:50:43:4f:00:02 mtu 9000",
		"sysctl host net.ipv6.conf.vethpco2.disable_ipv6=1",
		"master vethpco2 vmbr2",
		"vlan add vethpco2 20",
		"vlan del vethpco2 1",
		"up host vethpco2",
		"up ns leg2",
		"sticky vethpco2 02:50:43:4f:00:02 vlan 20",
		"addr add ns leg2 10.50.0.250/24 noprefixroute",
		"route add ns unreachable default table 102",
		"route add ns 10.50.0.0/24 dev leg2 src 10.50.0.250 table 102",
		"rule add ns 1002 fwmark 2 table 102",
	}
	last := -1
	for _, want := range order {
		i := index(t, c, want)
		require.Greater(t, i, last, "%s comes after %s", want, order[max(0, slices.Index(order, want)-1)])
		last = i
	}
	require.Equal(t, []portVLAN{{vid: 20, pvid: true, untagged: true}}, h.sys.host.links["vethpco2"].vlans)
	require.Equal(t, `"leg2" : 10.50.0.250 . 2048-3071`, h.nsNft.tables[gwTable].elems[mapLegSNAT][0])
}

func TestATagOnABridgeThatFiltersNoVLANsTakesItsVLANBridgeOnceThere(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	l := legB
	l.Bridge = "vmbr0"
	h.sys.bridge("vmbr0", false, 1500, "eno0")
	states := h.sync(t, []Leg{l}, nil, nil, nil)
	require.Equal(t, []LegState{{ID: "vmbr2.20", Iface: "leg2", Problem: "bridge vmbr0v20 is absent"}}, states)
	require.Empty(t, slices.DeleteFunc(h.sys.changes(), func(c string) bool { return strings.HasPrefix(c, "route del") }))
	require.Contains(t, h.nsNft.tables[gwTable].elems[setLegs], `"leg2"`, "its services are programmed all the same")

	h.sys.bridge("vmbr0v20", false, 1500, "eno0.20")
	states = h.sync(t, []Leg{l}, nil, nil, nil)
	require.True(t, states[0].Up, states[0].Problem)
	require.Equal(t, "vmbr0v20", h.sys.host.links["vethpco2"].master)
	require.Empty(t, h.sys.host.links["vethpco2"].vlans)
	require.Contains(t, h.sys.changes(), "sticky vethpco2 02:50:43:4f:00:02 vlan 0")
}

func TestDuplicateAddressDetectionOnAnUplinkBridge(t *testing.T) {
	uplink := legA
	uplink.Uplink = true

	t.Run("an answer keeps the address off", func(t *testing.T) {
		h := newHarness(t).node()
		h.up(t)
		h.sys.answers[addr("10.50.0.250")] = []net.HardwareAddr{mac("52:54:00:12:34:56")}
		states := h.sync(t, []Leg{uplink}, nil, nil, nil)
		require.Equal(t, []LegState{{ID: "vmbr1", Iface: "leg1", MTU: 1500, Problem: "address in use by 52:54:00:12:34:56"}}, states)
		require.Contains(t, h.sys.calls, "probe leg1 10.50.0.250 300ms")
		require.NotContains(t, strings.Join(h.sys.changes(), "\n"), "addr add ns leg1")
		require.Equal(t, []route{{table: 101, unreachable: true}}, slices.DeleteFunc(h.sys.ns.routes, func(r route) bool { return r.table != 101 }))
	})
	t.Run("no answer raises it", func(t *testing.T) {
		h := newHarness(t).node()
		h.up(t)
		states := h.sync(t, []Leg{uplink}, nil, nil, nil)
		require.Equal(t, []LegState{{ID: "vmbr1", Up: true, Iface: "leg1", MTU: 1500, DADAt: h.now}}, states)
		c := h.sys.calls
		require.Less(t, index(t, c, "probe leg1 10.50.0.250"), index(t, c, "addr add ns leg1"))
		h.reset()
		h.sync(t, []Leg{uplink}, nil, nil, nil)
		require.Empty(t, h.sys.calls, "once raised it is not probed again")
	})
	t.Run("no carrier raises it unprobed, and the next Sync probes", func(t *testing.T) {
		h := newHarness(t).node()
		h.sys.host.links["eno1"].carrier = false
		h.up(t)
		h.sys.answers[addr("10.50.0.250")] = []net.HardwareAddr{mac("52:54:00:12:34:56")}
		states := h.sync(t, []Leg{uplink}, nil, nil, nil)
		require.Equal(t, []LegState{{ID: "vmbr1", Up: true, Iface: "leg1", MTU: 1500, Problem: "no carrier, DAD not run"}}, states)
		require.NotContains(t, strings.Join(h.sys.calls, "\n"), "probe")
		h.reset()
		h.sys.host.links["eno1"].carrier = true
		states = h.sync(t, []Leg{uplink}, nil, nil, nil)
		require.Equal(t, "address in use by 52:54:00:12:34:56", states[0].Problem)
		require.False(t, states[0].Up)
		require.Contains(t, h.sys.changes(), "addr del ns leg1 10.50.0.250/24")
	})
	t.Run("a leg on a bridge without an uplink is not probed", func(t *testing.T) {
		h := newHarness(t).node()
		h.up(t)
		h.sys.answers[addr("10.50.0.250")] = []net.HardwareAddr{mac("52:54:00:12:34:56")}
		states := h.sync(t, []Leg{legA}, nil, nil, nil)
		require.True(t, states[0].Up)
		require.NotContains(t, strings.Join(h.sys.calls, "\n"), "probe")
	})
}

func TestTheManagedLegIsNotIsolatedOnItsIsolatingBridge(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sync(t, []Leg{legM}, nil, nil, nil)
	h.sys.host.links["vethpco3"].isolated = true
	h.reset()
	h.sync(t, []Leg{legM}, nil, nil, nil)
	require.Equal(t, []string{"isolated vethpco3 false"}, h.sys.changes())
	require.Contains(t, h.nsNft.tables[gwTable].elems[setDHCPLegs], `"leg3"`)
	require.Equal(t, mac("fe:50:43:4f:ff:01"), h.sys.host.links["vethpco3"].mac)
}

func TestServicesAreAddedAndRemovedAsElements(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sync(t, testLegs, []Service{svcA}, nil, nil)
	h.reset()
	h.sync(t, testLegs, []Service{svcB, svcM}, nil, nil)
	require.Len(t, h.nsNft.scripts, 1, "one transaction")
	script := h.nsNft.scripts[0]
	require.NotContains(t, script, "table inet pco_gw {")
	require.Less(t, strings.Index(script, "add element inet pco_gw svc_dnat { 198.18.1.1 . 80"),
		strings.Index(script, "delete element inet pco_gw svc_dnat { 198.18.1.0 . 443"))
	require.Equal(t, sortedElements(elementsOf(testLegs, []Service{svcB, svcM})), sortedElements(h.nsNft.tables[gwTable].elems))
	h.reset()
	h.sync(t, testLegs, []Service{svcB, svcM}, nil, nil)
	require.Empty(t, h.nsNft.scripts, "nothing changed")
	require.Empty(t, h.sys.changes())
}

func TestPinsAreReplacedAndThoseNotGivenRemoved(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	pins := []Pin{
		{Leg: "vmbr1", Addr: addr("10.50.0.10"), MAC: mac("bc:24:11:00:00:01")},
		{Leg: "vmbr2.20", Addr: addr("10.50.0.10"), MAC: mac("bc:24:11:00:00:02")},
	}
	h.sync(t, []Leg{legA, legB}, nil, pins, nil)
	require.Contains(t, h.sys.changes(), "neigh leg1 10.50.0.10 bc:24:11:00:00:01")
	require.Contains(t, h.sys.changes(), "neigh leg2 10.50.0.10 bc:24:11:00:00:02")
	h.reset()
	moved := []Pin{{Leg: "vmbr1", Addr: addr("10.50.0.10"), MAC: mac("bc:24:11:00:00:03")}}
	h.sync(t, []Leg{legA, legB}, nil, moved, nil)
	require.ElementsMatch(t, []string{"neigh leg1 10.50.0.10 bc:24:11:00:00:03", "neigh del leg2 10.50.0.10"}, h.sys.changes())
}

func TestStickyEntriesOfManagedGuests(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	guest := mac("06:50:43:4f:76:05")
	sticky := []Sticky{{MAC: guest, Bridge: ManagedBridge, Port: "tap101i1"}}

	h.sync(t, []Leg{legM}, nil, nil, sticky)
	require.NotContains(t, strings.Join(h.sys.changes(), "\n"), "tap101i1", "the port is absent: the entry waits")

	h.sys.host.links["tap101i1"] = &fakeLink{link: link{name: "tap101i1", kind: "tun", up: true, carrier: true, master: ManagedBridge}}
	h.reset()
	h.sync(t, []Leg{legM}, nil, nil, sticky)
	require.Equal(t, []string{"sticky tap101i1 06:50:43:4f:76:05 vlan 0"}, h.sys.changes())
	require.NoError(t, h.b.Verify(t.Context()))

	h.reset()
	h.sync(t, []Leg{legM}, nil, nil, []Sticky{{MAC: guest, Bridge: ManagedBridge}})
	require.Equal(t, []string{"fdb del tap101i1 06:50:43:4f:76:05 vlan 0"}, h.sys.changes(), "a stopped guest's entry goes")
}

// A Sync reads the forwarding entries of the ports whose entries it keeps or
// takes away, not those of every port of the managed bridge: it runs at every
// cycle and on link events.
func TestStickyEntriesAreReadOnlyOnThePortsTheyConcern(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	for _, p := range []string{"tap101i1", "tap102i1", "tap103i1"} {
		h.sys.host.links[p] = &fakeLink{link: link{name: p, kind: "tun", up: true, carrier: true, master: ManagedBridge}}
	}
	guest := mac("06:50:43:4f:76:05")
	h.sync(t, []Leg{legM}, nil, nil, []Sticky{{MAC: guest, Bridge: ManagedBridge, Port: "tap101i1"}})

	h.sys.fdbReads = map[string]int{}
	h.sync(t, []Leg{legM}, nil, nil, []Sticky{{MAC: guest, Bridge: ManagedBridge, Port: "tap101i1"}})
	require.Equal(t, map[string]int{"tap101i1": 1, "vethpco3": 1}, h.sys.fdbReads)

	h.reset()
	h.sys.fdbReads = map[string]int{}
	h.sync(t, []Leg{legM}, nil, nil, []Sticky{{MAC: guest, Bridge: ManagedBridge, Port: "tap102i1"}})
	require.Equal(t, map[string]int{"tap101i1": 1, "tap102i1": 1, "vethpco3": 1}, h.sys.fdbReads, "the guest moved to another port")
	require.Equal(t, []string{"sticky tap102i1 06:50:43:4f:76:05 vlan 0", "fdb del tap101i1 06:50:43:4f:76:05 vlan 0"}, h.sys.changes())
}

func TestLegsThatAreNotGivenAreRemoved(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sync(t, []Leg{legA, legB}, []Service{svcA, svcB}, nil, nil)
	h.reset()
	h.sync(t, []Leg{legB}, []Service{svcB}, nil, nil)
	c := h.sys.changes()
	require.Contains(t, c, "del host vethpco1")
	require.Contains(t, c, "route del ns unreachable default table 101")
	require.Contains(t, c, "rule del ns 1001")
	require.NotContains(t, h.sys.host.links, "vethpco1")
	require.NotContains(t, h.sys.ns.links, "leg1")
	require.NotContains(t, h.nsNft.tables[gwTable].elems[setLegs], `"leg1"`)
}

func TestALegWhoseBridgeEndLostItsPeerIsMadeAgain(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sync(t, []Leg{legA}, nil, nil, nil)
	delete(h.sys.ns.links, "leg1")
	h.reset()
	states := h.sync(t, []Leg{legA}, nil, nil, nil)
	require.True(t, states[0].Up)
	c := h.sys.changes()
	require.Less(t, index(t, c, "del host vethpco1"), index(t, c, "veth vethpco1"))
}

// The backend does not take the engine's word for a service: one that would
// put an address outside the prefix into the table, or DNAT to an address
// outside its leg's network, is left out and reported on its leg.
func TestABadServiceIsReportedOnItsLegAndLeftOut(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	bad := []Service{
		{Leg: "vmbr1", Service: addr("198.19.0.1"), Addr: addr("10.50.0.11"), Ports: []uint16{80}},
		{Leg: "vmbr1", Service: addr("198.18.0.2"), Addr: addr("10.50.0.12"), Ports: []uint16{80}},
		{Leg: "vmbr1", Service: addr("198.18.1.7"), Addr: addr("10.60.0.5"), Ports: []uint16{80}},
		{Leg: "vmbr1", Service: addr("198.18.1.8"), Addr: addr("10.50.0.250"), Ports: []uint16{80}},
		{Leg: "vmbr1", Service: svcA.Service, Addr: addr("10.50.0.13"), Ports: []uint16{22}},
	}
	states := h.sync(t, []Leg{legA, legB}, append([]Service{svcA, svcB}, bad...), nil, nil)
	require.Equal(t, "service address 198.19.0.1 is outside 198.18.0.0/16; "+
		"service address 198.18.0.2 is one of the transit link; "+
		"10.60.0.5 of service address 198.18.1.7 is outside 10.50.0.0/24, the network of the leg; "+
		"10.50.0.250 of service address 198.18.1.8 is the address of the leg; "+
		"service address 198.18.1.0 is given for 10.50.0.10 and for 10.50.0.13", states[0].Problem)
	require.True(t, states[0].Up)
	require.Empty(t, states[1].Problem)
	require.Equal(t, sortedElements(elementsOf([]Leg{legA, legB}, []Service{svcA, svcB})), sortedElements(h.nsNft.tables[gwTable].elems))

	inside := legA
	inside.Prefix = prefix("198.18.5.1/24")
	states = h.sync(t, []Leg{inside}, nil, nil, nil)
	require.Equal(t, "198.18.5.1/24 overlaps the service prefix 198.18.0.0/16", states[0].Problem)
}

func TestABadLegIsReportedAndLeftOut(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	bad := legA
	bad.Mark = 63
	twin := legB
	twin.Mark = 1
	states := h.sync(t, []Leg{bad, legA, twin}, nil, nil, nil)
	require.Equal(t, "mark 63 is outside 1-62", states[0].Problem)
	require.True(t, states[1].Up)
	require.Equal(t, "mark 1 is that of network vmbr1 too", states[2].Problem)
}

func TestVerifyFindsWhatChanged(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(h *harness)
		want   string
	}{
		{"a leg removed", func(h *harness) { delete(h.sys.host.links, "vethpco1"); delete(h.sys.ns.links, "leg1") }, "vethpco1 is missing in the host"},
		{"a leg's address", func(h *harness) { h.sys.ns.addrs["leg1"] = nil }, "leg1 has not the address 10.50.0.250/24 in the namespace pco"},
		{"a leg's rule", func(h *harness) { h.sys.ns.rules = nil }, "the rule 1001: fwmark 1 lookup 101 is missing"},
		{"a leg's VLAN", func(h *harness) {
			h.sys.host.links["vethpco2"].vlans = []portVLAN{{vid: 1, pvid: true, untagged: true}}
		},
			"vethpco2 is in the VLANs [1], want only 20, untagged"},
		{"the sticky entry of a leg", func(h *harness) { h.sys.host.links["vethpco1"].fdb = nil }, "the sticky entry of 0250434f0001 on vethpco1 is missing"},
		{"a sysctl of the namespace", func(h *harness) { h.sys.ns.sysctls["net.ipv4.conf.all.arp_ignore"] = "0" },
			"net.ipv4.conf.all.arp_ignore is 0 in the namespace pco, want 1"},
		{"the fallback route", func(h *harness) {
			h.sys.host.routes = slices.DeleteFunc(h.sys.host.routes, func(r route) bool { return r.unreachable })
		}, "the route unreachable 198.18.0.0/16 metric 1000 is missing in the host"},
		{"the host table", func(h *harness) { delete(h.hostNft.tables, hostTable) }, "table inet pco_host is missing"},
		{"a rule of the host table", func(h *harness) { h.hostNft.dropRule(hostTable, "forward", 0) },
			"chain forward of table inet pco_host has 1 rules, want 2"},
		{"an element", func(h *harness) {
			e := h.nsNft.tables[gwTable].elems
			e[mapSvcDNAT] = slices.DeleteFunc(e[mapSvcDNAT], func(el string) bool { return el == "198.18.1.0 . 80 : 10.50.0.10 . 80" })
		}, "svc_dnat of pco_gw lacks 198.18.1.0 . 80 : 10.50.0.10 . 80"},
		{"IPv6 on the managed bridge", func(h *harness) { h.sys.host.sysctls["net.ipv6.conf.pconet.disable_ipv6"] = "0" },
			"net.ipv6.conf.pconet.disable_ipv6 is 0 in the host, want 1"},
		{"the namespace", func(h *harness) { h.sys.ns = nil }, "the namespace pco is not pinned"},
		{"a route more in a leg's table", func(h *harness) {
			h.sys.ns.routes = append(h.sys.ns.routes, route{table: 101, dst: prefix("10.99.0.0/24"), dev: "leg1", src: addr("10.50.0.250")})
		}, "the route 10.99.0.0/24 dev leg1 src 10.50.0.250 table 101 is not pco's"},
		{"a second rule on a leg's mark", func(h *harness) {
			h.sys.ns.rules = append(h.sys.ns.rules, rule{pref: 900, mark: 1, table: mainTable})
		}, "the rule 900: fwmark 1 lookup 254 is not pco's"},
		{"another rule at a leg's preference", func(h *harness) {
			h.sys.ns.rules = append(h.sys.ns.rules, rule{pref: 1002, mark: 7, table: 107})
		}, "the rule 1002: fwmark 7 lookup 107 is not pco's"},
		{"a sysctl of a leg", func(h *harness) { h.sys.ns.sysctls["net.ipv4.conf.leg1.rp_filter"] = "1" },
			"net.ipv4.conf.leg1.rp_filter is 1 in the namespace pco, want 0"},
		{"IPv6 on the bridge end of a leg", func(h *harness) { h.sys.host.sysctls["net.ipv6.conf.vethpco2.disable_ipv6"] = "0" },
			"net.ipv6.conf.vethpco2.disable_ipv6 is 0 in the host, want 1"},
		{"IPv6 on the transit link", func(h *harness) { h.sys.ns.sysctls["net.ipv6.conf.transit0.disable_ipv6"] = "0" },
			"net.ipv6.conf.transit0.disable_ipv6 is 0 in the namespace pco, want 1"},
		{"a leg of no network", func(h *harness) {
			h.sys.host.links["vethpco9"] = &fakeLink{link: link{name: "vethpco9", kind: "veth", up: true}}
		}, "vethpco9 is the leg of no network"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t).node()
			h.up(t)
			h.sync(t, []Leg{legA, legB}, []Service{svcA}, nil, nil)
			require.NoError(t, h.b.Verify(t.Context()))
			tt.change(h)
			err := h.b.Verify(t.Context())
			require.ErrorIs(t, err, ErrChanged)
			var ce *ChangedError
			require.True(t, errors.As(err, &ce))
			require.Contains(t, ce.Differences, tt.want)
		})
	}
}

func TestDetachRemovesALegAndItsServices(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sync(t, []Leg{legA, legB}, []Service{svcA, svcB}, nil, nil)
	h.reset()
	require.NoError(t, h.b.Detach(t.Context(), "vmbr1"))
	require.Contains(t, h.sys.changes(), "del host vethpco1")
	require.Equal(t, sortedElements(elementsOf([]Leg{legB}, []Service{svcB})), sortedElements(h.nsNft.tables[gwTable].elems))
	require.NoError(t, h.b.Verify(t.Context()))
	h.reset()
	require.NoError(t, h.b.Detach(t.Context(), "vmbr1"))
	require.Empty(t, h.sys.changes())
}

func TestDownRemovesEverythingUpAndSyncMade(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sys.host.links["tap101i1"] = &fakeLink{link: link{name: "tap101i1", kind: "tun", up: true, master: ManagedBridge}}
	h.sync(t, testLegs, testServices, nil, []Sticky{{MAC: mac("06:50:43:4f:76:05"), Bridge: ManagedBridge, Port: "tap101i1"}})
	// An entry no Sync of this process made, as one from before a restart.
	h.sys.host.links["tap102i1"] = &fakeLink{link: link{name: "tap102i1", kind: "tun", up: true, master: ManagedBridge},
		fdb: []fdbEntry{{mac: mac("06:50:43:4f:76:06"), static: true, sticky: true}}}
	require.NoError(t, h.b.Down(t.Context()))
	require.Empty(t, h.sys.host.links["tap102i1"].fdb)
	for name := range h.sys.host.links {
		require.False(t, strings.HasPrefix(name, "vethpco") || name == "pcotr0", name)
	}
	require.Empty(t, slices.DeleteFunc(slices.Clone(h.sys.host.routes), func(r route) bool { return !ServicePrefix.Overlaps(r.dst) }))
	require.Nil(t, h.sys.ns)
	require.Empty(t, h.sys.host.links["tap101i1"].fdb)
	require.NotContains(t, h.hostNft.tables, hostTable)
}

// An empty file where the pin would be, left by a pin that failed, is no
// namespace, and Down takes it away all the same.
func TestDownRemovesWhatAFailedPinLeft(t *testing.T) {
	h := newHarness(t).node()
	h.sys.leftoverPin = true
	require.NoError(t, h.b.Down(t.Context()))
	require.Contains(t, h.sys.changes(), "remove pin")
	require.False(t, h.sys.leftoverPin)
}

func TestProbeThroughATemporaryPortRemovesIt(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sys.answers[addr("10.50.0.7")] = []net.HardwareAddr{mac("52:54:00:00:00:07"), mac("52:54:00:00:00:07")}
	macs, err := h.b.Probe(t.Context(), "vmbr2", 30, addr("10.50.0.7"))
	require.NoError(t, err)
	require.Equal(t, []net.HardwareAddr{mac("52:54:00:00:00:07")}, macs)
	c := h.sys.calls
	require.Less(t, index(t, c, "veth vethpcop fe:50:43:4f:00:fe probe0 02:50:43:4f:00:fe mtu 9000"), index(t, c, "vlan add vethpcop 30"))
	require.Less(t, index(t, c, "vlan del vethpcop 1"), index(t, c, "probe probe0 10.50.0.7"))
	require.Equal(t, "del host vethpcop", c[len(c)-1])
	require.NotContains(t, h.sys.host.links, "vethpcop")

	_, err = h.b.Probe(t.Context(), "vmbr9", 0, addr("10.50.0.7"))
	require.EqualError(t, err, "bridge vmbr9 is absent")
}

func TestProbeThroughALegUsesIt(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sync(t, []Leg{legA}, nil, nil, nil)
	h.reset()
	h.sys.answers[addr("10.50.0.7")] = []net.HardwareAddr{mac("52:54:00:00:00:07")}
	macs, err := h.b.Probe(t.Context(), "vmbr1", 0, addr("10.50.0.7"))
	require.NoError(t, err)
	require.Equal(t, []net.HardwareAddr{mac("52:54:00:00:00:07")}, macs)
	require.Equal(t, []string{"probe leg1 10.50.0.7 300ms"}, h.sys.calls)
}

func TestDialGoesThroughTheLegWithItsMark(t *testing.T) {
	h := newHarness(t).node()
	c, err := h.b.Dial(t.Context(), legB, netip.MustParseAddrPort("10.50.0.10:80"))
	require.NoError(t, err)
	_ = c.Close()
	require.Equal(t, []string{"dial 2 10.50.0.250 10.50.0.10:80"}, h.sys.calls)
}

func TestRestoreFromACache(t *testing.T) {
	pins := []Pin{{Leg: "vmbr1", Addr: addr("10.50.0.10"), MAC: mac("bc:24:11:00:00:01")}}
	t.Run("fresh pins are set", func(t *testing.T) {
		h := newHarness(t).node()
		c := Cache{Legs: testLegs, Services: testServices, Pins: pins, PinsAt: h.now.Add(-time.Minute)}
		states, err := Restore(t.Context(), h.b, c, h.now)
		require.NoError(t, err)
		require.Len(t, states, 3)
		require.Equal(t, renderGateway(snatMap, elementsOf(testLegs, testServices), testLegs), h.nsNft.scripts[0])
		require.Len(t, h.nsNft.scripts, 1, "the table was loaded with its elements, nothing to add after")
		require.Contains(t, h.sys.changes(), "neigh leg1 10.50.0.10 bc:24:11:00:00:01")
	})
	t.Run("stale pins leave those there", func(t *testing.T) {
		h := newHarness(t).node()
		h.up(t)
		h.sync(t, testLegs, testServices, pins, nil)
		h.reset()
		c := Cache{Legs: testLegs, Services: testServices, Pins: pins, PinsAt: h.now.Add(-PinsFresh)}
		_, err := Restore(t.Context(), h.b, c, h.now)
		require.NoError(t, err)
		require.Empty(t, h.sys.changes(), "a second pco netns up changes nothing")
		require.Empty(t, h.nsNft.scripts)
		require.Len(t, h.sys.ns.neigh["leg1"], 1)
	})
}

func TestTheCacheRoundTrips(t *testing.T) {
	dir := t.TempDir()
	_, ok, err := ReadCache(dir)
	require.NoError(t, err)
	require.False(t, ok)
	c := Cache{
		Legs: testLegs, Services: testServices,
		Pins:   []Pin{{Leg: "vmbr1", Addr: addr("10.50.0.10"), MAC: mac("bc:24:11:00:00:01")}},
		Sticky: []Sticky{{MAC: mac("06:50:43:4f:76:05"), Bridge: ManagedBridge, Port: "tap101i1"}},
		PinsAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(t, WriteCache(dir, c))
	got, ok, err := ReadCache(dir)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, c, got)
	require.True(t, got.Fresh(c.PinsAt.Add(4*time.Minute)))
	require.False(t, got.Fresh(c.PinsAt.Add(5*time.Minute)))
	require.False(t, got.Fresh(c.PinsAt.Add(-time.Minute)))
}
