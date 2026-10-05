package engine

import (
	"maps"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// syncCredentials reads the credentials, keeps a client for each and works
// out the zones, without those the last check of a credential left out. A
// credential whose last check did not look at a zone it lists now is checked
// again at once. It returns false when the credentials cannot be read.
// Credentials that cannot be read, none at all, or zones that were never
// listed hold Cloudflare.
func (c *cycleRun) syncCredentials() bool {
	creds, err := c.e.d.Store.Credentials()
	if err != nil {
		c.hold(c.storeProblem("reading the credentials", err))
		return false
	}
	c.e.syncClients(c, creds)
	info := make([]credentialInfo, len(creds))
	ids := make([]string, len(creds))
	for i, cr := range creds {
		info[i] = credentialInfo{id: cr.ID, label: cr.Label, kind: cr.Kind}
		ids[i] = cr.ID
	}
	c.st.Credentials = c.e.credentialViews(info)
	for _, v := range c.st.Credentials {
		if v.Checked && !v.Report.Usable {
			c.problem("credential %s (%s): its last check found that the token cannot be used: %s; "+
				"once it is fixed, pco credential check %s says so", v.ID, v.Label, failedChecks(v.Report), v.ID)
		}
	}
	if len(creds) == 0 {
		c.st.Zones = nil
		c.hold(c.problem(problemNoCredential))
		return true
	}
	checks := c.e.zoneChecks()
	c.refreshZones(ids, checks)
	c.credIDs = ids
	c.zones = c.e.zones.set(ids, c.settings.ZonePins, checks, c.now, c.storeHold || c.e.storeHeld.Load())
	c.st.Zones = c.zones.views
	for _, id := range c.zones.recheck {
		c.e.recheckBy(id, c.now)
	}
	c.offer.stale = c.zones.staleShown
	c.offer.lines = append(c.offer.lines, c.zones.staleLines...)
	c.st.Problems = append(c.st.Problems, c.zones.problems...)
	if c.zones.unlisted != "" {
		c.hold(c.zones.unlisted)
	}
	return true
}

// syncClients keeps one client per stored credential, built anew when its
// token changed. A credential whose client is new has its zones listed anew.
func (e *Engine) syncClients(c *cycleRun, creds []store.Credential) {
	seen := make(map[string]bool, len(creds))
	for _, cr := range creds {
		seen[cr.ID] = true
		old, known := e.tokens[cr.ID]
		if known && old.Equal(cr.Token) && e.clients[cr.ID] != nil {
			continue
		}
		// A new token keeps what the credential listed until it lists anew.
		delete(e.clients, cr.ID)
		delete(e.tokens, cr.ID)
		if known && !old.Equal(cr.Token) {
			e.forgetReport(cr.ID)
		}
		api, err := e.d.NewClient(cr)
		if err != nil {
			c.problem("credential %s: building its client: %v", cr.ID, err)
			continue
		}
		e.clients[cr.ID] = api
		e.tokens[cr.ID] = cr.Token
		e.zones.due = true
	}
	known := slices.Concat(slices.Collect(maps.Keys(e.tokens)), slices.Collect(maps.Keys(e.zones.byCred)))
	for _, id := range slices.Compact(slices.Sorted(slices.Values(known))) {
		if !seen[id] {
			e.dropClient(id)
		}
	}
	e.repMu.Lock()
	maps.DeleteFunc(e.reports, func(id string, _ credentials.Report) bool { return !seen[id] })
	maps.DeleteFunc(e.refusedAgain, func(id string, _ map[string]bool) bool { return !seen[id] })
	maps.DeleteFunc(e.tried, func(id string, _ checkTry) bool { return !seen[id] })
	maps.DeleteFunc(e.recheckAt, func(id string, _ time.Time) bool { return !seen[id] })
	e.repMu.Unlock()
}

// dropClient forgets the client of a credential and what was listed with it.
func (e *Engine) dropClient(id string) {
	delete(e.clients, id)
	delete(e.tokens, id)
	e.zones.forget(id)
}
