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

// reasonAttestationStale is why a target served on another node's
// attestation is reported at observed: no controller has confirmed that
// node's word for longer than a proof may stand, which only a silence of the
// controller allows; it stays served until one of this node's checks fails.
const reasonAttestationStale = "attestation stale: no controller has confirmed the attestation of %s for too long; served until a check of this node fails"

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
// controller, and when this node received the heartbeat.
type namedIn struct {
	statusAge time.Duration
	at        time.Time
}

// heardOf is what the last heartbeat said of node: how old its status was at
// the controller, and when this node received the heartbeat. When the last
// heartbeat does not name node, or is of an epoch below the one the agent
// follows, it is what the last heartbeat that named node said, recalled: the
// resolver lets such times keep an address on the attestation it stood on,
// never prove one. Zero when no heartbeat this process received named it.
// Every heartbeat kept was of the epoch the agent followed, or a later one,
// when it came: a stale controller is refused as it answers. It takes
// heardMu for the heartbeat; the caller holds the agent's lock, under which
// the epoch the agent follows is read.
func (a *Agent) heardOf(node string) (time.Duration, time.Time, bool) {
	a.heardMu.Lock()
	defer a.heardMu.Unlock()
	if a.hb != nil {
		if c, _ := a.hb.Epoch.Order(a.remembered); c >= 0 {
			if fresh, ok := a.hb.Nodes[node]; ok {
				return fresh.StatusAge, a.hbAt, false
			}
		}
	}
	if n, ok := a.named[node]; ok {
		return n.statusAge, n.at, true
	}
	return 0, time.Time{}, false
}

// prove proves the address of t again outside a pass: as this node resolves
// it, or as it verifies the one another node chose.
func (a *Agent) prove(ctx context.Context, t agentproto.Target, inv inventory.Snapshot, prev *resolve.Binding, deny resolve.Denylist) resolve.Result {
	if t.Resolve {
		return a.d.Resolver.Resolve(ctx, t.Route, inv, prev, deny, resolve.Level(t.Required), nil)
	}
	return a.verify(ctx, t, inv, prev, deny).Result
}
