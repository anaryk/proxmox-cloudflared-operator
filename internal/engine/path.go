package engine

import (
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// The ways a path reaches its guest, as PathView.Via says them: directly, or
// through the leg of an attached network or of the managed one.
const (
	ViaDirect  = "direct"
	ViaAttach  = "attach:"
	ViaManaged = "managed"
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
	// Addr is the address the binding verified. Via is how the node reaches
	// it: "direct", "attach:<network>" through the leg of an attached
	// network, or "managed" through the leg of the managed one. Service is
	// the address the connector dials when it is not Addr, the service
	// address of a path through a leg, and Leg says how that leg is: "up",
	// or why not; empty when the gateway has not said.
	Addr    netip.Addr `json:"addr,omitzero"`
	Via     string     `json:"via,omitempty"`
	Service netip.Addr `json:"service,omitzero"`
	Leg     string     `json:"leg,omitempty"`
}

// pathOf is the path of the binding b of a route of owner on node, with the
// tag of the NIC of b's guest in snap that has b's MAC. A binding made for
// another owner is none of the route's. legState says how the leg of a
// network is.
func pathOf(node, owner string, b *resolve.Binding, snap inventory.Snapshot, legState func(network string) string) *PathView {
	if b == nil || b.Owner != owner {
		return nil
	}
	p := &PathView{
		Node: node, Bridge: b.Bridge, Port: b.Port, MAC: b.MAC, VerifiedAt: b.VerifiedAt, Since: b.Since,
		Addr: b.Addr, Via: viaOf(b.Via),
	}
	if b.Via != "" {
		p.Service, p.Leg = b.Service, legState(b.Via)
	}
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

// viaOf is how a binding proven through the leg of network reaches its
// guest, as PathView.Via says it.
func viaOf(network string) string {
	switch network {
	case "":
		return ViaDirect
	case resolve.ManagedBridge:
		return ViaManaged
	}
	return ViaAttach + network
}
