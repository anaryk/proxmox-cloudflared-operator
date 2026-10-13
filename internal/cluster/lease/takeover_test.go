package lease

import (
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// pmxcfs answers every unlock request with EACCES and removes an expired lock
// a moment later: the waiting node takes it with the mkdir that follows its
// request, not at its next poll.
func TestASecondNodeTakesOverOnlyAfterTheLockExpiredByTheClockOfTheLowestNodeID(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	pve1, pve2 := l.m("pve1"), l.m("pve2")
	l.run(31 * time.Second)
	require.True(t, pve1.holds())
	require.False(t, pve2.holds())
	refused := l.cfs.errs("pve2", "unlock")
	require.Len(t, refused, 7, "an unlock request at every poll")
	for _, err := range refused {
		require.ErrorIs(t, err, syscall.EACCES, "held and fresh")
	}

	pve1.stop()
	// pve1 has the lowest node id: the expiry is timed by its clock.
	last := l.cfs.lastSeen("pve1", DefaultLock)
	l.runUntil(3*time.Minute, pve2.holds)
	took := l.clocks["pve1"].Now().Sub(last)
	require.Greater(t, took, 120*time.Second)
	require.LessOrEqual(t, took, 120*time.Second+Tick+AcquireEvery+removalWait)
	require.Equal(t, 3, pve2.epoch().Generation)
	require.Equal(t, pve2.epoch(), l.leader().Writer)
	require.Equal(t, "pve2", l.leader().Node)

	calls := l.cfs.callsOf("pve2")
	took2 := lastMkdir(t, calls)
	request := calls[took2-1]
	require.Equal(t, "unlock", request.op)
	require.ErrorIs(t, request.err, syscall.EACCES, "the request that had the lock removed was refused too")
	require.Equal(t, removalWait, calls[took2].at.Sub(request.at))
}

// lastMkdir is the index of the last mkdir in calls, which must have
// succeeded.
func lastMkdir(t *testing.T, calls []fsCall) int {
	t.Helper()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].op == "mkdir" {
			require.NoError(t, calls[i].err)
			return i
		}
	}
	t.Fatal("no mkdir")
	return 0
}

// An unlock request is decided by the wall clock of the node with the lowest
// node id, whichever node asks. A step forward there lets the next request
// remove a fresh lock; a step anywhere else changes nothing. A node whose
// agent hears the holder sends no request, so the step costs nothing; one
// that does not hear it takes the lock, and the holder stops writing at the
// latest the gate age after its last refresh. A holder with the lowest node
// id refreshes at the first tick after its own step, before the next request
// here; what is left of that case is in steps_test.go.
func TestAStepOfAWallClock(t *testing.T) {
	cases := []struct {
		name            string
		holder, stepped string
		step            time.Duration
		heard           bool
		takesOver       bool
	}{
		{name: "the waiting node has the lowest node id and hears the holder",
			holder: "pve2", stepped: "pve1", step: 3 * time.Minute, heard: true},
		{name: "the waiting node has the lowest node id and does not hear the holder",
			holder: "pve2", stepped: "pve1", step: 3 * time.Minute, takesOver: true},
		{name: "another waiting node, hearing the holder",
			holder: "pve1", stepped: "pve2", step: 3 * time.Minute, heard: true},
		{name: "another waiting node, not hearing the holder",
			holder: "pve1", stepped: "pve2", step: 3 * time.Minute},
		{name: "the holder has the lowest node id and the waiting node hears it",
			holder: "pve1", stepped: "pve1", step: time.Hour, heard: true},
		{name: "the holder has the lowest node id and the waiting node does not hear it",
			holder: "pve1", stepped: "pve1", step: time.Hour},
		{name: "the holder has the lowest node id, a step back",
			holder: "pve1", stepped: "pve1", step: -time.Hour},
		{name: "another holder, a step forward",
			holder: "pve2", stepped: "pve2", step: time.Hour},
		{name: "another holder, a step back",
			holder: "pve2", stepped: "pve2", step: -time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLab(t, "pve1", "pve2")
			holder, waiter := l.m(c.holder), l.m("pve1")
			if c.holder == "pve1" {
				waiter = l.m("pve2")
			}
			waiter.setHeard(true)
			l.run(time.Second)
			require.True(t, holder.holds())
			waiter.setHeard(c.heard)
			rec := l.leader()
			requests := l.cfs.count(waiter.name, "unlock")

			l.clocks[c.stepped].step(c.step)
			if !c.takesOver {
				l.run(3 * time.Minute)
				require.True(t, holder.holds())
				require.False(t, waiter.holds())
				require.Equal(t, rec, l.leader())
				require.NoError(t, holder.gate())
				require.Zero(t, l.cfs.count(holder.name, "rmdir"))
				if c.heard {
					require.Equal(t, requests, l.cfs.count(waiter.name, "unlock"), "a node that hears the holder sends no request")
				}
				return
			}

			require.LessOrEqual(t, l.runUntil(AcquireEvery+removalWait+Tick, waiter.holds), AcquireEvery+removalWait)
			require.Equal(t, waiter.epoch(), l.leader().Writer)
			calls := l.cfs.callsOf(waiter.name)
			request := calls[lastMkdir(t, calls)-1]
			require.Equal(t, "unlock", request.op)
			require.ErrorIs(t, request.err, syscall.EACCES)
			if holder.holds() {
				l.wait(max(holder.lastGood()+GateAge-holder.clocks.Monotonic(), 0) + time.Nanosecond)
				require.ErrorIs(t, holder.gate(), ErrNotHolder)
			}
			require.False(t, holder.holds())
			require.Zero(t, l.cfs.count(holder.name, "rmdir"))
			owner, _ := l.cfs.owner()
			require.Equal(t, waiter.name, owner)
		})
	}
}

// What a waiting node does with each answer to its unlock request.
func TestTheAnswersToAnUnlockRequest(t *testing.T) {
	cases := []struct {
		name string
		// answer is pmxcfs's answer; removed has the lock gone with it.
		answer  syscall.Errno
		removed bool
		takes   bool
		why     string
	}{
		{name: "EACCES, removed a moment later", answer: syscall.EACCES, removed: true, takes: true},
		{name: "ENOENT, removed already", answer: syscall.ENOENT, removed: true, takes: true},
		{name: "success, removed already", removed: true, takes: true},
		{name: "ENOTEMPTY", answer: syscall.ENOTEMPTY, why: "something put a file"},
		{name: "another error", answer: syscall.EIO, why: "the unlock request failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLab(t, "pve1", "pve2")
			pve1, pve2 := l.m("pve1"), l.m("pve2")
			pve2.setHeard(true)
			l.run(time.Second)
			pve1.stop()
			pve2.setHeard(false)
			pve2.k.cfg.FS = unlockAnswer{FS: pve2.k.cfg.FS, answer: func() error {
				if c.removed {
					l.cfs.remove()
				}
				if c.answer == 0 {
					return nil
				}
				return errno("utime", DefaultLock, c.answer)
			}}
			before := len(l.cfs.callsOf("pve2"))

			l.run(AcquireEvery + removalWait)
			require.Equal(t, c.takes, pve2.holds())
			calls := l.cfs.callsOf("pve2")[before:]
			if c.takes {
				mk := lastMkdir(t, calls)
				wait := time.Duration(0)
				if c.answer == syscall.EACCES {
					wait = removalWait
				}
				require.Equal(t, wait, calls[mk].at.Sub(calls[mk-1].at), "the mkdir after the request")
				return
			}
			require.Contains(t, pve2.k.State().Waiting, c.why)
			require.Zero(t, l.cfs.count("pve2", "rmdir"))
			var ops []string
			for _, call := range calls {
				ops = append(ops, call.op)
			}
			require.Equal(t, []string{"mkdir"}, ops, "nothing follows the request until the next poll")
		})
	}
}

// While the lock is held, a waiting node sends one unlock request per poll and
// one mkdir right after each.
func TestAWaitingNodeLooksAgainOnceAfterEachUnlockRequest(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.run(time.Second)
	l.run(4 * AcquireEvery)

	var unlocks, mkdirs []time.Time
	for _, c := range l.cfs.callsOf("pve2") {
		switch c.op {
		case "unlock":
			require.ErrorIs(t, c.err, syscall.EACCES)
			unlocks = append(unlocks, c.at)
		case "mkdir":
			require.ErrorIs(t, c.err, syscall.EEXIST)
			mkdirs = append(mkdirs, c.at)
		}
	}
	require.Len(t, unlocks, 5)
	require.Len(t, mkdirs, 2*len(unlocks)-1)
	for i, at := range unlocks {
		require.Equal(t, at, mkdirs[2*i], "the poll's mkdir found the lock")
		if i > 0 {
			require.Equal(t, AcquireEvery, at.Sub(unlocks[i-1]))
		}
		if 2*i+1 < len(mkdirs) {
			require.Equal(t, at.Add(removalWait), mkdirs[2*i+1], "and one more after the request")
		}
	}
}

// unlockAnswer answers unlock requests as answer says, without pmxcfs.
type unlockAnswer struct {
	FS
	answer func() error
}

func (f unlockAnswer) UnlockRequest(string) error { return f.answer() }
