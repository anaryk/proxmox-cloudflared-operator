package lease

import (
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A gate that waited for another operation looks for a pause again before it
// trusts a refresh made meanwhile: a suspend during the wait stops the
// monotonic clock, so that refresh still looks fresh by it.
func TestAGateSeesASuspendWhileItWaited(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	l.run(time.Second)
	l.pass(GateAge)
	l.wait(time.Second / 2)

	pve1.k.op <- struct{}{} // another operation runs
	errc := make(chan error)
	go func() { errc <- pve1.gate() }()
	for {
		pve1.k.mu.Lock()
		looked := pve1.k.lookedAt == pve1.clocks.Monotonic()
		pve1.k.mu.Unlock()
		if looked {
			break
		}
		runtime.Gosched()
	}
	// The gate found the lease too old and waits for the operation, which
	// refreshes; then the node is suspended.
	require.NoError(t, pve1.k.refresh(l.ctx, false))
	pve1.clocks.suspend(2 * SuspendJump)
	before := pve1.refreshes.Load()
	<-pve1.k.op

	require.NoError(t, <-errc)
	require.Equal(t, before+1, pve1.refreshes.Load(), "it refreshed after the suspend")
}

// A suspend during the gate's own synchronous refresh fences the write.
func TestAGateSeesASuspendDuringItsRefresh(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	l.run(time.Second)
	l.pass(GateAge + time.Second)
	pve1.k.cfg.FS = refreshThen{FS: pve1.k.cfg.FS, then: func() { pve1.clocks.suspend(2 * SuspendJump) }}

	err := pve1.gate()
	require.ErrorIs(t, err, ErrFenced)
	require.ErrorContains(t, err, "pause")
}
