package agent

import (
	"context"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

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
		if out[i].Attested && t.Attestation != nil {
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
		req.Attestation.StatusAge, req.Attestation.HeardAt = a.heardOf(at.Node)
	}
	return a.d.Resolver.VerifyAddress(ctx, req)
}

// heardOf is what the last heartbeat said of node: how old its status was at
// the controller, and when this node received the heartbeat. Zero when no
// heartbeat names it, or the last one is of an epoch below the one the agent
// follows: a stale controller vouches for nothing. It takes heardMu for the
// heartbeat; the caller holds the agent's lock, under which the epoch the
// agent follows is read.
func (a *Agent) heardOf(node string) (time.Duration, time.Time) {
	a.heardMu.Lock()
	defer a.heardMu.Unlock()
	if a.hb == nil {
		return 0, time.Time{}
	}
	if c, _ := a.hb.Epoch.Order(a.remembered); c < 0 {
		return 0, time.Time{}
	}
	fresh, ok := a.hb.Nodes[node]
	if !ok {
		return 0, time.Time{}
	}
	return fresh.StatusAge, a.hbAt
}

// prove proves the address of t again outside a pass: as this node resolves
// it, or as it verifies the one another node chose.
func (a *Agent) prove(ctx context.Context, t agentproto.Target, inv inventory.Snapshot, prev *resolve.Binding, deny resolve.Denylist) resolve.Result {
	if t.Resolve {
		return a.d.Resolver.Resolve(ctx, t.Route, inv, prev, deny, resolve.Level(t.Required), nil)
	}
	return a.verify(ctx, t, inv, prev, deny).Result
}
