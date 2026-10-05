package engine

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const tagModeNote = "; the admission mode is tag, so it matters only for the routes at observed until the mode is approve"

// ApprovalView is the approval of a guest: the identity it was approved in,
// and whether the guest has that identity now.
type ApprovalView struct {
	Owner    string     `json:"owner"`
	Guest    *GuestView `json:"guest,omitempty"`
	Identity string     `json:"identity"`          // the identity that was approved
	Current  string     `json:"current,omitempty"` // the identity the last listing showed; empty when it did not have the guest
	Matches  bool       `json:"matches"`           // the guest has the identity that was approved
	// MACs may answer for the routes of the guest at the observed level, and
	// Addresses are the soft-denied addresses it may be published at.
	MACs      []string     `json:"macs,omitempty"`
	Addresses []netip.Addr `json:"addresses,omitempty"`
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
		a := approvals[owner]
		v := ApprovalView{Owner: owner, Guest: l.guestView(owner), Identity: a.Identity, MACs: a.MACs, Addresses: a.Addresses}
		v.Current = l.identity(owner)
		v.Matches = v.Current != "" && v.Current == v.Identity
		out = append(out, v)
	}
	return out, nil
}

// Approval is an approval as ApproveGuest recorded it, with the admission
// mode it matters in: the MACs and the addresses are all those it holds now.
type Approval struct {
	Owner     string       `json:"owner"`
	Guest     *GuestView   `json:"guest,omitempty"`
	Identity  string       `json:"identity"`
	Mode      string       `json:"mode"` // the admission mode: "tag" or "approve"
	MACs      []string     `json:"macs,omitempty"`
	Addresses []netip.Addr `json:"addresses,omitempty"`
}

// ApproveGuest approves a guest in the identity the last listing showed for
// it, so that a guest re-created under its VMID, or a clone, is not approved
// by it. A guest the last cycle did not see, as when it did not list every
// guest, is refused: what is approved is what the admin can see. identity,
// when it is not empty, is the identity the admin was shown: a guest that has
// another one now changed since, and is refused.
//
// macs are the MACs the admin was shown the guest waits for at observed,
// which must be those the last state shows, and addrs the soft-denied
// addresses the admin allows it, shown or not. An approval of the same
// identity as before keeps the MACs approved before; one of another identity
// drops them. The addresses allowed before are kept either way.
func (e *Engine) ApproveGuest(ctx context.Context, owner, identity string, macs []string, addrs []netip.Addr) (Approval, error) {
	ref, err := guestOwner(owner)
	if err != nil {
		return Approval{}, err
	}
	macs, err = approvedMACs(macs)
	if err != nil {
		return Approval{}, err
	}
	if err := checkAllowed(addrs); err != nil {
		return Approval{}, err
	}
	addrs = slices.CompactFunc(slices.SortedFunc(slices.Values(addrs), netip.Addr.Compare), func(x, y netip.Addr) bool { return x == y })
	if err := e.acquireAdmin(ctx); err != nil {
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
	if shown := e.waitsFor(ref); !slices.Equal(macs, shown) {
		return Approval{}, fmt.Errorf("%w: %s changed since it was shown: it was shown with %s and waits for %s now; look at it again",
			ErrRefused, owner, macsText(macs), macsText(shown))
	}
	mode, note, err := e.admission()
	if err != nil {
		return Approval{}, err
	}
	before, err := e.d.Store.Approvals()
	if err != nil {
		return Approval{}, fmt.Errorf("reading the approvals: %w", err)
	}
	a := store.Approval{Owner: owner, Identity: g.identity, MACs: macs, Addresses: addrs}
	if old, ok := before[owner]; ok {
		a.Addresses = slices.Concat(old.Addresses, addrs)
		if old.Identity == g.identity {
			a.MACs = slices.Concat(old.MACs, macs)
		}
	}
	a.MACs = slices.Compact(slices.Sorted(slices.Values(a.MACs)))
	a.Addresses = slices.CompactFunc(slices.SortedFunc(slices.Values(a.Addresses), netip.Addr.Compare), func(x, y netip.Addr) bool { return x == y })
	if err := e.d.Store.SaveApproval(a); err != nil {
		return Approval{}, fmt.Errorf("saving the approval: %w", err)
	}
	e.adminEvent(ctx, owner, fmt.Sprintf("%s is approved in identity %s%s%s", OwnerName(owner, l.guestView(owner)), g.identity, withText(macs, addrs), note))
	e.Trigger()
	return Approval{Owner: owner, Guest: l.guestView(owner), Identity: g.identity, Mode: mode, MACs: a.MACs, Addresses: a.Addresses}, nil
}

// waitsFor returns the MACs the last state shows the guest waits for.
func (e *Engine) waitsFor(ref model.GuestRef) []string {
	for _, w := range e.State().Unapproved {
		if w.GuestRef == ref {
			return w.MACs
		}
	}
	return nil
}

// approvedMACs returns macs in normal form, sorted and each once.
func approvedMACs(macs []string) ([]string, error) {
	out := make([]string, 0, len(macs))
	for _, m := range macs {
		mac, err := model.NormalizeMAC(m)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		out = append(out, mac)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return slices.Compact(slices.Sorted(slices.Values(out))), nil
}

// checkAllowed refuses an address that is not one a route may be published
// at: only IPv4 is.
func checkAllowed(addrs []netip.Addr) error {
	for _, a := range addrs {
		if !a.Is4() {
			return fmt.Errorf("%w: %s is not an IPv4 address a guest could be published at", ErrInvalid, a)
		}
	}
	return nil
}

// macsText names MACs: "no MAC", "MAC m", "MACs m, n".
func macsText(macs []string) string {
	switch len(macs) {
	case 0:
		return "no MAC"
	case 1:
		return "MAC " + macs[0]
	}
	return "MACs " + strings.Join(macs, ", ")
}

// withText says what an approval records besides the identity: " with MAC m
// and address a", or nothing.
func withText(macs []string, addrs []netip.Addr) string {
	var parts []string
	if len(macs) > 0 {
		parts = append(parts, macsText(macs))
	}
	switch len(addrs) {
	case 0:
	case 1:
		parts = append(parts, "address "+addrs[0].String())
	default:
		names := make([]string, len(addrs))
		for i, a := range addrs {
			names[i] = a.String()
		}
		parts = append(parts, "addresses "+strings.Join(names, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return " with " + strings.Join(parts, " and ")
}

// RevokeGuest removes the approval of a guest.
func (e *Engine) RevokeGuest(ctx context.Context, owner string) error {
	if _, err := guestOwner(owner); err != nil {
		return err
	}
	if err := e.acquireAdmin(ctx); err != nil {
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
	e.adminEvent(ctx, owner, fmt.Sprintf("the approval of %s is revoked%s", OwnerName(owner, e.lastListing().guestView(owner)), note))
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
