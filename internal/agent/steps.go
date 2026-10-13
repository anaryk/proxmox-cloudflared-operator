package agent

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Steps is a pass that the engine of the same process runs one step at a
// time, inside its own cycle. The agent resolves the targets; the engine then
// decides which of them it serves, as it does with every proof it judges (the
// observed rules, the minimum, the isolation of the managed network, the
// service addresses of the legs); only then does the agent give the egress
// filter its set; and after the engine's tunnel run it keeps the connectors.
// A node of its own thus serves exactly what its engine decided, with each
// change made once, at the point of the cycle it was made at before the agent
// did it.
//
// A pass of steps holds the agent's lock from Begin to End: a verification of
// an address whose MAC moved waits for it, as it waits for any pass.
type Steps struct {
	a *Agent
	p *pass
	// fed and finished say that the pass gave the filter its set and kept
	// the connectors; ended that the lock was let go.
	fed, finished, ended bool
}

// Resolution is what Steps.Resolve found.
type Resolution struct {
	// Results are the resolver's, by target key.
	Results map[string]resolve.Result
	// Lost are the addresses whose proof was lost in this pass, or that a
	// block names: they left the egress filter at once, and stay out of the
	// set of the pass.
	Lost []netip.Addr
	// Problems are what taking them out of the filter found.
	Problems []string
}

// Begin starts a pass of s for the engine of this process; it waits for the
// agent's lock until ctx ends. s is neither checked nor kept: Finish keeps
// the snapshot the pass ends with.
func (a *Agent) Begin(ctx context.Context, s agentproto.Snapshot) (*Steps, error) {
	if err := a.acquire(ctx); err != nil {
		return nil, err
	}
	a.hear()
	return &Steps{a: a, p: a.newPass(ctx, s)}, nil
}

// Resolve resolves every target of the snapshot this node resolves, with the
// binding stored for its hostname, takes the addresses blocked and those whose
// proof was lost out of the egress filter at once, and returns what the
// resolver found. The error says that the bindings could not be read, or
// that the pass's context ended while it resolved: nothing that follows may
// change anything then.
func (st *Steps) Resolve() (Resolution, error) {
	p := st.p
	from := len(p.problems)
	p.expect()
	p.takeOutBlocked()
	if !p.load() {
		return Resolution{}, p.err
	}
	p.resolveAll()
	p.verifyAll()
	if err := p.ctx.Err(); err != nil {
		p.err = err
		p.problem(problemPartly, err)
		return Resolution{}, err
	}
	p.blockLost()
	out := Resolution{Results: make(map[string]resolve.Result, len(p.results)), Problems: slices.Clone(p.problems[from:])}
	for key, res := range p.results {
		out.Results[key] = cloneResult(res)
	}
	for addr := range p.lost {
		out.Lost = append(out.Lost, addr)
	}
	for addr := range p.blocked {
		if !p.lost[addr] {
			out.Lost = append(out.Lost, addr)
		}
	}
	slices.SortFunc(out.Lost, netip.Addr.Compare)
	return out, nil
}

// TakeOut takes addr out of the egress filter at once, when the set holds it,
// and keeps it out of the set of this pass: the engine has a service address
// forward nothing while the route it was made for left its address.
func (st *Steps) TakeOut(addr netip.Addr) error {
	p := st.p
	if p.lost[addr] {
		return nil
	}
	p.lost[addr] = true
	if !slices.ContainsFunc(st.a.set, func(t egress.Target) bool { return t.Addr == addr }) {
		return nil
	}
	return st.a.d.Egress.Remove(p.ctx, addr)
}

// serve takes what the engine serves of the targets: by target key, each
// result as it judged it.
func (st *Steps) serve(served map[string]resolve.Result) {
	p := st.p
	p.served = make(map[string]resolve.Result, len(served))
	for key, res := range served {
		p.served[key] = cloneResult(res)
	}
}

// EgressSet is the set the pass gives the filter when the engine serves
// served: the targets it verified and serves, and those of the set kept that
// a configuration verified at Cloudflare still sends a connector to, unless
// withdrawn, less the addresses lost.
func (st *Steps) EgressSet(served map[string]resolve.Result) []egress.Target {
	st.serve(served)
	return st.p.egressSet()
}

// Bind takes what the engine serves of the targets, has the watch of the
// network watch every address served on a binding, and keeps the binding of
// every target. legs name the interface a pin of a target on a leg names, by
// network. It returns what keeping the bindings found.
func (st *Steps) Bind(served map[string]resolve.Result, legs []gateway.Leg) []string {
	p := st.p
	st.serve(served)
	p.cache.Legs = legs
	p.judge()
	p.watch()
	from := len(p.problems)
	p.saveBindings()
	return slices.Clone(p.problems[from:])
}

// Feed gives the egress filter the set of the pass, less the addresses whose
// MAC moved and that were not verified again yet, and keeps it. It returns
// the set and why the filter did not take it; a filter the admin switched off
// takes nothing and is no failure.
func (st *Steps) Feed() ([]egress.Target, error) {
	p := st.p
	set := p.egressSet()
	p.feedEgress(set)
	st.fed = true
	return set, p.setErr
}

// Finish ends the pass with s, the snapshot as the engine has it after its
// tunnel run. With keep, the agent keeps s on disk, as it keeps a snapshot it
// applies, when s is one it may follow, and runs its actions. The connectors
// follow the tunnels of s, and the status of every connector on the node is
// read. It returns the statuses and what keeping the connectors found.
func (st *Steps) Finish(s agentproto.Snapshot, keep bool) ([]connector.Status, []string) {
	p := st.p
	p.s = s
	taken := false
	if keep {
		taken = st.keep(s)
	}
	from := len(p.problems)
	p.connectors()
	lines := slices.Clone(p.problems[from:])
	if taken {
		p.actions()
	}
	st.finished = true
	return slices.Clone(p.conns), lines
}

// keep keeps s as the desired state the agent follows, when it may follow
// it, and reports whether it does.
func (st *Steps) keep(s agentproto.Snapshot) bool {
	a := st.a
	if err := a.accept(s); err != nil {
		a.refused = "refused a desired state: " + err.Error()
		a.d.Log.Warn().Err(err).Msg("refused a desired state")
		return false
	}
	if err := a.persist(s); err != nil {
		if errors.Is(err, store.ErrEpochBack) {
			a.refused = "refused a desired state: " + err.Error()
		} else {
			a.refused = "a desired state could not be kept: " + err.Error()
		}
		return false
	}
	a.refused, a.stood = "", ""
	return true
}

// End ends the pass: what it found is reported, and the agent's lock let go.
// Further calls do nothing.
func (st *Steps) End() {
	if st.ended {
		return
	}
	st.ended = true
	a, p := st.a, st.p
	a.repMu.Lock()
	if st.fed {
		a.rep.targets, a.rep.setOf = p.reports()
		a.rep.egress = p.egress
		a.rep.localBlocks = p.localBlocks
	}
	if st.finished {
		a.rep.conns = p.conns
		a.rep.actions = a.actionResults(p.s)
	}
	a.rep.problems = p.problems
	a.fill()
	a.repMu.Unlock()
	a.release()
}

// Token reads the run token the connector of a tunnel has on this node.
func (a *Agent) Token(tunnelID string) (string, bool, error) {
	return a.d.Connectors.Token(tunnelID)
}

// Restart starts the connector of a tunnel, or restarts it, with token: one
// the engine read again at Cloudflare, as after the tunnel's secret was
// rotated.
func (a *Agent) Restart(ctx context.Context, tunnelID, token string) error {
	return a.d.Connectors.Ensure(ctx, a.d.InstallID, tunnelID, token)
}

// Halt gives the egress filter nothing, while the set is kept for when the
// node serves again, and has no verification of a moved address put one
// back: the engine of an appliance that is a copy, or that a principal can
// reach into, serves nothing. A filter the admin switched off is no failure.
func (a *Agent) Halt(ctx context.Context) error {
	a.halted.Store(true)
	a.egMu.Lock()
	err := a.d.Egress.Set(ctx, nil)
	a.egMu.Unlock()
	if errors.Is(err, egress.ErrOff) {
		return nil
	}
	return err
}

// Resume ends a Halt: the appliance serves again.
func (a *Agent) Resume() { a.halted.Store(false) }

// StopConnectors stops every connector of the install and keeps their files,
// so that the next pass that keeps them starts them again.
func (a *Agent) StopConnectors(ctx context.Context) error {
	return a.d.Connectors.StopAll(ctx, a.d.InstallID)
}

// ProofDue reports whether a proof of addr made at at is to be made again at
// now although the watch of the network vouches for it: once in every
// interval of every, at a time the address takes from its hash.
func ProofDue(addr netip.Addr, at, now time.Time, every time.Duration) bool {
	return due(addr, at, now, every)
}
