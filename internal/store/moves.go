package store

import (
	"errors"
	"fmt"
	"time"
)

// The steps of a Move, in their order.
const (
	MoveRulesAdded     = "rules-added"     // the rules are in the tunnel moved to
	MoveRolledOut      = "rolled-out"      // its connectors run the configuration with them
	MoveCNAMEsSwitched = "cnames-switched" // the records point at the tunnel moved to
)

// Move is the intent to move the routes of an owner, a guest or a manual
// route, from one tunnel to another. It is kept so that a controller that
// takes over in the middle aborts before the records were switched and goes
// on after.
type Move struct {
	Owner string `json:"owner"`
	// From and To are the names of the tunnels.
	From string    `json:"from"`
	To   string    `json:"to"`
	Step string    `json:"step"`
	At   time.Time `json:"at"`
	// CNAMEAt is the time the records were switched, as Cloudflare says;
	// the old rules go some minutes after it.
	CNAMEAt time.Time `json:"cnameAt,omitzero"`
}

func (m Move) check() error {
	switch {
	case m.Owner == "":
		return errors.New("move has no owner")
	case m.From == "":
		return fmt.Errorf("move of %s has no tunnel to move from", m.Owner)
	case m.To == "":
		return fmt.Errorf("move of %s has no tunnel to move to", m.Owner)
	case m.From == m.To:
		return fmt.Errorf("move of %s is to the same tunnel %s", m.Owner, m.To)
	case m.Step != MoveRulesAdded && m.Step != MoveRolledOut && m.Step != MoveCNAMEsSwitched:
		return fmt.Errorf("move of %s: step %q: want %q, %q or %q", m.Owner, m.Step, MoveRulesAdded, MoveRolledOut, MoveCNAMEsSwitched)
	case m.At.IsZero():
		return fmt.Errorf("move of %s has no time of its step", m.Owner)
	}
	return nil
}

// Moves returns the moves under way, by owner.
func (s *Store) Moves() (map[string]Move, error) {
	return loadMap(s.cluster, kindMoves, "owner", func(m *Move) *string { return &m.Owner })
}

// SaveMove stores a move under its owner, unless it is what is stored
// already.
func (s *Store) SaveMove(m Move) error {
	if err := m.check(); err != nil {
		return err
	}
	return s.cluster.put(kindMoves, m.Owner, m, true)
}

// DeleteMove removes the move of an owner, once it is done or given up. A
// missing one is not an error.
func (s *Store) DeleteMove(owner string) error { return s.cluster.Delete(kindMoves, owner) }
