package engine

import (
	"net/netip"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// learnNodeAddrs adds the node addresses of the snapshot to the ones known,
// which start with those saved before. Only a cycle that goes on saves them.
func (c *cycleRun) learnNodeAddrs() bool {
	a := &c.e.addrs
	if !a.loaded {
		saved, err := c.e.d.Store.NodeAddrs()
		if err != nil {
			c.hold(c.problem("reading the saved node addresses: %v", err))
			return false
		}
		a.list, a.loaded = saved, true
	}
	merged := slices.Compact(slices.SortedFunc(slices.Values(append(slices.Clone(a.list), c.snap.NodeAddrs()...)), netip.Addr.Compare))
	if len(merged) != len(a.list) {
		a.list, a.dirty = merged, true
	}
	return true
}

// resolveTargets resolves the address of every winner, with the binding of
// its hostname, takes the addresses that lost their proof out of the egress
// filter, holds back what is proven below the identity minimum and saves the
// bindings. A cycle whose context ends in the middle plans and changes
// nothing.
func (c *cycleRun) resolveTargets() bool {
	c.results = c.resolveAll(c.bindings, c.deny)
	if err := c.ctx.Err(); err != nil {
		c.hold(c.problem(problemStoppedResolve, err))
		return false
	}
	c.blockLost()
	c.holdBelowMinimum()
	c.watch()

	next := make(map[string]resolve.Binding, len(c.results))
	for host, res := range c.results {
		if res.Binding != nil {
			next[host] = *res.Binding
		}
	}
	if err := c.e.d.Store.SaveBindings(next); err != nil {
		c.problem("saving the bindings: %v", err)
	}
	if c.e.addrs.dirty {
		if err := c.e.d.Store.SaveNodeAddrs(c.e.addrs.list); err != nil {
			c.problem("saving the node addresses: %v", err)
		} else {
			c.e.addrs.dirty = false
		}
	}
	return true
}

// resolveAll runs Resolve for each winner, at most resolveConcurrency at a
// time and each with a deadline of its own, so that one guest that reports
// many addresses cannot hold up the others.
func (c *cycleRun) resolveAll(stored map[string]resolve.Binding, deny resolve.Denylist) map[string]resolve.Result {
	winners := c.claims.Winners
	out := make([]resolve.Result, len(winners))
	slots := make(chan struct{}, resolveConcurrency)
	done := make(chan struct{})
	started := 0
	for i, rt := range winners {
		select {
		case slots <- struct{}{}:
		case <-c.ctx.Done():
		}
		if c.ctx.Err() != nil {
			break
		}
		var prev *resolve.Binding
		if b, ok := stored[rt.Hostname]; ok {
			prev = &b
		}
		started++
		go func() {
			defer func() { <-slots; done <- struct{}{} }()
			ctx, cancel := c.e.timeout(c.ctx, resolveTimeout)
			defer cancel()
			out[i] = c.e.d.Resolver.Resolve(ctx, rt, c.snap, prev, deny)
		}()
	}
	for range started {
		<-done
	}
	results := make(map[string]resolve.Result, len(winners))
	for i, rt := range winners[:started] {
		results[rt.Hostname] = out[i]
	}
	return results
}
