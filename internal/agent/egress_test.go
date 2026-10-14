package agent

import (
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestATargetEntersTheSetOnceVerifiedAndListed(t *testing.T) {
	e := newEnv(t)

	st := e.apply(e.snapshot("www.example.com -> :8080", "api.example.com -> :9090"))

	got, ok := e.egr.last()
	require.True(t, ok)
	require.Equal(t, egressTargets("10.0.0.11:8080", "10.0.0.11:9090"), got)
	require.Equal(t, "on", st.Egress)
	require.Equal(t, egressTargets("10.0.0.11:8080", "10.0.0.11:9090"), e.agent.EgressSet())
}

// A route that goes keeps its target in the set while the configuration last
// verified at Cloudflare still sends a connector to it; it leaves with the
// first snapshot whose verified configuration no longer names it.
func TestATargetLeavesOnlyWhenItIsInNeitherList(t *testing.T) {
	e := newEnv(t)
	e.apply(verified(e.snapshot("www.example.com -> :8080", "api.example.com -> :9090"), "10.0.0.11:8080", "10.0.0.11:9090"))

	st := e.apply(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080", "10.0.0.11:9090"))
	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080", "10.0.0.11:9090"), got, "the rule is still at Cloudflare")
	require.Len(t, st.Targets, 1)

	e.apply(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080"))
	got, _ = e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got)
}

// A target the verified configuration names but the agent never had in its
// set does not enter by that alone: only what it verified enters.
func TestAVerifiedConfigurationAddsNothingToTheSet(t *testing.T) {
	e := newEnv(t)

	e.apply(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080", "10.0.0.50:443"))

	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got)
}

// Only the configurations of the tunnels this node may run keep a target in
// its set: the shared one and the one pinned to this node. One pinned to
// another node, or a tunnel the install does not name, vouches for nothing
// here.
func TestOnlyTheTunnelsOfThisNodeKeepATargetInItsSet(t *testing.T) {
	keep := func(name, kind string) []egress.Target {
		e := newEnv(t)
		e.apply(e.snapshot("www.example.com -> :8080", "api.example.com -> :9090"))
		s := e.snapshot("www.example.com -> :8080")
		s.Tunnels = append(s.Tunnels, agentproto.Tunnel{
			AccountID: "acc1", TunnelID: "t9", Name: name, Kind: kind,
			Verified: []netip.AddrPort{netip.MustParseAddrPort("10.0.0.11:9090")},
		})
		e.apply(s)
		got, _ := e.egr.last()
		return got
	}
	require.Equal(t, egressTargets("10.0.0.11:8080", "10.0.0.11:9090"), keep("pco-"+testInstall+"-"+testNode, agentproto.TunnelPinned))
	require.Equal(t, egressTargets("10.0.0.11:8080"), keep("pco-"+testInstall+"-pve2", agentproto.TunnelPinned))
	require.Equal(t, egressTargets("10.0.0.11:8080"), keep("pco-"+testInstall+"-pve2", agentproto.TunnelShared))
	require.Equal(t, egressTargets("10.0.0.11:8080"), keep("pco-other1", agentproto.TunnelShared))
}

func TestAnAddressThatLostItsProofLeavesTheFilterAtOnce(t *testing.T) {
	e := newEnv(t)
	log := &callLog{}
	e.egr.log = log
	s := verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080")
	e.apply(s)
	e.res.stop("www.example.com", "identity check failed: 10.0.0.11 answered by bc:24:11:ff:ff:01")
	from := len(log.all())

	e.clock.advance(10 * time.Second)
	st := e.apply(s)

	calls := log.all()[from:]
	require.Equal(t, []string{"egress remove 10.0.0.11", "egress set none"}, calls, "out at once, and out of the set whose rule is still at Cloudflare")
	require.False(t, report(st, "www.example.com").InEgressSet)

	t.Run("once", func(t *testing.T) {
		e.clock.advance(10 * time.Second)
		e.apply(s)

		require.Equal(t, []netip.Addr{guestAddr}, e.egr.removes(), "a binding withdrawn before lost nothing")
		got, _ := e.egr.last()
		require.Empty(t, got, "a withdrawn address is no verified target")
	})
}

// An address that lost its proof is taken out of the filter only when the set
// holds it: a target held back by its minimum never put it there.
func TestAnAddressTheSetDoesNotHoldIsNotRemoved(t *testing.T) {
	e := newEnv(t)
	e.res.observe("www.example.com")
	e.apply(e.snapshot("www.example.com -> :8080"))
	got, _ := e.egr.last()
	require.Empty(t, got)

	e.res.stop("www.example.com", "guest is not running")
	e.apply(e.snapshot("www.example.com -> :8080"))

	require.Empty(t, e.egr.removes())
}

// A target whose port does not answer is still proven: it stays in the set,
// as the engine keeps it, and is reported not to verify.
func TestATargetWhosePortDoesNotAnswerStaysInTheSet(t *testing.T) {
	e := newEnv(t)
	e.res.silence("www.example.com")

	st := e.apply(e.snapshot("www.example.com -> :8080"))

	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got)
	r := report(st, "www.example.com")
	require.False(t, r.OK)
	require.True(t, r.InEgressSet)
	require.Equal(t, "dial tcp 10.0.0.11:8080: connection refused", r.Reason)
}

func TestARejectedTargetIsNotInTheSet(t *testing.T) {
	e := newEnv(t)
	e.res.reject("www.example.com")

	st := e.apply(e.snapshot("www.example.com -> :8080"))

	got, _ := e.egr.last()
	require.Empty(t, got)
	require.Equal(t, "address of a cluster node", report(st, "www.example.com").Reason)
}

func TestATargetBelowItsMinimumIsNotInTheSet(t *testing.T) {
	e := newEnv(t)
	e.res.observe("www.example.com")

	st := e.apply(e.snapshot("www.example.com -> :8080", "api.example.com -> :9090"))

	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:9090"), got)
	r := report(st, "www.example.com")
	require.False(t, r.OK)
	require.Equal(t, "identity level observed is below the required port", r.Reason)
	res, err := DecodeResult(r.Result)
	require.NoError(t, err)
	require.Equal(t, guestAddr, res.Target.Addr, "the result is the resolver's, for the controller to judge")
	require.Equal(t, resolve.LevelObserved, res.Level)

	s := e.snapshot("www.example.com -> :8080", "api.example.com -> :9090")
	s.Targets[0].Required = "observed"
	st = e.apply(s)
	require.True(t, report(st, "www.example.com").InEgressSet, "a minimum of observed lets it in")
}

// An address whose proof falls below the minimum is still proven: it leaves
// the set after its rule, as a route that goes does. Only a lost proof takes
// it out at once.
func TestATargetThatFallsBelowItsMinimumLeavesAfterItsRule(t *testing.T) {
	e := newEnv(t)
	s := verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080")
	e.apply(s)
	e.res.observe("www.example.com")

	e.apply(s)
	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got)
	require.Empty(t, e.egr.removes())

	e.apply(verified(e.snapshot("www.example.com -> :8080")))
	got, _ = e.egr.last()
	require.Empty(t, got)
}

func TestAManualRouteToTheNodeIsAnExactEntry(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot()
	s.Targets = []agentproto.Target{manualTarget("pve.example.com", netip.AddrPortFrom(nodeAddr, 8006), true)}

	e.apply(s)

	got, _ := e.egr.last()
	require.Equal(t, []egress.Target{{Addr: nodeAddr, Port: 8006, AllowNode: true}}, got, "accepted before the addresses of the node are refused")

	// Kept as such while its rule is verified, also by a restarted agent,
	// whose memory has no mark.
	e.agent = e.newAgent()
	e.apply(verified(e.snapshot(), "10.0.0.2:8006"))
	got, _ = e.egr.last()
	require.Equal(t, []egress.Target{{Addr: nodeAddr, Port: 8006}}, got, "a restarted agent keeps it until the rule is gone")

	s = verified(e.snapshot(), "10.0.0.2:8006")
	s.Targets = []agentproto.Target{manualTarget("pve.example.com", netip.AddrPortFrom(nodeAddr, 8006), true)}
	e.apply(s)
	got, _ = e.egr.last()
	require.Equal(t, []egress.Target{{Addr: nodeAddr, Port: 8006, AllowNode: true}}, got, "one of allowNode is the one kept")
}

// The agent stops after it saved a withdrawn binding and before it saved its
// set: the set it reads at its start still has the address, and the verified
// configuration its rule. The binding says it is withdrawn, which keeps it
// out whatever the set says.
func TestAWithdrawnAddressDoesNotComeBackThroughAStaleSet(t *testing.T) {
	e := newEnv(t)
	s := verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080")
	e.apply(s)
	stale, _, err := e.store.AgentEgress()
	require.NoError(t, err)
	require.Equal(t, []netip.AddrPort{netip.MustParseAddrPort("10.0.0.11:8080")}, stale)
	e.res.stop("www.example.com", "identity check failed: 10.0.0.11 answered by bc:24:11:ff:ff:01")
	e.apply(s)
	require.NoError(t, e.store.SaveAgentEgress(stale), "the set that pass could not save")

	e.agent = e.newAgent()
	for range 2 {
		e.clock.advance(10 * time.Second)
		e.apply(s)

		got, _ := e.egr.last()
		require.Empty(t, got)
	}

	t.Run("of a route that is gone", func(t *testing.T) {
		require.NoError(t, e.store.SaveAgentEgress(stale))
		e.agent = e.newAgent()

		e.apply(verified(e.snapshot(), "10.0.0.11:8080"))

		got, _ := e.egr.last()
		require.Empty(t, got, "nothing resolves it, but its stored binding says it lost its proof")
	})
}

func TestTheSetSurvivesARestart(t *testing.T) {
	e := newEnv(t)
	e.apply(verified(e.snapshot("www.example.com -> :8080", "api.example.com -> :9090"), "10.0.0.11:8080", "10.0.0.11:9090"))

	e.agent = e.newAgent()
	e.apply(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080", "10.0.0.11:9090"))

	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080", "10.0.0.11:9090"), got)

	e.agent = e.newAgent()
	e.apply(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080"))
	got, _ = e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got)
}

// The first agent on a node that ran the engine of an earlier release finds
// the set where that engine kept it.
func TestTheSetOfAnEarlierReleaseIsTakenOver(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.store.SaveEngineMemory(store.EngineMemory{InstallID: testInstall, Egress: []netip.AddrPort{netip.MustParseAddrPort("10.0.0.11:9090")}}))
	e.agent = e.newAgent()

	e.apply(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:9090"))

	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080", "10.0.0.11:9090"), got)
}

func TestABlockTakesAnAddressOutAtOnceAndKeepsItOut(t *testing.T) {
	e := newEnv(t)
	log := &callLog{}
	e.egr.log = log
	e.apply(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080"))
	from := len(log.all())
	resolved := 0
	e.res.onResolve = func(string) { resolved = len(log.all()) }

	s := verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080")
	s.Blocks = []agentproto.Block{{Addr: guestAddr, Reason: agentproto.BlockConflict, Until: t0.Add(10 * time.Minute)}}
	st := e.apply(s)

	require.Equal(t, "egress remove 10.0.0.11", log.all()[from])
	require.Equal(t, from+1, resolved, "before the targets are resolved")
	got, _ := e.egr.last()
	require.Empty(t, got)
	require.False(t, report(st, "www.example.com").InEgressSet)

	e.clock.advance(11 * time.Minute)
	e.apply(s)
	got, _ = e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got, "a block whose time is over holds no more")
}

func TestALocalBlockIsReported(t *testing.T) {
	e := newEnv(t)
	e.blocked = []netip.Addr{guestAddr}

	st := e.apply(e.snapshot("www.example.com -> :8080"))

	require.Equal(t, []netip.Addr{guestAddr}, st.LocalBlocks)
	r := report(st, "www.example.com")
	require.True(t, r.OK)
	require.False(t, r.InEgressSet, "the filter refuses what is blocked on the node")
}

func TestAFailedSetIsAProblemAndASwitchedOffFilterIsNone(t *testing.T) {
	e := newEnv(t)
	e.egr.failSet(errBoom)

	st := e.apply(e.snapshot("www.example.com -> :8080"))

	require.Equal(t, []string{"setting the egress filter: boom"}, st.Problems)
	require.Equal(t, "not loaded", st.Egress)
	require.False(t, report(st, "www.example.com").InEgressSet)

	e.egr.failSet(egress.ErrOff)
	st = e.apply(e.snapshot("www.example.com -> :8080"))
	require.Empty(t, st.Problems)
	require.Equal(t, "off", st.Egress)
}

// A target on a leg is dialed at the service address the controller gave its
// address on that network, and the egress set holds that; without one it is
// not in the set.
func TestATargetOnALegIsInTheSetByItsServiceAddress(t *testing.T) {
	e := newEnv(t)
	e.res.onLeg("www.example.com", "lan")
	service := netip.MustParseAddr("10.203.118.5")

	st := e.apply(e.snapshot("www.example.com -> :8080"))
	got, _ := e.egr.last()
	require.Empty(t, got, "no service address yet")
	require.False(t, report(st, "www.example.com").InEgressSet)

	s := e.snapshot("www.example.com -> :8080")
	s.Targets[0].Addr, s.Targets[0].Service = guestAddr, service
	st = e.apply(s)
	got, _ = e.egr.last()
	require.Equal(t, egressTargets("10.203.118.5:8080"), got)
	require.True(t, report(st, "www.example.com").InEgressSet)
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Equal(t, service, bindings["www.example.com"].Service, "the binding says what it was published under")

	e.res.moveTo("www.example.com", netip.MustParseAddr("10.0.0.12"))
	e.apply(s)
	got, _ = e.egr.last()
	require.Empty(t, got, "a service address given for another address is not this one's")
}

// A service address forwards to one address on a leg. When the guest of its
// target moves to another, it would forward to an address anyone may answer
// at now: it leaves the filter at once, the set and the gateway, though the
// configuration verified at Cloudflare still names it, until the controller
// moves it.
func TestTheServiceAddressOfATargetThatLeftItsAddressForwardsNothing(t *testing.T) {
	e := newEnv(t)
	e.gw = &fakeGateway{}
	e.agent = e.newAgent()
	e.res.onLeg("www.example.com", "lan")
	legs, err := json.Marshal(gateway.Cache{Legs: []gateway.Leg{{
		ID: "lan", Mark: 3, Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.0.0.250/24"),
		MAC: mustMAC("02:50:43:4f:00:03"),
	}}})
	require.NoError(t, err)
	service := netip.MustParseAddr("10.203.118.5")
	s := verified(e.snapshot("www.example.com -> :8080"), "10.203.118.5:8080")
	s.Networks = legs
	s.Targets[0].Addr, s.Targets[0].Service = guestAddr, service
	e.apply(s)
	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.203.118.5:8080"), got)

	e.res.moveTo("www.example.com", netip.MustParseAddr("10.0.0.12"))
	st := e.apply(s)

	require.Equal(t, []netip.Addr{service}, e.egr.removes(), "out of the filter at once")
	got, _ = e.egr.last()
	require.Empty(t, got, "and out of the set, though the verified configuration names it")
	sync, ok := e.gw.last()
	require.True(t, ok)
	require.Empty(t, sync.services, "the gateway forwards nothing to the address left")
	require.False(t, report(st, "www.example.com").InEgressSet)
	require.Contains(t, e.events.messages(), "www.example.com left 10.0.0.11 on network lan; its service address 10.203.118.5 forwards nothing until the controller moves it")

	t.Run("it stays while another target is still served at the address", func(t *testing.T) {
		e := newEnv(t)
		e.res.onLeg("www.example.com", "lan")
		e.res.onLeg("api.example.com", "lan")
		s := verified(e.snapshot("www.example.com -> :8080", "api.example.com -> :9090"), "10.203.118.5:8080", "10.203.118.5:9090")
		for i := range s.Targets {
			s.Targets[i].Addr, s.Targets[i].Service = guestAddr, service
		}
		e.apply(s)

		e.res.moveTo("www.example.com", netip.MustParseAddr("10.0.0.12"))
		e.apply(s)

		require.Empty(t, e.egr.removes())
		got, _ := e.egr.last()
		require.Equal(t, egressTargets("10.203.118.5:8080", "10.203.118.5:9090"), got, "api is served there still, and www keeps its rule")
	})

	t.Run("the same address on another network is another address", func(t *testing.T) {
		e := newEnv(t)
		e.res.onLeg("www.example.com", "lan")
		s := verified(e.snapshot("www.example.com -> :8080"), "10.203.118.5:8080")
		s.Targets[0].Addr, s.Targets[0].Service = guestAddr, service
		e.apply(s)

		e.res.onLeg("www.example.com", "dmz")
		e.apply(s)

		require.Equal(t, []netip.Addr{service}, e.egr.removes())
		got, _ := e.egr.last()
		require.Empty(t, got)
	})
}

func TestTheGatewayIsGivenTheLegsAndWhatTheSetHoldsBehindThem(t *testing.T) {
	e := newEnv(t)
	e.gw = &fakeGateway{}
	e.agent = e.newAgent()
	e.res.onLeg("www.example.com", "lan")
	legs, err := json.Marshal(gateway.Cache{Legs: []gateway.Leg{{
		ID: "lan", Mark: 3, Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.0.0.250/24"),
		MAC: mustMAC("02:50:43:4f:00:03"),
	}}})
	require.NoError(t, err)
	s := e.snapshot("www.example.com -> :8080")
	s.Networks = legs
	s.Targets[0].Addr, s.Targets[0].Service = guestAddr, netip.MustParseAddr("10.203.118.5")

	e.apply(s)

	got, ok := e.gw.last()
	require.True(t, ok)
	require.Len(t, got.legs, 1)
	require.Equal(t, []gateway.Service{{Leg: "lan", Service: netip.MustParseAddr("10.203.118.5"), Addr: guestAddr, Ports: []uint16{8080}}}, got.services)
	require.Equal(t, []gateway.Pin{{Leg: "lan", Addr: guestAddr, MAC: mustMAC(testMAC)}}, got.pins)
	pin := e.agent.Bound()[netip.MustParseAddr("10.203.118.5")]
	require.Equal(t, "leg3", pin.Dev, "the watch knows the leg the guest answers on")
	require.Equal(t, guestAddr, pin.Real)

	t.Run("without networks the gateway is left alone", func(t *testing.T) {
		syncs := len(e.gw.syncs)
		e.apply(e.snapshot("api.example.com -> :9090"))
		require.Len(t, e.gw.syncs, syncs)
	})
}
