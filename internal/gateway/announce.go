package gateway

import (
	"context"
	"net/netip"
	"time"

	"github.com/rs/zerolog"
)

// A leg that raised its address announces it this many times, this far
// apart. A guest keeps the MAC it learned for the leg's address until its
// entry expires, which after a pair was made anew, as when a mark passed to
// another network, is the MAC of the old pair; a gratuitous ARP replaces it
// at once. More than one, as a single frame may be lost, and as a kernel
// that updated the entry within the last second may ignore the first.
const (
	announceRounds = 3
	announceGap    = time.Second
)

type announcer interface {
	announce(dev string, addr netip.Addr) error
}

// announceRaised sends the gratuitous ARP of every leg whose address was
// raised since its last announcement, in rounds, each round to every such
// leg. A frame that cannot be sent is logged and changes nothing else: the
// guests then learn the MAC when their entries expire.
func announceRaised(ctx context.Context, sys announcer, legs []Leg, records map[string]*legRecord,
	sleep func(context.Context, time.Duration) error, log zerolog.Logger,
) {
	var due []Leg
	for _, l := range legs {
		if rec := records[l.ID]; rec != nil && rec.announce {
			rec.announce = false
			due = append(due, l)
		}
	}
	if len(due) == 0 {
		return
	}
	for round := range announceRounds {
		if round > 0 && sleep(ctx, announceGap) != nil {
			return
		}
		for _, l := range due {
			if err := sys.announce(l.Iface(), l.Prefix.Addr()); err != nil {
				log.Warn().Err(err).Str("leg", l.Iface()).Stringer("addr", l.Prefix.Addr()).
					Msg("the address of the leg could not be announced")
			}
		}
	}
}
