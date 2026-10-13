package pve

import (
	"cmp"
	"context"
	"errors"
	"fmt"
)

// StorageInfo is what the controller needs to know of a storage: whether
// every node sees the same volumes on it, which decides what a guest the HA
// manager moved finds there.
type StorageInfo struct {
	ID     string
	Type   string // "dir", "nfs", "zfspool", "rbd", ...
	Shared bool
}

type storageWire struct {
	Storage string   `json:"storage"`
	Type    string   `json:"type"`
	Shared  flexBool `json:"shared"`
}

// Storage reads the storage with id, which takes Datastore.Audit on
// /storage/<id>. A storage of a kind that is shared by nature, such as NFS,
// reports shared whether or not its configuration says so.
func (c *Client) Storage(ctx context.Context, id string) (StorageInfo, error) {
	if !validSegment(id) {
		return StorageInfo{}, fmt.Errorf("invalid storage id %q", id)
	}
	var w storageWire
	if err := c.get(ctx, "storage/"+id, nil, &w); err != nil {
		return StorageInfo{}, fmt.Errorf("fetching storage %s: %w", id, err)
	}
	if w.Type == "" {
		return StorageInfo{}, fmt.Errorf("fetching storage %s: %w", id, errors.New("unexpected response: no type"))
	}
	return StorageInfo{ID: cmp.Or(w.Storage, id), Type: w.Type, Shared: bool(w.Shared)}, nil
}
