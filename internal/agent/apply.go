package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const problemPartly = "the desired state was applied only in part: resolution ended (%v); the egress set and the connectors are left as they are"

// Apply takes a snapshot from the controller. It refuses one that is not for
// this node and install, of a lower epoch than the one the agent follows, or
// whose seq goes back within it, and says why in the status. One it takes it
// keeps on disk first, then resolves the targets this node chooses the
// address of, sets the egress set, keeps the connectors and runs the actions
// it has not run. ctx bounds the pass; each resolution is bounded on its own
// as well. The error says why the snapshot was refused, or why it was applied
// only in part.
func (a *Agent) Apply(ctx context.Context, s agentproto.Snapshot) (agentproto.Status, error) {
	if err := a.acquire(ctx); err != nil {
		return a.Status(ctx), err
	}
	defer a.release()
	if err := a.accept(s); err != nil {
		a.refused = "refused a desired state: " + err.Error()
		a.d.Log.Warn().Err(err).Msg("refused a desired state")
		a.republish()
		return a.Status(ctx), fmt.Errorf("refusing the desired state: %w", err)
	}
	if err := a.persist(s); err != nil {
		// An epoch the store refuses was moved under the agent: that is a
		// refusal, which the next snapshot of the epoch it follows clears.
		if errors.Is(err, store.ErrEpochBack) {
			a.refused = "refused a desired state: " + err.Error()
		} else {
			a.refused = "a desired state could not be kept, so it was not applied: " + err.Error()
		}
		a.republish()
		return a.Status(ctx), err
	}
	a.refused, a.stood = "", ""
	a.hear()
	if a.boot.held {
		a.boot.answered = true
	}
	p := a.newPass(ctx, s)
	p.runActions = true
	p.run()
	return a.Status(ctx), p.err
}

// accept checks a snapshot before anything of it is kept.
func (a *Agent) accept(s agentproto.Snapshot) error {
	if err := s.Validate(a.d.Node, a.remembered, a.applied); err != nil {
		return err
	}
	for _, m := range []map[string]string{s.Deny.Reasons, s.Deny.Soft} {
		if _, err := addrMap(m); err != nil {
			return err
		}
	}
	for _, t := range s.Targets {
		switch resolve.Level(t.Required) {
		case resolve.LevelObserved, resolve.LevelFiltered, resolve.LevelPort:
		default:
			if t.Route.Guest != nil {
				return fmt.Errorf("target %s requires the level %q, which is none", t.Key, t.Required)
			}
		}
	}
	return nil
}

// Reverify applies the desired state the agent follows again, as the
// controller left it: every target is resolved again and the egress set and
// the connectors follow. The agent does so every poll interval while no new
// snapshot comes. It runs no action. A node that stood down does nothing.
func (a *Agent) Reverify(ctx context.Context) (agentproto.Status, error) {
	if err := a.acquire(ctx); err != nil {
		return a.Status(ctx), err
	}
	defer a.release()
	if a.desired == nil || a.stood != "" {
		a.republish()
		return a.Status(ctx), nil
	}
	p := a.newPass(ctx, *a.desired)
	p.run()
	return a.Status(ctx), p.err
}

// pass is one application of a desired state.
type pass struct {
	a   *Agent
	ctx context.Context
	s   agentproto.Snapshot
	now time.Time
	// runActions says that the actions of s not run yet are run: the
	// controller sent s for this pass, or the agent kept it when it last
	// stopped and the node boots.
	runActions bool

	inv      inventory.Snapshot
	deny     resolve.Denylist
	cache    gateway.Cache // the legs and sticky entries of s
	networks bool          // s gives the networks of the node, and they could be read
	bindings map[string]resolve.Binding
	// results are the resolver's, by target key; served are the same after
	// the minimum held back what is proven below it and the targets on a leg
	// were given their service addresses.
	results     map[string]resolve.Result
	served      map[string]resolve.Result
	held        map[string]string // why a target is held back, by key
	lost        map[netip.Addr]bool
	moved       map[string]bool     // the hostnames whose guest this pass found elsewhere than their stored binding
	blocked     map[netip.Addr]bool // the blocks in force, of the whole install
	verified    map[string]bool     // the target keys that verified
	attested    map[string]string   // the target keys proven at port through another node's attestation, to that node
	conns       []connector.Status
	egress      string       // how the egress filter took the set
	setErr      error        // why the egress filter did not take it
	localBlocks []netip.Addr // blocked on this node alone, which the filter refuses
	problems    []string
	err         error
	done        bool // the pass got to its end
}

func (a *Agent) newPass(ctx context.Context, s agentproto.Snapshot) *pass {
	return &pass{
		a: a, ctx: ctx, s: s, now: a.d.Now(),
		results: map[string]resolve.Result{}, served: map[string]resolve.Result{}, held: map[string]string{},
		lost: map[netip.Addr]bool{}, moved: map[string]bool{}, blocked: map[netip.Addr]bool{}, verified: map[string]bool{},
		attested: map[string]string{},
	}
}

func (p *pass) problem(format string, args ...any) {
	p.problems = append(p.problems, fmt.Sprintf(format, args...))
}

// run goes through the steps of a pass in order. A pass whose context ends
// while it resolves changes nothing after that, as a cycle of the engine.
func (p *pass) run() {
	defer p.a.publish(p)
	p.expect()
	p.takeOutBlocked()
	if !p.load() {
		return
	}
	p.resolveAll()
	p.verifyAll()
	if err := p.ctx.Err(); err != nil {
		p.err = err
		p.problem(problemPartly, err)
		return
	}
	p.blockLost()
	p.freezeMoved()
	p.filtered()
	p.holdBelowMinimum()
	p.markLegs()
	p.judge()
	set := p.egressSet()
	p.syncGateway(set)
	p.watch()
	p.saveBindings()
	p.feedEgress(set)
	p.connectors()
	if p.runActions {
		p.actions()
	}
	p.done = true
}

// load reads the bindings and makes what resolution reads of the snapshot.
// A pass that cannot read the bindings changes nothing.
func (p *pass) load() bool {
	bindings, err := p.a.d.Store.Bindings()
	if err != nil {
		p.err = err
		p.problem("reading the bindings: %v; the egress set and the connectors are left as they are", err)
		return false
	}
	p.bindings = bindings
	p.inv = inventoryOf(p.s)
	deny, err := denylistOf(p.s.Deny)
	if err != nil {
		p.err = err
		p.problem("%v", err)
		return false
	}
	p.deny = deny
	if len(p.s.Networks) > 0 {
		if err := json.Unmarshal(p.s.Networks, &p.cache); err != nil {
			p.problem("reading the networks of the desired state: %v; the gateway is left as it is", err)
			p.cache = gateway.Cache{}
		} else {
			p.networks = true
		}
	}
	return true
}
