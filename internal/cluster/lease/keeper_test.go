package lease

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestANodeAcquiresAndDrawsTheNextEpoch(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	l.run(time.Second)

	s := pve1.k.State()
	require.True(t, s.Holder)
	require.True(t, s.Valid)
	require.Equal(t, Validity, s.ValidFor)
	require.Empty(t, s.Fenced)
	require.Equal(t, pve1.clocks.Now(), s.LastRefresh)

	rec := l.leader()
	require.Equal(t, installID, rec.InstallID)
	require.Equal(t, 2, rec.Generation, "one above what setup wrote")
	require.NotEqual(t, setupWriter.Nonce, rec.Nonce)
	require.Equal(t, "pve1/boot-1", rec.Incarnation)
	require.Equal(t, "pve1", rec.Node)
	require.Equal(t, pve1.clocks.Now(), rec.AcquiredAt)
	require.Equal(t, rec, s.Record)
	require.Equal(t, rec.Writer, s.Writer)

	owner, ok := l.cfs.owner()
	require.True(t, ok)
	require.Equal(t, "pve1", owner)

	w, err := pve1.k.Gate(l.ctx)
	require.NoError(t, err)
	require.Equal(t, rec.Writer, w)

	l.run(30 * time.Second)
	require.EqualValues(t, 3, pve1.refreshes.Load(), "a refresh every ten seconds")
	require.Equal(t, 1, l.cfs.count("pve1", "mkdir"))
}

func TestANodeThatHearsALiveControllerNeverTakesOver(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	pve1, pve2 := l.m("pve1"), l.m("pve2")
	pve2.setHeard(true)
	l.run(31 * time.Second)
	pve1.stop()
	l.run(4 * time.Minute)
	require.False(t, pve2.holds())
	require.Zero(t, l.cfs.count("pve2", "mkdir")+l.cfs.count("pve2", "unlock"), "it does not even try")
	require.Contains(t, pve2.k.State().Waiting, "live controller")

	pve2.setHeard(false)
	require.LessOrEqual(t, l.runUntil(time.Minute, pve2.holds), AcquireEvery+removalWait)
}

func TestTheHolderFencesSixtySecondsAfterItsLastGoodRefresh(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	l.run(time.Second)
	epoch, rev := pve1.epoch(), l.rev()
	l.m("pve1").failOn("refresh", errno("utime", DefaultLock, syscall.EIO))

	l.run(Validity - time.Second)
	s := pve1.k.State()
	require.True(t, s.Holder)
	require.True(t, s.Valid)
	require.Equal(t, time.Second, s.ValidFor)
	require.Contains(t, s.Fenced, "input/output error", "the last refresh failed")

	l.run(time.Second)
	s = pve1.k.State()
	require.True(t, s.Holder, "the lock may still be its own")
	require.False(t, s.Valid)
	require.Zero(t, s.ValidFor)
	require.Contains(t, s.Fenced, "not refreshed for 1m0s")
	require.ErrorIs(t, pve1.gate(), ErrFenced)

	l.m("pve1").failOn("refresh", nil)
	l.run(RefreshEvery)
	require.True(t, pve1.k.State().Valid)
	require.Equal(t, epoch, pve1.epoch(), "the same epoch resumes")
	require.Equal(t, rev, l.rev(), "and leader.json is not written")
	require.NoError(t, pve1.gate())
}

func TestTheErrnoOfARefusedRefresh(t *testing.T) {
	t.Run("without quorum it decides nothing and the same epoch resumes", func(t *testing.T) {
		l := newLab(t, "pve1")
		pve1 := l.m("pve1")
		l.run(time.Second)
		epoch, rev := pve1.epoch(), l.rev()
		l.cfs.setQuorate("pve1", false)
		before := len(l.cfs.errs("pve1", "refresh"))

		l.run(100 * time.Second)
		require.True(t, pve1.holds())
		refused := l.cfs.errs("pve1", "refresh")[before:]
		require.Len(t, refused, 10, "it keeps refreshing on the normal period")
		for _, err := range refused {
			require.ErrorIs(t, err, syscall.EACCES)
		}
		require.ErrorIs(t, pve1.gate(), ErrNotQuorate)
		require.False(t, pve1.k.State().Valid)

		l.cfs.setQuorate("pve1", true)
		l.run(RefreshEvery)
		require.True(t, pve1.k.State().Valid)
		require.Equal(t, epoch, pve1.epoch())
		require.Equal(t, rev, l.rev())
		require.NoError(t, pve1.gate())
	})

	t.Run("EACCES while quorate ends the tenure", func(t *testing.T) {
		l := newLab(t, "pve1", "pve2")
		pve1 := l.m("pve1")
		l.m("pve2").stop()
		l.run(time.Second)
		rec := l.leader()
		l.cfs.takeOver()

		l.run(RefreshEvery + Tick)
		require.False(t, pve1.holds())
		require.NotEmpty(t, pve1.k.State().Waiting)
		require.Zero(t, l.cfs.count("pve1", "rmdir"))
		require.Equal(t, rec, l.leader(), "it wrote nothing")
	})

	t.Run("EACCES decides only on a quorum read begun after it", func(t *testing.T) {
		l := newLab(t, "pve1", "pve2")
		pve1 := l.m("pve1")
		l.m("pve2").stop()
		l.run(time.Second)
		pve1.staleQuorum(pve1.clocks.Now())
		l.cfs.takeOver()

		l.run(RefreshEvery + Tick)
		require.True(t, pve1.holds())
		require.ErrorIs(t, pve1.gate(), ErrNotQuorate)

		pve1.staleQuorum(time.Time{})
		l.run(Tick)
		require.False(t, pve1.holds())
	})

	t.Run("ENOENT ends the tenure and the next one draws", func(t *testing.T) {
		l := newLab(t, "pve1")
		pve1 := l.m("pve1")
		l.run(time.Second)
		epoch := pve1.epoch()
		l.cfs.remove()

		l.run(RefreshEvery)
		require.False(t, pve1.holds())
		l.run(Tick)
		require.True(t, pve1.holds())
		require.Equal(t, epoch.Generation+1, pve1.epoch().Generation, "a lock that lapsed is a new tenure")
		require.Equal(t, 2, l.cfs.count("pve1", "mkdir"))
	})

	t.Run("ENOENT without quorum decides nothing", func(t *testing.T) {
		l := newLab(t, "pve1")
		pve1 := l.m("pve1")
		l.run(time.Second)
		pve1.failOn("refresh", errno("utime", DefaultLock, syscall.ENOENT))
		l.cfs.setQuorate("pve1", false)
		l.run(RefreshEvery + Tick)
		require.True(t, pve1.holds())
	})

	t.Run("leader.json naming another nonce ends it without a refresh", func(t *testing.T) {
		l := newLab(t, "pve1")
		pve1 := l.m("pve1")
		l.run(time.Second)
		other := l.leader()
		other.Nonce = "other"
		l.cfs.writeLeader(other)
		before := pve1.refreshes.Load()

		l.run(RefreshEvery)
		require.False(t, pve1.holds())
		require.Equal(t, before, pve1.refreshes.Load())
		require.ErrorIs(t, pve1.gate(), ErrNotHolder)
	})

	t.Run("another error decides nothing", func(t *testing.T) {
		l := newLab(t, "pve1")
		pve1 := l.m("pve1")
		l.run(time.Second)
		l.m("pve1").failOn("refresh", errno("utime", DefaultLock, syscall.EIO))
		l.run(2 * Validity)
		require.True(t, pve1.holds())
	})

	t.Run("leader.json that cannot be read decides nothing", func(t *testing.T) {
		l := newLab(t, "pve1")
		pve1 := l.m("pve1")
		l.run(time.Second)
		before := pve1.refreshes.Load()
		l.m("pve1").failOn("leader", store.ErrNotMounted)
		l.run(RefreshEvery)
		require.True(t, pve1.holds())
		require.Equal(t, before, pve1.refreshes.Load(), "no refresh without finding its nonce")
		require.Contains(t, pve1.k.State().Fenced, "not mounted")
		require.ErrorIs(t, pve1.gate(), ErrFenced)
	})
}

func TestReleaseLetsTheNextNodeAcquireAtItsNextPoll(t *testing.T) {
	l := newLab(t, "pve1", "pve2", "pve3")
	pve1, pve2, pve3 := l.m("pve1"), l.m("pve2"), l.m("pve3")
	pve2.setHeard(true)
	pve3.setHeard(true)
	l.run(3 * time.Second)

	require.NoError(t, pve1.k.Release(l.ctx))
	rec := l.leader()
	require.Equal(t, pve1.clocks.Now(), rec.ReleasedAt)
	require.Nil(t, rec.Handoff)
	_, held := l.cfs.owner()
	require.False(t, held)
	require.False(t, pve1.holds())
	require.ErrorIs(t, pve1.gate(), ErrNotHolder)

	waited := l.runUntil(10*time.Second, func() bool { return pve2.holds() || pve3.holds() })
	require.LessOrEqual(t, waited, AcquireEvery+Tick)
	require.True(t, pve2.holds(), "the first mkdir wins")
	require.Equal(t, 3, pve2.epoch().Generation)

	l.run(time.Minute)
	require.Equal(t, 1, l.cfs.count("pve1", "mkdir"), "a keeper that released does not acquire again")
	require.Equal(t, 1, l.cfs.count("pve1", "rmdir"))
}

func TestAHandoffGoesToTheNamedNode(t *testing.T) {
	l := newLab(t, "pve1", "pve2", "pve3")
	pve1, pve2, pve3 := l.m("pve1"), l.m("pve2"), l.m("pve3")
	pve2.setHeard(true)
	pve3.setHeard(true)
	l.run(time.Second)

	require.NoError(t, pve1.k.Handoff(l.ctx, "pve3"))
	rec := l.leader()
	require.Equal(t, &store.Handoff{To: "pve3", Until: pve1.clocks.Now().Add(HandoffBackoff)}, rec.Handoff)
	require.Equal(t, pve1.clocks.Now(), rec.ReleasedAt)
	require.False(t, pve1.holds())

	require.LessOrEqual(t, l.runUntil(10*time.Second, pve3.holds), AcquireEvery+Tick)
	require.False(t, pve2.holds())
	require.Zero(t, l.cfs.count("pve2", "mkdir"))
	require.Equal(t, 1, l.cfs.count("pve1", "mkdir"))
}

func TestOthersHoldBackForTheHandoffBackoff(t *testing.T) {
	l := newLab(t, "pve1", "pve2", "pve3")
	pve1, pve2, pve3 := l.m("pve1"), l.m("pve2"), l.m("pve3")
	pve2.setHeard(true)
	l.run(3 * time.Second)
	pve3.stop()
	until := pve1.clocks.Now().Add(HandoffBackoff)

	require.NoError(t, pve1.k.Handoff(l.ctx, "pve3"))
	l.runUntil(time.Minute, func() bool { return pve1.holds() || pve2.holds() })
	took := pve2.clocks.Now()
	require.False(t, took.Before(until), "nobody but the named node before the back-off ends")
	require.LessOrEqual(t, took.Sub(until), AcquireEvery+Tick)
}

func TestAHandoffWithoutATargetHoldsThisNodeBack(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	pve1, pve2 := l.m("pve1"), l.m("pve2")
	pve2.setHeard(true)
	l.run(3 * time.Second)

	require.NoError(t, pve1.k.Handoff(l.ctx, ""))
	require.Nil(t, l.leader().Handoff)
	l.runUntil(10*time.Second, pve2.holds)
	require.False(t, pve1.holds())
}

func TestAHandoffIsRefusedWhenItCannotBeWritten(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	require.ErrorIs(t, pve1.k.Handoff(l.ctx, "pve2"), ErrNotHolder)
	l.run(time.Second)
	require.ErrorContains(t, pve1.k.Handoff(l.ctx, "pve1"), "itself")

	rec := l.leader()
	l.cfs.setQuorate("pve1", false)
	require.ErrorIs(t, pve1.k.Handoff(l.ctx, "pve2"), ErrNotQuorate)
	require.True(t, pve1.holds(), "a refused hand-off keeps the lease")
	require.Equal(t, rec, l.leader())
	require.Zero(t, l.cfs.count("pve1", "rmdir"))
}

func TestAFencedKeeperNeverRemovesTheLock(t *testing.T) {
	cases := map[string]func(l *lab){
		"without quorum": func(l *lab) { l.cfs.setQuorate("pve1", false) },
		"after its lease ran out": func(l *lab) {
			l.m("pve1").failOn("refresh", errno("utime", DefaultLock, syscall.EIO))
			l.run(Validity)
		},
		"after a pause": func(l *lab) {
			l.m("pve1").failOn("refresh", errno("utime", DefaultLock, syscall.EIO))
			l.wait(TickGap + time.Second)
		},
		"while another controller writes": func(l *lab) {
			pve1 := l.m("pve1")
			pve1.k.FirstWrite()
			pve1.k.ReportedEpoch(agentproto.Epoch{InstallID: installID, Generation: pve1.epoch().Generation + 1, Nonce: "split"})
		},
	}
	for name, fence := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLab(t, "pve1")
			pve1 := l.m("pve1")
			l.run(time.Second)
			rec := l.leader()
			fence(l)

			err := pve1.k.Release(l.ctx)
			require.Error(t, err)
			require.Contains(t, err.Error(), "left to expire")
			require.False(t, pve1.holds())
			require.Zero(t, l.cfs.count("pve1", "rmdir"))
			require.Equal(t, rec, l.leader(), "nothing written")
			_, held := l.cfs.owner()
			require.True(t, held)
		})
	}
}

// pmxcfs lets any node remove a lock. The refresh right before the rmdir is
// what keeps a keeper from removing one that is no longer its own.
func TestAReleaseRemovesTheLockOnlyRightAfterARefresh(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	pve1 := l.m("pve1")
	l.m("pve2").stop()
	pve1.remember(agentproto.Epoch{InstallID: installID, Generation: 4, Nonce: "gone4"})
	l.run(time.Second)
	require.NotEmpty(t, pve1.k.State().Behind, "so the release writes nothing that would notice")
	l.cfs.takeOver()

	require.ErrorContains(t, pve1.k.Release(l.ctx), "left to expire")
	owner, held := l.cfs.owner()
	require.True(t, held)
	require.Equal(t, "pve2", owner)
	require.Zero(t, l.cfs.count("pve1", "rmdir"))
}

// pmxcfs lets the old owner remove the lock of the node that took it over.
func TestAnOldOwnerNeverRemovesTheNewHoldersLock(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	pve1, pve2 := l.m("pve1"), l.m("pve2")
	l.run(time.Second)
	pve1.stop()
	l.runUntil(3*time.Minute, pve2.holds)
	require.True(t, pve1.holds(), "it has not noticed")

	pve1.resume()
	require.ErrorContains(t, pve1.k.Release(l.ctx), "left to expire")
	owner, _ := l.cfs.owner()
	require.Equal(t, "pve2", owner)
	require.Zero(t, l.cfs.count("pve1", "rmdir"))
	require.Equal(t, pve2.epoch(), l.leader().Writer)
	require.NoError(t, pve2.gate())
}

// claimFails fails the write of leader.json after it ran then.
type claimFails struct {
	inner Leader
	then  func()
}

func (s claimFails) Leader() (store.LeaderRecord, int64, bool, error) { return s.inner.Leader() }

func (s claimFails) SaveLeaderIf(store.LeaderRecord, int64) error {
	s.then()
	return errors.New("no space left on device")
}

// A claim that cannot write leader.json gives back the lock it made, but only
// right after pmxcfs took a refresh of it from this node, with a quorum and no
// pause since the claim began: the lock may be another node's by then.
func TestAFailedClaimRemovesOnlyALockStillItsOwn(t *testing.T) {
	cases := map[string]struct {
		then    func(l *lab)
		owner   string
		removes bool
	}{
		"its own": {then: func(*lab) {}, removes: true},
		"taken over meanwhile": {
			then:  func(l *lab) { l.cfs.remove(); l.cfs.takeOver() },
			owner: "pve2",
		},
		"without quorum meanwhile": {then: func(l *lab) { l.cfs.setQuorate("pve1", false) }, owner: "pve1"},
		"after a pause meanwhile":  {then: func(l *lab) { l.wait(TickGap + time.Second) }, owner: "pve1"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLab(t, "pve1", "pve2")
			pve1 := l.m("pve1")
			l.m("pve2").stop()
			pve1.k.cfg.Store = claimFails{inner: pve1.k.cfg.Store, then: func() { c.then(l) }}
			l.tick()
			require.False(t, pve1.holds())
			require.Contains(t, pve1.k.State().Waiting, "no space left")
			owner, held := l.cfs.owner()
			require.Equal(t, c.owner != "", held)
			require.Equal(t, c.owner, owner)
			if c.removes {
				require.Equal(t, 1, l.cfs.count("pve1", "rmdir"))
				return
			}
			require.Zero(t, l.cfs.count("pve1", "rmdir"))
			require.Contains(t, pve1.k.State().Waiting, "left to expire")
		})
	}
}

func TestARestartOnTheHoldersNodeKeepsTheEpoch(t *testing.T) {
	l := newLab(t, "pve1")
	old := l.m("pve1")
	l.run(11 * time.Second)
	epoch, rev := old.epoch(), l.rev()
	old.stop()
	l.run(5 * time.Second)

	restarted := l.add("pve1", "pve1/boot-1")
	l.run(time.Second)
	require.True(t, restarted.holds())
	require.Equal(t, epoch, restarted.epoch())
	require.Equal(t, rev, l.rev(), "no draw, nothing written")
	require.Zero(t, l.cfs.count("pve1", "unlock"))
	require.NoError(t, restarted.gate())
}

func TestARebootDrawsANewEpoch(t *testing.T) {
	l := newLab(t, "pve1")
	old := l.m("pve1")
	l.run(11 * time.Second)
	epoch := old.epoch()
	old.stop()
	l.run(5 * time.Second)

	rebooted := l.add("pve1", "pve1/boot-2")
	last := l.cfs.lastSeen("pve1", DefaultLock)
	l.runUntil(3*time.Minute, rebooted.holds)
	require.Greater(t, rebooted.clocks.Now().Sub(last), 120*time.Second, "the lock of the last boot expires first")
	require.Equal(t, epoch.Generation+1, rebooted.epoch().Generation)
	require.Equal(t, "pve1/boot-2", l.leader().Incarnation)
}

// pmxcfs owns a lock per node and not per process: once a process of the
// node took the lock after it lapsed, pmxcfs takes the refresh of another
// process of the node as well. The nonce in leader.json tells them apart.
func TestTwoKeepersOfOneNodeCannotBothHold(t *testing.T) {
	l := newLab(t, "pve1")
	a := l.m("pve1")
	l.run(time.Second)
	a.stop()

	b := l.add("pve1", "pve1/boot-2")
	l.runUntil(3*time.Minute, b.holds)
	require.True(t, a.holds(), "the stalled one has not noticed")
	require.NotEqual(t, a.epoch(), b.epoch())
	require.Equal(t, b.epoch(), l.leader().Writer)
	owner, _ := l.cfs.owner()
	require.Equal(t, "pve1", owner, "the same owner for pmxcfs")

	before := a.refreshes.Load()
	a.resume()
	require.ErrorIs(t, a.gate(), ErrNotHolder, "its first write is refused")
	l.run(time.Minute)
	require.False(t, a.holds())
	require.True(t, b.holds())
	require.Equal(t, before, a.refreshes.Load(), "it never refreshes as the other")
	require.NoError(t, b.gate())
}

func TestAFailedWriteOfLeaderJSONGivesTheLockBackAtOnce(t *testing.T) {
	for name, err := range map[string]error{
		"a write that fails":            errors.New("no space left on device"),
		"leader.json changed meanwhile": store.ErrRevision,
	} {
		t.Run(name, func(t *testing.T) {
			l := newLab(t, "pve1")
			pve1 := l.m("pve1")
			l.m("pve1").failOn("save", err)
			l.run(time.Second)
			require.False(t, pve1.holds())
			_, held := l.cfs.owner()
			require.False(t, held)
			require.Equal(t, 1, l.cfs.count("pve1", "rmdir"))
			require.Equal(t, setupWriter, l.leader().Writer)
			require.Contains(t, pve1.k.State().Waiting, err.Error())
		})
	}
}

// otherWrite stores leader.json with another node in it, as a write that
// raced with another one would leave it.
type otherWrite struct{ inner Leader }

func (s otherWrite) Leader() (store.LeaderRecord, int64, bool, error) { return s.inner.Leader() }

func (s otherWrite) SaveLeaderIf(r store.LeaderRecord, rev int64) error {
	r.Node = "pve9"
	return s.inner.SaveLeaderIf(r, rev)
}

func TestTheEpochIsUsedOnlyOnceItReadsBack(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	pve1.k.cfg.Store = otherWrite{pve1.k.cfg.Store}
	l.run(time.Second)
	require.False(t, pve1.holds())
	require.Equal(t, 1, l.cfs.count("pve1", "rmdir"))
	require.Contains(t, pve1.k.State().Waiting, "does not read back")
}

func TestALockWithAFileInsideIsNeverRemoved(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	pve1, pve2 := l.m("pve1"), l.m("pve2")
	l.run(time.Second)
	l.cfs.putFile(DefaultLock)
	pve1.stop()

	l.run(3 * time.Minute)
	require.False(t, pve2.holds())
	for _, err := range l.cfs.errs("pve2", "unlock") {
		require.ErrorIs(t, err, syscall.EACCES, "pmxcfs refuses every request, and never removes it")
	}
	require.Zero(t, l.cfs.count("pve2", "rmdir"))
	owner, _ := l.cfs.owner()
	require.Equal(t, "pve1", owner)
}

func TestANodeTriesToAcquireOnlyWhileItMay(t *testing.T) {
	cases := map[string]struct {
		block, clear func(*member)
		why          string
	}{
		"not eligible": {
			block: func(m *member) { m.mu.Lock(); m.notEligble = "the node is draining"; m.mu.Unlock() },
			clear: func(m *member) { m.mu.Lock(); m.notEligble = ""; m.mu.Unlock() },
			why:   "the node is draining",
		},
		"not quorate": {
			block: func(m *member) { m.lab.cfs.setQuorate(m.name, false) },
			clear: func(m *member) { m.lab.cfs.setQuorate(m.name, true) },
			why:   "not quorate",
		},
		"a quorum read too old": {
			block: func(m *member) { m.staleQuorum(m.clocks.Now().Add(-time.Minute)) },
			clear: func(m *member) { m.staleQuorum(time.Time{}) },
			why:   "old",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLab(t, "pve1")
			pve1 := l.m("pve1")
			c.block(pve1)
			l.run(20 * time.Second)
			require.False(t, pve1.holds())
			require.Zero(t, l.cfs.count("pve1", "mkdir"))
			require.Contains(t, pve1.k.State().Waiting, c.why)

			c.clear(pve1)
			require.LessOrEqual(t, l.runUntil(time.Minute, pve1.holds), AcquireEvery)
		})
	}
}

func TestTheHolderKeepsRefreshingAcrossStepsOfItsWallClock(t *testing.T) {
	for _, step := range []time.Duration{-time.Hour, time.Hour} {
		t.Run(step.String(), func(t *testing.T) {
			l := newLab(t, "pve1")
			pve1 := l.m("pve1")
			l.run(time.Second)
			pve1.clocks.step(step)
			l.run(time.Minute)
			for _, err := range l.cfs.errs("pve1", "refresh") {
				require.NoError(t, err)
			}
			require.EqualValues(t, 6, pve1.refreshes.Load())
			require.True(t, pve1.k.State().Valid)
			require.NoError(t, pve1.gate())
		})
	}
}

func TestTheKeeperMakesTheDirectoryOfLocksOnlyInsideTheClusterFilesystem(t *testing.T) {
	l := newLab(t, "pve1")
	l.cfs.removeDir(lockParent)
	l.run(time.Second)
	require.True(t, l.m("pve1").holds())

	l = newLab(t, "pve1")
	l.cfs.removeDir(lockParent)
	l.cfs.removeDir("/etc/pve/priv")
	l.run(time.Second)
	require.False(t, l.m("pve1").holds())
	require.Equal(t, 2, l.cfs.count("pve1", "mkdir"), "the lock and its directory, never their parent")
	require.False(t, l.cfs.hasDir("/etc/pve/priv"))
}

func TestRunTicksUntilItsContextEnds(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	ticks := make(chan time.Time)
	pve1.k.ticker = func() (<-chan time.Time, func()) { return ticks, func() {} }
	ctx, cancel := context.WithCancel(l.ctx)
	done := make(chan error)
	go func() { done <- pve1.k.Run(ctx) }()

	ticks <- t0
	require.True(t, pve1.holds(), "the first tick is at the start")
	cancel()
	require.NoError(t, <-done)
}

func TestRunEndsAfterARelease(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	ticks := make(chan time.Time)
	pve1.k.ticker = func() (<-chan time.Time, func()) { return ticks, func() {} }
	done := make(chan error)
	go func() { done <- pve1.k.Run(l.ctx) }()
	ticks <- t0
	require.NoError(t, pve1.k.Release(l.ctx))
	ticks <- t0
	require.NoError(t, <-done)
}

func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	l := newLab(t, "pve1")
	good := l.m("pve1").k.cfg
	cases := map[string]func(*Config){
		"no node":        func(c *Config) { c.Node = "" },
		"no incarnation": func(c *Config) { c.Incarnation = "" },
		"no filesystem":  func(c *Config) { c.FS = nil },
		"no store":       func(c *Config) { c.Store = nil },
		"no quorum":      func(c *Config) { c.Quorum = nil },
		"no memory":      func(c *Config) { c.Remembered = nil },
		"no eligibility": func(c *Config) { c.Eligible = nil },
		"no guard":       func(c *Config) { c.Guard = nil },
		"no clocks":      func(c *Config) { c.Clocks = nil },
		"no randomness":  func(c *Config) { c.Rand = nil },
	}
	for name, change := range cases {
		cfg := good
		change(&cfg)
		_, err := New(cfg)
		require.Error(t, err, name)
	}
	cfg := good
	cfg.Lock = ""
	k, err := New(cfg)
	require.NoError(t, err)
	require.Equal(t, DefaultLock, k.cfg.Lock)
}
