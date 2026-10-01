package model

import (
	"cmp"
	"net/netip"
	"strings"
)

// Scheme is the protocol used to reach the target.
type Scheme string

const (
	SchemeHTTP  Scheme = "http"
	SchemeHTTPS Scheme = "https"
)

// Target is where a route sends traffic.
type Target struct {
	Scheme Scheme     `json:"scheme"`
	Addr   netip.Addr `json:"addr,omitzero"` // zero value: resolve from the guest
	Port   uint16     `json:"port"`
}

// RouteOptions tunes how the tunnel connects to the target.
type RouteOptions struct {
	NoTLSVerify bool   `json:"noTLSVerify,omitempty"`
	HostHeader  string `json:"hostHeader,omitempty"`
	SNI         string `json:"sni,omitempty"`
	Via         string `json:"via,omitempty"` // "net1" or an IPv4 address
}

// SourceKind says where a route definition came from.
type SourceKind string

const (
	SourceAnnotation SourceKind = "annotation"
	SourceManual     SourceKind = "manual"
)

// Route maps a public hostname to a target.
type Route struct {
	Hostname string       `json:"hostname"`
	Target   Target       `json:"target"`
	Options  RouteOptions `json:"options"`
	Source   SourceKind   `json:"source"`
	Guest    *GuestRef    `json:"guest,omitempty"`
	ManualID string       `json:"manualId,omitempty"`
}

const manualPrefix = "manual/"

// Owner identifies who claims the hostname. An annotation route belongs to its
// guest ("qemu/101", "lxc/200"). A manual route, that is one with Source
// manual or a ManualID, belongs to "manual/<id>" even when it names a guest:
// the admin who wrote it owns it, not whoever controls the guest's Notes.
func (r Route) Owner() string {
	if r.Source != SourceManual && r.ManualID == "" && r.Guest != nil {
		return r.Guest.String()
	}
	return manualPrefix + r.ManualID
}

// CompareOwners orders owners deterministically: guests before manual routes,
// qemu before lxc, lower vmid first, manual ids lexically. Anything that is
// not a valid owner sorts last, so the order stays total.
func CompareOwners(a, b string) int {
	rankA, vmidA := ownerKey(a)
	rankB, vmidB := ownerKey(b)
	// Owners of the same class share a prefix, so the final string comparison
	// is the lexical comparison of the manual ids.
	return cmp.Or(
		cmp.Compare(rankA, rankB),
		cmp.Compare(vmidA, vmidB),
		strings.Compare(a, b),
	)
}

func ownerKey(owner string) (rank, vmid int) {
	if ref, err := ParseGuestRef(owner); err == nil {
		if ref.Kind == KindQEMU {
			return 0, ref.VMID
		}
		return 1, ref.VMID
	}
	if strings.HasPrefix(owner, manualPrefix) {
		return 2, 0
	}
	return 3, 0
}
