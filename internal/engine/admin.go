package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
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
		e.confirm = &request{at: e.d.Now()}
		e.adminEvent("", "the deletes held by the mass delete guard are confirmed for the next run")
		for _, ref := range e.vanished {
			e.gone[ref] = true
		}
		if len(e.vanished) > 0 {
			e.adminEvent("", fmt.Sprintf("%d guests that Proxmox no longer lists are confirmed removed", len(e.vanished)))
		}
		for _, name := range e.zones.confirmGone() {
			e.adminEvent(name, "the zone that left its listing is confirmed gone")
		}
	}
	return nil
}

// Adopt asks an enforcing DNS run to take over the record that holds name, a
// hostname pco publishes: a record of someone else, or one of ours that lost
// its marker. The request waits until the tunnel of the name is verified and
// its connector ready, and expires after requestTTL.
func (e *Engine) Adopt(ctx context.Context, name string) error {
	host, err := hostname.Normalize(name)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if !e.inTheWay(host) {
		return fmt.Errorf("%w: no record of someone else holds %s, and none of ours lost its marker there", ErrNotFound, host)
	}
	if err := e.acquire(ctx); err != nil {
		return err
	}
	defer e.Trigger()
	defer e.release()
	e.adopt[host] = &request{at: e.d.Now()}
	e.adminEvent(host, "adoption requested for the next run")
	return nil
}

// inTheWay reports whether the last state shows a record at host that pco
// would take over: one in conflict, or one that lost the marker of this
// install.
func (e *Engine) inTheWay(host string) bool {
	st := e.State()
	same := func(name string) bool { return strings.EqualFold(strings.TrimSuffix(name, "."), host) }
	return slices.ContainsFunc(st.Conflicts, func(c reconcile.Conflict) bool { return same(c.Name) }) ||
		slices.ContainsFunc(st.Lost, same)
}

func (e *Engine) adminEvent(subject, msg string) {
	e.events.add(Event{At: e.d.Now(), Level: levelInfo, Kind: kindAdmin, Subject: subject, Message: msg})
}
