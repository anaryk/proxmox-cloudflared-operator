package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// minMaxGap is the least MaxGap the DNS reconciler gets; with a long poll
// interval it is six intervals.
const minMaxGap = 2 * time.Minute

// rolloutAskEvery is how often the connectors of a tunnel are asked for the
// version they run while none reports the one written.
const rolloutAskEvery = 30 * time.Second

func (c *cycleRun) mode() reconcile.Mode {
	if c.settings.ObserveOnly {
		return reconcile.Observe
	}
	return reconcile.Enforce
}

// reconcileTunnels brings the tunnel of every planned account in line and
// reports whether the cycle may go on: a stale or foreign writer stops it. A
// frozen account is left out: its tunnel is not touched.
func (c *cycleRun) reconcileTunnels() bool {
	plans := slices.DeleteFunc(slices.Clone(c.plan.Tunnels), func(p planner.TunnelPlan) bool { return c.zones.frozen[p.AccountID] })
	res := c.e.tunnels.Run(c.ctx, plans, c.zones.known, c.mode())
	c.tunnelVerdict = res.Verdict
	c.st.Tunnels = res.Tunnels
	c.st.Actions = append(c.st.Actions, res.Actions...)
	c.st.Problems = append(c.st.Problems, res.Problems...)
	c.st.WriterVerdict = verdictName(res.Verdict)
	return res.Verdict == reconcile.WriterProceed && c.writerStill("after the tunnel run")
}

// reconcileConnectors keeps a connector running for every tunnel the tunnel
// run found, and in enforce mode removes the connectors of tunnels Cloudflare
// shows gone. The status of the connectors is read in both modes.
func (c *cycleRun) reconcileConnectors() {
	var existing []reconcile.TunnelState
	for _, t := range c.st.Tunnels {
		if t.Exists && t.ID != "" {
			existing = append(existing, t)
		}
	}
	shown := existing
	if c.mode() == reconcile.Enforce {
		for _, t := range existing {
			c.ensure(t)
		}
		others, failed := c.lookUpOthers()
		c.prune(append(slices.Clone(existing), others...), failed)
		c.confirmRollouts(existing)
		shown = append(slices.Clone(existing), others...)
	}
	statuses := make([]connector.Status, 0, len(shown))
	for _, t := range shown {
		st, err := c.e.d.Connectors.Status(c.ctx, t.ID)
		if err != nil {
			c.problem("tunnel %s in account %s: reading the connector status: %v", t.Name, t.AccountID, err)
			continue
		}
		statuses = append(statuses, st)
	}
	c.st.Connectors = statuses
}

// lookUpOthers looks up, without changing anything, the tunnel of every
// account of the install that the tunnel run did not report on: the frozen
// ones and those without a zone. It returns the tunnels found and why a
// lookup failed.
func (c *cycleRun) lookUpOthers() (found []reconcile.TunnelState, failed []string) {
	reported := make(map[string]bool, len(c.st.Tunnels))
	for _, t := range c.st.Tunnels {
		reported[t.AccountID] = true
	}
	name := planner.TunnelName(c.install.ID)
	for _, account := range slices.Sorted(maps.Keys(c.zones.accounts)) {
		if reported[account] {
			continue
		}
		cred := c.zones.accounts[account]
		api := c.e.clients[cred]
		if api == nil {
			failed = append(failed, fmt.Sprintf("looking up the tunnel of account %s failed: no client for credential %s", account, cred))
			continue
		}
		t, ok, err := api.FindTunnel(c.ctx, account, name)
		switch {
		case err != nil:
			failed = append(failed, fmt.Sprintf("looking up the tunnel of account %s failed: %v", account, err))
		case !ok:
			c.forgetTunnelsOf(account)
		default:
			found = append(found, reconcile.TunnelState{AccountID: account, CredentialID: cred, Name: t.Name, ID: t.ID, Exists: true})
			if !c.zones.frozen[account] {
				c.problem("tunnel %s in account %s serves no zone pco sees; the tunnel and its connector are left as they are", t.Name, account)
			}
		}
	}
	return found, failed
}

// forgetTunnelsOf forgets the tunnels seen in an account whose tunnel
// Cloudflare shows absent.
func (c *cycleRun) forgetTunnelsOf(account string) {
	maps.DeleteFunc(c.e.seen, func(_ string, t seenTunnel) bool { return t.account == account })
}

// prune removes the connectors of the tunnels that are gone. The engine keeps
// the connector of every tunnel it has no proof is gone, so it prunes only
// when every account the credentials see was looked at and answered: no
// tunnel is in an unknown state, the account listing of every credential
// worked in this cycle and every lookup answered. A tunnel seen before in an
// account that no credential sees any more is kept.
func (c *cycleRun) prune(existing []reconcile.TunnelState, failed []string) {
	var why []string
	for _, t := range c.st.Tunnels {
		switch {
		case t.Unknown:
			why = append(why, fmt.Sprintf("the tunnel of account %s is in an unknown state", t.AccountID))
		case !t.Exists:
			c.forgetTunnelsOf(t.AccountID)
		}
	}
	for _, id := range c.credIDs {
		if cz := c.e.zones.byCred[id]; cz == nil || !cz.accountsOK {
			err := "never tried"
			if cz != nil {
				err = cz.accountsErr
			}
			why = append(why, fmt.Sprintf("listing the accounts of credential %s failed: %s", id, err))
		}
	}
	why = append(why, failed...)

	keep := make([]string, 0, len(existing))
	for _, t := range existing {
		keep = append(keep, t.ID)
		c.e.seen[t.ID] = seenTunnel{account: t.AccountID, name: t.Name}
	}
	for _, id := range slices.Sorted(maps.Keys(c.e.seen)) {
		t := c.e.seen[id]
		if _, visible := c.zones.accounts[t.account]; !visible {
			keep = append(keep, id)
			c.problem("tunnel %s in account %s is not visible through any credential; its connector is kept", t.name, t.account)
		}
	}
	if len(why) > 0 {
		c.problem("connectors are not pruned in this cycle: %s", strings.Join(why, "; "))
		return
	}
	keep = slices.Compact(slices.Sorted(slices.Values(keep)))
	if err := c.e.d.Connectors.Prune(c.ctx, keep); err != nil {
		c.problem("removing the connectors of other tunnels: %v", err)
	}
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
			c.problem("tunnel %s in account %s: fetching its token: %v", t.Name, t.AccountID, err)
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
		last, asked := c.e.asked[t.ID]
		if api == nil || asked && c.now.Sub(last) < rolloutAskEvery && !c.now.Before(last) {
			continue
		}
		c.e.asked[t.ID] = c.now
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

// reconcileDNS brings the records in line. The admin's one-shot requests,
// confirmed deletes and adoptions, go only to an enforcing run and stay
// pending until a run decided on them; an adoption also waits until its
// tunnel can serve the name.
func (c *cycleRun) reconcileDNS() {
	mode := c.mode()
	in := reconcile.DNSInput{
		Records: slices.DeleteFunc(slices.Clone(c.plan.Records), func(rp planner.RecordPlan) bool {
			return c.zones.frozen[rp.AccountID]
		}),
		Zones:         c.zones.dns,
		Tunnels:       c.st.Tunnels,
		TunnelVerdict: c.tunnelVerdict,
		Keep:          c.kept(),
		InventoryOK:   c.snap.Complete && !c.col.PolicyInvalid,
		StillUnwanted: c.stillUnwanted,
		BeforeReplace: c.beforeReplace,
	}
	if mode == reconcile.Enforce {
		in.ConfirmDeletes = c.e.confirm != nil
		var ready []string
		ready, c.adoptWaits = c.adoptable(c.publishedThrough(in.Records))
		in.Adopt = make(map[string]bool, len(ready))
		for _, name := range ready {
			in.Adopt[name] = true
		}
	}
	res := c.e.dnsReconciler(c.dnsSettings()).Run(c.ctx, in, mode)
	c.settleRequests(in, res, mode)
	c.st.Actions = append(c.st.Actions, res.Actions...)
	c.st.Problems = append(c.st.Problems, res.Problems...)
	if res.Decided && res.Verdict == reconcile.WriterProceed {
		c.st.Conflicts, c.st.Lost = c.lookedAt(res)
	}
	if res.Verdict != reconcile.WriterProceed {
		c.st.WriterVerdict = verdictName(res.Verdict)
		return
	}
	c.writerStill("after the DNS run")
}

// publishedThrough maps every hostname with a record plan to the state of the
// tunnel its record points at.
func (c *cycleRun) publishedThrough(records []planner.RecordPlan) map[string]reconcile.TunnelState {
	out := make(map[string]reconcile.TunnelState, len(records))
	for _, rp := range records {
		for _, t := range c.st.Tunnels {
			if t.AccountID == rp.AccountID && t.Name == rp.TunnelName {
				out[strings.ToLower(rp.Name)] = t
			}
		}
	}
	return out
}

// lookedAt is what a DNS run that decided found in conflict or lost. The
// zones it did not manage in this cycle, as those of a frozen account, keep
// what was found there before.
func (c *cycleRun) lookedAt(res reconcile.DNSResult) ([]reconcile.Conflict, []string) {
	managed := make(map[string]bool, len(c.zones.dns))
	for _, z := range c.zones.dns {
		managed[z.Name] = true
	}
	conflicts := slices.Clone(res.Conflicts)
	for _, old := range c.st.Conflicts {
		if !managed[old.Zone] {
			conflicts = append(conflicts, old)
		}
	}
	slices.SortFunc(conflicts, func(a, b reconcile.Conflict) int {
		return cmp.Or(
			cmp.Compare(a.Zone, b.Zone),
			cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)),
			cmp.Compare(a.Type, b.Type),
			cmp.Compare(a.Content, b.Content),
		)
	})
	lost := slices.Clone(res.Lost)
	for _, name := range c.st.Lost {
		if c.zones.zoneOf(name) == "" {
			lost = append(lost, name)
		}
	}
	return slices.Compact(conflicts), lost
}

// beforeReplace keeps a copy of the record an adoption is about to change or
// delete, in the adopted log; the adoption goes ahead only once it is kept.
func (c *cycleRun) beforeReplace(_ context.Context, zone reconcile.ZoneRef, rec cfapi.Record) error {
	return c.e.d.Store.AppendAdopted(c.now, zone.Name, rec)
}

// writerStill reads leader.json again after a reconciler run that let this
// writer proceed: a run that could not read it, or found it changed, may have
// stopped writing without saying so in its verdict. It reports whether the
// cycle may go on.
func (c *cycleRun) writerStill(when string) bool {
	us, stored, err := c.e.writer()
	switch {
	case err != nil:
		c.st.WriterVerdict = verdictUnknown
		c.problem("the writer identity cannot be read %s (%v); the rest is left as it is", when, err)
		return false
	case stored.Generation != us.Generation || stored.Nonce != us.Nonce:
		c.st.WriterVerdict = verdictStale
		c.problem("leader.json names another writer %s; the rest is left as it is", when)
		return false
	}
	return true
}

// kept are the hostnames claimed or published in this cycle that have no
// record plan, as their rule answers 503: their records are left alone.
func (c *cycleRun) kept() map[string]bool {
	keep := make(map[string]bool, len(c.claims.Claims))
	for host := range c.claims.Claims {
		keep[host] = true
	}
	for _, st := range c.plan.Routes {
		keep[st.Hostname] = true
	}
	for _, rp := range c.plan.Records {
		delete(keep, rp.Name)
	}
	return keep
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
	ctx, cancel := c.e.timeout(ctx, refreshTimeout)
	defer cancel()
	snap := c.e.d.Inventory.Refresh(ctx)
	switch {
	case !snap.Complete:
		return nil, fmt.Errorf("the inventory is incomplete: %s", strings.Join(snap.Problems, "; "))
	case len(snap.Guests) == 0 && len(c.snap.Guests) > 0:
		// An empty answer is more likely a failure than every guest gone.
		return nil, errors.New("the inventory lists no guest any more")
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
