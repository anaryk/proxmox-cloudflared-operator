package engine

import (
	"maps"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// syncCredentials reads the credentials, keeps a client for each and works
// out the zones. It returns false when the credentials cannot be read.
// Credentials that cannot be read, none at all, or zones that were never
// listed hold Cloudflare.
func (c *cycleRun) syncCredentials() bool {
	creds, err := c.e.d.Store.Credentials()
	if err != nil {
		c.storeProblem("reading the credentials", err)
		c.cfHold = true
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
	if len(creds) == 0 {
		c.problem(problemNoCredential)
		c.cfHold = true
		return true
	}
	c.refreshZones(ids)
	c.credIDs = ids
	c.zones = c.e.zones.set(ids, c.settings.ZonePins)
	c.offer.stale = c.zones.staleShown
	c.offer.lines = append(c.offer.lines, c.zones.staleLines...)
	c.st.Problems = append(c.st.Problems, c.zones.problems...)
	if !c.zones.ready {
		c.cfHold = true
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
			e.repMu.Lock()
			delete(e.reports, cr.ID)
			e.repMu.Unlock()
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
	e.repMu.Unlock()
}

// dropClient forgets the client of a credential and what was listed with it.
func (e *Engine) dropClient(id string) {
	delete(e.clients, id)
	delete(e.tokens, id)
	e.zones.forget(id)
}
