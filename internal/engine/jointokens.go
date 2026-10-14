package engine

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// NewJoinToken makes the token an appliance agent of node enrols with, once,
// within pki.JoinTokenTTL. The install keeps the hash of its secret, the
// node and the expiry; the token itself is in the answer and nowhere else:
// not in the events, not in the log.
func (e *Engine) NewJoinToken(ctx context.Context, node string) (JoinTokenView, error) {
	if err := e.acquireWriter(ctx); err != nil {
		return JoinTokenView{}, err
	}
	defer e.release()
	entry, err := e.entryOf(node)
	switch {
	case err != nil && !errors.Is(err, ErrNotFound):
		return JoinTokenView{}, err
	case err != nil:
	case entry.Node.State == store.NodeContested:
		return JoinTokenView{}, fmt.Errorf("%w: node %s is contested; pco node forget %s first, then a new token", ErrRefused, node, node)
	case entry.Node.State == store.NodeActive, entry.Node.State == store.NodeJoining, entry.Node.State == store.NodeDraining:
		return JoinTokenView{}, fmt.Errorf("%w: node %s is %s in the install already: a token would enrol a second agent under its name; "+
			"pco node forget %s first when it is to be replaced", ErrRefused, node, entry.Node.State, node)
	}
	e.dropExpiredJoinTokens()
	inst, found, err := e.d.Store.Install()
	switch {
	case err != nil:
		return JoinTokenView{}, fmt.Errorf("reading the install identity: %w", err)
	case !found:
		return JoinTokenView{}, fmt.Errorf("%w: %s", ErrRefused, problemNotSetUp)
	}
	cur, _, _, err := e.d.Store.CA()
	switch {
	case errors.Is(err, store.ErrNoCA):
		return JoinTokenView{}, fmt.Errorf("%w: the install has no CA yet; the daemon makes one at its first start on a cluster", ErrRefused)
	case err != nil:
		return JoinTokenView{}, fmt.Errorf("reading the CA: %w", err)
	}
	ca, err := pki.ParseCA([]byte(cur.CertPEM), []byte(cur.KeyPEM.Reveal()))
	if err != nil {
		return JoinTokenView{}, fmt.Errorf("reading the CA: %w", err)
	}
	var endpoints []string
	if e.d.Endpoints != nil {
		endpoints = e.d.Endpoints()
	}
	if len(endpoints) == 0 && inst.Appliance != nil && inst.Appliance.KindName() == store.ApplianceCombined {
		return JoinTokenView{}, fmt.Errorf("%w: a combined appliance takes no agents of other nodes; "+
			"pco appliance convert --vmid %d on its node makes it a controller", ErrRefused, inst.Appliance.VMID)
	}
	if len(endpoints) == 0 {
		return JoinTokenView{}, fmt.Errorf("%w: this controller has no agent endpoint to name in the token; pco setup gives the node an agent address", ErrRefused)
	}
	now := e.d.Now()
	tok, err := pki.NewJoinToken(inst.ID, node, endpoints, ca, now, rand.Reader)
	if err != nil {
		return JoinTokenView{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	rec := store.JoinRecord{ID: randomHex(8)(), Node: node, SecretSHA256: tok.SecretHash(), CreatedAt: now, Expires: tok.Expires}
	if err := e.d.Store.SaveJoinToken(rec); err != nil {
		return JoinTokenView{}, fmt.Errorf("keeping the join token: %w", err)
	}
	e.adminEvent(ctx, node, fmt.Sprintf("a join token (%s) was made for node %s; it expires at %s and is good once", rec.ID, node, rec.Expires.UTC().Format("2006-01-02T15:04:05Z")))
	return JoinTokenView{ID: rec.ID, Node: node, Expires: rec.Expires, Token: tok.Encode()}, nil
}

// expireJoinTokens deletes the records of the join tokens that expired, in a
// cycle that may write the store of a cluster.
func (c *cycleRun) expireJoinTokens() {
	if c.leaseHeld || c.storeHold || len(c.nodes) <= 1 {
		return
	}
	c.e.dropExpiredJoinTokens()
}

// dropExpiredJoinTokens deletes the records of the join tokens that expired:
// such a token enrols nothing any more. A record that cannot be read or
// deleted stays for the next time.
func (e *Engine) dropExpiredJoinTokens() {
	tokens, err := e.d.Store.JoinTokens()
	if err != nil {
		return
	}
	now := e.d.Now()
	for id, rec := range tokens {
		if now.After(rec.Expires) {
			_ = e.d.Store.DeleteJoinToken(id)
		}
	}
}
