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
// note says so. The caller holds the cycle lock.
func (e *Engine) recall(installID string) (note string, err error) {
	if e.remembered {
		return "", nil
	}
	m, err := e.d.Store.EngineMemory()
	if err != nil {
		return "", err
	}
	e.memoryOf, e.remembered = installID, true
	if m.InstallID != "" && m.InstallID != installID {
		return fmt.Sprintf("the engine memory on this node is of install %s, not %s; it is set aside and replaced",
			m.InstallID, installID), nil
	}
	for _, z := range m.Served {
		e.zones.served[z.Name] = planner.Zone{ID: z.ID, Name: z.Name, AccountID: z.AccountID, CredentialID: z.CredentialID}
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
	return "", nil
}

// memory is what the engine remembers, as the store keeps it.
func (e *Engine) memory() store.EngineMemory {
	m := store.EngineMemory{InstallID: e.memoryOf}
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
	for _, id := range slices.Sorted(maps.Keys(e.seen)) {
		t := e.seen[id]
		m.Tunnels = append(m.Tunnels, store.SeenTunnel{ID: id, Name: t.name, AccountID: t.account, CredentialID: t.credential})
	}
	for ref := range e.gone {
		m.GoneGuests = append(m.GoneGuests, ref)
	}
	slices.SortFunc(m.GoneGuests, func(a, b model.GuestRef) int { return model.CompareOwners(a.String(), b.String()) })
	return m
}

// saveMemory stores what the engine remembers, once it was read; the store
// writes only a change. It reports whether the memory is safe in the store.
func (c *cycleRun) saveMemory() bool {
	if !c.e.remembered {
		return false
	}
	if err := c.e.d.Store.SaveEngineMemory(c.e.memory()); err != nil {
		c.problem("saving what the engine remembers: %v; nothing is changed at Cloudflare until it is saved", err)
		return false
	}
	return true
}
