package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// pveWriteWait is how long a write of a guest's card waits for the agent of
// the guest's node to answer before it is given up, and asked again in a
// later cycle.
const pveWriteWait = 2 * time.Minute

// pveWrites are the writes of the cards of guests of other nodes that the
// controller hands their agents as pveWrite actions, by action id, and the
// nodes whose agents take them: the host nodes that are Active and report.
type pveWrites struct {
	mu     sync.Mutex
	acts   map[string]*pveAct
	agents map[string]bool
	said   map[string]bool // what the managed step said of writes handed an agent
}

// pveAct is one write, the epoch it was issued under and the agent's answer.
type pveAct struct {
	node   string
	epoch  agentproto.Epoch
	action agentproto.Action
	at     time.Time
	answer *agentproto.ActionResult
}

// PVEWriter returns how the configurer of the guests' cards hands the agent
// of another node a write of a card of a guest there.
func (e *Engine) PVEWriter() managednet.Remote { return pveWriter{e} }

type pveWriter struct{ e *Engine }

func (w pveWriter) Agent(node string) bool {
	p := &w.e.writes
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.agents[node]
}

// Write hands w to the agent of node under the epoch of this process. A
// write with the same content under the same epoch is one action, which the
// agent runs once; one the agent answered is forgotten here, and returns the
// agent's error when it failed.
func (w pveWriter) Write(_ context.Context, node string, write agentproto.PVEWrite) error {
	e := w.e
	epoch := agentproto.Epoch{InstallID: e.us.InstallID, Generation: e.us.Generation, Nonce: e.us.Nonce}
	if epoch.Generation == 0 {
		return errors.New("no writer identity: no change is handed to an agent")
	}
	args, err := json.Marshal(write)
	if err != nil {
		return fmt.Errorf("writing the request: %w", err)
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%d %s %s %s", epoch.Generation, epoch.Nonce, node, args))
	id := "pvewrite-" + hex.EncodeToString(sum[:12])
	now := e.d.Now()
	p := &e.writes
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.acts == nil {
		p.acts = map[string]*pveAct{}
	}
	if act, ok := p.acts[id]; ok {
		switch {
		case act.answer != nil && !act.answer.OK:
			delete(p.acts, id)
			return errors.New(act.answer.Error)
		case act.answer != nil:
			delete(p.acts, id)
			return managednet.ErrSent
		case now.Sub(act.at) >= pveWriteWait:
			delete(p.acts, id)
			return fmt.Errorf("the agent of node %s did not answer within %s", node, pveWriteWait)
		}
		return managednet.ErrSent
	}
	p.acts[id] = &pveAct{node: node, epoch: epoch, at: now,
		action: agentproto.Action{ID: id, Kind: agentproto.ActionPVEWrite, Args: args}}
	return managednet.ErrSent
}

// agentNodes are the nodes whose agents take writes, as the last cycle
// found them.
func (p *pveWrites) agentNodes() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.agents)
}

// actionsFor are the writes for node issued under epoch, by id.
func (p *pveWrites) actionsFor(node string, epoch agentproto.Epoch) []agentproto.Action {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []agentproto.Action{}
	for _, id := range slices.Sorted(maps.Keys(p.acts)) {
		if act := p.acts[id]; act.node == node && act.epoch == epoch && act.answer == nil {
			out = append(out, act.action)
		}
	}
	return out
}

// note takes the agents' answers to the writes from their reports, each from
// the report of the node the write was for, and which nodes take writes:
// the host nodes that are Active and report fresh. Writes of another epoch
// are dropped.
func (p *pveWrites) note(fleet *fleetView, epoch agentproto.Epoch) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.agents = map[string]bool{}
	if fleet == nil {
		return
	}
	for name, f := range fleet.nodes {
		if f.heard && f.fresh && f.host && f.entry.State == store.NodeActive && f.report.Status.Kind == pki.KindHost {
			p.agents[name] = true
		}
	}
	for id, act := range p.acts {
		if act.epoch != epoch {
			delete(p.acts, id)
			continue
		}
		f := fleet.nodes[act.node]
		if f == nil || !f.heard || f.report.Status.EpochSeen != epoch {
			continue
		}
		for _, res := range f.report.Status.Actions {
			if res.ID == id {
				answer := res
				act.answer = &answer
			}
		}
	}
}

// sayOnce reports whether what is said under key was not said before, and
// notes it.
func (p *pveWrites) sayOnce(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.said == nil {
		p.said = map[string]bool{}
	}
	if p.said[key] {
		return false
	}
	p.said[key] = true
	return true
}
