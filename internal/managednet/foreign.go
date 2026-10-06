package managednet

import (
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Classify lists, in owner and index order, every card on the managed
// network in the snapshot that is not an attachment's card with that index
// and that MAC: a card of any guest, running or stopped, tagged or not. A
// clone or a restore of a managed guest carries such a card, with a new MAC
// and, on a container, the original's address, which it would answer ARP
// for. A card with the MAC of pco's leg is foreign wherever it is, but for
// the appliance's own leg card, which self names; on the host self is the
// zero ref.
//
// Templates are left out: a template never runs, and each clone made of one
// is classified on its own.
func Classify(guests []model.Guest, attachments map[string]store.Attachment, self model.GuestRef, legMAC string) []ForeignNIC {
	var out []ForeignNIC
	for _, g := range guests {
		if g.Template {
			continue
		}
		owner := g.Ref.String()
		att, attached := attachments[owner]
		for _, nic := range g.NICs {
			if nic.Bridge != gateway.ManagedBridge {
				continue
			}
			isLeg := strings.EqualFold(nic.MAC, legMAC)
			switch {
			case isLeg && self != (model.GuestRef{}) && g.Ref == self:
				continue
			case !isLeg && attached && att.NIC == nic.Index && strings.EqualFold(att.MAC, nic.MAC):
				continue
			}
			f := ForeignNIC{Owner: owner, NIC: nic.Index, MAC: strings.ToLower(nic.MAC)}
			if len(nic.Static) > 0 {
				f.Addr = nic.Static[0]
			}
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b ForeignNIC) int {
		if c := model.CompareOwners(a.Owner, b.Owner); c != 0 {
			return c
		}
		return a.NIC - b.NIC
	})
	return out
}
