package engine

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"time"
)

// MaxEventLimit is the most events a query answers with.
const MaxEventLimit = 5000

const (
	defaultEventLimit = 1000
	// maxHistory is how much of the event log on disk a query reads, the
	// newest first.
	maxHistory = 10 << 20
)

// EventQuery asks for the events that pass every filter it sets. Of a list,
// an event has to have one of the values, exactly; an event that is about no
// route, for one, is of none.
type EventQuery struct {
	Since time.Time
	// Until is the time of the last event that passes; the zero time sets
	// no end. The limit counts back from it, so that a range in the past
	// gets the newest events of that range.
	Until time.Time
	// After is the seq of the last event the client has, of Boot or, when
	// Boot is empty, of this process. After a seq of another boot every
	// event passes.
	After                                      uint64
	Boot                                       string
	Route, Guest, Tunnel, Account, Kind, Level []string
	Limit                                      int  // default 1000, at most 5000
	History                                    bool // also read events.log and events.log.1
}

// Match reports whether ev happened after Since and not after Until, and has
// a value of every list of q that is not empty. After is QueryEvents' to
// apply: it depends on the boot.
func (q EventQuery) Match(ev Event) bool {
	if !ev.At.After(q.Since) || !q.Until.IsZero() && ev.At.After(q.Until) {
		return false
	}
	for _, f := range [...]struct {
		want []string
		have string
	}{
		{q.Route, ev.Route}, {q.Guest, ev.Guest}, {q.Tunnel, ev.Tunnel},
		{q.Account, ev.Account}, {q.Kind, ev.Kind}, {q.Level, ev.Level},
	} {
		if len(f.want) > 0 && (f.have == "" || !slices.Contains(f.want, f.have)) {
			return false
		}
	}
	return true
}

// QueryEvents returns the newest events that pass the query, at most its
// limit, oldest first. They come from the ring, and with History also from
// the newest 10 MiB of events.log.1 and events.log, where a line that is not
// an event is skipped.
func (e *Engine) QueryEvents(q EventQuery) ([]Event, error) {
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = defaultEventLimit
	case limit > MaxEventLimit:
		limit = MaxEventLimit
	}
	after := q.After
	if q.Boot != "" && q.Boot != e.boot {
		after = 0
	}
	keep := func(ev Event) bool {
		return q.Match(ev) && (after == 0 || ev.Boot == e.boot && ev.Seq > after)
	}

	ring, disk, err := e.events.view(q.History)
	if err != nil {
		return nil, err
	}
	defer disk.close()
	var out []Event
	if q.History {
		// The ring holds the newest events of this process; the files have
		// them as well.
		inRing := func(ev Event) bool { return len(ring) > 0 && ev.Boot == e.boot && ev.Seq >= ring[0].Seq }
		if out, err = disk.read(func(ev Event) bool { return !inRing(ev) && keep(ev) }, limit); err != nil {
			return nil, err
		}
	}
	for _, ev := range ring {
		if keep(ev) {
			out = append(out, ev)
		}
	}
	return nonNil(newest(out, limit)), nil
}

// newest is the last limit of events.
func newest(events []Event, limit int) []Event {
	if len(events) > limit {
		return slices.Clone(events[len(events)-limit:])
	}
	return events
}

// view returns a copy of the ring and, when asked, the files of the event log
// as they are with it: each was written up to its size by then.
func (l *eventLog) view(files bool) ([]Event, history, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ring := slices.Clone(l.ring)
	if !files || l.path == "" {
		return ring, history{}, nil
	}
	var h history
	for _, path := range []string{l.path + ".1", l.path} {
		f, err := os.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil {
			var info fs.FileInfo
			if info, err = f.Stat(); err == nil {
				h.files, h.sizes = append(h.files, f), append(h.sizes, info.Size())
				continue
			}
			_ = f.Close()
		}
		h.close()
		return nil, history{}, fmt.Errorf("reading the event log: %w", err)
	}
	return ring, h, nil
}

// history is the event log on disk as one query found it: the files, oldest
// first, and how much of each was written then.
type history struct {
	files []*os.File
	sizes []int64
}

func (h history) close() {
	for _, f := range h.files {
		_ = f.Close()
	}
}

// read returns the newest limit events of the files that keep takes, oldest
// first, from the newest maxHistory bytes.
func (h history) read(keep func(Event) bool, limit int) ([]Event, error) {
	parts := make([][]Event, len(h.files))
	budget := int64(maxHistory)
	for i := len(h.files) - 1; i >= 0 && budget > 0; i-- {
		from := max(0, h.sizes[i]-budget)
		budget -= h.sizes[i] - from
		events, err := readEvents(h.files[i], from, h.sizes[i], keep, limit)
		if err != nil {
			return nil, fmt.Errorf("reading the event log: %w", err)
		}
		parts[i] = events
	}
	return newest(slices.Concat(parts...), limit), nil
}

// readEvents reads the lines of f from from to size and keeps the newest
// limit events that keep takes. A read that begins within the file begins a
// byte early, so that the line it cuts is known and left out.
func readEvents(f *os.File, from, size int64, keep func(Event) bool, limit int) ([]Event, error) {
	cut := from > 0
	if cut {
		from--
	}
	r := bufio.NewReader(io.NewSectionReader(f, from, size-from))
	var out []Event
	for {
		line, err := r.ReadBytes('\n')
		var ev Event
		switch {
		case cut:
			cut = false
		case json.Unmarshal(line, &ev) == nil && ev.Seq > 0 && keep(ev):
			out = append(out, ev)
			if len(out) >= 2*limit {
				out = slices.Delete(out, 0, len(out)-limit)
			}
		}
		if errors.Is(err, io.EOF) {
			return newest(out, limit), nil
		}
		if err != nil {
			return nil, err
		}
	}
}
