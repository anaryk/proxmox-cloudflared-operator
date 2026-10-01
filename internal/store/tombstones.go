package store

import (
	"context"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// Tombstones returns the store of the DNS tombstones, which are one object on
// the cluster root.
func (s *Store) Tombstones() reconcile.TombstoneStore { return tombstoneStore{d: s.cluster} }

type tombstoneStore struct{ d Dir }

var _ reconcile.TombstoneStore = tombstoneStore{}

// Load returns the tombstones, an empty map when none were saved. The map is
// the caller's.
func (t tombstoneStore) Load(ctx context.Context) (map[string]reconcile.Tombstone, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m := make(map[string]reconcile.Tombstone)
	found, err := t.d.Get(kindMeta, idTombstones, &m)
	if err != nil {
		return nil, err
	}
	if !found || m == nil {
		return make(map[string]reconcile.Tombstone), nil
	}
	return m, nil
}

// Save stores the tombstones, unless they are what is stored already.
func (t tombstoneStore) Save(ctx context.Context, m map[string]reconcile.Tombstone) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil {
		m = make(map[string]reconcile.Tombstone)
	}
	return t.d.put(kindMeta, idTombstones, m, true)
}
