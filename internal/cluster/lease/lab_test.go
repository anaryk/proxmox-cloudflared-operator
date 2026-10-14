package lease

import (
	"context"
	"crypto/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

const installID = "0123456789ab"

// setupWriter is leader.json as setup leaves it, before any node held the
// lease.
var setupWriter = planner.Writer{InstallID: installID, Generation: 1, Nonce: "s3tup"}

// lab is a cluster of nodes that share one pmxcfs, each running keepers. Time
// moves in whole seconds and every running keeper ticks once a second, in the
// order the keepers were added.
type lab struct {
	t       *testing.T
	ctx     context.Context
	cfs     *pmxcfs
	clocks  map[string]*clocks
	order   []string
	members []*member

	// locked are the nodes where a keeper holds the process lock.
	lockMu sync.Mutex
	locked map[string]bool
	// alone says that the node stands alone: no cluster in the members
	// file, no other node in the registry.
	alone atomic.Bool
}

// member is one keeper and what its node tells it.
type member struct {
	lab    *lab
	name   string
	clocks *clocks
	k      *Keeper
	// refreshes counts the refreshes this keeper sent, which pmxcfs counts
	// for the node.
	refreshes atomic.Int64

	mu         sync.Mutex
	heard      bool
	notEligble string
	remembered *agentproto.Epoch
	quorumAt   time.Time
	stopped    bool
}

func newLab(t *testing.T, nodes ...string) *lab {
	l := &lab{t: t, ctx: t.Context(), cfs: newPmxcfs(), clocks: map[string]*clocks{}, locked: map[string]bool{}}
	l.cfs.writeLeader(store.LeaderRecord{Writer: setupWriter})
	for _, n := range nodes {
		l.add(n, n+"/boot-1")
	}
	return l
}

// add starts a keeper on node, which takes the process lock of the node as
// Run does.
func (l *lab) add(node, incarnation string) *member {
	l.t.Helper()
	m := l.build(node, incarnation, l.processLock(node))
	require.NoError(l.t, m.k.hold())
	return m
}

// start is add that returns the error of the process lock.
func (l *lab) start(node, incarnation string) (*member, error) {
	m := l.build(node, incarnation, l.processLock(node))
	return m, m.k.hold()
}

// addUnlocked is add with a process lock that never refuses, as a lock file
// someone removed would be.
func (l *lab) addUnlocked(node, incarnation string) *member {
	l.t.Helper()
	m := l.build(node, incarnation, func() (func(), error) { return func() {}, nil })
	require.NoError(l.t, m.k.hold())
	return m
}

// processLock is the process lock of node: one holder at a time.
func (l *lab) processLock(node string) func() (func(), error) {
	return func() (func(), error) {
		l.lockMu.Lock()
		defer l.lockMu.Unlock()
		if l.locked[node] {
			return nil, ErrRunning
		}
		l.locked[node] = true
		return func() {
			l.lockMu.Lock()
			defer l.lockMu.Unlock()
			l.locked[node] = false
		}, nil
	}
}

// build makes a keeper on node. A second keeper on a node shares its clocks
// and its place in pmxcfs: pmxcfs cannot tell two processes of a node apart.
func (l *lab) build(node, incarnation string, exclusive func() (func(), error)) *member {
	c, ok := l.clocks[node]
	if !ok {
		c = newClocks(t0)
		if len(l.order) > 0 {
			first := l.clocks[l.order[0]]
			c = newClocks(first.Now())
			c.mono, c.boot = first.Monotonic(), first.Boottime()
		}
		l.clocks[node] = c
		l.order = append(l.order, node)
		l.cfs.addNode(node, c)
	}
	m := &member{lab: l, name: node, clocks: c}
	k, err := New(Config{
		Node:        node,
		Incarnation: incarnation,
		FS:          countingFS{FS: l.cfs.fs(node), refreshes: &m.refreshes},
		Store:       l.cfs.store(node),
		Quorum:      m.quorum,
		Remembered:  m.rememberedEpoch,
		Eligible:    m.eligible,
		Guard:       m.heardLive,
		Standalone:  l.alone.Load,
		Exclusive:   exclusive,
		Clocks:      c,
		Rand:        rand.Reader,
		Log:         zerolog.New(zerolog.NewTestWriter(l.t)),
	})
	require.NoError(l.t, err)
	m.k = k
	l.members = append(l.members, m)
	return m
}

// m is the first keeper added on node.
func (l *lab) m(node string) *member {
	for _, m := range l.members {
		if m.name == node {
			return m
		}
	}
	l.t.Fatalf("no keeper on %s", node)
	return nil
}

// run moves time by d, a second at a time, and ticks every running keeper
// after each second.
func (l *lab) run(d time.Duration) {
	l.t.Helper()
	for range int(d / time.Second) {
		l.wait(time.Second)
		l.tick()
	}
}

// tick ticks every running keeper once, without moving time.
func (l *lab) tick() {
	for _, m := range l.members {
		if !m.isStopped() {
			m.k.tick(l.ctx)
		}
	}
}

// pass moves time by d with a tick after every whole second, as Run does,
// so that the keepers see no pause.
func (l *lab) pass(d time.Duration) {
	l.run(d.Truncate(time.Second))
	l.wait(d % time.Second)
}

// wait moves time by d on every node without a tick: longer than TickGap,
// a pause of every keeper.
func (l *lab) wait(d time.Duration) {
	for _, n := range l.order {
		l.clocks[n].advance(d)
	}
}

// runUntil runs a second at a time until cond holds, at most limit, and
// returns how long it ran.
func (l *lab) runUntil(limit time.Duration, cond func() bool) time.Duration {
	l.t.Helper()
	for ran := time.Duration(0); ran < limit; ran += time.Second {
		l.run(time.Second)
		if cond() {
			return ran + time.Second
		}
	}
	l.t.Fatalf("still waiting after %s", limit)
	return 0
}

func (l *lab) leader() store.LeaderRecord {
	r, _ := l.cfs.leaderRecord()
	return r
}

func (l *lab) rev() int64 {
	_, rev := l.cfs.leaderRecord()
	return rev
}

func (m *member) quorum() (bool, time.Time) {
	m.mu.Lock()
	at := m.quorumAt
	m.mu.Unlock()
	if at.IsZero() {
		at = m.clocks.Now()
	}
	return m.lab.cfs.quorate(m.name), at
}

func (m *member) rememberedEpoch() (agentproto.Epoch, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.remembered == nil {
		return agentproto.Epoch{}, false
	}
	return *m.remembered, true
}

func (m *member) eligible() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.notEligble == "", m.notEligble
}

func (m *member) heardLive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.heard
}

func (m *member) setHeard(heard bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.heard = heard
}

func (m *member) remember(e agentproto.Epoch) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remembered = &e
}

// staleQuorum has the quorum reads of the member say they began at at.
func (m *member) staleQuorum(at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quorumAt = at
}

// stop stops the keeper as a crash or the loss of its node does: it never
// ticks again and releases nothing.
func (m *member) stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = true
}

// exit ends the process of the keeper: it never ticks again and its process
// lock is free.
func (m *member) exit() {
	m.stop()
	m.k.unhold()
}

// resume lets a stopped keeper tick again, as after SIGCONT.
func (m *member) resume() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = false
}

func (m *member) isStopped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopped
}

func (m *member) holds() bool { return m.k.State().Holder }

func (m *member) gate() error {
	_, err := m.k.Gate(m.lab.ctx)
	return err
}

// epoch is the epoch the member holds.
func (m *member) epoch() planner.Writer { return m.k.State().Writer }

// failOn makes every call op of the member's node fail with err; a nil err
// clears it.
func (m *member) failOn(op string, err error) { m.lab.cfs.failOn(m.name, op, err) }

// countingFS counts the refreshes of one keeper.
type countingFS struct {
	FS
	refreshes *atomic.Int64
}

func (f countingFS) Refresh(path string, mtime time.Time) error {
	f.refreshes.Add(1)
	return f.FS.Refresh(path, mtime)
}

// lastGood is the monotonic time of the start of the last good refresh.
func (m *member) lastGood() time.Duration {
	m.k.mu.Lock()
	defer m.k.mu.Unlock()
	return m.k.goodMono
}
