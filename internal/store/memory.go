package store

import (
	"cmp"
	"net/netip"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

const idEngineMemory = "engine-memory"

// EngineMemory is what the reconcile cycle learns and must still know after a
// restart: forgetting any of it would turn a hold into a removal.
type EngineMemory struct {
	// InstallID is the install the memory belongs to: a memory of another
	// install, as after a new setup on the node, is not this one's.
	InstallID string `json:"installId,omitempty"`
	// Served are the zones as they were last served, each by one credential
	// alone: who serves a zone that several credentials see, and what a
	// credential listed before, so that a zone that left its listing while
	// the daemon was down is still noticed.
	Served []RememberedZone `json:"served,omitempty"`
	// Stale are zones a credential listed once and lists no more.
	Stale []RememberedZone `json:"stale,omitempty"`
	// Tunnels are the tunnels of the install seen to exist.
	Tunnels []SeenTunnel `json:"tunnels,omitempty"`
	// GoneGuests are guests that hold a claim, no longer listed by Proxmox,
	// that the admin confirmed removed.
	GoneGuests []model.GuestRef `json:"goneGuests,omitempty"`
	// Reports are the last check of each credential: what its token could
	// do, which holds no secret.
	Reports []CheckedCredential `json:"reports,omitempty"`
	// Egress is the set of targets the egress filter was last given, and
	// Verified, by account, the targets of the tunnel configuration last
	// verified at Cloudflare: a target leaves the set only once no verified
	// configuration sends a connector to it, also across a restart.
	Egress   []netip.AddrPort  `json:"egress,omitempty"`
	Verified []VerifiedTargets `json:"verified,omitempty"`
}

// VerifiedTargets are the targets of the tunnel configuration of an account
// as it was last verified at Cloudflare.
type VerifiedTargets struct {
	AccountID string           `json:"accountId"`
	Targets   []netip.AddrPort `json:"targets"`
}

// CheckedCredential is the last check of a credential.
type CheckedCredential struct {
	CredentialID string             `json:"credentialId"`
	Report       credentials.Report `json:"report"`
}

// RememberedZone is a zone and the credential that saw it.
type RememberedZone struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	AccountID    string `json:"accountId"`
	CredentialID string `json:"credentialId"`
}

// SeenTunnel is a tunnel of the install that was seen to exist.
type SeenTunnel struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	AccountID    string `json:"accountId"`
	CredentialID string `json:"credentialId"`
}

// EngineMemory returns what the engine remembered, and an empty memory when
// none was saved. It is kept on the node-local root.
func (s *Store) EngineMemory() (EngineMemory, error) {
	m, _, err := getOne[EngineMemory](s.local, kindMeta, idEngineMemory)
	if err != nil {
		return EngineMemory{}, err
	}
	return m.sorted(), nil
}

// SaveEngineMemory stores what the engine remembers, unless it is what is
// stored already.
func (s *Store) SaveEngineMemory(m EngineMemory) error {
	return s.local.put(kindMeta, idEngineMemory, m.sorted(), true)
}

// sorted returns a copy of m with its lists in a fixed order, so that the
// same memory is always written alike.
func (m EngineMemory) sorted() EngineMemory {
	byCredential := func(a, b RememberedZone) int {
		return cmp.Or(cmp.Compare(a.CredentialID, b.CredentialID), cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	}
	m.Served = slices.Clone(m.Served)
	slices.SortFunc(m.Served, byCredential)
	m.Stale = slices.Clone(m.Stale)
	slices.SortFunc(m.Stale, byCredential)
	m.Tunnels = slices.Clone(m.Tunnels)
	slices.SortFunc(m.Tunnels, func(a, b SeenTunnel) int { return cmp.Compare(a.ID, b.ID) })
	m.GoneGuests = slices.Clone(m.GoneGuests)
	slices.SortFunc(m.GoneGuests, func(a, b model.GuestRef) int { return model.CompareOwners(a.String(), b.String()) })
	m.Reports = slices.Clone(m.Reports)
	slices.SortFunc(m.Reports, func(a, b CheckedCredential) int { return cmp.Compare(a.CredentialID, b.CredentialID) })
	m.Egress = sortedTargets(m.Egress)
	m.Verified = slices.Clone(m.Verified)
	for i := range m.Verified {
		m.Verified[i].Targets = sortedTargets(m.Verified[i].Targets)
	}
	slices.SortFunc(m.Verified, func(a, b VerifiedTargets) int { return cmp.Compare(a.AccountID, b.AccountID) })
	if len(m.Served) == 0 {
		m.Served = nil
	}
	if len(m.Stale) == 0 {
		m.Stale = nil
	}
	if len(m.Tunnels) == 0 {
		m.Tunnels = nil
	}
	if len(m.GoneGuests) == 0 {
		m.GoneGuests = nil
	}
	if len(m.Reports) == 0 {
		m.Reports = nil
	}
	if len(m.Egress) == 0 {
		m.Egress = nil
	}
	if len(m.Verified) == 0 {
		m.Verified = nil
	}
	return m
}

// sortedTargets returns the targets sorted and once each, never nil.
func sortedTargets(ts []netip.AddrPort) []netip.AddrPort {
	out := slices.Clone(ts)
	slices.SortFunc(out, netip.AddrPort.Compare)
	return append([]netip.AddrPort{}, slices.Compact(out)...)
}
