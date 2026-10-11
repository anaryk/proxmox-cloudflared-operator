// Package managednet decides which guests pco puts on its managed network,
// gives them addresses, and changes their network cards. Decide and Classify
// are pure: values in, decisions out. The configurers are what writes a
// guest's configuration: as root with qm and pct on the host, through the
// Proxmox API under the admin's grant in the appliance.
package managednet

import (
	"net/netip"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Tag is the per-guest consent tag. It counts only while it is registered as
// admin-only, as only an admin can put such a tag on a guest.
const Tag = "cf-tunnel-managed"

// The actions of a Decision.
const (
	// ActionAttach puts a card on the managed network on the guest.
	ActionAttach = "attach"
	// ActionKeep keeps the card the guest has, and ends a grace that runs.
	ActionKeep = "keep"
	// ActionDetach lets the grace run, and removes the card once DetachAt
	// has passed.
	ActionDetach = "detach-after-grace"
	// ActionWait changes nothing: the guest waits for what Why names. A card
	// it has stays, and so does a grace that runs.
	ActionWait = "wait"
	// ActionNone: the guest gets no card.
	ActionNone = "none"
	// ActionReplace removes a foreign card and puts the guest's own in its
	// place, in one change, with a new lease.
	ActionReplace = "replace-foreign"
	// ActionLinkDown sets link_down=1 on a foreign card.
	ActionLinkDown = "link-down"
	// ActionReport only reports a foreign card.
	ActionReport = "report-foreign"
)

// What a guest's card was given by, as Decision.Consent and
// store.Attachment.Consent say it.
const (
	ByTag      = "tag"
	ByApproval = "approval"
	ByAuto     = "auto"
)

// Decision is what a cycle decides for one tagged guest, for one attachment
// whose guest is no longer tagged, or for one foreign card.
type Decision struct {
	Owner  string
	Action string
	// Consent is what consents this cycle: ByTag, ByApproval, ByAuto, or
	// empty. It is not what the attachment was made under, and on a keep it
	// never overwrites store.Attachment.Consent, which records that.
	Consent string
	// Why is the reason the state shows: for a wait, a detach, a foreign
	// card, and for none.
	Why string
	// NIC is the index of the guest's card, for an attachment and for a
	// foreign card. For an attach it is set only when an attachment exists
	// whose card is gone from the guest: the card goes back at that index.
	NIC int
	MAC string // the foreign card's, for the foreign actions
	// Lease is the lease of the attachment's card, when there is one.
	Lease *store.Lease
	// DetachAt is when the card goes, for ActionDetach: the time a grace
	// that runs already ends at, or the end of a grace that starts now. The
	// card is removed once the clock is not before it.
	DetachAt *time.Time
}

// DecideInput is what Decide reads, all of it from one complete snapshot and
// what the cycle read with it.
type DecideInput struct {
	Now      time.Time
	Settings store.ManagedSettings
	GateTag  string
	// Appliance says that pco runs in the appliance, where it turns the
	// guest firewall on and so needs the firewall-impact confirmation; a
	// host ignores Impact and AcceptImpact.
	Appliance bool
	// Registered are the registered-tags as read this cycle; nil when they
	// could not be read.
	Registered []string
	Guests     []model.Guest    // tagged, not templates
	OptOut     []model.GuestRef // the tagged guests whose Notes carry cf-tunnel-managed=off
	// Needs, by owner: none of the guest's cards that are not on the managed
	// network is on a segment pco reaches, or a route says via=managed.
	Needs map[string]bool
	// ValidRoutes, by owner: the routes that parsed and are not rejected.
	ValidRoutes   map[string]int
	AdmissionMode string          // the settings' admission mode
	Admitted      map[string]bool // by owner: the identity is admitted (mode approve)
	Consents      map[string]store.Consent
	Attachments   map[string]store.Attachment
	Leases        map[string]store.Lease // by MAC
	Foreign       []ForeignNIC           // Classify's answer
	Node          string                 // the node pco runs on
	// Writable, by owner: Configurer.CanWrite, for every tagged guest and
	// every guest with a foreign card. A guest missing here is not written.
	Writable map[string]bool
	// Grant, by owner: the reason CanWrite gave when it said no, which in
	// the appliance is the pveum line that grants pco the guest.
	Grant map[string]string
	// Impact names, by owner, the card of the guest that turning its
	// firewall on would newly filter: another card with firewall=1 while the
	// guest firewall is off. Empty when there is none. Appliance only.
	Impact map[string]string
	// AcceptImpact is the per-guest confirmation of that impact
	// (store.Consent.AcceptFirewallImpact); Settings.AcceptFirewallImpact is
	// the admin's standing one for every guest.
	AcceptImpact map[string]bool
}

// ForeignNIC is a card on the managed network that no attachment explains.
type ForeignNIC struct {
	Owner string
	NIC   int
	MAC   string
	// Addr is the address of an LXC card's ip=, when it has one. A clone or
	// a restore carries the address of the original's lease there.
	Addr netip.Addr
}
