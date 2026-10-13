package agent

import (
	"fmt"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

const (
	problemFilteredLost = "filtered lost: %s; the routes on pconet are served at observed"
	kindManaged         = "managed"
)

// filteredMemory is what the agent keeps of the filtered level between its
// passes: when the predicate was last judged, so that only the frames from
// unknown MACs since then count, and whether it held, for the events. It is
// guarded by the agent's lock.
type filteredMemory struct {
	judged time.Time
	held   *bool
	why    string // the first reason said, while it does not hold
}

// filtered judges, on an appliance node with the leg of the managed network,
// whether every guest on the vnet is isolated by its firewall, and raises the
// targets proven at observed through that leg to filtered while that holds,
// or takes them back to observed. It runs after resolution and before the
// minimum, as the controller's cycle did on one node; the agent judges it on
// its own node, so that the level does not age while the controller is away.
// A host agent, and an appliance that reads nothing of its node, raises
// nothing.
func (p *pass) filtered() {
	a := p.a
	leg := slices.ContainsFunc(p.cache.Legs, func(l gateway.Leg) bool { return l.ID == gateway.ManagedBridge })
	if a.d.Kind != pki.KindAppliance || a.d.Filtered == nil || !leg {
		p.raiseFiltered(false)
		a.filtered.held = nil
		return
	}
	in := a.d.Filtered(p.ctx, a.filtered.judged)
	res := managednet.Filtered(in, managednet.FirewallComment(a.d.InstallID))
	a.filtered.judged = p.now
	p.raiseFiltered(res.Holds)
	if !res.Holds {
		p.problem(problemFilteredLost, firstWhy(res.Why))
	}
	p.filteredEvent(res)
}

// raiseFiltered raises every result proven at observed through the managed
// network to filtered while the predicate holds, its binding with it; and,
// while it does not, takes a binding raised before back to observed, so that
// nothing stands on a proof that no longer holds. The resolver's result and
// the served one change alike.
func (p *pass) raiseFiltered(holds bool) {
	for _, results := range []map[string]resolve.Result{p.results, p.served} {
		for key, res := range results {
			if res.Binding == nil || res.Binding.Via != gateway.ManagedBridge {
				continue
			}
			b := *res.Binding
			switch {
			case holds && res.Level == resolve.LevelObserved:
				res.Level, b.Level = resolve.LevelFiltered, resolve.LevelFiltered
			case !holds && (b.Level == resolve.LevelFiltered || res.Level == resolve.LevelFiltered):
				b.Level = resolve.LevelObserved
				if res.Level == resolve.LevelFiltered {
					res.Level = resolve.LevelObserved
				}
			default:
				continue
			}
			res.Binding = &b
			results[key] = res
		}
	}
}

// filteredEvent says once when the predicate stops holding, or holds for
// another first reason, and once when it holds again.
func (p *pass) filteredEvent(res managednet.FilteredResult) {
	mem := &p.a.filtered
	last := mem.held
	holds := res.Holds
	mem.held = &holds
	switch {
	case !holds && (last == nil || *last || mem.why != firstWhy(res.Why)):
		mem.why = firstWhy(res.Why)
		p.a.eventOf(Event{At: p.now, Level: levelWarn, Kind: kindManaged, Subject: gateway.ManagedBridge, Message: fmt.Sprintf(problemFilteredLost, mem.why)})
	case holds && last != nil && !*last:
		mem.why = ""
		p.a.eventOf(Event{At: p.now, Level: levelInfo, Kind: kindManaged, Subject: gateway.ManagedBridge,
			Message: "filtered holds on pconet again: every guest on the vnet is isolated by its firewall"})
	}
}

func firstWhy(why []string) string {
	if len(why) == 0 {
		return "no reason was found"
	}
	return why[0]
}
