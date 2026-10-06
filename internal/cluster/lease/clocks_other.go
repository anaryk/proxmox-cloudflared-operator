//go:build !linux

package lease

import "time"

// systemClocks of a system without CLOCK_BOOTTIME count both from their own
// start, so a suspend is not seen there; a gap between ticks still is.
type systemClocks struct{ start time.Time }

func newSystemClocks() systemClocks { return systemClocks{start: time.Now()} }

func (systemClocks) Now() time.Time             { return time.Now() }
func (c systemClocks) Monotonic() time.Duration { return time.Since(c.start) }
func (c systemClocks) Boottime() time.Duration  { return time.Since(c.start) }
