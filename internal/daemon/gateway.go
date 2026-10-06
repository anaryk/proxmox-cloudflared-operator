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

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
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
}

func newGatewayPart(b gateway.Backend, local string, now func() time.Time, log zerolog.Logger) *gatewayPart {
	return &gatewayPart{b: b, local: local, now: now, log: log}
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
		g.log.Warn().Str("found", err.Error()).Msg("the gateway is not as its cache says; the first cycle that may change it syncs it")
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
	states, err := g.b.Sync(ctx, want.Legs, want.Services, want.Pins, want.Sticky)
	if err != nil {
		return nil, err
	}
	g.setUp(upOf(want.Legs, states))
	return states, nil
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
	var rerr error
	allowed := true
	if ierr := k.idle(ctx, func() {
		if allowed = k.gw.mayChange(); allowed {
			rerr = k.gw.restore(ctx)
		}
	}); ierr != nil || !allowed {
		return 0
	}
	what := err.Error()
	var changed *gateway.ChangedError
	if errors.As(err, &changed) {
		what = changed.Error()
	}
	if rerr != nil {
		k.log.Error().Err(rerr).Str("found", what).Msg("the gateway namespace was changed outside pco and could not be brought back")
		k.noteGw(engine.GatewayCheck{NotKept: rerr.Error()})
		return 0
	}
	k.log.Warn().Str("found", what).Msg("the gateway namespace was changed outside pco and was brought back")
	k.noteGw(engine.GatewayCheck{Restored: what})
	return 0
}
