package pve

import (
	"cmp"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// HAResource is a guest the HA manager keeps running, as /cluster/ha/resources
// lists it.
type HAResource struct {
	SID string // "ct:105", "vm:101"
	Ref model.GuestRef
	// State is the one asked of the manager: started, stopped, disabled or
	// ignored.
	State string
	// Group is the HA group, empty without one; newer releases place
	// resources by rules and list no group.
	Group string
}

// HAService is an HA resource as the cluster manager has it: the node it is
// on and the state it is in.
type HAService struct {
	Node  string
	State string
}

// HAStatus is the state of the HA stack as /cluster/ha/status/current
// reports it.
type HAStatus struct {
	// Maintenance says, for every node whose resource manager reported,
	// whether that node is in maintenance mode. A node that stands alone, or
	// a cluster that never had an HA resource, has no manager reporting.
	Maintenance map[string]bool
	// Services are the HA resources by service id.
	Services map[string]HAService
}

type haResourceWire struct {
	SID   string `json:"sid"`
	State string `json:"state"`
	Group string `json:"group"`
}

type haStatusWire struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Node   string `json:"node"`
	Status string `json:"status"`
	SID    string `json:"sid"`
	State  string `json:"state"`
}

// HAResources lists the HA resources, which takes Sys.Audit on /.
func (c *Client) HAResources(ctx context.Context) ([]HAResource, error) {
	var rows []haResourceWire
	if err := c.get(ctx, "cluster/ha/resources", nil, &rows); err != nil {
		return nil, fmt.Errorf("fetching ha resources: %w", err)
	}
	out := make([]HAResource, 0, len(rows))
	for i, row := range rows {
		ref, err := parseSID(row.SID)
		if err != nil {
			return nil, fmt.Errorf("fetching ha resources: row %d: %w", i, err)
		}
		out = append(out, HAResource{SID: row.SID, Ref: ref, State: row.State, Group: row.Group})
	}
	return out, nil
}

// parseSID reads a service id of the HA manager: ct:<vmid> for a container,
// vm:<vmid> for a virtual machine.
func parseSID(sid string) (model.GuestRef, error) {
	kind, id, ok := strings.Cut(sid, ":")
	vmid, err := strconv.Atoi(id)
	if !ok || err != nil || vmid < 1 {
		return model.GuestRef{}, fmt.Errorf("%q is no service id of a guest", sid)
	}
	switch kind {
	case "ct":
		return model.GuestRef{Kind: model.KindLXC, VMID: vmid}, nil
	case "vm":
		return model.GuestRef{Kind: model.KindQEMU, VMID: vmid}, nil
	}
	return model.GuestRef{}, fmt.Errorf("%q is no service id of a guest", sid)
}

// HAStatus reads the state of the HA stack, which takes Sys.Audit on /.
func (c *Client) HAStatus(ctx context.Context) (HAStatus, error) {
	var rows []haStatusWire
	if err := c.get(ctx, "cluster/ha/status/current", nil, &rows); err != nil {
		return HAStatus{}, fmt.Errorf("fetching ha status: %w", err)
	}
	st := HAStatus{Maintenance: map[string]bool{}, Services: map[string]HAService{}}
	for i, row := range rows {
		switch row.Type {
		case "lrm":
			node := cmp.Or(row.Node, strings.TrimPrefix(row.ID, "lrm:"))
			if node == "" {
				return HAStatus{}, fmt.Errorf("fetching ha status: row %d names no node for its resource manager", i)
			}
			st.Maintenance[node] = inMaintenance(row.Status)
		case "service":
			sid := cmp.Or(row.SID, strings.TrimPrefix(row.ID, "service:"))
			if sid == "" {
				return HAStatus{}, fmt.Errorf("fetching ha status: row %d names no service", i)
			}
			st.Services[sid] = HAService{Node: row.Node, State: row.State}
		}
	}
	return st, nil
}

// inMaintenance reads the mode of a resource manager from the text of its
// report, "<node> (<state or mode>, <watchdog>, <time>)": the mode has no
// field of its own, and the first part in the parentheses is the state of
// the manager, or its mode followed by the word mode when it is in one.
func inMaintenance(status string) bool {
	_, rest, ok := strings.Cut(status, "(")
	if !ok {
		return false
	}
	first, _, _ := strings.Cut(rest, ",")
	first = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(first), ")"))
	return strings.EqualFold(first, "maintenance mode")
}
