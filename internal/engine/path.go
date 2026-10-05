package engine

import (
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// PathView is where the winner of a hostname was proven: the binding the
// cycle stored for it.
type PathView struct {
	Node       string    `json:"node"`
	Bridge     string    `json:"bridge,omitempty"` // empty: the proof placed the MAC on no bridge
	VLAN       int       `json:"vlan,omitempty"`   // the tag of the guest's NIC with that MAC, as configured
	Port       string    `json:"port,omitempty"`
	MAC        string    `json:"mac,omitempty"`
	VerifiedAt time.Time `json:"verifiedAt"`
	Since      time.Time `json:"since,omitzero"`
}

// pathOf is the path of the binding b of a route of owner on node, with the
// tag of the NIC of b's guest in snap that has b's MAC. A binding made for
// another owner is none of the route's.
func pathOf(node, owner string, b *resolve.Binding, snap inventory.Snapshot) *PathView {
	if b == nil || b.Owner != owner {
		return nil
	}
	p := &PathView{Node: node, Bridge: b.Bridge, Port: b.Port, MAC: b.MAC, VerifiedAt: b.VerifiedAt, Since: b.Since}
	ref, err := model.ParseGuestRef(b.Guest)
	if err != nil {
		return p
	}
	if g, ok := snap.Guest(ref); ok {
		if i := slices.IndexFunc(g.NICs, func(n model.NIC) bool { return strings.EqualFold(n.MAC, b.MAC) }); i >= 0 {
			p.VLAN = g.NICs[i].VLAN
		}
	}
	return p
}
