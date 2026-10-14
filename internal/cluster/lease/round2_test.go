package lease

import (
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// pmxcfs answers an unlock request on a lock with a file inside with EACCES,
// as it answers one on a fresh lock, and never removes it. A waiting node
// lists the lock, which any node may, and raises the alarm.
func TestAWaitingNodeSeesAFileInTheLock(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	pve1, pve2 := l.m("pve1"), l.m("pve2")
	l.run(time.Second)
	l.cfs.putFile(DefaultLock)
	pve1.stop()
	asked := l.cfs.count("pve2", "unlock")

	l.run(3 * time.Minute)

	require.False(t, pve2.holds())
	require.Contains(t, pve2.k.State().Waiting, "something put a file into the lock")
	require.Equal(t, asked, l.cfs.count("pve2", "unlock"), "no unlock request on a lock that never expires")
}

// A refresh pmxcfs answers with EBUSY, as during a change of the membership,
// is a failed refresh: the tenure goes on, fenced once the lease is too old,
// and the next good refresh resumes the same epoch.
func TestARefreshAnsweredEBUSYIsAFailedRefresh(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	l.run(time.Second)
	epoch := pve1.epoch()

	pve1.failOn("refresh", syscall.EBUSY)
	l.run(20 * time.Second)
	require.True(t, pve1.holds(), "the tenure goes on")
	require.ErrorIs(t, pve1.gate(), ErrFenced)

	pve1.failOn("refresh", nil)
	l.run(11 * time.Second)
	require.True(t, pve1.holds())
	require.Equal(t, epoch, pve1.epoch())
	require.NoError(t, pve1.gate())
}

// A holder whose wall clock stepped back an hour and that restarted in the
// same boot has its refresh refused; the lock is taken again about two
// minutes later, with a new epoch, also when the node with the stepped
// clock judges the expiry: pmxcfs starts its count again when the clock is
// behind the change it saw.
func TestARestartBehindASteppedBackClockRetakesTheLockAfterTwoMinutes(t *testing.T) {
	l := newLab(t, "pve1")
	old := l.m("pve1")
	l.run(11 * time.Second)
	before := old.epoch()
	old.exit()
	old.clocks.step(-time.Hour)

	restarted := l.add("pve1", "pve1/boot-1")
	took := l.runUntil(5*time.Minute, restarted.holds)

	require.True(t, restarted.holds())
	require.Greater(t, took, lockTimeout*time.Second)
	require.Less(t, took, 135*time.Second)
	require.Equal(t, before.Generation+1, restarted.epoch().Generation)
}
