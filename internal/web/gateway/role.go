package gateway

import (
	"context"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web"
)

// roleRefreshEvery is how often at most the state is read again for the
// role while the stream does not say that the state held is current.
const roleRefreshEvery = 2 * time.Second

// Role is what the daemon says about the node, as the lease of its state
// shows it: the node is the controller while the state has no lease or names
// the node as the holder; else the holder's address, and ok while one is
// known. Only the state the gateway holds is read, so a page never waits
// for the daemon: while the stream does not say that the state held is
// current, it is read again in the background, at most every
// roleRefreshEvery, and until something is held nothing is known.
//
// The address is the holder's agent address as the daemon's registry has it,
// and when the state names the holder among the nodes, the two must agree;
// an address a browser must not be sent to (web.Redirectable) is none.
func (g *Gateway) Role() (controller bool, addr string, ok bool) {
	s := g.states.newest()
	if s == nil || s.digest != g.hub.digest() {
		g.refreshRoleState()
	}
	if s == nil {
		return false, "", false
	}
	st, err := s.state()
	if err != nil {
		return false, "", false
	}
	lease := st.Lease
	if lease == nil || lease.Holder == st.Node {
		return true, "", false
	}
	if lease.Holder == "" || !web.Redirectable(lease.Address) {
		return false, "", false
	}
	for _, n := range st.Nodes {
		if n.Name == lease.Holder && n.AgentAddress != lease.Address {
			return false, "", false
		}
	}
	return false, lease.Address.String(), true
}

// refreshRoleState reads the state in the background, one read at a time
// and at most every roleRefreshEvery.
func (g *Gateway) refreshRoleState() {
	g.roleMu.Lock()
	now := g.now()
	if g.roleReading || now.Sub(g.roleAskedAt) < roleRefreshEvery {
		g.roleMu.Unlock()
		return
	}
	g.roleReading, g.roleAskedAt = true, now
	g.roleMu.Unlock()
	go func() {
		defer func() {
			g.roleMu.Lock()
			g.roleReading = false
			g.roleMu.Unlock()
		}()
		if _, err := g.current(context.Background(), ""); err != nil {
			g.log.Debug().Err(err).Msg("reading the state of the daemon for the role of the node")
		}
	}()
}
