package lease

import (
	"time"

	"golang.org/x/sys/unix"
)

type systemClocks struct{}

func newSystemClocks() systemClocks { return systemClocks{} }

func (systemClocks) Now() time.Time           { return time.Now() }
func (systemClocks) Monotonic() time.Duration { return clock(unix.CLOCK_MONOTONIC) }
func (systemClocks) Boottime() time.Duration  { return clock(unix.CLOCK_BOOTTIME) }

// clock reads one of the clocks, which Linux always has. Should the read
// fail all the same, zero reads as a pause, which costs a refresh.
func clock(id int32) time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(id, &ts); err != nil {
		return 0
	}
	return time.Duration(ts.Nano())
}
