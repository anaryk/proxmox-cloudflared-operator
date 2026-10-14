package store

import (
	"errors"
	"time"
)

// idObserve is meta/observe: since when the controller is in observe-only
// mode, which the settings keep as a flag alone.
const idObserve = "observe"

// ObserveSince is the start of the controller's observe-only window, kept
// apart from the settings, whose file the previous release decodes
// strictly.
type ObserveSince struct {
	Since time.Time `json:"since"`
}

// ObserveSince returns since when the controller is in observe-only mode,
// as recorded; found is false when nothing is.
func (s *Store) ObserveSince() (ObserveSince, bool, error) {
	return getOne[ObserveSince](s.cluster, kindMeta, idObserve)
}

// SaveObserveSince records the start of the observe-only window.
func (s *Store) SaveObserveSince(o ObserveSince) error {
	if o.Since.IsZero() {
		return errors.New("observe-only: no time it began")
	}
	return s.cluster.put(kindMeta, idObserve, o, true)
}

// DeleteObserveSince records that observe-only mode ended. A missing record
// is not an error.
func (s *Store) DeleteObserveSince() error { return s.cluster.Delete(kindMeta, idObserve) }
