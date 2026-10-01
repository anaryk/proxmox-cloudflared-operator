package pve

import (
	"net/netip"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// Version is the Proxmox VE release the API reports.
type Version struct {
	Release string // "9.1"
	Major   int
	Minor   int
}

// Resource is one guest as listed by the cluster resources endpoint.
type Resource struct {
	Kind     model.GuestKind
	VMID     int
	Name     string
	Node     string
	Status   string // "running", "stopped", ...
	Template bool
	Tags     []string // lower case
}

// GuestConfig is a guest's configuration with every value as text.
type GuestConfig struct {
	Values map[string]string // every key stringified, without "digest"
	Digest string
}

// GuestIface is a network interface as seen from inside a guest.
type GuestIface struct {
	Name  string
	MAC   string       // normalised; empty when the guest reports none
	Addrs []netip.Addr // IPv4 only
}

// ClusterNode is a cluster member.
type ClusterNode struct {
	Name   string
	Addr   netip.Addr // zero when unknown or not IPv4
	Online bool
	Local  bool // the node the API call was answered by
}

// NodeIface is a network interface of a node.
type NodeIface struct {
	Name   string
	Type   string // "bridge", "eth", "bond", "vlan", ...
	Active bool
	Addrs  []netip.Prefix // IPv4 only
	Ports  []string       // bridge_ports
}
