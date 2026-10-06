package agent

import (
	"context"
	"fmt"
	"maps"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// interrupted is what an action is marked with before it runs: when the
// agent stops while it runs, that is its answer, and it is not run again.
const interrupted = "the agent stopped while it ran"

// actions runs the actions of the snapshot this node has not run, each at
// most once: it is marked done on disk before it runs and answered with its
// result after. The actions the snapshot no longer lists are forgotten.
func (p *pass) actions() {
	a := p.a
	next := maps.Clone(a.done)
	listed := map[string]bool{}
	for _, act := range p.s.Actions {
		listed[act.ID] = true
		if _, ran := next[act.ID]; ran || act.ID == "" || act.Kind == "" {
			continue
		}
		next[act.ID] = store.ActionDone{Kind: act.Kind, At: p.now, Error: interrupted}
		if err := a.saveDone(maps.Clone(next)); err != nil {
			p.problem("%v; action %s is not run", err, act.ID)
			delete(next, act.ID)
			continue
		}
		err := a.runAction(p.ctx, p.s, act)
		res := agentproto.ActionResult{ID: act.ID, OK: err == nil}
		if err != nil {
			res.Error = err.Error()
		}
		a.answers[act.ID] = res
		next[act.ID] = store.ActionDone{Kind: act.Kind, At: p.now, Error: res.Error}
		if err := a.saveDone(maps.Clone(next)); err != nil {
			p.problem("%v", err)
		}
	}
	maps.DeleteFunc(next, func(id string, _ store.ActionDone) bool { return !listed[id] })
	maps.DeleteFunc(a.answers, func(id string, _ agentproto.ActionResult) bool { return !listed[id] })
	if err := a.saveDone(next); err != nil {
		p.problem("%v", err)
	}
}

// runAction carries out one action.
func (a *Agent) runAction(ctx context.Context, s agentproto.Snapshot, act agentproto.Action) error {
	if act.Kind == agentproto.ActionPVEWrite {
		return a.pveWrite(ctx, s, act.Args)
	}
	return fmt.Errorf("this agent does not carry out %s actions", act.Kind)
}

// actionResults are the answers to the actions the snapshot lists that the
// agent ran: as this process answered them, or as kept on disk when an
// earlier one did.
func (a *Agent) actionResults(s agentproto.Snapshot) []agentproto.ActionResult {
	var out []agentproto.ActionResult
	for _, act := range s.Actions {
		if res, ok := a.answers[act.ID]; ok {
			out = append(out, res)
			continue
		}
		if d, ok := a.done[act.ID]; ok {
			out = append(out, agentproto.ActionResult{ID: act.ID, OK: d.Error == "", Error: d.Error})
		}
	}
	return out
}
