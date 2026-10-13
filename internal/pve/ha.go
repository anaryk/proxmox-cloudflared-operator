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

// Maintenance is whether a node's resource manager is in maintenance mode,
// or that it cannot be told, as of a manager whose report is stale, could
// not be read, or names no state. A caller that drains on maintenance
// treats unknown as what it knew before, not as off.
type Maintenance int

const (
	MaintenanceUnknown Maintenance = iota
	MaintenanceOff
	MaintenanceOn
)

func (m Maintenance) String() string {
	switch m {
	case MaintenanceOff:
		return "off"
	case MaintenanceOn:
		return "on"
	}
	return "unknown"
}

// HAStatus is the state of the HA stack as /cluster/ha/status/current
// reports it.
type HAStatus struct {
	// Maintenance says, for every node whose resource manager has an entry,
	// whether that node is in maintenance mode. A node that stands alone, or
	// a cluster that never had an HA resource, has no manager reporting.
	Maintenance map[string]Maintenance
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
// vm:<vmid> for a virtual machine, the vmid in digits alone.
func parseSID(sid string) (model.GuestRef, error) {
	kind, id, ok := strings.Cut(sid, ":")
	if !ok || !digits(id) {
		return model.GuestRef{}, fmt.Errorf("%q is no service id of a guest", sid)
	}
	vmid, err := strconv.Atoi(id)
	if err != nil || vmid < 1 {
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

// digits says that s is one or more digits without a leading zero.
func digits(s string) bool {
	if s == "" || s[0] == '0' {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// HAStatus reads the state of the HA stack, which takes Sys.Audit on /.
func (c *Client) HAStatus(ctx context.Context) (HAStatus, error) {
	var rows []haStatusWire
	if err := c.get(ctx, "cluster/ha/status/current", nil, &rows); err != nil {
		return HAStatus{}, fmt.Errorf("fetching ha status: %w", err)
	}
	st := HAStatus{Maintenance: map[string]Maintenance{}, Services: map[string]HAService{}}
	for i, row := range rows {
		switch row.Type {
		case "lrm":
			node := cmp.Or(row.Node, strings.TrimPrefix(row.ID, "lrm:"))
			if node == "" {
				return HAStatus{}, fmt.Errorf("fetching ha status: row %d names no node for its resource manager", i)
			}
			st.Maintenance[node] = maintenanceOf(row.Status)
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

// maintenanceOf reads the mode of a resource manager from the text of its
// report, "<node> (<state or mode>, <watchdog>, <time>)": the mode has no
// field of its own, and the first part in the parentheses is the state of
// the manager, or its mode followed by the word mode when it is in one. A
// report that is stale, could not be read, or has no state in it says
// nothing of the mode.
func maintenanceOf(status string) Maintenance {
	_, rest, ok := strings.Cut(status, "(")
	if !ok {
		return MaintenanceUnknown
	}
	first, _, _ := strings.Cut(rest, ",")
	first = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(first), ")")))
	switch first {
	case "maintenance mode":
		return MaintenanceOn
	case "", "unknown", "old timestamp - dead?", "unable to read lrm status":
		return MaintenanceUnknown
	}
	return MaintenanceOff
}
