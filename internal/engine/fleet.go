package engine

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// NodeReport is the last status the agent of a node reported, as the
// controller received it.
type NodeReport struct {
	Status     agentproto.Status
	ReceivedAt time.Time
	// Fresh says that the status is recent enough to go by.
	Fresh bool
}

// Fleet is the agents of the nodes of the install, through which the engine
// does everything it does on a node: resolve the targets, give the egress
// filter its set, keep the connectors.
type Fleet interface {
	// Local is the node the engine runs on.
	Local() string
	// Publish hands node its snapshot. The local agent applies it before
	// Publish returns; a remote one later.
	Publish(ctx context.Context, node string, s agentproto.Snapshot) error
	// Reports returns the last report of every node; with wait > 0 it waits
	// up to wait for each node named in seq to report that seq or a later one.
	Reports(ctx context.Context, wait time.Duration, seq map[string]uint64) map[string]NodeReport
}

// LocalFleet is the fleet of one node: the agent of the node the engine runs
// on, in the same process. The engine runs its passes in steps, inside its
// cycle (see agent.Steps), so that the node serves what the engine decides at
// the point of the cycle it decides it; Publish applies a snapshot whole, as
// an agent on another node does.
//
// The agent is made once the install is known, of the install, and anew when
// the install changes, as after pco setup on a node set up before.
type LocalFleet struct {
	deps agent.Deps

	mu       sync.Mutex
	a        *agent.Agent
	install  string
	watching bool
	attached bool
}

var errNoAgent = errors.New("the agent of this node is not running: the install is not known yet")

// NewLocalFleet returns the fleet of the node d names. The install, the kind
// of the node and the hooks of the engine (events, changes, timers) are the
// engine's to give; what d says of them is replaced.
func NewLocalFleet(d agent.Deps) *LocalFleet {
	return &LocalFleet{deps: d}
}

// Local is the node of the agent.
func (f *LocalFleet) Local() string { return f.deps.Node }

// Publish has the agent apply s whole, as one on another node applies a
// snapshot it receives.
func (f *LocalFleet) Publish(ctx context.Context, node string, s agentproto.Snapshot) error {
	if node != f.deps.Node {
		return fmt.Errorf("node %s has no agent in this fleet: it is %s's", node, f.deps.Node)
	}
	a := f.made()
	if a == nil {
		return errNoAgent
	}
	_, err := agent.Local{A: a}.Publish(ctx, s)
	return err
}

// Reports returns the status of the agent of this node, which is always
// fresh: it reports in the same process.
func (f *LocalFleet) Reports(ctx context.Context, _ time.Duration, _ map[string]uint64) map[string]NodeReport {
	a := f.made()
	if a == nil {
		return map[string]NodeReport{}
	}
	st := a.Status(ctx)
	return map[string]NodeReport{f.deps.Node: {Status: st, ReceivedAt: st.Clock, Fresh: true}}
}

// attach gives the agents the hooks of e. The agent is made when it is first
// needed: by the first cycle, or by the daemon before it.
func (f *LocalFleet) attach(e *Engine) error {
	f.mu.Lock()
	switch {
	case f.attached:
		f.mu.Unlock()
		return errors.New("the fleet is attached to another engine")
	case f.deps.Resolver == nil:
		f.mu.Unlock()
		return errors.New("the agent has no resolver")
	case f.deps.Connectors == nil:
		f.mu.Unlock()
		return errors.New("the agent has no connector manager")
	case f.deps.Egress == nil:
		f.mu.Unlock()
		return errors.New("the agent has no egress filter")
	}
	f.attached = true
	f.deps.Node, f.deps.Store, f.deps.Now, f.deps.Log = e.d.Node, e.d.Store, e.d.Now, e.d.Log
	f.deps.Event = e.agentEvent
	f.deps.Changed = e.Trigger
	f.deps.After = func(d time.Duration) (<-chan time.Time, func() bool) { return e.after(d) }
	f.deps.Timeout = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		return e.timeout(ctx, d)
	}
	f.mu.Unlock()
	return nil
}

// made is the agent, made of the install the store names when there is none
// yet; nil while the store names none.
func (f *LocalFleet) made() *agent.Agent {
	if a := f.current(); a != nil {
		return a
	}
	f.mu.Lock()
	attached := f.attached
	f.mu.Unlock()
	if !attached {
		return nil
	}
	inst, found, err := f.deps.Store.Install()
	if err != nil || !found {
		return nil
	}
	a, err := f.agentOf(inst)
	if err != nil {
		f.deps.Log.Warn().Err(err).Msg("starting the agent of this node failed; the next cycle tries again")
		return nil
	}
	return a
}

// agentOf returns the agent of the install, made when there is none of it.
func (f *LocalFleet) agentOf(inst store.Install) (*agent.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.a != nil && f.install == inst.ID {
		return f.a, nil
	}
	d := f.deps
	d.InstallID, d.Kind = inst.ID, kindOf(inst)
	a, err := agent.New(d)
	if err != nil {
		return nil, err
	}
	if f.watching {
		a.Watching(true)
	}
	f.a, f.install = a, inst.ID
	return a, nil
}

// current is the agent, or nil before the install is known.
func (f *LocalFleet) current() *agent.Agent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.a
}

// watch notes whether the watch of the network runs, for the agent there is
// and for one made later.
func (f *LocalFleet) watch(on bool) {
	f.mu.Lock()
	f.watching = on
	a := f.a
	f.mu.Unlock()
	if a != nil {
		a.Watching(on)
	}
}

// kindOf is the kind of node an install's agent runs as.
func kindOf(inst store.Install) pki.Kind {
	if inst.ProfileName() == store.ProfileAppliance {
		return pki.KindAppliance
	}
	return pki.KindHost
}

// Agent is the agent of this node; nil while the store names no install.
func (e *Engine) Agent() *agent.Agent { return e.fleet.made() }

// agentEvent keeps an event of the agent of this node as one of the engine.
func (e *Engine) agentEvent(ev agent.Event) {
	e.events.add(Event{At: ev.At, Level: ev.Level, Kind: ev.Kind, Subject: ev.Subject, Message: ev.Message, Tunnel: ev.Tunnel})
}

// Watching tells the agent of this node whether the watch of the network
// runs. Until it does, and again after it stopped, every cycle proves every
// address anew.
//
// The daemon says so as it starts the watch, before the watch has subscribed
// to the tables of the kernel. That is sound: the watch reads both tables
// whole when it subscribes, and again whenever it subscribes anew, so what
// moved before then is reported then; and a proof stands only when the watch
// ran, and had the address pinned, before the proof was made, so the first
// proof after the start of the watch is made anew in the next cycle.
func (e *Engine) Watching(on bool) { e.fleet.watch(on) }

// Bound returns the pins of the addresses the last cycle served on a binding,
// for the watch of the network, by the address the egress set holds for each:
// the service address of a target on a leg. The map is not changed
// afterwards.
func (e *Engine) Bound() map[netip.Addr]egress.Pin {
	a := e.fleet.made()
	if a == nil {
		return nil
	}
	return a.Bound()
}

// Moved has the agent of this node take an address whose MAC moved out of
// the egress filter at once, and verify its routes again as soon as no cycle
// runs, at most once every few seconds: once that passes, the address comes
// back; when it fails, the address stays out and a cycle is asked for, which
// withdraws its routes. The address is one Bound gave. Moved does not wait
// for the verification; Run returns only once it ended.
func (e *Engine) Moved(ctx context.Context, addr netip.Addr) {
	a := e.fleet.made()
	if a == nil {
		return
	}
	e.moving.Add(1)
	a.Moved(ctx, addr)
	go func() {
		defer e.moving.Done()
		a.Wait()
	}()
}
