package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

// The objects of the agent of a node, in the kind agent of its local root:
// the desired state it follows, the epoch it remembers and the actions it
// ran. The agent acts on each only once it is on disk, so they are written
// durably wherever the local root is.
const (
	idAgentDesired = "desired"
	idAgentEpoch   = "epoch"
	idAgentActions = "actions"

	// maxGeneration is the highest generation of an epoch: one above it was
	// forged, as no draw makes it.
	maxGeneration = 1 << 53
)

// Epoch is the epoch an agent follows: the install, the generation and the
// nonce of the writer it accepts desired state from.
type Epoch struct {
	InstallID  string `json:"installId"`
	Generation int    `json:"generation"`
	Nonce      string `json:"nonce"`
}

// ActionDone is an action of the controller the agent ran, so that a restart
// does not run it twice.
type ActionDone struct {
	Kind  string    `json:"kind"`
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// agent is the local root, writing durably.
func (s *Store) agent() Dir {
	d := s.local
	d.durable = true
	return d
}

// AgentSnapshot returns the desired state the agent last accepted, as it
// came. It holds run tokens, and is kept only on the node.
func (s *Store) AgentSnapshot() (json.RawMessage, bool, error) {
	var raw json.RawMessage
	found, err := s.local.Get(kindAgent, idAgentDesired, &raw)
	if err != nil || !found {
		return nil, false, err
	}
	return raw, true, nil
}

// SaveAgentSnapshot stores the desired state the agent accepted, unless it
// is what is stored already, which an idle controller sends again.
func (s *Store) SaveAgentSnapshot(raw json.RawMessage) error {
	if !json.Valid(raw) || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("agent snapshot: not a JSON value")
	}
	return s.agent().put(kindAgent, idAgentDesired, raw, true)
}

// AgentEpoch returns the epoch the agent remembers.
func (s *Store) AgentEpoch() (Epoch, bool, error) {
	return getOne[Epoch](s.local, kindAgent, idAgentEpoch)
}

// SaveAgentEpoch stores the epoch the agent follows. Whether it may follow
// it, being lower than the one remembered, is the agent's to judge.
func (s *Store) SaveAgentEpoch(e Epoch) error {
	switch {
	case e.InstallID == "" || e.Nonce == "":
		return errors.New("agent epoch needs an install id and a nonce")
	case e.Generation < 0 || e.Generation > maxGeneration:
		return fmt.Errorf("agent epoch: generation %d is not 0 to %d", e.Generation, maxGeneration)
	}
	return s.agent().put(kindAgent, idAgentEpoch, e, true)
}

// DeleteAgentEpoch forgets the epoch the agent remembers. A missing one is
// not an error.
func (s *Store) DeleteAgentEpoch() error { return s.agent().Delete(kindAgent, idAgentEpoch) }

// AgentActions returns the actions the agent ran, by id.
func (s *Store) AgentActions() (map[string]ActionDone, error) {
	out := make(map[string]ActionDone)
	if _, err := s.local.Get(kindAgent, idAgentActions, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = make(map[string]ActionDone)
	}
	return out, nil
}

// SaveAgentActions stores the actions the agent ran, unless they are what is
// stored already.
func (s *Store) SaveAgentActions(done map[string]ActionDone) error {
	for _, id := range slices.Sorted(maps.Keys(done)) {
		switch a := done[id]; {
		case id == "":
			return errors.New("agent action has no id")
		case a.Kind == "":
			return fmt.Errorf("agent action %s has no kind", id)
		}
	}
	if done == nil {
		done = map[string]ActionDone{}
	}
	return s.agent().put(kindAgent, idAgentActions, done, true)
}
