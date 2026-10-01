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

// zoneCache is what was listed per credential. It lives across cycles.
type zoneCache struct {
	byCred map[string]*credZones
	at     time.Time // of the last refresh of every credential
	due    bool      // a credential changed: list again in the next cycle
}

// credZones is the zones of one credential.
type credZones struct {
	zones  []cfapi.Zone // active ones, sorted by name and id
	listed bool         // a listing worked at least once
	at     time.Time    // of the last listing that worked
	err    string       // why the last listing failed; empty when it worked
}

func newZoneCache() *zoneCache { return &zoneCache{byCred: make(map[string]*credZones)} }

func (z *zoneCache) forget(id string) {
	if _, ok := z.byCred[id]; ok {
		delete(z.byCred, id)
		z.due = true
	}
}

// refreshZones lists the zones of each credential every zoneRefreshEvery,
// after a credential changed, and in every cycle for a credential that was
// never listed. A listing that fails keeps the previous list.
func (c *cycleRun) refreshZones(ids []string) {
	z := c.e.zones
	due := z.due || c.now.Sub(z.at) >= zoneRefreshEvery || c.now.Before(z.at)
	for _, id := range ids {
		cz := z.byCred[id]
		if cz == nil {
			cz = &credZones{}
			z.byCred[id] = cz
		}
		if !due && cz.listed {
			continue
		}
		api := c.e.clients[id]
		if api == nil {
			cz.err = "it has no client"
			continue
		}
		got, err := api.Zones(c.ctx)
		if err != nil {
			cz.err = err.Error()
			continue
		}
		cz.zones, cz.listed, cz.at, cz.err = activeZones(got), true, c.now, ""
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
	planned []planner.Zone      // for Build: every zone with the credential that sees it, pins applied
	dns     []reconcile.ZoneRef // the zones whose records the DNS reconciler manages: one credential each
	known   map[string]string   // account id -> credential id of every account a zone was planned in
	ready   bool                // the zones of every credential are known
	// problems are the listings that failed and the zones that need a pin.
	problems []string
}

// set works out the zones of the credentials ids. A zone pinned to a
// credential is used only through it. A zone that more than one credential
// sees without a pin gets no records managed: Build gives its routes no zone,
// and its records are left as they are until the admin pins it.
func (z *zoneCache) set(ids []string, pins map[string]string) zoneSet {
	out := zoneSet{known: make(map[string]string), ready: true}
	byName := make(map[string][]planner.Zone)
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
		for _, zone := range cz.zones {
			name, err := hostname.Normalize(zone.Name)
			if err != nil {
				out.problems = append(out.problems, fmt.Sprintf("credential %s: zone %s: %v", id, zone.ID, err))
				continue
			}
			byName[name] = append(byName[name], planner.Zone{ID: zone.ID, Name: name, AccountID: zone.AccountID, CredentialID: id})
		}
	}
	out.problems = append(out.problems, applyPins(byName, pins)...)

	for _, name := range slices.Sorted(maps.Keys(byName)) {
		zs := byName[name]
		out.planned = append(out.planned, zs...)
		for _, zone := range zs {
			if cur, ok := out.known[zone.AccountID]; !ok || zone.CredentialID < cur {
				out.known[zone.AccountID] = zone.CredentialID
			}
		}
		if creds := credentialsOf(zs); len(creds) > 1 {
			out.problems = append(out.problems, fmt.Sprintf(
				"zone %s is visible through credentials %s; pin it to one in zonePins", name, strings.Join(creds, ", ")))
			continue
		}
		for _, zone := range zs {
			out.dns = append(out.dns, reconcile.ZoneRef{ID: zone.ID, Name: zone.Name, CredentialID: zone.CredentialID})
		}
	}
	return out
}

// applyPins keeps only the pinned credential's view of each pinned zone.
func applyPins(byName map[string][]planner.Zone, pins map[string]string) []string {
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(pins)) {
		zs, seen := byName[name]
		if !seen {
			continue
		}
		pinned := slices.DeleteFunc(slices.Clone(zs), func(z planner.Zone) bool { return z.CredentialID != pins[name] })
		if len(pinned) == 0 {
			problems = append(problems, fmt.Sprintf("zone %s is pinned to credential %s, which does not see it", name, pins[name]))
			delete(byName, name)
			continue
		}
		byName[name] = pinned
	}
	return problems
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
