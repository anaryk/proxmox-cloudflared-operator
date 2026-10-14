package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// watchdogAfter is WatchdogSec of pco.service: systemd restarts the daemon
// when no ping came for that long.
const watchdogAfter = 90 * time.Second

func TestHealthy(t *testing.T) {
	now := t0.Add(time.Hour)
	for name, tc := range map[string]struct {
		p    Progress
		want bool
	}{
		"a cycle that started 74 s ago":   {Progress{Running: true, StartedAt: now.Add(-74 * time.Second)}, true},
		"a cycle that started 75 s ago":   {Progress{Running: true, StartedAt: now.Add(-cycleDeadline)}, false},
		"waiting for a cycle due later":   {Progress{DueAt: now.Add(8 * time.Second)}, true},
		"waiting for a cycle due now":     {Progress{DueAt: now}, true},
		"due 14 s ago":                    {Progress{DueAt: now.Add(-14 * time.Second)}, true},
		"due 15 s ago":                    {Progress{DueAt: now.Add(-dueGrace)}, false},
		"the loop has not started":        {Progress{}, false},
		"a cycle runs, the last was late": {Progress{Running: true, StartedAt: now, DueAt: now.Add(-time.Hour)}, true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.eng.progress.p = tc.p

			require.Equal(t, tc.want, e.eng.Healthy(now))
		})
	}
}

// An admin action may hold the cycle lock for longer than a cycle: the loop
// waits for it, its due time passes, and pings stop dueGrace later. The last
// ping came at or after the due time, so the watchdog outlasts a hold of 70
// s that began before it.
func TestALoopWaitingBehindAnAdminActionIsNotRunning(t *testing.T) {
	e := newEnv(t)
	due := e.clock.now()
	e.eng.progress.due(due)
	require.NoError(t, e.eng.acquire(t.Context()))
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.eng.timedCycle(t.Context())
	}()

	require.Never(t, func() bool { return e.eng.Progress().Running }, 50*time.Millisecond, 5*time.Millisecond,
		"no cycle runs while it waits for the lock")
	require.True(t, e.eng.Healthy(due.Add(dueGrace-time.Second)))
	require.False(t, e.eng.Healthy(due.Add(dueGrace)))
	require.Greater(t, watchdogAfter, 70*time.Second, "the last ping, at the due time or after, plus the watchdog outlasts the hold")

	e.eng.release()
	<-done
	require.False(t, e.eng.Progress().Running)
	require.Equal(t, e.clock.now(), e.eng.Progress().StartedAt)
}

// cutCtx is the context of a cycle whose deadline passes when the test says.
type cutCtx struct {
	context.Context
	once sync.Once
	done chan struct{}
}

func (c *cutCtx) Done() <-chan struct{} { return c.done }

func (c *cutCtx) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (c *cutCtx) cut() { c.once.Do(func() { close(c.done) }) }

// A step still running when the deadline of the cycle passes is cut, and the
// cycle holds the rest: nothing is written, and the next cycle goes on.
func TestACycleOfRunIsCutAtItsDeadlineAndHoldsTheRest(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	var cycle *cutCtx
	var asked []time.Duration
	e.eng.timeout = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		asked = append(asked, d)
		if d == cycleDeadline {
			cycle = &cutCtx{Context: ctx, done: make(chan struct{})}
			return cycle, cycle.cut
		}
		return context.WithTimeout(ctx, d)
	}
	// The inventory hangs until the deadline passes.
	e.inv.hook(func() { cycle.cut() })

	st := e.eng.timedCycle(t.Context())

	require.Contains(t, asked, cycleDeadline)
	require.Contains(t, st.Hold, "context deadline exceeded")
	require.Empty(t, e.writes())
	require.False(t, e.eng.Progress().Running)

	e.inv.hook(nil)
	st = e.eng.timedCycle(t.Context())
	require.Empty(t, st.Problems)
	require.NotEmpty(t, e.writes())
}

// The test hook holds a cycle as it begins, with the context of the daemon:
// the deadline of the cycle does not end it, so the watchdog sees a cycle
// that makes no progress.
func TestTheHoldOfTheTestHookCountsAsARunningCycle(t *testing.T) {
	e := newEnv(t)
	type key struct{}
	var running, deadline bool
	var value any
	e.holdCycle = func(ctx context.Context) {
		running = e.eng.Progress().Running
		_, deadline = ctx.Deadline()
		value = ctx.Value(key{})
	}
	e.eng = e.newEngine()

	e.eng.timedCycle(context.WithValue(t.Context(), key{}, "daemon"))

	require.True(t, running)
	require.False(t, deadline, "not the cycle's deadline")
	require.Equal(t, "daemon", value)
}

// The heartbeat calls the lease valid only while the engine makes progress.
func TestTheHeartbeatsLeaseIsValidOnlyWhileTheEngineMakesProgress(t *testing.T) {
	e, l := onHost(t)
	now := e.clock.now()
	e.eng.progress.due(now)

	ok, left := e.eng.LeaseValid(now)
	require.True(t, ok)
	require.Equal(t, time.Minute, left)

	e.eng.progress.started(now.Add(-cycleDeadline))
	ok, _ = e.eng.LeaseValid(now)
	require.False(t, ok, "a cycle that hangs")

	e.eng.progress.due(now)
	l.set(func(st *lease.State) { st.Valid = false })
	ok, _ = e.eng.LeaseValid(now)
	require.False(t, ok, "a lease that is not valid")

	ok, _ = newEnv(t).eng.LeaseValid(now)
	require.False(t, ok, "no lease")
}

// progressAccess is the access control of Proxmox, which notes what the loop
// of Run was doing while it was read.
type progressAccess struct {
	*fakeAccess
	eng     **Engine
	running []bool
}

func (a *progressAccess) ACL(ctx context.Context) ([]pve.ACLEntry, error) {
	a.running = append(a.running, (*a.eng).Progress().Running)
	return a.fakeAccess.ACL(ctx)
}

// The reads before a cycle, the first checks of the credentials at
// Cloudflare and the access control of Proxmox, count as running: each has a
// deadline of its own, so a slow answer is not taken for an engine that waits
// past its due time, and the watchdog is not left without a ping.
func TestTheReadsBeforeACycleCountAsRunning(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	acc := &progressAccess{fakeAccess: e.acc, eng: &e.eng}
	e.eng.d.Access = acc
	var checking []bool
	e.eng.d.NewClient = func(c store.Credential) (cfapi.API, error) {
		checking = append(checking, e.eng.Progress().Running)
		return e.newClient(c)
	}
	ctx, cancel := context.WithCancel(t.Context())
	e.inv.hook(cancel)

	require.NoError(t, e.eng.Run(ctx))

	require.NotEmpty(t, checking)
	require.True(t, checking[0], "the first check of the credential")
	require.Equal(t, []bool{true}, acc.running, "the access control")
	require.LessOrEqual(t, firstCheckTimeout+accessTimeout, 60*time.Second, "both fit in a running stretch")
}
