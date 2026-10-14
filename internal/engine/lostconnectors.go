package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// lostView is what Cloudflare lists of a tunnel's connectors, told apart by
// who may run them.
type lostView struct {
	listed int
	// lost are the connectors the nodes that are gone may run, and waiting
	// those no node owns yet that started with the new secret.
	lost    []string
	waiting []string
	// unsure says that a node still in the install that runs a connector of
	// the tunnel reported none: a connector it runs looks like a lost one.
	unsure bool
}

// ownGrace is how long a connector that started after the secret was given
// may go without a node owning it: a replica that restarted with the new
// token reports its id within it.
const ownGrace = 2 * time.Minute

// errNotHeard says that this process has not yet heard the connectors the
// nodes still in the install run, as a controller that just took over: a
// connector of theirs would look lost.
var errNotHeard = errors.New("this controller has not heard the connectors the remaining nodes run yet; " +
	"nothing is dropped until it has")

// lostConnectors lists the connectors of a tunnel the nodes that are gone
// may run: those of ids, and every other one no node still in the install
// owns. One Cloudflare says started after since, when the secret was given,
// waits for ownGrace for a node to report it, as a replica that restarted
// with the new token does; it is lost after that all the same.
func (e *Engine) lostConnectors(ctx context.Context, t reconcile.TunnelState, ids []string, since time.Time) (lostView, error) {
	api := e.clients[t.CredentialID]
	if api == nil {
		return lostView{}, fmt.Errorf("%w: no client for credential %s", ErrRefused, t.CredentialID)
	}
	listed, err := api.Connectors(ctx, t.AccountID, t.ID)
	if err != nil {
		return lostView{}, err
	}
	owned, sure, err := e.ownedConnectors(t)
	if err != nil {
		return lostView{}, err
	}
	v := lostView{listed: len(listed), unsure: !sure}
	for _, c := range listed {
		switch {
		case slices.Contains(ids, c.ID):
		case owned[c.ID]:
			continue
		case !since.IsZero() && c.RunAt.After(since) && e.d.Now().Before(c.RunAt.Add(ownGrace)):
			v.waiting = append(v.waiting, c.ID)
			continue
		}
		v.lost = append(v.lost, c.ID)
	}
	return v, nil
}

// dropLost ends the connections of the connectors of a tunnel the nodes that
// are gone may run, as lostConnectors finds them. When that cannot be done
// connector by connector, because the listing failed or shows nothing, a
// node still in the install cannot be told from the ones that are gone, or a
// connector cannot be dropped, the connections of every connector of the
// tunnel are ended: a session of a gone node must not outlive the rotation,
// and the replicas connect again with the new token.
func (e *Engine) dropLost(ctx context.Context, t reconcile.TunnelState, ids []string, since time.Time) error {
	api := e.clients[t.CredentialID]
	if api == nil {
		return fmt.Errorf("%w: no client for credential %s", ErrRefused, t.CredentialID)
	}
	v, err := e.lostConnectors(ctx, t, ids, since)
	switch {
	case errors.Is(err, errNotHeard):
		return err
	case err != nil:
	case v.listed == 0:
		// A listing may lag a connection: nothing in it tells that the
		// nodes keep none.
		err = errors.New("no connector is listed to tell the nodes' from")
	case v.unsure && len(v.lost) > 0:
		err = errors.New("a node still in the install reported no connector, so its own cannot be told from the lost ones")
	default:
		for _, id := range v.lost {
			if err = api.CleanUpConnector(ctx, t.AccountID, t.ID, id); err != nil {
				err = fmt.Errorf("connector %s: %w", id, err)
				break
			}
		}
	}
	if err == nil {
		return nil
	}
	if cleanErr := api.CleanUpConnections(ctx, t.AccountID, t.ID); cleanErr != nil {
		return fmt.Errorf("the secret of tunnel %s in account %s was rotated, but the connectors of the nodes that are gone could not be dropped: %w; "+
			"nor could every connection of the tunnel be ended: %w", t.Name, t.AccountID, err, cleanErr)
	}
	return nil
}

// ownedConnectors are the ids of the connectors of a tunnel that this node
// runs, as the last state or this cycle shows them, or that the agent of a
// node still served reported as its own, and whether every node that runs
// one was heard of: the replicas for the shared tunnel, the node of a
// pinned one.
// Until this process heard every such node, or takeoverHold passed since
// it first could, it says errNotHeard: a node it did not hear yet is not
// one that reported nothing.
func (e *Engine) ownedConnectors(t reconcile.TunnelState) (map[string]bool, bool, error) {
	mem := &e.place
	if mem.heardSince.IsZero() {
		return nil, false, errNotHeard
	}
	waits := e.d.Now().Sub(mem.heardSince) < takeoverHold
	nodes, err := e.d.Store.Nodes()
	if err != nil {
		return nil, false, fmt.Errorf("reading the node registry: %w", err)
	}
	s, err := e.d.Store.Settings()
	if err != nil {
		return nil, false, fmt.Errorf("reading the settings: %w", err)
	}
	owned := map[string]bool{}
	sure := true
	for _, st := range e.State().Connectors {
		if st.TunnelID == t.ID && st.ConnectorID != "" {
			owned[st.ConnectorID] = true
		}
	}
	for _, id := range mem.self[t.ID] {
		owned[id] = true
	}
	if e.runsHere(t) && len(owned) == 0 && runsConnector(store.NodeEntry{Name: e.d.Node, State: store.NodeActive}, t, s) {
		sure = false
	}
	for _, n := range nodes {
		if n.Name == e.d.Node || !servedState(n.State) {
			continue
		}
		ids := e.place.ids[n.Name][t.ID]
		for id := range ids {
			owned[id] = true
		}
		switch {
		case len(ids) > 0 || !runsConnector(n, t, s):
		case !mem.heard[n.Name] && waits:
			return nil, false, errNotHeard
		default:
			sure = false
		}
	}
	return owned, sure, nil
}

// runsConnector says whether a node runs a connector of a tunnel: an active,
// undrained replica the shared one, its node a pinned one.
func runsConnector(n store.NodeEntry, t reconcile.TunnelState, s store.Settings) bool {
	if t.Kind == TunnelPinned {
		return t.Node == n.Name
	}
	if list := s.Cluster.Replicas; len(list) > 0 && !slices.Contains(list, n.Name) {
		return false
	}
	return n.State == store.NodeActive && n.Drained == nil
}
