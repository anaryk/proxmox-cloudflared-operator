package engine

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// zoneRefreshEvery is how often the zones of every credential are listed.
const zoneRefreshEvery = 5 * time.Minute

// zoneCache is what was listed per credential, and which credential served
// each zone. It lives across cycles, in memory only.
type zoneCache struct {
	byCred   map[string]*credZones
	at       time.Time         // of the last refresh of every credential
	due      bool              // a credential changed: list again in the next cycle
	servedBy map[string]string // zone name -> the credential that last served it alone
}

// credZones is what one credential sees.
type credZones struct {
	zones  []cfapi.Zone // the active ones of the last listing that worked, sorted
	listed bool         // a zone listing worked at least once
	at     time.Time    // of the last zone listing that worked
	err    string       // why the last zone listing failed; empty when it worked
	// stale holds, by name, the zones a listing had and a later one did not.
	stale map[string]cfapi.Zone

	accounts    []cfapi.Account // of the last account listing that worked
	accountsOK  bool            // the account listing of this cycle worked
	accountsErr string
}

func newZoneCache() *zoneCache {
	return &zoneCache{byCred: make(map[string]*credZones), servedBy: make(map[string]string)}
}

// forget drops what was listed with a credential, its stale zones and the
// zones it served.
func (z *zoneCache) forget(id string) {
	if _, ok := z.byCred[id]; ok {
		delete(z.byCred, id)
		z.due = true
	}
	maps.DeleteFunc(z.servedBy, func(_, cred string) bool { return cred == id })
}

// confirmGone forgets every stale zone, as the admin confirmed that they are
// gone, and returns their names.
func (z *zoneCache) confirmGone() []string {
	var names []string
	for _, cz := range z.byCred {
		names = append(names, slices.Collect(maps.Keys(cz.stale))...)
		clear(cz.stale)
	}
	return slices.Compact(slices.Sorted(slices.Values(names)))
}

// update takes a zone listing that worked. A zone the previous listing had
// and this one has not becomes stale; a stale zone listed again is not.
func (cz *credZones) update(zones []cfapi.Zone, at time.Time) {
	listed := make(map[string]bool, len(zones))
	for _, zone := range zones {
		listed[zoneName(zone)] = true
	}
	if cz.stale == nil {
		cz.stale = make(map[string]cfapi.Zone)
	}
	if cz.listed {
		for _, old := range cz.zones {
			if name := zoneName(old); !listed[name] {
				cz.stale[name] = old
			}
		}
	}
	maps.DeleteFunc(cz.stale, func(name string, _ cfapi.Zone) bool { return listed[name] })
	cz.zones, cz.listed, cz.at, cz.err = zones, true, at, ""
}

// zoneName is the name of a zone in normal form, or as Cloudflare gave it
// when it does not normalise.
func zoneName(z cfapi.Zone) string {
	if name, err := hostname.Normalize(z.Name); err == nil {
		return name
	}
	return z.Name
}

// refreshZones lists the zones of each credential every zoneRefreshEvery,
// after a credential changed, and in every cycle for a credential that was
// never listed; a listing that fails keeps the previous list. The accounts of
// every credential are listed in every cycle, as pruning needs them current.
func (c *cycleRun) refreshZones(ids []string) {
	z := c.e.zones
	due := z.due || c.now.Sub(z.at) >= zoneRefreshEvery || c.now.Before(z.at)
	for _, id := range ids {
		cz := z.byCred[id]
		if cz == nil {
			cz = &credZones{}
			z.byCred[id] = cz
		}
		api := c.e.clients[id]
		if api == nil {
			cz.err, cz.accountsOK, cz.accountsErr = "it has no client", false, "it has no client"
			continue
		}
		if due || !cz.listed {
			if got, err := api.Zones(c.ctx); err != nil {
				cz.err = err.Error()
			} else {
				cz.update(activeZones(got), c.now)
			}
		}
		if got, err := api.Accounts(c.ctx); err != nil {
			cz.accountsOK, cz.accountsErr = false, err.Error()
		} else {
			cz.accounts, cz.accountsOK, cz.accountsErr = got, true, ""
		}
	}
	if due {
		z.at, z.due = c.now, false
	}
}

// activeZones keeps the zones that are active, sorted. A zone that is
// pending or moved serves nothing through pco.
func activeZones(zones []cfapi.Zone) []cfapi.Zone {
	out := slices.DeleteFunc(slices.Clone(zones), func(z cfapi.Zone) bool { return z.Status != "active" })
	slices.SortFunc(out, func(a, b cfapi.Zone) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	return out
}

// zoneSet is what a cycle makes of the zones.
type zoneSet struct {
	planned []planner.Zone      // for Build: every zone with the credential that serves it, and the zones in doubt
	dns     []reconcile.ZoneRef // the zones whose records the DNS run manages
	// known maps every account with a complete, current plan basis to the
	// credential its tunnel is managed through: each of its zones is listed
	// and none is in doubt. An account without a zone is not in it.
	known map[string]string
	// frozen holds the accounts with a zone in doubt: their tunnel, records
	// and connector are left as they are.
	frozen map[string]bool
	// accounts maps every account a credential sees to the first credential
	// that sees it.
	accounts map[string]string
	ready    bool // the zones of every credential are known
	problems []string
}

// zoneEntry is one credential's view of a zone.
type zoneEntry struct {
	zone  planner.Zone
	stale bool
}

// set works out the zones of the credentials ids, which are sorted.
//
// A zone is in doubt when more than one credential sees it, no pin names one
// of them and none of them served it alone before; when a pin names a
// credential that does not see it; or when it left the listing of its
// credential. The account of a zone in doubt is frozen. Several credentials
// with one that served the zone before keep that one, with a problem asking
// for a pin.
func (z *zoneCache) set(ids []string, pins map[string]string) zoneSet {
	out := zoneSet{known: map[string]string{}, frozen: map[string]bool{}, accounts: map[string]string{}, ready: true}
	byName := map[string][]zoneEntry{}
	for _, id := range ids {
		cz := z.byCred[id]
		switch {
		case cz == nil || !cz.listed:
			why := "never tried"
			if cz != nil && cz.err != "" {
				why = cz.err
			}
			out.ready = false
			out.problems = append(out.problems, fmt.Sprintf(
				"credential %s: its zones are not listed yet (%s); nothing is changed at Cloudflare until they are", id, why))
			continue
		case cz.err != "":
			out.problems = append(out.problems, fmt.Sprintf("credential %s: listing its zones failed (%s); using the list from %s",
				id, cz.err, cz.at.UTC().Format(time.RFC3339)))
		}
		for _, a := range cz.accounts {
			out.seen(a.ID, id)
		}
		add := func(zone cfapi.Zone, stale bool) {
			out.seen(zone.AccountID, id)
			name := zoneName(zone)
			byName[name] = append(byName[name], zoneEntry{
				zone:  planner.Zone{ID: zone.ID, Name: name, AccountID: zone.AccountID, CredentialID: id},
				stale: stale,
			})
		}
		for _, zone := range cz.zones {
			add(zone, false)
		}
		for _, name := range slices.Sorted(maps.Keys(cz.stale)) {
			add(cz.stale[name], true)
		}
	}

	var served []planner.Zone
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		entries := byName[name]
		chosen, note, doubt := z.choose(name, entries, pins[name])
		if note != "" {
			out.problems = append(out.problems, note)
		}
		if doubt != "" {
			out.problems = append(out.problems, doubt)
			for _, en := range entries {
				out.frozen[en.zone.AccountID] = true
				// Planned all the same, so that the routes keep their state.
				out.planned = append(out.planned, en.zone)
			}
			continue
		}
		out.planned = append(out.planned, chosen...)
		served = append(served, chosen...)
	}
	for _, zone := range served {
		if out.frozen[zone.AccountID] {
			continue
		}
		out.dns = append(out.dns, reconcile.ZoneRef{ID: zone.ID, Name: zone.Name, CredentialID: zone.CredentialID})
		if cur, ok := out.known[zone.AccountID]; !ok || zone.CredentialID < cur {
			out.known[zone.AccountID] = zone.CredentialID
		}
	}
	return out
}

// seen notes that credential id sees an account; ids come in order, so the
// first one stays.
func (s zoneSet) seen(account, id string) {
	if _, ok := s.accounts[account]; !ok {
		s.accounts[account] = id
	}
}

// choose decides which credential's view of a zone is used. note is a problem
// that leaves the zone served; doubt one that freezes its account.
func (z *zoneCache) choose(name string, entries []zoneEntry, pin string) (chosen []planner.Zone, note, doubt string) {
	var live []planner.Zone
	var staleBy []string
	for _, en := range entries {
		if en.stale {
			staleBy = append(staleBy, en.zone.CredentialID)
			continue
		}
		live = append(live, en.zone)
	}
	accounts := accountsOf(entries)
	if pin != "" {
		pinned := slices.DeleteFunc(slices.Clone(live), func(zone planner.Zone) bool { return zone.CredentialID != pin })
		if len(pinned) == 0 {
			return nil, "", fmt.Sprintf("zone %s is pinned to credential %s, which does not see it; %s is left as it is until the pin is fixed",
				name, pin, accounts)
		}
		z.servedBy[name] = pin
		return pinned, "", ""
	}
	if len(staleBy) > 0 {
		return nil, "", fmt.Sprintf("zone %s is no longer listed by credential %s; %s is left as it is "+
			"until the zone is listed again or pco apply --confirm-deletes confirms it is gone", name, andList(staleBy), accounts)
	}
	creds := credentialsOf(live)
	if len(creds) == 1 {
		z.servedBy[name] = creds[0]
		return live, "", ""
	}
	if prev, ok := z.servedBy[name]; ok && slices.Contains(creds, prev) {
		kept := slices.DeleteFunc(slices.Clone(live), func(zone planner.Zone) bool { return zone.CredentialID != prev })
		return kept, fmt.Sprintf("zone %s is visible through credentials %s; pin it with zonePins (%s serves it until then)",
			name, andList(creds), prev), ""
	}
	return nil, "", fmt.Sprintf("zone %s is visible through credentials %s and none of them served it before; "+
		"pin it with zonePins; %s is left as it is until then", name, andList(creds), accounts)
}

// accountsOf names the accounts of the entries: "account a" or "accounts a and b".
func accountsOf(entries []zoneEntry) string {
	var ids []string
	for _, en := range entries {
		ids = append(ids, en.zone.AccountID)
	}
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(ids) == 1 {
		return "account " + ids[0]
	}
	return "accounts " + andList(ids)
}

// andList joins "a", "a and b", "a, b and c".
func andList(items []string) string {
	items = slices.Compact(slices.Sorted(slices.Values(items)))
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

func credentialsOf(zs []planner.Zone) []string {
	ids := make([]string, 0, len(zs))
	for _, z := range zs {
		ids = append(ids, z.CredentialID)
	}
	return slices.Compact(slices.Sorted(slices.Values(ids)))
}

// zoneOf returns the name of the managed zone a record name belongs to.
func (s zoneSet) zoneOf(name string) string {
	names := make([]string, 0, len(s.dns))
	for _, z := range s.dns {
		names = append(names, z.Name)
	}
	zone, _ := hostname.MatchZone(strings.ToLower(strings.TrimSuffix(name, ".")), names)
	return zone
}
