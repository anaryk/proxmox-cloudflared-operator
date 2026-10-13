package clusterapi

import (
	"context"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const (
	// missGap is the least time between two refreshes of the allowlist
	// made for one source it does not hold, and missFloor between two made
	// for any, so that many sources cannot make it read the inventory again
	// and again.
	missGap   = 10 * time.Second
	missFloor = time.Second
	// missTimeout bounds a refresh made for a miss, which runs while a
	// connection waits.
	missTimeout = 5 * time.Second
)

// Sources are the addresses the listener takes a connection from: by node,
// those of its agent (on a host its agent address and the one the cluster
// lists for it; in the appliance the current addresses of its container),
// and the containers that may join, which have no node yet.
type Sources struct {
	Nodes map[string][]netip.Addr
	Join  []netip.Addr
}

// Allowlist is the set of sources a connection is taken from, decided before
// any TLS handshake. It is refreshed from the inventory every cycle, and
// when a source it does not hold connects, at most every ten seconds for
// that source and every second for all, so that a node just registered gets
// in without waiting for a cycle while unknown sources cannot make it read
// the inventory again and again.
type Allowlist struct {
	refresh func(context.Context) (Sources, error)
	now     func() time.Time
	log     zerolog.Logger

	mu       sync.Mutex
	nodes    map[string][]netip.Addr
	byAddr   map[netip.Addr]string
	join     map[netip.Addr]bool
	missed   map[netip.Addr]time.Time // the last refresh made for each source's miss
	lastMiss time.Time
}

// NewAllowlist returns an allowlist that reads its sources with refresh.
// Nothing is allowed before the first refresh.
func NewAllowlist(refresh func(context.Context) (Sources, error), now func() time.Time, log zerolog.Logger) *Allowlist {
	return &Allowlist{refresh: refresh, now: now, log: log, nodes: map[string][]netip.Addr{}, byAddr: map[netip.Addr]string{}, join: map[netip.Addr]bool{}, missed: map[netip.Addr]time.Time{}}
}

// Refresh reads the sources again. What was known stays when the read fails.
func (l *Allowlist) Refresh(ctx context.Context) error {
	src, err := l.refresh(ctx)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.set(src)
	return nil
}

// set installs src. The caller holds the lock.
func (l *Allowlist) set(src Sources) {
	l.nodes, l.byAddr, l.join = map[string][]netip.Addr{}, map[netip.Addr]string{}, map[netip.Addr]bool{}
	for node, addrs := range src.Nodes {
		for _, a := range addrs {
			a = a.Unmap()
			l.nodes[node] = append(l.nodes[node], a)
			l.byAddr[a] = node
		}
	}
	for _, a := range src.Join {
		l.join[a.Unmap()] = true
	}
}

// Allowed reports whether a connection from src is taken: as the agent of a
// registered node, or, forJoin, as a container that may join. A source not
// held makes the allowlist read its sources again, unless it did so for a
// miss within the last ten seconds.
func (l *Allowlist) Allowed(src netip.Addr, forJoin bool) bool {
	src = src.Unmap()
	return l.lookup(src, func() bool {
		if forJoin {
			return l.join[src]
		}
		_, ok := l.byAddr[src]
		return ok
	})
}

// Belongs reports whether src is an address of node's agent, reading the
// sources again on a miss as Allowed does.
func (l *Allowlist) Belongs(node string, src netip.Addr) bool {
	src = src.Unmap()
	return l.lookup(src, func() bool { return slices.Contains(l.nodes[node], src) })
}

// lookup answers holds on what is known, and on a miss reads the sources
// again, unless that was done for a miss of src within missGap or for any
// miss within missFloor. holds runs under the lock.
func (l *Allowlist) lookup(src netip.Addr, holds func() bool) bool {
	l.mu.Lock()
	if holds() {
		l.mu.Unlock()
		return true
	}
	now := l.now()
	last, seen := l.missed[src]
	if (seen && now.Sub(last) < missGap) || (len(l.missed) > 0 && now.Sub(l.lastMiss) < missFloor) {
		l.mu.Unlock()
		return false
	}
	// The floor keeps this map to about missGap/missFloor sources.
	for a, t := range l.missed {
		if now.Sub(t) >= missGap {
			delete(l.missed, a)
		}
	}
	l.missed[src], l.lastMiss = now, now
	l.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), missTimeout)
	defer cancel()
	sources, err := l.refresh(ctx)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		l.log.Warn().Err(err).Str("source", src.String()).Msg("the sources of the agent endpoint could not be read again")
		return false
	}
	l.set(sources)
	return holds()
}

// Addrs returns the addresses the agent of node connects from, as last read;
// nil for a node it does not hold.
func (l *Allowlist) Addrs(node string) []netip.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.nodes[node])
}
