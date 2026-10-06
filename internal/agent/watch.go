package agent

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// reverifyEvery is the least time between the starts of two verifications of
// an address whose MAC moved, however often it moves.
const reverifyEvery = 5 * time.Second

// watched is what the last pass bound, for the watch of the network: the
// targets of each served address with their bindings, and what resolving one
// of them again takes. It is guarded by the agent's lock.
type watched struct {
	inv     inventory.Snapshot
	deny    resolve.Denylist
	targets map[netip.Addr][]watchedTarget
}

type watchedTarget struct {
	target  agentproto.Target
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

// watch keeps, from the pass, the targets whose address it served on a
// binding, and their pins for the watch of the network, each by the address
// the egress set holds for it: a target on a leg by its service address, with
// the leg its guest answers on and the guest's own address there, so that two
// networks with one subnet never share a pin.
func (p *pass) watch() {
	legIfaces := map[string]string{}
	for _, l := range p.cache.Legs {
		legIfaces[l.ID] = gateway.LegIface(l.Mark)
	}
	w := watched{inv: p.inv, deny: p.deny, targets: map[netip.Addr][]watchedTarget{}}
	pins := map[netip.Addr]egress.Pin{}
	for _, t := range p.s.Targets {
		res, ok := p.served[t.Key]
		rt := res.Target
		key, served := rt.Dialed()
		switch {
		case !ok, t.Route.Guest == nil, res.Binding == nil, !served, rt.Withdrawn, rt.Rejected:
			continue
		}
		hw, err := net.ParseMAC(res.Binding.MAC)
		if err != nil {
			continue
		}
		w.targets[key] = append(w.targets[key], watchedTarget{target: t, binding: *res.Binding})
		pin, pinned := pins[key]
		if !pinned {
			pin = egress.Pin{MAC: hw, Bridge: res.Binding.Bridge, Port: res.Binding.Port}
			if rt.Leg {
				pin.Dev, pin.Real = legIfaces[res.Binding.Via], rt.Addr
			}
		}
		if g, ok := p.inv.Guest(*t.Route.Guest); ok {
			pin.Own = ownMACs(pin, g, res.Binding.Ports)
		}
		pins[key] = pin
	}
	p.a.watched = w
	p.a.egMu.Lock()
	p.a.pins = pins
	p.a.egMu.Unlock()
	p.a.vouch.pin(pins, p.a.d.Now())
}

// ownMACs adds to the other MACs of a pin those of the guest's cards, each
// with the ports of the bridge it belongs on: the one the proof found it on,
// or else those Proxmox gives its card.
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

// Bound returns the pins of the addresses the last pass served on a binding,
// for the watch of the network, by the address the egress set holds for
// each. The map is not changed afterwards.
func (a *Agent) Bound() map[netip.Addr]egress.Pin {
	a.egMu.Lock()
	defer a.egMu.Unlock()
	return a.pins
}

// Moved takes an address whose MAC moved out of the egress filter at once,
// and has its targets resolved again as soon as no pass runs, at most once
// every reverifyEvery: once that passes the address comes back; when it
// fails, it stays out, and the controller is told. The address is one Bound
// gave. Moved does not wait for the verification.
func (a *Agent) Moved(ctx context.Context, addr netip.Addr) {
	a.vouch.moved(addr, a.d.Now())
	a.egMu.Lock()
	first := !a.suspects[addr]
	a.suspects[addr] = true
	err := a.d.Egress.Remove(ctx, addr)
	check := a.checks[addr]
	if check == nil {
		check = &moveCheck{}
		a.checks[addr] = check
	}
	start := !check.pending
	check.pending, check.again = true, check.again || !start
	wait := check.last.Add(reverifyEvery).Sub(a.d.Now())
	a.egMu.Unlock()

	if err != nil {
		a.d.Log.Warn().Err(err).Stringer("addr", addr).Msg("taking an address whose MAC moved out of the egress filter failed")
	}
	if first {
		a.event(levelWarn, kindEgress, addr.String(),
			fmt.Sprintf("the MAC of %s moved; the connectors do not reach it until it is verified again", addr))
	}
	if start {
		a.moving.Add(1)
		go a.reverifyAfter(ctx, addr, wait)
	}
}

// reverifyAfter verifies addr again once wait is over.
func (a *Agent) reverifyAfter(ctx context.Context, addr netip.Addr, wait time.Duration) {
	defer a.moving.Done()
	if wait > 0 {
		ch, stop := a.after(wait)
		select {
		case <-ctx.Done():
			stop()
			a.dropCheck(addr)
			return
		case <-ch:
		}
	}
	if err := a.acquire(ctx); err != nil {
		a.dropCheck(addr)
		return
	}
	defer a.release()
	a.reverify(ctx, addr)
}

// dropCheck forgets a verification that will not run.
func (a *Agent) dropCheck(addr netip.Addr) {
	a.egMu.Lock()
	defer a.egMu.Unlock()
	if check := a.checks[addr]; check != nil {
		check.pending, check.again = false, false
	}
}

// reverify resolves every target served on addr again and lets the address
// back into the egress filter only when all of them are served there again
// at the level they require. The caller holds the agent's lock.
func (a *Agent) reverify(ctx context.Context, addr netip.Addr) {
	a.egMu.Lock()
	began := a.d.Now()
	a.checks[addr].again = false
	a.egMu.Unlock()

	why, passed := a.verifyMoved(ctx, addr)
	if ctx.Err() != nil {
		a.dropCheck(addr)
		return
	}

	a.egMu.Lock()
	check := a.checks[addr]
	check.last = began
	again := check.again
	if !passed {
		a.set = slices.DeleteFunc(slices.Clone(a.set), func(t egress.Target) bool { return t.Addr == addr })
		pins := maps.Clone(a.pins)
		delete(pins, addr)
		a.pins = pins
	}
	var err error
	if !again {
		check.pending = false
		delete(a.suspects, addr)
		if passed && a.stood == "" {
			err = a.d.Egress.Set(ctx, a.unsuspected())
		}
	}
	a.egMu.Unlock()

	switch {
	case again:
		// It moved again while it was verified: once more, when the rate
		// limit lets it, before it comes back.
		a.moving.Add(1)
		go a.reverifyAfter(ctx, addr, began.Add(reverifyEvery).Sub(a.d.Now()))
	case !passed:
		a.event(levelWarn, kindEgress, addr.String(),
			fmt.Sprintf("%s did not pass its verification after its MAC moved (%s); it stays out of the egress filter, and its routes are withdrawn", addr, why))
	case err != nil:
		a.d.Log.Warn().Err(err).Stringer("addr", addr).Msg("letting an address verified again back into the egress filter failed; the next pass sets it")
	default:
		a.event(levelInfo, kindEgress, addr.String(),
			fmt.Sprintf("%s was verified again after its MAC moved; the connectors reach it again", addr))
	}
	if !passed {
		if err := a.saveSet(); err != nil {
			a.d.Log.Warn().Err(err).Msg("saving the egress set failed; the next pass saves it")
		}
		a.republish()
		a.changed()
	}
}

// verifyMoved resolves the targets served on addr again and reports whether
// each of them is served there again at the level it requires: at the
// address its binding has, through the leg it was proven through. A target
// that is not keeps the binding resolution gave it, so that the next pass
// starts from what was found. The caller holds the agent's lock.
func (a *Agent) verifyMoved(ctx context.Context, addr netip.Addr) (why string, passed bool) {
	w := a.watched
	targets := w.targets[addr]
	if len(targets) == 0 {
		return "no route is served on it any more", false
	}
	for i, wt := range targets {
		rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
		prev := wt.binding
		required := resolve.Level(wt.target.Required)
		res := a.d.Resolver.Resolve(rctx, wt.target.Route, w.inv, &prev, w.deny, required, nil)
		cancel()
		if ctx.Err() != nil {
			return "the agent stops", false
		}
		t, b := res.Target, res.Binding
		if b != nil && b.Via == wt.binding.Via && b.Addr == wt.binding.Addr {
			// The same target is published where it was.
			b.Service = wt.binding.Service
		}
		if t.Addr == wt.binding.Addr && !t.Withdrawn && !t.Rejected && b != nil && b.Via == wt.binding.Via && res.Level.AtLeast(required) {
			targets[i].binding = *b
			continue
		}
		a.keepBinding(wt.target.Route.Hostname, b)
		why = t.Reason
		if why == "" {
			why = fmt.Sprintf(reasonBelowMinimum, string(res.Level), required)
			if res.Level == "" {
				why = fmt.Sprintf(reasonBelowMinimum, "none", required)
			}
		}
		return why, false
	}
	return "", true
}

// keepBinding saves the binding a verification outside a pass found for a
// hostname. One that cannot be saved is logged: the address stays out of the
// filter all the same, and the next pass resolves it anew.
func (a *Agent) keepBinding(host string, b *resolve.Binding) {
	if b == nil {
		return
	}
	bindings, err := a.d.Store.Bindings()
	if err == nil {
		bindings[host] = *b
		err = a.d.Store.SaveBindings(bindings)
	}
	if err != nil {
		a.d.Log.Warn().Err(err).Str("route", host).Msg("saving the binding of a route verified after its MAC moved failed")
	}
}
