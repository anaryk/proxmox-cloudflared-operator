package daemon

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/vishvananda/netns"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// startVerifyTimeout bounds the check of the gateway at the start of the
// daemon.
const startVerifyTimeout = 30 * time.Second

// gatewayPart is the gateway of the networks reached through a leg as the
// daemon runs it: the backend, what was last wanted of it, which the keeper
// brings it back to, and the legs that are up, which the prober lists. The
// engine syncs it in every cycle; the keeper checks it between cycles.
type gatewayPart struct {
	b     gateway.Backend
	local string
	now   func() time.Time
	log   zerolog.Logger
	// allowed says whether the gateway may be changed outside a cycle; nil
	// is always. The appliance's says no until it proved itself, so that a
	// copy never adds the original's cards to itself.
	allowed func() bool

	// mu gives the backend's calls their turns and guards wanted, what the
	// last Sync was given or the cache before the first; upMu guards up, the
	// legs the last Sync reported up, which the prober reads while a check of
	// the backend runs.
	mu     sync.Mutex
	wanted gateway.Cache
	upMu   sync.Mutex
	up     []gateway.Leg

	// portExists says whether a link of the node is there; applySticky
	// gives the backend the sticky entries between two syncs. present are
	// the ports of the wanted entries that were there when the entries were
	// last given, guarded by mu.
	portExists  func(name string) bool
	applySticky func(ctx context.Context, b gateway.Backend, sticky []gateway.Sticky) error
	present     map[string]bool
}

func newGatewayPart(b gateway.Backend, local string, now func() time.Time, log zerolog.Logger) *gatewayPart {
	return &gatewayPart{b: b, local: local, now: now, log: log, portExists: linkExists, applySticky: gateway.ApplySticky}
}

// linkExists says whether the node has a link of the name.
func linkExists(name string) bool {
	_, err := net.InterfaceByName(name)
	return err == nil
}

// start takes the cache of the gateway, which the last sync wrote and which
// pco netns up restores the host's namespace from at boot, as what the
// gateway holds, so that a keeper that brings it back before the first cycle
// loads the table with its elements, never with empty maps. A gateway that
// is not as the cache says is restored from it, as pco netns up does, with
// pins only while they are fresh. The legs then up are the ones the first
// cycle probes through, before it syncs the gateway itself; a gateway that
// cannot be checked is taken as its cache says. A node without a cache never
// had a network.
func (g *gatewayPart) start(ctx context.Context) {
	c, found, err := gateway.ReadCache(g.local)
	switch {
	case err != nil:
		g.log.Warn().Err(err).Msg("reading the cache of the gateway failed; the first cycle syncs the gateway anew")
		return
	case !found:
		return
	}
	gateway.Expect(g.b, c)
	g.mu.Lock()
	defer g.mu.Unlock()
	if !c.Fresh(g.now()) {
		c.Pins = nil
	}
	g.wanted = c
	ctx, cancel := context.WithTimeout(ctx, startVerifyTimeout)
	defer cancel()
	err = g.b.Verify(ctx)
	switch {
	case err == nil:
		g.setUp(slices.Clone(c.Legs))
		return
	case !errors.Is(err, gateway.ErrChanged):
		// Probing through no leg would withdraw every route behind one
		// for a cycle; the first cycle's Sync corrects what is not so.
		g.log.Warn().Err(err).Msg("checking the gateway at the start failed; its legs are taken as its cache says until the first cycle syncs it")
		g.setUp(slices.Clone(c.Legs))
		return
	}
	if !g.mayChange() {
		g.log.Warn().Str("found", err.Error()).Msg("the gateway is not as its cache says; the first cycle that may change it brings it back")
		g.setUp(slices.Clone(c.Legs))
		return
	}
	states, rerr := gateway.Restore(ctx, g.b, c, g.now())
	if rerr != nil {
		g.log.Error().Err(rerr).Str("found", err.Error()).Msg("the gateway is not as its cache says and could not be restored from it")
		return
	}
	g.log.Warn().Str("found", err.Error()).Msg("the gateway was not as its cache says and was restored from it")
	g.setUp(upOf(c.Legs, states))
}

// mayChange says whether the gateway may be changed outside a cycle now.
func (g *gatewayPart) mayChange() bool { return g.allowed == nil || g.allowed() }

// upOf are the legs states says are up.
func upOf(legs []gateway.Leg, states []gateway.LegState) []gateway.Leg {
	var out []gateway.Leg
	for _, l := range legs {
		if slices.ContainsFunc(states, func(s gateway.LegState) bool { return s.ID == l.ID && s.Up }) {
			out = append(out, l)
		}
	}
	return out
}

// UpLegs returns the legs that are up as the last Sync reported them, for
// the prober. It is safe for concurrent use; the list changes only while no
// cycle runs, or in a cycle after its resolution.
func (g *gatewayPart) UpLegs() []gateway.Leg {
	g.upMu.Lock()
	defer g.upMu.Unlock()
	return slices.Clone(g.up)
}

func (g *gatewayPart) setUp(legs []gateway.Leg) {
	g.upMu.Lock()
	defer g.upMu.Unlock()
	g.up = legs
}

// active says that something was wanted of the gateway: a node that never
// had a network has nothing there to keep.
func (g *gatewayPart) active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.wanted.Legs) > 0 || len(g.wanted.Services) > 0
}

func (g *gatewayPart) wantedNow() gateway.Cache {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.wanted
}

// Sync syncs the backend and keeps what it was given, and the legs it says
// are up. A sync that fails says nothing of the legs.
func (g *gatewayPart) Sync(ctx context.Context, legs []gateway.Leg, services []gateway.Service, pins []gateway.Pin, sticky []gateway.Sticky) ([]gateway.LegState, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sync(ctx, gateway.Cache{Legs: slices.Clone(legs), Services: slices.Clone(services), Pins: slices.Clone(pins), Sticky: slices.Clone(sticky)})
}

func (g *gatewayPart) sync(ctx context.Context, want gateway.Cache) ([]gateway.LegState, error) {
	g.wanted = want
	present := g.stickyPorts()
	states, err := g.b.Sync(ctx, want.Legs, want.Services, want.Pins, want.Sticky)
	if err != nil {
		return nil, err
	}
	g.present = present
	g.setUp(upOf(want.Legs, states))
	return states, nil
}

// stickyPorts are the ports of the wanted sticky entries that are there now.
func (g *gatewayPart) stickyPorts() map[string]bool {
	out := map[string]bool{}
	for _, s := range g.wanted.Sticky {
		if s.Port != "" && g.portExists(s.Port) {
			out[s.Port] = true
		}
	}
	return out
}

// keepSticky gives the wanted sticky entries to the ports of managed guests
// that appeared since the entries were last given: joined, the ports the port
// watch saw join the managed bridge, and any other the keeper's check finds
// there now. A guest that starts makes its port anew, without the entry its
// card had; until it is given again, another guest may take the card's MAC
// over. The rest of the gateway is left to the cycle.
func (g *gatewayPart) keepSticky(ctx context.Context, joined ...string) error {
	if !g.mayChange() {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.wanted.Sticky) == 0 {
		return nil
	}
	now := g.stickyPorts()
	appeared := false
	for port := range now {
		if !g.present[port] || slices.Contains(joined, port) {
			appeared = true
		}
	}
	if !appeared {
		g.present = now
		return nil
	}
	if err := g.applySticky(ctx, g.b, g.wanted.Sticky); err != nil {
		return err
	}
	g.present = now
	return nil
}

// portJoinedQueue is how many ports that joined the managed bridge wait for
// their entries; more at once are left to the keeper's next check.
const portJoinedQueue = 64

// watchPorts gives the port of a managed guest its sticky entry as soon as
// it joins the managed bridge, until ctx ends. A watch that fails is started
// again after a while; until then the keeper's checks give the entries.
func (g *gatewayPart) watchPorts(ctx context.Context, watch func(ctx context.Context, ns netns.NsHandle, bridge string, joined func(port string)) error,
	sleep func(context.Context, time.Duration) error,
) {
	joined := make(chan string, portJoinedQueue)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case port := <-joined:
				// The ports that joined meanwhile are given their entries
				// at once: a port whose link changes often costs one apply
				// per turn, not one per change.
				ports := drainPorts(joined, port)
				if err := g.keepSticky(ctx, ports...); err != nil && ctx.Err() == nil {
					g.log.Warn().Err(err).Strs("ports", ports).Msg("giving the ports of managed guests their sticky entries failed; the keeper tries again")
				}
			}
		}
	}()
	defer func() { <-done }()
	for {
		err := watch(ctx, netns.None(), gateway.ManagedBridge, func(port string) {
			select {
			case joined <- port:
			default:
			}
		})
		if ctx.Err() != nil {
			return
		}
		g.log.Warn().Err(err).Dur("again", watchAgainAfter).Msg("watching the ports of the managed bridge failed; " +
			"until it runs again, a guest's port gets its sticky entry at the keeper's check")
		if sleep(ctx, watchAgainAfter) != nil {
			return
		}
	}
}

// drainPorts returns first and the ports waiting in joined, once each; there
// are no more than the queue holds.
func drainPorts(joined <-chan string, first string) []string {
	ports := []string{first}
	for {
		select {
		case port := <-joined:
			if !slices.Contains(ports, port) {
				ports = append(ports, port)
			}
		default:
			return ports
		}
	}
}

// managedLeg returns the leg of the managed network, while it is up.
func (g *gatewayPart) managedLeg() (gateway.Leg, bool) {
	for _, l := range g.UpLegs() {
		if l.Managed {
			return l, true
		}
	}
	return gateway.Leg{}, false
}

// restore brings the gateway back to what was last wanted of it: Up, then
// the last Sync again.
func (g *gatewayPart) restore(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.b.Up(ctx); err != nil {
		return err
	}
	_, err := g.sync(ctx, g.wanted)
	return err
}

func (g *gatewayPart) Up(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.b.Up(ctx)
}

func (g *gatewayPart) Verify(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.b.Verify(ctx)
}

func (g *gatewayPart) Probe(ctx context.Context, bridge string, vlan int, addr netip.Addr) ([]net.HardwareAddr, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.b.Probe(ctx, bridge, vlan, addr)
}

// Detach removes a leg and its services, and takes them out of what is
// wanted and of the cache: the keeper must not bring the leg back, nor a boot,
// before the next cycle syncs the gateway without it.
func (g *gatewayPart) Detach(ctx context.Context, id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.b.Detach(ctx, id); err != nil {
		return err
	}
	w := g.wanted
	w.Legs = slices.DeleteFunc(slices.Clone(w.Legs), func(l gateway.Leg) bool { return l.ID == id })
	w.Services = slices.DeleteFunc(slices.Clone(w.Services), func(s gateway.Service) bool { return s.Leg == id })
	w.Pins = slices.DeleteFunc(slices.Clone(w.Pins), func(p gateway.Pin) bool { return p.Leg == id })
	g.wanted = w
	g.setUp(slices.DeleteFunc(g.UpLegs(), func(l gateway.Leg) bool { return l.ID == id }))
	if err := gateway.WriteCache(g.local, w); err != nil {
		g.log.Warn().Err(err).Str("network", id).Msg("the cache of the gateway still has the leg detached; the next cycle writes it")
	}
	return nil
}

// Dial needs no turn of its own: it opens a socket in the namespace.
func (g *gatewayPart) Dial(ctx context.Context, leg gateway.Leg, addr netip.AddrPort) (net.Conn, error) {
	return g.b.Dial(ctx, leg, addr)
}

// Check checks the gateway and brings it back to what was last wanted of
// it when it differs, for the cycle, which has the proof the start and the
// keeper wait for in the appliance. A gateway nothing was wanted of is left
// to the cycle's Sync.
func (g *gatewayPart) Check(ctx context.Context) engine.GatewayCheck {
	if !g.active() {
		return engine.GatewayCheck{}
	}
	ctx, cancel := context.WithTimeout(ctx, startVerifyTimeout)
	defer cancel()
	err := g.Verify(ctx)
	switch {
	case err == nil:
		return engine.GatewayCheck{}
	case !errors.Is(err, gateway.ErrChanged):
		g.log.Warn().Err(err).Msg("checking the gateway namespace failed")
		return engine.GatewayCheck{Failed: err.Error()}
	}
	return g.bringBack(ctx, err)
}

// bringBack brings the gateway back to what was last wanted of it after a
// check found it changed, and says what the check found.
func (g *gatewayPart) bringBack(ctx context.Context, found error) engine.GatewayCheck {
	what := found.Error()
	var changed *gateway.ChangedError
	if errors.As(found, &changed) {
		what = changed.Error()
	}
	if err := g.restore(ctx); err != nil {
		g.log.Error().Err(err).Str("found", what).Msg("the gateway namespace was changed outside pco and could not be brought back")
		return engine.GatewayCheck{NotKept: err.Error()}
	}
	g.log.Warn().Str("found", what).Msg("the gateway namespace was changed outside pco and was brought back")
	return engine.GatewayCheck{Restored: what}
}

var _ engine.Gateway = (*gatewayPart)(nil)

// checkGateway checks the gateway, once something was wanted of it, and
// brings it back to what was last wanted when it differs, no sooner than
// reloadEvery after it last did, while no cycle runs: nothing probes through
// a namespace that is made again. An appliance that has not proved itself
// leaves it to the cycle. It returns how soon it has to check again, when
// that is sooner than the next timed check, and zero otherwise.
func (k *keeper) checkGateway(ctx context.Context) time.Duration {
	if k.gw == nil || !k.gw.active() {
		return 0
	}
	// A guest's port that appeared since the last sync is no change of the
	// gateway: it is given its entry before the check.
	if err := k.gw.keepSticky(ctx); err != nil {
		k.log.Warn().Err(err).Msg("giving the ports of managed guests their sticky entries failed; the next check tries again")
	}
	err := k.gw.Verify(ctx)
	switch {
	case err == nil:
		k.noteGw(engine.GatewayCheck{})
		return 0
	case !errors.Is(err, gateway.ErrChanged):
		k.log.Warn().Err(err).Msg("checking the gateway namespace failed")
		k.noteGw(engine.GatewayCheck{Failed: err.Error()})
		return 0
	}
	if wait := k.lastGwRestore.Add(reloadEvery).Sub(k.now()); wait > 0 && !k.lastGwRestore.After(k.now()) {
		return wait
	}
	k.lastGwRestore = k.now()
	var found engine.GatewayCheck
	allowed := true
	if ierr := k.idle(ctx, func() {
		if allowed = k.gw.mayChange(); allowed {
			found = k.gw.bringBack(ctx, err)
		}
	}); ierr != nil || !allowed {
		return 0
	}
	k.noteGw(found)
	return 0
}

// Of the watch of the managed leg for cards no lease knows.
const (
	// legPollEvery is how often the watch looks for the managed leg while
	// it has none to listen on.
	legPollEvery = 5 * time.Second
	// reportEvery is how often at most the engine is told of one MAC, and
	// maxReported how many MACs it is told of apart within that time. Past
	// that the engine is still told, of one MAC per reportEvery: whatever a
	// guest sprays, the engine acts on an unknown card at least that often
	// while one speaks, and the watch keeps no more than maxReported MACs.
	reportEvery = time.Second
	maxReported = 32
	// droppedSaidEvery is how often the frames the watch did not pass on are
	// logged.
	droppedSaidEvery = time.Minute
	// leasesReadEvery is how often at most a frame of a MAC the watch does
	// not know has the leases read again: a card attached in this cycle
	// speaks before the gateway is synced with its lease, and a guest that
	// sends from ever new MACs reads the store no more often than this.
	leasesReadEvery = time.Second
)

// sourceWatch listens on the managed leg for frames of cards no lease knows,
// in both profiles, and tells the engine of each: it reads the guests of the
// node again, so that a card added outside pco is seen within a cycle rather
// than at the next full read, and the appliance drops filtered at once.
type sourceWatch struct {
	gw      *gatewayPart
	leases  func() (map[string]store.Lease, error)
	unknown func(mac net.HardwareAddr, at time.Time)
	run     func(ctx context.Context, w gateway.SourceWatch, ns netns.NsHandle, dev string) error
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
	log     zerolog.Logger

	mu     sync.Mutex
	known  map[string]bool // the MACs of the leases as last read
	read   bool            // the leases were read once
	readAt time.Time
	// reported are, by MAC, when the engine was last told of it; never more
	// than maxReported. overflowAt is when the engine was last told of a MAC
	// past those. dropped counts the frames not passed on since droppedAt,
	// when they were last logged.
	reported   map[string]time.Time
	overflowAt time.Time
	dropped    int
	droppedAt  time.Time
	failed     string // why the watch last failed, said once
}

// newSourceWatch is the watch of the managed leg of a daemon. It waits for
// the leg on the clock, apart from the daemon's sleep, which its start
// spends waiting for the cluster filesystem.
func newSourceWatch(gw *gatewayPart, st *store.Store, eng *engine.Engine, deps Deps, log zerolog.Logger) *sourceWatch {
	return &sourceWatch{gw: gw, leases: st.Leases, unknown: eng.UnknownMAC, run: deps.WatchSources, now: deps.Now, sleep: sleepContext, log: log}
}

// runSourceWatch is the watch on the managed leg as it runs on Linux.
func runSourceWatch(ctx context.Context, w gateway.SourceWatch, ns netns.NsHandle, dev string) error {
	return w.Run(ctx, ns, dev)
}

// keep runs the watch on the managed leg until ctx ends, and again on the
// leg there then whenever it ends: when the leg went away, or was made anew.
func (s *sourceWatch) keep(ctx context.Context) {
	for ctx.Err() == nil {
		if leg, ok := s.gw.managedLeg(); ok {
			w := gateway.SourceWatch{Known: s.isLease, Own: []net.HardwareAddr{leg.MAC}, Unknown: s.report}
			err := s.run(ctx, w, s.gw.b.Netns(), leg.Iface())
			if ctx.Err() != nil {
				return
			}
			s.fail(leg, err)
		}
		if s.sleep(ctx, legPollEvery) != nil {
			return
		}
	}
}

func (s *sourceWatch) fail(leg gateway.Leg, err error) {
	why := "it ended"
	if err != nil {
		why = err.Error()
	}
	level := zerolog.DebugLevel
	if why != s.failed {
		level = zerolog.WarnLevel
	}
	s.failed = why
	s.log.WithLevel(level).Str("leg", leg.Iface()).Str("why", why).Msg("watching the managed network for cards no lease knows stopped; it starts again")
}

// report tells the engine of a MAC no lease knows, which acts on it each
// time: once per MAC in reportEvery for maxReported MACs, and past those of
// one MAC in reportEvery, so that a spoof hidden in a flood of MACs is acted
// on all the same. The bound is checked before a MAC is taken in: a guest
// that sprays MACs grows neither the memory of the watch nor the work of the
// engine without end. The frames not passed on, each a repeat within a
// second of an action, are counted and logged.
func (s *sourceWatch) report(mac net.HardwareAddr) {
	key, now := mac.String(), s.now()
	s.mu.Lock()
	pass := s.admit(key, now)
	if !pass {
		s.dropped++
	}
	var dropped int
	if s.dropped > 0 && now.Sub(s.droppedAt) >= droppedSaidEvery {
		dropped, s.dropped, s.droppedAt = s.dropped, 0, now
	}
	s.mu.Unlock()
	if dropped > 0 {
		s.log.Warn().Int("frames", dropped).Msg("frames of cards no lease knows were seen on the managed network faster than " +
			"they are passed on; the engine is told at least once a second while one speaks")
	}
	if pass {
		s.unknown(mac, now)
	}
}

// admit says whether a report of key goes to the engine, and records it.
func (s *sourceWatch) admit(key string, now time.Time) bool {
	if s.reported == nil {
		s.reported = make(map[string]time.Time, maxReported)
	}
	at, seen := s.reported[key]
	switch {
	case seen && now.Sub(at) < reportEvery:
		return false
	case seen || len(s.reported) < maxReported:
		s.reported[key] = now
		return true
	}
	for other, at := range s.reported {
		if now.Sub(at) >= reportEvery {
			delete(s.reported, other)
		}
	}
	if len(s.reported) < maxReported {
		s.reported[key] = now
		return true
	}
	// Full: fail closed, the engine is told of this one when none of the
	// others past the bound was within reportEvery.
	if now.Sub(s.overflowAt) < reportEvery {
		return false
	}
	s.overflowAt = now
	return true
}

// isLease says whether a MAC is a lease's. A MAC it does not know has the
// leases read again, no more often than leasesReadEvery. While they were
// never read, every MAC counts as known: what cannot be told is no finding.
func (s *sourceWatch) isLease(mac net.HardwareAddr) bool {
	key := mac.String()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.known[key] {
		return true
	}
	now := s.now()
	if s.read && now.Sub(s.readAt) < leasesReadEvery {
		return false
	}
	s.readAt = now
	leases, err := s.leases()
	if err != nil {
		s.log.Debug().Err(err).Msg("reading the leases for the watch of the managed network failed")
		return !s.read
	}
	s.known = make(map[string]bool, len(leases))
	for _, l := range leases {
		if m, err := net.ParseMAC(l.MAC); err == nil {
			s.known[m.String()] = true
		}
	}
	s.read = true
	return s.known[key]
}
