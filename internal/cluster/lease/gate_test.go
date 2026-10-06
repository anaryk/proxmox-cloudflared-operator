package lease

import (
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTheGateRefreshesALeaseOlderThanTheGateAge(t *testing.T) {
	t.Run("at the gate age it passes as it is", func(t *testing.T) {
		l := newLab(t, "pve1")
		pve1 := l.m("pve1")
		l.run(time.Second)
		before := pve1.refreshes.Load()
		l.pass(GateAge)
		require.NoError(t, pve1.gate())
		require.Equal(t, before, pve1.refreshes.Load())
	})

	t.Run("a nanosecond later a synchronous refresh renews it", func(t *testing.T) {
		l := newLab(t, "pve1")
		pve1 := l.m("pve1")
		l.run(time.Second)
		before := pve1.refreshes.Load()
		l.pass(GateAge + time.Nanosecond)
		require.NoError(t, pve1.gate())
		require.Equal(t, before+1, pve1.refreshes.Load())
		require.Equal(t, pve1.clocks.Now(), pve1.k.State().LastRefresh)
	})

	t.Run("a refresh that fails closes it", func(t *testing.T) {
		l := newLab(t, "pve1")
		pve1 := l.m("pve1")
		l.run(time.Second)
		l.pass(GateAge + time.Nanosecond)
		l.m("pve1").failOn("refresh", errno("utime", DefaultLock, syscall.EIO))
		err := pve1.gate()
		require.ErrorIs(t, err, ErrFenced)
		require.ErrorContains(t, err, "input/output error")
		require.True(t, pve1.holds(), "a failed refresh alone ends no tenure")
	})
}

func TestGatesShareASynchronousRefresh(t *testing.T) {
	for _, fails := range []bool{false, true} {
		name := "that renews"
		if fails {
			name = "that fails"
		}
		t.Run(name, func(t *testing.T) {
			l := newLab(t, "pve1")
			pve1 := l.m("pve1")
			l.run(time.Second)
			l.pass(GateAge + time.Nanosecond)
			if fails {
				l.m("pve1").failOn("refresh", errno("utime", DefaultLock, syscall.EIO))
			}
			before := pve1.refreshes.Load()

			errs := make([]error, 10)
			var wg sync.WaitGroup
			for i := range errs {
				wg.Go(func() { errs[i] = pve1.gate() })
			}
			wg.Wait()
			require.Equal(t, before+1, pve1.refreshes.Load(), "one refresh for ten gates")
			for _, err := range errs {
				if fails {
					require.ErrorIs(t, err, ErrFenced)
				} else {
					require.NoError(t, err)
				}
			}

			l.pass(ShareRefresh - time.Nanosecond)
			for range 10 {
				_ = pve1.gate()
			}
			require.Equal(t, before+1, pve1.refreshes.Load(), "nor another inside the window")

			l.wait(2 * time.Nanosecond)
			_ = pve1.gate()
			if fails {
				require.Equal(t, before+2, pve1.refreshes.Load(), "a failed one is tried again after the window")
			} else {
				require.Equal(t, before+1, pve1.refreshes.Load(), "a good one keeps the gate open up to its age")
			}
		})
	}
}

func TestAFailedRefreshIsNotTriedAgainWithinASecond(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	l.run(time.Second)
	l.m("pve1").failOn("refresh", errno("utime", DefaultLock, syscall.EIO))
	l.run(RefreshEvery)
	before := pve1.refreshes.Load()

	l.wait(time.Second - time.Nanosecond)
	require.ErrorIs(t, pve1.gate(), ErrFenced)
	require.Equal(t, before, pve1.refreshes.Load())
	l.wait(time.Nanosecond)
	require.ErrorIs(t, pve1.gate(), ErrFenced)
	require.Equal(t, before+1, pve1.refreshes.Load())
}

func TestAPauseInvalidatesTheLastRefresh(t *testing.T) {
	cases := []struct {
		name    string
		pause   func(*clocks)
		invalid bool
	}{
		{"a tick gap of the limit is no pause", func(c *clocks) { c.advance(TickGap) }, false},
		{"a tick gap over the limit", func(c *clocks) { c.advance(TickGap + time.Nanosecond) }, true},
		{"a suspend of the limit is none", func(c *clocks) { c.suspend(SuspendJump) }, false},
		{"a suspend over the limit", func(c *clocks) { c.suspend(SuspendJump + time.Nanosecond) }, true},
	}
	for _, c := range cases {
		for _, seenBy := range []string{"tick", "state", "gate"} {
			t.Run(c.name+", seen by the "+seenBy, func(t *testing.T) {
				l := newLab(t, "pve1")
				pve1 := l.m("pve1")
				l.run(time.Second)
				before := pve1.refreshes.Load()

				c.pause(pve1.clocks)
				if seenBy == "tick" {
					pve1.k.tick(l.ctx)
				}
				if seenBy != "gate" {
					s := pve1.k.State()
					require.Equal(t, !c.invalid, s.Valid)
					if c.invalid {
						require.Contains(t, s.Fenced, "pause")
					}
				}
				require.NoError(t, pve1.gate())
				want := before
				if c.invalid {
					want++
				}
				require.Equal(t, want, pve1.refreshes.Load())
				require.True(t, pve1.k.State().Valid)
			})
		}
	}
}

// A call that began before a pause says nothing of the time after it, even
// when it returns after the pause was seen.
func TestACallThatBeganBeforeAPauseRenewsNothing(t *testing.T) {
	t.Run("a refresh", func(t *testing.T) {
		l := newLab(t, "pve1")
		pve1 := l.m("pve1")
		l.run(time.Second)
		pve1.k.after = func(time.Duration) <-chan time.Time { return nil }
		h := l.cfs.hangOn("pve1", "refresh")
		l.pass(GateAge + time.Nanosecond)

		errc := make(chan error)
		go func() { errc <- pve1.gate() }()
		<-h.entered
		l.wait(TickGap + time.Second)
		require.False(t, pve1.k.State().Valid)
		close(h.release)
		err := <-errc
		require.ErrorIs(t, err, ErrFenced)
		require.ErrorContains(t, err, "pause")
		require.False(t, pve1.k.State().Valid)
	})

	t.Run("a claim", func(t *testing.T) {
		l := newLab(t, "pve1")
		pve1 := l.m("pve1")
		pve1.k.after = func(time.Duration) <-chan time.Time { return nil }
		h := l.cfs.hangOn("pve1", "save")

		done := make(chan struct{})
		go func() {
			pve1.k.tick(l.ctx)
			close(done)
		}()
		<-h.entered
		l.wait(TickGap + time.Second)
		pve1.k.State()
		close(h.release)
		<-done
		s := pve1.k.State()
		require.True(t, s.Holder)
		require.False(t, s.Valid)
		before := pve1.refreshes.Load()
		require.NoError(t, pve1.gate())
		require.Equal(t, before+1, pve1.refreshes.Load())
	})
}

func TestACallThatHangsIsCutAndHoldsTheNextOnesOff(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	l.run(time.Second)
	expire := make(chan time.Time, 1)
	pve1.k.after = func(d time.Duration) <-chan time.Time {
		require.Equal(t, callTimeout, d)
		return expire
	}
	h := l.cfs.hangOn("pve1", "refresh")
	l.pass(GateAge + time.Nanosecond)

	errc := make(chan error)
	go func() { errc <- pve1.gate() }()
	<-h.entered
	expire <- time.Time{}
	err := <-errc
	require.ErrorIs(t, err, ErrFenced)
	require.ErrorContains(t, err, "did not answer within 2s")

	l.pass(ShareRefresh)
	err = pve1.gate()
	require.ErrorIs(t, err, ErrFenced)
	require.ErrorContains(t, err, "still")
	require.Len(t, h.entered, 0, "no second call while the first hangs")

	close(h.release)
	for pve1.k.busy.Load() {
		runtime.Gosched()
	}
	l.pass(ShareRefresh)
	require.NoError(t, pve1.gate())
}
