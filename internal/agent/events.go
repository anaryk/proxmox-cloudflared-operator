package agent

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

const (
	// eventsResent is how far back the events go that an agent sends again
	// to the controller of a new epoch, so that the merged log survives a
	// failover while the old node is up.
	eventsResent = 24 * time.Hour
	// eventsKept bounds the events the log holds, whatever their age.
	eventsKept = 2 * agentproto.MaxResend
)

// eventWire is an event as a status carries it.
type eventWire struct {
	Cursor  uint64    `json:"cursor"`
	At      time.Time `json:"at"`
	Level   string    `json:"level"`
	Kind    string    `json:"kind"`
	Subject string    `json:"subject,omitempty"`
	Message string    `json:"message"`
	Tunnel  string    `json:"tunnel,omitempty"`
}

// EventLog holds the agent's events until the controller acknowledged them.
// Each event gets a cursor; a status carries the events after the cursor the
// controller last acknowledged, and the cursor moves only on the
// acknowledgement in its heartbeat, so a status that is lost is sent again.
// The controller of a new epoch gets the events of the last 24 hours again,
// at most the newest agentproto.MaxResend. Its methods are safe for
// concurrent use; Add is what Deps.Event takes.
type EventLog struct {
	now func() time.Time

	mu       sync.Mutex
	events   []eventWire // oldest first
	next     uint64      // the cursor of the last event added
	acked    uint64      // every event up to it was acknowledged
	epoch    agentproto.Epoch
	resendTo uint64 // while a resend runs, the cursor of its last event
	resendN  int
}

// NewEventLog returns an empty log.
func NewEventLog(now func() time.Time) *EventLog {
	if now == nil {
		now = time.Now
	}
	return &EventLog{now: now}
}

// Add keeps an event to send.
func (l *EventLog) Add(ev Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	l.events = append(l.events, eventWire{
		Cursor: l.next, At: ev.At, Level: ev.Level, Kind: ev.Kind, Subject: ev.Subject, Message: ev.Message, Tunnel: ev.Tunnel,
	})
	l.trim()
}

// trim drops the events past 24 hours and past the bound. The caller holds
// the lock.
func (l *EventLog) trim() {
	cut := l.now().Add(-eventsResent)
	drop := 0
	for drop < len(l.events) && (len(l.events)-drop > eventsKept || l.events[drop].At.Before(cut)) {
		drop++
	}
	if drop > 0 {
		l.events = append([]eventWire(nil), l.events[drop:]...)
	}
}

// Batch returns what the next status carries: the events after the
// acknowledged cursor, at most agentproto.MaxEventBatch, the cursor of the
// last of them (the acknowledged one when there are none), and while a
// resend runs how many events it holds.
func (l *EventLog) Batch() (events []json.RawMessage, cursor uint64, resend int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cursor = l.acked
	for _, ev := range l.events {
		if ev.Cursor <= l.acked {
			continue
		}
		if len(events) == agentproto.MaxEventBatch {
			break
		}
		raw, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		events = append(events, raw)
		cursor = ev.Cursor
	}
	if l.acked < l.resendTo {
		resend = l.resendN
	}
	return events, cursor, resend
}

// Ack takes the controller's heartbeat: the cursor it acknowledged, which
// moves the log on only up to what was sent, and its epoch. A heartbeat of
// another epoch than the one the log sent to makes it send the last 24
// hours again; the first controller the agent hears gets what is pending.
func (l *EventLog) Ack(epoch agentproto.Epoch, cursor uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.epoch == (agentproto.Epoch{}) {
		l.epoch = epoch
	}
	if epoch != l.epoch {
		l.epoch = epoch
		l.trim()
		recent := l.events
		if len(recent) > agentproto.MaxResend {
			recent = recent[len(recent)-agentproto.MaxResend:]
		}
		l.resendTo, l.resendN = 0, 0
		if len(recent) > 0 {
			l.acked = recent[0].Cursor - 1
			l.resendTo, l.resendN = recent[len(recent)-1].Cursor, len(recent)
		}
		return
	}
	if cursor > l.acked && cursor <= l.next {
		l.acked = cursor
	}
}

// ResendRefused ends a resend the controller refused as too large: the
// events of it not acknowledged yet are given up, and the newer ones go as
// ordinary events.
func (l *EventLog) ResendRefused() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.resendTo > l.acked {
		l.acked = l.resendTo
	}
	l.resendTo, l.resendN = 0, 0
}

// Skip gives up the events up to cursor, which made a status the controller
// refused as too large.
func (l *EventLog) Skip(cursor uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cursor > l.acked && cursor <= l.next {
		l.acked = cursor
	}
}
