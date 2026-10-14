package gateway

import (
	"context"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web"
)

const (
	// roleRefreshEvery is how long a state read for the role is taken as
	// current while the stream cannot say so, and how long a read that
	// failed is not tried again.
	roleRefreshEvery = 2 * time.Second
	// roleReadTimeout is the longest a page waits for the state to be read
	// for the role; without an answer by then it is served here.
	roleReadTimeout = 500 * time.Millisecond
)

// Role is what the daemon says about the node, as the lease of its state
// shows it, the state the API answers from: the node is the controller
// while the state has no lease or names the node as the holder; else the
// holder's address, and ok while one is known. The state held is used while
// it is known to be the daemon's now: the stream names it, or it was read a
// moment ago. Else it is read before the page is answered, so that a page is
// neither sent to a controller that is gone nor kept from the one that took
// over: a read waits roleReadTimeout at most, one at a time, and until a
// state is read nothing is known, and the page is served here.
//
// The address is the holder's agent address as the daemon's registry has it,
// and when the state names the holder among the nodes, the two must agree;
// an address a browser must not be sent to (web.Redirectable) is none.
func (g *Gateway) Role() (controller bool, addr string, ok bool) {
	s := g.roleState()
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

// roleState is the state the role is read from: the one held while it is
// current, else the daemon's read now; nil when none can be had.
func (g *Gateway) roleState() *snapshot {
	g.roleMu.Lock()
	defer g.roleMu.Unlock()
	now := g.now()
	held, streamed := g.states.newest(), g.hub.status().Up
	switch {
	case held != nil && streamed && held.digest != "" && held.digest == g.hub.digest():
		return held
	case held != nil && !streamed && held == g.roleRead && now.Sub(g.roleReadAt) < roleRefreshEvery:
		return held
	case !g.roleFailedAt.IsZero() && now.Sub(g.roleFailedAt) < roleRefreshEvery:
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), roleReadTimeout)
	defer cancel()
	s, err := g.current(ctx, "")
	if err != nil {
		g.roleFailedAt = now
		g.log.Debug().Err(err).Msg("reading the state of the daemon for the role of the node")
		return nil
	}
	g.roleRead, g.roleReadAt, g.roleFailedAt = s, now, time.Time{}
	return s
}
