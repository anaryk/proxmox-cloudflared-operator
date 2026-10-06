package managednet_test

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	legMAC   = "02:50:43:4f:ff:01"
	cloneMAC = "bc:24:11:aa:bb:cc"
	clone    = "lxc/102"
)

// onManaged gives g the card net<index> on the managed network.
func onManaged(g model.Guest, index int, mac string, static ...string) model.Guest {
	nic := model.NIC{Index: index, MAC: mac, Bridge: "pconet"}
	for _, s := range static {
		nic.Static = append(nic.Static, netip.MustParseAddr(s))
	}
	g.NICs = append(g.NICs, nic)
	return g
}

func TestClassify(t *testing.T) {
	ours := store.ManagedMAC(netip.MustParseAddr("10.203.118.5"))
	running := onManaged(guest(101), 1, ours)
	stopped := onManaged(guest(102), 1, cloneMAC, "10.203.118.5")
	stopped.Running = false
	untagged := onManaged(guest(103), 2, "bc:24:11:00:00:03")
	untagged.Tags = nil
	moved := onManaged(guest(104), 2, store.ManagedMAC(netip.MustParseAddr("10.203.118.6")))
	appliance := onManaged(guest(900), 1, legMAC)
	appliance.Ref.Kind = model.KindLXC
	spoofer := onManaged(guest(105), 1, legMAC)
	template := onManaged(guest(106), 1, "bc:24:11:00:00:06")
	template.Template = true
	elsewhere := guest(107) // only a card on vmbr0
	upper := onManaged(guest(108), 1, "06:50:43:4F:76:08")

	attachments := map[string]store.Attachment{
		"lxc/101": {Owner: "lxc/101", NIC: 1, MAC: ours},
		"lxc/104": {Owner: "lxc/104", NIC: 1, MAC: store.ManagedMAC(netip.MustParseAddr("10.203.118.6"))},
		"lxc/108": {Owner: "lxc/108", NIC: 1, MAC: "06:50:43:4f:76:08"},
	}
	self := model.GuestRef{Kind: model.KindLXC, VMID: 900}
	got := managednet.Classify(
		[]model.Guest{untagged, spoofer, running, stopped, moved, appliance, template, elsewhere, upper},
		attachments, self, legMAC)
	require.Equal(t, []managednet.ForeignNIC{
		{Owner: "lxc/102", NIC: 1, MAC: cloneMAC, Addr: netip.MustParseAddr("10.203.118.5")},
		{Owner: "lxc/103", NIC: 2, MAC: "bc:24:11:00:00:03"},
		{Owner: "lxc/104", NIC: 2, MAC: "06:50:43:4f:76:06"},
		{Owner: "lxc/105", NIC: 1, MAC: legMAC},
	}, got)
}

// On the host there is no appliance: every card with the leg's MAC is
// foreign.
func TestClassifyOnTheHostTakesEveryLegMACForForeign(t *testing.T) {
	g := onManaged(guest(101), 1, legMAC)
	got := managednet.Classify([]model.Guest{g}, nil, model.GuestRef{}, legMAC)
	require.Equal(t, []managednet.ForeignNIC{{Owner: "lxc/101", NIC: 1, MAC: legMAC}}, got)
}

// A card with the leg's MAC is foreign even where an attachment names it.
func TestClassifyNeverExplainsTheLegMAC(t *testing.T) {
	g := onManaged(guest(101), 1, legMAC)
	attachments := map[string]store.Attachment{web: {Owner: web, NIC: 1, MAC: legMAC}}
	got := managednet.Classify([]model.Guest{g}, attachments, model.GuestRef{}, legMAC)
	require.Len(t, got, 1)
}

func TestDecideForeign(t *testing.T) {
	foreign := managednet.ForeignNIC{Owner: clone, NIC: 1, MAC: cloneMAC, Addr: netip.MustParseAddr("10.203.118.5")}
	tests := []struct {
		name    string
		tags    []string
		setup   func(*managednet.DecideInput)
		foreign managednet.ForeignNIC
		action  string
		consent string
		why     string
	}{
		{
			name:    "a copied card on a consented guest is replaced by the guest's own",
			tags:    []string{managednet.Tag},
			foreign: foreign, action: managednet.ActionReplace, consent: managednet.ByTag,
			why: "a card on pconet that pco did not attach, as a clone or a restore copies one; pco replaces it with the guest's own",
		},
		{
			name: "a copied card on an automatically consented guest is replaced",
			setup: func(in *managednet.DecideInput) {
				auto(in)
			},
			foreign: foreign, action: managednet.ActionReplace, consent: managednet.ByAuto,
			why: "a card on pconet that pco did not attach, as a clone or a restore copies one; pco replaces it with the guest's own",
		},
		{
			name:    "a copied card on a guest without consent that pco may write gets its link set down",
			foreign: foreign, action: managednet.ActionLinkDown,
			why: "a card on pconet that pco did not attach, as a clone or a restore copies one; pco sets its link down",
		},
		{
			name:    "a copied card on a guest pco may not write is reported",
			setup:   func(in *managednet.DecideInput) { in.Writable[clone] = false },
			foreign: foreign, action: managednet.ActionReport,
			why: "a card on pconet that pco did not attach, as a clone or a restore copies one; pco may not change lxc/102: remove the card",
		},
		{
			name: "a consented guest waiting for the firewall confirmation gets the link of the copy set down",
			tags: []string{managednet.Tag},
			setup: func(in *managednet.DecideInput) {
				in.Appliance = true
				in.Impact[clone] = "net0"
			},
			foreign: foreign, action: managednet.ActionLinkDown,
			why: "a card on pconet that pco did not attach, as a clone or a restore copies one; pco sets its link down",
		},
		{
			name:    "a card with the MAC of pco's leg is foreign",
			foreign: managednet.ForeignNIC{Owner: clone, NIC: 1, MAC: legMAC}, action: managednet.ActionLinkDown,
			why: "a card on pconet with the MAC of pco's leg (02:50:43:4f:ff:01); pco sets its link down",
		},
		{
			name:    "a card with the MAC of pco's leg on a consented guest is replaced too",
			tags:    []string{managednet.Tag},
			foreign: managednet.ForeignNIC{Owner: clone, NIC: 1, MAC: legMAC}, action: managednet.ActionReplace, consent: managednet.ByTag,
			why: "a card on pconet with the MAC of pco's leg (02:50:43:4f:ff:01); pco replaces it with the guest's own",
		},
		{
			name: "a foreign card on a guest with its own card gets its link set down",
			tags: []string{managednet.Tag},
			setup: func(in *managednet.DecideInput) {
				attached(in, clone, managednet.ByTag)
				a := in.Attachments[clone]
				a.NIC = 2
				in.Attachments[clone] = a
			},
			foreign: foreign, action: managednet.ActionLinkDown,
			why: "a card on pconet that pco did not attach, as a clone or a restore copies one; pco sets its link down",
		},
		{
			name: "a copied card on a guest that opted out gets its link set down",
			tags: []string{managednet.Tag},
			setup: func(in *managednet.DecideInput) {
				in.OptOut = []model.GuestRef{{Kind: model.KindLXC, VMID: 102}}
			},
			foreign: foreign, action: managednet.ActionLinkDown,
			why: "a card on pconet that pco did not attach, as a clone or a restore copies one; pco sets its link down",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := onManaged(guest(102, tt.tags...), 1, cloneMAC, "10.203.118.5")
			in := input(g)
			if tt.setup != nil {
				tt.setup(&in)
			}
			in.Foreign = []managednet.ForeignNIC{tt.foreign}
			var found []managednet.Decision
			for _, d := range managednet.Decide(in) {
				if d.Owner == clone && d.MAC == tt.foreign.MAC {
					found = append(found, d)
				}
			}
			require.Len(t, found, 1)
			d := found[0]
			require.Equal(t, tt.action, d.Action)
			require.Equal(t, tt.consent, d.Consent)
			require.Equal(t, tt.why, d.Why)
			require.Equal(t, 1, d.NIC)
			require.Nil(t, d.Lease)
		})
	}
}

// A clone of a managed container in per-guest mode carries the consent tag
// and a copy of the original's card, with the original's address. The copy
// is replaced in one change, and the next cycle finds the clone with a card
// and a lease of its own, while the original keeps its card all along.
func TestDecideCloneInPerGuestMode(t *testing.T) {
	ours := store.ManagedMAC(netip.MustParseAddr("10.203.118.5"))
	original := onManaged(guest(101, managednet.Tag), 1, ours, "10.203.118.5")
	copied := onManaged(guest(102, managednet.Tag), 1, cloneMAC, "10.203.118.5")
	in := input(original, copied)
	attached(&in, web, managednet.ByTag)
	in.Foreign = managednet.Classify(in.Guests, in.Attachments, model.GuestRef{}, legMAC)

	ds := managednet.Decide(in)
	require.Equal(t, managednet.ActionKeep, decision(t, ds, web).Action)
	d := decision(t, ds, clone)
	require.Equal(t, managednet.ActionReplace, d.Action, "the copy is replaced, and no second card is attached beside it")
	require.Equal(t, managednet.ByTag, d.Consent)
	require.Equal(t, 1, d.NIC)
	require.Equal(t, cloneMAC, d.MAC)

	// The cycle replaced the card with one of the clone's own lease.
	own := netip.MustParseAddr("10.203.118.6")
	ownMAC := store.ManagedMAC(own)
	in.Guests[1] = onManaged(guest(102, managednet.Tag), 1, ownMAC, "10.203.118.6")
	in.Attachments[clone] = store.Attachment{Owner: clone, Identity: "mac:bc:24:11:00:00:01", NIC: 1, MAC: ownMAC, Addr: own, Consent: managednet.ByTag, Since: now}
	in.Leases[ownMAC] = store.Lease{MAC: ownMAC, Addr: own, Owner: clone, Identity: "mac:bc:24:11:00:00:01", NIC: 1, Since: now}
	in.Foreign = managednet.Classify(in.Guests, in.Attachments, model.GuestRef{}, legMAC)
	require.Empty(t, in.Foreign)

	ds = managednet.Decide(in)
	require.Equal(t, managednet.ActionKeep, decision(t, ds, web).Action)
	d = decision(t, ds, clone)
	require.Equal(t, managednet.ActionKeep, d.Action)
	require.Equal(t, own, d.Lease.Addr, "the clone has its own lease")
	require.Equal(t, netip.MustParseAddr("10.203.118.5"), decision(t, ds, web).Lease.Addr)
}

// A guest with two copied cards gets one replaced, the lowest, and the link
// of the other set down.
func TestDecideReplacesOneCardOfAGuest(t *testing.T) {
	g := onManaged(onManaged(guest(102, managednet.Tag), 2, "bc:24:11:aa:bb:02"), 1, cloneMAC)
	in := input(g)
	in.Foreign = managednet.Classify(in.Guests, in.Attachments, model.GuestRef{}, legMAC)
	ds := managednet.Decide(in)
	require.Len(t, ds, 2)
	require.Equal(t, managednet.ActionReplace, ds[0].Action)
	require.Equal(t, 1, ds[0].NIC)
	require.Equal(t, managednet.ActionLinkDown, ds[1].Action)
	require.Equal(t, 2, ds[1].NIC)
}

// A foreign card of a guest that is not tagged is decided as well: Decide
// sees it only through Classify.
func TestDecideForeignOfAnUntaggedGuest(t *testing.T) {
	in := input()
	in.Foreign = []managednet.ForeignNIC{{Owner: "qemu/300", NIC: 3, MAC: cloneMAC}}
	in.Writable["qemu/300"] = true
	ds := managednet.Decide(in)
	require.Len(t, ds, 1)
	require.Equal(t, managednet.ActionLinkDown, ds[0].Action)
	require.Equal(t, 3, ds[0].NIC)
}
