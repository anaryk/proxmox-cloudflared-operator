package lease

import (
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// refreshThen runs then right after every refresh pmxcfs took, as if the
// process stood still or was slow there.
type refreshThen struct {
	FS
	then func()
}

func (f refreshThen) Refresh(path string, mtime time.Time) error {
	err := f.FS.Refresh(path, mtime)
	if err == nil {
		f.then()
	}
	return err
}

// pmxcfs has no check-and-remove: any node's rmdir removes any lock. A
// release removes the lock only in the same step as a refresh pmxcfs took
// from this node at its epoch, and only when nothing stood still between the
// two; otherwise it leaves the lock to expire.
func TestAReleaseRemovesTheLockOnlyInTheStepThatConfirmedIt(t *testing.T) {
	cases := []struct {
		name string
		// between runs after the release's refresh and before its rmdir.
		between func(l *lab)
		removes bool
		owner   string
	}{
		{name: "nothing between", between: func(*lab) {}, removes: true},
		{name: "a pause, and a takeover it did not see", owner: "pve2", between: func(l *lab) {
			l.wait(3 * time.Minute)
			l.cfs.remove()
			l.cfs.takeOver()
		}},
		{name: "a suspend", owner: "pve1", between: func(l *lab) { l.clocks["pve1"].suspend(3 * time.Minute) }},
		{name: "a slow refresh", owner: "pve1", between: func(l *lab) { l.wait(removeWithin + time.Second) }},
		{name: "a file put into the lock", owner: "pve1", between: func(l *lab) { l.cfs.putFile(DefaultLock) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLab(t, "pve1", "pve2")
			pve1 := l.m("pve1")
			l.m("pve2").stop()
			l.run(time.Second)
			pve1.k.cfg.FS = refreshThen{FS: pve1.k.cfg.FS, then: func() { c.between(l) }}

			err := pve1.k.Release(l.ctx)
			require.False(t, pve1.holds())
			owner, held := l.cfs.owner()
			if c.removes {
				require.NoError(t, err)
				require.False(t, held)
				return
			}
			require.ErrorContains(t, err, "left to expire")
			require.True(t, held)
			require.Equal(t, c.owner, owner)
			if c.name != "a file put into the lock" {
				require.Zero(t, l.cfs.count("pve1", "rmdir"))
			}
		})
	}
}

// The re-read of leader.json is part of the same step: a release that finds
// another epoch there removes nothing, even with the lock still this node's.
func TestAReleaseThatFindsAnotherEpochRemovesNothing(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	l.run(time.Second)
	pve1.k.cfg.FS = refreshThen{FS: pve1.k.cfg.FS, then: func() {}}
	before := pve1.refreshes.Load()
	other := l.leader()
	other.Nonce = "other"
	l.cfs.writeLeader(other)

	require.ErrorContains(t, pve1.k.Release(l.ctx), "left to expire")
	require.Zero(t, l.cfs.count("pve1", "rmdir"))
	require.Equal(t, before, pve1.refreshes.Load(), "no refresh as another epoch")
	_, held := l.cfs.owner()
	require.True(t, held)
	for _, err := range l.cfs.errs("pve1", "refresh") {
		require.NotErrorIs(t, err, syscall.EACCES)
	}
}
