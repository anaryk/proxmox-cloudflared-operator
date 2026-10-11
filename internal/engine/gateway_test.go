package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// legHost is the route of web-1, and dbMAC the MAC of db-1.
const (
	legHost = "www.example.com"
	dbMAC   = "bc:24:11:00:00:02"
)

var (
	// legAddr is the address of the guests behind the legs: both networks
	// have the subnet 10.50.0.0/24.
	legAddr = netip.MustParseAddr("10.50.0.10")
	svc0    = netip.MustParseAddr("198.18.1.0") // the first address of the pool
	svc1    = netip.MustParseAddr("198.18.1.1")
)

// fakeGateway records what the engine gives the gateway, in the log of the
// other fakes too.
type fakeGateway struct {
	mu    sync.Mutex
	log   *callLog
	syncs []gatewaySync
	err   error
	down  map[string]string // the legs Sync reports not up, by network, with why
	check func() GatewayCheck
}

type gatewaySync struct {
	legs     []gateway.Leg
	services []gateway.Service
	pins     []gateway.Pin
	sticky   []gateway.Sticky
}

func (f *fakeGateway) Sync(_ context.Context, legs []gateway.Leg, services []gateway.Service, pins []gateway.Pin, sticky []gateway.Sticky) ([]gateway.LegState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs = append(f.syncs, gatewaySync{legs: slices.Clone(legs), services: slices.Clone(services), pins: slices.Clone(pins), sticky: slices.Clone(sticky)})
	f.log.add("gateway sync " + servicesText(services))
	if f.err != nil {
		return nil, f.err
	}
	states := make([]gateway.LegState, 0, len(legs))
	for _, l := range legs {
		why, down := f.down[l.ID]
		states = append(states, gateway.LegState{ID: l.ID, Iface: l.Iface(), Up: !down, Problem: why})
	}
	return states, nil
}

func (f *fakeGateway) Up(context.Context) error     { return nil }
func (f *fakeGateway) Verify(context.Context) error { return nil }
func (f *fakeGateway) Detach(context.Context, string) error {
	return nil
}

func (f *fakeGateway) Probe(context.Context, string, int, netip.Addr) ([]net.HardwareAddr, error) {
	return nil, nil
}

func (f *fakeGateway) Dial(context.Context, gateway.Leg, netip.AddrPort) (net.Conn, error) {
	return nil, errors.New("no dial in the fake")
}

func (f *fakeGateway) Check(context.Context) GatewayCheck {
	f.mu.Lock()
	check := f.check
	f.mu.Unlock()
	if check == nil {
		return GatewayCheck{}
	}
	return check()
}

func (f *fakeGateway) onCheck(check func() GatewayCheck) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.check = check
}

func (f *fakeGateway) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeGateway) setDown(id, why string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down[id] = why
}

// last is what the last Sync was given.
func (f *fakeGateway) last(t *testing.T) gatewaySync {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.syncs, "the gateway was never synced")
	return f.syncs[len(f.syncs)-1]
}

func (f *fakeGateway) syncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.syncs)
}

func servicesText(services []gateway.Service) string {
	if len(services) == 0 {
		return "none"
	}
	parts := make([]string, len(services))
	for i, s := range services {
		ports := make([]string, len(s.Ports))
		for j, p := range s.Ports {
			ports[j] = fmt.Sprint(p)
		}
		parts[i] = fmt.Sprintf("%s->%s:%s", s.Service, s.Addr, strings.Join(ports, ","))
	}
	return strings.Join(parts, " ")
}

func (f *fakeResolver) goVia(host, network string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vias[host] = network
}

func (f *fakeResolver) resolved() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.hosts)
}

// legEnv is an env with two attached networks, vmbr1 and vmbr2, both with
// the subnet 10.50.0.0/24 and the leg address 10.50.0.250 on this node, a
// gateway that records what it is given, and www.example.com of web-1 proven
// at 10.50.0.10 through the leg of vmbr1.
type legEnv struct {
	*env
	gw  *fakeGateway
	log *callLog
}

func newLegEnv(t *testing.T) *legEnv {
	t.Helper()
	e, log := orderLogged(t)
	e.enforce()
	gw := &fakeGateway{log: log, down: map[string]string{}}
	e.gateway = gw
	saveNetwork(t, e.store, "vmbr1", 1)
	saveNetwork(t, e.store, "vmbr2", 2)
	e.res.goVia(legHost, "vmbr1")
	e.res.moveTo(legHost, legAddr)
	e.eng = e.newEngine()
	return &legEnv{env: e, gw: gw, log: log}
}

func saveNetwork(t *testing.T, s *store.Store, id string, mark int) {
	t.Helper()
	require.NoError(t, s.SaveNetwork(store.Network{
		ID: id, Kind: store.NetworkAttach, Bridge: id, Prefix: netip.MustParsePrefix("10.50.0.0/24"),
		Legs:   map[string]netip.Addr{testNode: netip.MustParseAddr("10.50.0.250")},
		LegMAC: store.LegMACFor(store.NetworkAttach, mark), Mark: mark, CreatedAt: t0,
	}, 0))
}

// legOf is the leg the engine wants for a network saved by saveNetwork.
func legOf(t *testing.T, id string, mark int) gateway.Leg {
	return gateway.Leg{
		ID: id, Mark: mark, Bridge: id, Prefix: netip.MustParsePrefix("10.50.0.250/24"),
		MAC: hardware(t, store.LegMACFor(store.NetworkAttach, mark)),
	}
}

// warm runs the first cycle of the engine, which makes no service address:
// until a cycle ran, nothing says that this process is the writer.
func (e *legEnv) warm() State {
	e.t.Helper()
	st := e.cycle()
	e.clock.advance(20 * time.Second)
	return st
}

// next runs a cycle twenty seconds after the last.
func (e *legEnv) next() State {
	e.t.Helper()
	st := e.cycle()
	e.clock.advance(20 * time.Second)
	return st
}

func (e *legEnv) mappings() map[string]netip.Addr {
	e.t.Helper()
	all, err := e.store.ServiceAddrs()
	require.NoError(e.t, err)
	out := map[string]netip.Addr{}
	for _, m := range all {
		out[m.Network+" "+m.Addr.String()] = m.Service
	}
	return out
}

// requireNeverSet fails when any set the egress filter was given held addr.
func (e *legEnv) requireNeverSet(addr netip.Addr) {
	e.t.Helper()
	e.egr.mu.Lock()
	defer e.egr.mu.Unlock()
	for _, set := range e.egr.sets {
		for _, tg := range set {
			require.NotEqual(e.t, addr, tg.Addr, "the egress set holds the real address of a target on a leg")
		}
	}
}

// requireOnlyServiceAddresses fails when a rule of a host in hosts, in the
// state or at Cloudflare, sends a connector anywhere but to a service
// address.
func requireOnlyServiceAddresses(t *testing.T, st State, rules []planner.IngressRule, hosts ...string) {
	t.Helper()
	all := slices.Clone(rules)
	for _, r := range st.Routes {
		if r.Rule != nil {
			all = append(all, *r.Rule)
		}
	}
	for _, r := range all {
		if !slices.Contains(hosts, r.Hostname) {
			continue
		}
		_, origin, ok := strings.Cut(r.Service, "://")
		if !ok {
			continue
		}
		ap, err := netip.ParseAddrPort(origin)
		require.NoError(t, err)
		require.True(t, gateway.ServicePrefix.Contains(ap.Addr()), "%s is sent to %s", r.Hostname, ap)
	}
}

func TestARouteOnALegIsServedThroughItsServiceAddress(t *testing.T) {
	e := newLegEnv(t)
	st := e.warm()

	r := route(st, legHost)
	require.Equal(t, planner.StateUnreachable, r.State)
	require.Equal(t, "service address not allocated: no cycle has run since the start", r.Reason)
	require.Empty(t, e.mappings())

	from := len(e.log.all())
	st = e.cycle()

	require.Empty(t, st.Problems)
	r = route(st, legHost)
	require.Equal(t, planner.StateActive, r.State)
	require.Equal(t, "http://198.18.1.0:8080", r.Service)
	require.Equal(t, withSentinel(hostRuleAt(legHost, "http://198.18.1.0:8080")), e.rules())
	require.Equal(t, map[string]netip.Addr{"vmbr1 10.50.0.10": svc0}, e.mappings())
	got, _ := e.egr.last()
	require.Equal(t, egressTargets("198.18.1.0:8080"), got)
	e.requireNeverSet(legAddr)

	sync := e.log.index("gateway sync 198.18.1.0->10.50.0.10:8080", from)
	set := e.log.index("egress set 198.18.1.0:8080", from)
	put := e.log.index("cf PutTunnelConfig", from)
	require.GreaterOrEqual(t, sync, 0, e.log.all())
	require.GreaterOrEqual(t, set, 0, e.log.all())
	require.Greater(t, put, sync, "the gateway is programmed before the rule is written")
	require.Greater(t, put, set, e.log.all())

	last := e.gw.last(t)
	require.Equal(t, []gateway.Leg{legOf(t, "vmbr1", 1), legOf(t, "vmbr2", 2)}, last.legs)
	require.Equal(t, []gateway.Service{{Leg: "vmbr1", Service: svc0, Addr: legAddr, Ports: []uint16{8080}}}, last.services)
	require.Equal(t, []gateway.Pin{{Leg: "vmbr1", Addr: legAddr, MAC: hardware(t, testMAC)}}, last.pins)
	require.Empty(t, last.sticky)

	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Equal(t, svc0, bindings[legHost].Service)
	require.Equal(t, "vmbr1", bindings[legHost].Via)

	require.NotNil(t, r.Path)
	require.Equal(t, "attach:vmbr1", r.Path.Via)
	require.Equal(t, svc0, r.Path.Service)
	require.Equal(t, legAddr, r.Path.Addr)
	require.Equal(t, LegUp, r.Path.Leg)

	events := e.eng.Events(time.Time{})
	require.Contains(t, unnumbered(events), Event{At: e.clock.now(), Level: levelInfo, Kind: kindNetwork, Subject: "vmbr1", Route: legHost,
		Message: "service address 198.18.1.0 allocated for 10.50.0.10 on network vmbr1"})
}

func TestADirectRouteHasNoServiceAddress(t *testing.T) {
	e := newLegEnv(t)
	e.res.goVia(legHost, "")
	e.res.moveTo(legHost, guestAddr)

	st := e.cycle()

	r := route(st, legHost)
	require.Equal(t, planner.StateActive, r.State)
	require.Equal(t, "http://10.0.0.11:8080", r.Service, "a direct target needs no cycle before it")
	require.Equal(t, "direct", r.Path.Via)
	require.False(t, r.Path.Service.IsValid())
	require.Empty(t, e.mappings())
	require.Empty(t, e.gw.last(t).services)
}

// A node without a network to reach never asks the gateway for anything.
func TestANodeWithoutNetworksLeavesTheGatewayAlone(t *testing.T) {
	e := newEnv(t)
	gw := &fakeGateway{down: map[string]string{}}
	e.gateway = gw
	e.eng = e.newEngine()

	for range 3 {
		require.Empty(t, e.cycle().Problems)
		e.clock.advance(20 * time.Second)
	}
	require.Zero(t, gw.syncCount())
}

func TestTwoNetworksWithOneAddressGetTwoServiceAddresses(t *testing.T) {
	e := newLegEnv(t)
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), guest(102, "db-1", "db.example.com -> :5432")))
	e.res.goVia("db.example.com", "vmbr2")
	e.res.moveTo("db.example.com", legAddr)
	e.res.answerFrom("db.example.com", dbMAC)
	e.warm()

	st := e.cycle()

	require.Empty(t, st.Problems)
	m := e.mappings()
	require.Len(t, m, 2)
	web, db := m["vmbr1 10.50.0.10"], m["vmbr2 10.50.0.10"]
	require.NotEqual(t, web, db)
	require.ElementsMatch(t, []netip.Addr{svc0, svc1}, []netip.Addr{web, db})
	require.Equal(t, "http://"+netip.AddrPortFrom(web, 8080).String(), route(st, legHost).Service)
	require.Equal(t, "http://"+netip.AddrPortFrom(db, 5432).String(), route(st, "db.example.com").Service)
	got, _ := e.egr.last()
	require.ElementsMatch(t, []egress.Target{{Addr: web, Port: 8080}, {Addr: db, Port: 5432}}, got)
	e.requireNeverSet(legAddr)
	last := e.gw.last(t)
	require.ElementsMatch(t, []gateway.Service{
		{Leg: "vmbr1", Service: web, Addr: legAddr, Ports: []uint16{8080}},
		{Leg: "vmbr2", Service: db, Addr: legAddr, Ports: []uint16{5432}},
	}, last.services)
	require.ElementsMatch(t, []gateway.Pin{
		{Leg: "vmbr1", Addr: legAddr, MAC: hardware(t, testMAC)},
		{Leg: "vmbr2", Addr: legAddr, MAC: hardware(t, dbMAC)},
	}, last.pins)
}

// Two routes to one guest share its service address, with the ports of both;
// a route that goes takes its port away only after the tunnel run verified
// its rule gone, and the last one takes the service address away then. The
// same guest published again gets a new one.
func TestAServiceAddressLeavesTheGatewayOnlyAfterItsRulesAreGone(t *testing.T) {
	e := newLegEnv(t)
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :9090")))
	e.res.goVia("api.example.com", "vmbr1")
	e.res.moveTo("api.example.com", legAddr)
	e.warm()
	e.next()
	require.Equal(t, []gateway.Service{{Leg: "vmbr1", Service: svc0, Addr: legAddr, Ports: []uint16{8080, 9090}}}, e.gw.last(t).services)

	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080")))
	e.next()
	require.Equal(t, []uint16{8080, 9090}, e.gw.last(t).services[0].Ports, "the rule was still there when the gateway was synced")
	got, _ := e.egr.last()
	require.Equal(t, egressTargets("198.18.1.0:8080", "198.18.1.0:9090"), got)

	e.next()
	require.Equal(t, []uint16{8080}, e.gw.last(t).services[0].Ports)
	got, _ = e.egr.last()
	require.Equal(t, egressTargets("198.18.1.0:8080"), got)

	e.inv.set(snapshot(guest(101, "web-1")))
	e.next()
	require.Len(t, e.gw.last(t).services, 1)
	require.Len(t, e.mappings(), 1, "kept while a verified configuration sends a connector there")

	st := e.next()
	require.Empty(t, e.gw.last(t).services)
	got, _ = e.egr.last()
	require.Empty(t, got)
	require.Empty(t, e.mappings(), "released once the rule is gone")
	require.Contains(t, unnumbered(e.eng.Events(time.Time{})), Event{At: st.At, Level: levelInfo, Kind: kindNetwork, Subject: "vmbr1",
		Message: "service address 198.18.1.0 of 10.50.0.10 on network vmbr1 released"})

	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080")))
	st = e.next()
	require.Equal(t, "http://198.18.1.1:8080", route(st, legHost).Service, "an address released is never given again")
	require.Equal(t, map[string]netip.Addr{"vmbr1 10.50.0.10": svc1}, e.mappings())
}

// A target whose proof is lost leaves the egress set at once by the address
// the set holds for it, its service address; a direct target at the same
// real address on another network stays.
func TestALostLegProofTakesItsServiceAddressOutAtOnce(t *testing.T) {
	e := newLegEnv(t)
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), guest(103, "nas", "nas.example.com -> :8080")))
	e.res.moveTo("nas.example.com", legAddr)
	e.warm()
	e.next()
	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.50.0.10:8080", "198.18.1.0:8080"), got)

	e.res.stop(legHost, "10.50.0.10 answered by bc:24:11:ff:ff:01, which is not this guest")
	st := e.cycle()

	require.Equal(t, []netip.Addr{svc0}, e.egr.removes())
	got, _ = e.egr.last()
	require.Equal(t, egressTargets("10.50.0.10:8080"), got, "the withdrawn target is not fed back while its rule is verified")
	require.Equal(t, planner.StateWithdrawn, route(st, legHost).State)
	require.Empty(t, e.gw.last(t).services)
	require.Len(t, e.mappings(), 1, "the rule was verified before this cycle's run")
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Equal(t, svc0, bindings[legHost].Service, "a withdrawn binding keeps the address it was published under")
}

// A target the minimum holds back is still published under its service
// address until its rule is gone, and its binding keeps that address, which a
// proof lost later takes it out of the set by.
func TestALegTargetHeldBackKeepsItsServiceAddress(t *testing.T) {
	e := newLegEnv(t)
	e.warm()
	e.next()
	e.res.setLevel(legHost, resolve.LevelObserved)

	e.next()

	got, _ := e.egr.last()
	require.Equal(t, egressTargets("198.18.1.0:8080"), got, "until the run verified its rule gone")
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Equal(t, svc0, bindings[legHost].Service)

	e.res.stop(legHost, "10.50.0.10 answered by bc:24:11:ff:ff:01, which is not this guest")
	e.cycle()

	got, _ = e.egr.last()
	require.Empty(t, got)
	require.Equal(t, []netip.Addr{svc0}, e.egr.removes(), "taken out at once by its service address")
}

func TestASyncThatFailsHoldsTheTunnelRun(t *testing.T) {
	e := newLegEnv(t)
	e.warm()
	e.gw.fail(errors.New("nft: table pco_gw: busy"))
	writes := len(e.writes())

	st := e.cycle()

	require.Len(t, e.writes(), writes, "nothing is written")
	require.Contains(t, st.Problems, "programming the gateway: nft: table pco_gw: busy; no tunnel configuration is written until it is programmed")
	require.NotEmpty(t, st.Actions)
	for _, a := range st.Actions {
		require.False(t, a.Applied)
	}
	require.Contains(t, actionKinds(st), "create-tunnel "+tunnelName+" held: "+heldGateway)
	require.Len(t, e.mappings(), 1, "the address was made before the gateway was asked")
}

func TestALegThatIsNotUpIsAProblem(t *testing.T) {
	e := newLegEnv(t)
	e.gw.setDown("vmbr2", "bridge vmbr2 is absent")

	st := e.cycle()

	require.Contains(t, st.Problems, "network vmbr2: the leg is not up: bridge vmbr2 is absent")
	require.Equal(t, []gateway.LegState{
		{ID: "vmbr1", Iface: "leg1", Up: true},
		{ID: "vmbr2", Iface: "leg2", Problem: "bridge vmbr2 is absent"},
	}, e.eng.LegStates())
}

// The cache of the gateway, the cache of its objects and the pins are
// written after every Sync, for pco netns up to restore at boot.
func TestTheCachesAreWrittenAfterEverySync(t *testing.T) {
	e := newLegEnv(t)
	e.warm()

	st := e.cycle()

	c, found, err := gateway.ReadCache(e.paths.Local)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, gateway.Cache{
		Legs:     []gateway.Leg{legOf(t, "vmbr1", 1), legOf(t, "vmbr2", 2)},
		Services: []gateway.Service{{Leg: "vmbr1", Service: svc0, Addr: legAddr, Ports: []uint16{8080}}},
		Pins:     []gateway.Pin{{Leg: "vmbr1", Addr: legAddr, MAC: hardware(t, testMAC)}},
		PinsAt:   st.At,
	}, c)
	nc, found, err := e.store.NetCache()
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, nc.Networks, 2)
	require.Equal(t, []store.ServiceAddr{{Network: "vmbr1", Addr: legAddr, Service: svc0, AllocatedAt: st.At}}, nc.ServiceAddrs)
	pins, at, err := e.store.Pins()
	require.NoError(t, err)
	require.Equal(t, []store.Pin{{Network: "vmbr1", Addr: legAddr, MAC: testMAC}}, pins)
	require.Equal(t, st.At, at)
}

func TestACacheThatCannotBeWrittenIsAProblem(t *testing.T) {
	e := newLegEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(e.paths.Local, "netns"), []byte("in the way\n"), 0o600))

	st := e.cycle()

	require.True(t, hasProblem(st, "saving the cache of the gateway: "), st.Problems)
	require.Empty(t, actionKinds(st), "a cache that is not written holds nothing")
}

// The gate of a new service address, one test for each thing that closes it:
// the route is held with the reason, and the rest of the cycle is as it would
// be.
func TestAClosedGateHoldsANewServiceAddress(t *testing.T) {
	tests := []struct {
		name  string
		close func(e *legEnv)
		why   string
	}{
		{
			name:  "the previous cycle found a stale writer",
			close: func(e *legEnv) { e.eng.lastVerdict = VerdictStale },
			why:   "the previous cycle found a stale writer",
		},
		{
			name:  "the previous cycle did not get to the tunnel run",
			close: func(e *legEnv) { e.eng.lastVerdict = verdictNoRun },
			why:   "the previous cycle did not get to the tunnel run",
		},
		{
			name: "leader.json cannot be used this cycle",
			close: func(e *legEnv) {
				require.NoError(t, e.store.SaveWriter(planner.Writer{InstallID: "other", Generation: 1, Nonce: "n1"}))
			},
			why: "the writer identity could not be read this cycle",
		},
		{
			name: "leader.json changed between the read and the write",
			close: func(e *legEnv) {
				var once sync.Once
				e.res.hook(func() {
					once.Do(func() {
						require.NoError(t, e.store.SaveWriter(planner.Writer{InstallID: testInstall, Generation: 2, Nonce: "n2"}))
					})
				})
			},
			why: "leader.json changed since the cycle began",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newLegEnv(t)
			e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), guest(102, "api", "api.example.com -> :8080")))
			e.res.moveTo("api.example.com", netip.MustParseAddr("10.0.0.12"))
			e.warm()
			tc.close(e)

			st := e.cycle()

			r := route(st, legHost)
			require.Equal(t, planner.StateUnreachable, r.State)
			require.Equal(t, "service address not allocated: "+tc.why, r.Reason)
			require.Contains(t, st.Problems, "hostname www.example.com is not served through network vmbr1: service address not allocated: "+tc.why)
			require.Empty(t, e.mappings())
			require.Equal(t, "http://10.0.0.12:8080", route(st, "api.example.com").Service, "the direct route is planned as ever")
			e.requireNeverSet(legAddr)
			requireOnlyServiceAddresses(t, st, nil, legHost)
		})
	}
}

// gateIdentity is the self-identification of an appliance as a test sets it.
type gateIdentity struct {
	mu  sync.Mutex
	why string // empty: proven
}

func (g *gateIdentity) Check(context.Context, inventory.Snapshot) appliance.Verdict {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.why == "" {
		return appliance.Verdict{OK: true}
	}
	return appliance.Verdict{Why: g.why}
}

func (g *gateIdentity) SetFlag(bool) error { return nil }

func (g *gateIdentity) fail(why string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.why = why
}

// quorum answers the reads of the quorum with what a test queued, and true
// once nothing is queued.
type quorum struct {
	mu      sync.Mutex
	answers []bool
}

func (q *quorum) read(context.Context) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.answers) == 0 {
		return true, nil
	}
	a := q.answers[0]
	q.answers = q.answers[1:]
	return a, nil
}

func (q *quorum) queue(answers ...bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.answers = append(q.answers, answers...)
}

// applianceLegEnv is a legEnv of an appliance whose identity, quorum and
// epoch a test sets.
type applianceLegEnv struct {
	*legEnv
	ident  *gateIdentity
	quorum *quorum
	drawn  atomic.Bool
}

func newApplianceLegEnv(t *testing.T) *applianceLegEnv {
	t.Helper()
	e := newLegEnv(t)
	require.NoError(t, e.store.SaveInstall(store.Install{ID: testInstall, CreatedAt: t0, Profile: store.ProfileAppliance, Appliance: &store.ApplianceInstall{
		VMID: ownVMID, Node: testNode, MACs: []string{ownMAC},
		Endpoints: []store.Endpoint{{Address: "10.92.0.1:8006", ServerName: testNode}},
	}}))
	a := &applianceLegEnv{legEnv: e, ident: &gateIdentity{}, quorum: &quorum{}}
	e.identity, e.quorate, e.epochDrawn = a.ident, a.quorum.read, a.drawn.Load
	e.eng = e.newEngine()
	return a
}

func TestAClosedGateHoldsANewServiceAddressInTheAppliance(t *testing.T) {
	tests := []struct {
		name  string
		close func(a *applianceLegEnv)
		why   string
	}{
		{
			name:  "the self-identification did not pass",
			close: func(a *applianceLegEnv) { a.ident.fail(appliance.WhyIncomplete) },
			why:   "the self-identification of the appliance did not pass this cycle",
		},
		{
			name:  "the cluster lost its quorum since the cycle began",
			close: func(a *applianceLegEnv) { a.quorum.queue(true, false) },
			why:   "the cluster is not quorate",
		},
		{
			name:  "this process drew a new epoch",
			close: func(a *applianceLegEnv) { a.drawn.Store(true) },
			why:   "this process drew a new epoch and none of its writes is verified at Cloudflare yet",
		},
		{
			name:  "the previous cycle found the appliance behind",
			close: func(a *applianceLegEnv) { a.eng.lastVerdict = VerdictBehind },
			why:   "the previous cycle found this appliance behind its last write at Cloudflare",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newApplianceLegEnv(t)
			st := a.warm()
			require.True(t, st.Identity.OK, st.Identity.Why)
			tc.close(a)

			st = a.cycle()

			require.Equal(t, "service address not allocated: "+tc.why, route(st, legHost).Reason)
			require.Empty(t, a.mappings())
			a.requireNeverSet(legAddr)
		})
	}

	t.Run("and opens when nothing closes it", func(t *testing.T) {
		a := newApplianceLegEnv(t)
		a.warm()

		st := a.cycle()

		require.Equal(t, "http://198.18.1.0:8080", route(st, legHost).Service)
	})
}

// The cycle brings the gateway back once the self-identification of the
// appliance passed, before anything is resolved through it: the start
// withholds that, since a copy would add the original's cards to itself, and
// the container that rebooted has its cards bare until then.
func TestTheCycleBringsTheGatewayBackOnceTheApplianceProvedItself(t *testing.T) {
	a := newApplianceLegEnv(t)
	var resolvedAtCheck []int
	a.gw.onCheck(func() GatewayCheck {
		resolvedAtCheck = append(resolvedAtCheck, len(a.res.resolved()))
		return GatewayCheck{Restored: "leg1 is down"}
	})

	st := a.warm()

	require.True(t, st.Identity.OK, st.Identity.Why)
	require.Equal(t, []int{0}, resolvedAtCheck, "once, before the resolution")
	require.Contains(t, st.Problems, problemGatewayRestored+" (leg1 is down)")

	a.ident.fail(appliance.WhyIncomplete)
	a.cycle()
	require.Equal(t, []int{0}, resolvedAtCheck, "not while the self-identification did not pass")

	a.ident.fail("this container is not lxc/9250")
	a.cycle()
	require.Equal(t, []int{0}, resolvedAtCheck)

	a.ident.fail("")
	a.cycle()
	require.Len(t, resolvedAtCheck, 2, "and again once it passes")
}

// The host's cycle checks the gateway too; what could not be brought back
// stands as one problem line for as long as that lasts.
func TestTheCycleSaysWhenTheGatewayCouldNotBeBroughtBack(t *testing.T) {
	e := newLegEnv(t)
	e.gw.onCheck(func() GatewayCheck { return GatewayCheck{NotKept: "the gateway namespace is not pinned"} })
	line := fmt.Sprintf(problemGatewayNotKept, "the gateway namespace is not pinned")

	st := e.warm()
	require.Equal(t, 1, count(st.Problems, line), st.Problems)

	st = e.next()
	require.Equal(t, 1, count(st.Problems, line), st.Problems)

	e.gw.onCheck(nil)
	st = e.next()
	require.NotContains(t, st.Problems, line)
}

func count(lines []string, line string) int {
	n := 0
	for _, l := range lines {
		if l == line {
			n++
		}
	}
	return n
}

// Outside a cycle the gateway of an appliance is changed only while the
// appliance proved itself and serves: a copy never adds a card of the
// original to itself.
func TestTheGatewayIsChangedOutsideACycleOnlyByAProvenAppliance(t *testing.T) {
	require.True(t, newLegEnv(t).eng.GatewayAllowed(), "the host")

	a := newApplianceLegEnv(t)
	require.False(t, a.eng.GatewayAllowed(), "before the first self-identification")
	a.warm()
	require.True(t, a.eng.GatewayAllowed())
	a.ident.fail("this container is not lxc/9250")
	a.cycle()
	require.False(t, a.eng.GatewayAllowed())
}

// After a restart the previous verdict is not known, which closes the gate:
// the targets whose service addresses exist find them all the same, and the
// first cycle writes nothing.
func TestARestartWithExistingServiceAddressesChangesNothing(t *testing.T) {
	e := newLegEnv(t)
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), guest(102, "db-1", "db.example.com -> :5432")))
	e.res.goVia("db.example.com", "vmbr2")
	e.res.moveTo("db.example.com", legAddr)
	e.warm()
	e.next()
	rules := e.rules()
	mappings := e.mappings()
	next, used, _, err := e.store.ServicePool()
	require.NoError(t, err)
	writes := len(e.writes())

	e.eng = e.newEngine()
	st := e.cycle()

	require.Empty(t, st.Problems)
	require.Len(t, e.writes(), writes, "no PUT")
	require.Equal(t, rules, e.rules())
	require.Equal(t, mappings, e.mappings())
	again, usedAgain, _, err := e.store.ServicePool()
	require.NoError(t, err)
	require.Equal(t, next, again, "nothing allocated")
	require.Equal(t, used, usedAgain)
	require.Equal(t, planner.StateActive, route(st, legHost).State)
	require.Equal(t, planner.StateActive, route(st, "db.example.com").State)
	got, _ := e.egr.last()
	require.Len(t, got, 2)
	e.requireNeverSet(legAddr)
}

// A target on a leg that moves to another address while the gate is closed
// keeps the rule last verified for it, and the state says why; a new target
// is held. No rule sends a connector to a real address.
func TestAClosedGateNeverYieldsARuleWithARealAddress(t *testing.T) {
	e := newLegEnv(t)
	e.warm()
	e.next()
	e.eng.lastVerdict = VerdictStale
	e.res.moveTo(legHost, netip.MustParseAddr("10.50.0.11"))
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), guest(102, "db-1", "db.example.com -> :5432")))
	e.res.goVia("db.example.com", "vmbr2")
	e.res.moveTo("db.example.com", netip.MustParseAddr("10.50.0.20"))

	st := e.cycle()

	r := route(st, legHost)
	require.Equal(t, planner.StateUnreachable, r.State)
	require.Equal(t, "http://198.18.1.0:8080", r.Service)
	require.Equal(t, "service address not moved: the previous cycle found a stale writer; the rule is kept and reaches nothing until it is", r.Reason)
	require.Contains(t, st.Problems, "hostname www.example.com moved to 10.50.0.11 on network vmbr1; its rule is kept, "+
		"and its service address 198.18.1.0 forwards nothing until it can follow: the previous cycle found a stale writer")
	require.Equal(t, "service address not allocated: the previous cycle found a stale writer", route(st, "db.example.com").Reason)
	require.Equal(t, map[string]netip.Addr{"vmbr1 10.50.0.10": svc0}, e.mappings())
	requireOnlyServiceAddresses(t, st, e.rules(), legHost, "db.example.com")
	e.requireNeverSet(netip.MustParseAddr("10.50.0.11"))
	e.requireNeverSet(netip.MustParseAddr("10.50.0.20"))
	require.Contains(t, e.rules(), hostRuleAt(legHost, "http://198.18.1.0:8080"), "the rule of www.example.com did not change")
	require.Contains(t, e.rules(), planner.IngressRule{Hostname: "db.example.com", Service: planner.BlockedService})
}

// web-1 leaves 10.50.0.10 for 10.50.0.11 while the gate is closed, and a
// stranger takes 10.50.0.10. The rule of www.example.com stays as it is, so
// that nothing is written at Cloudflare, but its service address forwards
// nothing from the cycle that saw the move; once the gate is open it follows
// web-1 to its new address, still without a write.
func TestAStrangerOnTheVacatedAddressIsNeverForwardedTo(t *testing.T) {
	e := newLegEnv(t)
	e.warm()
	e.next()
	writes := len(e.writes())
	removes := len(e.egr.removes())
	synced := e.gw.syncCount()
	e.eng.lastVerdict = VerdictStale
	e.res.moveTo(legHost, netip.MustParseAddr("10.50.0.11"))

	e.next()

	require.Empty(t, e.gw.last(t).services, "no DNAT to the address web-1 left")
	got, _ := e.egr.last()
	require.Empty(t, got)
	require.Equal(t, []netip.Addr{svc0}, e.egr.removes()[removes:], "out of the filter at once")
	require.Len(t, e.writes(), writes, "the rule stays as it is")

	st := e.next()

	require.Equal(t, []gateway.Service{{Leg: "vmbr1", Service: svc0, Addr: netip.MustParseAddr("10.50.0.11"), Ports: []uint16{8080}}},
		e.gw.last(t).services, "it follows web-1")
	require.Equal(t, map[string]netip.Addr{"vmbr1 10.50.0.11": svc0}, e.mappings())
	got, _ = e.egr.last()
	require.Equal(t, egressTargets("198.18.1.0:8080"), got)
	require.Len(t, e.writes(), writes, "nothing was written at Cloudflare")
	require.Equal(t, planner.StateActive, route(st, legHost).State)
	require.Contains(t, unnumbered(e.eng.Events(time.Time{})), Event{At: st.At, Level: levelInfo, Kind: kindNetwork, Subject: "vmbr1", Route: legHost,
		Message: "service address 198.18.1.0 moved from 10.50.0.10 on network vmbr1 to 10.50.0.11 on network vmbr1, with www.example.com"})
	for _, s := range e.gw.syncs[synced:] {
		for _, svc := range s.services {
			if svc.Service == svc0 {
				require.NotEqual(t, legAddr, svc.Addr, "after the move was seen, nothing forwards to the address left")
			}
		}
	}
}

// The stranger on the address web-1 left has routes of its own: they wait
// while the service address of that address waits for web-1, and get their
// own once it followed web-1.
func TestAStrangerWithARouteWaitsForTheServiceAddressToFollow(t *testing.T) {
	e := newLegEnv(t)
	e.warm()
	e.next()
	e.eng.lastVerdict = VerdictStale
	e.res.moveTo(legHost, netip.MustParseAddr("10.50.0.11"))
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), guest(102, "x", "x.example.com -> :8080")))
	e.res.goVia("x.example.com", "vmbr1")
	e.res.moveTo("x.example.com", legAddr)
	e.res.answerFrom("x.example.com", dbMAC)

	st := e.next()

	require.Equal(t, "service address not allocated: service address 198.18.1.0 of this address waits for the route that left it",
		route(st, "x.example.com").Reason)
	require.Empty(t, e.gw.last(t).services)

	st = e.next()

	require.Equal(t, "http://198.18.1.1:8080", route(st, "x.example.com").Service, "a service address of its own")
	require.Equal(t, "http://198.18.1.0:8080", route(st, legHost).Service)
	require.Equal(t, []gateway.Service{
		{Leg: "vmbr1", Service: svc1, Addr: legAddr, Ports: []uint16{8080}},
		{Leg: "vmbr1", Service: svc0, Addr: netip.MustParseAddr("10.50.0.11"), Ports: []uint16{8080}},
	}, e.gw.last(t).services)
}

// A guest renumbered while the gate is open takes its service address along
// in the same cycle: no write at Cloudflare, nothing forwards to the address
// it left.
func TestARouteThatMovesTakesItsServiceAddressAlong(t *testing.T) {
	e := newLegEnv(t)
	e.warm()
	e.next()
	writes := len(e.writes())
	e.res.moveTo(legHost, netip.MustParseAddr("10.50.0.11"))

	st := e.next()

	require.Equal(t, planner.StateActive, route(st, legHost).State)
	require.Equal(t, []gateway.Service{{Leg: "vmbr1", Service: svc0, Addr: netip.MustParseAddr("10.50.0.11"), Ports: []uint16{8080}}},
		e.gw.last(t).services)
	require.Equal(t, []gateway.Pin{{Leg: "vmbr1", Addr: netip.MustParseAddr("10.50.0.11"), MAC: hardware(t, testMAC)}}, e.gw.last(t).pins)
	require.Len(t, e.writes(), writes)
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Equal(t, svc0, bindings[legHost].Service)

	t.Run("to another network", func(t *testing.T) {
		e.res.goVia(legHost, "vmbr2")
		e.next()

		require.Equal(t, []gateway.Service{{Leg: "vmbr2", Service: svc0, Addr: netip.MustParseAddr("10.50.0.11"), Ports: []uint16{8080}}},
			e.gw.last(t).services)
		require.Len(t, e.writes(), writes)
	})
}

// A route that moves to an address with a service address of its own, or off
// the legs, cannot take its service address along: the old one is released
// at once, and the route gets the one of its new address.
func TestAServiceAddressNoRouteCanTakeAlongIsReleasedAtOnce(t *testing.T) {
	e := newLegEnv(t)
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), guest(102, "db-1", "db.example.com -> :5432")))
	e.res.goVia("db.example.com", "vmbr1")
	e.res.moveTo("db.example.com", netip.MustParseAddr("10.50.0.11"))
	e.res.answerFrom("db.example.com", dbMAC)
	e.warm()
	e.next()
	m := e.mappings()
	web, db := m["vmbr1 10.50.0.10"], m["vmbr1 10.50.0.11"]
	e.res.moveTo(legHost, netip.MustParseAddr("10.50.0.11"))

	st := e.next()

	require.Equal(t, map[string]netip.Addr{"vmbr1 10.50.0.11": db}, e.mappings(), "released at once")
	require.Equal(t, "http://"+netip.AddrPortFrom(db, 8080).String(), route(st, legHost).Service)
	for _, svc := range e.gw.last(t).services {
		require.NotEqual(t, web, svc.Service)
	}

	e.res.goVia(legHost, "")
	e.res.moveTo(legHost, guestAddr)
	e.next()
	require.Equal(t, "http://10.0.0.11:8080", route(e.eng.State(), legHost).Service)
}

// The service addresses of a network with no leg on this node are another
// node's: none is released here, and none is given for it.
func TestTheServiceAddressesOfAnotherNodesNetworkAreLeftAlone(t *testing.T) {
	e := newLegEnv(t)
	require.NoError(t, e.store.SaveNetwork(store.Network{
		ID: "vmbr9", Kind: store.NetworkAttach, Bridge: "vmbr9", Prefix: netip.MustParsePrefix("10.90.0.0/24"),
		Legs: map[string]netip.Addr{"pve2": netip.MustParseAddr("10.90.0.250")}, LegMAC: store.LegMACFor(store.NetworkAttach, 9), Mark: 9, CreatedAt: t0,
	}, 0))
	other, err := e.store.AllocateServiceAddr("vmbr9", netip.MustParseAddr("10.90.0.10"), t0)
	require.NoError(t, err)
	e.res.goVia(legHost, "vmbr9")
	e.res.moveTo(legHost, netip.MustParseAddr("10.90.0.11"))
	e.warm()

	st := e.next()

	require.Equal(t, "service address not allocated: network vmbr9 has no leg on this node", route(st, legHost).Reason)
	require.Equal(t, map[string]netip.Addr{"vmbr9 10.90.0.10": other.Service}, e.mappings())
	require.Equal(t, []gateway.Leg{legOf(t, "vmbr1", 1), legOf(t, "vmbr2", 2)}, e.gw.last(t).legs)
}

func TestAnExhaustedPoolHoldsOnlyTheNewTarget(t *testing.T) {
	e := newLegEnv(t)
	e.warm()
	e.next()
	pool := `{"schemaVersion": 1, "rev": 99, "id": "svcpool", "data": {"next": "198.19.0.0"}}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(e.paths.Cluster, "meta", "svcpool.json"), []byte(pool), 0o600))
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), guest(102, "db-1", "db.example.com -> :5432")))
	e.res.goVia("db.example.com", "vmbr2")

	st := e.cycle()

	why := "service address not allocated: every service address was given out: every address from 198.18.1.0 to 198.18.255.255 was given out once"
	require.Equal(t, why, route(st, "db.example.com").Reason)
	require.True(t, hasProblem(st, "hostname db.example.com is not served through network vmbr2: "+why), st.Problems)
	require.Equal(t, planner.StateActive, route(st, legHost).State)
	require.Equal(t, "http://198.18.1.0:8080", route(st, legHost).Service)
}

// Two networks with the same subnet have a guest each at the same address,
// with its own MAC: each is pinned by its service address, with the leg it
// answers on, and a move of one re-verifies that one and takes that address
// out, and no other.
func TestTwoGuestsWithOneAddressOnTwoLegsArePinnedApart(t *testing.T) {
	e := newLegEnv(t)
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), guest(102, "db-1", "db.example.com -> :5432")))
	e.res.goVia("db.example.com", "vmbr2")
	e.res.moveTo("db.example.com", legAddr)
	e.res.answerFrom("db.example.com", dbMAC)
	e.warm()
	e.next()
	m := e.mappings()
	web, db := m["vmbr1 10.50.0.10"], m["vmbr2 10.50.0.10"]

	require.Equal(t, map[netip.Addr]egress.Pin{
		web: {MAC: hardware(t, testMAC), Dev: "leg1", Real: legAddr},
		db:  {MAC: hardware(t, dbMAC), Dev: "leg2", Real: legAddr},
	}, e.eng.Bound())

	tm := &timers{}
	tm.install(e.eng)
	resolved := len(e.res.resolved())
	e.eng.Moved(t.Context(), web)
	e.eng.moving.Wait()

	require.Equal(t, []string{legHost}, e.res.resolved()[resolved:], "only the route on the address that moved is verified again")
	require.Equal(t, []netip.Addr{web}, e.egr.removes())
	got, _ := e.egr.last()
	require.ElementsMatch(t, []egress.Target{{Addr: web, Port: 8080}, {Addr: db, Port: 5432}}, got, "back once verified")
	require.Equal(t, hardware(t, dbMAC), e.eng.Bound()[db].MAC, "the other pin is untouched")
}

func TestAMovedLegTargetThatFailsStaysOut(t *testing.T) {
	e := newLegEnv(t)
	e.warm()
	e.next()
	tm := &timers{}
	tm.install(e.eng)
	e.res.stop(legHost, "10.50.0.10 answered by bc:24:11:ff:ff:01, which is not this guest")

	e.eng.Moved(t.Context(), svc0)
	e.eng.moving.Wait()

	require.NotContains(t, e.eng.Bound(), svc0)
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.True(t, bindings[legHost].Withdrawn)
	require.Equal(t, svc0, bindings[legHost].Service, "the next cycle takes it out by the address the set holds")
}

// The watch sees web-1's MAC move, and the verification that follows finds
// web-1 at another address: its binding keeps the service address it was
// published under, so that the next cycle has the service address follow it
// there, without a write at Cloudflare.
func TestAMoveTheWatchFindsIsFollowedByTheServiceAddress(t *testing.T) {
	e := newLegEnv(t)
	e.warm()
	e.next()
	writes := len(e.writes())
	tm := &timers{}
	tm.install(e.eng)
	e.res.moveTo(legHost, netip.MustParseAddr("10.50.0.11"))

	e.eng.Moved(t.Context(), svc0)
	e.eng.moving.Wait()

	require.NotContains(t, e.eng.Bound(), svc0, "out until the next cycle")
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Equal(t, netip.MustParseAddr("10.50.0.11"), bindings[legHost].Addr)
	require.Equal(t, svc0, bindings[legHost].Service)

	st := e.next()

	require.Equal(t, planner.StateActive, route(st, legHost).State)
	require.Equal(t, map[string]netip.Addr{"vmbr1 10.50.0.11": svc0}, e.mappings())
	require.Equal(t, []gateway.Service{{Leg: "vmbr1", Service: svc0, Addr: netip.MustParseAddr("10.50.0.11"), Ports: []uint16{8080}}},
		e.gw.last(t).services)
	require.Len(t, e.writes(), writes, "nothing was written at Cloudflare")
}

// A proof the watch vouches for is keyed by the address the set holds: two
// guests at one real address on two legs are vouched for apart, and one never
// published through its leg is not watched at all.
func TestTheWatchVouchesForATargetOnALegByItsServiceAddress(t *testing.T) {
	e := newLegEnv(t)
	e.warm()
	e.next()

	e.eng.vouch.mu.Lock()
	_, pinned := e.eng.vouch.pinned[svc0]
	_, real := e.eng.vouch.pinned[legAddr]
	e.eng.vouch.mu.Unlock()
	require.True(t, pinned)
	require.False(t, real, "the real address is not what the watch watches")

	for _, tc := range []struct {
		name string
		b    resolve.Binding
		want netip.Addr
	}{
		{"a direct target", resolve.Binding{Addr: legAddr}, legAddr},
		{"a target on the first leg", resolve.Binding{Addr: legAddr, Via: "vmbr1", Service: svc0}, svc0},
		{"another at the same address on the second", resolve.Binding{Addr: legAddr, Via: "vmbr2", Service: svc1}, svc1},
		{"a target on a leg never published", resolve.Binding{Addr: legAddr, Via: "vmbr1"}, netip.Addr{}},
	} {
		require.Equal(t, tc.want, setAddr(tc.b), tc.name)
	}
}

func TestWithoutAGatewayARouteOnALegIsNotServed(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	saveNetwork(t, e.store, "vmbr1", 1)
	e.res.goVia(legHost, "vmbr1")
	e.res.moveTo(legHost, legAddr)

	e.cycle()
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	r := route(st, legHost)
	require.Equal(t, planner.StateUnreachable, r.State)
	require.Equal(t, "service address not allocated: no gateway runs on this node", r.Reason)
	for _, set := range e.egr.sets {
		require.NotContains(t, set, egress.Target{Addr: legAddr, Port: 8080})
	}
}

// What the keeper found between cycles is said by the next cycle, whose own
// check of the gateway finds the same.
func TestTheGatewayKeeperIsHeard(t *testing.T) {
	e := newLegEnv(t)

	e.eng.NoteGateway(GatewayCheck{Restored: "leg1 is down"})
	st := e.next()

	require.Contains(t, st.Problems, "the gateway namespace was changed outside pco and was brought back (leg1 is down)")
	events := unnumbered(e.eng.Events(time.Time{}))
	require.Contains(t, events, Event{At: t0, Level: levelWarn, Kind: kindNetwork, Subject: gateway.Namespace,
		Message: "the gateway namespace was changed outside pco and was brought back"})
	require.NotContains(t, e.next().Problems, "the gateway namespace was changed outside pco and was brought back (leg1 is down)", "said once")

	gone := "the gateway namespace is not pinned; systemctl restart pco-netns"
	e.gw.onCheck(func() GatewayCheck { return GatewayCheck{NotKept: gone} })
	e.eng.NoteGateway(GatewayCheck{NotKept: gone})
	e.eng.NoteGateway(GatewayCheck{NotKept: gone})
	for range 2 {
		require.Contains(t, e.next().Problems, "the gateway namespace was changed outside pco and could not be brought back: "+gone)
	}
	notKept := 0
	for _, ev := range e.eng.Events(time.Time{}) {
		if ev.Kind == kindNetwork && ev.Level == levelError {
			notKept++
		}
	}
	require.Equal(t, 1, notKept, "an event when it starts")

	e.gw.onCheck(nil)
	e.eng.NoteGateway(GatewayCheck{})
	require.NotContains(t, e.next().Problems, "the gateway namespace was changed outside pco and could not be brought back: "+gone)

	e.eng.NoteGateway(GatewayCheck{Failed: "listing the table: nft: no such file"})
	require.Contains(t, e.next().Problems, "checking the gateway namespace: listing the table: nft: no such file")
}
