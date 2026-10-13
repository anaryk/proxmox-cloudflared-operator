package agent

import (
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
)

// facts fills what the status says of the forwarding tables of this node:
// since when they are watched without a break, which ports of published
// guests they show, and the moves and conflicts they showed. An appliance
// agent reads no bridge and says nothing of them, and a node that stood down
// attests nothing.
func (a *Agent) facts(st *agentproto.Status) {
	if a.d.Kind != pki.KindHost {
		return
	}
	a.repMu.Lock()
	stood := a.rep.stood != ""
	a.repMu.Unlock()
	if stood {
		return
	}
	st.WatchRunningSince = a.watchRunningSince()
	st.Attestations, st.Observations = a.obs.report(a.d.Node, st.Clock)
}

// watchRunningSince is since when both watches of the node's tables have
// run without a break: the one that pins the addresses this node serves and
// the one that observes the forwarding tables of its bridges. It is zero
// while either is off: the controller counts the node as covered only on
// both, since a conflict is seen by the one and a move of a pinned address
// by the other.
func (a *Agent) watchRunningSince() time.Time {
	a.vouch.mu.Lock()
	pinned := a.vouch.watching
	a.vouch.mu.Unlock()
	a.obs.mu.Lock()
	observed := a.obs.since
	a.obs.mu.Unlock()
	switch {
	case pinned.IsZero() || observed.IsZero():
		return time.Time{}
	case observed.After(pinned):
		return observed
	}
	return pinned
}
