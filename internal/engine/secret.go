package engine

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// TunnelRotation is what a rotation of the secret of a tunnel did.
type TunnelRotation struct {
	Tunnel   string `json:"tunnel"`
	TunnelID string `json:"tunnelId"`
	Account  string `json:"accountId"`
}

// accountsListed says whether the accounts of a credential were listed in
// this cycle, which they are every zoneRefreshEvery.
func (c *cycleRun) accountsListed(credential string) bool {
	cz := c.e.zones.byCred[credential]
	return cz != nil && cz.accountsOK && cz.accountsAt.Equal(c.now)
}

// fetchToken asks Cloudflare for the run token of a tunnel.
func (c *cycleRun) fetchToken(t reconcile.TunnelState) (string, error) {
	api := c.e.clients[t.CredentialID]
	if api == nil {
		return "", fmt.Errorf("no client for credential %s", t.CredentialID)
	}
	token, err := api.TunnelToken(c.ctx, t.AccountID, t.ID)
	switch {
	case err != nil:
		return "", err
	case strings.TrimSpace(token) == "":
		return "", errors.New("the token Cloudflare returned is empty")
	}
	return token, nil
}

// tokenRetry is a read of a run token again that failed: when, and why.
type tokenRetry struct {
	at  time.Time
	why string
}

// freshToken reads the run token of a tunnel again and returns it, or the one
// the connector has when it cannot be read, with why; a read that fails is
// kept, for followRefusals to try again. A token that changed, as after a
// rotation of the tunnel's secret, is an event: Ensure writes it and restarts
// the connector.
func (c *cycleRun) freshToken(t reconcile.TunnelState, stored string) (token, why string) {
	if c.reread == nil {
		c.reread = make(map[string]bool)
	}
	c.reread[t.ID] = true
	token, err := c.fetchToken(t)
	if err != nil {
		why := redact(err.Error(), stored)
		c.e.retries[t.ID] = tokenRetry{at: c.now, why: why}
		return stored, why
	}
	delete(c.e.retries, t.ID)
	if token != stored {
		c.events = append(c.events, Event{
			At: c.now, Level: levelInfo, Kind: kindConnector, Subject: t.Name, Tunnel: t.Name, Account: t.AccountID,
			Message: fmt.Sprintf("the run token of tunnel %s in account %s changed at Cloudflare; its connector restarts with the new one", t.Name, t.AccountID),
		})
	}
	return token, ""
}

// followRefusals reports every connector whose token Cloudflare refuses and,
// in enforce mode, reads the token of its tunnel again at once when the
// refusal starts, unless this cycle read it already: once per refusal, every
// zoneRefreshEvery while it lasts, and, while the last read failed, every
// rolloutAskEvery, as the connector serves nothing meanwhile.
func (c *cycleRun) followRefusals(existing []reconcile.TunnelState, before, now []connector.Status) {
	refused := func(statuses []connector.Status, id string) bool {
		return slices.ContainsFunc(statuses, func(s connector.Status) bool { return s.TunnelID == id && s.TokenRefused })
	}
	for _, t := range existing {
		if !refused(now, t.ID) {
			delete(c.e.retries, t.ID)
			continue
		}
		c.problem("tunnel %s in account %s: Cloudflare refuses the token its connector runs with; "+
			"pco reads the token again when that starts and every five minutes, and pco tunnel rotate gives the tunnel a new secret",
			t.Name, t.AccountID)
		if !c.mayEnsure() {
			continue
		}
		retry, failed := c.e.retries[t.ID]
		switch {
		case c.reread[t.ID]:
			// Read in this cycle, which said so when it failed.
			continue
		case failed && c.now.Before(retry.at.Add(rolloutAskEvery)) && !c.now.Before(retry.at):
			c.noteRetry(t, retry)
			continue
		case !failed && refused(before, t.ID):
			continue
		}
		stored, found, err := c.e.d.Connectors.Token(t.ID)
		if err != nil || !found {
			continue
		}
		token, why := c.freshToken(t, stored)
		switch {
		case why != "":
			c.noteRetry(t, c.e.retries[t.ID])
		case token != stored:
			c.ensureWith(t, token)
		}
	}
}

// noteRetry says that the read of a token failed, and when it is tried again.
func (c *cycleRun) noteRetry(t reconcile.TunnelState, retry tokenRetry) {
	c.problem("tunnel %s in account %s: reading its token again failed: %s; it is read again at %s, "+
		"while Cloudflare refuses the token its connector runs with",
		t.Name, t.AccountID, retry.why, retry.at.Add(rolloutAskEvery).UTC().Format(time.RFC3339))
}

// mayEnsure says whether the cycle may start or restart a connector: in
// enforce mode, once the settings and the install were read, and not while an
// appliance serves nothing. A cycle that holds after that may, for a token of
// the install's own tunnel.
func (c *cycleRun) mayEnsure() bool {
	return c.install.ID != "" && c.mode() == reconcile.Enforce && !c.e.notServing.Load()
}

// RotateTunnel gives the tunnel of the install in an account a new secret at
// Cloudflare and ends the connections of all its connectors: one that runs
// elsewhere with the old token cannot connect again, and the one on this node
// is restarted at once with the new token. account may be empty when the
// install has one tunnel. The tunnel is one the last state shows with its id
// and credential, also when the last cycle held; in observe-only mode nothing
// is done.
func (e *Engine) RotateTunnel(ctx context.Context, account string) (TunnelRotation, error) {
	if err := e.acquireAdmin(ctx); err != nil {
		return TunnelRotation{}, err
	}
	defer e.release()
	if err := e.refusedAsCopy(); err != nil {
		return TunnelRotation{}, err
	}
	s, err := e.d.Store.Settings()
	if err != nil {
		return TunnelRotation{}, fmt.Errorf("reading the settings: %w", err)
	}
	if s.ObserveOnly {
		return TunnelRotation{}, fmt.Errorf("%w: pco is in observe-only mode and changes nothing at Cloudflare; run pco apply to end it", ErrRefused)
	}
	inst, found, err := e.d.Store.Install()
	switch {
	case err != nil:
		return TunnelRotation{}, fmt.Errorf("reading the install identity: %w", err)
	case !found:
		return TunnelRotation{}, fmt.Errorf("%w: %s", ErrRefused, problemNotSetUp)
	}
	v, err := RotationTarget(e.State().Tunnels, account)
	if err != nil {
		return TunnelRotation{}, err
	}
	t := v.TunnelState
	api := e.clients[t.CredentialID]
	if api == nil {
		return TunnelRotation{}, fmt.Errorf("%w: no client for credential %s", ErrRefused, t.CredentialID)
	}
	res := TunnelRotation{Tunnel: t.Name, TunnelID: t.ID, Account: t.AccountID}
	what := fmt.Sprintf("tunnel %s in account %s", t.Name, t.AccountID)

	secret := make([]byte, cfapi.MinTunnelSecret)
	if _, err := rand.Read(secret); err != nil {
		return res, fmt.Errorf("making a secret: %w", err)
	}
	if err := api.RotateTunnelSecret(ctx, t.AccountID, t.ID, secret); err != nil {
		return res, fmt.Errorf("rotating the secret of %s: %w", what, err)
	}
	cleanErr := api.CleanUpConnections(ctx, t.AccountID, t.ID)
	// The connectors are listed in the next cycle: the rollout to the one
	// restarted, and whether any other came back.
	delete(e.rolledOut, t.ID)
	delete(e.asked, t.ID)
	defer e.Trigger()
	token, err := api.TunnelToken(ctx, t.AccountID, t.ID)
	if err == nil && strings.TrimSpace(token) == "" {
		err = errors.New("the token Cloudflare returned is empty")
	}
	if err != nil {
		e.retries[t.ID] = tokenRetry{at: e.d.Now(), why: err.Error()}
		return res, fmt.Errorf("the secret of %s was rotated, but its new token could not be read: %w; "+
			"its connector on this node takes it up when Cloudflare refuses the old one", what, err)
	}
	if err := e.d.Connectors.Ensure(ctx, inst.ID, t.ID, token); err != nil {
		return res, fmt.Errorf("the secret of %s was rotated, but its connector on this node could not be restarted: %s", what, redact(err.Error(), token))
	}
	if cleanErr != nil {
		return res, fmt.Errorf("the secret of %s was rotated and its connector on this node restarts with the new token, "+
			"but the connections of the tunnel could not be ended: %w; a connector elsewhere keeps its session until it reconnects: "+
			"run pco tunnel rotate again", what, cleanErr)
	}
	e.adminEvent(ctx, t.Name, fmt.Sprintf("the secret of %s was rotated: every connector of the tunnel was disconnected, "+
		"and the one on this node restarts with the new token", what))
	return res, nil
}

// RotationTarget returns the tunnel a rotation is of among the tunnels of a
// state: the one in account, or the only one when account is empty. It has to
// exist with its id and credential known. A tunnel left as it is for a reason
// of its own, as a frozen account, is refused, also when the last cycle did
// not check it; one the last cycle did not check, whatever held it, is not: a
// hold must not keep the secret of a tunnel that someone else runs a
// connector of from being rotated.
func RotationTarget(tunnels []TunnelView, account string) (TunnelView, error) {
	var found []TunnelView
	for _, t := range tunnels {
		if t.Exists && t.ID != "" && t.CredentialID != "" && !t.Unknown && (account == "" || t.AccountID == account) {
			found = append(found, t)
		}
	}
	switch {
	case len(found) == 0 && account != "":
		return TunnelView{}, fmt.Errorf("%w: no tunnel of this install is known in account %s; pco status lists the tunnels", ErrNotFound, account)
	case len(found) == 0:
		return TunnelView{}, fmt.Errorf("%w: no tunnel of this install is known; pco status lists the tunnels", ErrNotFound)
	case len(found) > 1:
		accounts := make([]string, 0, len(found))
		for _, t := range found {
			accounts = append(accounts, t.AccountID)
		}
		return TunnelView{}, fmt.Errorf("%w: the install has tunnels in accounts %s; name one with --account", ErrInvalid, andList(accounts))
	case found[0].LeftAsIs:
		return TunnelView{}, fmt.Errorf("%w: tunnel %s in account %s is left as it is: %s; nothing was changed",
			ErrRefused, found[0].Name, found[0].AccountID, found[0].Held)
	}
	return found[0], nil
}
