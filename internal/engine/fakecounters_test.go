package engine

import (
	"context"
	"maps"
	"net/netip"
	"sync"
)

// fakeCounters is the counter source of the fake egress filter: every read
// answers what count set last, or fails with readErr, and is counted.
type fakeCounters struct {
	mu         sync.Mutex
	generation string
	counts     map[netip.AddrPort]uint64
	readErr    error
	reads      int
}

func (f *fakeCounters) FlowCounts(context.Context) (string, map[netip.AddrPort]uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.readErr != nil {
		return "", nil, f.readErr
	}
	return f.generation, maps.Clone(f.counts), nil
}

// count makes the next reads answer these counters, by "addr:port", of a
// table of this generation.
func (f *fakeCounters) count(generation string, counts map[string]uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.generation, f.readErr = generation, nil
	f.counts = make(map[netip.AddrPort]uint64, len(counts))
	for s, n := range counts {
		f.counts[netip.MustParseAddrPort(s)] = n
	}
}

func (f *fakeCounters) failReads(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readErr = err
}

func (f *fakeCounters) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}
