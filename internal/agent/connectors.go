package agent

import (
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
)

// connectors keeps a connector running for every tunnel the snapshot runs,
// with the run token it gives, or else the one on disk, as for a tunnel the
// controller may not ask Cloudflare about. A tunnel it keeps without running
// is left as it is. The boot gate and the self-demotion may hold the shared
// tunnels' back. Once a second node is registered, the shared units are not
// enabled at boot. Only a snapshot that says so removes the connectors of the
// install of the tunnels it does not list. The status of every connector on
// the node is read, also of those of another install, for the controller.
func (p *pass) connectors() {
	a := p.a
	gateHolds := p.gate()
	multi := a.multiNode()
	for _, t := range p.s.Tunnels {
		shared := t.Kind == agentproto.TunnelShared
		switch {
		case !t.Run:
			continue
		case shared && gateHolds:
			continue
		case shared && p.demoted(t):
			continue
		}
		if !a.wasActive[t.TunnelID] {
			// Ensure enables a unit it starts.
			delete(a.bootOff, t.TunnelID)
		}
		if !p.ensure(t) {
			continue
		}
		if shared && multi && !a.bootOff[t.TunnelID] {
			if err := a.d.Connectors.Disable(p.ctx, t.TunnelID); err != nil {
				p.problem("tunnel %s in account %s: taking its boot enable off: %v", t.Name, t.AccountID, err)
			} else {
				a.bootOff[t.TunnelID] = true
			}
		}
	}
	if p.s.Prune {
		keep := make([]string, 0, len(p.s.Tunnels))
		for _, t := range p.s.Tunnels {
			keep = append(keep, t.TunnelID)
		}
		keep = slices.Compact(slices.Sorted(slices.Values(keep)))
		if err := a.d.Connectors.PruneInstall(p.ctx, a.d.InstallID, keep); err != nil {
			p.problem("removing the connectors of other tunnels: %v", err)
		}
	}
	p.readStatuses()
}

// ensure starts the connector of t, or restarts it with what changed.
func (p *pass) ensure(t agentproto.Tunnel) bool {
	a := p.a
	token := t.Token.Reveal()
	if token == "" {
		disk, found, err := a.d.Connectors.Token(t.TunnelID)
		switch {
		case err != nil:
			p.problem("tunnel %s in account %s: reading the connector token: %v", t.Name, t.AccountID, err)
			return false
		case !found:
			p.problem("tunnel %s in account %s: no run token in the desired state and none on this node; its connector is not started", t.Name, t.AccountID)
			return false
		}
		token = disk
	}
	if err := a.d.Connectors.Ensure(p.ctx, a.d.InstallID, t.TunnelID, token); err != nil {
		p.problem("tunnel %s in account %s: starting its connector: %s", t.Name, t.AccountID, redact(err.Error(), token))
		return false
	}
	return true
}

// readStatuses reads the status of the connector of every tunnel of the
// snapshot, and of every other connector on the node.
func (p *pass) readStatuses() {
	a := p.a
	var ids []string
	for _, t := range p.s.Tunnels {
		if !slices.Contains(ids, t.TunnelID) {
			ids = append(ids, t.TunnelID)
		}
	}
	listed, err := a.d.Connectors.List(p.ctx)
	if err != nil {
		p.problem("listing the connectors on this node: %v", err)
	}
	for _, id := range listed {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	p.conns = make([]connector.Status, 0, len(ids))
	active, ready := map[string]bool{}, map[string]bool{}
	for _, id := range ids {
		st, err := a.d.Connectors.Status(p.ctx, id)
		if err != nil {
			p.problem("connector of tunnel %s: reading its status: %v", id, err)
			continue
		}
		p.conns = append(p.conns, st)
		active[id], ready[id] = st.Active, st.Active && st.Ready
	}
	a.wasActive, a.ownReady = active, ready
}

// redact blanks a token out of a message, in case an error repeats it.
func redact(msg, token string) string {
	if strings.TrimSpace(token) == "" {
		return msg
	}
	return strings.ReplaceAll(msg, token, "[redacted]")
}
