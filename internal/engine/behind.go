package engine

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	problemStoreWentBack = "the store went backwards (restored?); pco promote adopts it"
	problemBehindHost    = "the store is older than what the agents remember (restored?); " +
		"pco promote shows the differences and draws a new epoch"
	problemOtherCluster = "this store was made in cluster %s (%s); pco promote here makes this cluster its writer"
	// problemClusterUnknown is a cluster whose identity cannot be read.
	problemClusterUnknown = "the identity of this cluster cannot be read (%v); nothing is written"
	// problemAgentEpochUnread is an epoch file of the local agent that
	// cannot be read, which may hold an epoch above the store's.
	problemAgentEpochUnread = "the epoch the agent of this node follows cannot be read (%v); nothing is written"
	// problemAboveReported names the nodes whose agents follow an epoch
	// above this controller's, which keep it from writing until pco
	// promote.
	problemAboveReported = "the agents of %s follow an epoch above this controller's; pco promote draws above it, or forget a node that should not"
)

// sameCluster holds every write of a store that was made in another
// cluster: restored there, or the image of a node copied elsewhere. The
// identity of a cluster is the fingerprint of its CA, which Proxmox makes
// once and never rotates.
func (c *cycleRun) sameCluster() bool {
	read := c.e.d.Cluster
	if read == nil {
		return true
	}
	here, err := read()
	if err != nil {
		c.hold(c.problem(problemClusterUnknown, err))
		return false
	}
	c.cluster = here
	rec := c.install.Cluster
	if rec == nil || rec.CASHA256 == here.CASHA256 {
		return true
	}
	c.holdBehind(c.problem(problemOtherCluster, clusterName(*rec), rec.CASHA256))
	return false
}

// clusterName is the name of a cluster for a message.
func clusterName(id store.ClusterIdentity) string {
	return cmp.Or(id.Name, "of a node standing alone")
}

// recordCluster stores the identity of the cluster with the install when it
// has none, as a store made before it was recorded, or no name while the
// cluster has one now: a node set up alone and joined to a cluster since.
// It runs only once the lease let this cycle write.
func (c *cycleRun) recordCluster() {
	here := c.cluster
	if here.CASHA256 == "" {
		return
	}
	rec := c.install.Cluster
	if rec != nil && (rec.Name != "" || here.Name == "") {
		return
	}
	inst := c.install
	inst.Cluster = &here
	if err := c.e.d.Store.SaveInstall(inst); err != nil {
		c.problem("saving the identity of the cluster with the install: %v", err)
		return
	}
	c.install = inst
}

// startTenure begins the store's record of what this tenure writes when the
// lease is of an epoch this process has not written under yet: a read that
// finds one of those objects at a lower revision, or gone, says that the
// store went back under the writer. What the placement of routes kept of
// the tenure before is dropped with it: another controller may have
// written the store between the two.
func (c *cycleRun) startTenure(st lease.State) {
	e := c.e
	if sameWriterEpoch(e.tenure, st.Writer) {
		return
	}
	e.d.Store.StartTenure()
	e.tenure, e.tenureAt, e.tenureWrote = st.Writer, c.now, false
	e.place.newTenure()
}

// sameWriterEpoch says whether two writers are of one epoch.
func sameWriterEpoch(a, b planner.Writer) bool {
	return a.InstallID == b.InstallID && a.Generation == b.Generation && a.Nonce == b.Nonce
}

// agentsReported hands the lease the epoch every agent follows, and before
// the first write of the tenure holds the cycle until every node that is
// neither down nor removed has reported since the tenure began, for at most
// takeoverHold. An agent that follows an epoch leader.json does not explain
// makes the controller behind: the store went back, or a second controller
// writes. An agent that lost what it remembered cannot say so, but another
// one can, which is why the first write waits for all of them. A node whose
// agent does not report within the hold is unknown, and is not waited for:
// a cluster whose agent channel is down on one node must not lose its
// controller; what that agent reports later still stops the writes.
func (c *cycleRun) agentsReported() bool {
	l := c.e.d.Lease
	ep, found, err := c.e.d.Store.AgentEpoch()
	switch {
	case err != nil:
		// What the agent of this node remembers cannot be told: it may be
		// an epoch above the store's.
		c.hold(c.problem(problemAgentEpochUnread, err))
		return false
	case found:
		c.e.noteAgentEpoch(c.e.d.Node, pki.KindHost, agentproto.Epoch(ep))
	}
	reports := c.reportsNow()
	for _, name := range slices.Sorted(maps.Keys(reports)) {
		n, ok := c.nodes[name]
		if !ok || n.Node.State == store.NodeRemoved || n.Node.State == store.NodeContested {
			// The endpoint refuses such a node now; what it reported
			// before is not its word any more.
			continue
		}
		c.e.noteAgentEpoch(name, reports[name].Status.Kind, reports[name].Status.EpochSeen)
	}
	if c.e.tenureWrote || l.State().Behind != "" {
		// Behind holds the cycle next, by its own line.
		return true
	}
	nodes, err := c.e.d.Store.Nodes()
	if err != nil {
		c.hold(c.storeProblem("reading the node registry", err))
		return false
	}
	if missing := unreported(nodes, reports, c.e.tenureAt); len(missing) > 0 {
		if left := takeoverHold - c.now.Sub(c.e.tenureAt); left > 0 {
			c.hold(c.problem(problemTakeoverHold, strings.Join(missing, ", "), left.Round(time.Second)))
			return false
		}
		c.e.d.Log.Warn().Strs("nodes", missing).Msg("the agents of these nodes did not report within the takeover hold; they are unknown")
		// What they would have said is not known: the first cycle that
		// writes adds and withdraws nothing.
		c.addOnly = c.problem(problemAddOnly, strings.Join(missing, ", "))
	}
	c.e.tenureWrote = true
	return true
}

// unreported are the nodes of the registry, neither down nor removed, whose
// agents have not reported since the tenure began at since.
func unreported(nodes []store.NodeEntry, reports map[string]NodeReport, since time.Time) []string {
	var out []string
	for _, n := range nodes {
		if n.State == store.NodeDown || n.State == store.NodeRemoved {
			continue
		}
		if r, ok := reports[n.Name]; !ok || r.ReceivedAt.Before(since) {
			out = append(out, n.Name)
		}
	}
	slices.Sort(out)
	return out
}

// leaseEvent says what changed of the lease since the last cycle: this node
// took it or lost it, it was fenced or resumed, or the controller is behind.
func (c *cycleRun) leaseEvent(st lease.State) {
	now, said := leaseSaid(st), c.e.leaseSaid
	if now == said.state && sameWriterEpoch(st.Writer, said.writer) {
		return
	}
	c.e.leaseSaid = leaseSaidAs{state: now, writer: st.Writer}
	epoch := fmt.Sprintf("generation %d", st.Writer.Generation)
	ev := Event{At: c.now, Kind: kindLease, Subject: c.e.d.Node}
	switch now {
	case leaseWaiting:
		if said.state == "" || said.state == leaseWaiting {
			return
		}
		ev.Level, ev.Message = levelWarn, "this node no longer holds the lease: "+cmp.Or(st.Waiting, "the tenure ended")
	case leaseBehind:
		ev.Level, ev.Message = levelError, fmt.Sprintf("behind at %s: %s; nothing is written until pco promote", epoch, st.Behind)
	case leaseFenced:
		ev.Level, ev.Message = levelWarn, fmt.Sprintf("fenced at %s: %s", epoch, st.Fenced)
	case leaseHeld:
		ev.Level = levelInfo
		switch {
		case said.state == leaseFenced && sameWriterEpoch(st.Writer, said.writer):
			ev.Message = "resumed at " + epoch
		default:
			ev.Message = "acquired at " + epoch
		}
	}
	c.events = append(c.events, ev)
}

// The states of the lease the events tell apart.
const (
	leaseWaiting = "waiting"
	leaseHeld    = "held"
	leaseFenced  = "fenced"
	leaseBehind  = "behind"
)

// leaseSaidAs is the state of the lease the last event told of.
type leaseSaidAs struct {
	state  string
	writer planner.Writer
}

func leaseSaid(st lease.State) string {
	switch {
	case !st.Holder:
		return leaseWaiting
	case st.Behind != "":
		return leaseBehind
	case !st.Valid:
		return leaseFenced
	}
	return leaseHeld
}

// NoteAgentEpoch hands the lease the epoch the agent of node reported, as
// its status arrives, so that a report above the store's stops the writes
// before the next cycle; the cycles hand it every report again. node and
// kind are those the agent endpoint authenticated.
func (e *Engine) NoteAgentEpoch(node string, kind pki.Kind, ep agentproto.Epoch) {
	e.noteAgentEpoch(node, kind, ep)
}

// noteAgentEpoch takes an agent's epoch into the lease when it may move it:
// a host agent's, since the lease is the host's, of this install, of a
// generation a draw makes and with a nonce. An epoch above the one the lease
// holds is kept with the node that reported it, which the hold names, until
// a promotion draws above it.
func (e *Engine) noteAgentEpoch(node string, kind pki.Kind, ep agentproto.Epoch) {
	l := e.d.Lease
	if l == nil || ep.Generation == 0 || kind != pki.KindHost {
		return
	}
	inst, found, err := e.d.Store.Install()
	if err != nil || !found || ep.InstallID != inst.ID {
		return
	}
	if _, err := agentproto.ParseEpoch(ep.String()); err != nil {
		e.d.Log.Warn().Str("node", node).Str("epoch", ep.String()).Msg("an agent reported an epoch no draw makes; it is not taken")
		return
	}
	l.ReportedEpoch(ep)
	ours := l.State().Writer
	if c, same := ep.Order(agentproto.Epoch{InstallID: ours.InstallID, Generation: ours.Generation, Nonce: ours.Nonce}); c > 0 || c == 0 && !same {
		e.above.note(node, ep)
	}
}

// aboveReports are, by node, the epochs agents reported above the one this
// controller holds, until a promotion: the hold says who said so.
type aboveReports struct {
	mu    sync.Mutex
	nodes map[string]agentproto.Epoch
}

func (a *aboveReports) note(node string, ep agentproto.Epoch) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.nodes == nil {
		a.nodes = map[string]agentproto.Epoch{}
	}
	if had, ok := a.nodes[node]; !ok || had.Generation < ep.Generation {
		a.nodes[node] = ep
	}
}

func (a *aboveReports) clear() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nodes = nil
}

// line names the nodes whose agents reported an epoch above this
// controller's; empty when none did.
func (a *aboveReports) line() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.nodes) == 0 {
		return ""
	}
	var parts []string
	for _, node := range slices.Sorted(maps.Keys(a.nodes)) {
		parts = append(parts, fmt.Sprintf("%s (epoch %s)", node, a.nodes[node]))
	}
	return fmt.Sprintf(problemAboveReported, strings.Join(parts, ", "))
}
