package engine

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

// kindEnrol is the event kind of the enrolment of a node: a certificate of
// a node the controller took, and the registry following it.
const kindEnrol = "enrol"

// maxAgentText bounds what one field of an agent's event may hold in the
// controller's log: an agent's report is its own word, and the log is
// everyone's.
const maxAgentText = 2048

// agentEventWire is an event as the status of an agent carries it.
type agentEventWire struct {
	Cursor  uint64    `json:"cursor"`
	At      time.Time `json:"at"`
	Level   string    `json:"level"`
	Kind    string    `json:"kind"`
	Subject string    `json:"subject"`
	Message string    `json:"message"`
	Tunnel  string    `json:"tunnel"`
}

// takenEvents is, by node, what the controller took of the events of its
// agent: the epoch they were sent to, the start of the agent that sent them
// and the cursor of the last one taken. An event at or below that cursor
// was taken already, so a status sent again after its answer was lost adds
// nothing, and so does the resend of the last 24 hours, which an agent
// sends once to every epoch.
type takenEvents struct {
	mu    sync.Mutex
	nodes map[string]takenOf
}

type takenOf struct {
	epoch   agentproto.Epoch
	started time.Time
	cursor  uint64
}

// TakeAgentEvents puts the events of a status of node's agent into the
// log, each tagged with the node, and returns the cursor to acknowledge in
// the heartbeat: that of the last event the status carried. An event that
// cannot be read is skipped and acknowledged with the others. The answers
// to doctor actions the status carries are taken as it comes, for the
// doctor that waits for them.
func (e *Engine) TakeAgentEvents(node string, st agentproto.Status, epoch agentproto.Epoch) uint64 {
	e.asks.take(node, st.Actions)
	t := &e.taken
	t.mu.Lock()
	if t.nodes == nil {
		t.nodes = map[string]takenOf{}
	}
	had := t.nodes[node]
	if had.epoch != epoch || !had.started.Equal(st.AgentStartedAt) {
		had = takenOf{epoch: epoch, started: st.AgentStartedAt}
	}
	var events []Event
	for _, raw := range st.Events {
		var w agentEventWire
		if err := json.Unmarshal(raw, &w); err != nil || w.Cursor <= had.cursor {
			continue
		}
		had.cursor = w.Cursor
		events = append(events, Event{
			At: w.At, Level: agentLevel(w.Level), Kind: agentKind(clip(w.Kind)), Subject: clip(w.Subject),
			Message: clip(w.Message), Tunnel: clip(w.Tunnel), Node: node,
		})
	}
	t.nodes[node] = had
	t.mu.Unlock()
	e.events.addTaken(events...)
	return st.EventCursor
}

// agentLevel is the level of an agent's event as the log knows them.
func agentLevel(level string) string {
	switch level {
	case levelInfo, levelWarn, levelError:
		return level
	}
	return levelInfo
}

func clip(s string) string {
	if len(s) > maxAgentText {
		return strings.ToValidUTF8(s[:maxAgentText], "")
	}
	return s
}

// NoteNode keeps an event about node that happened outside a cycle, as a
// status of its agent the controller refused.
func (e *Engine) NoteNode(node, msg string) {
	e.events.add(Event{At: e.d.Now(), Level: levelWarn, Kind: kindNode, Subject: node, Message: clip(msg)})
}

// NoteEnrol keeps an event of the enrolment of node, which the check of the
// agent endpoint made: it is of the audit trail.
func (e *Engine) NoteEnrol(node, msg string) {
	e.events.add(Event{At: e.d.Now(), Level: levelInfo, Kind: kindEnrol, Subject: node, Message: clip(msg), Security: true})
}
