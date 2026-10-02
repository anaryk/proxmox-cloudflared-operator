package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

const (
	maxEvents    = 1000
	eventsFile   = "events.log"
	maxEventsLog = 5 << 20

	levelInfo  = "info"
	levelWarn  = "warn"
	levelError = "error"

	kindRoute    = "route"
	kindConflict = "conflict"
	kindAction   = "action"
	kindProblem  = "problem"
	kindClaim    = "claim"
	kindRollout  = "rollout"
	kindWriter   = "writer"
	kindAdmin    = "admin"
)

// Event is something that changed, as the event log keeps it.
type Event struct {
	// Seq numbers the events of a process from 1, in the order they were
	// made: a client resumes from the last one it saw.
	Seq     uint64    `json:"seq"`
	At      time.Time `json:"at,omitzero"`
	Level   string    `json:"level"` // "info", "warn", "error"
	Kind    string    `json:"kind"`
	Subject string    `json:"subject"`
	Message string    `json:"message"`
}

// eventLog keeps the last maxEvents events in memory and appends every event
// to a file as one JSON line. The file is rotated at maxEventsLog, keeping one
// previous file.
type eventLog struct {
	mu   sync.Mutex
	ring []Event // oldest first
	seq  uint64  // of the last event
	path string  // empty: memory only
	log  zerolog.Logger
}

func newEventLog(dir string, log zerolog.Logger) *eventLog {
	l := &eventLog{log: log}
	if dir != "" {
		l.path = filepath.Join(dir, eventsFile)
	}
	return l
}

func (l *eventLog) add(events ...Event) {
	if len(events) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	events = slices.Clone(events)
	for i := range events {
		l.seq++
		events[i].Seq = l.seq
	}
	l.ring = append(l.ring, events...)
	if n := len(l.ring) - maxEvents; n > 0 {
		l.ring = slices.Delete(l.ring, 0, n)
	}
	if l.path == "" {
		return
	}
	if err := l.append(events); err != nil {
		l.log.Warn().Err(err).Str("file", l.path).Msg("could not write the event log")
	}
}

// append writes events to the file, rotating it first when they would take it
// over the limit. The caller holds the lock.
func (l *eventLog) append(events []Event) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			return err
		}
	}
	info, err := os.Stat(l.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case info.Size() > 0 && info.Size()+int64(buf.Len()) > maxEventsLog:
		if err := os.Rename(l.path, l.path+".1"); err != nil {
			return fmt.Errorf("rotating: %w", err)
		}
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(buf.Bytes())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// since returns the events after t, oldest first. The clock may have stepped
// back between two events, so every event is looked at.
func (l *eventLog) since(t time.Time) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []Event{}
	for _, ev := range l.ring {
		if ev.At.After(t) {
			out = append(out, ev)
		}
	}
	return out
}

// changes lists what differs between two states: routes whose state changed,
// conflicts that appeared or cleared, actions applied, a writer verdict that
// changed and problems that appeared.
func changes(prev, next State) []Event {
	var out []Event
	out = append(out, routeChanges(prev, next)...)
	out = append(out, conflictChanges(prev, next)...)
	for _, a := range next.Actions {
		if a.Applied {
			out = append(out, Event{At: next.At, Level: levelInfo, Kind: kindAction, Subject: a.Target, Message: actionText(a)})
		}
	}
	if next.WriterVerdict != prev.WriterVerdict {
		level := levelError
		if next.WriterVerdict == VerdictOK {
			level = levelInfo
		}
		out = append(out, Event{At: next.At, Level: level, Kind: kindWriter, Subject: "leader.json", Message: "writer verdict is " + next.WriterVerdict})
	}
	for _, p := range next.Problems {
		if !slices.Contains(prev.Problems, p) {
			out = append(out, Event{At: next.At, Level: levelWarn, Kind: kindProblem, Message: p})
		}
	}
	return out
}

func routeChanges(prev, next State) []Event {
	type key struct{ host, owner string }
	before := make(map[key]RouteView, len(prev.Routes))
	for _, r := range prev.Routes {
		before[key{r.Hostname, r.Owner}] = r
	}
	var out []Event
	after := make(map[key]bool, len(next.Routes))
	for _, r := range next.Routes {
		k := key{r.Hostname, r.Owner}
		after[k] = true
		if old, ok := before[k]; ok && old.State == r.State {
			continue
		}
		level := levelWarn
		if r.State == planner.StateActive {
			level = levelInfo
		}
		msg := fmt.Sprintf("%s: %s", r.Owner, r.State)
		if r.Reason != "" {
			msg += " (" + r.Reason + ")"
		}
		out = append(out, Event{At: next.At, Level: level, Kind: kindRoute, Subject: r.Hostname, Message: msg})
	}
	for _, r := range prev.Routes {
		if !after[key{r.Hostname, r.Owner}] {
			out = append(out, Event{At: next.At, Level: levelInfo, Kind: kindRoute, Subject: r.Hostname, Message: r.Owner + ": no longer routed"})
		}
	}
	return out
}

func conflictChanges(prev, next State) []Event {
	var out []Event
	for _, c := range next.Conflicts {
		if !slices.Contains(prev.Conflicts, c) {
			out = append(out, Event{At: next.At, Level: levelWarn, Kind: kindConflict, Subject: c.Name,
				Message: fmt.Sprintf("%s %s in zone %s is not ours; the hostname is not published", c.Type, c.Content, c.Zone)})
		}
	}
	for _, c := range prev.Conflicts {
		if !slices.Contains(next.Conflicts, c) {
			out = append(out, Event{At: next.At, Level: levelInfo, Kind: kindConflict, Subject: c.Name,
				Message: fmt.Sprintf("%s %s in zone %s no longer conflicts", c.Type, c.Content, c.Zone)})
		}
	}
	return out
}

func actionText(a reconcile.Action) string {
	if a.Detail == "" {
		return string(a.Kind)
	}
	return string(a.Kind) + " " + a.Detail
}

// claimEvents reports the changes ResolveClaims made.
func claimEvents(at time.Time, events []planner.ClaimEvent) []Event {
	out := make([]Event, 0, len(events))
	for _, ev := range events {
		level := levelInfo
		if ev.Kind == planner.ClaimConflict {
			level = levelWarn
		}
		out = append(out, Event{At: at, Level: level, Kind: kindClaim, Subject: ev.Hostname,
			Message: fmt.Sprintf("%s %s: %s", ev.Kind, ev.Owner, ev.Detail)})
	}
	return out
}
