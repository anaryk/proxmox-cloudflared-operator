package agent

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
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

// Boot applies, on a node that just booted, the desired state the agent kept:
// the egress filter starts empty and a target enters it once it verifies, the
// connectors of pinned tunnels start at once, and the shared ones wait at the
// boot gate. The gateway came back from its cache before, with the unit that
// restores it. A connector that runs, as a shared unit still enabled at boot
// on a node alone, is not stopped.
func (a *Agent) Boot(ctx context.Context) error {
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	a.boot = bootGate{held: true, at: a.d.Now()}
	a.set = nil
	if a.desired == nil || a.stood != "" {
		a.republish()
		return nil
	}
	p := a.newPass(ctx, *a.desired)
	p.run()
	return p.err
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
