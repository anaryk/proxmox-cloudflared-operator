package agent

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// reasonAttestationStale is why a target served on another node's
// attestation is reported at observed: no controller has confirmed that
// node's word for longer than a proof may stand, which only a silence of the
// controller allows; it stays served until one of this node's checks fails
// or the hold-over reaches its ceiling.
const reasonAttestationStale = "attestation stale: no controller has confirmed the attestation of %s for too long; served until a check of this node fails, for 15 minutes at most"

// verifyAll proves, on this node, the address another node chose for every
// target this node does not resolve: with the MACs it was chosen with, the
// attestation of the node that hosts the guest and the coverage the
// controller found. It runs as resolveAll does, at most resolveConcurrency
// at a time and each within resolveTimeout. A target whose address no node
// chose yet is left as it is.
func (p *pass) verifyAll() {
	var targets []agentproto.Target
	for _, t := range p.s.Targets {
		if !t.Resolve && t.Addr.IsValid() {
			targets = append(targets, t)
		}
	}
	out := make([]resolve.VerifyResult, len(targets))
	slots := make(chan struct{}, resolveConcurrency)
	var wg sync.WaitGroup
	started := 0
	for i, t := range targets {
		select {
		case slots <- struct{}{}:
		case <-p.ctx.Done():
		}
		if p.ctx.Err() != nil {
			break
		}
		var prev *resolve.Binding
		if b, ok := p.bindings[t.Route.Hostname]; ok {
			prev = &b
		}
		started++
		wg.Go(func() {
			defer func() { <-slots }()
			ctx, cancel := context.WithTimeout(p.ctx, resolveTimeout)
			defer cancel()
			out[i] = p.a.verify(ctx, t, p.inv, prev, p.deny)
		})
	}
	wg.Wait()
	for i, t := range targets[:started] {
		p.results[t.Key] = out[i].Result
		p.served[t.Key] = cloneResult(out[i].Result)
		switch {
		case !out[i].Attested || t.Attestation == nil:
		case out[i].Stale:
			p.stale[t.Key] = t.Attestation.Node
		default:
			p.attested[t.Key] = t.Attestation.Node
		}
	}
}

// verify proves the address another node chose for t on this node. The
// attestation the snapshot carries goes to the resolver with the times that
// let it judge freshness itself: how old the attesting node's status was at
// the controller, as the last heartbeat says, when this node heard that
// heartbeat, and since when the MAC has been on its port. Without a
// heartbeat that names the attesting node the times are zero, and the
// attestation stands for nothing. An appliance agent reads no bridge, so it
// passes no attestation and no coverage, whatever the snapshot says.
func (a *Agent) verify(ctx context.Context, t agentproto.Target, inv inventory.Snapshot, prev *resolve.Binding, deny resolve.Denylist) resolve.VerifyResult {
	req := resolve.VerifyRequest{
		Route: t.Route, Addr: t.Addr, Port: t.Port, MACs: t.MACs, Snap: inv, Deny: deny,
		Required: resolve.Level(t.Required), Prev: prev,
	}
	if t.Guest != nil {
		req.Guest = *t.Guest
	}
	if a.d.Kind != pki.KindHost {
		return a.d.Resolver.VerifyAddress(ctx, req)
	}
	req.Covered = t.Covered
	if at := t.Attestation; at != nil {
		req.Attestation = &resolve.Attestation{
			Node: at.Node, MAC: at.MAC, Bridge: at.Bridge, VLAN: at.VLAN, Port: at.Port, StableSince: at.Since,
		}
		req.Attestation.StatusAge, req.Attestation.HeardAt, req.Attestation.Recalled = a.heardOf(at.Node)
	}
	return a.d.Resolver.VerifyAddress(ctx, req)
}

// namedIn is what a heartbeat said of a node: how old its status was at the
// controller, when this node received the heartbeat, and its epoch.
type namedIn struct {
	statusAge time.Duration
	at        time.Time
	epoch     agentproto.Epoch
}

// noteNamed keeps what a heartbeat of the epoch the agent follows says of
// the nodes listed. A node it leaves out was dropped by a controller that
// answers: what an earlier heartbeat of the same epoch said of it is
// forgotten. The caller holds heardMu.
func (a *Agent) noteNamed(h agentproto.Heartbeat, now time.Time) {
	if a.named == nil {
		a.named = map[string]namedIn{}
	}
	for node := range a.listed {
		if fresh, ok := h.Nodes[node]; ok {
			a.named[node] = namedIn{statusAge: fresh.StatusAge, at: now, epoch: h.Epoch}
		} else if n, ok := a.named[node]; ok {
			if _, same := n.epoch.Order(h.Epoch); same {
				delete(a.named, node)
			}
		}
	}
}

// pruneNamed forgets the nodes no longer listed, and what is older than a
// hold-over may be. The caller holds heardMu.
func (a *Agent) pruneNamed(now time.Time) {
	maps.DeleteFunc(a.named, func(node string, n namedIn) bool {
		return !a.listed[node] || now.Sub(n.at) > resolve.MaxHoldOver
	})
}

// listNodes notes the nodes the desired state names, of which alone
// heartbeats are kept.
func (a *Agent) listNodes(nodes []agentproto.Node) {
	listed := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		listed[n.Name] = true
	}
	a.heardMu.Lock()
	defer a.heardMu.Unlock()
	a.listed = listed
	a.pruneNamed(a.d.Now())
}

// heardOf is what the last heartbeat said of node: how old its status was at
// the controller, and when this node received the heartbeat. When the last
// heartbeat does not name node, or is of an epoch below the one the agent
// follows, and no controller answers, or one of a newer epoch than the last
// word about node, as during a takeover, it is that last word, recalled:
// the resolver lets such times keep an address on the attestation it stood
// on for a while, never prove one. Zero otherwise: a controller of the same
// epoch that answers and leaves node out has dropped it. It takes heardMu
// for the heartbeat; the caller holds the agent's lock, under which the
// epoch the agent follows is read.
func (a *Agent) heardOf(node string) (time.Duration, time.Time, bool) {
	now := a.d.Now()
	a.heardMu.Lock()
	defer a.heardMu.Unlock()
	if a.hb == nil {
		return 0, time.Time{}, false
	}
	if c, _ := a.hb.Epoch.Order(a.remembered); c >= 0 {
		if fresh, ok := a.hb.Nodes[node]; ok {
			return fresh.StatusAge, a.hbAt, false
		}
	}
	n, ok := a.named[node]
	if !ok || now.Sub(n.at) > resolve.MaxHoldOver {
		return 0, time.Time{}, false
	}
	silent := now.Sub(a.hbAt) > resolve.MaxHeartbeatAge
	newer, _ := a.hb.Epoch.Order(n.epoch)
	following, _ := a.remembered.Order(n.epoch)
	if !silent && newer <= 0 && following <= 0 {
		return 0, time.Time{}, false
	}
	return n.statusAge, n.at, true
}

// prove proves the address of t again outside a pass: as this node resolves
// it, or as it verifies the one another node chose.
func (a *Agent) prove(ctx context.Context, t agentproto.Target, inv inventory.Snapshot, prev *resolve.Binding, deny resolve.Denylist) resolve.Result {
	if t.Resolve {
		return a.d.Resolver.Resolve(ctx, t.Route, inv, prev, deny, resolve.Level(t.Required), nil)
	}
	return a.verify(ctx, t, inv, prev, deny).Result
}
