package lease

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The holder refreshes at the first tick that sees its wall clock move
// against the monotonic one by more than SuspendJump: on the node with the
// lowest node id, which judges the expiry of every lock, the refresh restarts
// the expiry by the new clock.
func TestAStepOfTheWallClockRefreshesAtOnce(t *testing.T) {
	cases := []struct {
		step    time.Duration
		refresh bool
	}{
		{step: time.Hour, refresh: true},
		{step: -time.Hour, refresh: true},
		{step: SuspendJump + time.Second, refresh: true},
		{step: SuspendJump},
		{step: -SuspendJump},
	}
	for _, c := range cases {
		t.Run(c.step.String(), func(t *testing.T) {
			l := newLab(t, "pve1")
			pve1 := l.m("pve1")
			l.run(3 * time.Second)
			before := pve1.refreshes.Load()

			pve1.clocks.step(c.step)
			l.run(time.Second)
			want := before
			if c.refresh {
				want++
			}
			require.Equal(t, want, pve1.refreshes.Load())
			l.run(time.Second)
			require.Equal(t, want, pve1.refreshes.Load(), "once")
			require.NoError(t, pve1.gate())
		})
	}
}

// What is left of a step forward on the holder with the lowest node id: an
// unlock request of a node that does not hear it, sent between the step and
// the holder's next tick, has the lock removed. The holder finds it gone at
// that tick and never writes in its epoch again; the next tenure draws.
func TestAStepOnTheHolderWithTheLowestNodeIDLeavesOneTick(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	pve1, pve2 := l.m("pve1"), l.m("pve2")
	l.run(time.Second)
	l.run(AcquireEvery - time.Second)
	require.True(t, pve1.holds())
	epoch := pve1.epoch()

	pve1.clocks.step(time.Hour)
	l.wait(time.Second)
	pve2.k.tick(l.ctx) // its poll is due and comes first
	pve1.k.tick(l.ctx)
	require.False(t, pve1.holds(), "the refresh at once finds the lock gone")
	require.ErrorIs(t, pve1.gate(), ErrNotHolder)

	l.runUntil(removalWait+Tick, func() bool { return pve1.holds() || pve2.holds() })
	require.NotEqual(t, pve1.holds(), pve2.holds())
	require.Greater(t, l.leader().Generation, epoch.Generation)
	require.Zero(t, l.cfs.count("pve1", "rmdir"))
}

// A daemon that restarts in the same boot keeps its epoch only if pmxcfs takes
// its refresh. After the wall clock stepped back past the lock's mtime pmxcfs
// refuses it, and the keeper says what that probably means.
func TestARestartAfterTheWallClockSteppedBackSaysWhy(t *testing.T) {
	l := newLab(t, "pve1")
	old := l.m("pve1")
	l.run(11 * time.Second)
	old.exit()
	old.clocks.step(-time.Hour)

	restarted := l.add("pve1", "pve1/boot-1")
	l.run(time.Second)
	require.False(t, restarted.holds())
	why := restarted.k.State().Waiting
	require.Contains(t, why, "refused the refresh of the lock this node and boot made")
	require.Contains(t, why, "stepped back")
}
