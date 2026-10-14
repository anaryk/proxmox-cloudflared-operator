package gateway

import (
	"context"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
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
// moment ago and the stream has announced nothing since. Else it is read
// before the page is answered, so that a page is neither sent to a
// controller that is gone nor kept from the one that took over. One read
// runs at a time, and the pages that come meanwhile wait for it, each
// roleReadTimeout at most; until a state is read nothing is known, and the
// page is served here.
//
// Only a holder the state lists among the nodes, with the same agent
// address as the lease, is redirected to; an address a browser must not be
// sent to (web.Redirectable) is none.
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
	i := slices.IndexFunc(st.Nodes, func(n engine.NodeView) bool { return n.Name == lease.Holder })
	if i < 0 || st.Nodes[i].AgentAddress != lease.Address {
		return false, "", false
	}
	return false, lease.Address.String(), true
}

// roleRead is a read of the state for the role; done is closed once s or
// err is set.
type roleRead struct {
	done chan struct{}
	s    *snapshot
	err  error
}

// roleState is the state the role is read from: the one held while it is
// current, else the one the running read gives, a read started for it when
// none runs; nil when none can be had within roleReadTimeout.
func (g *Gateway) roleState() *snapshot {
	g.roleMu.Lock()
	now := g.now()
	held, streamed, announced := g.states.newest(), g.hub.status().Up, g.hub.digest()
	switch {
	case held != nil && streamed && held.digest != "" && held.digest == announced:
		g.roleMu.Unlock()
		return held
	case held != nil && held == g.roleLast && now.Sub(g.roleLastAt) < roleRefreshEvery && (!streamed || g.roleLastFor == announced):
		g.roleMu.Unlock()
		return held
	case !g.roleFailedAt.IsZero() && now.Sub(g.roleFailedAt) < roleRefreshEvery:
		g.roleMu.Unlock()
		return nil
	}
	r := g.roleReading
	if r == nil {
		r = &roleRead{done: make(chan struct{})}
		g.roleReading = r
		go g.readRole(r, announced)
	}
	g.roleMu.Unlock()

	wait := time.NewTimer(roleReadTimeout)
	defer wait.Stop()
	select {
	case <-r.done:
		if r.err != nil {
			return nil
		}
		return r.s
	case <-wait.C:
		return nil
	}
}

// readRole reads the state for r, and keeps what it gave: a state for a
// while, under the digest the stream had announced as the read began, or
// that the read failed.
func (g *Gateway) readRole(r *roleRead, announced string) {
	ctx, cancel := context.WithTimeout(context.Background(), roleReadTimeout)
	defer cancel()
	s, err := g.current(ctx, "")
	g.roleMu.Lock()
	defer g.roleMu.Unlock()
	now := g.now()
	if err != nil {
		g.roleFailedAt = now
		g.log.Debug().Err(err).Msg("reading the state of the daemon for the role of the node")
	} else {
		g.roleLast, g.roleLastAt, g.roleLastFor, g.roleFailedAt = s, now, announced, time.Time{}
	}
	g.roleReading = nil
	r.s, r.err = s, err
	close(r.done)
}
