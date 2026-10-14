package engine

import (
	"cmp"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

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

// feedEgress has the agent give the egress filter the set of this cycle:
// every target it verified that the engine serves, and every target of the
// last set that a tunnel configuration as last verified at Cloudflare still
// sends a connector to, unless its binding is withdrawn, less the addresses
// that lost their proof; an address whose MAC moved stays out until it is
// verified again. A target thus enters the set before the tunnel run that
// publishes its rule, and leaves it only after a tunnel run verified a
// configuration without it, unless its proof is lost. A cycle that holds
// because of the store leaves the set as it is. A set that cannot be given
// holds the writes of the tunnel run; a filter the admin switched off is no
// failure. The traffic keeps, for each target of the set, the routes it
// serves.
func (c *cycleRun) feedEgress() {
	if c.storeHold || c.agentRefused {
		return
	}
	set, err := c.steps.Feed()
	c.e.egress = set
	c.e.keepServed(c.servedTargets())
	if err != nil {
		c.egressHeld = heldEgress
		c.problem("setting the egress filter: %v; no tunnel configuration is written until it is set", err)
	}
}

// egressSet is the set of targets the agent gives the filter when the cycle
// serves its results as they are now, as the gateway is given the service
// addresses of.
func (c *cycleRun) egressSet() []egress.Target {
	return c.steps.EgressSet(c.results)
}

// verifiedTarget is the target this cycle verified for a winner, if any.
func (c *cycleRun) verifiedTarget(rt model.Route) (egress.Target, bool) {
	res, ok := c.results[rt.Hostname]
	t := res.Target
	dialed, served := t.Dialed()
	switch {
	case !ok, !t.Addr.IsValid(), !served, t.Withdrawn, t.Rejected, t.Owner != "" && t.Owner != rt.Owner(), rt.Target.Port == 0:
		return egress.Target{}, false
	}
	return egress.Target{Addr: dialed, Port: rt.Target.Port, AllowNode: rt.Source == model.SourceManual && rt.Options.AllowNode}, true
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
	rules := make(map[tunnelRef][]planner.IngressRule, len(c.plan.Tunnels))
	for _, p := range c.plan.Tunnels {
		rules[tunnelRef{p.AccountID, p.Name}] = p.Rules
	}
	for _, t := range tunnels {
		ref := tunnelRef{t.AccountID, t.Name}
		switch {
		case t.Verified:
			c.e.verified[ref] = ruleTargets(rules[ref])
		case !t.Exists && !t.Unknown:
			delete(c.e.verified, ref)
		}
	}
}

// tunnelRef names one tunnel of the install: the shared one of an account
// and those pinned to nodes have names of their own.
type tunnelRef struct {
	account, name string
}

func compareTunnelRefs(a, b tunnelRef) int {
	return cmp.Or(cmp.Compare(a.account, b.account), cmp.Compare(a.name, b.name))
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

// rememberEgress takes the verified configurations from the memory of an
// earlier process. The set is the agent's, which keeps it itself.
func (e *Engine) rememberEgress(m store.EngineMemory) {
	for _, v := range m.Verified {
		// One saved before tunnels had names of their own is the shared
		// tunnel's.
		e.verified[tunnelRef{v.AccountID, cmp.Or(v.Name, e.sharedName(m.InstallID))}] = fromAddrPorts(v.Targets)
	}
}

// egressMemory is the set and the verified configurations as the memory
// keeps them; the shared tunnel goes without its name.
func (e *Engine) egressMemory(m *store.EngineMemory) {
	m.Egress = toAddrPorts(e.egress)
	for _, ref := range slices.SortedFunc(maps.Keys(e.verified), compareTunnelRefs) {
		v := store.VerifiedTargets{AccountID: ref.account, Targets: toAddrPorts(e.verified[ref])}
		if ref.name != e.sharedName(m.InstallID) {
			v.Name = ref.name
		}
		m.Verified = append(m.Verified, v)
	}
}

// sharedName is the name of the shared tunnel of the install, of install
// when the memory names one.
func (e *Engine) sharedName(install string) string {
	return planner.TunnelName(cmp.Or(install, e.memoryOf))
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
