package resolve

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// An attestation stands while the attesting node's status was at most
// maxStatusAge old at the controller when it sent the heartbeat that carries
// it, and that heartbeat is at most maxHeartbeatAge old here. While no
// controller has a newer word, it goes on standing for an address that
// already stood on it (see crossCheck.holdsOver).
const (
	maxStatusAge    = 30 * time.Second
	maxHeartbeatAge = 15 * time.Second
)

// Attestation is what the node that hosts a guest says its own forwarding
// table shows: the MAC learned on the guest's own port of its bridge. It
// carries times rather than a verdict, so that whether it stands is decided
// here, against this node's clock.
type Attestation struct {
	Node string
	MAC  string
	// Bridge and VLAN are where the attesting node learned the MAC; empty
	// when it does not say.
	Bridge string
	VLAN   int
	Port   string
	// StatusAge is how old the attesting node's last status was at the
	// controller when it sent the heartbeat that carries this attestation.
	StatusAge time.Duration
	// HeardAt is when this node received that heartbeat.
	HeardAt time.Time
	// Recalled says that StatusAge and HeardAt are of an earlier heartbeat
	// than the last one this node received, which did not name Node, as
	// the first ones of a controller that took over before that node
	// reported to it. Such times can keep an address on the attestation it
	// already stood on, never prove one.
	Recalled bool
	// StableSince is since when the attesting node has seen the MAC on the
	// port without a move.
	StableSince time.Time
}

// VerifyRequest is an address another node chose for a route, to be proven
// on this one.
type VerifyRequest struct {
	Route model.Route
	Guest model.Guest
	Addr  netip.Addr
	// Port is the port dialled; zero takes the route's.
	Port uint16
	// MACs are the MACs the address was chosen with; only they may answer
	// for it. Empty allows every MAC of the guest's cards on the segment.
	MACs []string
	Snap inventory.Snapshot
	Deny Denylist
	// Required is the least level the caller serves at, as for Resolve.
	Required Level
	// Attestation is the hosting node's, nil when there is none.
	Attestation *Attestation
	// Covered says that every online node runs a forwarding-table watch, so
	// that a guest answering in another's name would be seen.
	Covered bool
	Prev    *Binding
}

// VerifyResult is the outcome of VerifyAddress.
type VerifyResult struct {
	Result
	Attested bool // the level is port through another node's attestation
	// Stale says that the attestation was held over a silence of the
	// controller for longer than MaxProofAge: the level stands, but nothing
	// has confirmed the attesting node's word for that long.
	Stale bool
}

// VerifyAddress proves addr for a guest whose address another node chose:
// the denylist, ARP consistency, the forwarding table (own port for a local
// guest; uplink and no local guest port for a guest of another node, port
// only with a fresh attestation and coverage), the dial.
//
// Only addr is tried, on the card that has one of MACs, and a binding of the
// route to another address or card is not carried over. The binding is kept,
// held and withdrawn as Resolve does with it. A route without a guest is
// checked as Resolve checks it.
func (r *Resolver) VerifyAddress(ctx context.Context, req VerifyRequest) VerifyResult {
	route := req.Route
	if req.Port != 0 {
		route.Target.Port = req.Port
	}
	if route.Guest == nil {
		return VerifyResult{Result: r.Resolve(ctx, route, req.Snap, nil, req.Deny, req.Required, nil)}
	}
	res, attested, stale := r.verifyAddress(ctx, route, req)
	return VerifyResult{Result: finished(route, req.Deny, res), Attested: attested, Stale: stale}
}

// verifyAddress verifies the address of a guest's route and reports whether
// the level it proved stands on another node's attestation, and whether that
// attestation was held over for longer than MaxProofAge.
func (r *Resolver) verifyAddress(ctx context.Context, route model.Route, req VerifyRequest) (Result, bool, bool) {
	guest, now := req.Guest, r.now()
	c, res, ok := verifiedCandidate(route, guest, req.Addr, req.MACs)
	if !ok {
		return res, false, false
	}
	prev := req.Prev
	if !prev.appliesTo(route) || prev.Addr != req.Addr || !sameMAC(prev.MAC, c.NIC.MAC) {
		prev = nil
	}
	if !guest.Running && !guest.StatusUnknown {
		return hold(prev, req.Deny, req.Snap, reasonNotRunning, now), false, false
	}
	a := newAttempt(r, route, guest, req.Snap, req.Deny, prev, now, []Candidate{c})
	a.required = req.Required
	a.cross = &crossCheck{attestation: req.Attestation, covered: req.Covered, expect: normalizeMACs(req.MACs), stood: stoodAtPort(prev)}
	switch {
	case !guest.Running:
		return a.guestUnknown(), false, false
	case ctx.Err() != nil:
		return a.cancelled(), false, false
	}
	res = a.resolve(ctx)
	attested := res.Level == LevelPort && !a.checksFDB()
	stale := attested && a.cross.heldOver && now.Sub(req.Attestation.HeardAt) > r.settings.MaxProofAge
	return res, attested, stale
}

// stoodAtPort returns b when it was served at port and placed its MACs on
// a bridge: the address it binds stood on a proof an attestation may go on
// holding up.
func stoodAtPort(b *Binding) *Binding {
	if b == nil || b.Withdrawn || b.Level != LevelPort || b.Bridge == "" || len(b.Ports) == 0 {
		return nil
	}
	return b
}

// verifiedCandidate returns the candidate of the route at addr on a card
// that has one of macs, or the result that says why there is none.
func verifiedCandidate(route model.Route, guest model.Guest, addr netip.Addr, macs []string) (Candidate, Result, bool) {
	cands, err := Candidates(route, guest)
	if err != nil {
		return Candidate{}, Result{Target: planner.ResolvedTarget{Rejected: true, Reason: err.Error()}}, false
	}
	expect := normalizeMACs(macs)
	atAddr := false
	for _, c := range cands {
		if c.Addr != addr {
			continue
		}
		atAddr = true
		if len(expect) == 0 || slices.Contains(expect, normalMAC(c.NIC.MAC)) {
			return c, Result{}, true
		}
	}
	reason := fmt.Sprintf("%s is not an address of the guest", addr)
	if atAddr {
		reason = fmt.Sprintf("no card of the guest has the MAC %s for %s", strings.Join(expect, " or "), addr)
	}
	return Candidate{}, Result{Target: planner.ResolvedTarget{Reason: reason}}, false
}

// crossCheck is what an attempt that verifies another node's choice knows
// beyond Resolve.
type crossCheck struct {
	attestation *Attestation
	covered     bool
	expect      []string // the MACs the address was chosen with, normalized
	// stood is the binding of the address when it was served at port, which
	// an attestation no controller refreshed may hold up; nil otherwise.
	stood *Binding
	// heldOver says that the last attestation that stood did so through
	// holdsOver, not fresh.
	heldOver bool
}

// expected fails when a MAC answered for addr that the address was not
// chosen with. A nil c expects nothing in particular.
func (c *crossCheck) expected(addr netip.Addr, macs []string) outcome {
	if c == nil || len(c.expect) == 0 {
		return outcome{}
	}
	for _, mac := range macs {
		if !slices.Contains(c.expect, mac) {
			return lost("%s answered by %s, not by the MACs the address was chosen with", addr, mac)
		}
	}
	return outcome{}
}

// attests reports whether the hosting node's attestation stands for every
// MAC that answered: the settings offer it, it is fresh at now, or held over
// a silence of the controller, and the MAC has stayed on its port for
// MaxProofAge, every online node is covered, it comes from the node the
// inventory says runs the guest, on the card's bridge and VLAN when it names
// them, and it places each MAC on the guest's own port of the card that has
// it.
func (c *crossCheck) attests(s Settings, now time.Time, guest model.Guest, nic model.NIC, macs []string, own map[string][]int) bool {
	at := c.attestation
	c.heldOver = false
	switch {
	case !s.Attested, at == nil, !c.covered:
		return false
	case at.Node == "" || at.Node != guest.Node || at.Node == s.LocalNode:
		return false
	case !at.stable(now, s.MaxProofAge) || !at.onCard(nic):
		return false
	case at.fresh(now):
	case c.holdsOver(now):
		c.heldOver = true
	default:
		return false
	}
	// An empty MAC, or one that does not parse, would be compared as it is
	// written and could match another such string: it attests nothing.
	attested, err := model.NormalizeMAC(at.MAC)
	if err != nil {
		return false
	}
	for _, mac := range macs {
		if attested != mac || !slices.Contains(GuestPorts(guest.Ref.VMID, own[mac]...), at.Port) {
			return false
		}
	}
	return true
}

// fresh reports whether the attesting node's status and the heartbeat that
// carried it are recent enough at now. A heartbeat dated after now says
// nothing about its age, and a recalled one is not the last.
func (at *Attestation) fresh(now time.Time) bool {
	heard := now.Sub(at.HeardAt)
	return !at.Recalled && at.heard(now) && heard <= maxHeartbeatAge
}

// heard reports whether a heartbeat received by now said that the attesting
// node's status was recent enough at the controller.
func (at *Attestation) heard(now time.Time) bool {
	return !at.HeardAt.IsZero() && !now.Before(at.HeardAt) && at.StatusAge >= 0 && at.StatusAge <= maxStatusAge
}

// holdsOver reports whether an attestation that is not fresh at now still
// stands, because no controller has said anything newer of the attesting
// node: its last heartbeat is too old, the controller being gone, or the
// last one does not name the node, a controller that took over not having
// heard it yet. The attesting node's status must have been recent in the
// last heartbeat that named it, and the address must have stood at port on
// it before: a silence keeps a level, it never raises one or admits an
// address. What the attestation says is checked as when it is fresh, and the
// wire must show what it showed (onUplink).
func (c *crossCheck) holdsOver(now time.Time) bool {
	return c.stood != nil && c.attestation.heard(now)
}

// placedAsStood reports whether the forwarding table placed every MAC where
// the proof the binding that stood on the attestation made found it: an
// attestation held over a silence keeps an address only while nothing on
// the wire has changed.
func (c *crossCheck) placedAsStood(bridge string, placed map[string]string) bool {
	b := c.stood
	if b == nil || b.Bridge != bridge || len(placed) != len(b.Ports) {
		return false
	}
	for mac, port := range placed {
		if b.Ports[mac] != port {
			return false
		}
	}
	return true
}

// stable reports whether the MAC has stayed on its port for at least
// minAge at now, so that a guest answering in another's name for a moment
// is not taken for it.
func (at *Attestation) stable(now time.Time, minAge time.Duration) bool {
	return !at.StableSince.IsZero() && now.Sub(at.StableSince) >= minAge
}

// onCard reports whether the bridge and VLAN the attestation names, if any,
// are the card's: as the card names them, or as the bridge Proxmox makes
// for the VLAN of a bridge that does not filter VLANs.
func (at *Attestation) onCard(nic model.NIC) bool {
	switch {
	case at.Bridge == "" && at.VLAN == 0:
		return true
	case at.Bridge == nic.Bridge && at.VLAN == nic.VLAN:
		return true
	}
	return nic.VLAN != 0 && at.Bridge == vlanBridge(nic) && at.VLAN == 0
}
