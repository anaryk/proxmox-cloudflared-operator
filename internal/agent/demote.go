package agent

import (
	"fmt"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

// demotion is what the agent knows to stop its shared connector by when it
// serves nobody: a node whose origin network is gone while its Internet uplink
// works would otherwise keep taking a share of the requests.
type demotion struct {
	had     map[string]bool      // the targets this agent verified at some time, by key
	lastOK  map[string]time.Time // by shared tunnel: when one of its targets the agent had verified last verified
	stopped map[string]bool      // the shared tunnels whose connector the agent stopped
}

func newDemotion() demotion {
	return demotion{had: map[string]bool{}, lastOK: map[string]time.Time{}, stopped: map[string]bool{}}
}

// demoted reports whether the connector of the shared tunnel t stays stopped
// in this pass. The agent stops it gracefully when no controller answered for
// demoteSilence, none of the tunnel's targets it had verified verified for
// demoteFailing, and the last heartbeat counted another replica of the tunnel
// ready. Losing some of the targets never stops it. It starts again as soon
// as one of them verifies.
func (p *pass) demoted(t agentproto.Tunnel) bool {
	a := p.a
	d := &a.demote
	had, now := false, false
	for _, tg := range p.s.Targets {
		if !slices.Contains(tg.Tunnels, t.TunnelID) {
			continue
		}
		if p.verified[tg.Key] {
			d.had[tg.Key], now = true, true
		}
		had = had || d.had[tg.Key]
	}
	if now {
		d.lastOK[t.TunnelID] = p.now
	}
	if d.stopped[t.TunnelID] {
		if !now {
			return true
		}
		delete(d.stopped, t.TunnelID)
		a.eventOf(Event{At: p.now, Level: levelInfo, Kind: kindConnector, Subject: t.TunnelID, Tunnel: t.Name,
			Message: fmt.Sprintf("a target of tunnel %s verifies again; its connector starts", t.Name)})
		return false
	}
	switch {
	case !had, now:
		return false
	case a.silentFor(p.now) < demoteSilence, p.now.Sub(d.lastOK[t.TunnelID]) < demoteFailing, a.otherReady(t.TunnelID) < 1:
		return false
	}
	if err := a.d.Connectors.Stop(p.ctx, t.TunnelID); err != nil {
		p.problem("tunnel %s in account %s: stopping its connector, as none of its targets verifies: %v", t.Name, t.AccountID, err)
		return false
	}
	d.stopped[t.TunnelID] = true
	a.eventOf(Event{At: p.now, Level: levelWarn, Kind: kindConnector, Subject: t.TunnelID, Tunnel: t.Name,
		Message: fmt.Sprintf("no controller answered for %s and no target of tunnel %s verified for %s while another replica is ready; its connector is stopped",
			demoteSilence, t.Name, demoteFailing)})
	return true
}
