package engine

import (
	"maps"
	"slices"
	"sync"
	"time"
)

// ClusterFacts is what the controller's cycles know of the cluster beyond
// the state, which the doctor reads.
type ClusterFacts struct {
	// CloudflareAt is when a cycle last got through to Cloudflare: zero
	// while none did since the daemon started, Started when it started.
	CloudflareAt time.Time
	Started      time.Time
	// ReadyReplicas are, by tunnel id, the nodes whose agent reported the
	// tunnel's connector ready in the last cycle, fresh reports only.
	ReadyReplicas map[string]int
	// PinnedEmpty are the tunnels pinned to a node that served no route
	// and that no record pointed at in the last cycle, with since when.
	PinnedEmpty []PinnedEmpty
	// AgentRule is what the firewall holds of the channel of the agents
	// that send no status.
	AgentRule AgentRuleFacts
}

// PinnedEmpty is a tunnel pinned to a node that serves nothing.
type PinnedEmpty struct {
	Name      string
	AccountID string
	Since     time.Time
}

// clusterFacts keeps the ClusterFacts the last cycle left.
type clusterFacts struct {
	mu    sync.Mutex
	facts ClusterFacts
}

// ClusterFacts returns what the last cycle knew of the cluster.
func (e *Engine) ClusterFacts() ClusterFacts {
	f := &e.facts
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.facts
	out.ReadyReplicas = maps.Clone(out.ReadyReplicas)
	out.PinnedEmpty = slices.Clone(out.PinnedEmpty)
	out.AgentRule.Silent = slices.Clone(out.AgentRule.Silent)
	return out
}

// noteFacts keeps what the cycle c found of the cluster. The caller holds
// the cycle lock, which guards the placement memory.
func (e *Engine) noteFacts(c *cycleRun) {
	ready := map[string]int{}
	for _, rep := range c.reports {
		if !rep.Fresh {
			continue
		}
		for _, s := range rep.Status.Connectors {
			if s.Ready && s.TunnelID != "" {
				ready[s.TunnelID]++
			}
		}
	}
	var empty []PinnedEmpty
	for ref, since := range e.place.emptySince {
		empty = append(empty, PinnedEmpty{Name: ref.name, AccountID: ref.account, Since: since})
	}
	slices.SortFunc(empty, func(a, b PinnedEmpty) int {
		return compareTunnelRefs(tunnelRef{a.AccountID, a.Name}, tunnelRef{b.AccountID, b.Name})
	})
	f := &e.facts
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.facts.Started.IsZero() {
		f.facts.Started = c.now
	}
	if c.checked || c.connected {
		f.facts.CloudflareAt = c.now
	}
	f.facts.ReadyReplicas, f.facts.PinnedEmpty = ready, empty
	f.facts.AgentRule = e.agentRule.facts
}
