package main

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeSignals stands in for the signals of the operating system.
type fakeSignals struct {
	mu       sync.Mutex
	ch       chan<- os.Signal
	released chan struct{}
}

func newFakeSignals() *fakeSignals { return &fakeSignals{released: make(chan struct{})} }

func (f *fakeSignals) Notify(c chan<- os.Signal, _ ...os.Signal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ch = c
}

func (f *fakeSignals) Stop(chan<- os.Signal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ch = nil
	select {
	case <-f.released:
	default:
		close(f.released)
	}
}

func (f *fakeSignals) registered() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ch != nil
}

func (f *fakeSignals) send(sig os.Signal) {
	f.mu.Lock()
	ch := f.ch
	f.mu.Unlock()
	ch <- sig
}

// waitUntilReleased blocks until the handler is given up, which the code
// under test does in a goroutine of its own.
func (f *fakeSignals) waitUntilReleased() {
	<-f.released
}

func TestTheSignalContextEndsOnTheFirstSignalAndLetsTheSecondThrough(t *testing.T) {
	src := newFakeSignals()
	ctx, stop := signalContext(t.Context(), src, os.Interrupt)
	defer stop()
	require.NoError(t, ctx.Err())
	require.True(t, src.registered(), "the signals are caught from the start")

	src.send(os.Interrupt)

	<-ctx.Done()
	src.waitUntilReleased()
	require.False(t, src.registered(), "the handler is gone once the first signal came: a second one has its default effect")
}

func TestTheSignalContextIsReleasedWhenItsParentEnds(t *testing.T) {
	src := newFakeSignals()
	parent, cancel := context.WithCancel(t.Context())
	ctx, stop := signalContext(parent, src, os.Interrupt)
	defer stop()

	cancel()

	<-ctx.Done()
	src.waitUntilReleased()
	require.False(t, src.registered())
}
