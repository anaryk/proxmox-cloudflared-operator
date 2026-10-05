// Package model defines the plain data types shared by the operator packages.
package model

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// GuestKind is the Proxmox guest type.
type GuestKind string

const (
	KindQEMU GuestKind = "qemu"
	KindLXC  GuestKind = "lxc"
)

// GuestRef identifies a guest within a cluster.
type GuestRef struct {
	Kind GuestKind `json:"kind"`
	VMID int       `json:"vmid"`
}

// String returns the "qemu/101" form that ParseGuestRef reads back.
func (r GuestRef) String() string {
	return string(r.Kind) + "/" + strconv.Itoa(r.VMID)
}

// ParseGuestRef is the inverse of GuestRef.String. Only the canonical form is
// accepted, so every guest has exactly one owner string.
func ParseGuestRef(s string) (GuestRef, error) {
	kind, id, ok := strings.Cut(s, "/")
	if !ok {
		return GuestRef{}, fmt.Errorf("guest ref %q: want <kind>/<vmid>", s)
	}
	k := GuestKind(kind)
	if k != KindQEMU && k != KindLXC {
		return GuestRef{}, fmt.Errorf("guest ref %q: unknown kind %q", s, kind)
	}
	vmid, err := strconv.ParseUint(id, 10, 31)
	ref := GuestRef{Kind: k, VMID: int(vmid)}
	if err != nil || vmid < 1 || ref.String() != s {
		return GuestRef{}, fmt.Errorf("guest ref %q: invalid vmid %q", s, id)
	}
	return ref, nil
}

// NIC is a network interface as configured on the guest.
type NIC struct {
	Index    int          `json:"index"`
	MAC      string       `json:"mac"` // lower case, colon separated
	Bridge   string       `json:"bridge"`
	VLAN     int          `json:"vlan,omitempty"`
	Firewall bool         `json:"firewall,omitempty"`
	Static   []netip.Addr `json:"static,omitempty"`
}

// ReportedAddr is an address the guest agent reports for an interface.
type ReportedAddr struct {
	Iface string     `json:"iface"`
	MAC   string     `json:"mac"`
	Addr  netip.Addr `json:"addr"`
}

// Guest is a VM or container together with what the operator needs to know
// about its network identity.
type Guest struct {
	Ref         GuestRef       `json:"ref"`
	Name        string         `json:"name"`
	Node        string         `json:"node"`
	Running     bool           `json:"running"`
	Template    bool           `json:"template,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	Pool        string         `json:"pool,omitempty"`
	Description string         `json:"description,omitempty"`
	Digest      string         `json:"digest,omitempty"`
	Identity    string         `json:"identity"`
	NICs        []NIC          `json:"nics,omitempty"`
	Reported    []ReportedAddr `json:"reported,omitempty"`
	// StatusUnknown is set when Proxmox reports neither running nor stopped
	// and no state was ever read for this guest. Running is then false, but
	// only because nothing is known.
	StatusUnknown bool `json:"statusUnknown,omitempty"`
}

// HasTag reports whether the guest carries tag. Tags are compared exactly, as
// Proxmox compares registered tags.
func (g Guest) HasTag(tag string) bool {
	return slices.Contains(g.Tags, tag)
}

// NIC returns the interface with the given index.
func (g Guest) NIC(index int) (NIC, bool) {
	for _, n := range g.NICs {
		if n.Index == index {
			return n, true
		}
	}
	return NIC{}, false
}

// NormalizeMAC returns s as a lower case, colon separated MAC address. Upper
// case and '-' separators are accepted.
func NormalizeMAC(s string) (string, error) {
	hw, err := net.ParseMAC(s)
	if err != nil {
		return "", fmt.Errorf("normalizing MAC: %w", err)
	}
	if len(hw) != 6 {
		return "", fmt.Errorf("normalizing MAC %q: want 6 bytes, got %d", s, len(hw))
	}
	return hw.String(), nil
}
