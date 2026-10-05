package engine

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// zoneRefreshEvery is how often the zones of every credential are listed.
const zoneRefreshEvery = 5 * time.Minute

// zoneCache is what was listed per credential, and which credential served
// each zone. It lives across cycles; the served and the stale zones are also
// kept in the engine's memory in the store.
type zoneCache struct {
	byCred map[string]*credZones
	at     time.Time // of the last refresh of every credential
	due    bool      // a credential changed: list again in the next cycle
	// served holds, by zone name, the zone as it was last served by one
	// credential alone.
	served map[string]planner.Zone
	// ever holds, by id, every zone that was served at some time: it may
	// hold records of the install, also once it is no longer served.
	ever map[string]planner.Zone
	// lookups holds, by account, what the last lookup of the tunnel of an
	// account the tunnel run does not report on found. A lookup that failed
	// is not kept.
	lookups map[string]tunnelLookup
}

// tunnelLookup is what a lookup of the tunnel of an account found, through
// which credential and when.
type tunnelLookup struct {
	credential string
	at         time.Time
	found      bool
	tunnel     cfapi.Tunnel
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
	accountsAt  time.Time // of the last account listing that worked
}

func newZoneCache() *zoneCache {
	return &zoneCache{
		byCred: make(map[string]*credZones), served: make(map[string]planner.Zone), ever: make(map[string]planner.Zone),
		lookups: make(map[string]tunnelLookup),
	}
}

// serve has zone served under name, by its credential alone.
func (z *zoneCache) serve(name string, zone planner.Zone) {
	z.served[name] = zone
	z.ever[zone.ID] = zone
}

// servedOnce returns the names of the zones that were served at some time.
func (z *zoneCache) servedOnce() []string {
	names := make([]string, 0, len(z.ever))
	for _, zone := range z.ever {
		names = append(names, zone.Name)
	}
	return slices.Compact(slices.Sorted(slices.Values(names)))
}

// credential returns what is known of credential id, made empty when nothing
// is.
func (z *zoneCache) credential(id string) *credZones {
	cz := z.byCred[id]
	if cz == nil {
		cz = &credZones{stale: make(map[string]cfapi.Zone)}
		z.byCred[id] = cz
	}
	return cz
}

// servedThrough returns the zones credential id served when they were last
// served, as a listing would show them.
func (z *zoneCache) servedThrough(id string) []cfapi.Zone {
	var out []cfapi.Zone
	for _, name := range slices.Sorted(maps.Keys(z.served)) {
		if zone := z.served[name]; zone.CredentialID == id {
			out = append(out, cfapi.Zone{ID: zone.ID, Name: zone.Name, Status: "active", AccountID: zone.AccountID})
		}
	}
	return out
}

// forget drops what was listed with a credential, its stale zones and the
// zones it served.
func (z *zoneCache) forget(id string) {
	if _, ok := z.byCred[id]; ok {
		delete(z.byCred, id)
		z.due = true
	}
	maps.DeleteFunc(z.served, func(_ string, zone planner.Zone) bool { return zone.CredentialID == id })
	maps.DeleteFunc(z.lookups, func(_ string, l tunnelLookup) bool { return l.credential == id })
}

// lookup returns what the last lookup of the tunnel of an account through a
// credential found, while it is as new as the last account listing of that
// credential: an account is looked up again whenever the accounts are listed.
func (z *zoneCache) lookup(account, credential string, now time.Time) (tunnelLookup, bool) {
	l, ok := z.lookups[account]
	cz := z.byCred[credential]
	if !ok || cz == nil || l.credential != credential || l.at.Before(cz.accountsAt) || now.Before(l.at) {
		return tunnelLookup{}, false
	}
	return l, true
}

// confirmGone forgets the stale zones the admin confirmed gone, also as zones
// served, and the refused ones as served through their credential. It returns
// the names of those that were still stale, and of those let go that were
// still served through it.
func (z *zoneCache) confirmGone(zones []staleZone) (gone, letGo []string) {
	for _, sz := range zones {
		if sz.refused {
			if z.served[sz.name].CredentialID == sz.credential {
				delete(z.served, sz.name)
				letGo = append(letGo, sz.name)
			}
			continue
		}
		cz := z.byCred[sz.credential]
		if cz == nil {
			continue
		}
		if _, ok := cz.stale[sz.name]; !ok {
			continue
		}
		delete(cz.stale, sz.name)
		if z.served[sz.name].CredentialID == sz.credential {
			delete(z.served, sz.name)
		}
		gone = append(gone, sz.name)
	}
	return slices.Compact(slices.Sorted(slices.Values(gone))), slices.Compact(slices.Sorted(slices.Values(letGo)))
}

// zoneCheck is what the last check of a credential that was kept found of
// the zones it lists, each by id, and when the token was and is checked.
type zoneCheck struct {
	looked   map[string]bool // read the DNS of, or tried to
	excluded map[string]bool // left out: its DNS was refused
	again    map[string]bool // left out by the check before as well
	tried    checkTry        // the last check made, kept or not
	next     time.Time       // when the next check is due; zero when it is
}

// unchecked reports whether the check did not look at a zone.
func (chk zoneCheck) unchecked(z cfapi.Zone) bool { return !chk.excluded[z.ID] && !chk.looked[z.ID] }

// zoneChecks returns, by credential id, what the last check of each
// credential found of its zones. Of a credential that was never checked, or
// whose last check listed no zone, nothing is known: it is not in it.
func (e *Engine) zoneChecks() map[string]zoneCheck {
	e.repMu.Lock()
	defer e.repMu.Unlock()
	out := make(map[string]zoneCheck, len(e.reports))
	for id, r := range e.reports {
		if len(r.Zones) == 0 {
			continue
		}
		chk := zoneCheck{
			looked: map[string]bool{}, excluded: map[string]bool{}, again: e.refusedAgain[id],
			tried: e.tried[id], next: e.recheckAt[id],
		}
		for _, c := range r.Checks {
			if c.Capability == credentials.CapDNSRead && c.ScopeID != "" {
				chk.looked[c.ScopeID] = true
			}
		}
		for _, x := range r.Excluded {
			chk.excluded[x.ZoneID] = true
		}
		out[id] = chk
	}
	return out
}

// update takes a zone listing that worked. A zone the previous listing had
// and this one has not becomes stale, unless the credential leaves it out
// (by id, in excluded) and never served it; a stale zone listed again is
// not. The first listing in a process compares with the zones the credential
// served before, so that a zone that left while the daemon was down is
// noticed.
func (cz *credZones) update(zones []cfapi.Zone, at time.Time, servedBefore []cfapi.Zone, excluded map[string]bool) {
	listed := make(map[string]bool, len(zones))
	for _, zone := range zones {
		listed[zoneName(zone)] = true
	}
	served := make(map[string]bool, len(servedBefore))
	for _, zone := range servedBefore {
		served[zoneName(zone)] = true
	}
	if cz.stale == nil {
		cz.stale = make(map[string]cfapi.Zone)
	}
	previous := cz.zones
	if !cz.listed {
		previous = servedBefore
	}
	for _, old := range previous {
		if name := zoneName(old); !listed[name] && (served[name] || !excluded[old.ID]) {
			cz.stale[name] = old
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

// refreshZones lists the zones and the accounts of each credential every
// zoneRefreshEvery, after a credential changed, and in every cycle for a
// credential whose last listing failed or that was never listed, so that a
// failure clears as soon as Cloudflare answers again; a listing that fails
// keeps the previous list.
func (c *cycleRun) refreshZones(ids []string, checks map[string]zoneCheck) {
	z := c.e.zones
	due := z.due || c.now.Sub(z.at) >= zoneRefreshEvery || c.now.Before(z.at)
	for _, id := range ids {
		cz := z.credential(id)
		api := c.e.clients[id]
		if api == nil {
			cz.err, cz.accountsOK, cz.accountsErr = "it has no client", false, "it has no client"
			continue
		}
		if due || !cz.listed || cz.err != "" {
			if got, err := api.Zones(c.ctx); err != nil {
				cz.err = err.Error()
			} else {
				cz.update(activeZones(got), c.now, z.servedThrough(id), checks[id].excluded)
			}
		}
		if due || !cz.accountsOK {
			if got, err := api.Accounts(c.ctx); err != nil {
				cz.accountsOK, cz.accountsErr = false, err.Error()
			} else {
				cz.accounts, cz.accountsOK, cz.accountsErr, cz.accountsAt = got, true, "", c.now
			}
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
	// and connector are left as they are. frozenWhy says why, by account.
	frozen    map[string]bool
	frozenWhy map[string]string
	// accounts maps every account a credential sees to the first credential
	// that sees it.
	accounts map[string]string
	// unlisted is the problem line of the first credential whose zones
	// are not known yet; empty when those of every credential are.
	unlisted string
	problems []string
	// staleShown are the stale zones the problems name, in staleLines.
	staleShown []staleZone
	staleLines []string
	// excluded maps every zone that is not served because the credentials
	// that list it leave it out to their ids.
	excluded map[string][]string
	// recheck are the credentials whose last check, answered, is older than
	// the listing of a zone it did not look at: they are to be checked again
	// now. One whose last check got no answer waits for its next one.
	recheck []string
}

// zoneEntry is one credential's view of a zone.
type zoneEntry struct {
	zone      planner.Zone
	stale     bool
	excluded  bool      // the credential lists the zone but may not read its DNS
	again     bool      // excluded by the last two checks of the credential
	unchecked bool      // the last check of the credential did not look at the zone
	next      time.Time // when the token of the credential is checked next
}

// set works out the zones of the credentials ids, which are sorted. A zone
// the last check of a credential left out, as checks say, is not served
// through it, and the credential does not see its account for it.
//
// A zone is in doubt when more than one credential can serve it, no pin
// names one of them and none of them served it alone before; when a pin
// names a credential that does not see it; when it left the listing of its
// credential; when the credential that served it was refused its DNS; or
// when no credential can serve it but one whose last check did not look at
// it. The account of a zone in doubt is frozen. Several credentials with one
// that served the zone before keep that one, with a problem asking for a pin.
// A pin to a credential that leaves the zone out serves it through none, with
// a problem.
func (z *zoneCache) set(ids []string, pins map[string]string, checks map[string]zoneCheck, now time.Time) zoneSet {
	out := zoneSet{
		known: map[string]string{}, frozen: map[string]bool{}, frozenWhy: map[string]string{},
		accounts: map[string]string{}, excluded: map[string][]string{},
	}
	byName := map[string][]zoneEntry{}
	for _, id := range ids {
		cz := z.byCred[id]
		switch {
		case cz == nil || !cz.listed:
			why := "never tried"
			if cz != nil && cz.err != "" {
				why = cz.err
			}
			line := fmt.Sprintf("credential %s: its zones are not listed yet (%s); nothing is changed at Cloudflare until they are", id, why)
			out.unlisted = cmp.Or(out.unlisted, line)
			out.problems = append(out.problems, line)
			continue
		case cz.err != "":
			out.problems = append(out.problems, fmt.Sprintf("credential %s: listing its zones failed (%s); using the list from %s",
				id, cz.err, cz.at.UTC().Format(time.RFC3339)))
		}
		for _, a := range cz.accounts {
			out.seen(a.ID, id)
		}
		chk, checked := checks[id]
		next := now
		if chk.next.After(now) {
			next = chk.next
		}
		if checked && chk.tried.answered && chk.tried.at.Before(cz.at) && slices.ContainsFunc(cz.zones, chk.unchecked) {
			out.recheck = append(out.recheck, id)
			next = now
		}
		add := func(zone cfapi.Zone, stale bool) {
			name := zoneName(zone)
			en := zoneEntry{zone: planner.Zone{ID: zone.ID, Name: name, AccountID: zone.AccountID, CredentialID: id}, stale: stale, next: next}
			if checked && !stale {
				en.excluded, en.again = chk.excluded[zone.ID], chk.again[zone.ID]
				en.unchecked = chk.unchecked(zone)
			}
			if !en.excluded {
				out.seen(zone.AccountID, id)
			}
			byName[name] = append(byName[name], en)
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
		ch := z.choose(name, entries, pins[name])
		out.staleShown = append(out.staleShown, ch.offered...)
		if ch.note != "" {
			out.problems = append(out.problems, ch.note)
		}
		if ch.doubt != "" {
			out.problems = append(out.problems, ch.doubt)
			if len(ch.offered) > 0 {
				out.staleLines = append(out.staleLines, ch.doubt)
			}
			why, _, _ := strings.Cut(ch.doubt, "; ")
			for _, en := range entries {
				out.frozen[en.zone.AccountID] = true
				if _, ok := out.frozenWhy[en.zone.AccountID]; !ok {
					out.frozenWhy[en.zone.AccountID] = why
				}
				// Planned all the same, so that the routes keep their state.
				out.planned = append(out.planned, en.zone)
			}
			continue
		}
		if by := excludedBy(entries); len(ch.chosen) == 0 && len(by) > 0 {
			out.excluded[name] = by
			// Planned without a credential, so that a name in it is not
			// taken for one of a parent zone.
			out.planned = append(out.planned, planner.Zone{ID: entries[0].zone.ID, Name: name})
		}
		out.planned = append(out.planned, ch.chosen...)
		served = append(served, ch.chosen...)
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

// choice is what is made of a zone: the credential that serves it, or why
// none does.
type choice struct {
	chosen []planner.Zone
	note   string // a problem that does not freeze the account of the zone
	doubt  string // a problem that does
	// offered are the zones a confirmation lets go, which ends the doubt.
	offered []staleZone
}

// choose decides which credential's view of a zone is used. A zone that
// every credential leaves out is chosen through none.
func (z *zoneCache) choose(name string, entries []zoneEntry, pin string) choice {
	var live []planner.Zone
	var stale []string
	var unchecked []zoneEntry
	for _, en := range entries {
		switch {
		case en.stale:
			stale = append(stale, en.zone.CredentialID)
		case en.unchecked:
			unchecked = append(unchecked, en)
		case !en.excluded:
			live = append(live, en.zone)
		}
	}
	accounts := accountsOf(entries)
	pinned := slices.DeleteFunc(slices.Clone(live), func(zone planner.Zone) bool { return zone.CredentialID != pin })
	if pin != "" && len(pinned) > 0 {
		z.serve(name, pinned[0])
		return choice{chosen: pinned}
	}
	if en, ok := z.refused(name, entries); ok {
		return refusedChoice(name, en, credentialsOf(live), accounts)
	}
	pinUnchecked := slices.DeleteFunc(slices.Clone(unchecked), func(en zoneEntry) bool { return en.zone.CredentialID != pin })
	switch {
	case pin != "" && len(pinUnchecked) > 0:
		return uncheckedChoice(name, pinUnchecked, accounts)
	case pin != "" && slices.Contains(excludedBy(entries), pin):
		return choice{note: fmt.Sprintf("zone %s is pinned to credential %s, which can list it but not read its DNS; "+
			"it is not served until the pin is changed or the credential is granted Zone > DNS > Edit on it", name, pin)}
	case pin != "":
		return choice{doubt: fmt.Sprintf("zone %s is pinned to credential %s, which does not see it; %s is left as it is until the pin is fixed",
			name, pin, accounts)}
	case len(stale) > 0:
		offered := make([]staleZone, len(stale))
		for i, cred := range stale {
			offered[i] = staleZone{credential: cred, name: name}
		}
		return choice{offered: offered, doubt: fmt.Sprintf("zone %s is no longer listed by credential %s; %s is left as it is "+
			"until the zone is listed again or pco apply --confirm-deletes confirms it is gone", name, andList(stale), accounts)}
	}
	creds := credentialsOf(live)
	switch len(creds) {
	case 0:
		if len(unchecked) > 0 {
			return uncheckedChoice(name, unchecked, accounts)
		}
		return choice{}
	case 1:
		z.serve(name, live[0])
		return choice{chosen: live}
	}
	if prev, ok := z.served[name]; ok && slices.Contains(creds, prev.CredentialID) {
		kept := slices.DeleteFunc(slices.Clone(live), func(zone planner.Zone) bool { return zone.CredentialID != prev.CredentialID })
		return choice{chosen: kept, note: fmt.Sprintf("zone %s is visible through credentials %s; pin it with zonePins (%s serves it until then)",
			name, andList(creds), prev.CredentialID)}
	}
	return choice{doubt: fmt.Sprintf("zone %s is visible through credentials %s and none of them served it before; "+
		"pin it with zonePins; %s is left as it is until then", name, andList(creds), accounts)}
}

// refused returns the entry of the credential that served the zone, when the
// last check of that credential left the zone out.
func (z *zoneCache) refused(name string, entries []zoneEntry) (zoneEntry, bool) {
	prev, ok := z.served[name]
	if !ok {
		return zoneEntry{}, false
	}
	i := slices.IndexFunc(entries, func(en zoneEntry) bool { return en.excluded && en.zone.CredentialID == prev.CredentialID })
	if i < 0 {
		return zoneEntry{}, false
	}
	return entries[i], true
}

// refusedChoice is the doubt of a zone whose DNS was refused to the
// credential that serves it, while readers can read it. One refusal may be
// Cloudflare's own: the problem says when the token is checked again. Two in
// a row are a problem that ends when a check finds the DNS readable again, a
// pin gives the zone to a reader, or a confirmation lets the zone go.
func refusedChoice(name string, en zoneEntry, readers []string, accounts string) choice {
	cred := en.zone.CredentialID
	if !en.again {
		return choice{doubt: fmt.Sprintf("the token of credential %s could not read the DNS of zone %s, which it serves; "+
			"%s is left as it is, checking again at %s", cred, name, accounts, clockTime(en.next))}
	}
	ways := "a check finds it readable again or pco apply --confirm-deletes lets the zone go"
	if len(readers) > 0 {
		ways = fmt.Sprintf("a check finds it readable again, a pin gives the zone to credential %s, which can read it, "+
			"or pco apply --confirm-deletes lets the zone go", joinList(readers, "or"))
	}
	return choice{
		doubt: fmt.Sprintf("credential %s can no longer read the DNS of zone %s, which it serves: grant it Zone > DNS > Edit there; "+
			"%s is left as it is until %s", cred, name, accounts, ways),
		offered: []staleZone{{credential: cred, name: name, refused: true, readers: joinList(readers, "or")}},
	}
}

// uncheckedChoice is the doubt of a zone that only credentials whose last
// check did not look at it could serve, as entries: its DNS may be one they
// cannot read. The problem says when the first of them is checked again.
func uncheckedChoice(name string, entries []zoneEntry, accounts string) choice {
	creds := make([]string, len(entries))
	next := entries[0].next
	for i, en := range entries {
		creds[i] = en.zone.CredentialID
		if en.next.Before(next) {
			next = en.next
		}
	}
	return choice{doubt: fmt.Sprintf("zone %s is listed by credential %s, whose last check did not look at it; "+
		"%s is left as it is, checking again at %s", name, andList(creds), accounts, clockTime(next))}
}

// clockTime is a time of the day as the engine's clock gives it: in the
// daemon, the local time of the node.
func clockTime(t time.Time) string { return t.Format("15:04") }

// excludedBy returns the ids of the credentials whose entries leave the zone
// out, sorted.
func excludedBy(entries []zoneEntry) []string {
	var ids []string
	for _, en := range entries {
		if en.excluded {
			ids = append(ids, en.zone.CredentialID)
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(ids)))
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
func andList(items []string) string { return joinList(items, "and") }

// joinList sorts items and joins them as "a", "a and b", "a, b and c", with
// word for "and".
func joinList(items []string, word string) string {
	items = slices.Compact(slices.Sorted(slices.Values(items)))
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " " + word + " " + items[len(items)-1]
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
