package engine

import "slices"

// The states of ZoneView.State.
const (
	ZoneServed    = "served"     // a credential serves it
	ZoneFrozen    = "frozen"     // in doubt: its account is frozen, FrozenWhy says why
	ZoneLeftOut   = "left out"   // every credential that lists it may not read its DNS
	ZoneNotServed = "not served" // pinned to a credential that leaves it out
)

// ZoneView is a zone as the last cycle used it, from the zone cache and the
// choice the cycle made, not from the reports of the daily check.
type ZoneView struct {
	Name        string   `json:"name"`
	ID          string   `json:"id"`
	Status      string   `json:"status"` // Cloudflare's, of the last listing that had it
	AccountID   string   `json:"accountId"`
	State       string   `json:"state"`
	Credentials []string `json:"credentials"` // that list it, those of Stale included, sorted
	ServedBy    string   `json:"servedBy,omitempty"`
	Pinned      string   `json:"pinned,omitempty"`
	Stale       []string `json:"stale"`    // credentials whose listing lost it
	Excluded    []string `json:"excluded"` // credentials refused its DNS
	// FrozenWhy says why the account of the zone is frozen, as the reason of
	// its routes does; it is set for a zone served in a frozen account too.
	FrozenWhy string `json:"frozenWhy,omitempty"`
}

// zoneViewOf is the view of zone name from the entries of the credentials
// that list it, the pin of the settings and the choice made of it. FrozenWhy
// is the account's, which set knows once every zone is chosen.
func zoneViewOf(name string, entries []zoneEntry, pin string, ch choice) ZoneView {
	shown := entries[0]
	if i := slices.IndexFunc(entries, func(en zoneEntry) bool { return !en.stale }); i >= 0 {
		shown = entries[i]
	}
	var listing, stale []string
	for _, en := range entries {
		listing = append(listing, en.zone.CredentialID)
		if en.stale {
			stale = append(stale, en.zone.CredentialID)
		}
	}
	v := ZoneView{Name: name, Pinned: pin, Credentials: sortedIDs(listing), Stale: sortedIDs(stale), Excluded: nonNil(excludedBy(entries))}
	switch {
	case len(ch.chosen) > 0:
		by := ch.chosen[0]
		if i := slices.IndexFunc(entries, func(en zoneEntry) bool { return en.zone == by && !en.stale }); i >= 0 {
			shown = entries[i]
		}
		v.State, v.ServedBy = ZoneServed, by.CredentialID
	case ch.doubt != "":
		v.State = ZoneFrozen
	case ch.note != "":
		v.State = ZoneNotServed
	default:
		v.State = ZoneLeftOut
	}
	v.ID, v.Status, v.AccountID = shown.zone.ID, shown.status, shown.zone.AccountID
	return v
}

func sortedIDs(ids []string) []string {
	return nonNil(slices.Compact(slices.Sorted(slices.Values(ids))))
}

// cloneZones copies zone views down to their lists.
func cloneZones(zones []ZoneView) []ZoneView {
	zones = slices.Clone(zones)
	for i := range zones {
		zones[i].Credentials = slices.Clone(zones[i].Credentials)
		zones[i].Stale = slices.Clone(zones[i].Stale)
		zones[i].Excluded = slices.Clone(zones[i].Excluded)
	}
	return zones
}
