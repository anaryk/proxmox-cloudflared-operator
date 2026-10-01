// Package resolve decides which address of a guest a published route may
// point at.
package resolve

import (
	"cmp"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// CandidateSource says how a candidate address was found.
type CandidateSource string

const (
	FromVia    CandidateSource = "via"      // named by the route, as via=<ip> or as its target
	FromStatic CandidateSource = "static"   // configured on the guest NIC
	FromAgent  CandidateSource = "reported" // reported by the guest agent
)

// Candidate is an address to try for a route.
type Candidate struct {
	Addr   netip.Addr
	NIC    model.NIC // the guest NIC this address is expected on
	Source CandidateSource
}

// Candidates lists addresses to try for a route, in order of preference.
// Nothing here looks at the wire or applies the denylist: the addresses are
// only what the guest's configuration and agent claim.
func Candidates(route model.Route, guest model.Guest) ([]Candidate, error) {
	if route.Guest == nil {
		return nil, errors.New("route has no guest")
	}
	if *route.Guest != guest.Ref {
		return nil, errors.New("guest does not match the route")
	}
	explicit, via := route.Target.Addr, route.Options.Via
	switch {
	case explicit.IsValid() && via != "":
		return nil, errors.New("route has both an address and via")
	case explicit.IsValid():
		return named(explicit, guest)
	case via == "":
		return discover(guest), nil
	}

	if index, ok := parseNICName(via); ok {
		return onNIC(index, guest)
	}
	addr, err := netip.ParseAddr(via)
	if err != nil {
		return nil, fmt.Errorf("invalid via %q", via)
	}
	return named(addr, guest)
}

// parseNICName reads "net1" as 1.
func parseNICName(s string) (int, bool) {
	digits, ok := strings.CutPrefix(s, "net")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(digits, 10, 31)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

// candidateList collects candidates, dropping unusable and repeated addresses.
type candidateList struct {
	out  []Candidate
	seen map[netip.Addr]struct{}
}

func (l *candidateList) add(addr netip.Addr, nic model.NIC, source CandidateSource) {
	if !usable(addr) {
		return
	}
	if _, dup := l.seen[addr]; dup {
		return
	}
	if l.seen == nil {
		l.seen = make(map[netip.Addr]struct{})
	}
	l.seen[addr] = struct{}{}
	l.out = append(l.out, Candidate{Addr: addr, NIC: copyNIC(nic), Source: source})
}

// addNIC adds the static addresses of nic, then the reported ones.
func (l *candidateList) addNIC(nic model.NIC, reported []model.ReportedAddr) {
	for _, a := range nic.Static {
		l.add(a, nic, FromStatic)
	}
	l.addReported(nic, reported)
}

func (l *candidateList) addReported(nic model.NIC, reported []model.ReportedAddr) {
	for _, r := range reported {
		if reportedOn(r, nic) {
			l.add(r.Addr, nic, FromAgent)
		}
	}
}

// usable reports whether addr may be a candidate at all. Whether it may be
// published is for the denylist to say.
func usable(addr netip.Addr) bool {
	return addr.Is4() && !addr.IsUnspecified() && !addr.IsMulticast()
}

// reportedOn reports whether the agent saw r on nic. An entry without a MAC
// belongs to no NIC.
func reportedOn(r model.ReportedAddr, nic model.NIC) bool {
	return r.MAC != "" && nic.MAC != "" && strings.EqualFold(r.MAC, nic.MAC)
}

// sortedNICs returns the NICs in index order without touching the guest.
func sortedNICs(guest model.Guest) []model.NIC {
	nics := slices.Clone(guest.NICs)
	slices.SortStableFunc(nics, func(a, b model.NIC) int { return cmp.Compare(a.Index, b.Index) })
	return nics
}

// discover lists the static addresses of every NIC, then the reported ones.
// Reported addresses whose MAC matches no NIC belong to something running
// inside the guest and are left out.
func discover(guest model.Guest) []Candidate {
	nics := sortedNICs(guest)
	var l candidateList
	for _, nic := range nics {
		for _, a := range nic.Static {
			l.add(a, nic, FromStatic)
		}
	}
	for _, nic := range nics {
		l.addReported(nic, guest.Reported)
	}
	return l.out
}

// onNIC lists the addresses of one NIC, for via=netN.
func onNIC(index int, guest model.Guest) ([]Candidate, error) {
	nic, ok := guest.NIC(index)
	if !ok {
		return nil, fmt.Errorf("guest has no net%d", index)
	}
	var l candidateList
	l.addNIC(nic, guest.Reported)
	return l.out, nil
}

// named returns the single candidate for an address the route names itself.
func named(addr netip.Addr, guest model.Guest) ([]Candidate, error) {
	if !usable(addr) {
		return nil, fmt.Errorf("address %s is not a usable IPv4 address", addr)
	}
	nic, err := nicFor(addr, sortedNICs(guest), guest.Reported)
	if err != nil {
		return nil, err
	}
	return []Candidate{{Addr: addr, NIC: copyNIC(nic), Source: FromVia}}, nil
}

// copyNIC returns nic with a Static slice of its own, so that a candidate
// shares no memory with the guest.
func copyNIC(nic model.NIC) model.NIC {
	nic.Static = slices.Clone(nic.Static)
	return nic
}

// nicFor finds the NIC an address is expected on: the one that lists it as a
// static address, else the one whose MAC the agent reports it with, else the
// first NIC that has a bridge. The last case is a guess; the identity check
// on the wire decides whether it holds.
func nicFor(addr netip.Addr, nics []model.NIC, reported []model.ReportedAddr) (model.NIC, error) {
	if len(nics) == 0 {
		return model.NIC{}, errors.New("guest has no network interface")
	}
	for _, nic := range nics {
		if slices.Contains(nic.Static, addr) {
			return nic, nil
		}
	}
	for _, nic := range nics {
		seen := slices.ContainsFunc(reported, func(r model.ReportedAddr) bool {
			return r.Addr == addr && reportedOn(r, nic)
		})
		if seen {
			return nic, nil
		}
	}
	for _, nic := range nics {
		if nic.Bridge != "" {
			return nic, nil
		}
	}
	return model.NIC{}, errors.New("guest has no network interface with a bridge")
}
