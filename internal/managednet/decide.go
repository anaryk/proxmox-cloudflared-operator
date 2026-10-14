package managednet

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	optOutWhy     = "cf-tunnel-managed=off"
	unreadableWhy = "registered-tags could not be read"
	noConsentWhy  = "the guest has neither the tag " + Tag + " nor an approval"
	notNeededWhy  = "the guest does not need the managed network"
	noRouteWhy    = "the guest has no valid route"
	foreignWhy    = "a card on " + gateway.ManagedBridge + " that pco did not attach, as a clone or a restore copies one"
)

// NoAgentWhy is why a guest of a node without an admitted agent waits, in a
// cluster: no one writes the guests of that node.
const NoAgentWhy = "node %s has no pco agent"

// verdict is what the consent of a guest says this cycle.
type verdict int

const (
	// noConsent: the guest gets no card, and the card it has goes after the
	// grace.
	noConsent verdict = iota
	// consented: the guest gets a card, or keeps the one it has.
	consented
	// unknown: registered-tags could not be read, and whether the consent
	// holds is not known. Nothing starts and nothing ends.
	unknown
	// stopped: automatic consent does not act while the gate tag is not
	// registered. Nothing attaches, and an automatic attachment stays.
	stopped
	// waiting: the guest would be consented automatically, but for what the
	// reason names.
	waiting
)

// Decide applies the consent rules to the tagged guests of a complete
// snapshot, to the attachments of guests no longer tagged, and to the
// foreign cards found on the managed network. Guests come in owner order,
// then the attachments without a tagged guest, then the foreign cards by
// owner and index.
//
// A guest on another node waits, whatever else holds, unless in a cluster the
// node has an admitted agent, which writes its guests: a node alone attaches
// the guests of its own node only. A guest that migrated keeps its card and
// lease.
// The opt-out marker wins over every consent. Per-guest consent, the
// registered consent tag or an approval of the guest's identity, attaches
// whether or not the guest needs the managed network; automatic consent
// attaches a guest that needs it, has a valid route and, in admission mode
// approve, an admitted identity, and only while the gate tag is registered.
// Whether a guest needs the network is judged before its card is added and
// never again, as the card it then has would answer the question.
//
// An attachment whose consent is gone is detached after the grace, and
// consent that comes back within it ends the grace. Under automatic consent
// an attachment is detached only for the guest itself: the marker, the gate
// tag gone from it, the guest gone, or the mode switched back to per-guest.
// While registered-tags cannot be read no grace starts or ends early.
func Decide(in DecideInput) []Decision {
	d := decider{in: in, optOut: map[string]bool{}}
	for _, ref := range in.OptOut {
		d.optOut[ref.String()] = true
	}
	guests := slices.Clone(in.Guests)
	slices.SortFunc(guests, func(a, b model.Guest) int { return model.CompareOwners(a.Ref.String(), b.Ref.String()) })
	tagged := map[string]bool{}
	var out []Decision
	for _, g := range guests {
		tagged[g.Ref.String()] = true
		out = append(out, d.guest(g))
	}
	for _, owner := range slices.SortedFunc(maps.Keys(in.Attachments), model.CompareOwners) {
		if !tagged[owner] {
			out = append(out, d.untagged(in.Attachments[owner]))
		}
	}
	foreign := d.foreign(out)
	out = slices.DeleteFunc(out, func(dec Decision) bool { return dec.Action == "" })
	return append(out, foreign...)
}

type decider struct {
	in     DecideInput
	optOut map[string]bool
}

func (d *decider) guest(g model.Guest) Decision {
	owner := g.Ref.String()
	dec := Decision{Owner: owner}
	att, attached := d.in.Attachments[owner]
	if attached {
		dec.NIC, dec.Lease = att.NIC, d.lease(att)
	}
	switch {
	case g.Node != d.in.Node && d.in.Agents == nil:
		dec.Action, dec.Why = ActionWait, fmt.Sprintf("guest is on node %s, not %s", g.Node, d.in.Node)
		return dec
	case g.Node != d.in.Node && !d.in.Agents[g.Node]:
		dec.Action, dec.Why = ActionWait, fmt.Sprintf(NoAgentWhy, g.Node)
		return dec
	case d.optOut[owner] && attached:
		return d.detach(dec, att, optOutWhy)
	case d.optOut[owner]:
		dec.Action, dec.Why = ActionNone, optOutWhy
		return dec
	case attached && att.RemovedAt != nil:
		// The card is being removed: consent that comes back now gets a new
		// card once the old one is gone.
		return d.detach(dec, att, att.DetachWhy)
	}

	by, v, why := d.consent(g, attached)
	if attached {
		switch {
		case v == consented && !hasCard(g, att):
			// The card is gone, or another one took its index: it goes back
			// with the lease the attachment has.
			dec.Action, dec.Consent = ActionAttach, by
			return dec
		case v == consented:
			dec.Action, dec.Consent = ActionKeep, by
			return dec
		case v == stopped && att.Consent == ByAuto:
			dec.Action, dec.Consent = ActionKeep, ByAuto
			return dec
		case v == unknown, v == stopped:
			return d.hold(dec, att)
		}
		return d.detach(dec, att, why)
	}
	switch v {
	case consented:
		return d.attach(dec, g, by)
	case noConsent:
		dec.Action, dec.Why = ActionNone, why
	default:
		dec.Action, dec.Consent, dec.Why = ActionWait, by, why
	}
	return dec
}

// consent is what consents to a card for g this cycle, and why it does not
// when it does not.
func (d *decider) consent(g model.Guest, attached bool) (by string, v verdict, why string) {
	owner := g.Ref.String()
	approval, approved := d.in.Consents[owner]
	switch {
	case approved && approval.Identity == g.Identity:
		return ByApproval, consented, ""
	case g.HasTag(Tag) && d.registered(Tag):
		return ByTag, consented, ""
	case g.HasTag(Tag) && d.in.Registered == nil:
		return ByTag, unknown, unreadableWhy
	}
	if d.in.Settings.Consent != store.ConsentAuto {
		switch {
		case approved:
			return "", noConsent, fmt.Sprintf("the approval of %s is for identity %s, and the guest is %s now", owner, approval.Identity, g.Identity)
		case g.HasTag(Tag):
			return "", noConsent, "the tag " + Tag + " is not registered as admin-only"
		case attached && d.in.Attachments[owner].Consent == ByAuto:
			return "", noConsent, "managed.consent is per-guest, and " + noConsentWhy
		}
		return "", noConsent, noConsentWhy
	}

	if !attached {
		switch {
		case !d.in.Needs[owner]:
			return "", noConsent, notNeededWhy
		case d.in.ValidRoutes[owner] < 1:
			return "", noConsent, noRouteWhy
		case d.in.AdmissionMode == store.AdmissionApprove && !d.in.Admitted[owner]:
			return ByAuto, waiting, fmt.Sprintf("the identity of %s is not admitted yet (admission mode approve)", owner)
		}
	}
	switch {
	case d.in.Registered == nil:
		return ByAuto, unknown, unreadableWhy
	case !d.registered(d.in.GateTag):
		return ByAuto, stopped, fmt.Sprintf("automatic attaches are stopped: the gate tag %s is not registered as admin-only", d.in.GateTag)
	}
	return ByAuto, consented, ""
}

func (d *decider) registered(tag string) bool { return slices.Contains(d.in.Registered, tag) }

// hasCard reports whether the guest carries the attachment's card: one at
// its index, on the managed network, with its MAC.
func hasCard(g model.Guest, att store.Attachment) bool {
	nic, ok := g.NIC(att.NIC)
	return ok && nic.Bridge == gateway.ManagedBridge && strings.EqualFold(nic.MAC, att.MAC)
}

// attach decides for a consented guest without a card: it waits while pco
// may not write it, and in the appliance while turning its firewall on would
// filter another of its cards that nobody confirmed.
func (d *decider) attach(dec Decision, g model.Guest, by string) Decision {
	dec.Consent = by
	if !d.in.Writable[dec.Owner] {
		dec.Action, dec.Why = ActionWait, "needs-grant"
		if line := d.in.Grant[dec.Owner]; line != "" {
			dec.Why += ": " + line
		}
		return dec
	}
	card := d.in.Impact[dec.Owner]
	if d.in.Appliance && card != "" && !d.in.AcceptImpact[dec.Owner] && !d.in.Settings.AcceptFirewallImpact {
		dec.Action = ActionWait
		dec.Why = fmt.Sprintf("needs-firewall-confirmation: turning on the firewall of %s also filters %s (firewall=1); "+
			"run pco managed approve %s --accept-firewall-impact, or set managed.acceptFirewallImpact for every guest",
			label(g), card, dec.Owner)
		return dec
	}
	dec.Action = ActionAttach
	return dec
}

// detach lets the grace of an attachment run, from now when none runs yet.
func (d *decider) detach(dec Decision, att store.Attachment, why string) Decision {
	end := d.in.Now.Add(d.grace())
	if att.DetachAt != nil {
		end = *att.DetachAt
	}
	dec.Action, dec.Why, dec.DetachAt = ActionDetach, why, &end
	return dec
}

// hold changes nothing about an attachment whose consent is not known or not
// acting: a grace that runs goes on, and none starts.
func (d *decider) hold(dec Decision, att store.Attachment) Decision {
	if att.DetachAt != nil {
		return d.detach(dec, att, att.DetachWhy)
	}
	dec.Action, dec.Consent = ActionKeep, att.Consent
	return dec
}

// untagged decides for an attachment whose guest is not among the tagged
// guests of the snapshot: it was removed, or it lost the gate tag.
func (d *decider) untagged(att store.Attachment) Decision {
	dec := Decision{Owner: att.Owner, NIC: att.NIC, Lease: d.lease(att)}
	why := fmt.Sprintf("the guest is gone, or no longer carries the tag %s", d.in.GateTag)
	if att.RemovedAt != nil {
		why = att.DetachWhy
	}
	return d.detach(dec, att, why)
}

func (d *decider) grace() time.Duration {
	if g := time.Duration(d.in.Settings.DetachGrace); g > 0 {
		return g
	}
	return time.Duration(store.DefaultSettings().Managed.DetachGrace)
}

func (d *decider) lease(att store.Attachment) *store.Lease {
	l, ok := d.in.Leases[strings.ToLower(att.MAC)]
	if !ok {
		return nil
	}
	return &l
}

// foreign decides for the foreign cards. The lowest foreign card of a guest
// that the cycle would give a card is replaced by that card, and the guest's
// own attach is dropped from guests; for a guest whose card goes back at the
// index of its attachment, only a foreign card at that index is. Any other
// foreign card gets its link set down where pco may write the guest, and is
// reported where it may not.
func (d *decider) foreign(guests []Decision) []Decision {
	attach := map[string]*Decision{}
	for i := range guests {
		if guests[i].Action == ActionAttach {
			attach[guests[i].Owner] = &guests[i]
		}
	}
	cards := slices.Clone(d.in.Foreign)
	slices.SortFunc(cards, func(a, b ForeignNIC) int {
		if c := model.CompareOwners(a.Owner, b.Owner); c != 0 {
			return c
		}
		return a.NIC - b.NIC
	})
	legMAC := gateway.ManagedLegMAC.String()
	var out []Decision
	for _, f := range cards {
		dec := Decision{Owner: f.Owner, NIC: f.NIC, MAC: f.MAC}
		why := foreignWhy
		if strings.EqualFold(f.MAC, legMAC) {
			why = fmt.Sprintf("a card on %s with the MAC of pco's leg (%s)", gateway.ManagedBridge, legMAC)
		}
		_, attached := d.in.Attachments[f.Owner]
		switch g := attach[f.Owner]; {
		case g != nil && (!attached || f.NIC == g.NIC):
			dec.Action, dec.Consent, dec.Why = ActionReplace, g.Consent, why+"; pco replaces it with the guest's own"
			dec.Lease = g.Lease
			g.Action = ""
			delete(attach, f.Owner)
		case d.in.Writable[f.Owner]:
			dec.Action, dec.Why = ActionLinkDown, why+"; pco sets its link down"
		default:
			dec.Action, dec.Why = ActionReport, fmt.Sprintf("%s; pco may not change %s: remove the card", why, f.Owner)
		}
		out = append(out, dec)
	}
	return out
}

// label names a guest in a reason: its owner, and its name when it has one.
func label(g model.Guest) string {
	if g.Name == "" {
		return g.Ref.String()
	}
	return fmt.Sprintf("%s (%s)", g.Ref, g.Name)
}
