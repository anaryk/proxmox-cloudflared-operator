package resolve

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// Attestation is what the node that hosts a guest says its own forwarding
// table shows: the MAC learned on the guest's own port of its bridge.
type Attestation struct {
	Node   string
	MAC    string
	Bridge string
	VLAN   int
	Port   string
	Fresh  bool // as the controller's heartbeat says
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
	res, attested := r.verifyAddress(ctx, route, req)
	return VerifyResult{Result: finished(route, req.Deny, res), Attested: attested}
}

// verifyAddress verifies the address of a guest's route and reports whether
// the level it proved stands on another node's attestation.
func (r *Resolver) verifyAddress(ctx context.Context, route model.Route, req VerifyRequest) (Result, bool) {
	guest, now := req.Guest, r.now()
	c, res, ok := verifiedCandidate(route, guest, req.Addr, req.MACs)
	if !ok {
		return res, false
	}
	prev := req.Prev
	if !prev.appliesTo(route) || prev.Addr != req.Addr || !sameMAC(prev.MAC, c.NIC.MAC) {
		prev = nil
	}
	if !guest.Running && !guest.StatusUnknown {
		return hold(prev, req.Deny, req.Snap, reasonNotRunning, now), false
	}
	a := newAttempt(r, route, guest, req.Snap, req.Deny, prev, now, []Candidate{c})
	a.required = req.Required
	a.cross = &crossCheck{attestation: req.Attestation, covered: req.Covered, expect: normalizeMACs(req.MACs)}
	switch {
	case !guest.Running:
		return a.guestUnknown(), false
	case ctx.Err() != nil:
		return a.cancelled(), false
	}
	res = a.resolve(ctx)
	return res, res.Level == LevelPort && !a.checksFDB()
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
// MAC that answered: the settings offer it, it is fresh, every online node
// is covered, it comes from the node the inventory says runs the guest, and
// it places each MAC on the guest's own port of the card that has it.
func (c *crossCheck) attests(s Settings, guest model.Guest, macs []string, own map[string][]int) bool {
	at := c.attestation
	switch {
	case !s.Attested, at == nil, !at.Fresh, !c.covered:
		return false
	case at.Node == "" || at.Node != guest.Node || at.Node == s.LocalNode:
		return false
	}
	for _, mac := range macs {
		if normalMAC(at.MAC) != mac || !slices.Contains(GuestPorts(guest.Ref.VMID, own[mac]...), at.Port) {
			return false
		}
	}
	return true
}
