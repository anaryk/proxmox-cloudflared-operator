package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
)

// bootGate holds the shared connectors of a node that booted until it may
// start them: when a controller marks the node active, or, when none answered
// within bootWait of boot, once a shared target verified or the desired state
// lists none.
type bootGate struct {
	held     bool
	at       time.Time
	answered bool // a controller sent a desired state since boot
}

var (
	gateActive = "the shared connector waits until the controller marks this node active"
	gateBoot   = fmt.Sprintf("the shared connector waits for the controller to mark this node active; "+
		"if none answers within %s of boot, the first shared target that verifies starts it", bootWait)
	gateTarget = fmt.Sprintf("the shared connector waits: no controller answered within %s of boot, and no shared target verifies", bootWait)
)

// Boot applies, as the daemon starts, the desired state the agent kept: the
// connectors of pinned tunnels start at once, the shared ones wait at the
// boot gate, and the actions kept that were not run yet run, each once. On
// a node that booted the egress filter is empty: the set the agent kept is
// dropped, and a target enters the filter once it verifies. When the filter
// holds targets the daemon alone was restarted: the set is kept, and what it
// holds goes on being served. A table that cannot be read is taken to hold
// nothing. With no desired state that can be read the agent can verify
// nothing: the filter is emptied, and the connectors reach nothing until
// the controller sends one. The gateway came back from its cache before,
// with the unit that restores it. A connector that runs, as a shared unit
// still enabled at boot on a node alone, is not stopped.
func (a *Agent) Boot(ctx context.Context) error {
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	a.boot = bootGate{held: true, at: a.d.Now()}
	live, err := a.d.Egress.Live(ctx)
	if err != nil || len(live.Targets) == 0 {
		a.set = nil
	}
	if err != nil && !errors.Is(err, egress.ErrNotLoaded) {
		a.d.Log.Warn().Err(err).Msg("the egress table could not be read at start; the set kept is dropped, and a target enters the filter as it verifies")
	}
	if a.desired == nil || a.stood != "" {
		a.emptyFilter(ctx)
		if a.unreadable != "" {
			a.event(levelWarn, kindEgress, a.d.Node, "the desired state kept on this node cannot be read; the connectors reach nothing until the controller sends one")
		}
		a.republish()
		return nil
	}
	p := a.newPass(ctx, *a.desired)
	p.runActions = true
	p.run()
	return p.err
}

// emptyFilter gives the egress filter nothing and keeps that. The caller
// holds the agent's lock.
func (a *Agent) emptyFilter(ctx context.Context) {
	a.set = nil
	a.egMu.Lock()
	err := a.d.Egress.Set(ctx, nil)
	a.egMu.Unlock()
	if err != nil && !errors.Is(err, egress.ErrOff) {
		a.d.Log.Warn().Err(err).Msg("emptying the egress filter failed; the next pass sets it")
	}
	if err := a.saveSet(); err != nil {
		a.d.Log.Warn().Err(err).Msg("saving the egress set failed; the next pass saves it")
	}
}

// gate reports whether the boot gate holds the shared connectors in this
// pass, and opens it when it may.
func (p *pass) gate() bool {
	a := p.a
	g := &a.boot
	if !g.held {
		return false
	}
	var opened string
	switch {
	case g.answered && p.s.State == agentproto.Active:
		opened = "the controller marks this node active; the shared connector starts"
	case g.answered || a.heardSince(g.at):
		p.problem("%s", gateActive)
		return true
	case p.now.Sub(g.at) < bootWait:
		p.problem("%s", gateBoot)
		return true
	case p.sharedVerified():
		opened = fmt.Sprintf("no controller answered within %s of boot and a shared target verified; the shared connector starts", bootWait)
	case !p.sharedListed():
		opened = fmt.Sprintf("no controller answered within %s of boot and no shared target is listed; the shared connector starts", bootWait)
	default:
		p.problem("%s", gateTarget)
		return true
	}
	g.held = false
	a.event(levelInfo, kindConnector, a.d.Node, opened)
	return false
}

// sharedTunnels are the ids of the shared tunnels of the snapshot.
func (p *pass) sharedTunnels() []string {
	var out []string
	for _, t := range p.s.Tunnels {
		if t.Kind == agentproto.TunnelShared {
			out = append(out, t.TunnelID)
		}
	}
	return out
}

func (p *pass) sharedListed() bool {
	shared := p.sharedTunnels()
	return slices.ContainsFunc(p.s.Targets, func(t agentproto.Target) bool {
		return slices.ContainsFunc(t.Tunnels, func(id string) bool { return slices.Contains(shared, id) })
	})
}

func (p *pass) sharedVerified() bool {
	shared := p.sharedTunnels()
	return slices.ContainsFunc(p.s.Targets, func(t agentproto.Target) bool {
		return p.verified[t.Key] && slices.ContainsFunc(t.Tunnels, func(id string) bool { return slices.Contains(shared, id) })
	})
}
