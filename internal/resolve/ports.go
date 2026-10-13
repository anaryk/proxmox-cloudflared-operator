package resolve

import (
	"context"
	"strings"
)

// PortKind is what a port of a bridge connects it to.
type PortKind string

const (
	// PortGuest is the port Proxmox makes for a guest's card:
	// tap<vmid>i<n>, veth<vmid>i<n> or fwpr<vmid>p<n>.
	PortGuest PortKind = "guest"
	// PortLeg is the bridge end of a leg of pco's gateway: vethpco<N>, or a
	// port with a MAC of the form fe:50:43:4f.
	PortLeg PortKind = "leg"
	// PortUplink leads off the node: a physical card, a bond, a VLAN
	// sub-interface or a VXLAN port.
	PortUplink PortKind = "uplink"
	// PortUnknown is any other port.
	PortUnknown PortKind = "unknown"
)

// legPortPrefix and legPortMAC are how the gateway names the bridge ends of
// its legs and the MACs it gives them.
const (
	legPortPrefix = "vethpco"
	legPortMAC    = "fe:50:43:4f:"
)

// PortClassifier is implemented by the host prober; a prober without it
// gives every cross-node proof the observed ceiling.
type PortClassifier interface {
	// PortKinds returns the kind of every port of bridge, by name.
	PortKinds(ctx context.Context, bridge string) (map[string]PortKind, error)
}

// ClassifyPort tells the kind of a bridge port from its name, the type of
// its link as netlink names it, and its MAC. The names Proxmox and pco give
// their ports win over the type, so that a guest's port is never taken for an
// uplink.
func ClassifyPort(name, linkType, mac string) PortKind {
	switch {
	case isGuestPort(name):
		return PortGuest
	case strings.HasPrefix(name, legPortPrefix), strings.HasPrefix(strings.ToLower(mac), legPortMAC):
		return PortLeg
	}
	switch linkType {
	case "device", "bond", "vlan", "vxlan":
		return PortUplink
	}
	return PortUnknown
}

// kindByName is what the name of a port alone tells of its kind.
func kindByName(name string) PortKind {
	return ClassifyPort(name, "", "")
}
