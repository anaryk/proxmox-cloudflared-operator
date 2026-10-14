package gateway

import (
	"context"
	"time"
)

// roleTimeout bounds the read of the state a page waits for when the state
// the gateway holds is not the daemon's current one.
const roleTimeout = 2 * time.Second

// Role is what the daemon says about the node, as the lease of its state
// shows it: the node is the controller while the state has no lease or names
// the node as the holder; else the holder's address, and ok while the daemon
// reports that holder's lease valid. The state the gateway holds is used
// while the stream says it is current; otherwise it is read again, and when
// the daemon does not answer nothing is known.
func (g *Gateway) Role() (controller bool, addr string, ok bool) {
	s := g.states.newest()
	if s == nil || s.digest != g.hub.digest() {
		ctx, cancel := context.WithTimeout(context.Background(), roleTimeout)
		defer cancel()
		fresh, err := g.current(ctx, "")
		if err != nil {
			g.log.Debug().Err(err).Msg("reading the state of the daemon for the role of the node")
			return false, "", false
		}
		s = fresh
	}
	st, err := s.state()
	if err != nil {
		return false, "", false
	}
	lease := st.Lease
	if lease == nil || lease.Holder == st.Node {
		return true, "", false
	}
	if lease.Address.IsValid() {
		addr = lease.Address.String()
	}
	return false, addr, lease.Holder != "" && addr != "" && lease.Valid
}
