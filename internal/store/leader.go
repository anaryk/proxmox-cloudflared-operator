package store

import (
	"errors"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// Handoff is a hand-off of the lease the holder announced: the node it is
// handed to, which alone may take it until Until.
type Handoff struct {
	To    string    `json:"to"`
	Until time.Time `json:"until"`
}

// LeaderRecord is meta/leader.json: the writer of the epoch, as the previous
// release reads it, and who holds the lease. It is a frozen object: its
// envelope keeps schema version 1 in every release, fields are only added and
// readers ignore those they do not know, so that every build of a cluster can
// tell who the holder is while the controller moves between builds. The
// writer is embedded, so its keys stay at the top of the object.
type LeaderRecord struct {
	planner.Writer
	Node       string    `json:"node,omitempty"`
	AcquiredAt time.Time `json:"acquiredAt,omitzero"`
	ReleasedAt time.Time `json:"releasedAt,omitzero"`
	Handoff    *Handoff  `json:"handoff,omitempty"`
	// CASHA256 is the fingerprint of the install's current CA: a host agent
	// that sees it change issues its certificate again from the new key.
	CASHA256 string `json:"caSHA256,omitempty"`
}

// Leader returns leader.json with its revision. The file is read on every
// call: a missing one is not found, one that cannot be read is an error.
func (s *Store) Leader() (r LeaderRecord, rev int64, found bool, err error) {
	rev, found, err = s.cluster.getRev(kindMeta, idLeader, &r, false)
	if err != nil || !found {
		return LeaderRecord{}, 0, false, err
	}
	return r, rev, true, nil
}

// SaveLeader stores leader.json, unless it is what is stored already. Writer
// returns its writer.
func (s *Store) SaveLeader(r LeaderRecord) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Handoff != nil && r.Handoff.To == "" {
		return errors.New("leader: the hand-off names no node")
	}
	return s.cluster.put(kindMeta, idLeader, r, true)
}
