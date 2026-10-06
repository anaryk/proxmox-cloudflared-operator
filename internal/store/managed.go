package store

import (
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// Consent is an admin's approval of a guest, in its identity, for the
// managed network: the second way of the per-guest mode, beside the tag.
type Consent struct {
	Owner    string    `json:"owner"`
	Identity string    `json:"identity"`
	At       time.Time `json:"at"`
	Actor    string    `json:"actor,omitempty"`
	// AcceptFirewallImpact confirms, for this guest, that pco may turn on
	// its firewall although another of its cards has firewall=1, which then
	// filters that card too (appliance only).
	AcceptFirewallImpact bool `json:"acceptFirewallImpact,omitempty"`
}

// Attachment records the NIC pco put on a guest and why.
type Attachment struct {
	Owner    string     `json:"owner"`
	Identity string     `json:"identity"`
	NIC      int        `json:"nic"`
	MAC      string     `json:"mac"`
	Addr     netip.Addr `json:"addr"`
	Consent  string     `json:"consent"` // "tag", "approval", "auto"
	Since    time.Time  `json:"since"`
	// Firewall is what the appliance changed on the guest, to restore on detach.
	Firewall  *FirewallBefore `json:"firewall,omitempty"`
	DetachAt  *time.Time      `json:"detachAt,omitempty"` // the grace runs; zero when consent holds
	DetachWhy string          `json:"detachWhy,omitempty"`
	// RemovedAt is when RemoveNIC was called after the grace; the attachment
	// and its lease go only once the guest's config no longer lists the NIC
	// (QEMU without hotplug keeps it pending until a restart: detach-pending).
	RemovedAt *time.Time `json:"removedAt,omitempty"`
}

// FirewallBefore is the guest firewall as it was before pco turned it on.
type FirewallBefore struct {
	Enabled bool `json:"enabled"`
	Rules   int  `json:"rules"` // how many rules were there before pco's
}

// attachedBy are the values of Attachment.Consent: the consent tag, an
// approval, or automatic consent.
var attachedBy = []string{"tag", "approval", "auto"}

// Consents returns the consents by owner.
func (s *Store) Consents() (map[string]Consent, error) {
	return loadMap(s.cluster, kindConsents, "owner", func(c *Consent) *string { return &c.Owner })
}

// SaveConsent records an admin's consent for the guest c.Owner in the
// identity c.Identity.
func (s *Store) SaveConsent(c Consent) error {
	if _, err := model.ParseGuestRef(c.Owner); err != nil {
		return fmt.Errorf("consent: %w", err)
	}
	switch {
	case c.Identity == "":
		return fmt.Errorf("consent of %s has no identity", c.Owner)
	case c.At.IsZero():
		return fmt.Errorf("consent of %s has no time it was given at", c.Owner)
	}
	return s.cluster.put(kindConsents, c.Owner, c, true)
}

// DeleteConsent removes the consent of an owner. A missing one is not an
// error.
func (s *Store) DeleteConsent(owner string) error { return s.cluster.Delete(kindConsents, owner) }

// Attachments returns the attachments by owner.
func (s *Store) Attachments() (map[string]Attachment, error) {
	return loadMap(s.cluster, kindAttachments, "owner", func(a *Attachment) *string { return &a.Owner })
}

// SaveAttachment stores an attachment under its owner, with its MAC in normal
// form. A MAC that is not the one ManagedMAC gives its address is refused.
func (s *Store) SaveAttachment(a Attachment) error {
	if _, err := model.ParseGuestRef(a.Owner); err != nil {
		return fmt.Errorf("attachment: %w", err)
	}
	mac, err := model.NormalizeMAC(a.MAC)
	if err != nil {
		return fmt.Errorf("attachment of %s: %w", a.Owner, err)
	}
	a.MAC = mac
	switch {
	case a.Identity == "":
		return fmt.Errorf("attachment of %s has no identity", a.Owner)
	case a.NIC < 0 || a.NIC > maxNICIndex:
		return fmt.Errorf("attachment of %s: net%d is not net0 to net%d", a.Owner, a.NIC, maxNICIndex)
	case !a.Addr.Is4():
		return fmt.Errorf("attachment of %s: %v is not an IPv4 address", a.Owner, a.Addr)
	case mac != ManagedMAC(a.Addr):
		return fmt.Errorf("attachment of %s: the card of %s has the MAC %s, not %s", a.Owner, a.Addr, ManagedMAC(a.Addr), mac)
	case !slices.Contains(attachedBy, a.Consent):
		return fmt.Errorf("attachment of %s: consent %q: want tag, approval or auto", a.Owner, a.Consent)
	}
	return s.cluster.put(kindAttachments, a.Owner, a, true)
}

// DeleteAttachment removes the attachment of an owner. A missing one is not
// an error.
func (s *Store) DeleteAttachment(owner string) error { return s.cluster.Delete(kindAttachments, owner) }
