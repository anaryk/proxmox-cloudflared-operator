package apifake

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Apply leaves observe-only mode and, with confirmDeletes, accepts what the
// state shows waiting when offer names it: the offer ends, and with it the
// problem lines that asked for the confirmation, as the daemon withdraws
// them.
func (e *Engine) Apply(ctx context.Context, confirmDeletes bool, offer string) (engine.ApplyResult, error) {
	args := struct {
		ConfirmDeletes bool   `json:"confirmDeletes"`
		Offer          string `json:"offer"`
	}{confirmDeletes, offer}
	if err := e.begin(ctx, "Apply", args); err != nil {
		return engine.ApplyResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if confirmDeletes && offer != e.state.Offer {
		return engine.ApplyResult{}, fmt.Errorf("%w: what waits for a confirmation changed since it was shown; look again and repeat", engine.ErrRefused)
	}
	res := engine.ApplyResult{Accepted: []engine.Waiting{}}
	s, err := e.store.Settings()
	if err != nil {
		return res, err
	}
	if s.ObserveOnly {
		s.ObserveOnly = false
		if err := e.store.SaveSettings(s); err != nil {
			return res, fmt.Errorf("leaving observe-only mode: %w", err)
		}
		res.LeftObserveOnly = true
		e.state.Mode = engine.ModeEnforce
		e.adminEvent(ctx, "", "observe-only mode ended; changes are applied from now on")
	}
	if confirmDeletes {
		res.Accepted = nonNil(slices.Clone(e.state.Waiting))
		for _, w := range res.Accepted {
			e.adminEvent(ctx, w.Subject, confirmedText(w))
		}
		if len(res.Accepted) == 0 {
			e.adminEvent(ctx, "", "the last state showed nothing that waits for a confirmation")
		}
		e.state.Waiting, e.state.Offer = []engine.Waiting{}, ""
		e.state.Problems = slices.DeleteFunc(slices.Clone(e.state.Problems), engine.AsksForConfirmation)
	}
	e.changed()
	return res, nil
}

// confirmedText says what a confirmation did of what waited.
func confirmedText(w engine.Waiting) string {
	switch w.Kind {
	case engine.WaitingRemovals:
		return "the deletes held by the mass delete guard are confirmed for the next run"
	case engine.WaitingVanished:
		return fmt.Sprintf("%d guests that Proxmox no longer lists are confirmed removed; "+
			"when their DNS records fall due, the mass delete guard may ask for a confirmation again", len(w.Items))
	case engine.WaitingZone:
		return "the zone that left its listing is confirmed gone"
	}
	return fmt.Sprintf("the tunnel %s is confirmed gone; its connector is removed", w.Subject)
}

// Adopt asks for the record that holds name to be taken over, which the
// state must show in conflict or without its marker.
func (e *Engine) Adopt(ctx context.Context, name string) error {
	if err := e.begin(ctx, "Adopt", struct {
		Name string `json:"name"`
	}{name}); err != nil {
		return err
	}
	host, err := hostname.Normalize(name)
	if err != nil {
		return fmt.Errorf("%w: %w", engine.ErrInvalid, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	same := func(n string) bool { return strings.EqualFold(strings.TrimSuffix(n, "."), host) }
	if !slices.ContainsFunc(e.state.Conflicts, func(c reconcile.Conflict) bool { return same(c.Name) }) && !slices.ContainsFunc(e.state.Lost, same) {
		return fmt.Errorf("%w: no record of someone else holds %s, and none of ours lost its marker there", engine.ErrNotFound, host)
	}
	e.adminEvent(ctx, host, "adoption requested for the next run")
	return nil
}

// RotateTunnel rotates nothing, but answers as the daemon does: the tunnel
// is the one engine.RotationTarget picks.
func (e *Engine) RotateTunnel(ctx context.Context, account string) (engine.TunnelRotation, error) {
	if err := e.begin(ctx, "RotateTunnel", struct {
		Account string `json:"account"`
	}{account}); err != nil {
		return engine.TunnelRotation{}, err
	}
	s, err := e.stored().Settings()
	if err != nil {
		return engine.TunnelRotation{}, err
	}
	if s.ObserveOnly {
		return engine.TunnelRotation{}, fmt.Errorf("%w: %s", engine.ErrRefused, engine.ObserveOnlyRefusal)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	v, err := engine.RotationTarget(e.state.Tunnels, account)
	if err != nil {
		return engine.TunnelRotation{}, err
	}
	what := fmt.Sprintf("tunnel %s in account %s", v.Name, v.AccountID)
	e.adminEvent(ctx, v.Name, fmt.Sprintf("the secret of %s was rotated: every connector of the tunnel was disconnected, "+
		"and the one on this node restarts with the new token", what))
	return engine.TunnelRotation{Tunnel: v.Name, TunnelID: v.ID, Account: v.AccountID}, nil
}

// AddCredential checks a token as finding the report of the scenario, and
// stores it under a new id when the daemon would: a report that is not
// usable, or of a check Cloudflare did not answer, refuses it, and the
// refusal carries the view of the credential as checked.
func (e *Engine) AddCredential(ctx context.Context, label, token string) (engine.CredentialView, error) {
	e.record(ctx, "AddCredential", struct {
		Label       string `json:"label"`
		TokenLength int    `json:"tokenLength"`
	}{label, len(token)})
	if r := e.refused("AddCredential"); r.err != nil {
		if r.credential != nil {
			return *r.credential, r.err
		}
		return engine.CredentialView{}, r.err
	}
	label, token = strings.TrimSpace(label), strings.TrimSpace(token)
	switch {
	case label == "":
		return engine.CredentialView{}, fmt.Errorf("%w: the label is empty", engine.ErrInvalid)
	case token == "":
		return engine.CredentialView{}, fmt.Errorf("%w: the token is empty", engine.ErrInvalid)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	report := e.report
	report.CheckedAt = e.now()
	v := engine.CredentialView{Label: label, Kind: "scoped", Checked: true, Report: report}
	if err := engine.Unusable(report); err != nil {
		return v, err
	}
	for n := len(e.state.Credentials) + 1; v.ID == ""; n++ {
		id := fmt.Sprintf("cred%d", n)
		if !slices.ContainsFunc(e.state.Credentials, func(c engine.CredentialView) bool { return c.ID == id }) {
			v.ID = id
		}
	}
	id := v.ID
	creds := append(slices.Clone(e.state.Credentials), v)
	slices.SortFunc(creds, func(a, b engine.CredentialView) int { return strings.Compare(a.ID, b.ID) })
	e.state.Credentials = creds
	e.adminEvent(ctx, id, fmt.Sprintf("credential %q added", label))
	e.changed()
	return v, nil
}

// Credentials returns the credentials of the state.
func (e *Engine) Credentials() ([]engine.CredentialView, error) {
	if err := e.begin(context.Background(), "Credentials", noArgs); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.state.Credentials), nil
}

// CheckCredential finds what it found before, now.
func (e *Engine) CheckCredential(ctx context.Context, id string, deep bool) (engine.CredentialView, error) {
	if err := e.begin(ctx, "CheckCredential", struct {
		ID   string `json:"id"`
		Deep bool   `json:"deep"`
	}{id, deep}); err != nil {
		return engine.CredentialView{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	i := slices.IndexFunc(e.state.Credentials, func(c engine.CredentialView) bool { return c.ID == id })
	if i < 0 {
		return engine.CredentialView{}, fmt.Errorf("%w: no credential %q", engine.ErrNotFound, id)
	}
	creds := slices.Clone(e.state.Credentials)
	v := &creds[i]
	if !v.Checked {
		v.Report = e.report
	}
	v.Checked, v.Report.Deep, v.Report.CheckedAt = true, deep, e.now()
	e.state.Credentials = creds
	e.changed()
	return *v, nil
}

// RemoveCredential removes a credential of the state.
func (e *Engine) RemoveCredential(ctx context.Context, id string) error {
	if err := e.begin(ctx, "RemoveCredential", struct {
		ID string `json:"id"`
	}{id}); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	i := slices.IndexFunc(e.state.Credentials, func(c engine.CredentialView) bool { return c.ID == id })
	if i < 0 {
		return fmt.Errorf("%w: no credential %q", engine.ErrNotFound, id)
	}
	label := e.state.Credentials[i].Label
	e.state.Credentials = slices.Delete(slices.Clone(e.state.Credentials), i, i+1)
	e.adminEvent(ctx, id, fmt.Sprintf("credential %q removed", label))
	e.changed()
	return nil
}

// Claims returns the claims of the scenario.
func (e *Engine) Claims() ([]engine.ClaimView, error) {
	if err := e.begin(context.Background(), "Claims", noArgs); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.claims), nil
}

// ResolveClaim hands a hostname to an owner that waits for it.
func (e *Engine) ResolveClaim(ctx context.Context, name, owner string) error {
	if err := e.begin(ctx, "ResolveClaim", struct {
		Hostname string `json:"hostname"`
		Owner    string `json:"owner"`
	}{name, owner}); err != nil {
		return err
	}
	host, err := hostname.Normalize(name)
	if err != nil {
		return fmt.Errorf("%w: %w", engine.ErrInvalid, err)
	}
	if _, err := model.ParseGuestRef(owner); err != nil && !strings.HasPrefix(owner, model.ManualPrefix) {
		return fmt.Errorf("%w: %q is no owner: name a guest, as qemu/101 or lxc/200, or a manual route, as manual/<id>", engine.ErrInvalid, owner)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	i := slices.IndexFunc(e.claims, func(c engine.ClaimView) bool { return c.Hostname == host })
	if i < 0 {
		return fmt.Errorf("%w: nobody holds a claim on %s", engine.ErrNotFound, host)
	}
	old := e.claims[i]
	j := slices.IndexFunc(old.Waiting, func(w engine.ClaimantView) bool { return w.Owner == owner })
	switch {
	case old.Holder == owner:
		return fmt.Errorf("%w: %s holds %s already", engine.ErrInvalid, owner, host)
	case j < 0:
		return fmt.Errorf("%w: %s no longer claims %s: it has no route for it and does not name it", engine.ErrRefused, owner, host)
	}
	now := e.now()
	moved := engine.ClaimView{
		Hostname: host, Holder: owner, Guest: old.Waiting[j].Guest, Since: now, State: old.State,
		Waiting: append(slices.Delete(slices.Clone(old.Waiting), j, j+1), engine.ClaimantView{Owner: old.Holder, Guest: old.Guest, Since: now}),
	}
	e.claims = slices.Clone(e.claims)
	e.claims[i] = moved
	e.adminEvent(ctx, host, fmt.Sprintf("the claim on %s was moved from %s to %s by the admin; %s waits for it from now on",
		host, old.Holder, owner, old.Holder))
	return nil
}

// Approvals returns the approvals of the scenario.
func (e *Engine) Approvals() ([]engine.ApprovalView, error) {
	if err := e.begin(context.Background(), "Approvals", noArgs); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.approvals), nil
}

// ApproveGuest approves a guest that waits for it, or one the guest list
// has, in the identity it has, with the MACs the state shows it waits for.
func (e *Engine) ApproveGuest(ctx context.Context, owner, identity string, macs []string, addrs []netip.Addr) (engine.Approval, error) {
	if err := e.begin(ctx, "ApproveGuest", struct {
		Owner     string       `json:"owner"`
		Identity  string       `json:"identity"`
		MACs      []string     `json:"macs"`
		Addresses []netip.Addr `json:"addresses"`
	}{owner, identity, macs, addrs}); err != nil {
		return engine.Approval{}, err
	}
	if _, err := model.ParseGuestRef(owner); err != nil {
		return engine.Approval{}, fmt.Errorf("%w: only a guest is approved, named as qemu/101 or lxc/200: %w", engine.ErrInvalid, err)
	}
	s, err := e.stored().Settings()
	if err != nil {
		return engine.Approval{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	guest, current, shown, listed := e.guestToApprove(owner)
	switch {
	case !e.state.Complete:
		return engine.Approval{}, fmt.Errorf("%w: the last cycle did not list every guest, so %s cannot be approved as it is now; "+
			"approve what you can see once a cycle has listed them all", engine.ErrRefused, owner)
	case !listed:
		return engine.Approval{}, fmt.Errorf("%w: %s is not in the last listing of Proxmox; approve what you can see", engine.ErrRefused, owner)
	case current == "":
		return engine.Approval{}, fmt.Errorf("%w: %s has no identity Proxmox reports, and an approval is of one", engine.ErrRefused, owner)
	case identity != "" && identity != current:
		return engine.Approval{}, fmt.Errorf("%w: %s changed since it was shown: it was shown in identity %s and has identity %s now; "+
			"look at it again", engine.ErrRefused, owner, identity, current)
	case !slices.Equal(slices.Sorted(slices.Values(macs)), slices.Sorted(slices.Values(shown))):
		return engine.Approval{}, fmt.Errorf("%w: %s changed since it was shown: it was shown with MACs %v and waits for %v now; look at it again",
			engine.ErrRefused, owner, macs, shown)
	}
	a := engine.Approval{Owner: owner, Guest: guest, Identity: current, Mode: s.Admission, MACs: slices.Sorted(slices.Values(macs)), Addresses: addrs}
	view := engine.ApprovalView{Owner: owner, Guest: guest, Identity: current, Current: current, Matches: true, MACs: a.MACs, Addresses: a.Addresses}
	e.approvals = slices.DeleteFunc(slices.Clone(e.approvals), func(v engine.ApprovalView) bool { return v.Owner == owner })
	e.approvals = append(e.approvals, view)
	slices.SortFunc(e.approvals, func(x, y engine.ApprovalView) int { return model.CompareOwners(x.Owner, y.Owner) })
	e.state.Unapproved = slices.DeleteFunc(slices.Clone(e.state.Unapproved), func(u engine.UnapprovedGuest) bool { return u.String() == owner })
	if held := e.held[owner]; len(held) > 0 {
		e.state.Routes = sortRoutes(append(slices.Clone(e.state.Routes), held...))
		delete(e.held, owner)
	}
	e.setApproval(owner, engine.ApprovalApproved)
	e.adminEvent(ctx, owner, fmt.Sprintf("%s is approved in identity %s", engine.OwnerName(owner, guest), current))
	e.changed()
	return a, nil
}

// guestToApprove is what the state and the guest list show of a guest: its
// name, its identity, the MACs it waits for and whether it is listed. The
// caller holds mu.
func (e *Engine) guestToApprove(owner string) (guest *engine.GuestView, identity string, macs []string, listed bool) {
	ref, _ := model.ParseGuestRef(owner)
	if i := slices.IndexFunc(e.state.Unapproved, func(u engine.UnapprovedGuest) bool { return u.GuestRef == ref }); i >= 0 {
		u := e.state.Unapproved[i]
		return &engine.GuestView{GuestRef: ref, Name: u.Name}, u.Identity, u.MACs, true
	}
	if i := slices.IndexFunc(e.guests, func(g engine.GuestListView) bool { return g.Ref == owner }); i >= 0 {
		g := e.guests[i]
		return &engine.GuestView{GuestRef: ref, Name: g.Name}, g.Identity, nil, true
	}
	return nil, "", nil, false
}

// setApproval sets what the guest list says of the approval of a guest. The
// caller holds mu.
func (e *Engine) setApproval(owner, approval string) {
	if i := slices.IndexFunc(e.guests, func(g engine.GuestListView) bool { return g.Ref == owner }); i >= 0 {
		e.guests = slices.Clone(e.guests)
		e.guests[i].Approval = approval
	}
}

// RevokeGuest removes the approval of a guest. In admission mode approve the
// guest waits for an approval again, as the daemon's next cycle shows it:
// its routes are taken out, and it waits with their hostnames; an approval
// puts them back.
func (e *Engine) RevokeGuest(ctx context.Context, owner string) error {
	if err := e.begin(ctx, "RevokeGuest", struct {
		Owner string `json:"owner"`
	}{owner}); err != nil {
		return err
	}
	ref, err := model.ParseGuestRef(owner)
	if err != nil {
		return fmt.Errorf("%w: only a guest is approved, named as qemu/101 or lxc/200: %w", engine.ErrInvalid, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s, err := e.store.Settings()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(e.approvals, func(v engine.ApprovalView) bool { return v.Owner == owner })
	if i < 0 {
		return fmt.Errorf("%w: %s has no approval", engine.ErrNotFound, owner)
	}
	revoked := e.approvals[i]
	e.approvals = slices.Delete(slices.Clone(e.approvals), i, i+1)
	approval := engine.ApprovalNotNeeded
	if s.Admission == store.AdmissionApprove {
		approval = engine.ApprovalWaiting
		e.waitForApproval(ref, revoked)
	}
	e.setApproval(owner, approval)
	e.adminEvent(ctx, owner, fmt.Sprintf("the approval of %s is revoked", engine.OwnerName(owner, revoked.Guest)))
	e.changed()
	return nil
}

// waitForApproval takes the routes of a guest whose approval was revoked out
// of the state and shows it waiting for an approval with their hostnames.
// The caller holds mu.
func (e *Engine) waitForApproval(ref model.GuestRef, revoked engine.ApprovalView) {
	owner := ref.String()
	var hosts []string
	e.state.Routes = slices.DeleteFunc(slices.Clone(e.state.Routes), func(r engine.RouteView) bool {
		if r.Owner != owner {
			return false
		}
		e.held[owner] = append(e.held[owner], r)
		hosts = append(hosts, r.Hostname)
		return true
	})
	u := engine.UnapprovedGuest{
		GuestView: engine.GuestView{GuestRef: ref}, Identity: cmp.Or(revoked.Current, revoked.Identity),
		Hostnames: nonNil(slices.Compact(slices.Sorted(slices.Values(hosts)))), Why: []string{"admission mode approve"},
	}
	if revoked.Guest != nil {
		u.Name = revoked.Guest.Name
	}
	unapproved := append(slices.DeleteFunc(slices.Clone(e.state.Unapproved), func(w engine.UnapprovedGuest) bool { return w.GuestRef == ref }), u)
	slices.SortFunc(unapproved, func(a, b engine.UnapprovedGuest) int { return model.CompareOwners(a.String(), b.String()) })
	e.state.Unapproved = unapproved
}

// Segments returns the segments of the state.
func (e *Engine) Segments() ([]engine.SegmentView, error) {
	if err := e.begin(context.Background(), "Segments", noArgs); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.state.Segments), nil
}

type segmentArgs struct {
	Bridge string `json:"bridge"`
	VLAN   int    `json:"vlan"`
}

// AcknowledgeSegment marks a segment of the state acknowledged.
func (e *Engine) AcknowledgeSegment(ctx context.Context, bridge string, vlan int) error {
	if err := e.begin(ctx, "AcknowledgeSegment", segmentArgs{bridge, vlan}); err != nil {
		return err
	}
	if err := checkSegment(bridge, vlan); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	segs := slices.Clone(e.state.Segments)
	i := slices.IndexFunc(segs, func(v engine.SegmentView) bool { return v.Bridge == bridge && v.VLAN == vlan })
	switch {
	case i >= 0 && segs[i].Acknowledged:
		return nil
	case i < 0:
		segs = append(segs, engine.SegmentView{Bridge: bridge, VLAN: vlan})
		slices.SortFunc(segs, func(a, b engine.SegmentView) int {
			return cmp.Or(strings.Compare(a.Bridge, b.Bridge), cmp.Compare(a.VLAN, b.VLAN))
		})
		i = slices.IndexFunc(segs, func(v engine.SegmentView) bool { return v.Bridge == bridge && v.VLAN == vlan })
	}
	segs[i].Acknowledged, segs[i].AcknowledgedAt = true, e.now()
	e.state.Segments = segs
	e.adminEvent(ctx, engine.SegmentArg(bridge, vlan), fmt.Sprintf("segment %s is acknowledged; the routes at observed on it are served from the next cycle",
		resolve.Segment{Bridge: bridge, VLAN: vlan}))
	e.changed()
	return nil
}

// RevokeSegment takes the acknowledgement of a segment back.
func (e *Engine) RevokeSegment(ctx context.Context, bridge string, vlan int) error {
	if err := e.begin(ctx, "RevokeSegment", segmentArgs{bridge, vlan}); err != nil {
		return err
	}
	if err := checkSegment(bridge, vlan); err != nil {
		return err
	}
	seg := resolve.Segment{Bridge: bridge, VLAN: vlan}
	e.mu.Lock()
	defer e.mu.Unlock()
	segs := slices.Clone(e.state.Segments)
	i := slices.IndexFunc(segs, func(v engine.SegmentView) bool { return v.Bridge == bridge && v.VLAN == vlan && v.Acknowledged })
	if i < 0 {
		return fmt.Errorf("%w: segment %s is not acknowledged", engine.ErrNotFound, seg)
	}
	if segs[i].Routes == 0 {
		segs = slices.Delete(segs, i, i+1)
	} else {
		segs[i].Acknowledged, segs[i].AcknowledgedAt = false, time.Time{}
	}
	e.state.Segments = segs
	e.adminEvent(ctx, engine.SegmentArg(bridge, vlan), fmt.Sprintf("the acknowledgement of segment %s is revoked; the routes at observed on it are held from the next cycle", seg))
	e.changed()
	return nil
}

func checkSegment(bridge string, vlan int) error {
	switch {
	case bridge == "":
		return fmt.Errorf("%w: name a bridge", engine.ErrInvalid)
	case vlan < 0 || vlan > 4094:
		return fmt.Errorf("%w: VLAN %d: want 0 for untagged, or 1 to 4094", engine.ErrInvalid, vlan)
	}
	return nil
}

// RequestRestart restarts the daemon at once: a new boot, and every stream
// ends.
func (e *Engine) RequestRestart(ctx context.Context) error {
	if err := e.begin(ctx, "RequestRestart", noArgs); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.adminEvent(ctx, "", "a restart of the daemon is asked for: it stops now, and systemd starts it again")
	e.restart()
	return nil
}

// adminEvent records an admin action, with who asked for it. The caller
// holds mu.
func (e *Engine) adminEvent(ctx context.Context, subject, msg string) {
	e.emit(engine.Event{Level: "info", Kind: "admin", Subject: subject, Message: msg, Actor: engine.ActorOf(ctx)})
}
