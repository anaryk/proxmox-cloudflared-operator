package store

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"
)

// MaxSecurityEvents is how many security events the file of one node keeps:
// the newest.
const MaxSecurityEvents = 500

// SecurityEvent is an event of the audit trail: a join token issued or used,
// an admission, a forget, a promotion, a hand-off, a rotation, an epoch
// drawn, a takeover, an admin action. The journal of the node that acted
// keeps it too; the cluster root keeps the last ones of every node, so that
// the trail survives the loss of the node that held the lease.
type SecurityEvent struct {
	At      time.Time `json:"at"`
	Level   string    `json:"level,omitempty"` // info, warn or error; empty is info
	Kind    string    `json:"kind"`
	Subject string    `json:"subject,omitempty"`
	Message string    `json:"message"`
	Actor   string    `json:"actor,omitempty"`
	// Node is the node that acted, whose file holds the event.
	Node string `json:"node"`
}

// securityLog is the file of the security events of one node, oldest first.
type securityLog struct {
	Node   string          `json:"node"`
	Events []SecurityEvent `json:"events"`
}

// AppendSecurityEvent adds an event to the file of node, which keeps the
// newest MaxSecurityEvents, and fewer when they would not fit in a file of
// the cluster filesystem. Only the node itself writes its file, so no two
// nodes ever write one file of the cluster root at once; an event of another
// node is refused.
func (s *Store) AppendSecurityEvent(node string, e SecurityEvent) error {
	return s.AppendSecurityEvents(node, []SecurityEvent{e})
}

// AppendSecurityEvents is AppendSecurityEvent for several events, oldest
// first, in one read and one write of the file.
func (s *Store) AppendSecurityEvents(node string, add []SecurityEvent) error {
	if err := checkNodeName("security event", node); err != nil {
		return err
	}
	add = slices.Clone(add)
	for i, e := range add {
		switch {
		case e.Node != "" && e.Node != node:
			return fmt.Errorf("security event of node %s cannot go into the file of node %s", e.Node, node)
		case e.Kind == "":
			return errors.New("security event has no kind")
		case e.At.IsZero():
			return errors.New("security event has no time")
		}
		add[i].Node = node
	}
	if len(add) == 0 {
		return nil
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	var log securityLog
	if _, err := s.cluster.Get(kindSecurityEvents, node, &log); err != nil {
		return err
	}
	events := append(slices.Clone(log.Events), add...)
	if len(events) > MaxSecurityEvents {
		events = events[len(events)-MaxSecurityEvents:]
	}
	events, err := s.fitSecurityLog(node, events)
	if err != nil {
		return err
	}
	return s.cluster.put(kindSecurityEvents, node, securityLog{Node: node, Events: events}, true)
}

// fitSecurityLog drops the oldest of events until the file of node holds
// them within the limit of a file.
func (s *Store) fitSecurityLog(node string, events []SecurityEvent) ([]SecurityEvent, error) {
	_, path, err := s.cluster.file(kindSecurityEvents, node)
	if err != nil {
		return nil, err
	}
	limit := s.cluster.limit()
	var failed error
	fits := func(drop int) bool {
		data, err := json.Marshal(securityLog{Node: node, Events: events[drop:]})
		if err == nil {
			var out []byte
			// The revision only grows; one of the widest ones measures it.
			out, err = encodeFile(path, 1<<62, node, data)
			if err == nil {
				return len(out) <= limit
			}
		}
		failed = err
		return true
	}
	if fits(0) && failed == nil {
		return events, nil
	}
	drop := sort.Search(len(events), fits)
	switch {
	case failed != nil:
		return nil, fmt.Errorf("encoding the security events of %s: %w", node, failed)
	case drop == len(events):
		return nil, fmt.Errorf("a security event of %s is over the %d byte limit of a file", node, limit)
	}
	return events[drop:], nil
}

// enrolmentKind is the kind the agent endpoint gave the enrolments it
// recorded before the engine's audit trail wrote them as enrol.
const enrolmentKind = "enrolment"

// SecurityEvents returns the security events of every node, merged by time,
// the oldest first. A file that cannot be read is left out, as
// SecurityTrail says.
func (s *Store) SecurityEvents() ([]SecurityEvent, error) {
	out, _, err := s.SecurityTrail()
	return out, err
}

// SecurityTrail returns the security events of every node, merged by time,
// the oldest first, and why each file it left out could not be read: one
// damaged file of one node does not hide the trail of the others. Each event
// is of the node whose file holds it, whatever it says, and an enrolment
// recorded under its earlier kind is enrol.
func (s *Store) SecurityTrail() (events []SecurityEvent, skipped []string, err error) {
	names, err := s.cluster.listObjects(kindSecurityEvents)
	if err != nil {
		return nil, nil, err
	}
	events = []SecurityEvent{}
	for _, id := range names {
		var log securityLog
		if _, err := s.cluster.Get(kindSecurityEvents, id, &log); err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		if log.Node != id {
			skipped = append(skipped, fmt.Sprintf("the file of %s holds the security events of %q", id, log.Node))
			continue
		}
		for _, e := range log.Events {
			e.Node = log.Node
			if e.Kind == enrolmentKind {
				e.Kind = "enrol"
			}
			events = append(events, e)
		}
	}
	slices.SortStableFunc(events, func(a, b SecurityEvent) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.Node, b.Node))
	})
	return events, skipped, nil
}
