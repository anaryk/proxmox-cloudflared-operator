package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

// ErrDoctorBusy is an ask of the agents that could not start within its
// wait: another ran all along.
var ErrDoctorBusy = errors.New("another ask of the agents was running")

// doctorAsks are the actions the controller hands the agents of the other
// nodes to answer at once, doctor and probe actions, by id, until they are
// answered or the asker stops waiting.
type doctorAsks struct {
	// one is held by the ask that runs: one at a time hands the agents
	// actions, so that the agents cannot be made to flood.
	one  chan struct{}
	mu   sync.Mutex
	n    uint64
	acts map[string]*doctorAsk
}

type doctorAsk struct {
	node   string
	action agentproto.Action
	answer *agentproto.ActionResult
	done   chan struct{} // closed once answered
}

// AskDoctor hands the agent of each node a doctor action, has a cycle put
// it in their snapshots, and waits for at most wait for the answers. It
// returns, by node, the answers that came; a node that did not answer in
// time is not in it, and its action is taken back. One ask runs at a time:
// another waits for it within its own wait, and is ErrDoctorBusy when it
// could not start.
func (e *Engine) AskDoctor(ctx context.Context, nodes []string, wait time.Duration) (map[string]agentproto.ActionResult, error) {
	args := make(map[string]json.RawMessage, len(nodes))
	for _, node := range nodes {
		args[node] = json.RawMessage(`{}`)
	}
	return e.askAgents(ctx, agentproto.ActionDoctor, args, wait)
}

// askAgents hands the agent of each node of args an action of kind with its
// arguments, as AskDoctor does.
func (e *Engine) askAgents(ctx context.Context, kind string, args map[string]json.RawMessage, wait time.Duration) (map[string]agentproto.ActionResult, error) {
	d := &e.asks
	ctx, cancel := e.timeout(ctx, wait)
	defer cancel()
	out := map[string]agentproto.ActionResult{}
	// A free slot is taken at once; only a taken one is waited for.
	select {
	case d.one <- struct{}{}:
	default:
		select {
		case d.one <- struct{}{}:
		case <-ctx.Done():
			return out, ErrDoctorBusy
		}
	}
	defer func() { <-d.one }()
	d.mu.Lock()
	if d.acts == nil {
		d.acts = map[string]*doctorAsk{}
	}
	mine := map[string]*doctorAsk{}
	for _, node := range slices.Sorted(maps.Keys(args)) {
		d.n++
		id := fmt.Sprintf("%s-%s-%d", strings.ToLower(kind), e.boot, d.n)
		a := &doctorAsk{node: node, action: agentproto.Action{ID: id, Kind: kind, Args: args[node]}, done: make(chan struct{})}
		d.acts[id], mine[node] = a, a
	}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, a := range mine {
			delete(d.acts, a.action.ID)
		}
	}()
	e.Trigger()
	for _, node := range slices.Sorted(maps.Keys(mine)) {
		select {
		case <-mine[node].done:
		case <-ctx.Done():
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for node, a := range mine {
		if a.answer != nil {
			out[node] = *a.answer
		}
	}
	return out, nil
}

// actionsFor are the actions asked of node it has not answered.
func (d *doctorAsks) actionsFor(node string) []agentproto.Action {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []agentproto.Action
	for _, id := range slices.Sorted(maps.Keys(d.acts)) {
		if a := d.acts[id]; a.node == node && a.answer == nil {
			out = append(out, a.action)
		}
	}
	return out
}

// take keeps the answers to the actions asked that a status of node
// carries. An answer of another node's action is not taken: an answer is
// the word of the node whose agent sent the status, of that node alone.
func (d *doctorAsks) take(node string, results []agentproto.ActionResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, res := range results {
		a, ok := d.acts[res.ID]
		if !ok || a.node != node || a.answer != nil {
			continue
		}
		answer := res
		a.answer = &answer
		close(a.done)
	}
}
