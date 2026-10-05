package engine

import (
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// reconcileConnectors keeps a connector running for every tunnel the tunnel
// run found, and in enforce mode removes the connectors of tunnels Cloudflare
// shows gone. The tunnels the cycle leaves as they are, as those of frozen
// accounts, are looked up and shown in both modes, and the status of every
// connector is read, for the DNS run that follows; watchTunnels holds them
// against what Cloudflare lists.
func (c *cycleRun) reconcileConnectors() {
	var existing []reconcile.TunnelState
	for _, t := range c.tunnels {
		if t.Exists && t.ID != "" {
			existing = append(existing, t)
		}
	}
	others, failed := c.lookUpOthers()
	invisible := c.invisibleTunnels()
	if c.mode() == reconcile.Enforce {
		for _, t := range existing {
			c.ensure(t)
		}
		for _, t := range others {
			if c.zones.frozen[t.AccountID] {
				c.keepRunning(t)
			}
		}
		c.prune(append(slices.Clone(existing), others...), invisible, failed)
	}
	for _, t := range others {
		held := "serves no zone pco sees"
		if c.zones.frozen[t.AccountID] {
			held = "account frozen: " + c.zones.frozenWhy[t.AccountID]
		}
		c.st.Tunnels = append(c.st.Tunnels, TunnelView{TunnelState: t, Held: held})
	}
	for _, t := range invisible {
		c.st.Tunnels = append(c.st.Tunnels, TunnelView{TunnelState: t, Held: "not visible through any credential"})
	}
	shown := slices.Concat(existing, others, invisible)
	c.connected, c.existing, c.shown = true, existing, shown
	c.readStatuses(shown)
	c.noteForeignConnectors(shown)
}

// readStatuses reads the status of the connectors of the tunnels into the
// state, and keeps what the last cycle read.
func (c *cycleRun) readStatuses(shown []reconcile.TunnelState) {
	c.before = c.st.Connectors
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

// watchTunnels reads the status of the connectors and holds the connectors
// Cloudflare lists against them, in every cycle, held or not: what it asks
// changes nothing, and a hold, as one a sentinel written with a stolen token
// makes, must not hide a connector that pco does not run. A cycle that did
// not get to the connectors watches the tunnels the tunnel run found, or else
// those the last state showed: what it knows of them stays as it was.
func (c *cycleRun) watchTunnels() {
	if c.ctx.Err() != nil {
		return
	}
	existing, shown := c.existing, c.shown
	if !c.connected {
		existing, shown = c.knownTunnels()
		if len(shown) == 0 {
			return
		}
		c.readStatuses(shown)
	}
	statuses := c.st.Connectors
	c.noteOwnIDs(statuses)
	c.notePortsHeld(shown, statuses)
	c.followRefusals(existing, c.before, statuses)
	c.watchConnectors(existing, statuses)
	if c.connected {
		c.forgetRogues(shown)
	}
}

// knownTunnels are the tunnels a cycle that did not get to the connectors
// knows to exist: those the tunnel run found, or else those the last state
// showed with an id, unless that state left them as they are for a reason of
// their own, as a frozen account. shown are those whose connectors the state
// showed.
func (c *cycleRun) knownTunnels() (existing, shown []reconcile.TunnelState) {
	if c.tunnels != nil {
		for _, t := range c.tunnels {
			if t.Exists && t.ID != "" {
				existing = append(existing, t)
			}
		}
		return existing, existing
	}
	for _, v := range c.st.Tunnels {
		if v.ID == "" {
			continue
		}
		shown = append(shown, v.TunnelState)
		if v.Exists && !v.Unknown && (v.Held == "" || v.Unchecked) {
			existing = append(existing, v.TunnelState)
		}
	}
	return existing, shown
}

func (c *cycleRun) noteEnsured(id string) {
	if c.ensured == nil {
		c.ensured = make(map[string]bool)
	}
	c.ensured[id] = true
}

// notePortsHeld names the connectors that another process keeps from starting
// by holding their metrics port, as any local user can: the connector manager
// gives each another port at its next Ensure, which the line says when to
// expect.
func (c *cycleRun) notePortsHeld(shown []reconcile.TunnelState, statuses []connector.Status) {
	for _, t := range shown {
		i := slices.IndexFunc(statuses, func(s connector.Status) bool { return s.TunnelID == t.ID && s.MetricsPortHeld })
		if i < 0 {
			continue
		}
		_, port, _ := net.SplitHostPort(statuses[i].MetricsAddr)
		remedy := "the connector gets another port in the first cycle that keeps it running"
		switch {
		case c.ensured[t.ID]:
			remedy = "the connector gets another port in the next cycle"
		case c.install.ID != "" && c.mode() == reconcile.Observe:
			remedy = "pco gives the connector another port once it changes things again, after pco apply"
		}
		c.problem("tunnel %s in account %s: metrics port %s is held by another process, which keeps its connector from starting; %s",
			t.Name, t.AccountID, port, remedy)
	}
}

// noteForeignConnectors names the connectors on the node that are of another
// install, or of none, as one left from before the store was lost and set up
// anew: they may serve hostnames this install knows nothing of, so they are
// never pruned, and the admin has to decide.
func (c *cycleRun) noteForeignConnectors(shown []reconcile.TunnelState) {
	ids, err := c.e.d.Connectors.List(c.ctx)
	if err != nil {
		c.problem("listing the connectors on this node: %v", err)
	}
	for _, id := range ids {
		if slices.ContainsFunc(shown, func(t reconcile.TunnelState) bool { return t.ID == id }) {
			continue
		}
		st, err := c.e.d.Connectors.Status(c.ctx, id)
		switch {
		case err != nil:
			c.problem("connector for tunnel %s: reading its status: %v", id, err)
		case st.Install == "":
			c.problem("connector for tunnel %s names no install; pco setup --recover adopts the install it was made for, "+
				"pco uninstall on this node removes it", id)
		case st.Install != c.install.ID:
			c.problem("connector for tunnel %s belongs to install %s; pco setup --recover adopts that install, "+
				"pco uninstall on this node removes it", id, st.Install)
		}
	}
}

// lookUpOthers looks up, without changing anything, the tunnel of every
// account of the install that the tunnel run did not report on: the frozen
// ones, in every cycle, and those without a zone, which are looked up again
// only when the accounts were listed anew, every zoneRefreshEvery, or when
// their last lookup failed: there may be many of them, and they cost no call
// in a cycle that lists nothing. It returns the tunnels found and why a
// lookup failed.
func (c *cycleRun) lookUpOthers() (found []reconcile.TunnelState, failed []string) {
	reported := make(map[string]bool, len(c.tunnels))
	for _, t := range c.tunnels {
		reported[t.AccountID] = true
	}
	lookups := c.e.zones.lookups
	maps.DeleteFunc(lookups, func(account string, _ tunnelLookup) bool { return reported[account] })
	name := planner.TunnelName(c.install.ID)
	for _, account := range slices.Sorted(maps.Keys(c.zones.accounts)) {
		if reported[account] {
			continue
		}
		cred := c.zones.accounts[account]
		l, known := c.e.zones.lookup(account, cred, c.now)
		if !known || c.zones.frozen[account] {
			api := c.e.clients[cred]
			if api == nil {
				failed = append(failed, fmt.Sprintf("looking up the tunnel of account %s failed: no client for credential %s", account, cred))
				continue
			}
			t, ok, err := api.FindTunnel(c.ctx, account, name)
			if err != nil {
				delete(lookups, account)
				failed = append(failed, fmt.Sprintf("looking up the tunnel of account %s failed: %v", account, err))
				continue
			}
			l = tunnelLookup{credential: cred, at: c.now, found: ok, tunnel: t}
			lookups[account] = l
		}
		t := l.tunnel
		switch {
		case !l.found:
			c.forgetTunnelsOf(account)
		default:
			found = append(found, reconcile.TunnelState{AccountID: account, CredentialID: cred, Name: t.Name, ID: t.ID, Exists: true})
			if !c.zones.frozen[account] {
				c.problem("tunnel %s in account %s serves no zone pco sees; the tunnel and its connector are left as they are "+
					"until a credential lists a zone of the account or the tunnel is deleted at Cloudflare", t.Name, account)
			}
		}
	}
	return found, failed
}

// keepRunning starts the connector of a tunnel of a frozen account again,
// when it is stopped, with the token it has on disk; nothing is asked of
// Cloudflare about such a tunnel.
func (c *cycleRun) keepRunning(t reconcile.TunnelState) {
	token, found, err := c.e.d.Connectors.Token(t.ID)
	switch {
	case err != nil:
		c.problem("tunnel %s in account %s: reading the connector token: %v", t.Name, t.AccountID, err)
	case found:
		c.noteEnsured(t.ID)
		if err := c.e.d.Connectors.Ensure(c.ctx, c.install.ID, t.ID, token); err != nil {
			c.problem("tunnel %s in account %s: starting its connector: %s", t.Name, t.AccountID, redact(err.Error(), token))
		}
	}
}

// invisibleTunnels returns the tunnels seen before in accounts that no
// credential sees now. Nothing can be asked about them, so they are unknown;
// their connectors are kept, each with a problem line.
func (c *cycleRun) invisibleTunnels() []reconcile.TunnelState {
	var out []reconcile.TunnelState
	for _, id := range slices.Sorted(maps.Keys(c.e.seen)) {
		t := c.e.seen[id]
		if _, visible := c.zones.accounts[t.account]; visible {
			continue
		}
		out = append(out, reconcile.TunnelState{AccountID: t.account, CredentialID: t.credential, Name: t.name, ID: id, Unknown: true})
		c.offer.invisible = append(c.offer.invisible, unseenTunnel{id: id, name: t.name, account: t.account})
		line := fmt.Sprintf("tunnel %s in account %s is not visible through any credential; its connector is kept "+
			"until a credential sees the account again or pco apply --confirm-deletes confirms the tunnel is gone", t.name, t.account)
		c.offer.lines = append(c.offer.lines, line)
		c.problem("%s", line)
	}
	return out
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
func (c *cycleRun) prune(existing, invisible []reconcile.TunnelState, failed []string) {
	var why []string
	for _, t := range c.tunnels {
		switch {
		case t.Unknown:
			why = append(why, fmt.Sprintf("the tunnel of account %s is in an unknown state", t.AccountID))
		case !t.Exists:
			c.forgetTunnelsOf(t.AccountID)
		}
	}
	for _, id := range c.credIDs {
		cz := c.e.zones.byCred[id]
		switch {
		case cz == nil || !cz.accountsOK:
			err := "never tried"
			if cz != nil {
				err = cz.accountsErr
			}
			why = append(why, fmt.Sprintf("listing the accounts of credential %s failed: %s", id, err))
		case c.now.Sub(cz.accountsAt) > accountsFreshFor || c.now.Before(cz.accountsAt):
			why = append(why, fmt.Sprintf("the accounts of credential %s were last listed at %s, more than %s ago",
				id, cz.accountsAt.UTC().Format(time.RFC3339), accountsFreshFor))
		}
	}
	why = append(why, failed...)

	keep := make([]string, 0, len(existing))
	for _, t := range existing {
		keep = append(keep, t.ID)
		c.e.seen[t.ID] = seenTunnel{account: t.AccountID, name: t.Name, credential: t.CredentialID}
	}
	for _, t := range invisible {
		keep = append(keep, t.ID)
	}
	if len(why) > 0 {
		c.problem("connectors are not pruned in this cycle: %s", strings.Join(why, "; "))
		return
	}
	keep = slices.Compact(slices.Sorted(slices.Values(keep)))
	if err := c.e.d.Connectors.PruneInstall(c.ctx, c.install.ID, keep); err != nil {
		c.problem("removing the connectors of other tunnels: %v", err)
	}
}

// ensure starts the connector of a tunnel with the token it has on disk, or
// with the one Cloudflare hands out when it has none. The token is read again
// whenever the accounts of the tunnel's credential are listed, every
// zoneRefreshEvery, so that a secret rotated at Cloudflare reaches the
// connector. The connector manager is the only one that writes the token.
func (c *cycleRun) ensure(t reconcile.TunnelState) {
	token, found, err := c.e.d.Connectors.Token(t.ID)
	if err != nil {
		c.problem("tunnel %s in account %s: reading the connector token: %v", t.Name, t.AccountID, err)
		return
	}
	switch {
	case !found:
		fetched, err := c.fetchToken(t)
		if err != nil {
			c.problem("tunnel %s in account %s: fetching its token: %v", t.Name, t.AccountID, err)
			return
		}
		token = fetched
	case c.accountsListed(t.CredentialID):
		token = c.freshToken(t, token)
	}
	c.ensureWith(t, token)
}

func (c *cycleRun) ensureWith(t reconcile.TunnelState, token string) {
	c.noteEnsured(t.ID)
	if err := c.e.d.Connectors.Ensure(c.ctx, c.install.ID, t.ID, token); err != nil {
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

// accountsFreshFor is how old the account listing of a credential may be for
// a prune: a tunnel in an account found later would lose its connector.
const accountsFreshFor = 10 * time.Minute

// rolloutAskEvery is how often the connectors of a tunnel are listed while
// none reports the version written, or one that pco does not run is shown.
const rolloutAskEvery = 30 * time.Second

// awaitsRollout says whether the connectors of a tunnel are yet to be seen
// running the configuration written in enforce mode. Only a verified version
// counts: after a write that was held, failed or read back different, Version
// says nothing about what Cloudflare serves.
func (c *cycleRun) awaitsRollout(t reconcile.TunnelState) bool {
	return c.mode() == reconcile.Enforce && t.Verified && c.e.rolledOut[t.ID] != t.Version
}

// confirmRollout notes that the connectors of a tunnel run its configuration,
// once every one Cloudflare lists reports it.
func (c *cycleRun) confirmRollout(t reconcile.TunnelState, conns []cfapi.Connector) {
	if len(conns) == 0 || slices.ContainsFunc(conns, func(x cfapi.Connector) bool { return x.ConfigVersion < t.Version }) {
		return
	}
	c.e.rolledOut[t.ID] = t.Version
	c.events = append(c.events, Event{
		At: c.now, Level: levelInfo, Kind: kindRollout, Subject: t.Name, Tunnel: t.Name, Account: t.AccountID,
		Message: fmt.Sprintf("configuration version %d runs on %d connectors in account %s", t.Version, len(conns), t.AccountID),
	})
}
