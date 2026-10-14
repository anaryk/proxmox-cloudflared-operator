package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// The watchdog is pinged only while the engine makes progress: a cycle that
// hangs stops the pings, and systemd restarts the daemon.
func TestTheWatchdogIsPingedOnlyWhileTheEngineMakesProgress(t *testing.T) {
	n := newFakeNotifier()
	healthy := true
	var asked []time.Time
	now := t0
	wd := newWatchdog(n, func(at time.Time) bool { asked = append(asked, at); return healthy }, func() time.Time { return now }, zerolog.Nop())

	wd.ping()
	require.Equal(t, 1, n.pinged())
	healthy = false
	now = now.Add(pingEvery)
	wd.ping()
	wd.ping()
	require.Equal(t, 1, n.pinged(), "none while the cycle hangs")
	healthy = true
	wd.ping()
	require.Equal(t, 2, n.pinged())
	require.Equal(t, []time.Time{t0, t0.Add(pingEvery), t0.Add(pingEvery), t0.Add(pingEvery)}, asked)
}

// The pinger pings at once, then at every tick, until the daemon stops.
func TestTheWatchdogPingsOnEveryTick(t *testing.T) {
	n := newFakeNotifier()
	ticks := make(chan time.Time)
	wd := newWatchdog(n, func(time.Time) bool { return true }, time.Now, zerolog.Nop())
	wd.ticker = func() (<-chan time.Time, func()) { return ticks, func() {} }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		wd.run(ctx)
	}()

	ticks <- time.Time{}
	ticks <- time.Time{}
	cancel()
	<-done

	require.Equal(t, 3, n.pinged())
}

func TestTheTestHookHoldsACycleOnlyWithTheVariable(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "hold-cycle")
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	require.Nil(t, holdHook(Config{HoldCycle: file}, nil, zerolog.Nop()), "without the test hooks the file means nothing")

	looks := 0
	sleep := func(context.Context, time.Duration) error {
		looks++
		if looks == 3 {
			require.NoError(t, os.Remove(file))
		}
		return nil
	}
	hold := holdHook(Config{TestHooks: true, HoldCycle: file}, sleep, zerolog.Nop())
	hold(t.Context())
	require.Equal(t, 3, looks, "held until the file went")

	require.NoError(t, os.WriteFile(file, nil, 0o600))
	stopped := func(context.Context, time.Duration) error { return errors.New("stopping") }
	holdHook(Config{TestHooks: true, HoldCycle: file}, stopped, zerolog.Nop())(t.Context())
}
