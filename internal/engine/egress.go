package engine

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Egress is the filter that confines the connectors to the targets the
// engine verified. Its methods may be called from more than one goroutine.
type Egress interface {
	// Set replaces the targets the connectors may open connections to.
	Set(ctx context.Context, targets []egress.Target) error
	// Remove takes every target of an address out at once.
	Remove(ctx context.Context, addr netip.Addr) error
	// FlowCounts reads the counters of the live table: by target, the
	// connections a connector opened to it, and the generation of the table.
	// Two reads of one generation are of the same counters.
	FlowCounts(ctx context.Context) (generation string, counts map[netip.AddrPort]uint64, err error)
}

// The states of the egress filter, as EgressView says them.
const (
	EgressOn        = "on"
	EgressOff       = "off"
	EgressNotLoaded = "not loaded"
	EgressChanged   = "changed"
)

// EgressView is the egress filter as the daemon last found it: on, switched
// off by the admin, or its table not loaded or not the one pco applied, and
// pco could not load it again. It is empty until the daemon has looked.
type EgressView struct {
	State string `json:"state"`
	// Since is when the admin switched the filter off; zero when it is not
	// off, or when the time could not be read.
	Since time.Time `json:"since,omitzero"`
}

// heldEgress is why a cycle whose egress filter could not be set does not
// write the configuration of a tunnel: a rule must not send a connector to a
// target it cannot reach.
const heldEgress = "the egress filter could not be set"

// blockLost takes out of the egress filter, at once, every address whose
// binding lost its proof in this cycle, and keeps them for the set: what the
// rest of the cycle takes may be long, and the address may now be someone
// else's. Only the addresses the last set holds can be in the table.
func (c *cycleRun) blockLost() {
	c.lost = map[netip.Addr]bool{}
	for _, rt := range c.claims.Winners {
		res, ok := c.results[rt.Hostname]
		prev, bound := c.bindings[rt.Hostname]
		if !ok || !bound {
			continue
		}
		if addr, lost := resolve.LostProof(rt, &prev, res); lost {
			c.lost[addr] = true
		}
	}
	for _, addr := range slices.SortedFunc(maps.Keys(c.lost), netip.Addr.Compare) {
		if !slices.ContainsFunc(c.e.egress, func(t egress.Target) bool { return t.Addr == addr }) {
			continue
		}
		if err := c.e.d.Egress.Remove(c.ctx, addr); err != nil {
			c.problem("taking %s out of the egress filter: %v", addr, err)
		}
	}
}

// feedEgress gives the egress filter the targets of this cycle: every target
// it verified for a winner, and every target of the last set that a tunnel
// configuration as last verified at Cloudflare still sends a connector to,
// unless its binding is withdrawn, less the addresses that lost their proof;
// an address whose MAC moved stays out until it is verified again. A target
// thus enters the set before the tunnel run that publishes its rule, and
// leaves it only after a tunnel run verified a configuration without it,
// unless its proof is lost. A cycle that holds because of the store leaves the
// set as it is. A set that cannot be given holds the writes of the tunnel run;
// a filter the admin switched off is no failure. The traffic keeps, for each
// target of the set, the routes it serves.
func (c *cycleRun) feedEgress() {
	if c.storeHold {
		return
	}
	set := c.verifiedTargets()
	sent := c.e.sentTargets()
	withdrawn := c.withdrawnAddrs()
	for _, t := range c.e.egress {
		// A target whose proof only fell below the minimum is still proven
		// and leaves after its rule, as any other that is no longer served.
		// A withdrawn one is not fed back, whether or not its proof was lost
		// in this cycle: the memory of the set may be older than the binding
		// that says so.
		if sent[egress.Target{Addr: t.Addr, Port: t.Port}] && !withdrawn[t.Addr] {
			set = append(set, t)
		}
	}
	set = slices.DeleteFunc(set, func(t egress.Target) bool { return c.lost[t.Addr] })
	// A target of allowNode sorts first among its equals, and is the one kept:
	// one of an earlier process comes back from the memory without the mark.
	slices.SortFunc(set, egress.CompareAllowNodeFirst)
	set = slices.CompactFunc(set, func(a, b egress.Target) bool { return egress.CompareEndpoints(a, b) == 0 })
	c.e.egress = set
	c.e.keepServed(c.servedTargets())
	c.e.egMu.Lock()
	err := c.e.d.Egress.Set(c.ctx, c.e.unsuspected())
	c.e.egMu.Unlock()
	switch {
	case err == nil, errors.Is(err, egress.ErrOff):
	default:
		c.egressHeld = heldEgress
		c.problem("setting the egress filter: %v; no tunnel configuration is written until it is set", err)
	}
}

// verifiedTargets are the targets this cycle verified for the winners: an
// address resolution proved for the owner that serves the hostname, at the
// route's port. A target the identity minimum holds back has no address any
// more. The target of a manual route with allowNode is marked so: it may be an
// address of the node, which the filter refuses for any other.
func (c *cycleRun) verifiedTargets() []egress.Target {
	var out []egress.Target
	for _, rt := range c.claims.Winners {
		if t, ok := c.verifiedTarget(rt); ok {
			out = append(out, t)
		}
	}
	return out
}

// verifiedTarget is the target this cycle verified for a winner, if any.
func (c *cycleRun) verifiedTarget(rt model.Route) (egress.Target, bool) {
	res, ok := c.results[rt.Hostname]
	t := res.Target
	switch {
	case !ok, !t.Addr.IsValid(), t.Withdrawn, t.Rejected, t.Owner != "" && t.Owner != rt.Owner(), rt.Target.Port == 0:
		return egress.Target{}, false
	}
	return egress.Target{Addr: t.Addr, Port: rt.Target.Port, AllowNode: rt.Source == model.SourceManual && rt.Options.AllowNode}, true
}

// withdrawnAddrs are the addresses of the winners whose binding is withdrawn
// after this cycle's resolution, and of every stored binding that was
// withdrawn when the cycle began, which covers a route gone since.
func (c *cycleRun) withdrawnAddrs() map[netip.Addr]bool {
	out := map[netip.Addr]bool{}
	for _, b := range c.bindings {
		if b.Withdrawn {
			out[b.Addr] = true
		}
	}
	for _, rt := range c.claims.Winners {
		if b := c.results[rt.Hostname].Binding; b != nil && b.Withdrawn {
			out[b.Addr] = true
		}
	}
	return out
}

// sentTargets are the targets the tunnel configurations last verified at
// Cloudflare send a connector to.
func (e *Engine) sentTargets() map[egress.Target]bool {
	out := map[egress.Target]bool{}
	for _, ts := range e.verified {
		for _, t := range ts {
			out[t] = true
		}
	}
	return out
}

// noteVerified keeps, for every tunnel the run verified, the targets of the
// configuration it holds, which is the one the run had for it, and forgets the
// configuration of a tunnel that is gone. Any other tunnel keeps what was last
// verified of it.
func (c *cycleRun) noteVerified(tunnels []reconcile.TunnelState) {
	rules := make(map[string][]planner.IngressRule, len(c.plan.Tunnels))
	for _, p := range c.plan.Tunnels {
		rules[p.AccountID] = p.Rules
	}
	for _, t := range tunnels {
		switch {
		case t.Verified:
			c.e.verified[t.AccountID] = ruleTargets(rules[t.AccountID])
		case !t.Exists && !t.Unknown:
			delete(c.e.verified, t.AccountID)
		}
	}
}

// ruleTargets returns the origins rules send a connector to, sorted and once
// each. A rule that answers by itself, such as with a status, has none.
func ruleTargets(rules []planner.IngressRule) []egress.Target {
	out := []egress.Target{}
	for _, r := range rules {
		_, origin, ok := strings.Cut(r.Service, "://")
		if !ok {
			continue
		}
		ap, err := netip.ParseAddrPort(origin)
		if err != nil || ap.Port() == 0 {
			continue
		}
		out = append(out, egress.Target{Addr: ap.Addr().Unmap(), Port: ap.Port()})
	}
	slices.SortFunc(out, egress.CompareEndpoints)
	return slices.Compact(out)
}

// rememberEgress takes the set and the verified configurations from the
// memory of an earlier process.
func (e *Engine) rememberEgress(m store.EngineMemory) {
	e.egress = fromAddrPorts(m.Egress)
	for _, v := range m.Verified {
		e.verified[v.AccountID] = fromAddrPorts(v.Targets)
	}
}

// egressMemory is the set and the verified configurations as the memory
// keeps them.
func (e *Engine) egressMemory(m *store.EngineMemory) {
	m.Egress = toAddrPorts(e.egress)
	for _, account := range slices.Sorted(maps.Keys(e.verified)) {
		m.Verified = append(m.Verified, store.VerifiedTargets{AccountID: account, Targets: toAddrPorts(e.verified[account])})
	}
}

func fromAddrPorts(aps []netip.AddrPort) []egress.Target {
	out := make([]egress.Target, 0, len(aps))
	for _, ap := range aps {
		out = append(out, egress.Target{Addr: ap.Addr(), Port: ap.Port()})
	}
	return out
}

func toAddrPorts(ts []egress.Target) []netip.AddrPort {
	out := make([]netip.AddrPort, 0, len(ts))
	for _, t := range ts {
		out = append(out, netip.AddrPortFrom(t.Addr, t.Port))
	}
	return out
}
