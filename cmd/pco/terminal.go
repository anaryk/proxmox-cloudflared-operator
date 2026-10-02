package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// termOps is what reading a secret asks of the terminal.
type termOps struct {
	getState     func(fd int) (*term.State, error)
	restore      func(fd int, state *term.State) error
	readPassword func(fd int) ([]byte, error)
}

// readSecret reads a line from the terminal at fd without echoing it, and gives
// the terminal back when the admin interrupts the read.
func readSecret(fd int) ([]byte, error) {
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupts)
	return readPasswordRestoring(fd, termOps{term.GetState, term.Restore, term.ReadPassword}, interrupts)
}

// readPasswordRestoring reads a secret without echo. A read gives the terminal
// back when it ends by itself, but a process that is interrupted while it
// waits is gone without having done so, and the admin is left with a terminal
// that does not show what is typed: the state is kept, and put back when the
// interrupt comes first.
func readPasswordRestoring(fd int, ops termOps, interrupts <-chan os.Signal) ([]byte, error) {
	state, err := ops.getState(fd)
	if err != nil {
		return nil, err
	}
	type result struct {
		secret []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		secret, err := ops.readPassword(fd)
		done <- result{secret, err}
	}()
	select {
	case r := <-done:
		return r.secret, r.err
	case <-interrupts:
		if err := ops.restore(fd, state); err != nil {
			return nil, fmt.Errorf("interrupted, and the terminal could not be restored: %w", err)
		}
		return nil, errors.New("interrupted")
	}
}
