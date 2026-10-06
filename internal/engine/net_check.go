package engine

import "fmt"

// The words of what a check of the appliance's service-prefix route and table
// found.
const (
	problemNetReloaded = "the service-prefix route or table was changed outside pco and was loaded again"
	problemNetNotKept  = "the service-prefix route or table was changed outside pco and could not be loaded again: %s"
	problemNetFailed   = "checking the service-prefix route and table: %s"
)

// NetCheck is what the daemon of the appliance found at one of its checks of
// what pco-net.service loads: the dummy device with its address, route and
// rules, and the table inet pco_net. The zero value is all of it in place.
type NetCheck struct {
	// Reloaded says what differed when it was loaded again.
	Reloaded string
	// NotKept says why it could not be loaded again, Failed why it could not
	// be checked.
	NotKept string
	Failed  string
}

// NoteNet takes what a check found. What was loaded again is an event and a
// problem line of the next cycle; what could not be is a problem line for as
// long as that stands, and an event when it starts. None of it holds a tunnel
// run: the egress table confines the connectors whatever happens to the
// route.
func (e *Engine) NoteNet(c NetCheck) {
	now := e.d.Now()
	var events []Event
	add := func(level, msg string) {
		events = append(events, Event{At: now, Level: level, Kind: kindEgress, Subject: "service prefix", Message: msg})
	}

	e.noteMu.Lock()
	switch {
	case c.Failed != "":
		e.notes.lines = appendOnce(e.notes.lines, fmt.Sprintf(problemNetFailed, c.Failed))
	case c.NotKept != "":
		if c.NotKept != e.notes.netFault {
			add(levelError, fmt.Sprintf(problemNetNotKept, c.NotKept))
		}
		e.notes.netFault = c.NotKept
	default:
		e.notes.netFault = ""
		if c.Reloaded != "" {
			add(levelWarn, problemNetReloaded)
			e.notes.lines = appendOnce(e.notes.lines, problemNetReloaded+" ("+c.Reloaded+")")
		}
	}
	e.noteMu.Unlock()
	e.events.add(events...)
}
