package engine

import (
	"fmt"
	"maps"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	problemForgetRotation = "node %s was forgotten: rotating the secret of tunnel %s in account %s failed: %v; " +
		"the run token the node holds may work until it is done, which is tried again in the next cycle"
	problemForgetHeld = "node %s was forgotten: the rotation of the shared tunnel's secret waits for a cycle that writes: %s; " +
		"until then the run token the node holds still works"
)

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
// tunnel the forgotten node may still run: none of its ids, and, when they
// are not known, none that no node still in the install owns. One that is
// listed is dropped again, and the rotation stays until a later listing
// shows none.
func (c *cycleRun) noLostConnection(name string, rot store.ForgetRotation) bool {
	clean := true
	for _, t := range c.tunnels {
		if t.Kind == TunnelPinned || !t.Exists || t.ID == "" {
			continue
		}
		lost, err := c.e.lostConnectors(c.ctx, t, rot.Connectors[t.ID])
		if err == nil && len(lost) == 0 {
			continue
		}
		clean = false
		if err == nil {
			err = c.e.dropLost(c.ctx, t, lost)
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
