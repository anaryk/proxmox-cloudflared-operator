package pve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// Snapshot is a snapshot of a guest.
type Snapshot struct {
	Name string
	Time time.Time // zero when Proxmox gives none
}

// ReplicationJob is a storage replication job.
type ReplicationJob struct {
	ID     string
	Guest  model.GuestRef // Kind is empty for a guest the cluster resources do not list
	Target string         // the node replicated to
}

type pendingWire struct {
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value"`
	Pending json.RawMessage `json:"pending"`
	Delete  json.RawMessage `json:"delete"`
}

type snapshotWire struct {
	Name     string `json:"name"`
	SnapTime *int64 `json:"snaptime"`
}

type replicationWire struct {
	ID     string          `json:"id"`
	Guest  json.RawMessage `json:"guest"`
	Target string          `json:"target"`
}

// PendingConfig returns the values that apply at the guest's next start: a
// key's pending value where it has one, and without the keys whose deletion
// is pending. It leaves out the digest.
func (c *Client) PendingConfig(ctx context.Context, node string, ref model.GuestRef) (map[string]string, error) {
	endpoint, err := guestEndpoint(node, ref.Kind, ref.VMID, "pending")
	if err != nil {
		return nil, err
	}
	var rows []pendingWire
	if err := c.get(ctx, endpoint, nil, &rows); err != nil {
		return nil, fmt.Errorf("fetching the pending config of %s: %w", ref, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("fetching the pending config of %s: unexpected response: empty configuration", ref)
	}
	out := make(map[string]string, len(rows))
	for i, row := range rows {
		if row.Key == "" {
			return nil, fmt.Errorf("fetching the pending config of %s: entry %d has no key", ref, i)
		}
		if row.Key == "digest" || deleted(row.Delete) {
			continue
		}
		text, ok := stringify(row.Pending)
		if !ok {
			text, ok = stringify(row.Value)
		}
		if ok {
			out[row.Key] = text
		}
	}
	return out, nil
}

// deleted reads the delete flag of a pending entry: 1, or 2 for a forced
// delete.
func deleted(raw json.RawMessage) bool {
	switch string(bytes.Trim(bytes.TrimSpace(raw), `"`)) {
	case "", "0", "null":
		return false
	}
	return true
}

// Snapshots lists the snapshots of a guest, without the entry for its current
// state.
func (c *Client) Snapshots(ctx context.Context, node string, ref model.GuestRef) ([]Snapshot, error) {
	endpoint, err := guestEndpoint(node, ref.Kind, ref.VMID, "snapshot")
	if err != nil {
		return nil, err
	}
	var rows []snapshotWire
	if err := c.get(ctx, endpoint, nil, &rows); err != nil {
		return nil, fmt.Errorf("fetching the snapshots of %s: %w", ref, err)
	}
	var out []Snapshot
	for i, row := range rows {
		switch row.Name {
		case "":
			return nil, fmt.Errorf("fetching the snapshots of %s: snapshot %d has no name", ref, i)
		case "current":
			continue
		}
		snap := Snapshot{Name: row.Name}
		if row.SnapTime != nil {
			snap.Time = time.Unix(*row.SnapTime, 0).UTC()
		}
		out = append(out, snap)
	}
	return out, nil
}

// Replication lists the storage replication jobs of the cluster. The listing
// names a guest by its vmid alone, so the kind is looked up in the cluster
// resources when there is a job.
func (c *Client) Replication(ctx context.Context) ([]ReplicationJob, error) {
	var rows []replicationWire
	if err := c.get(ctx, "cluster/replication", nil, &rows); err != nil {
		return nil, fmt.Errorf("fetching replication jobs: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]ReplicationJob, 0, len(rows))
	for i, row := range rows {
		vmid, err := vmidOf(row.Guest)
		if err != nil || row.ID == "" {
			return nil, fmt.Errorf("fetching replication jobs: job %d has no valid id or guest", i)
		}
		out = append(out, ReplicationJob{ID: row.ID, Guest: model.GuestRef{VMID: vmid}, Target: row.Target})
	}
	guests, err := c.guestRows(ctx)
	if err != nil {
		return nil, err
	}
	kinds := make(map[int]model.GuestKind, len(guests))
	for _, g := range guests {
		kinds[g.VMID] = model.GuestKind(g.Type)
	}
	for i := range out {
		out[i].Guest.Kind = kinds[out[i].Guest.VMID]
	}
	return out, nil
}

// vmidOf reads a vmid given as a number or as a string.
func vmidOf(raw json.RawMessage) (int, error) {
	var vmid int
	if err := json.Unmarshal(bytes.Trim(bytes.TrimSpace(raw), `"`), &vmid); err != nil {
		return 0, err
	}
	if vmid < 1 {
		return 0, errors.New("vmid below 1")
	}
	return vmid, nil
}

// Uptimes returns the uptime of every running guest as the cluster resources
// report it. A guest whose uptime is not reported, as while pvestatd does not
// run, is left out.
func (c *Client) Uptimes(ctx context.Context) (map[model.GuestRef]time.Duration, error) {
	rows, err := c.guestRows(ctx)
	if err != nil {
		return nil, err
	}
	out := map[model.GuestRef]time.Duration{}
	for _, row := range rows {
		if row.Status != "running" || row.Uptime == nil {
			continue
		}
		ref := model.GuestRef{Kind: model.GuestKind(row.Type), VMID: row.VMID}
		out[ref] = time.Duration(*row.Uptime) * time.Second
	}
	return out, nil
}
