package egress

import "errors"

// TargetFlows is what the filter counted for one target since its element was
// added: the connections a connector opened to that address and port (the
// packet counter of its element in flows4 or flows6, which only new
// connections look up).
type TargetFlows struct {
	Target     Target
	Flows      uint64
	Generation Generation
}

// Generation tells one life of the counters from the next: the handle nft
// gave the table, read from the table listing. Filter.sync renders the whole
// table anew whenever a target is added, and a table rendered anew gets a new
// handle, so a changed handle means every counter restarted, whatever its
// value. Set handles do not tell it: they count within a table and repeat
// after a new render.
type Generation struct {
	Table uint64
}

// ErrNoCounters says that the live table has no counting sets, as a table an
// older pco loaded.
var ErrNoCounters = errors.New("the egress table has no counting sets")
