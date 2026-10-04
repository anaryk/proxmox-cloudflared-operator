package resolve

import (
	"context"
	"net/netip"
	"slices"
	"sync"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// Shared is what the Resolve calls of one cycle share. Calls given the same
// Shared read the host's interfaces once, check the address of a guest NIC on
// the wire once and dial an address and port once, however many routes point
// there, and find the guests that have a MAC in one index. A Shared serves
// one snapshot and is safe for concurrent use.
//
// A call that is cancelled before its answer came hands nothing on: the next
// call that needs it asks again.
type Shared struct {
	reuse func(Binding) bool

	ifaces memo[struct{}, hostView]
	wire   memo[wireKey, wireAnswer]
	dials  memo[netip.AddrPort, outcome]

	macsOnce sync.Once
	macs     macIndex
}

// NewShared returns what a cycle's calls share. reuse, when not nil, says
// whether the proof a route's binding carries may stand in this cycle without
// being made again; Resolve asks it only about a binding whose proof is in no
// doubt of its own.
func NewShared(reuse func(Binding) bool) *Shared {
	return &Shared{reuse: reuse}
}

// Reusable reports whether s lets the proof b carries stand, as Resolve asks.
func (s *Shared) Reusable(b Binding) bool {
	return s != nil && s.reuse != nil && s.reuse(b)
}

// hostView is the answer of the host's interfaces.
type hostView struct {
	ifaces []HostIface
	err    error
}

// wireKey is the address of a guest NIC that the wire is asked about.
type wireKey struct {
	guest model.GuestRef
	nic   int
	mac   string
	addr  netip.Addr
}

// wireAnswer is what the wire showed of a candidate: what proves its
// identity, with the MACs that answered for its address and, by MAC, the port
// of the guest the forwarding table placed each on; or the outcome that ended
// the checks.
type wireAnswer struct {
	proof  proof
	macs   []string
	placed map[string]string
	o      outcome
}

// memo makes a call once for each key and hands its answer to every caller of
// that key. An answer the call does not keep is handed to its own caller
// only.
type memo[K comparable, T any] struct {
	mu    sync.Mutex
	calls map[K]*memoCall[T]
}

type memoCall[T any] struct {
	done chan struct{}
	val  T
	kept bool
}

// get returns the answer for key, making the call with fn when no other
// caller made or makes it. fn reports whether its answer may be handed on.
// ok is false when ctx ended while get waited for another caller's answer.
func (m *memo[K, T]) get(ctx context.Context, key K, fn func() (T, bool)) (val T, ok bool) {
	for {
		m.mu.Lock()
		c, found := m.calls[key]
		if !found {
			if m.calls == nil {
				m.calls = make(map[K]*memoCall[T])
			}
			c = &memoCall[T]{done: make(chan struct{})}
			m.calls[key] = c
			m.mu.Unlock()
			c.val, c.kept = fn()
			if !c.kept {
				m.mu.Lock()
				delete(m.calls, key)
				m.mu.Unlock()
			}
			close(c.done)
			return c.val, true
		}
		m.mu.Unlock()
		select {
		case <-c.done:
			if c.kept {
				return c.val, true
			}
		case <-ctx.Done():
			return val, false
		}
	}
}

// macIndex maps every MAC configured on a guest that runs, or may run because
// Proxmox has never said whether it does, to those guests, in the order of the
// snapshot.
type macIndex map[string][]model.GuestRef

func indexMACs(snap inventory.Snapshot) macIndex {
	idx := macIndex{}
	for _, g := range snap.Guests {
		if !g.Running && !g.StatusUnknown {
			continue
		}
		var seen []string
		for _, n := range g.NICs {
			mac, err := model.NormalizeMAC(n.MAC)
			if err != nil || slices.Contains(seen, mac) {
				continue
			}
			seen = append(seen, mac)
			idx[mac] = append(idx[mac], g.Ref)
		}
	}
	return idx
}

// macsOf returns the index of snap, made on first use.
func (s *Shared) macsOf(snap inventory.Snapshot) macIndex {
	s.macsOnce.Do(func() { s.macs = indexMACs(snap) })
	return s.macs
}
