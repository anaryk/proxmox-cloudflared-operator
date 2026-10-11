package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"time"
)

// The objects of the agent of a node, in the kind agent of its local root:
// the desired state it follows, the epoch it remembers, the actions it ran
// and the targets it last gave the egress filter. The agent acts on each only
// once it is on disk, so they are written durably wherever the local root is.
const (
	idAgentDesired = "desired"
	idAgentEpoch   = "epoch"
	idAgentActions = "actions"
	idAgentEgress  = "egress"

	// maxGeneration is the highest generation of an epoch: one above it was
	// forged, as no draw makes it.
	maxGeneration = 1 << 53

	// MaxAgentActions is how many of the actions it ran the agent keeps: an
	// action is sent again only while it is in the desired state, which holds
	// far fewer.
	MaxAgentActions = 1000
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

// DeleteAgentSnapshot forgets the desired state the agent accepted, as a node
// that was removed from its install does. A missing one is not an error.
func (s *Store) DeleteAgentSnapshot() error { return s.agent().Delete(kindAgent, idAgentDesired) }

// AgentEgress returns the targets the agent last gave the egress filter, by
// address and port. found is false when it never saved any.
func (s *Store) AgentEgress() (set []netip.AddrPort, found bool, err error) {
	set, found, err = getOne[[]netip.AddrPort](s.local, kindAgent, idAgentEgress)
	if err != nil || !found {
		return nil, found, err
	}
	return sortedTargets(set), true, nil
}

// SaveAgentEgress stores the targets the agent gave the egress filter, sorted
// and once each, unless they are what is stored already: an agent that gives
// the filter the same set again writes nothing.
func (s *Store) SaveAgentEgress(set []netip.AddrPort) error {
	return s.agent().put(kindAgent, idAgentEgress, sortedTargets(set), true)
}

// AgentEpoch returns the epoch the agent remembers.
func (s *Store) AgentEpoch() (Epoch, bool, error) {
	return getOne[Epoch](s.local, kindAgent, idAgentEpoch)
}

// ErrEpochBack is the error of SaveAgentEpoch for an epoch below the one the
// agent remembers: a lower generation, the same one of another writer, or
// another install. The remembered epoch is what tells that a store went back,
// so it moves down only through DeleteAgentEpoch.
var ErrEpochBack = errors.New("the epoch is not above the one the agent remembers")

// SaveAgentEpoch stores the epoch the agent follows: the one it remembers, or
// a higher one of the same install; anything else is ErrEpochBack.
func (s *Store) SaveAgentEpoch(e Epoch) error {
	switch {
	case e.InstallID == "" || e.Nonce == "":
		return errors.New("agent epoch needs an install id and a nonce")
	case e.Generation < 0 || e.Generation > maxGeneration:
		return fmt.Errorf("agent epoch: generation %d is not 0 to %d", e.Generation, maxGeneration)
	}
	d := s.agent()
	var have Epoch
	rev, found, err := d.getRev(kindAgent, idAgentEpoch, &have, false)
	if err != nil {
		return err
	}
	if found && have != e {
		switch {
		case have.InstallID != e.InstallID:
			return fmt.Errorf("%w: it remembers install %s, not %s", ErrEpochBack, have.InstallID, e.InstallID)
		case e.Generation < have.Generation:
			return fmt.Errorf("%w: generation %d is below %d", ErrEpochBack, e.Generation, have.Generation)
		case e.Generation == have.Generation:
			return fmt.Errorf("%w: generation %d is the one it remembers, of another writer", ErrEpochBack, e.Generation)
		}
	}
	_, err = d.putIf(kindAgent, idAgentEpoch, e, rev)
	return err
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
// stored already: those NewestAgentActions keeps.
func (s *Store) SaveAgentActions(done map[string]ActionDone) error {
	for _, id := range slices.Sorted(maps.Keys(done)) {
		switch a := done[id]; {
		case id == "":
			return errors.New("agent action has no id")
		case a.Kind == "":
			return fmt.Errorf("agent action %s has no kind", id)
		}
	}
	return s.agent().put(kindAgent, idAgentActions, NewestAgentActions(done), true)
}

// NewestAgentActions is what SaveAgentActions keeps of done: the newest
// MaxAgentActions by the time they ran, the lower id first among those that
// ran at the same time.
func NewestAgentActions(done map[string]ActionDone) map[string]ActionDone {
	ids := slices.Sorted(maps.Keys(done))
	slices.SortStableFunc(ids, func(a, b string) int { return done[b].At.Compare(done[a].At) })
	kept := make(map[string]ActionDone, min(len(done), MaxAgentActions))
	for _, id := range ids[:min(len(ids), MaxAgentActions)] {
		kept[id] = done[id]
	}
	return kept
}
