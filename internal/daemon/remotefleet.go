package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/clusterapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// statusFresh is how old a node's last status may be for the controller to
// go by it.
const statusFresh = 30 * time.Second

var errHostJoin = errors.New("a host node joins with pco setup, not over the agent channel")

// controllerSide is what the remote fleet asks of the controller it serves
// for: whether its lease is valid now, to tell the agents in the heartbeat,
// the epoch it writes under, and where the events of the agents go.
type controllerSide interface {
	LeaseValid(now time.Time) (bool, time.Duration)
	TakeAgentEvents(node string, st agentproto.Status, epoch agentproto.Epoch) uint64
	NoteAgentEpoch(node string, kind pki.Kind, ep agentproto.Epoch)
	Trigger()
}

// RemoteFleet is the agents of the other nodes as the controller sees them:
// the snapshot the engine serves each, which their long-polls take, and the
// last status each reported, which the engine reads. It is the Controller of
// the agent endpoint and the Remote of the engine.
type RemoteFleet struct {
	now   func() time.Time
	epoch func() agentproto.Epoch
	local func(ctx context.Context) agentproto.Status

	mu      sync.Mutex
	ctl     controllerSide
	snaps   map[string]agentproto.Snapshot
	reports map[string]engine.NodeReport
	changed chan struct{} // closed and made anew at every change
}

// newRemoteFleet returns a fleet without a controller; attach gives it one.
// epoch is the epoch the controller writes under, as the lease has it.
func newRemoteFleet(now func() time.Time, epoch func() agentproto.Epoch) *RemoteFleet {
	return &RemoteFleet{
		now: now, epoch: epoch,
		snaps: map[string]agentproto.Snapshot{}, reports: map[string]engine.NodeReport{},
		changed: make(chan struct{}),
	}
}

// attach gives the fleet the controller it answers for, and the status of
// the agent of this node, which the heartbeat counts with the others.
func (f *RemoteFleet) attach(ctl controllerSide, local func(ctx context.Context) agentproto.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctl, f.local = ctl, local
}

// changedLocked wakes every wait. The caller holds the lock.
func (f *RemoteFleet) changedLocked() {
	close(f.changed)
	f.changed = make(chan struct{})
}

// Serve takes the snapshots the engine renders for the other nodes.
func (f *RemoteFleet) Serve(snaps map[string]agentproto.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if maps.EqualFunc(f.snaps, snaps, func(a, b agentproto.Snapshot) bool { return a.Epoch == b.Epoch && a.Seq == b.Seq }) {
		return
	}
	f.snaps = maps.Clone(snaps)
	f.changedLocked()
}

// Snapshot answers a long-poll: node's snapshot once its seq in epoch is
// above after, or at once when the snapshot is of another epoch.
func (f *RemoteFleet) Snapshot(ctx context.Context, node string, epoch agentproto.Epoch, after uint64) (agentproto.Snapshot, error) {
	for {
		f.mu.Lock()
		s, ok := f.snaps[node]
		wait := f.changed
		f.mu.Unlock()
		if ok && (s.Epoch != epoch || s.Seq > after) {
			return s, nil
		}
		select {
		case <-ctx.Done():
			return agentproto.Snapshot{}, ctx.Err()
		case <-wait:
		}
	}
}

// Report takes a status of node and answers with the controller's
// heartbeat. The events it carries go into the controller's log.
func (f *RemoteFleet) Report(ctx context.Context, node string, st agentproto.Status) (agentproto.Heartbeat, error) {
	if err := checkStatus(node, st); err != nil {
		return agentproto.Heartbeat{}, err
	}
	now := f.now()
	epoch := f.epoch()
	f.mu.Lock()
	ctl, local := f.ctl, f.local
	f.mu.Unlock()
	var ack uint64
	if ctl != nil {
		// An epoch above the controller's stops its writes now, not with
		// the next cycle.
		ctl.NoteAgentEpoch(node, st.Kind, st.EpochSeen)
		ack = ctl.TakeAgentEvents(node, st, epoch)
	}
	kept := st
	kept.Events = nil
	f.mu.Lock()
	before, heard := f.reports[node]
	// A node heard for the first time, one whose agent started again and
	// one that took another snapshot have a cycle soon: a new controller
	// waits for every agent before it writes.
	news := !heard || !before.Status.AgentStartedAt.Equal(st.AgentStartedAt) ||
		before.Status.EpochSeen != st.EpochSeen || before.Status.SeqApplied != st.SeqApplied
	f.reports[node] = engine.NodeReport{Status: kept, ReceivedAt: now, Fresh: true}
	f.changedLocked()
	hb := agentproto.Heartbeat{
		Epoch:         epoch,
		Seq:           f.snaps[node].Seq,
		Nodes:         map[string]agentproto.NodeFreshness{},
		ReadyReplicas: map[string]int{},
		EventAck:      ack,
	}
	statuses := map[string]agentproto.Status{}
	for name, r := range f.reports {
		hb.Nodes[name] = agentproto.NodeFreshness{StatusAge: max(now.Sub(r.ReceivedAt), 0), WatchRunning: !r.Status.WatchRunningSince.IsZero()}
		if now.Sub(r.ReceivedAt) <= statusFresh {
			statuses[name] = r.Status
		}
	}
	f.mu.Unlock()
	if local != nil {
		own := local(ctx)
		if own.Node != "" {
			hb.Nodes[own.Node] = agentproto.NodeFreshness{WatchRunning: !own.WatchRunningSince.IsZero()}
			statuses[own.Node] = own
		}
	}
	for _, s := range statuses {
		for _, c := range s.Connectors {
			if c.Ready && c.TunnelID != "" {
				hb.ReadyReplicas[c.TunnelID]++
			}
		}
	}
	if ctl != nil {
		hb.LeaseValid, hb.LeaseValidFor = ctl.LeaseValid(now)
		if news {
			ctl.Trigger()
		}
	}
	return hb, nil
}

// checkStatus refuses a status that does not hold together, for its node
// alone: one of another node, or that follows an epoch no draw makes. The
// endpoint has bounded and decoded it already.
func checkStatus(node string, st agentproto.Status) error {
	if st.Node != node {
		return fmt.Errorf("the status is of node %q, sent by node %s", st.Node, node)
	}
	if st.EpochSeen.Generation != 0 {
		if _, err := agentproto.ParseEpoch(st.EpochSeen.String()); err != nil {
			return fmt.Errorf("the status of node %s follows an epoch no draw makes: %w", node, err)
		}
	}
	return nil
}

// Reports returns the last report of every node, fresh while it is at most
// 30 seconds old; with wait above zero it first waits, for at most wait,
// for each node named in seq to report that seq or a later one.
func (f *RemoteFleet) Reports(ctx context.Context, wait time.Duration, seq map[string]uint64) map[string]engine.NodeReport {
	if wait > 0 && len(seq) > 0 {
		ctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		for {
			f.mu.Lock()
			done := true
			for node, want := range seq {
				if r, ok := f.reports[node]; !ok || r.Status.SeqApplied < want {
					done = false
				}
			}
			ch := f.changed
			f.mu.Unlock()
			if done {
				break
			}
			select {
			case <-ctx.Done():
			case <-ch:
				continue
			}
			break
		}
	}
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]engine.NodeReport, len(f.reports))
	for node, r := range f.reports {
		r.Fresh = now.Sub(r.ReceivedAt) <= statusFresh
		out[node] = r
	}
	return out
}

// Join refuses: a host node joins with pco setup on the node itself.
func (f *RemoteFleet) Join(context.Context, pki.JoinToken, []byte, netip.Addr, clusterapi.JoinFacts) ([]byte, error) {
	return nil, errHostJoin
}

// Renew refuses: a host node issues its own certificate from the install's
// CA, which it reads from the cluster filesystem.
func (f *RemoteFleet) Renew(context.Context, string, []byte) ([]byte, error) {
	return nil, errors.New("a host node issues its own certificates")
}
