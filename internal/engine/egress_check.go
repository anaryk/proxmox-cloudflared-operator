package engine

import (
	"fmt"
	"slices"
	"time"
)

// The words of what a check of the egress table found.
const (
	problemReloaded = "the egress table was changed or removed outside pco and was loaded again"
	problemNotKept  = "the egress table was changed or removed outside pco and could not be loaded again: %s; " +
		"no tunnel configuration is written until it is"
)

// EgressCheck is what the daemon found at one of its checks of the egress
// table.
type EgressCheck struct {
	// View is the filter as the check found it, or as it left it after it
	// loaded the table again; empty when the check could not tell.
	View EgressView
	// Reloaded says what differed when the table was gone, dormant or not the
	// one pco applied, and was loaded again.
	Reloaded string
	// Failed says why the check could not list the table, or could not load
	// it again; Holds says it is the latter, which holds the tunnel runs
	// until a check finds the table in place.
	Failed string
	Holds  bool
}

// egressNotes is what the checks of the egress table found between two
// cycles. It is guarded by noteMu.
type egressNotes struct {
	view  EgressView
	lines []string // problem lines for the next cycle
	fault string   // why the table could not be loaded again; holds the tunnel runs
}

// NoteEgress takes what a check of the egress table found. The state shows
// the filter as it is at once; a switch off or on, a table loaded again and
// one that could not be are events, and problem lines of the next cycle.
// While the table could not be loaded again, no tunnel configuration is
// written.
func (e *Engine) NoteEgress(c EgressCheck) {
	now := e.d.Now()
	var events []Event
	add := func(level, msg string) {
		events = append(events, Event{At: now, Level: level, Kind: kindEgress, Subject: "egress", Message: msg})
	}

	e.noteMu.Lock()
	prev := e.notes.view
	if c.View.State != "" {
		e.notes.view = c.View
		if !c.Holds {
			e.notes.fault = ""
		}
	}
	switch next := e.notes.view.State; {
	case next == prev.State:
	case next == EgressOff:
		add(levelWarn, fmt.Sprintf("the egress filter was switched off (since %s); the connectors are not confined until pco egress on",
			sinceText(c.View.Since)))
	case prev.State == EgressOff:
		add(levelInfo, "the egress filter was switched on")
	}
	if c.Reloaded != "" {
		add(levelWarn, problemReloaded)
		e.notes.lines = appendOnce(e.notes.lines, problemReloaded+" ("+c.Reloaded+")")
	}
	switch {
	case c.Holds && c.Failed != e.notes.fault:
		e.notes.fault = c.Failed
		add(levelError, fmt.Sprintf(problemNotKept, c.Failed))
	case c.Failed != "" && !c.Holds:
		e.notes.lines = appendOnce(e.notes.lines, "checking the egress table: "+c.Failed)
	}
	view := e.notes.view
	e.noteMu.Unlock()

	e.stateMu.Lock()
	e.state.Egress = view
	e.stateMu.Unlock()
	e.events.add(events...)
}

// noteEgress gives the state of the cycle the problem lines the checks of
// the egress table left for it, and the one of a table that could not be
// loaded again, for as long as that stands.
func (c *cycleRun) noteEgress() {
	c.e.noteMu.Lock()
	defer c.e.noteMu.Unlock()
	c.st.Problems = append(c.st.Problems, c.e.notes.lines...)
	c.e.notes.lines = nil
	if fault := c.e.notes.fault; fault != "" {
		c.problem(problemNotKept, fault)
	}
}

// egressFault says why the egress table could not be loaded again, while
// that stands.
func (e *Engine) egressFault() string {
	e.noteMu.Lock()
	defer e.noteMu.Unlock()
	return e.notes.fault
}

func appendOnce(lines []string, line string) []string {
	if slices.Contains(lines, line) {
		return lines
	}
	return append(lines, line)
}

// sinceText is a time of the switch, or that it is not known.
func sinceText(t time.Time) string {
	if t.IsZero() {
		return "an unknown time"
	}
	return t.UTC().Format(time.RFC3339)
}
