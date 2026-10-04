package engine

import (
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// vouching is what the engine knows of the watch of the network for the
// proofs of identity a cycle may reuse: whether the watch runs, since when it
// has watched each address with the pin it has now, when it last reported an
// address moved, and since when each guest has run on the same node with the
// same configuration. It is safe for concurrent use.
type vouching struct {
	mu       sync.Mutex
	watching time.Time // when the watch that runs started; zero while none does
	pinned   map[netip.Addr]pinnedSince
	fired    map[netip.Addr]time.Time
	guests   map[string]guestSeen // by guest
}

type pinnedSince struct {
	mac, bridge, port string
	since             time.Time
}

type guestSeen struct {
	digest, node     string
	running, unknown bool
	since            time.Time
}

// Watching tells the engine whether the watch of the network runs. Until it
// does, and again after it stopped, every cycle proves every address anew.
func (e *Engine) Watching(on bool) {
	v := &e.vouch
	v.mu.Lock()
	defer v.mu.Unlock()
	if !on {
		v.watching = time.Time{}
		return
	}
	if v.watching.IsZero() {
		v.watching = e.d.Now()
	}
}

// moved notes that the watch reported addr moved at.
func (v *vouching) moved(addr netip.Addr, at time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.fired == nil {
		v.fired = make(map[netip.Addr]time.Time)
	}
	v.fired[addr] = at
}

// pin notes the pins the watch is given now, at. A pin that is the one the
// watch had keeps the time it got it; an address that is no longer pinned is
// forgotten, with its report.
func (v *vouching) pin(pins map[netip.Addr]egress.Pin, at time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	next := make(map[netip.Addr]pinnedSince, len(pins))
	for addr, p := range pins {
		cur := pinnedSince{mac: p.MAC.String(), bridge: p.Bridge, port: p.Port, since: at}
		if old, ok := v.pinned[addr]; ok && old.mac == cur.mac && old.bridge == cur.bridge && old.port == cur.port {
			cur.since = old.since
		}
		next[addr] = cur
	}
	for addr := range v.fired {
		if _, ok := next[addr]; !ok {
			delete(v.fired, addr)
		}
	}
	v.pinned = next
}

// see notes the guests of a snapshot taken for the cycle that began at now.
func (v *vouching) see(snap inventory.Snapshot, now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	next := make(map[string]guestSeen, len(snap.Guests))
	for _, g := range snap.Guests {
		cur := guestSeen{digest: g.Digest, node: g.Node, running: g.Running, unknown: g.StatusUnknown, since: now}
		if old, ok := v.guests[g.Ref.String()]; ok {
			if old.digest == cur.digest && old.node == cur.node && old.running == cur.running && old.unknown == cur.unknown {
				cur.since = old.since
			}
		}
		next[g.Ref.String()] = cur
	}
	v.guests = next
}

// vouches reports whether the proof b carries may stand at now without being
// made again: the watch ran when the proof was made and has not stopped
// since, it watched the address with the MAC of b from before then and has
// not reported it moved since, the guest has run on the same node with the
// same configuration from before then, and the proof is younger than every.
// Whatever happened at the very time of the proof counts as after it.
func (v *vouching) vouches(b resolve.Binding, now time.Time, every time.Duration) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	at := b.VerifiedAt
	pin, pinned := v.pinned[b.Addr]
	g, seen := v.guests[b.Guest]
	fired, moved := v.fired[b.Addr]
	switch {
	case v.watching.IsZero() || !v.watching.Before(at):
		return false
	case !pinned || !pin.since.Before(at) || !sameHardware(pin.mac, b.MAC):
		return false
	case moved && !fired.Before(at):
		return false
	case !seen || g.since.After(at) || !g.running:
		return false
	}
	age := now.Sub(at)
	return age >= 0 && age < every
}

func sameHardware(a, b string) bool {
	x, errA := net.ParseMAC(a)
	y, errB := net.ParseMAC(b)
	return errA == nil && errB == nil && x.String() == y.String()
}

// reusable is the cycle's answer to the resolver: whether the proof of a
// binding may stand in this cycle.
func (c *cycleRun) reusable(b resolve.Binding) bool {
	return c.e.vouch.vouches(b, c.now, time.Duration(c.settings.ReverifyInterval))
}
