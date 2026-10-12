package apifake

import (
	"context"
	"fmt"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Managed returns the managed network of the state.
func (e *Engine) Managed() (engine.ManagedView, error) {
	if err := e.begin(context.Background(), "Managed", noArgs); err != nil {
		return engine.ManagedView{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state.Managed, nil
}

// ApproveManaged consents to a card for a guest of the state: one that
// waits for the managed network no longer does, and its card is shown as
// attached from the next cycle, which the scenario has no way to run; the
// consent is answered as the daemon would.
func (e *Engine) ApproveManaged(ctx context.Context, owner, identity string, acceptFirewallImpact bool) (store.Consent, error) {
	if err := e.begin(ctx, "ApproveManaged", struct {
		Owner                string `json:"owner"`
		Identity             string `json:"identity"`
		AcceptFirewallImpact bool   `json:"acceptFirewallImpact"`
	}{owner, identity, acceptFirewallImpact}); err != nil {
		return store.Consent{}, err
	}
	ref, err := model.ParseGuestRef(owner)
	if err != nil {
		return store.Consent{}, fmt.Errorf("%w: only a guest is approved, named as qemu/101 or lxc/200: %w", engine.ErrInvalid, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	i := slices.IndexFunc(e.guests, func(g engine.GuestListView) bool { return g.Ref == owner })
	if i < 0 {
		return store.Consent{}, fmt.Errorf("%w: %s is not in the last listing of Proxmox; approve what you can see", engine.ErrRefused, owner)
	}
	g := e.guests[i]
	switch {
	case g.Identity == "":
		return store.Consent{}, fmt.Errorf("%w: %s has no identity Proxmox reports, and a consent is of one", engine.ErrRefused, owner)
	case identity != "" && identity != g.Identity:
		return store.Consent{}, fmt.Errorf("%w: %s changed since it was shown: it was shown in identity %s and has identity %s now; "+
			"look at it again", engine.ErrRefused, owner, identity, g.Identity)
	}
	m := e.state.Managed
	m.Waiting = slices.DeleteFunc(slices.Clone(m.Waiting), func(w engine.ManagedWaiting) bool { return w.GuestRef == ref })
	e.state.Managed = m
	msg := fmt.Sprintf("%s is approved for the managed network in identity %s", engine.OwnerName(owner, &engine.GuestView{GuestRef: ref, Name: g.Name}), g.Identity)
	if acceptFirewallImpact {
		msg += ", with the firewall-impact confirmation"
	}
	e.adminEvent(ctx, owner, msg)
	e.changed()
	return store.Consent{Owner: owner, Identity: g.Identity, At: e.now(), Actor: engine.ActorOf(ctx), AcceptFirewallImpact: acceptFirewallImpact}, nil
}

// RevokeManaged removes the consent of a guest: its attachment, when the
// state shows one made by an approval, starts its grace.
func (e *Engine) RevokeManaged(ctx context.Context, owner string) error {
	if err := e.begin(ctx, "RevokeManaged", struct {
		Owner string `json:"owner"`
	}{owner}); err != nil {
		return err
	}
	if _, err := model.ParseGuestRef(owner); err != nil {
		return fmt.Errorf("%w: only a guest is approved, named as qemu/101 or lxc/200: %w", engine.ErrInvalid, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.state.Managed
	i := slices.IndexFunc(m.Attachments, func(a engine.AttachmentView) bool { return a.Owner == owner && a.Consent == "approval" })
	if i < 0 {
		return fmt.Errorf("%w: %s has no consent for the managed network", engine.ErrNotFound, owner)
	}
	atts := slices.Clone(m.Attachments)
	at := e.now().Add(10 * 60e9)
	atts[i].DetachAt, atts[i].DetachWhy = &at, "the guest has neither the tag cf-tunnel-managed nor an approval"
	atts[i].State, atts[i].Detail = engine.AttachmentDetaching, "goes at "+at.UTC().Format("2006-01-02T15:04:05Z07:00")+": "+atts[i].DetachWhy
	m.Attachments = atts
	e.state.Managed = m
	e.adminEvent(ctx, owner, fmt.Sprintf("the consent of %s for the managed network is revoked; a card it has goes after the grace",
		engine.OwnerName(owner, atts[i].Guest)))
	e.changed()
	return nil
}

// EnableManaged turns the managed network of the state on.
func (e *Engine) EnableManaged(ctx context.Context) error {
	if err := e.begin(ctx, "EnableManaged", noArgs); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.Managed.Enabled {
		return nil
	}
	m := e.state.Managed
	m.Enabled = true
	e.state.Managed = m
	e.emit(engine.Event{Level: "info", Kind: "managed", Subject: "pconet", Actor: engine.ActorOf(ctx),
		Message: fmt.Sprintf("the managed network is enabled: pconet with the subnet %s, the leg %s on node %s, mark 1", m.Subnet, m.Subnet.Addr().Next(), e.state.Node)})
	e.changed()
	return nil
}
