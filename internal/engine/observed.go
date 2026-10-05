package engine

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/access"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// cause is why the observed rules hold a route.
type cause int

const (
	causeSegment cause = iota
	causeMAC
	causeDelegated
	causeUnreadable
	causeSoft
	causes // how many there are
)

// causeNames are the words the problem line counts the held routes by.
var causeNames = [causes]string{
	causeSegment:    "segment not acknowledged",
	causeMAC:        "MAC changed",
	causeDelegated:  "delegated guest",
	causeUnreadable: "access control unreadable",
	causeSoft:       "soft-denied address",
}

// observedHold is what the observed rules hold one winner by, and what an
// approval of its guest would record to release it.
type observedHold struct {
	causes  []cause
	segment resolve.Segment // the segment not acknowledged, when causeSegment
	why     []string        // the causes an approval releases, in words
	mac     string          // the MAC an approval would record, when one is asked for
	addr    netip.Addr      // the soft-denied address an approval would allow
}

// holdObserved applies the observed rules to every winner whose guest's
// address is published, after the minimum had its say. A winner proven at
// observed is held when its segment is not acknowledged, or when it waits for
// an approval of its guest: its MAC is not the one its claim pinned, or others
// may configure the guest's network (or that cannot be told), unless the
// approval of the guest in its identity holds the MAC. A winner at any level
// is held when its address is soft-denied, unless that approval names the
// address. A held target loses its address as one below the minimum does,
// the claim is kept, and the guest is listed in Unapproved with why. At
// observed a claim without a MAC pins the one the address answers from; the
// claims changed so are saved again. Above observed nothing is pinned.
func (c *cycleRun) holdObserved() {
	acc := c.access
	d := &delegates{v: acc, ownUser: c.e.d.OwnUser, byGuest: map[model.GuestRef][]access.Principal{}}
	// The access control matters only where routes are served at observed;
	// the subnets and resolvers read with it are denied at any level.
	if c.requiredLevel() == resolve.LevelObserved && acc.problem != "" {
		c.problem("%s", acc.problem)
	}
	if acc.factsErr != "" {
		c.problem("reading the SDN subnets or the resolvers of the nodes: %s; the ones read before stay denied", acc.factsErr)
	}
	seen := map[resolve.Segment]int{}
	waiting := map[model.GuestRef]*UnapprovedGuest{}
	var counts [causes]int
	held := 0
	var pinned []Event
	for _, rt := range c.claims.Winners {
		res, ok := c.results[rt.Hostname]
		if !ok || rt.Guest == nil || res.Binding == nil {
			continue
		}
		level, published := standsOn(res)
		if !published {
			continue
		}
		atObserved := level == resolve.LevelObserved
		if seg := res.Binding.Segment; atObserved && !seg.IsZero() {
			seen[seg]++
		}
		h, pin := c.observedHoldOf(rt, res, d, acc.unreadable, atObserved)
		if pin != nil {
			pinned = append(pinned, *pin)
		}
		if len(h.causes) == 0 {
			continue
		}
		held++
		for _, k := range h.causes {
			counts[k]++
		}
		c.holdWinner(rt, res, h)
		if len(h.why) > 0 {
			c.waitFor(waiting, *rt.Guest, rt.Hostname, h)
		}
	}
	c.st.Segments = c.segmentViews(seen)
	c.listWaiting(waiting)
	if held > 0 {
		c.problem("%s", observedHeld(held, counts))
	}
	c.savePins(pinned)
}

// observedHoldOf says what holds the winner rt, whose result res stands on a
// published proof, at observed when atObserved, and there pins the MAC of its
// address to its claim when the claim has none or an approval lets it change.
// pin is the event of a pin made, to be reported once it is saved.
func (c *cycleRun) observedHoldOf(rt model.Route, res resolve.Result, d *delegates, unreadable, atObserved bool) (h observedHold, pin *Event) {
	ref := *rt.Guest
	b := res.Binding
	approval, approved := c.approvalOf(ref)
	if atObserved {
		if seg := b.Segment; !seg.IsZero() && !c.acknowledged(seg) {
			h.causes, h.segment = append(h.causes, causeSegment), seg
		}
		mac, macErr := model.NormalizeMAC(b.MAC)
		if macErr != nil {
			mac = b.MAC
		}
		macApproved := approved && slices.Contains(approval.MACs, mac)
		claim, claimed := c.claims.Claims[rt.Hostname]
		switch {
		case !claimed || macErr != nil:
		case claim.MAC == "" && !res.Target.Withdrawn:
			claim.MAC = mac
			c.claims.Claims[rt.Hostname] = claim
			pin = c.pinEvent(rt, fmt.Sprintf("pinned MAC %s for %s", mac, rt.Hostname))
		case claim.MAC == "" || claim.MAC == mac:
		case macApproved && !res.Target.Withdrawn:
			old := claim.MAC
			claim.MAC = mac
			c.claims.Claims[rt.Hostname] = claim
			pin = c.pinEvent(rt, fmt.Sprintf("pinned MAC %s for %s in place of %s, as the approval of %s allows", mac, rt.Hostname, old, ref))
		case !macApproved:
			h.causes = append(h.causes, causeMAC)
			h.why = append(h.why, fmt.Sprintf("MAC changed from %s to %s", claim.MAC, mac))
			h.mac = mac
		}
		switch {
		case macApproved:
		case unreadable:
			h.causes = append(h.causes, causeUnreadable)
			h.why = append(h.why, whyUnreadable)
			h.mac = mac
		default:
			if delegated := d.of(ref); len(delegated) > 0 {
				h.causes = append(h.causes, causeDelegated)
				h.why = append(h.why, delegatedWhy(delegated))
				h.mac = mac
			}
		}
	}
	if res.SoftDenied != "" {
		addr := res.Target.Addr
		if !approved || !slices.Contains(approval.Addresses, addr) {
			h.causes = append(h.causes, causeSoft)
			h.why = append(h.why, fmt.Sprintf("address %s is the %s", addr, res.SoftDenied))
			h.addr = addr
		}
	}
	return h, pin
}

// delegatedWhy names the principals a guest is delegated to.
func delegatedWhy(ps []access.Principal) string {
	ids := make([]string, len(ps))
	for i, p := range ps {
		ids[i] = p.ID
	}
	verb := "hold"
	if len(ids) == 1 {
		verb = "holds"
	}
	return fmt.Sprintf("delegated: %s %s VM.Config.Network", strings.Join(ids, ", "), verb)
}

// approvalOf returns the approval of a guest when it is of the identity the
// guest has now.
func (c *cycleRun) approvalOf(ref model.GuestRef) (store.Approval, bool) {
	a, ok := c.approvals[ref.String()]
	g, listed := c.snap.Guest(ref)
	return a, ok && listed && g.Identity != "" && a.Identity == g.Identity
}

func (c *cycleRun) acknowledged(seg resolve.Segment) bool {
	_, ok := c.segments[store.SegmentID(seg.Bridge, seg.VLAN)]
	return ok
}

// holdWinner takes the address from the target of rt, as the minimum does,
// with the reason the first thing to do says.
func (c *cycleRun) holdWinner(rt model.Route, res resolve.Result, h observedHold) {
	reason := approvalReason(h.why, *rt.Guest)
	if slices.Contains(h.causes, causeSegment) {
		reason = SegmentReason(h.segment.Bridge, h.segment.VLAN)
	}
	if res.Target.Withdrawn {
		res.Target.Addr = netip.Addr{}
	} else {
		res.Target = planner.ResolvedTarget{Reason: reason}
	}
	c.results[rt.Hostname] = res
}

// approvalReason is the reason of a route that waits for an approval of its
// guest.
func approvalReason(why []string, ref model.GuestRef) string {
	if len(why) == 1 && strings.HasPrefix(why[0], "MAC changed ") {
		return why[0] + "; approve the guest to accept it"
	}
	return strings.Join(why, "; ") + "; pco guest approve " + ref.String()
}

// waitFor lists the guest of a held route among those that wait for an
// approval, with what an approval would record.
func (c *cycleRun) waitFor(waiting map[model.GuestRef]*UnapprovedGuest, ref model.GuestRef, host string, h observedHold) {
	w := waiting[ref]
	if w == nil {
		w = &UnapprovedGuest{GuestView: *c.guestView(ref), Why: []string{}}
		if g, ok := c.snap.Guest(ref); ok {
			w.Identity = g.Identity
		}
		waiting[ref] = w
	}
	w.Hostnames = append(w.Hostnames, host)
	for _, why := range h.why {
		if !slices.Contains(w.Why, why) {
			w.Why = append(w.Why, why)
		}
	}
	if h.mac != "" && !slices.Contains(w.MACs, h.mac) {
		w.MACs = append(w.MACs, h.mac)
	}
	if h.addr.IsValid() && !slices.Contains(w.Addresses, h.addr) {
		w.Addresses = append(w.Addresses, h.addr)
	}
}

// listWaiting adds the guests the observed rules hold to those that wait for
// admission, in the order of their owners.
func (c *cycleRun) listWaiting(waiting map[model.GuestRef]*UnapprovedGuest) {
	for _, w := range waiting {
		w.Hostnames = slices.Compact(slices.Sorted(slices.Values(w.Hostnames)))
		slices.Sort(w.MACs)
		slices.SortFunc(w.Addresses, netip.Addr.Compare)
		c.st.Unapproved = append(c.st.Unapproved, *w)
	}
	slices.SortStableFunc(c.st.Unapproved, func(a, b UnapprovedGuest) int {
		return model.CompareOwners(a.String(), b.String())
	})
}

// observedHeld says how many routes the observed rules hold, by cause.
func observedHeld(n int, counts [causes]int) string {
	which := "1 route is held"
	if n != 1 {
		which = fmt.Sprintf("%d routes are held", n)
	}
	var parts []string
	for k, count := range counts {
		if count > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", causeNames[k], count))
		}
	}
	return fmt.Sprintf("%s by the observed rules: %s; pco routes says why each is held", which, strings.Join(parts, ", "))
}

// pinEvent is the event of a MAC pinned to the claim of rt.
func (c *cycleRun) pinEvent(rt model.Route, msg string) *Event {
	return &Event{At: c.now, Level: levelInfo, Kind: kindClaim, Subject: rt.Hostname, Route: rt.Hostname, Guest: rt.Guest.String(), Message: msg}
}

// savePins saves the claims again when a MAC was pinned. A pin that cannot be
// saved is made again by the next cycle; until then nothing says it was made.
func (c *cycleRun) savePins(pinned []Event) {
	if len(pinned) == 0 {
		return
	}
	if err := c.e.d.Store.SaveClaims(c.claims.Claims); err != nil {
		c.problem("saving the MACs pinned to the claims: %v", err)
		return
	}
	slices.SortStableFunc(pinned, func(a, b Event) int { return cmp.Compare(a.Subject, b.Subject) })
	c.events = append(c.events, pinned...)
}
