package engine

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// reverifyEvery is the least time between the starts of two verifications of
// an address whose MAC moved, however often it moves.
const reverifyEvery = 5 * time.Second

// watched is what the last cycle that resolved the routes bound, for the
// watch of the network: the routes of each served address with their
// bindings, and what resolving one of them again takes. It is guarded by the
// cycle lock.
type watched struct {
	snap     inventory.Snapshot
	deny     resolve.Denylist
	required resolve.Level
	routes   map[netip.Addr][]watchedRoute
}

type watchedRoute struct {
	route   model.Route
	binding resolve.Binding
}

// moveCheck is the verification of one address whose MAC moved: whether one
// waits or runs, whether the MAC moved again since it began, and when the
// last one began.
type moveCheck struct {
	pending bool
	again   bool
	last    time.Time
}

// watch keeps, from the results of the cycle, the routes whose address it
// served on a binding, and their pins for the watch of the network.
func (c *cycleRun) watch() {
	w := watched{snap: c.snap, deny: c.deny, required: c.requiredLevel(), routes: map[netip.Addr][]watchedRoute{}}
	pins := map[netip.Addr]egress.Pin{}
	for _, rt := range c.claims.Winners {
		res, ok := c.results[rt.Hostname]
		t := res.Target
		switch {
		case !ok, rt.Guest == nil, res.Binding == nil, !t.Addr.IsValid(), t.Withdrawn, t.Rejected:
			continue
		}
		hw, err := net.ParseMAC(res.Binding.MAC)
		if err != nil {
			continue
		}
		w.routes[t.Addr] = append(w.routes[t.Addr], watchedRoute{route: rt, binding: *res.Binding})
		p, pinned := pins[t.Addr]
		if !pinned {
			p = egress.Pin{MAC: hw, Bridge: res.Binding.Bridge, Port: res.Binding.Port}
		}
		if g, ok := c.snap.Guest(*rt.Guest); ok {
			p.Own = ownMACs(p, g, res.Binding.Ports)
		}
		pins[t.Addr] = p
	}
	c.e.watched = w
	c.e.egMu.Lock()
	c.e.pins = pins
	c.e.egMu.Unlock()
}

// ownMACs adds to the other MACs of a pin those of the guest's NICs, each
// with the ports of the bridge it belongs on: the one the proof found it on,
// or else those Proxmox gives its NIC. Only the pin's bridge is watched, and
// a MAC learned there on another port has moved.
func ownMACs(p egress.Pin, g model.Guest, placed map[string]string) []egress.OwnMAC {
	own := p.Own
	for _, n := range g.NICs {
		hw, err := net.ParseMAC(n.MAC)
		if err != nil || hw.String() == p.MAC.String() ||
			slices.ContainsFunc(own, func(o egress.OwnMAC) bool { return o.MAC.String() == hw.String() }) {
			continue
		}
		ports := resolve.GuestPorts(g.Ref.VMID, n.Index)
		if port := placed[hw.String()]; port != "" {
			ports = []string{port}
		}
		own = append(own, egress.OwnMAC{MAC: hw, Ports: ports})
	}
	return own
}

// Bound returns the pins of the addresses the last cycle served on a binding,
// for the watch of the network. The map is not changed afterwards.
func (e *Engine) Bound() map[netip.Addr]egress.Pin {
	e.egMu.Lock()
	defer e.egMu.Unlock()
	return e.pins
}

// Moved takes an address whose MAC moved out of the egress filter at once,
// and has its routes verified again as soon as the cycle lock lets it, at
// most once every reverifyEvery: once that passes, the address comes back;
// when it fails, the address stays out and a cycle is asked for, which
// withdraws its routes. Moved does not wait for the verification.
func (e *Engine) Moved(ctx context.Context, addr netip.Addr) {
	e.egMu.Lock()
	first := !e.suspects[addr]
	e.suspects[addr] = true
	err := e.d.Egress.Remove(ctx, addr)
	check := e.checks[addr]
	if check == nil {
		check = &moveCheck{}
		e.checks[addr] = check
	}
	start := !check.pending
	check.pending, check.again = true, check.again || !start
	wait := check.last.Add(reverifyEvery).Sub(e.d.Now())
	e.egMu.Unlock()

	if err != nil {
		e.d.Log.Warn().Err(err).Stringer("addr", addr).Msg("taking an address whose MAC moved out of the egress filter failed")
	}
	if first {
		e.events.add(Event{At: e.d.Now(), Level: levelWarn, Kind: kindEgress, Subject: addr.String(),
			Message: fmt.Sprintf("the MAC of %s moved; the connectors do not reach it until it is verified again", addr)})
	}
	if start {
		e.moving.Add(1)
		go e.reverifyAfter(ctx, addr, wait)
	}
}

// reverifyAfter verifies addr again once wait is over.
func (e *Engine) reverifyAfter(ctx context.Context, addr netip.Addr, wait time.Duration) {
	defer e.moving.Done()
	if wait > 0 {
		ch, stop := e.after(wait)
		select {
		case <-ctx.Done():
			stop()
			e.dropCheck(addr)
			return
		case <-ch:
		}
	}
	if err := e.acquire(ctx); err != nil {
		e.dropCheck(addr)
		return
	}
	defer e.release()
	e.reverify(ctx, addr)
}

// dropCheck forgets a verification that will not run, as the daemon stops.
func (e *Engine) dropCheck(addr netip.Addr) {
	e.egMu.Lock()
	defer e.egMu.Unlock()
	if check := e.checks[addr]; check != nil {
		check.pending, check.again = false, false
	}
}

// reverify resolves every route served on addr again, the way a cycle does,
// and lets the address back into the egress filter only when all of them
// are served there again at the level the settings ask for. The caller holds
// the cycle lock.
func (e *Engine) reverify(ctx context.Context, addr netip.Addr) {
	e.egMu.Lock()
	began := e.d.Now()
	e.checks[addr].again = false
	e.egMu.Unlock()

	why, passed := e.verifyMoved(ctx, addr)
	if ctx.Err() != nil {
		e.dropCheck(addr)
		return
	}

	e.egMu.Lock()
	check := e.checks[addr]
	check.last = began
	again := check.again
	if !passed {
		e.egress = slices.DeleteFunc(slices.Clone(e.egress), func(t egress.Target) bool { return t.Addr == addr })
		pins := maps.Clone(e.pins)
		delete(pins, addr)
		e.pins = pins
	}
	var err error
	if !again {
		check.pending = false
		delete(e.suspects, addr)
		if passed {
			err = e.d.Egress.Set(ctx, e.unsuspected())
		}
	}
	e.egMu.Unlock()

	switch {
	case again:
		// It moved again while it was verified: once more, when the rate
		// limit lets it, before it comes back.
		e.moving.Add(1)
		go e.reverifyAfter(ctx, addr, began.Add(reverifyEvery).Sub(e.d.Now()))
	case !passed:
		e.events.add(Event{At: e.d.Now(), Level: levelWarn, Kind: kindEgress, Subject: addr.String(),
			Message: fmt.Sprintf("%s did not pass its verification after its MAC moved (%s); it stays out of the egress filter, and its routes are withdrawn", addr, why)})
	case err != nil:
		e.d.Log.Warn().Err(err).Stringer("addr", addr).Msg("letting an address verified again back into the egress filter failed; the next cycle sets it")
	default:
		e.events.add(Event{At: e.d.Now(), Level: levelInfo, Kind: kindEgress, Subject: addr.String(),
			Message: fmt.Sprintf("%s was verified again after its MAC moved; the connectors reach it again", addr)})
	}
	if !passed {
		e.saveEgress()
		e.Trigger()
	}
}

// verifyMoved resolves the routes served on addr again and reports whether
// each of them is served there again at the level required. A route that is
// not keeps the binding resolution gave it, so that the next cycle starts
// from what was found. The caller holds the cycle lock.
func (e *Engine) verifyMoved(ctx context.Context, addr netip.Addr) (why string, passed bool) {
	w := e.watched
	routes := w.routes[addr]
	if len(routes) == 0 {
		return "no route is served on it any more", false
	}
	for i, wr := range routes {
		rctx, cancel := e.timeout(ctx, resolveTimeout)
		prev := wr.binding
		res := e.d.Resolver.Resolve(rctx, wr.route, w.snap, &prev, w.deny, w.required)
		cancel()
		if ctx.Err() != nil {
			return "the daemon stops", false
		}
		t := res.Target
		if t.Addr == addr && !t.Withdrawn && !t.Rejected && res.Binding != nil && res.Level.AtLeast(w.required) {
			routes[i].binding = *res.Binding
			continue
		}
		e.keepBinding(wr.route.Hostname, res.Binding)
		why = t.Reason
		if why == "" {
			why = fmt.Sprintf("identity level %s is below the required %s", cmp.Or(string(res.Level), "none"), w.required)
		}
		return why, false
	}
	return "", true
}

// keepBinding saves the binding a verification outside the cycle found for a
// hostname. One that cannot be saved is logged: the address stays out of the
// filter all the same, and the next cycle resolves it anew.
func (e *Engine) keepBinding(host string, b *resolve.Binding) {
	if b == nil {
		return
	}
	bindings, err := e.d.Store.Bindings()
	if err == nil {
		bindings[host] = *b
		err = e.d.Store.SaveBindings(bindings)
	}
	if err != nil {
		e.d.Log.Warn().Err(err).Str("route", host).Msg("saving the binding of a route verified after its MAC moved failed")
	}
}

// saveEgress saves the memory after the set changed outside a cycle, once
// the memory was read. The caller holds the cycle lock.
func (e *Engine) saveEgress() {
	if !e.remembered {
		return
	}
	if err := e.d.Store.SaveEngineMemory(e.memory()); err != nil {
		e.d.Log.Warn().Err(err).Msg("saving what the engine remembers failed; the next cycle saves it")
	}
}

// unsuspected is the set of targets less the addresses whose MAC moved and
// that were not verified again yet. The caller holds egMu.
func (e *Engine) unsuspected() []egress.Target {
	return slices.DeleteFunc(slices.Clone(e.egress), func(t egress.Target) bool { return e.suspects[t.Addr] })
}
