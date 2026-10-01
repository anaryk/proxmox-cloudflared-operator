package connector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// pendingFile is the marker that says the files of a tunnel changed and its
// unit has not been started or restarted since. It is hidden, like the
// temporary files, so that nothing that lists the connectors takes it for one.
func pendingFile(id string) string { return "." + id + pendingExt }

// markerID returns the tunnel id in the name of a marker file.
func markerID(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, ".")
	if !ok {
		return "", false
	}
	return strings.CutSuffix(rest, pendingExt)
}

// pendingState is what is known of the marker of a tunnel at one time.
type pendingState struct {
	present bool
	info    fs.FileInfo // nil when the marker is there but could not be examined
}

// sameAs reports whether both describe the one marker file. A marker that
// could not be examined can only be told to be the same as another one that
// could not.
func (a pendingState) sameAs(b pendingState) bool {
	switch {
	case !a.present || !b.present:
		return false
	case a.info == nil || b.info == nil:
		return a.info == nil && b.info == nil
	}
	return os.SameFile(a.info, b.info)
}

// statPending examines the marker of a tunnel. A marker that cannot be told to
// be absent counts as there: an extra restart is better than a stale token.
func (m *Manager) statPending(id string) pendingState {
	info, err := m.lstat(m.path(pendingFile(id)))
	switch {
	case err == nil:
		return pendingState{present: true, info: info}
	case errors.Is(err, fs.ErrNotExist):
		return pendingState{}
	}
	return pendingState{present: true}
}

func (m *Manager) markPending(id string) error {
	if m.statPending(id).present {
		return nil
	}
	return writeAtomic(m.path(pendingFile(id)), nil, pendingMode)
}

// apply starts a unit that does not run, restarts one whose files changed,
// and then clears the marker of the change.
//
// A marker that cannot be cleared must not turn into a restart on every call.
// The marker for which a start or restart was queued is remembered, and while
// it is still the same one, a call that wrote no file leaves the unit alone and
// returns the error from clearing it, so that the problem stays visible. A call
// that replaced a file restarts regardless: its change is new. A unit that is
// not running is always started.
func (m *Manager) apply(ctx context.Context, id string, wrote bool) error {
	unit := UnitName(id)
	active, err := m.sd.IsActive(ctx, unit)
	if err != nil {
		return fmt.Errorf("checking %s: %w", unit, err)
	}
	marker := m.statPending(id)
	if !marker.present {
		delete(m.queued, id)
	}
	switch {
	case !active:
		if err := m.sd.EnableNow(ctx, unit); err != nil {
			return fmt.Errorf("starting %s: %w", unit, err)
		}
		m.log.Info().Str("tunnel", id).Msg("started connector")
	case marker.present && (wrote || !marker.sameAs(m.queued[id])):
		if err := m.sd.Restart(ctx, unit); err != nil {
			return fmt.Errorf("restarting %s: %w", unit, err)
		}
		m.log.Info().Str("tunnel", id).Msg("restarted connector after a change")
	case !marker.present:
		return nil
	}
	if marker.present {
		m.queued[id] = marker
	}
	if err := m.clearPending(id); err != nil {
		return fmt.Errorf("tunnel %s: clearing the pending marker: %w", id, err)
	}
	delete(m.queued, id)
	return nil
}

func (m *Manager) clearPending(id string) error {
	if err := os.Remove(m.path(pendingFile(id))); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
