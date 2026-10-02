package resolve

import "errors"

// Level is how strongly resolution proved that an address belongs to the
// guest of a route. The zero value is a target that was not verified.
type Level string

const (
	// LevelPort: ARP for the address is answered only by the guest's MACs on
	// its bridge and VLAN, and the forwarding table of this node has learned
	// every one of them on the guest's own port.
	LevelPort Level = "port"
	// LevelFiltered is the level of a network that pins every guest NIC to
	// its address. Nothing proves it yet; it exists for the minimum the
	// settings ask for.
	LevelFiltered Level = "filtered"
	// LevelObserved: ARP for the address is answered only by the guest's MACs,
	// but no forwarding table placed them on its port, as for a guest on
	// another node; or the address is a trusted static one, which only the
	// guest's configuration vouches for.
	LevelObserved Level = "observed"
	// LevelManual is the level of a route that names an address rather than a
	// guest: there is no identity to prove, the admin who wrote it vouches
	// for it.
	LevelManual Level = "manual"
)

var errUnknownLevel = errors.New("unknown identity level")

// rank places l on the scale of proofs. A level that is not on it, manual
// included, ranks with no proof at all.
func (l Level) rank() int {
	switch l {
	case LevelObserved:
		return 1
	case LevelFiltered:
		return 2
	case LevelPort:
		return 3
	}
	return 0
}

// AtLeast reports whether l proves at least what min asks for, on the scale
// "" < observed < filtered < port. A min that is not on the scale is met by
// nothing.
func (l Level) AtLeast(min Level) bool {
	switch min {
	case "", LevelObserved, LevelFiltered, LevelPort:
		return l.rank() >= min.rank()
	}
	return false
}

// UnmarshalText accepts the levels a proof can have, and the zero level, so
// that a stored binding carries nothing else. The value is not repeated in
// the error: the file may hold anything.
func (l *Level) UnmarshalText(text []byte) error {
	switch v := Level(text); v {
	case "", LevelObserved, LevelFiltered, LevelPort:
		*l = v
		return nil
	}
	return errUnknownLevel
}
