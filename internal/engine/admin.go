package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// Apply leaves observe-only mode. With confirmDeletes it also accepts what
// the last published state showed waiting for a confirmation, such as more
// DNS deletes than the mass delete guard lets through, when offer names
// exactly that; any other offer is refused and changes nothing. It waits for
// a running cycle, so that the offer is compared with what that cycle showed
// and the request is not used up by a cycle that read the settings before it,
// and then asks for a cycle.
func (e *Engine) Apply(ctx context.Context, confirmDeletes bool, offer string) (ApplyResult, error) {
	if err := e.acquireAdmin(ctx); err != nil {
		return ApplyResult{}, err
	}
	defer e.release()
	if confirmDeletes && offer != e.offered.token {
		return ApplyResult{}, fmt.Errorf("%w: what waits for a confirmation changed since it was shown; look again and repeat", ErrRefused)
	}
	defer e.Trigger()

	res := ApplyResult{Accepted: []Waiting{}}
	s, err := e.d.Store.Settings()
	if err != nil {
		return res, fmt.Errorf("reading the settings: %w", err)
	}
	// The confirmation must outlive a restart of the daemon. It is kept
	// before anything takes effect, the mode too, so that one that cannot be
	// kept changes nothing.
	if confirmDeletes {
		if err := e.keepConfirmation(e.offered.what); err != nil {
			return res, err
		}
	}
	if s.ObserveOnly {
		s.ObserveOnly = false
		if err := e.d.Store.SaveSettings(s); err != nil {
			if confirmDeletes {
				e.dropConfirmation()
			}
			return res, fmt.Errorf("leaving observe-only mode: %w", err)
		}
		res.LeftObserveOnly = true
		e.adminEvent("", "observe-only mode ended; changes are applied from now on")
	}
	if confirmDeletes {
		res.Accepted = e.confirmShown()
	}
	return res, nil
}

// keepConfirmation saves the memory as it is once what the last state offered
// is accepted. A memory that was never read is not written over. The caller
// holds the cycle lock.
func (e *Engine) keepConfirmation(o confirmable) error {
	if !e.remembered {
		return nil
	}
	if err := e.d.Store.SaveEngineMemory(e.memoryAccepting(o)); err != nil {
		return fmt.Errorf("keeping the confirmation: %w", err)
	}
	return nil
}

// dropConfirmation puts the memory back as it was before keepConfirmation,
// for a call that fails after it. Should that fail too, the next cycle saves
// it as it is. The caller holds the cycle lock.
func (e *Engine) dropConfirmation() {
	if !e.remembered {
		return
	}
	if err := e.d.Store.SaveEngineMemory(e.memory()); err != nil {
		e.d.Log.Warn().Err(err).Msg("putting back the memory after a confirmation that did not take effect failed")
	}
}

// confirmShown accepts what the last published state showed waiting for a
// confirmation, each with a problem line, and nothing it did not show: the
// removals the mass delete guard held, the vanished guests behind a vanish
// hold, the zones that left their listing and the tunnels no credential sees.
// It returns what it accepted. The offer is used up: until the next cycle
// nothing waits, and the problem lines that asked for it are gone. The
// caller holds the cycle lock and has kept the confirmation.
func (e *Engine) confirmShown() []Waiting {
	o := e.offered
	e.withdrawOffer()
	w := o.what
	if w.guard != "" {
		e.confirm = &request{at: e.d.Now()}
		e.adminEvent("", "the deletes held by the mass delete guard are confirmed for the next run")
	}
	for _, ref := range w.vanished {
		e.gone[ref] = true
	}
	if len(w.vanished) > 0 {
		// The DNS guard confirms only removals that are pending already;
		// those of these guests are not yet.
		e.adminEvent("", fmt.Sprintf("%d guests that Proxmox no longer lists are confirmed removed; "+
			"when their DNS records fall due, the mass delete guard may ask for a confirmation again", len(w.vanished)))
	}
	for _, name := range e.zones.confirmGone(w.stale) {
		e.adminEvent(name, "the zone that left its listing is confirmed gone")
	}
	for _, u := range w.invisible {
		if t, ok := e.seen[u.id]; ok {
			delete(e.seen, u.id)
			e.adminEvent(u.id, fmt.Sprintf("the tunnel %s in account %s is confirmed gone; its connector is removed", t.name, t.account))
		}
	}
	if len(o.waiting) == 0 {
		e.adminEvent("", "the last state showed nothing that waits for a confirmation")
	}
	return nonNil(cloneWaiting(o.waiting))
}

// withdrawOffer ends the offer a confirmation used, in the state served too,
// with the problem lines that asked for it. The caller holds the cycle lock.
func (e *Engine) withdrawOffer() {
	lines := e.offered.what.lines
	e.offered = offer{}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.state.Waiting, e.state.Offer = []Waiting{}, ""
	e.state.Problems = slices.DeleteFunc(slices.Clone(e.state.Problems), func(p string) bool { return slices.Contains(lines, p) })
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
	if err := e.acquireAdmin(ctx); err != nil {
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
