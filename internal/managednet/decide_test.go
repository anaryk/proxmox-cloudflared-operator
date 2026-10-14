package managednet_test

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	gate     = "cf-tunnel"
	thisNode = "pve1"
	web      = "lxc/101"
	grant    = "pveum acl modify /vms/101 --users pco@pve --tokens 'pco@pve!vm900' --roles PCOManaged"
)

var now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// guest is a tagged container on this node with one card on vmbr0.
func guest(vmid int, tags ...string) model.Guest {
	ref := model.GuestRef{Kind: model.KindLXC, VMID: vmid}
	return model.Guest{
		Ref:      ref,
		Name:     "web",
		Node:     thisNode,
		Running:  true,
		Tags:     append([]string{gate}, tags...),
		Identity: "mac:bc:24:11:00:00:01",
		NICs:     []model.NIC{{Index: 0, MAC: "bc:24:11:00:00:01", Bridge: "vmbr0", Firewall: true}},
	}
}

// input is a cycle in per-guest mode with both tags registered, in which pco
// may write every guest given.
func input(guests ...model.Guest) managednet.DecideInput {
	in := managednet.DecideInput{
		Now:           now,
		Settings:      store.DefaultSettings().Managed,
		GateTag:       gate,
		Registered:    []string{gate, managednet.Tag},
		Guests:        guests,
		Needs:         map[string]bool{},
		ValidRoutes:   map[string]int{},
		AdmissionMode: store.AdmissionTag,
		Admitted:      map[string]bool{},
		Consents:      map[string]store.Consent{},
		Attachments:   map[string]store.Attachment{},
		Leases:        map[string]store.Lease{},
		Node:          thisNode,
		Writable:      map[string]bool{},
		Grant:         map[string]string{},
		Impact:        map[string]string{},
		AcceptImpact:  map[string]bool{},
	}
	for _, g := range guests {
		in.Writable[g.Ref.String()] = true
	}
	return in
}

// auto switches the input to automatic consent, with every guest needing the
// managed network and holding a valid route.
func auto(in *managednet.DecideInput) {
	in.Settings.Consent = store.ConsentAuto
	for _, g := range in.Guests {
		in.Needs[g.Ref.String()] = true
		in.ValidRoutes[g.Ref.String()] = 1
	}
}

// attached gives the guest owner its card net1 on the managed network, made
// under consent, with its lease; the card is put on the guest unless it has
// a net1 already.
func attached(in *managednet.DecideInput, owner, consent string) {
	addr := netip.MustParseAddr("10.203.118.5")
	mac := store.ManagedMAC(addr)
	in.Attachments[owner] = store.Attachment{
		Owner: owner, Identity: "mac:bc:24:11:00:00:01", NIC: 1, MAC: mac, Addr: addr,
		Consent: consent, Since: now.Add(-time.Hour),
	}
	in.Leases[mac] = store.Lease{MAC: mac, Addr: addr, Owner: owner, Identity: "mac:bc:24:11:00:00:01", NIC: 1, Since: now.Add(-time.Hour)}
	for i, g := range in.Guests {
		if _, taken := g.NIC(1); g.Ref.String() == owner && !taken {
			in.Guests[i] = onManaged(g, 1, mac)
		}
	}
}

// cardless takes the card net1 off lxc/101.
func cardless(in *managednet.DecideInput) {
	for i, g := range in.Guests {
		if g.Ref.String() == web {
			in.Guests[i].NICs = slices.DeleteFunc(slices.Clone(g.NICs), func(n model.NIC) bool { return n.Index == 1 })
		}
	}
}

// grace makes the grace of the attachment of lxc/101 run, ending at end.
func grace(in *managednet.DecideInput, end time.Time, why string) {
	a := in.Attachments[web]
	a.DetachAt, a.DetachWhy = &end, why
	in.Attachments[web] = a
}

// decision returns the one decision for owner that is not about a foreign
// card.
func decision(t *testing.T, ds []managednet.Decision, owner string) managednet.Decision {
	t.Helper()
	var found []managednet.Decision
	for _, d := range ds {
		if d.Owner == owner {
			found = append(found, d)
		}
	}
	require.Len(t, found, 1, "decisions for %s: %+v", owner, ds)
	return found[0]
}

func at(t time.Time) *time.Time { return &t }

func TestDecidePerGuest(t *testing.T) {
	tests := []struct {
		name    string
		guest   model.Guest
		setup   func(*managednet.DecideInput)
		action  string
		consent string
		why     string
	}{
		{
			name:  "the consent tag while it is registered attaches",
			guest: guest(101, managednet.Tag), action: managednet.ActionAttach, consent: managednet.ByTag,
		},
		{
			name:   "the consent tag that is not registered counts for nothing",
			guest:  guest(101, managednet.Tag),
			setup:  func(in *managednet.DecideInput) { in.Registered = []string{gate} },
			action: managednet.ActionNone,
			why:    "the tag cf-tunnel-managed is not registered as admin-only",
		},
		{
			name:  "an approval of the guest's identity attaches",
			guest: guest(101),
			setup: func(in *managednet.DecideInput) {
				in.Consents[web] = store.Consent{Owner: web, Identity: "mac:bc:24:11:00:00:01", At: now}
			},
			action: managednet.ActionAttach, consent: managednet.ByApproval,
		},
		{
			name:  "an approval of another identity counts for nothing",
			guest: guest(101),
			setup: func(in *managednet.DecideInput) {
				in.Consents[web] = store.Consent{Owner: web, Identity: "mac:bc:24:11:00:00:99", At: now}
			},
			action: managednet.ActionNone,
			why:    "the approval of lxc/101 is for identity mac:bc:24:11:00:00:99, and the guest is mac:bc:24:11:00:00:01 now",
		},
		{
			name:   "a guest without the tag or an approval gets nothing",
			guest:  guest(101),
			action: managednet.ActionNone,
			why:    "the guest has neither the tag cf-tunnel-managed nor an approval",
		},
		{
			name:  "per-guest consent attaches a guest that does not need the managed network",
			guest: guest(101, managednet.Tag),
			setup: func(in *managednet.DecideInput) {
				in.Needs[web], in.ValidRoutes[web] = false, 0
			},
			action: managednet.ActionAttach, consent: managednet.ByTag,
		},
		{
			name:   "the tag waits while registered-tags cannot be read",
			guest:  guest(101, managednet.Tag),
			setup:  func(in *managednet.DecideInput) { in.Registered = nil },
			action: managednet.ActionWait, consent: managednet.ByTag,
			why: "registered-tags could not be read",
		},
		{
			name:  "an approval attaches while registered-tags cannot be read",
			guest: guest(101, managednet.Tag),
			setup: func(in *managednet.DecideInput) {
				in.Registered = nil
				in.Consents[web] = store.Consent{Owner: web, Identity: "mac:bc:24:11:00:00:01", At: now}
			},
			action: managednet.ActionAttach, consent: managednet.ByApproval,
		},
		{
			name:   "a guest on another node waits",
			guest:  func() model.Guest { g := guest(101, managednet.Tag); g.Node = "pve2"; return g }(),
			action: managednet.ActionWait,
			why:    "guest is on node pve2, not pve1",
		},
		{
			name:   "in a cluster a guest of a node with an agent is attached",
			guest:  func() model.Guest { g := guest(101, managednet.Tag); g.Node = "pve2"; return g }(),
			setup:  func(in *managednet.DecideInput) { in.Agents = map[string]bool{"pve2": true} },
			action: managednet.ActionAttach, consent: managednet.ByTag,
		},
		{
			name:   "in a cluster a guest of a node without an agent waits",
			guest:  func() model.Guest { g := guest(101, managednet.Tag); g.Node = "pve3"; return g }(),
			setup:  func(in *managednet.DecideInput) { in.Agents = map[string]bool{"pve2": true} },
			action: managednet.ActionWait, why: "node pve3 has no pco agent",
		},
		{
			name:   "the marker wins over the tag",
			guest:  guest(101, managednet.Tag),
			setup:  func(in *managednet.DecideInput) { in.OptOut = []model.GuestRef{{Kind: model.KindLXC, VMID: 101}} },
			action: managednet.ActionNone,
			why:    "cf-tunnel-managed=off",
		},
		{
			name:  "pco may not write the guest: it waits for the grant",
			guest: guest(101, managednet.Tag),
			setup: func(in *managednet.DecideInput) {
				in.Appliance = true
				in.Writable[web] = false
				in.Grant[web] = grant
			},
			action: managednet.ActionWait, consent: managednet.ByTag,
			why: "needs-grant: " + grant,
		},
		{
			name:   "a guest missing from Writable is not written",
			guest:  guest(101, managednet.Tag),
			setup:  func(in *managednet.DecideInput) { delete(in.Writable, web) },
			action: managednet.ActionWait, consent: managednet.ByTag,
			why: "needs-grant",
		},
		{
			name:  "the firewall impact without a confirmation waits, naming the card and both ways out",
			guest: guest(101, managednet.Tag),
			setup: func(in *managednet.DecideInput) {
				in.Appliance = true
				in.Impact[web] = "net0"
			},
			action: managednet.ActionWait, consent: managednet.ByTag,
			why: "needs-firewall-confirmation: turning on the firewall of lxc/101 (web) also filters net0 (firewall=1); " +
				"run pco managed approve lxc/101 --accept-firewall-impact, or set managed.acceptFirewallImpact for every guest",
		},
		{
			name:  "the grant is asked for before the firewall impact",
			guest: guest(101, managednet.Tag),
			setup: func(in *managednet.DecideInput) {
				in.Appliance = true
				in.Impact[web] = "net0"
				in.Writable[web] = false
				in.Grant[web] = grant
			},
			action: managednet.ActionWait, consent: managednet.ByTag,
			why: "needs-grant: " + grant,
		},
		{
			name:  "the guest's own confirmation of the impact attaches",
			guest: guest(101, managednet.Tag),
			setup: func(in *managednet.DecideInput) {
				in.Appliance = true
				in.Impact[web] = "net0"
				in.AcceptImpact[web] = true
			},
			action: managednet.ActionAttach, consent: managednet.ByTag,
		},
		{
			name:  "the admin's standing confirmation of the impact attaches",
			guest: guest(101, managednet.Tag),
			setup: func(in *managednet.DecideInput) {
				in.Appliance = true
				in.Impact[web] = "net0"
				in.Settings.AcceptFirewallImpact = true
			},
			action: managednet.ActionAttach, consent: managednet.ByTag,
		},
		{
			name:   "a guest without the impact needs no confirmation",
			guest:  guest(101, managednet.Tag),
			setup:  func(in *managednet.DecideInput) { in.Appliance = true },
			action: managednet.ActionAttach, consent: managednet.ByTag,
		},
		{
			name:   "a host ignores the impact",
			guest:  guest(101, managednet.Tag),
			setup:  func(in *managednet.DecideInput) { in.Impact[web] = "net0" },
			action: managednet.ActionAttach, consent: managednet.ByTag,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := input(tt.guest)
			if tt.setup != nil {
				tt.setup(&in)
			}
			d := decision(t, managednet.Decide(in), web)
			require.Equal(t, tt.action, d.Action)
			require.Equal(t, tt.consent, d.Consent)
			require.Equal(t, tt.why, d.Why)
			require.Nil(t, d.DetachAt)
		})
	}
}

func TestDecideAuto(t *testing.T) {
	stopped := "automatic attaches are stopped: the gate tag cf-tunnel is not registered as admin-only"
	tests := []struct {
		name    string
		setup   func(*managednet.DecideInput)
		action  string
		consent string
		why     string
	}{
		{
			name:   "a guest that needs the managed network and has a valid route attaches",
			action: managednet.ActionAttach, consent: managednet.ByAuto,
		},
		{
			name:   "a guest that does not need the managed network gets nothing",
			setup:  func(in *managednet.DecideInput) { in.Needs[web] = false },
			action: managednet.ActionNone,
			why:    "the guest does not need the managed network",
		},
		{
			name:   "a guest without a valid route gets nothing",
			setup:  func(in *managednet.DecideInput) { in.ValidRoutes[web] = 0 },
			action: managednet.ActionNone,
			why:    "the guest has no valid route",
		},
		{
			name:   "in admission mode approve a guest whose identity is not admitted waits",
			setup:  func(in *managednet.DecideInput) { in.AdmissionMode = store.AdmissionApprove },
			action: managednet.ActionWait, consent: managednet.ByAuto,
			why: "the identity of lxc/101 is not admitted yet (admission mode approve)",
		},
		{
			name: "in admission mode approve an admitted guest attaches",
			setup: func(in *managednet.DecideInput) {
				in.AdmissionMode = store.AdmissionApprove
				in.Admitted[web] = true
			},
			action: managednet.ActionAttach, consent: managednet.ByAuto,
		},
		{
			name:   "nothing attaches while the gate tag is not registered",
			setup:  func(in *managednet.DecideInput) { in.Registered = []string{managednet.Tag} },
			action: managednet.ActionWait, consent: managednet.ByAuto,
			why: stopped,
		},
		{
			name:   "nothing attaches while registered-tags cannot be read",
			setup:  func(in *managednet.DecideInput) { in.Registered = nil },
			action: managednet.ActionWait, consent: managednet.ByAuto,
			why: "registered-tags could not be read",
		},
		{
			name: "a guest that would not qualify does not wait for the registration",
			setup: func(in *managednet.DecideInput) {
				in.Registered = []string{managednet.Tag}
				in.Needs[web] = false
			},
			action: managednet.ActionNone,
			why:    "the guest does not need the managed network",
		},
		{
			name: "the consent tag still attaches while the gate tag is not registered",
			setup: func(in *managednet.DecideInput) {
				in.Registered = []string{managednet.Tag}
				in.Guests[0].Tags = append(in.Guests[0].Tags, managednet.Tag)
			},
			action: managednet.ActionAttach, consent: managednet.ByTag,
		},
		{
			name:   "the marker wins over automatic consent",
			setup:  func(in *managednet.DecideInput) { in.OptOut = []model.GuestRef{{Kind: model.KindLXC, VMID: 101}} },
			action: managednet.ActionNone,
			why:    "cf-tunnel-managed=off",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := input(guest(101))
			auto(&in)
			if tt.setup != nil {
				tt.setup(&in)
			}
			d := decision(t, managednet.Decide(in), web)
			require.Equal(t, tt.action, d.Action)
			require.Equal(t, tt.consent, d.Consent)
			require.Equal(t, tt.why, d.Why)
			require.Nil(t, d.DetachAt)
		})
	}
}

func TestDecideAttached(t *testing.T) {
	end := now.Add(10 * time.Minute)
	tests := []struct {
		name     string
		guest    model.Guest
		setup    func(*managednet.DecideInput)
		action   string
		consent  string
		why      string
		detachAt *time.Time
	}{
		{
			name:   "consent that holds keeps the card",
			guest:  guest(101, managednet.Tag),
			setup:  func(in *managednet.DecideInput) { attached(in, web, managednet.ByTag) },
			action: managednet.ActionKeep, consent: managednet.ByTag,
		},
		{
			name:  "the card gone from the guest goes back at its index",
			guest: guest(101, managednet.Tag),
			setup: func(in *managednet.DecideInput) {
				attached(in, web, managednet.ByTag)
				cardless(in)
			},
			action: managednet.ActionAttach, consent: managednet.ByTag,
		},
		{
			name:  "another MAC at the index of the card is replaced by the guest's own",
			guest: onManaged(guest(101), 1, cloneMAC),
			setup: func(in *managednet.DecideInput) {
				attached(in, web, managednet.ByTag)
				in.Consents[web] = store.Consent{Owner: web, Identity: "mac:bc:24:11:00:00:01", At: now}
			},
			action: managednet.ActionAttach, consent: managednet.ByApproval,
		},
		{
			name: "the card on another bridge at the index goes back",
			guest: func() model.Guest {
				g := guest(101, managednet.Tag)
				g.NICs = append(g.NICs, model.NIC{Index: 1, MAC: "bc:24:11:00:00:09", Bridge: "vmbr1"})
				return g
			}(),
			setup:  func(in *managednet.DecideInput) { attached(in, web, managednet.ByTag) },
			action: managednet.ActionAttach, consent: managednet.ByTag,
		},
		{
			name:  "the card gone while the grace runs goes back once consent is back",
			guest: guest(101, managednet.Tag),
			setup: func(in *managednet.DecideInput) {
				attached(in, web, managednet.ByTag)
				cardless(in)
				grace(in, now.Add(3*time.Minute), "the guest has neither the tag cf-tunnel-managed nor an approval")
			},
			action: managednet.ActionAttach, consent: managednet.ByTag,
		},
		{
			name:  "the card gone from a guest whose consent is gone is not put back",
			guest: guest(101),
			setup: func(in *managednet.DecideInput) {
				attached(in, web, managednet.ByTag)
				cardless(in)
			},
			action: managednet.ActionDetach,
			why:    "the guest has neither the tag cf-tunnel-managed nor an approval", detachAt: &end,
		},
		{
			name:  "the card gone while registered-tags cannot be read is not put back",
			guest: guest(101, managednet.Tag),
			setup: func(in *managednet.DecideInput) {
				attached(in, web, managednet.ByTag)
				cardless(in)
				in.Registered = nil
			},
			action: managednet.ActionKeep, consent: managednet.ByTag,
		},
		{
			name:  "the card gone from an automatic attachment is not put back while the gate tag is not registered",
			guest: guest(101),
			setup: func(in *managednet.DecideInput) {
				auto(in)
				attached(in, web, managednet.ByAuto)
				cardless(in)
				in.Registered = []string{}
			},
			action: managednet.ActionKeep, consent: managednet.ByAuto,
		},
		{
			name:   "the tag removed starts the grace",
			guest:  guest(101),
			setup:  func(in *managednet.DecideInput) { attached(in, web, managednet.ByTag) },
			action: managednet.ActionDetach,
			why:    "the guest has neither the tag cf-tunnel-managed nor an approval", detachAt: &end,
		},
		{
			name:  "the tag no longer registered starts the grace",
			guest: guest(101, managednet.Tag),
			setup: func(in *managednet.DecideInput) {
				attached(in, web, managednet.ByTag)
				in.Registered = []string{gate}
			},
			action: managednet.ActionDetach,
			why:    "the tag cf-tunnel-managed is not registered as admin-only", detachAt: &end,
		},
		{
			name:   "the approval revoked starts the grace",
			guest:  guest(101),
			setup:  func(in *managednet.DecideInput) { attached(in, web, managednet.ByApproval) },
			action: managednet.ActionDetach,
			why:    "the guest has neither the tag cf-tunnel-managed nor an approval", detachAt: &end,
		},
		{
			name:  "an identity that changed since the approval starts the grace",
			guest: guest(101),
			setup: func(in *managednet.DecideInput) {
				attached(in, web, managednet.ByApproval)
				in.Consents[web] = store.Consent{Owner: web, Identity: "mac:bc:24:11:00:00:99", At: now}
			},
			action:   managednet.ActionDetach,
			why:      "the approval of lxc/101 is for identity mac:bc:24:11:00:00:99, and the guest is mac:bc:24:11:00:00:01 now",
			detachAt: &end,
		},
		{
			name:  "the mode switched to per-guest starts the grace of an automatic attachment",
			guest: guest(101),
			setup: func(in *managednet.DecideInput) {
				attached(in, web, managednet.ByAuto)
				in.Needs[web], in.ValidRoutes[web] = true, 1
			},
			action:   managednet.ActionDetach,
			why:      "managed.consent is per-guest, and the guest has neither the tag cf-tunnel-managed nor an approval",
			detachAt: &end,
		},
		{
			name:   "the mode switched to per-guest keeps a guest with the tag",
			guest:  guest(101, managednet.Tag),
			setup:  func(in *managednet.DecideInput) { attached(in, web, managednet.ByAuto) },
			action: managednet.ActionKeep, consent: managednet.ByTag,
		},
		{
			name:  "the marker starts the grace",
			guest: guest(101, managednet.Tag),
			setup: func(in *managednet.DecideInput) {
				attached(in, web, managednet.ByTag)
				in.OptOut = []model.GuestRef{{Kind: model.KindLXC, VMID: 101}}
			},
			action: managednet.ActionDetach, why: "cf-tunnel-managed=off", detachAt: &end,
		},
		{
			name:  "the marker starts the grace under automatic consent",
			guest: guest(101),
			setup: func(in *managednet.DecideInput) {
				auto(in)
				attached(in, web, managednet.ByAuto)
				in.OptOut = []model.GuestRef{{Kind: model.KindLXC, VMID: 101}}
			},
			action: managednet.ActionDetach, why: "cf-tunnel-managed=off", detachAt: &end,
		},
		{
			name:  "consent that comes back within the grace cancels it",
			guest: guest(101, managednet.Tag),
			setup: func(in *managednet.DecideInput) {
				attached(in, web, managednet.ByTag)
				grace(in, now.Add(3*time.Minute), "the guest has neither the tag cf-tunnel-managed nor an approval")
			},
			action: managednet.ActionKeep, consent: managednet.ByTag,
		},
		{
			name:  "a grace that runs keeps its end",
			guest: guest(101),
			setup: func(in *managednet.DecideInput) {
				attached(in, web, managednet.ByTag)
				grace(in, now.Add(3*time.Minute), "the tag cf-tunnel-managed is not registered as admin-only")
			},
			action: managednet.ActionDetach,
			why:    "the guest has neither the tag cf-tunnel-managed nor an approval", detachAt: at(now.Add(3 * time.Minute)),
		},
		{
			name:  "a grace that ended removes the card",
			guest: guest(101),
			setup: func(in *managednet.DecideInput) {
				attached(in, web, managednet.ByTag)
				grace(in, now.Add(-time.Second), "the guest has neither the tag cf-tunnel-managed nor an approval")
			},
			action: managednet.ActionDetach,
			why:    "the guest has neither the tag cf-tunnel-managed nor an approval", detachAt: at(now.Add(-time.Second)),
		},
		{
			name:  "a card being removed goes on being removed whatever the consent",
			guest: guest(101, managednet.Tag),
			setup: func(in *managednet.DecideInput) {
				attached(in, web, managednet.ByTag)
				grace(in, now.Add(-time.Minute), "cf-tunnel-managed=off")
				a := in.Attachments[web]
				a.RemovedAt = at(now.Add(-time.Minute))
				in.Attachments[web] = a
			},
			action: managednet.ActionDetach, why: "cf-tunnel-managed=off", detachAt: at(now.Add(-time.Minute)),
		},
		{
			name:  "the tag kept while registered-tags cannot be read keeps the card",
			guest: guest(101, managednet.Tag),
			setup: func(in *managednet.DecideInput) {
				attached(in, web, managednet.ByTag)
				in.Registered = nil
			},
			action: managednet.ActionKeep, consent: managednet.ByTag,
		},
		{
			name:   "an attached guest on another node waits and keeps its card",
			guest:  func() model.Guest { g := guest(101, managednet.Tag); g.Node = "pve2"; return g }(),
			setup:  func(in *managednet.DecideInput) { attached(in, web, managednet.ByTag) },
			action: managednet.ActionWait, why: "guest is on node pve2, not pve1",
		},
		{
			name:  "an automatic attachment is kept although the guest no longer needs the network",
			guest: guest(101),
			setup: func(in *managednet.DecideInput) {
				auto(in)
				attached(in, web, managednet.ByAuto)
				in.Needs[web], in.ValidRoutes[web] = false, 0
				in.AdmissionMode = store.AdmissionApprove
			},
			action: managednet.ActionKeep, consent: managednet.ByAuto,
		},
		{
			name:  "an automatic attachment is kept while the gate tag is not registered",
			guest: guest(101),
			setup: func(in *managednet.DecideInput) {
				auto(in)
				attached(in, web, managednet.ByAuto)
				in.Registered = []string{}
			},
			action: managednet.ActionKeep, consent: managednet.ByAuto,
		},
		{
			name:  "an automatic attachment is kept while registered-tags cannot be read",
			guest: guest(101),
			setup: func(in *managednet.DecideInput) {
				auto(in)
				attached(in, web, managednet.ByAuto)
				in.Registered = nil
			},
			action: managednet.ActionKeep, consent: managednet.ByAuto,
		},
		{
			name:  "an unreadable registered-tags does not cancel a grace that runs",
			guest: guest(101),
			setup: func(in *managednet.DecideInput) {
				auto(in)
				attached(in, web, managednet.ByTag)
				grace(in, now.Add(5*time.Minute), "the guest has neither the tag cf-tunnel-managed nor an approval")
				in.Registered = nil
			},
			action: managednet.ActionDetach,
			why:    "the guest has neither the tag cf-tunnel-managed nor an approval", detachAt: at(now.Add(5 * time.Minute)),
		},
		{
			name:  "under automatic consent the consent tag removed keeps the card",
			guest: guest(101),
			setup: func(in *managednet.DecideInput) {
				auto(in)
				attached(in, web, managednet.ByTag)
			},
			action: managednet.ActionKeep, consent: managednet.ByAuto,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := input(tt.guest)
			tt.setup(&in)
			d := decision(t, managednet.Decide(in), web)
			require.Equal(t, tt.action, d.Action)
			require.Equal(t, tt.consent, d.Consent)
			require.Equal(t, tt.why, d.Why)
			require.Equal(t, tt.detachAt, d.DetachAt)
			require.Equal(t, 1, d.NIC)
			require.NotNil(t, d.Lease, "the decision carries the attachment's lease")
			require.Equal(t, in.Attachments[web].MAC, d.Lease.MAC)
		})
	}
}

// An attached guest whose card is gone gets it back where it was, and a
// foreign card at that index is replaced by it in one change: the decision
// carries the lease the attachment holds. A foreign card at another index is
// no candidate for that, and gets its link set down.
func TestDecideHealsAnAttachmentWhoseCardIsGone(t *testing.T) {
	in := input(guest(101, managednet.Tag))
	attached(&in, web, managednet.ByTag)
	cardless(&in)
	in.Guests[0] = onManaged(in.Guests[0], 2, "bc:24:11:aa:bb:02")
	in.Foreign = managednet.Classify(in.Guests, in.Attachments, model.GuestRef{}, legMAC)
	require.Len(t, in.Foreign, 1)

	ds := managednet.Decide(in)
	require.Len(t, ds, 2)
	require.Equal(t, managednet.ActionAttach, ds[0].Action)
	require.Equal(t, 1, ds[0].NIC, "the card goes back at the index of the attachment")
	require.Equal(t, in.Attachments[web].MAC, ds[0].Lease.MAC, "with the lease the attachment has")
	require.Nil(t, ds[0].DetachAt)
	require.Equal(t, managednet.ActionLinkDown, ds[1].Action)
	require.Equal(t, 2, ds[1].NIC)

	in.Guests[0] = onManaged(guest(101, managednet.Tag), 1, cloneMAC)
	in.Foreign = managednet.Classify(in.Guests, in.Attachments, model.GuestRef{}, legMAC)
	ds = managednet.Decide(in)
	require.Len(t, ds, 1)
	require.Equal(t, managednet.ActionReplace, ds[0].Action, "the copy at the index is replaced, with no second attach")
	require.Equal(t, 1, ds[0].NIC)
	require.Equal(t, cloneMAC, ds[0].MAC)
	require.Equal(t, managednet.ByTag, ds[0].Consent)
	require.Equal(t, in.Attachments[web].MAC, ds[0].Lease.MAC, "the replacement takes the lease the attachment has")
}

func TestDecideStartsTheGraceOfAGuestThatIsGone(t *testing.T) {
	in := input()
	attached(&in, web, managednet.ByTag)
	d := decision(t, managednet.Decide(in), web)
	require.Equal(t, managednet.ActionDetach, d.Action)
	require.Equal(t, "the guest is gone, or no longer carries the tag cf-tunnel", d.Why)
	require.Equal(t, at(now.Add(10*time.Minute)), d.DetachAt)
}

func TestDecideTakesTheGraceOfTheSettings(t *testing.T) {
	in := input(guest(101))
	in.Settings.DetachGrace = store.Duration(2 * time.Minute)
	attached(&in, web, managednet.ByTag)
	d := decision(t, managednet.Decide(in), web)
	require.Equal(t, at(now.Add(2*time.Minute)), d.DetachAt)
}

// apply is what the cycle does with the decisions for attached guests: a
// keep ends a grace, a detach records its end.
func apply(in *managednet.DecideInput, ds []managednet.Decision) {
	for _, d := range ds {
		a, ok := in.Attachments[d.Owner]
		if !ok {
			continue
		}
		switch d.Action {
		case managednet.ActionKeep:
			a.DetachAt, a.DetachWhy = nil, ""
		case managednet.ActionDetach:
			a.DetachAt, a.DetachWhy = d.DetachAt, d.Why
		}
		in.Attachments[d.Owner] = a
	}
}

// The admin clearing registered-tags for a moment, or the option being
// unreadable for a while, must not take the cards of automatic attachments:
// no grace starts, nothing new attaches, and the cards stay when it is back.
func TestDecideAutoOptionClearedForThreeCyclesAndBack(t *testing.T) {
	for _, cleared := range [][]string{{}, nil} {
		in := input(guest(101), guest(102))
		auto(&in)
		attached(&in, web, managednet.ByAuto)
		const other = "lxc/102"
		for cycle := range 5 {
			in.Now = now.Add(time.Duration(cycle) * time.Minute)
			in.Registered = []string{gate, managednet.Tag}
			if cycle >= 1 && cycle <= 3 {
				in.Registered = cleared
			}
			ds := managednet.Decide(in)
			apply(&in, ds)
			require.Nil(t, in.Attachments[web].DetachAt, "cycle %d: no grace", cycle)
			require.Equal(t, managednet.ActionKeep, decision(t, ds, web).Action, "cycle %d", cycle)
			if cycle >= 1 && cycle <= 3 {
				require.Equal(t, managednet.ActionWait, decision(t, ds, other).Action, "cycle %d: nothing attaches", cycle)
			} else {
				require.Equal(t, managednet.ActionAttach, decision(t, ds, other).Action, "cycle %d", cycle)
			}
		}
	}
}

func TestDecideListsGuestsInOwnerOrder(t *testing.T) {
	a, b := guest(102, managednet.Tag), guest(101, managednet.Tag)
	b.Ref.Kind = model.KindQEMU
	in := input(a, b)
	attached(&in, "lxc/103", managednet.ByTag)
	var owners []string
	for _, d := range managednet.Decide(in) {
		owners = append(owners, d.Owner)
	}
	require.Equal(t, []string{"qemu/101", "lxc/102", "lxc/103"}, owners)
}
