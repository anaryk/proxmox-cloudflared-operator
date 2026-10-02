package main

import (
	"context"
	"os"
	"os/signal"
)

// signalSource delivers the signals of the operating system.
type signalSource interface {
	Notify(c chan<- os.Signal, sig ...os.Signal)
	Stop(c chan<- os.Signal)
}

// osSignals is the signals of os/signal.
type osSignals struct{}

func (osSignals) Notify(c chan<- os.Signal, sig ...os.Signal) { signal.Notify(c, sig...) }
func (osSignals) Stop(c chan<- os.Signal)                     { signal.Stop(c) }

// signalContext returns a context that ends on the first of sigs, or when the
// parent ends. The signals are caught only until then: once the first one has
// come, a second has its default effect, so that a daemon that is slow to stop
// does not make the admin kill it by other means.
func signalContext(parent context.Context, src signalSource, sigs ...os.Signal) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	src.Notify(ch, sigs...)
	go func() {
		select {
		case <-ch:
		case <-ctx.Done():
		}
		src.Stop(ch)
		cancel()
	}()
	return ctx, cancel
}
