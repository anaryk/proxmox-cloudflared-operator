package main

import (
	"context"
	"os"
	"syscall"
	"time"
)

// upgradeGuard keeps SIGINT, SIGTERM and SIGHUP from ending pco upgrade
// between an install and the hold that follows it: dpkg sets a held package
// back to install, and only this run holds it again. A signal is kept, and
// the run stops on it once what it installed is held: at the end of a step,
// or in a wait after the hold.
type upgradeGuard struct {
	src signalSource
	ch  chan os.Signal
}

// stoppedOn is the error of a run that a signal stopped.
type stoppedOn struct{ sig os.Signal }

func (s stoppedOn) Error() string { return "pco upgrade stopped on " + s.sig.String() }

func guardUpgrade(src signalSource) *upgradeGuard {
	g := &upgradeGuard{src: src, ch: make(chan os.Signal, 1)}
	src.Notify(g.ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	return g
}

// release gives the signals back their default effect.
func (g *upgradeGuard) release() { g.src.Stop(g.ch) }

// stopped is the error of a signal that came, or nil.
func (g *upgradeGuard) stopped() error {
	select {
	case sig := <-g.ch:
		return stoppedOn{sig}
	default:
		return nil
	}
}

// sleep is sleep that ends a wait that follows a hold on a signal that came:
// at the next look, a second at most.
func (g *upgradeGuard) sleep(sleep func(context.Context, time.Duration) error) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		if err := g.stopped(); err != nil {
			return err
		}
		return sleep(ctx, d)
	}
}
