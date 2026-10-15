package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
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
	if len(m.Guests) > 0 {
		e.rememberGuests(m.Guests)
	}
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
	if e.clustered {
		m.Guests = e.guestsMemory()
	}
	e.repMu.Lock()
	defer e.repMu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(e.reports)) {
		m.Reports = append(m.Reports, store.CheckedCredential{CredentialID: id, Report: cloneReport(e.reports[id])})
	}
	return m
}

const (
	// guestsBudget is how much of the memory the guests take at most; the
	// cluster filesystem takes no file over 1 MiB.
	guestsBudget = 256 << 10
	// rememberedFor is how long after it was read from Proxmox a guest is
	// kept and given to the inventory of a later controller: older, it says
	// nothing safe of the guest now. A time read further in the future than
	// readSkew is not taken. guestsConfirmed is how often the times they
	// were read are written again while the guests stay the same.
	rememberedFor   = 24 * time.Hour
	readSkew        = 5 * time.Minute
	guestsConfirmed = time.Hour

	problemGuestsLeftOut = "%d guests with routes are left out of the memory, past its %d KiB for them: " +
		"a controller that takes over while the node of one is down holds until the node is back"
)

// guestsMemory is what the memory keeps of the guests: those the inventory
// remembers that are recent enough, as many as guestsBudget takes, each
// with when it was read. While they stay the same, the times they were read
// are written again only guestsConfirmed after they last were, so that the
// memory is not written every cycle.
func (e *Engine) guestsMemory() []store.RememberedGuest {
	var out []store.RememberedGuest
	size, left := 0, 0
	now := e.d.Now()
	for _, r := range e.d.Inventory.Remembered() {
		if !recentRead(r.ReadAt, now) {
			continue
		}
		g := store.RememberedGuest{Ref: r.Ref, Digest: r.Digest, Values: r.Values, ReadAt: r.ReadAt}
		raw, err := json.Marshal(g)
		if err != nil || size+len(raw)+1 > guestsBudget {
			left++
			continue
		}
		size += len(raw) + 1
		out = append(out, g)
	}
	e.guestsLeftOut = left
	sum := sha256.New()
	for _, g := range out {
		g.ReadAt = time.Time{}
		raw, _ := json.Marshal(g)
		sum.Write(raw)
	}
	digest := hex.EncodeToString(sum.Sum(nil))
	if digest != e.guestsDigest || now.Sub(e.guestsAt) >= guestsConfirmed {
		e.guestsDigest, e.guestsAt, e.guestsKept = digest, now, out
	}
	return e.guestsKept
}

// recentRead says whether a guest read at at may still be used at now.
func recentRead(at, now time.Time) bool {
	return !at.IsZero() && now.Sub(at) < rememberedFor && at.Sub(now) <= readSkew
}

// rememberGuests gives the inventory the guests an earlier controller read
// that are recent enough: a guest of a node that is offline now is built
// from them, rather than holding every cycle as one the inventory cannot
// tell from a guest that is gone.
func (e *Engine) rememberGuests(gs []store.RememberedGuest) {
	rs := make([]inventory.Remembered, 0, len(gs))
	now := e.d.Now()
	for _, g := range gs {
		if recentRead(g.ReadAt, now) {
			rs = append(rs, inventory.Remembered{Ref: g.Ref, Digest: g.Digest, Values: g.Values, ReadAt: g.ReadAt})
		}
	}
	e.d.Inventory.Remember(rs)
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
	m := c.e.memory()
	if n := c.e.guestsLeftOut; n > 0 && !c.guestsSaid {
		c.guestsSaid = true
		c.problem(problemGuestsLeftOut, n, guestsBudget>>10)
	}
	err := c.e.d.Store.SaveEngineMemory(m)
	if err != nil && len(m.Guests) > 0 {
		// The guests are kept for a takeover alone: the rest of the memory
		// is saved without them rather than holding the cycle.
		c.problem("saving the guests with routes in what the engine remembers: %v; it is saved without them", err)
		m.Guests = nil
		err = c.e.d.Store.SaveEngineMemory(m)
	}
	if err != nil {
		c.storeHold = true
		return c.problem("saving what the engine remembers: %v; nothing is changed at Cloudflare until it is saved", err), false
	}
	return "", true
}
