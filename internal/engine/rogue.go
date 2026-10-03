package engine

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// RogueConnector is a connector that Cloudflare lists on a tunnel of the
// install and that pco does not run on this node. Whoever runs it holds the
// tunnel's run token and gets a share of the requests to every hostname of
// the tunnel.
type RogueConnector struct {
	Tunnel   string `json:"tunnel"`
	TunnelID string `json:"tunnelId"`
	Account  string `json:"accountId"`
	ID       string `json:"id"`
	// OriginIP and Version are what Cloudflare says of it: where its
	// connections come from and the cloudflared it runs.
	OriginIP string    `json:"originIp,omitempty"`
	Version  string    `json:"version,omitempty"`
	Since    time.Time `json:"since"` // when a listing first showed it
}

// Text names the connector with what Cloudflare says of it, as in
// "c1 from 198.51.100.7 (cloudflared 2026.9.1)".
func (r RogueConnector) Text() string {
	version := "of an unknown version"
	if r.Version != "" {
		version = r.Version
	}
	return fmt.Sprintf("%s from %s (cloudflared %s)", r.ID, cmp.Or(r.OriginIP, "an unknown address"), version)
}

func compareRogues(a, b RogueConnector) int {
	return cmp.Or(cmp.Compare(a.Account, b.Account), cmp.Compare(a.TunnelID, b.TunnelID), cmp.Compare(a.ID, b.ID))
}

// watchConnectors lists the connectors Cloudflare shows on each tunnel the
// tunnel run found, when that is due, to see its configuration rolled out and
// to find the connectors that pco does not run. Each listing is one call.
func (c *cycleRun) watchConnectors(existing []reconcile.TunnelState, before, now []connector.Status) {
	for _, t := range existing {
		rollout := c.awaitsRollout(t)
		api := c.e.clients[t.CredentialID]
		if api == nil || !c.connectorsDue(t, rollout) {
			continue
		}
		c.e.asked[t.ID] = c.now
		conns, err := api.Connectors(c.ctx, t.AccountID, t.ID)
		if err != nil {
			c.e.d.Log.Debug().Err(err).Str("tunnel", t.Name).Msg("listing the connectors of a tunnel failed")
			continue
		}
		if rollout {
			c.confirmRollout(t, conns)
		}
		c.compareConnectors(t, conns, before, now)
	}
}

// connectorsDue says whether the connectors of a tunnel are listed in this
// cycle: whenever the accounts of its credential are listed, every
// zoneRefreshEvery, and besides at most every rolloutAskEvery while its
// configuration is not seen running yet or a connector pco does not run is
// shown on it.
func (c *cycleRun) connectorsDue(t reconcile.TunnelState, rollout bool) bool {
	if cz := c.e.zones.byCred[t.CredentialID]; cz != nil && cz.accountsOK && cz.accountsAt.Equal(c.now) {
		return true
	}
	if !rollout && !slices.ContainsFunc(c.st.RogueConnectors, func(r RogueConnector) bool { return r.TunnelID == t.ID }) {
		return false
	}
	last, asked := c.e.asked[t.ID]
	return !asked || c.now.Sub(last) >= rolloutAskEvery || c.now.Before(last)
}

// compareConnectors holds the connectors Cloudflare lists on a tunnel against
// the one pco runs for it on this node, as /ready names it now and named it in
// the cycle before: a connector that restarted is listed under its old id for
// a moment. Every other one is rogue, an error the first time it is seen.
// Without a connector of its own that is ready pco cannot tell, as one that
// just came up is listed before it says it is ready, and what was known of the
// tunnel stays.
func (c *cycleRun) compareConnectors(t reconcile.TunnelState, conns []cfapi.Connector, before, now []connector.Status) {
	own := readyIDs(t.ID, now)
	if len(own) == 0 {
		return
	}
	own = append(own, readyIDs(t.ID, before)...)
	known := make(map[string]RogueConnector)
	var kept []RogueConnector
	for _, r := range c.st.RogueConnectors {
		if r.TunnelID == t.ID {
			known[r.ID] = r
		} else {
			kept = append(kept, r)
		}
	}
	for _, x := range conns {
		if slices.Contains(own, x.ID) {
			continue
		}
		r := RogueConnector{Tunnel: t.Name, TunnelID: t.ID, Account: t.AccountID, ID: x.ID, OriginIP: x.OriginIP, Version: x.Version, Since: c.now}
		if prev, ok := known[x.ID]; ok {
			r.Since = prev.Since
			delete(known, x.ID)
		} else {
			c.events = append(c.events, rogueEvent(c.now, levelError, r,
				fmt.Sprintf("connector %s serves tunnel %s in account %s and is not one pco runs on this node", r.Text(), t.Name, t.AccountID)))
		}
		kept = append(kept, r)
	}
	for _, id := range slices.Sorted(maps.Keys(known)) {
		c.events = append(c.events, rogueEvent(c.now, levelInfo, known[id],
			fmt.Sprintf("connector %s no longer serves tunnel %s in account %s", id, t.Name, t.AccountID)))
	}
	slices.SortFunc(kept, compareRogues)
	c.st.RogueConnectors = kept
}

// readyIDs returns the id /ready gave the connector of a tunnel, when it was
// ready.
func readyIDs(tunnel string, statuses []connector.Status) []string {
	var out []string
	for _, st := range statuses {
		if st.TunnelID == tunnel && st.Ready && st.ConnectorID != "" {
			out = append(out, st.ConnectorID)
		}
	}
	return out
}

func rogueEvent(at time.Time, level string, r RogueConnector, msg string) Event {
	return Event{At: at, Level: level, Kind: kindConnector, Subject: r.ID, Tunnel: r.Tunnel, Account: r.Account, Message: msg}
}

// forgetRogues forgets the rogue connectors of the tunnels the cycle no
// longer shows: those of a tunnel that is gone went with it.
func (c *cycleRun) forgetRogues(shown []reconcile.TunnelState) {
	c.st.RogueConnectors = slices.DeleteFunc(slices.Clone(c.st.RogueConnectors), func(r RogueConnector) bool {
		return !slices.ContainsFunc(shown, func(t reconcile.TunnelState) bool { return t.ID == r.TunnelID })
	})
}

// noteRogueConnectors has a problem line for every rogue connector known, in
// every cycle, also one that did not get as far as listing them.
func (c *cycleRun) noteRogueConnectors() {
	for _, r := range c.st.RogueConnectors {
		c.problem("tunnel %s in account %s is served by connector %s, which pco does not run on this node: "+
			"it takes a share of the requests to every hostname of the tunnel; "+
			"unless you run it, rotate the tunnel secret with pco tunnel rotate --account %s", r.Tunnel, r.Account, r.Text(), r.Account)
	}
}
