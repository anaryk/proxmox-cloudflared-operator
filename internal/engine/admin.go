package engine

import (
	"context"
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
)

// Apply leaves observe-only mode. With confirmDeletes the next enforcing DNS
// run may delete more records than the mass delete guard lets through. It
// waits for a running cycle, so that the request is not used up by a cycle
// that read the settings before it, and then asks for a cycle.
func (e *Engine) Apply(ctx context.Context, confirmDeletes bool) error {
	if err := e.acquire(ctx); err != nil {
		return err
	}
	defer e.Trigger()
	defer e.release()

	s, err := e.d.Store.Settings()
	if err != nil {
		return fmt.Errorf("reading the settings: %w", err)
	}
	if s.ObserveOnly {
		s.ObserveOnly = false
		if err := e.d.Store.SaveSettings(s); err != nil {
			return fmt.Errorf("leaving observe-only mode: %w", err)
		}
		e.adminEvent("", "observe-only mode ended; changes are applied from now on")
	}
	if confirmDeletes {
		e.confirmDeletes = true
		e.adminEvent("", "the deletes held by the mass delete guard are confirmed for the next run")
	}
	return nil
}

// Adopt asks the next enforcing DNS run to take over the record of someone
// else that holds name, a hostname pco publishes.
func (e *Engine) Adopt(ctx context.Context, name string) error {
	host, err := hostname.Normalize(name)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := e.acquire(ctx); err != nil {
		return err
	}
	defer e.Trigger()
	defer e.release()
	e.adopt[host] = true
	e.adminEvent(host, "adoption requested for the next run")
	return nil
}

func (e *Engine) adminEvent(subject, msg string) {
	e.events.add(Event{At: e.d.Now(), Level: levelInfo, Kind: kindAdmin, Subject: subject, Message: msg})
}
