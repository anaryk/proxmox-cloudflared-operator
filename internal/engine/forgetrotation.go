package engine

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	problemForgetRotation = "node %s was forgotten: rotating the secret of tunnel %s in account %s failed: %v; " +
		"the run token the node holds may work until it is done, which is tried again in the next cycle"
	problemForgetHeld = "node %s was forgotten: the rotation of the shared tunnel's secret waits for a cycle that writes: %s; " +
		"until then the run token the node holds still works"
	problemStillConnects = "node %s still connects to tunnel %s in account %s with the run token it kept; " +
		"rotate the shared secret: pco tunnel rotate --account %s"
)

// forgottenConnector is a connector of a tunnel that a forgotten node
// reported.
type forgottenConnector struct{ node, tunnel, id string }

// forgetRotations finishes the rotations of the shared tunnel's secret that
// the forget of a node began and could not finish: a secret given once, then
// the connectors that remain taking the new token up and the node's dropped.
// The entry keeps the progress, so that a controller that takes over goes
// on; it is cleared once a cycle that read every account through saw every
// shared tunnel done.
func (c *cycleRun) forgetRotations() {
	if c.leaseHeld || c.storeHold {
		return
	}
	for _, name := range slices.Sorted(maps.Keys(c.nodes)) {
		if n := c.nodes[name].Node; n.State == store.NodeRemoved && n.Rotation != nil {
			c.forgetRotation(name)
		}
	}
}

// noLostConnection says that Cloudflare lists no connector of a shared
// tunnel the forgotten node may still run: none of its ids, and none that
// no node still in the install owns and that started before the secret was
// given. One that is listed is dropped again, and the rotation stays until
// a later listing shows none.
func (c *cycleRun) noLostConnection(name string, rot store.ForgetRotation) bool {
	clean := true
	for _, t := range c.tunnels {
		if t.Kind == TunnelPinned || !t.Exists || t.ID == "" {
			continue
		}
		v, err := c.e.lostConnectors(c.ctx, t, rot.Connectors[t.ID], rot.At)
		if err == nil && len(v.lost) == 0 {
			// A connector that started with the new secret and that no
			// node owns yet keeps the rotation until a node reports it.
			clean = clean && len(v.waiting) == 0
			continue
		}
		clean = false
		if err == nil {
			err = c.e.dropLost(c.ctx, t, v.lost, rot.At)
		}
		if err != nil {
			c.problem(problemForgetRotation, name, t.Name, t.AccountID, err)
		}
	}
	return clean
}

func (c *cycleRun) forgetRotation(name string) {
	if why := c.rotationHeld(); why != "" {
		c.problem(problemForgetHeld, name, why)
		return
	}
	entry := c.nodes[name]
	rot := *entry.Node.Rotation
	save := func() error {
		n := entry.Node
		n.Rotation = &rot
		rev, err := c.e.d.Store.SaveNodeIf(n, entry.Rev)
		if err != nil {
			return err
		}
		entry = store.NodeRev{Node: n, Rev: rev}
		c.nodes[name] = entry
		return nil
	}
	finished := c.checked && len(c.unreported) == 0
	for _, t := range c.tunnels {
		if t.Kind == TunnelPinned || !t.Exists || t.ID == "" {
			continue
		}
		if err := c.e.rotateFor(c.ctx, t, &rot.TunnelsRotated, save); err != nil {
			c.problem(problemForgetRotation, name, t.Name, t.AccountID, err)
			finished = false
		}
	}
	if !finished || !c.noLostConnection(name, rot) {
		return
	}
	n := entry.Node
	n.Rotation = nil
	rev, err := c.e.d.Store.SaveNodeIf(n, entry.Rev)
	if err != nil {
		c.problem("saving the registry entry of node %s: %v", name, err)
		return
	}
	c.nodes[name] = store.NodeRev{Node: n, Rev: rev}
	c.events = append(c.events, Event{At: c.now, Level: levelWarn, Kind: kindNode, Subject: name, Node: name, Security: true,
		Message: fmt.Sprintf("node %s was forgotten: the secret of the shared tunnel is rotated in every account, and the run token the node holds no longer works", name)})
}

// dropForgotten drops at Cloudflare, connector by connector, the connectors
// a node forgotten without a rotation of the shared secret reported as its
// own: its run token still works, and its agent stands down by itself, but
// one that does not, or cannot read that it was removed, keeps a share of
// the requests. The connectors of a tunnel are listed for it at most every
// rolloutAskEvery, and an id a listing no longer shows is not looked for
// again: a connector that starts anew has another one.
func (c *cycleRun) dropForgotten() {
	if c.leaseHeld || c.fleet == nil || c.cfHold || !c.checked || c.mode() != reconcile.Enforce {
		return
	}
	// A rotation of every secret drops the connectors of the nodes it is
	// for itself, as each tunnel gets its new secret.
	if _, rotating, err := c.e.d.Store.CARotation(); err != nil || rotating {
		return
	}
	mem := &c.e.place
	for _, name := range slices.Sorted(maps.Keys(mem.ids)) {
		entry, ok := c.nodes[name]
		if !ok || entry.Node.State != store.NodeRemoved || entry.Node.Rotation != nil {
			continue
		}
		for _, t := range c.tunnels {
			if t.Kind == TunnelPinned || !t.Exists || t.ID == "" || len(mem.ids[name][t.ID]) == 0 {
				continue
			}
			c.dropOf(name, t)
			if c.stillConnects(name, t) {
				c.problem(problemStillConnects, name, t.Name, t.AccountID, t.AccountID)
			}
		}
	}
}

// stillConnects says whether a connector of a tunnel that a forgotten node
// reported came back after it was dropped: its run token works, and only a
// rotation of the secret ends it.
func (c *cycleRun) stillConnects(node string, t reconcile.TunnelState) bool {
	for id := range c.e.place.ids[node][t.ID] {
		if c.e.place.forgottenDrops[forgottenConnector{node, t.ID, id}] >= 2 {
			return true
		}
	}
	return false
}

// dropOf drops the connectors of a tunnel that a forgotten node reported.
func (c *cycleRun) dropOf(node string, t reconcile.TunnelState) {
	mem := &c.e.place
	if mem.forgottenAsked == nil {
		mem.forgottenAsked = map[string]time.Time{}
	}
	if last, ok := mem.forgottenAsked[t.ID]; ok && !c.now.Before(last) && c.now.Sub(last) < rolloutAskEvery {
		return
	}
	api := c.e.clients[t.CredentialID]
	if api == nil {
		return
	}
	mem.forgottenAsked[t.ID] = c.now
	listed, err := api.Connectors(c.ctx, t.AccountID, t.ID)
	if err != nil {
		c.problem("node %s was forgotten: listing the connectors of tunnel %s in account %s failed: %v; its own are dropped once a listing shows them",
			node, t.Name, t.AccountID, err)
		return
	}
	if mem.forgottenDrops == nil {
		mem.forgottenDrops = map[forgottenConnector]int{}
	}
	ids := mem.ids[node][t.ID]
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		key := forgottenConnector{node, t.ID, id}
		if !slices.ContainsFunc(listed, func(x cfapi.Connector) bool { return x.ID == id }) {
			delete(ids, id)
			delete(mem.forgottenDrops, key)
			continue
		}
		if err := api.CleanUpConnector(c.ctx, t.AccountID, t.ID, id); err != nil {
			c.problem("node %s was forgotten: dropping its connector %s of tunnel %s in account %s failed: %v; it is tried again",
				node, id, t.Name, t.AccountID, err)
			continue
		}
		// It comes back with the same id while its token works: the event
		// is said once, and a drop after it makes the problem line.
		if mem.forgottenDrops[key]++; mem.forgottenDrops[key] > 1 {
			continue
		}
		c.events = append(c.events, Event{At: c.now, Level: levelWarn, Kind: kindNode, Subject: node, Node: node, Tunnel: t.Name, Account: t.AccountID,
			Security: true, Message: fmt.Sprintf("node %s was forgotten: its connector %s of tunnel %s in account %s is dropped; "+
				"its run token still works, as cluster.rotateOnForget is off", node, id, t.Name, t.AccountID)})
	}
}

// notOwned takes the connector ids the nodes removed from the install or
// contested reported out of the ones the install owns: a connector of
// theirs is not one of the install's, for a rollout or a rogue connector,
// from the cycle on that sees them so.
func (c *cycleRun) notOwned() {
	for node, byTunnel := range c.e.place.ids {
		n, ok := c.nodes[node]
		if !ok || n.Node.State != store.NodeRemoved && n.Node.State != store.NodeContested {
			continue
		}
		for tunnel, ids := range byTunnel {
			for id := range ids {
				delete(c.e.ownIDs[tunnel], id)
			}
		}
	}
}
