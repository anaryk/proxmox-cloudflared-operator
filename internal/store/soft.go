package store

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"
)

const idSoftDeny = "soft-deny"

// SoftEntry is an address of the soft deny list: a gateway or a resolver of a
// node or of the appliance, with the time it was last seen as one.
type SoftEntry struct {
	Addr     netip.Addr `json:"addr"`
	Why      string     `json:"why"` // "gateway of node pve1", "resolver of the appliance"
	LastSeen time.Time  `json:"lastSeen"`
}

// SoftDeny is what the node-local file soft-deny.json keeps: the gateways and
// resolvers of the nodes and of this appliance, each with the time it was last
// seen, so that a node that is offline after a restart still contributes and
// an address that is gone for 30 days ages out.
type SoftDeny struct {
	Entries []SoftEntry `json:"entries"`
}

// SoftDeny returns the soft deny list, sorted by address, and an empty one when
// none was saved.
func (s *Store) SoftDeny() (SoftDeny, error) {
	var v SoftDeny
	if _, err := s.local.Get(kindMeta, idSoftDeny, &v); err != nil {
		return SoftDeny{}, err
	}
	return SoftDeny{Entries: compactSoft(v.Entries)}, nil
}

// SaveSoftDeny stores the soft deny list sorted by address, with one entry per
// address: the one seen last, and of those seen at once the first reason in
// order. An entry without a valid address or a reason is refused and nothing
// is written; an unchanged list is not written either.
func (s *Store) SaveSoftDeny(v SoftDeny) error {
	for _, e := range v.Entries {
		switch {
		case !e.Addr.IsValid():
			return fmt.Errorf("soft deny entry %q: the address is not valid", e.Why)
		case e.Why == "":
			return fmt.Errorf("soft deny entry %s has no reason", e.Addr)
		}
	}
	return s.local.put(kindMeta, idSoftDeny, SoftDeny{Entries: compactSoft(v.Entries)}, true)
}

func compactSoft(entries []SoftEntry) []SoftEntry {
	out := slices.Clone(entries)
	slices.SortFunc(out, func(a, b SoftEntry) int {
		return cmp.Or(a.Addr.Compare(b.Addr), b.LastSeen.Compare(a.LastSeen), strings.Compare(a.Why, b.Why))
	})
	out = slices.CompactFunc(out, func(a, b SoftEntry) bool { return a.Addr == b.Addr })
	if out == nil {
		out = []SoftEntry{}
	}
	return out
}
