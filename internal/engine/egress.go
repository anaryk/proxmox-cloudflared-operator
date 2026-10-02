package engine

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
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
// less the addresses that lost their proof. A target thus enters the set
// before the tunnel run that publishes its rule, and leaves it only after a
// tunnel run verified a configuration without it. A cycle that holds because
// of the store leaves the set as it is. A set that cannot be given holds the
// writes of the tunnel run; a filter the admin switched off is no failure.
func (c *cycleRun) feedEgress() {
	if c.storeHold {
		return
	}
	set := c.verifiedTargets()
	sent := c.e.sentTargets()
	for _, t := range c.e.egress {
		if sent[t] {
			set = append(set, t)
		}
	}
	set = slices.DeleteFunc(set, func(t egress.Target) bool { return c.lost[t.Addr] })
	slices.SortFunc(set, compareTargets)
	set = slices.Compact(set)
	c.e.egress = set
	err := c.e.d.Egress.Set(c.ctx, slices.Clone(set))
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
// more.
func (c *cycleRun) verifiedTargets() []egress.Target {
	var out []egress.Target
	for _, rt := range c.claims.Winners {
		res, ok := c.results[rt.Hostname]
		t := res.Target
		switch {
		case !ok, !t.Addr.IsValid(), t.Withdrawn, t.Rejected, t.Owner != "" && t.Owner != rt.Owner(), rt.Target.Port == 0:
			continue
		}
		out = append(out, egress.Target{Addr: t.Addr, Port: rt.Target.Port})
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
	slices.SortFunc(out, compareTargets)
	return slices.Compact(out)
}

func compareTargets(a, b egress.Target) int {
	return cmp.Or(a.Addr.Compare(b.Addr), cmp.Compare(a.Port, b.Port))
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
