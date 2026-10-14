package engine

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The guests of the three-node world, each with one card on vmbr0: web on
// pve1, db on pve2, app on pve3.
var (
	webCard = "bc:24:11:00:01:01"
	dbCard  = "bc:24:11:00:02:01"
	appCard = "bc:24:11:00:03:01"
	dbAddr  = netip.MustParseAddr("10.0.0.22")
)

// carded is g on node with one card of mac on vmbr0.
func carded(g model.Guest, node, mac string) model.Guest {
	g.Node = node
	g.NICs = []model.NIC{{Index: 0, MAC: mac, Bridge: "vmbr0"}}
	return g
}

// agentAct is how the fake agent of a node fares: whether its watch runs,
// since when its agent runs, and, by hostname, what it reports of a
// target; a target it has no word for verifies at port in its set.
type agentAct struct {
	silent  bool
	kind    pki.Kind
	started time.Time
	noWatch bool
	fail    map[string]string // hostname -> reason it fails there
	addrs   map[string]netip.Addr
	// edit changes the status before it is reported.
	edit func(st *agentproto.Status)
}

// world is three nodes of one install: this one, pve1, holds the lease;
// pve2 and pve3 report through a fake remote fleet as their agents would,
// from the last snapshot the controller served them.
type world struct {
	*env
	r      *fakeRemote
	lease  *fakeLease
	listed *[]members.Member
	acts   map[string]*agentAct
}

func threeNodes(t *testing.T) *world {
	t.Helper()
	one, two := 1, 2
	e, r, listed := clusterOf(t,
		store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one},
		store.NodeEntry{Name: "pve3", Since: t0, State: store.NodeActive, StateSince: t0, Index: &two},
	)
	*listed = []members.Member{{Name: "pve1", ID: 1, Online: true}, {Name: "pve2", ID: 2, Online: true}, {Name: "pve3", ID: 3, Online: true}}
	l := holding(testWriter)
	e.lease = l
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	e.res.addrs["db.example.com"] = dbAddr
	e.inv.set(inventory.Snapshot{
		Guests: []model.Guest{
			carded(guest(101, "web", "www.example.com -> :8080"), "pve1", webCard),
			carded(guest(102, "db", "db.example.com -> :8080"), "pve2", dbCard),
			carded(guest(103, "app", "app.example.com -> :8080"), "pve3", appCard),
		},
		Nodes: []inventory.Node{
			{Name: "pve1", Addr: nodeAddr, Online: true, Local: true},
			{Name: "pve2", Addr: netip.MustParseAddr("10.0.0.3"), Online: true},
			{Name: "pve3", Addr: netip.MustParseAddr("10.0.0.4"), Online: true},
		},
		Complete: true,
	})
	e.eng = e.newEngine()
	e.enforce()
	w := &world{env: e, r: r, lease: l, listed: listed, acts: map[string]*agentAct{
		"pve2": {kind: pki.KindHost, started: t0.Add(-time.Hour)},
		"pve3": {kind: pki.KindHost, started: t0.Add(-time.Hour)},
	}}
	w.watchHere(t)
	return w
}

// watchHere has the agent of this node watch its tables, as the daemon has
// it do, with an observer that sees nothing.
func (w *world) watchHere(t *testing.T) {
	t.Helper()
	w.eng.Watching(true)
	a := w.eng.Agent()
	require.NotNil(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.Observe(ctx, func(ctx context.Context, _ func() map[string]egress.Expected, _ func(egress.Observation)) error {
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	require.Eventually(t, func() bool { return !a.Status(t.Context()).WatchRunningSince.IsZero() }, 5*time.Second, time.Millisecond)
}

// report has every fake agent that is not silent report what it would of
// its last snapshot.
func (w *world) report() {
	for _, node := range slices.Sorted(maps.Keys(w.acts)) {
		act := w.acts[node]
		if act.silent {
			continue
		}
		w.r.reportStatus(node, w.clock.now(), w.statusOf(node, act))
	}
}

// statusOf is the status the agent of node would report now.
func (w *world) statusOf(node string, act *agentAct) agentproto.Status {
	now := w.clock.now()
	st := agentproto.Status{
		Node: node, Kind: act.kind, Protocol: agentproto.Range{Min: 1, Max: 1},
		AgentStartedAt: act.started, Clock: now, Egress: "on",
	}
	if act.kind == pki.KindHost && !act.noWatch {
		st.WatchRunningSince = act.started
	}
	snap, served := w.r.lastOf(node)
	if served {
		st.EpochSeen, st.SeqApplied = snap.Epoch, snap.Seq
		for _, t := range snap.Targets {
			r := agentproto.TargetReport{Key: t.Key, Addr: t.Addr, Level: "port", OK: true, InEgressSet: true, VerifiedAt: now}
			if a, ok := act.addrs[t.Key]; ok {
				r.Addr = a
			}
			if !r.Addr.IsValid() {
				r.Addr = guestAddr
				if t.Key == "db.example.com" {
					r.Addr = dbAddr
				}
			}
			if why, bad := act.fail[t.Key]; bad {
				r.OK, r.InEgressSet, r.Level, r.Reason = false, false, "observed", why
			}
			st.Targets = append(st.Targets, r)
		}
	}
	if !st.WatchRunningSince.IsZero() {
		snapNow := w.inv.snapshot()
		for _, g := range snapNow.Guests {
			if g.Node != node {
				continue
			}
			for _, nic := range g.NICs {
				st.Attestations = append(st.Attestations, agentproto.Attestation{
					Node: node, Guest: g.Ref, NIC: nic.Index, MAC: nic.MAC, Bridge: nic.Bridge,
					Port: fmt.Sprintf("tap%di%d", g.Ref.VMID, nic.Index), Since: act.started,
				})
			}
		}
	}
	if act.edit != nil {
		act.edit(&st)
	}
	return st
}

// tick moves the clock on by d, has the agents report and runs a cycle.
func (w *world) tick(d time.Duration) State {
	w.clock.advance(d)
	w.report()
	return w.cycle()
}

// settle runs cycles until the served snapshots and the reports agree.
func (w *world) settle() State {
	var st State
	for range 3 {
		st = w.tick(time.Second)
	}
	return st
}

// localVerify is the last request this node's agent made to verify host.
func (w *world) localVerify(t *testing.T, host string) resolve.VerifyRequest {
	t.Helper()
	reqs := w.res.verifications()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].Route.Hostname == host {
			return reqs[i]
		}
	}
	t.Fatalf("this node never verified %s", host)
	return resolve.VerifyRequest{}
}

// targetOf is the target of host in the snapshot last served to node.
func (w *world) targetOf(t *testing.T, node, host string) agentproto.Target {
	t.Helper()
	s, ok := w.r.lastOf(node)
	require.True(t, ok, "node %s was served no snapshot", node)
	for _, tg := range s.Targets {
		if tg.Key == host {
			return tg
		}
	}
	t.Fatalf("the snapshot of %s has no target %s", node, host)
	return agentproto.Target{}
}

// reportStatus has node's agent report st, received at at.
func (f *fakeRemote) reportStatus(node string, at time.Time, st agentproto.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reports == nil {
		f.reports = map[string]NodeReport{}
	}
	f.reports[node] = NodeReport{Status: st, ReceivedAt: at, Fresh: true}
}

// lastOf is the snapshot last served to node; false when none was.
func (f *fakeRemote) lastOf(node string) (agentproto.Snapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.served) == 0 {
		return agentproto.Snapshot{}, false
	}
	s, ok := f.served[len(f.served)-1][node]
	return s, ok
}

func (f *fakeInventory) snapshot() inventory.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}
