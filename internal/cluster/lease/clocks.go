package lease

import "time"

// Clocks are the clocks the lease is judged by. Now is the wall clock, by
// which pmxcfs stamps and expires a lock. Monotonic and Boottime count from
// the boot of the node; Boottime goes on while the node is suspended and
// Monotonic does not.
type Clocks interface {
	Now() time.Time
	Monotonic() time.Duration
	Boottime() time.Duration
}

// SystemClocks are the clocks of this node.
func SystemClocks() Clocks { return newSystemClocks() }
