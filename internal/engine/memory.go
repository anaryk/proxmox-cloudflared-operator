package engine

import (
	"fmt"
	"maps"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// recall reads the memory of an earlier process from the store, once. Until
// it is read, the caller must not act on the zones, the tunnels or the gone
// guests it holds: a memory that cannot be read is not an empty one. A memory
// of another install, as after a new setup on this node, is set aside, and
// the next cycle says so. The caller holds the cycle lock.
func (e *Engine) recall(installID string) error {
	if e.remembered {
		return nil
	}
	m, err := e.d.Store.EngineMemory()
	if err != nil {
		return err
	}
	e.memoryOf, e.remembered = installID, true
	if m.InstallID != "" && m.InstallID != installID {
		e.setAside = fmt.Sprintf("the engine memory on this node is of install %s, not %s; it is set aside and replaced",
			m.InstallID, installID)
		return nil
	}
	for _, z := range m.EverServed {
		e.zones.ever[z.ID] = planner.Zone{ID: z.ID, Name: z.Name, AccountID: z.AccountID, CredentialID: z.CredentialID}
	}
	for _, z := range m.Served {
		zone := planner.Zone{ID: z.ID, Name: z.Name, AccountID: z.AccountID, CredentialID: z.CredentialID}
		e.zones.serve(z.Name, zone)
		if m.Version == 0 {
			// Saved before the zones listed were kept apart, it may hold
			// them only here.
			e.zones.listed(zone)
		}
	}
	for _, z := range m.Stale {
		e.zones.credential(z.CredentialID).stale[z.Name] = cfapi.Zone{ID: z.ID, Name: z.Name, Status: "active", AccountID: z.AccountID}
	}
	for _, t := range m.Tunnels {
		e.seen[t.ID] = seenTunnel{account: t.AccountID, name: t.Name, credential: t.CredentialID}
	}
	for _, ref := range m.GoneGuests {
		e.gone[ref] = true
	}
	e.rememberEgress(m)
	e.repMu.Lock()
	defer e.repMu.Unlock()
	for _, r := range m.Reports {
		// A check of this process is newer.
		if _, ok := e.reports[r.CredentialID]; !ok {
			e.reports[r.CredentialID] = r.Report
		}
	}
	return nil
}

// memory is what the engine remembers, as the store keeps it.
func (e *Engine) memory() store.EngineMemory {
	m := store.EngineMemory{Version: store.MemoryVersion, InstallID: e.memoryOf}
	for _, name := range slices.Sorted(maps.Keys(e.zones.served)) {
		z := e.zones.served[name]
		m.Served = append(m.Served, store.RememberedZone{ID: z.ID, Name: z.Name, AccountID: z.AccountID, CredentialID: z.CredentialID})
	}
	for _, id := range slices.Sorted(maps.Keys(e.zones.byCred)) {
		cz := e.zones.byCred[id]
		for _, name := range slices.Sorted(maps.Keys(cz.stale)) {
			z := cz.stale[name]
			m.Stale = append(m.Stale, store.RememberedZone{ID: z.ID, Name: name, AccountID: z.AccountID, CredentialID: id})
		}
	}
	for _, id := range slices.Sorted(maps.Keys(e.zones.ever)) {
		z := e.zones.ever[id]
		m.EverServed = append(m.EverServed, store.RememberedZone{ID: id, Name: z.Name, AccountID: z.AccountID, CredentialID: z.CredentialID})
	}
	for _, id := range slices.Sorted(maps.Keys(e.seen)) {
		t := e.seen[id]
		m.Tunnels = append(m.Tunnels, store.SeenTunnel{ID: id, Name: t.name, AccountID: t.account, CredentialID: t.credential})
	}
	for ref := range e.gone {
		m.GoneGuests = append(m.GoneGuests, ref)
	}
	slices.SortFunc(m.GoneGuests, func(a, b model.GuestRef) int { return model.CompareOwners(a.String(), b.String()) })
	e.egressMemory(&m)
	e.repMu.Lock()
	defer e.repMu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(e.reports)) {
		m.Reports = append(m.Reports, store.CheckedCredential{CredentialID: id, Report: cloneReport(e.reports[id])})
	}
	return m
}

// memoryAccepting is the memory as it is once the engine accepted o: the
// guests are gone, and the zones still stale and the tunnels still seen are
// forgotten, a zone also as served through the credential it left; a zone let
// go is forgotten as served through the credential refused its DNS. Either
// stays remembered as served once when its records were listed: they may be
// there.
func (e *Engine) memoryAccepting(o confirmable) store.EngineMemory {
	m := e.memory()
	confirmed := make(map[staleZone]bool, len(o.stale))
	m.Stale = slices.DeleteFunc(m.Stale, func(z store.RememberedZone) bool {
		sz := staleZone{credential: z.CredentialID, name: z.Name}
		if slices.Contains(o.stale, sz) {
			confirmed[sz] = true
		}
		return confirmed[sz]
	})
	m.Served = slices.DeleteFunc(m.Served, func(z store.RememberedZone) bool {
		sz := staleZone{credential: z.CredentialID, name: z.Name}
		return confirmed[sz] || slices.ContainsFunc(o.stale, func(x staleZone) bool {
			return x.refused && x.credential == sz.credential && x.name == sz.name
		})
	})
	m.Tunnels = slices.DeleteFunc(m.Tunnels, func(t store.SeenTunnel) bool {
		return slices.ContainsFunc(o.invisible, func(u unseenTunnel) bool { return u.id == t.ID })
	})
	for _, ref := range o.vanished {
		if !slices.Contains(m.GoneGuests, ref) {
			m.GoneGuests = append(m.GoneGuests, ref)
		}
	}
	return m
}

// saveMemory stores what the engine remembers, once it was read; the store
// writes only a change. It reports whether the memory is safe in the store,
// and when it is not, why.
func (c *cycleRun) saveMemory() (string, bool) {
	if !c.e.remembered {
		return "what the engine remembered could not be read", false
	}
	if err := c.e.d.Store.SaveEngineMemory(c.e.memory()); err != nil {
		c.storeHold = true
		return c.problem("saving what the engine remembers: %v; nothing is changed at Cloudflare until it is saved", err), false
	}
	return "", true
}
