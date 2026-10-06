package resolve

import (
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// leg1 is a leg of the gateway on vmbr1, where the node has no address of
// its own: the leg is in 10.50.0.0/24, and the bridge's table holds its
// frames.
var leg1 = HostIface{
	Name: "leg1", Addrs: prefixes("10.50.0.250/24"),
	Segment: Segment{Bridge: "vmbr1"}, Table: FDBRef{Bridge: "vmbr1"}, Via: "vmbr1",
}

// legScenario is web-1 with net0 on vmbr1 and the address 10.50.0.10, which
// the node reaches through leg1 only.
func legScenario(t *testing.T) *scenario {
	s := newScenario(t)
	s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr1", 0, "10.50.0.10")}
	s.prober.ifaces = append(s.prober.ifaces, leg1)
	s.prober.arp[arpKey("leg1", "10.50.0.10")] = []string{mac0}
	s.prober.fdb[fdbKey("vmbr1", 0, mac0)] = []string{"tap101i0"}
	return s
}

func TestAGuestBehindALegIsProvenAtPortThroughIt(t *testing.T) {
	s := legScenario(t)

	res := s.resolve(t, webRoute(), nil)

	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.50.0.10"), Reachable: true, Owner: webOwner}, res.Target)
	require.Equal(t, LevelPort, res.Level)
	require.Equal(t, "vmbr1", res.Binding.Via, "the binding names the network whose leg proved it")
	require.Equal(t, "vmbr1", res.Binding.Bridge)
	require.Equal(t, "tap101i0", res.Binding.Port)
	require.Equal(t, Segment{Bridge: "vmbr1"}, res.Binding.Segment)
	require.Equal(t, []probeCall{
		{op: "interfaces"},
		{op: "route", addr: ip("10.50.0.10")},
		{op: "arp", iface: "leg1", addr: ip("10.50.0.10")},
		{op: "fdb", iface: "vmbr1", mac: mac0},
		{op: "dial", addr: ip("10.50.0.10"), port: 80},
	}, s.prober.calls)
	require.Equal(t, []hint{
		{op: "route", iface: "leg1", addr: ip("10.50.0.10")},
		{op: "dial", iface: "leg1", addr: ip("10.50.0.10")},
	}, s.prober.hints, "the route and the dial are those of the leg")
}

func TestAnAddressTheNodeReachesDirectlyHasNoVia(t *testing.T) {
	s := newScenario(t)

	res := s.resolve(t, webRoute(), nil)

	requireServed(t, res, "10.20.0.10", t0)
	require.Empty(t, res.Binding.Via)
	require.Equal(t, []hint{
		{op: "route", iface: "vmbr0", addr: ip("10.20.0.10")},
		{op: "dial", iface: "vmbr0", addr: ip("10.20.0.10")},
	}, s.prober.hints, "the route and the dial are asked for the interface the identity was checked on")
}

// A leg's frames are in the forwarding table of the bridge its port is on:
// the VLAN of a bridge that filters VLANs, or the bridge Proxmox makes for
// the VLAN of one that does not.
func TestALegChecksTheForwardingTableItsFramesAreIn(t *testing.T) {
	tests := []struct {
		name  string
		table FDBRef
	}{
		{name: "the bridge Proxmox makes for the VLAN", table: FDBRef{Bridge: "vmbr0v20"}},
		{name: "the VLAN of a bridge that filters VLANs", table: FDBRef{Bridge: "vmbr0", VLAN: 20}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScenario(t)
			s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr0", 20, "10.60.0.10")}
			s.prober.ifaces = append(s.prober.ifaces, HostIface{
				Name: "leg2", Addrs: prefixes("10.60.0.250/24"),
				Segment: Segment{Bridge: "vmbr0", VLAN: 20}, Table: tt.table, Via: "vmbr0.20",
			})
			s.prober.arp[arpKey("leg2", "10.60.0.10")] = []string{mac0}
			s.prober.fdb[fdbKey(tt.table.Bridge, tt.table.VLAN, mac0)] = []string{"tap101i0"}

			res := s.resolve(t, webRoute(), nil)

			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.60.0.10"), Reachable: true, Owner: webOwner}, res.Target)
			require.Equal(t, LevelPort, res.Level)
			require.Equal(t, []probeCall{{op: "fdb", iface: tt.table.Bridge, vlan: tt.table.VLAN, mac: mac0}}, s.prober.ops("fdb"))
			require.Equal(t, tt.table.Bridge, res.Binding.Bridge)
			require.Equal(t, "vmbr0.20", res.Binding.Via)
		})
	}
}

// The leg's address is an address of the node: a guest that claims it is
// never served.
func TestAGuestClaimingTheLegAddressIsRejected(t *testing.T) {
	s := legScenario(t)
	s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr1", 0, "10.50.0.250")}

	res := s.resolve(t, webRoute(), nil)

	require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: reasonNodeAddress}, res.Target)
	require.Nil(t, res.Binding)
	require.False(t, s.prober.touched(ip("10.50.0.250")))
}

// The leg's table answers for the leg only: a route that leaves elsewhere,
// or through a gateway, is no proof.
func TestARouteThatDoesNotLeaveThroughTheLegIsNoProof(t *testing.T) {
	tests := []struct {
		name   string
		route  fakeRoute
		reason string
	}{
		{name: "another interface", route: fakeRoute{iface: "transit0", onLink: true}, reason: "route to 10.50.0.10 leaves through transit0, not leg1"},
		{name: "not from the leg's own table", route: fakeRoute{iface: "leg1"}, reason: "route to 10.50.0.10 leaves through a gateway"},
		{name: "no route", route: fakeRoute{err: ErrNoRoute}, reason: "node has no route to 10.50.0.10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := legScenario(t)
			s.prober.legRoutes[arpKey("leg1", "10.50.0.10")] = tt.route

			res := s.resolve(t, webRoute(), nil)

			requireNotServed(t, res, tt.reason)
			require.Empty(t, s.prober.ops("arp"))
		})
	}
}

// Two networks with the same prefix, each with a guest of the same address:
// each is proven and dialled through its own leg, and the answer of one is
// never the other's, also when the calls of a cycle share what they find.
func TestTwoGuestsWithOneAddressBehindTwoLegsAreToldApart(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "alone", true: "shared"}[shared], func(t *testing.T) {
			s := legScenario(t)
			s.prober.ifaces = append(s.prober.ifaces, HostIface{
				Name: "leg2", Addrs: prefixes("10.50.0.250/24"),
				Segment: Segment{Bridge: "vmbr2"}, Table: FDBRef{Bridge: "vmbr2"}, Via: "vmbr2",
			})
			s.addDB1(true, nicOn(0, mac1, "vmbr2", 0, "10.50.0.10"))
			s.prober.arp[arpKey("leg2", "10.50.0.10")] = []string{mac1}
			s.prober.fdb[fdbKey("vmbr2", 0, mac1)] = []string{"tap102i0"}
			s.prober.dialOn[arpKey("leg2", "10.50.0.10")] = errDial
			var share *Shared
			if shared {
				share = NewShared(nil)
			}

			web := s.resolveShared(t, webRoute(), nil, share)
			db := s.resolveShared(t, routeTo(db1Ref), nil, share)

			require.Equal(t, planner.ResolvedTarget{Addr: ip("10.50.0.10"), Reachable: true, Owner: webOwner}, web.Target)
			require.Equal(t, "vmbr1", web.Binding.Via)
			require.Equal(t, planner.ResolvedTarget{Reason: "port 80: connection refused"}, db.Target, "the port of the first guest is not the second's")
			require.Equal(t, []CandidateResult{{Addr: ip("10.50.0.10"), Source: FromStatic, Reason: "port 80: connection refused", Level: string(LevelPort)}}, db.Candidates)
			require.Equal(t, []probeCall{
				{op: "arp", iface: "leg1", addr: ip("10.50.0.10")},
				{op: "arp", iface: "leg2", addr: ip("10.50.0.10")},
			}, s.prober.ops("arp"))
			require.Len(t, s.prober.ops("dial"), 2, "each leg is dialled")
		})
	}
}

// A proof made through a leg stands, carried by the binding, only while the
// node still reaches the address through that leg; otherwise it is made
// again on the interface there is now.
func TestACarriedProofKeepsItsLegOrIsMadeAgain(t *testing.T) {
	carriedThroughLeg := func() *Binding {
		b := carrying()
		b.Addr, b.Bridge, b.Segment, b.Via = ip("10.50.0.10"), "vmbr1", Segment{Bridge: "vmbr1"}, "vmbr1"
		return b
	}
	yes := func() *Shared { return NewShared(func(Binding) bool { return true }) }

	t.Run("the leg is there", func(t *testing.T) {
		s := legScenario(t)

		res := s.resolveShared(t, webRoute(), carriedThroughLeg(), yes())

		require.Equal(t, carriedThroughLeg(), res.Binding)
		require.Empty(t, s.prober.ops("arp"), "nothing is asked of the wire")
		require.Equal(t, []hint{{op: "dial", iface: "leg1", addr: ip("10.50.0.10")}}, s.prober.hints)
	})

	t.Run("the node has an address of its own there now", func(t *testing.T) {
		s := legScenario(t)
		s.prober.ifaces = []HostIface{{Name: "vmbr1", Addrs: prefixes("10.50.0.2/24"), Segment: Segment{Bridge: "vmbr1"}, Table: FDBRef{Bridge: "vmbr1"}}}
		s.prober.arp[arpKey("vmbr1", "10.50.0.10")] = []string{mac0}

		res := s.resolveShared(t, webRoute(), carriedThroughLeg(), yes())

		require.Equal(t, planner.ResolvedTarget{Addr: ip("10.50.0.10"), Reachable: true, Owner: webOwner}, res.Target)
		require.Equal(t, t0, res.Binding.VerifiedAt, "proven anew")
		require.Empty(t, res.Binding.Via)
		require.Equal(t, []probeCall{{op: "arp", iface: "vmbr1", addr: ip("10.50.0.10")}}, s.prober.ops("arp"))
	})

	t.Run("a binding without a leg once the leg is there", func(t *testing.T) {
		s := legScenario(t)
		direct := carriedThroughLeg()
		direct.Via = ""

		res := s.resolveShared(t, webRoute(), direct, yes())

		require.Equal(t, t0, res.Binding.VerifiedAt, "proven anew")
		require.Equal(t, "vmbr1", res.Binding.Via)
	})
}

// A route of a trusted address the node reaches through a router is asked
// as the host sends it: through no interface of the check.
func TestTheRouteOfATrustedAddressIsTheHosts(t *testing.T) {
	s := newScenario(t)
	s.settings.TrustStatic, s.settings.TrustedCIDRs = true, prefixes("10.40.0.0/16")
	s.web().NICs = []model.NIC{nicOn(0, mac0, "vmbr1", 0, "10.40.0.10")}

	res := s.resolve(t, webRoute(), nil)

	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.40.0.10"), Reachable: true, Owner: webOwner}, res.Target)
	require.Empty(t, res.Binding.Via)
	require.Equal(t, []hint{
		{op: "route", addr: ip("10.40.0.10")},
		{op: "dial", addr: ip("10.40.0.10")},
	}, s.prober.hints)
}

func TestAManualRouteIsDialledAsTheHostSendsIt(t *testing.T) {
	s := newScenario(t)
	route := model.Route{
		Hostname: "manual.example.com",
		Target:   model.Target{Scheme: model.SchemeHTTP, Addr: ip("10.20.0.50"), Port: 8080},
		Source:   model.SourceManual,
	}

	res := s.resolve(t, route, nil)

	require.True(t, res.Target.Reachable, res.Target.Reason)
	require.Equal(t, []hint{{op: "dial", addr: ip("10.20.0.50")}}, s.prober.hints)
}

func TestABindingKeepsItsLegInJSON(t *testing.T) {
	b, err := json.Marshal(Binding{Via: "vmbr1.20"})
	require.NoError(t, err)
	require.Contains(t, string(b), `"via":"vmbr1.20"`)
	var back Binding
	require.NoError(t, json.Unmarshal(b, &back))
	require.Equal(t, "vmbr1.20", back.Via)

	b, err = json.Marshal(Binding{})
	require.NoError(t, err)
	require.NotContains(t, string(b), "via", "an address the node reaches directly names no network")
}

// managedScenario is web-1 with net0 on vmbr0 and net1 on pconet, where the
// managed leg leg3 is: net1 has the lease 10.203.118.5, which the agent
// reports too.
func managedScenario(t *testing.T) *scenario {
	s := newScenario(t)
	managed := model.NIC{Index: 1, MAC: mac1, Bridge: "pconet", Leased: ip("10.203.118.5")}
	s.web().NICs = append(s.web().NICs, managed)
	s.web().Reported = []model.ReportedAddr{rep(mac1, "10.203.118.5")}
	s.prober.ifaces = append(s.prober.ifaces, HostIface{
		Name: "leg3", Addrs: prefixes("10.203.118.1/24"),
		Segment: Segment{Bridge: "pconet"}, Table: FDBRef{Bridge: "pconet"}, Via: "pconet",
	})
	s.prober.arp[arpKey("leg3", "10.203.118.5")] = []string{mac1}
	s.prober.fdb[fdbKey("pconet", 0, mac1)] = []string{"tap101i1"}
	return s
}

// The JSON of the candidates is what the state shows of a route: the leased
// address comes first and says it is pco's.
func TestTheLeasedAddressComesFirstInTheJSONOfTheCandidates(t *testing.T) {
	s := managedScenario(t)

	res := s.resolve(t, webRoute(), nil)

	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.203.118.5"), Reachable: true, Owner: webOwner}, res.Target)
	got, err := json.MarshalIndent(struct {
		Candidates []CandidateResult `json:"candidates"`
		Binding    *Binding          `json:"binding"`
	}{res.Candidates, res.Binding}, "", "  ")
	require.NoError(t, err)
	requireGolden(t, "candidates_managed.json", string(got)+"\n")
}

func TestViaManagedServesTheCardOnTheManagedNetwork(t *testing.T) {
	s := managedScenario(t)

	res := s.resolve(t, routeFor(netip.Addr{}, "managed"), nil)

	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.203.118.5"), Reachable: true, Owner: webOwner}, res.Target)
	require.Equal(t, "pconet", res.Binding.Via)
	require.Equal(t, []CandidateResult{{Addr: ip("10.203.118.5"), Source: FromManaged, OK: true, Level: string(LevelPort)}}, res.Candidates)

	t.Run("a binding on another card does not apply", func(t *testing.T) {
		s := managedScenario(t)
		prev := boundTo("10.20.0.10")

		res := s.resolve(t, routeFor(netip.Addr{}, "managed"), prev)

		require.Equal(t, ip("10.203.118.5"), res.Target.Addr)
		require.Equal(t, []CandidateResult{{Addr: ip("10.203.118.5"), Source: FromManaged, OK: true, Level: string(LevelPort)}}, res.Candidates)
	})

	t.Run("a guest without a card there", func(t *testing.T) {
		s := newScenario(t)

		res := s.resolve(t, routeFor(netip.Addr{}, "managed"), nil)

		require.Equal(t, planner.ResolvedTarget{Rejected: true, Reason: "guest has no network card on pconet"}, res.Target)
	})
}

func TestAProofThroughALegIsRenewedWithItsLeg(t *testing.T) {
	s := legScenario(t)
	s.prober.dialErr[ip("10.50.0.10")] = errDial
	prev := boundTo("10.50.0.10")
	prev.Since = t0.Add(-time.Hour)

	res := s.resolve(t, webRoute(), prev)

	require.Equal(t, planner.ResolvedTarget{Addr: ip("10.50.0.10"), Reason: "port 80: connection refused", Owner: webOwner}, res.Target)
	require.Equal(t, "vmbr1", res.Binding.Via, "a failing dial renews the proof, and its leg with it")
}
