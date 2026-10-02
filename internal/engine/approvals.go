package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const tagModeNote = "; the admission mode is tag, so it matters only once the mode is approve"

// ApprovalView is the approval of a guest: the identity it was approved in,
// and whether the guest has that identity now.
type ApprovalView struct {
	Owner    string     `json:"owner"`
	Guest    *GuestView `json:"guest,omitempty"`
	Identity string     `json:"identity"`          // the identity that was approved
	Current  string     `json:"current,omitempty"` // the identity the last listing showed; empty when it did not have the guest
	Matches  bool       `json:"matches"`           // the guest has the identity that was approved
}

// Approvals returns the approvals the store keeps, by owner, with what the
// last listing showed of each guest.
func (e *Engine) Approvals() ([]ApprovalView, error) {
	approvals, err := e.d.Store.Approvals()
	if err != nil {
		return nil, fmt.Errorf("reading the approvals: %w", err)
	}
	l := e.lastListing()
	owners := slices.SortedFunc(maps.Keys(approvals), model.CompareOwners)
	out := make([]ApprovalView, 0, len(owners))
	for _, owner := range owners {
		v := ApprovalView{Owner: owner, Guest: l.guestView(owner), Identity: approvals[owner]}
		v.Current = l.identity(owner)
		v.Matches = v.Current != "" && v.Current == v.Identity
		out = append(out, v)
	}
	return out, nil
}

// Approval is an approval as ApproveGuest recorded it, with the admission
// mode it matters in.
type Approval struct {
	Owner    string     `json:"owner"`
	Guest    *GuestView `json:"guest,omitempty"`
	Identity string     `json:"identity"`
	Mode     string     `json:"mode"` // the admission mode: "tag" or "approve"
}

// ApproveGuest approves a guest in the identity the last listing showed for
// it, so that a guest re-created under its VMID, or a clone, is not approved
// by it. A guest the last cycle did not see, as when it did not list every
// guest, is refused: what is approved is what the admin can see. identity,
// when it is not empty, is the identity the admin was shown: a guest that has
// another one now changed since, and is refused.
func (e *Engine) ApproveGuest(ctx context.Context, owner, identity string) (Approval, error) {
	ref, err := guestOwner(owner)
	if err != nil {
		return Approval{}, err
	}
	if err := e.acquire(ctx); err != nil {
		return Approval{}, err
	}
	defer e.release()

	l := e.lastListing()
	g, listed := l.guests[ref]
	switch {
	case !l.complete:
		return Approval{}, fmt.Errorf("%w: the last cycle did not list every guest, so %s cannot be approved as it is now; "+
			"approve what you can see once a cycle has listed them all", ErrRefused, owner)
	case !listed:
		return Approval{}, fmt.Errorf("%w: %s is not in the last listing of Proxmox; approve what you can see", ErrRefused, owner)
	case g.identity == "":
		return Approval{}, fmt.Errorf("%w: %s has no identity Proxmox reports, and an approval is of one", ErrRefused, owner)
	case identity != "" && identity != g.identity:
		return Approval{}, fmt.Errorf("%w: %s changed since it was shown: it was shown in identity %s and has identity %s now; "+
			"look at it again", ErrRefused, owner, identity, g.identity)
	}
	mode, note, err := e.admission()
	if err != nil {
		return Approval{}, err
	}
	if err := e.d.Store.SaveApproval(owner, g.identity); err != nil {
		return Approval{}, fmt.Errorf("saving the approval: %w", err)
	}
	e.adminEvent(owner, fmt.Sprintf("%s is approved in identity %s%s", OwnerName(owner, l.guestView(owner)), g.identity, note))
	e.Trigger()
	return Approval{Owner: owner, Guest: l.guestView(owner), Identity: g.identity, Mode: mode}, nil
}

// RevokeGuest removes the approval of a guest.
func (e *Engine) RevokeGuest(ctx context.Context, owner string) error {
	if _, err := guestOwner(owner); err != nil {
		return err
	}
	if err := e.acquire(ctx); err != nil {
		return err
	}
	defer e.release()

	approvals, err := e.d.Store.Approvals()
	if err != nil {
		return fmt.Errorf("reading the approvals: %w", err)
	}
	if _, ok := approvals[owner]; !ok {
		return fmt.Errorf("%w: %s has no approval", ErrNotFound, owner)
	}
	_, note, err := e.admission()
	if err != nil {
		return err
	}
	if err := e.d.Store.DeleteApproval(owner); err != nil {
		return fmt.Errorf("removing the approval: %w", err)
	}
	e.adminEvent(owner, fmt.Sprintf("the approval of %s is revoked%s", OwnerName(owner, e.lastListing().guestView(owner)), note))
	e.Trigger()
	return nil
}

// guestOwner reads the owner of an approval, which is always a guest.
func guestOwner(owner string) (model.GuestRef, error) {
	ref, err := model.ParseGuestRef(owner)
	if err != nil {
		return model.GuestRef{}, fmt.Errorf("%w: only a guest is approved, named as qemu/101 or lxc/200: %w", ErrInvalid, err)
	}
	return ref, nil
}

// admission is the admission mode, and what an approval event adds about it.
func (e *Engine) admission() (mode, note string, err error) {
	s, err := e.d.Store.Settings()
	if err != nil {
		return "", "", fmt.Errorf("reading the settings: %w", err)
	}
	if s.Admission == store.AdmissionApprove {
		return s.Admission, "", nil
	}
	return s.Admission, tagModeNote, nil
}

// OwnerName names an owner, with the name of its guest when that is known:
// "qemu/101 (web-1)", "qemu/101", "manual/www".
func OwnerName(owner string, guest *GuestView) string {
	if guest == nil || guest.Name == "" {
		return owner
	}
	return owner + " (" + guest.Name + ")"
}
