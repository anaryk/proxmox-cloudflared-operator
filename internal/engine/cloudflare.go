package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// minMaxGap is the least MaxGap the DNS reconciler gets; with a long poll
// interval it is six intervals.
const minMaxGap = 2 * time.Minute

func (c *cycleRun) mode() reconcile.Mode {
	if c.settings.ObserveOnly {
		return reconcile.Observe
	}
	return reconcile.Enforce
}

// reconcileTunnels brings the tunnel of every planned account in line and
// reports whether the cycle may go on: a stale or foreign writer stops it.
func (c *cycleRun) reconcileTunnels() bool {
	res := c.e.tunnels.Run(c.ctx, c.plan.Tunnels, c.zones.known, c.mode())
	c.st.Tunnels = res.Tunnels
	c.st.Actions = append(c.st.Actions, res.Actions...)
	c.st.Problems = append(c.st.Problems, res.Problems...)
	c.st.WriterVerdict = verdictName(res.Verdict)
	return res.Verdict == reconcile.WriterProceed
}

// reconcileConnectors keeps a connector running for every tunnel of this
// install that exists, and in enforce mode removes the others when the cycle
// knows every tunnel. The status of the connectors is read in both modes.
func (c *cycleRun) reconcileConnectors() {
	var existing []reconcile.TunnelState
	unknown := false
	for _, t := range c.st.Tunnels {
		unknown = unknown || t.Unknown
		if t.Exists && t.ID != "" {
			existing = append(existing, t)
		}
	}
	if c.mode() == reconcile.Enforce {
		for _, t := range existing {
			c.ensure(t)
		}
		c.prune(existing, unknown)
		c.confirmRollouts(existing)
	}
	statuses := make([]connector.Status, 0, len(existing))
	for _, t := range existing {
		st, err := c.e.d.Connectors.Status(c.ctx, t.ID)
		if err != nil {
			c.problem("tunnel %s in account %s: reading the connector status: %v", t.Name, t.AccountID, err)
			continue
		}
		statuses = append(statuses, st)
	}
	c.st.Connectors = statuses
}

// ensure starts the connector of a tunnel with the token it has on disk, or
// with the one Cloudflare hands out when it has none. The connector manager
// is the only one that writes the token.
func (c *cycleRun) ensure(t reconcile.TunnelState) {
	token, found, err := c.e.d.Connectors.Token(t.ID)
	if err != nil {
		c.problem("tunnel %s in account %s: reading the connector token: %v", t.Name, t.AccountID, err)
		return
	}
	if !found {
		api := c.e.clients[t.CredentialID]
		if api == nil {
			c.problem("tunnel %s in account %s: no client for credential %s to fetch its token", t.Name, t.AccountID, t.CredentialID)
			return
		}
		token, err = api.TunnelToken(c.ctx, t.AccountID, t.ID)
		switch {
		case err != nil:
			c.problem("tunnel %s in account %s: fetching its token: %s", t.Name, t.AccountID, redact(err.Error(), token))
			return
		case strings.TrimSpace(token) == "":
			c.problem("tunnel %s in account %s: Cloudflare returned an empty token", t.Name, t.AccountID)
			return
		}
	}
	if err := c.e.d.Connectors.Ensure(c.ctx, t.ID, token); err != nil {
		c.problem("tunnel %s in account %s: starting its connector: %s", t.Name, t.AccountID, redact(err.Error(), token))
	}
}

// redact blanks a token out of a message, in case an error repeats it.
func redact(msg, token string) string {
	if strings.TrimSpace(token) == "" {
		return msg
	}
	return strings.ReplaceAll(msg, token, "[redacted]")
}

// prune removes the connectors of tunnels that are not ours any more. It runs
// only when the tunnel run looked at the tunnel of every account the zones
// name and found each one or found it absent: Cloudflare held nothing back and
// the writer let the run proceed.
func (c *cycleRun) prune(existing []reconcile.TunnelState, unknown bool) {
	if unknown {
		c.e.d.Log.Debug().Msg("not pruning connectors: a tunnel is in an unknown state")
		return
	}
	keep := make([]string, 0, len(existing))
	for _, t := range existing {
		keep = append(keep, t.ID)
	}
	slices.Sort(keep)
	if err := c.e.d.Connectors.Prune(c.ctx, keep); err != nil {
		c.problem("removing the connectors of other tunnels: %v", err)
	}
}

// confirmRollouts checks, for each tunnel whose configuration was read back
// equal to the plan, whether its connectors run that version. Only a verified
// version counts: after a write that was held, failed or read back different,
// Version says nothing about what Cloudflare serves.
func (c *cycleRun) confirmRollouts(existing []reconcile.TunnelState) {
	for _, t := range existing {
		if !t.Verified || c.e.rolledOut[t.ID] == t.Version {
			continue
		}
		api := c.e.clients[t.CredentialID]
		if api == nil {
			continue
		}
		conns, err := api.Connectors(c.ctx, t.AccountID, t.ID)
		if err != nil {
			c.e.d.Log.Debug().Err(err).Str("tunnel", t.Name).Msg("listing the connectors of a tunnel failed")
			continue
		}
		if len(conns) == 0 || slices.ContainsFunc(conns, func(x cfapi.Connector) bool { return x.ConfigVersion < t.Version }) {
			continue
		}
		c.e.rolledOut[t.ID] = t.Version
		c.events = append(c.events, Event{
			At: c.now, Level: levelInfo, Kind: kindRollout, Subject: t.Name,
			Message: fmt.Sprintf("configuration version %d runs on %d connectors in account %s", t.Version, len(conns), t.AccountID),
		})
	}
}

// reconcileDNS brings the records in line. The one-shot requests of the
// admin, confirmed deletes and adoptions, go to the first enforcing run and
// are cleared after it, whatever its outcome.
func (c *cycleRun) reconcileDNS() {
	mode := c.mode()
	in := reconcile.DNSInput{
		Records:       c.plan.Records,
		Zones:         c.zones.dns,
		Tunnels:       c.st.Tunnels,
		InventoryOK:   c.snap.Complete && !c.col.PolicyInvalid,
		StillUnwanted: c.stillUnwanted,
	}
	if mode == reconcile.Enforce {
		in.ConfirmDeletes = c.e.confirmDeletes
		in.Adopt = maps.Clone(c.e.adopt)
	}
	res := c.e.dnsReconciler(c.dnsSettings()).Run(c.ctx, in, mode)
	if mode == reconcile.Enforce {
		c.e.confirmDeletes = false
		clear(c.e.adopt)
	}
	c.st.Actions = append(c.st.Actions, res.Actions...)
	c.st.Conflicts = res.Conflicts
	c.st.Lost = res.Lost
	c.st.Problems = append(c.st.Problems, res.Problems...)
	if res.Verdict != reconcile.WriterProceed {
		c.st.WriterVerdict = verdictName(res.Verdict)
	}
	c.logReplaced(res.Replaced)
}

func (c *cycleRun) dnsSettings() reconcile.DNSSettings {
	return reconcile.DNSSettings{
		InstallID: c.install.ID,
		Grace:     time.Duration(c.settings.Grace),
		MaxGap:    max(minMaxGap, 6*time.Duration(c.settings.PollInterval)),
	}
}

// dnsReconciler returns the DNS reconciler, made anew only when its settings
// change: it remembers the names it saw wanted between runs.
func (e *Engine) dnsReconciler(s reconcile.DNSSettings) *reconcile.DNSReconciler {
	if e.dns == nil || e.dnsSet != s {
		if e.dns != nil {
			e.d.Log.Info().Dur("grace", s.Grace).Dur("maxGap", s.MaxGap).Msg("dns settings changed")
		}
		e.dns = reconcile.NewDNSReconciler(e.clients, e.d.Store.Tombstones(), e.writer, s, e.d.Now, e.d.Log)
		e.dnsSet = s
	}
	return e.dns
}

// logReplaced appends every record an adoption changed or replaced to the
// adopted log, so that it can be put back by hand.
func (c *cycleRun) logReplaced(records []cfapi.Record) {
	for _, rec := range records {
		zone := c.zones.zoneOf(rec.Name)
		if err := c.e.d.Store.AppendAdopted(c.now, zone, rec); err != nil {
			c.problem("recording the replaced record %s %s %s (ttl %d) of zone %s: %v",
				rec.Type, rec.Name, rec.Content, rec.TTL, zone, err)
		}
	}
}

// recheck is the second look at the inventory that a cycle takes before its
// first DNS delete.
type recheck struct {
	done  bool
	names map[string]bool // lower-case hostnames a route or a held name still carries
	err   error
}

// stillUnwanted answers the DNS reconciler right before a delete: true only
// when a fresh, complete snapshot under a valid policy has no route and no
// held name for the hostname. The inventory is asked once per cycle.
func (c *cycleRun) stillUnwanted(ctx context.Context, name string) (bool, error) {
	r := &c.recheck
	if !r.done {
		r.done = true
		r.names, r.err = c.wantedNow(ctx)
	}
	if r.err != nil {
		return false, r.err
	}
	return !r.names[strings.ToLower(strings.TrimSuffix(name, "."))], nil
}

func (c *cycleRun) wantedNow(ctx context.Context) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	snap := c.e.d.Inventory.Refresh(ctx)
	if !snap.Complete {
		return nil, fmt.Errorf("the inventory is incomplete: %s", strings.Join(snap.Problems, "; "))
	}
	col := c.collectFrom(snap)
	if col.PolicyInvalid {
		return nil, fmt.Errorf("the settings contain an invalid allow or deny pattern")
	}
	names := make(map[string]bool, len(col.Routes)+len(col.Held))
	for _, rt := range col.Routes {
		names[strings.ToLower(rt.Hostname)] = true
	}
	for _, h := range col.Held {
		names[strings.ToLower(h.Hostname)] = true
	}
	return names, nil
}
