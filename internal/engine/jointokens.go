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
	if entry, err := e.entryOf(node); err == nil && entry.Node.State == store.NodeContested {
		return JoinTokenView{}, fmt.Errorf("%w: node %s is contested; pco node forget %s first, then a new token", ErrRefused, node, node)
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return JoinTokenView{}, err
	}
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
