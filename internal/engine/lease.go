package engine

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// Lease is the lease of the host profile, which makes one node the writer:
// *lease.Keeper.
type Lease interface {
	// Gate is the write gate: the writer to write as, or why not now.
	Gate(ctx context.Context) (planner.Writer, error)
	State() lease.State
	// ReportedEpoch takes the epoch an agent follows, and FirstWrite says
	// that the tenure wrote: an epoch leader.json does not explain makes the
	// controller behind before it, and stops the writes after it.
	ReportedEpoch(e agentproto.Epoch)
	FirstWrite()
	// Promote draws an epoch above seed, explicitly.
	Promote(ctx context.Context, seed int) error
}

const (
	problemNotHolder = "this node does not hold the lease; node %s is the controller"
	problemNoLease   = "this node does not hold the lease yet: %s; nothing is written"
	// problemLeaseFenced names a holder that may not write now.
	problemLeaseFenced = "the lease is fenced: %s; nothing is written"
	// problemQuorumStale is a members file not read for a while, which says
	// nothing of the quorum now.
	problemQuorumStale = "the quorum of the cluster is not known: %s; nothing is written"

	// quorumAge is the oldest read of the members file the engine goes by.
	quorumAge = 5 * time.Second
)

// leaseAdmits decides, on the host, whether this cycle may write anything at
// all, to the store, to Cloudflare or to Proxmox: the cluster is quorate by a
// recent read of the members file, the store belongs to this cluster, this
// node holds the lease, and the controller is not behind what the agents
// remember. A cycle it refuses holds where it is, before anything is saved,
// and says why. The appliance has no lease and is not asked.
func (c *cycleRun) leaseAdmits() bool {
	l := c.e.d.Lease
	if l == nil {
		return true
	}
	defer func() {
		st := l.State()
		c.st.Lease = leaseView(st, c.now, c.agentAddress)
		c.leaseEvent(st)
	}()
	if !c.quorumKnown() {
		c.e.dropClients()
		return false
	}
	st := l.State()
	if !st.Holder {
		c.e.dropClients()
		c.hold(c.notHolder(st))
		return false
	}
	if !c.sameCluster() {
		return false
	}
	c.startTenure(st)
	if err := c.e.storeWentBack(); err != nil {
		c.e.d.Log.Error().Err(err).Msg("the store went back under this writer")
		c.holdBehind(c.problem(problemStoreWentBack))
		return false
	}
	if !c.agentsReported() {
		return false
	}
	if st = l.State(); st.Behind != "" {
		c.e.d.Log.Error().Str("behind", st.Behind).Msg("this controller only observes until pco promote")
		c.holdBehind(c.problem(problemBehindHost))
		return false
	}
	w, err := l.Gate(c.ctx)
	if err != nil {
		if !l.State().Valid {
			c.e.dropClients()
		}
		c.hold(c.problem(problemLeaseFenced, err))
		return false
	}
	if c.writerRead && !sameWriterEpoch(w, c.e.us) {
		c.hold(c.problem(problemLeaseFenced, fmt.Sprintf("the lease is of epoch %d.%s, and leader.json names %d.%s",
			w.Generation, w.Nonce, c.e.us.Generation, c.e.us.Nonce)))
		return false
	}
	l.FirstWrite()
	c.recordCluster()
	return true
}

// quorumKnown holds the cycle while the last read of the members file does
// not say that the cluster is quorate now. Without quorum pmxcfs takes no
// write, and the lease is not refreshed: nothing at all is written, also in a
// cycle with nothing to save.
func (c *cycleRun) quorumKnown() bool {
	q := c.e.d.Quorum
	if q == nil {
		return true
	}
	ok, at := q()
	age := c.now.Sub(at)
	switch {
	case at.IsZero():
		c.hold(c.problem(problemQuorumStale, "the members file was not read yet"))
		return false
	case age > quorumAge || age < -quorumAge:
		c.hold(c.problem(problemQuorumStale, fmt.Sprintf("the members file was last read %s ago", age.Round(time.Second))))
		return false
	case !ok:
		c.hold(c.problem(problemNotQuorate))
		return false
	}
	return true
}

// notHolder is the line of a node that does not hold the lease.
func (c *cycleRun) notHolder(st lease.State) string {
	rec, _, found, err := c.e.d.Store.Leader()
	if err == nil && found && rec.Node != "" && rec.Node != c.e.d.Node && rec.ReleasedAt.IsZero() {
		return c.problem(problemNotHolder, rec.Node)
	}
	why := st.Waiting
	if why == "" {
		why = "the lease keeper has not tried yet"
	}
	return c.problem(problemNoLease, why)
}

// holdBehind holds the cycle of a controller that only observes until pco
// promote.
func (c *cycleRun) holdBehind(line string) {
	c.st.Mode = ModeObserve
	c.hold(line)
}

// leaseView is what the state shows of the lease; addrOf gives the agent
// address a node is registered with. A node that does not hold the lease
// names the holder as leader.json was last read, with its generation and
// address, so that whoever asks this node can be sent there; what it cannot
// judge, the validity and the nonce, it leaves out.
func leaseView(st lease.State, now time.Time, addrOf func(node string) netip.Addr) *LeaseView {
	if !st.Holder {
		v := &LeaseView{Fenced: st.Waiting}
		if rec := st.Record; rec.Node != "" && rec.ReleasedAt.IsZero() {
			v.Holder, v.Generation, v.AcquiredAt, v.Address = rec.Node, rec.Generation, rec.AcquiredAt, addrOf(rec.Node)
		}
		return v
	}
	v := &LeaseView{
		Holder:     st.Record.Node,
		Address:    addrOf(st.Record.Node),
		Generation: st.Writer.Generation,
		Nonce:      st.Writer.Nonce,
		AcquiredAt: st.Record.AcquiredAt,
		Valid:      st.Valid,
		Fenced:     st.Fenced,
		Behind:     st.Behind,
	}
	if !st.LastRefresh.IsZero() {
		v.RefreshAge = max(now.Sub(st.LastRefresh), 0)
	}
	if h := st.Record.Handoff; h != nil {
		v.Handoff = h.To
	}
	return v
}

// agentAddress is the agent address a node is registered with in the
// store; none when it has no entry or the registry cannot be read.
func (c *cycleRun) agentAddress(node string) netip.Addr {
	nodes, err := c.e.d.Store.Nodes()
	if err != nil {
		return netip.Addr{}
	}
	for _, n := range nodes {
		if n.Name == node {
			return n.AgentAddress
		}
	}
	return netip.Addr{}
}

// writer is the callback of both reconcilers: the identity read at the start
// of the cycle and leader.json as it is stored now, read afresh on every call.
// On the host the lease must let this node write first: the gate refreshes it
// when its last refresh is older than a few seconds, and refuses while it is
// fenced, behind, or held by another node. The reconcilers run only within a
// cycle, under the cycle lock.
func (e *Engine) writer() (us, stored planner.Writer, err error) {
	if l := e.d.Lease; l != nil {
		w, err := l.Gate(e.cycleContext())
		if err != nil {
			return e.us, planner.Writer{}, fmt.Errorf("the lease lets no write through: %w", err)
		}
		if w.Generation != e.us.Generation || w.Nonce != e.us.Nonce {
			return e.us, planner.Writer{}, fmt.Errorf("the lease is of epoch %d.%s, not of this cycle's writer %d.%s",
				w.Generation, w.Nonce, e.us.Generation, e.us.Nonce)
		}
		if err := e.storeWentBack(); err != nil {
			return e.us, planner.Writer{}, err
		}
	}
	w, found, err := e.d.Store.Writer()
	switch {
	case err != nil:
		return e.us, planner.Writer{}, err
	case !found:
		return e.us, planner.Writer{}, errors.New("leader.json is missing")
	}
	return e.us, w, nil
}

// cycleContext is the context of the cycle that runs, for the gate the
// writer callback asks.
func (e *Engine) cycleContext() context.Context {
	if e.cycleCtx != nil {
		return e.cycleCtx
	}
	return context.Background()
}

// dropClients forgets every Cloudflare client and the tokens they were built
// with: a node that does not hold the lease keeps no credential in memory.
// The next cycle that holds it builds them again.
func (e *Engine) dropClients() {
	clear(e.clients)
	clear(e.tokens)
}

// leaseRefused is why an admin action that writes to Cloudflare by itself is
// refused on the host: the gate of the lease; nil when it may go on.
func (e *Engine) leaseRefused(ctx context.Context) error {
	if e.d.Lease == nil {
		return nil
	}
	if _, err := e.d.Lease.Gate(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrRefused, err)
	}
	return e.storeWentBack()
}

// storeWentBack is the step back of the store the tenure saw, kept by the
// engine until a promotion draws a new epoch: a new store tenure, begun by a
// promotion that then failed, must not let the writes go on.
func (e *Engine) storeWentBack() error {
	if err := e.d.Store.WentBack(); err != nil && e.wentBack == nil {
		e.wentBack = err
	}
	return e.wentBack
}

// NoteLease keeps an event of the lease that happened outside a cycle, as
// its release when the daemon stops.
func (e *Engine) NoteLease(msg string) {
	e.events.add(Event{At: e.d.Now(), Level: levelInfo, Kind: kindLease, Subject: e.d.Node, Message: msg})
}

// HeardLive is the guard of the lease keeper: whether the agent of this
// node heard, at now, a controller that called its lease valid within
// lease.HeardWithin. It makes no agent: before there is one, nothing was
// heard.
func (e *Engine) HeardLive(now time.Time) bool {
	a := e.fleet.current()
	return a != nil && a.HeardLive(now, lease.HeardWithin)
}

// leaseHolds says, without a refresh, whether this node holds a valid lease
// it may write under, or there is no lease: the checks of the credentials,
// which build clients with the tokens, run only then.
func (e *Engine) leaseHolds() bool {
	if e.d.Lease == nil {
		return true
	}
	st := e.d.Lease.State()
	return st.Holder && st.Valid && st.Behind == ""
}
