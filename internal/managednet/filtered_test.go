package managednet

import (
	"errors"
	"net/netip"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const filteredComment = "pco:" + installID

// holding is an input under which filtered holds: the iptables backend, the
// datacenter firewall on, pve-firewall running, two attached guests, one
// running and silent to the canary, one stopped.
func holding() FilteredInput {
	card := func(nic int, lease string) FirewallCard {
		return FirewallCard{NIC: nic, Leg: netip.MustParseAddr("10.203.118.1"), Lease: netip.MustParseAddr(lease), Ports: []uint16{443}}
	}
	read := func(c FirewallCard) *GuestFirewall {
		return &GuestFirewall{
			Options: pve.GuestFirewallOptions{Enabled: true},
			Rules:   WantRules(c, filteredComment),
			Set:     &pve.IPSet{Name: IPSetName(c.NIC), Comment: filteredComment},
			Entries: []pve.IPSetEntry{{CIDR: c.Lease.String()}},
		}
	}
	running, stopped := card(1, "10.203.118.2"), card(2, "10.203.118.3")
	return FilteredInput{
		Node:            "pve1",
		ClusterFirewall: pve.ClusterFirewallOptions{Enabled: true},
		Service:         "running",
		Cards: []FilteredCard{
			{Owner: "qemu/9320", Card: running, Firewall: true, Running: true, Read: read(running), Canary: &CanaryResult{Port: 1023, Verdict: CanarySilent}},
			{Owner: "lxc/9321", Card: stopped, Firewall: true, Read: read(stopped)},
		},
	}
}

func TestFilteredHolds(t *testing.T) {
	require.Equal(t, FilteredResult{Holds: true, Offered: true, Backend: BackendIPTables}, Filtered(holding(), filteredComment))
	explicit := holding()
	explicit.NodeFirewall = pve.NodeFirewallOptions{Enabled: true, NFTables: false}
	require.True(t, Filtered(explicit, filteredComment).Holds, "an explicit nftables: 0 is the iptables backend")
	none := holding()
	none.Cards = nil
	require.True(t, Filtered(none, filteredComment).Holds, "a vnet without guests is isolated")
}

func TestTheNFTablesBackendIsNotOfferedWhateverTheServicesSay(t *testing.T) {
	in := holding()
	in.NodeFirewall.NFTables = true
	got := Filtered(in, filteredComment)
	require.Equal(t, FilteredResult{Backend: BackendNFTables, Why: []string{
		"filtered is not supported with the nftables firewall backend (nftables: 1) of node pve1, which installs no guest rules; set nftables: 0 in its firewall options",
	}}, got)
	in.Service = "stopped"
	require.Equal(t, got, Filtered(in, filteredComment))
}

func TestEachConditionFailsFiltered(t *testing.T) {
	boom := errors.New("connection refused")
	for _, c := range []struct {
		name   string
		change func(*FilteredInput)
		why    string
	}{
		{"node options unreadable", func(in *FilteredInput) { in.NodeFirewallErr = boom }, "the firewall options of node pve1 could not be read: connection refused"},
		{"datacenter firewall off", func(in *FilteredInput) { in.ClusterFirewall.Enabled = false }, "the datacenter firewall is off"},
		{"datacenter unreadable", func(in *FilteredInput) { in.ClusterFirewallErr = boom }, "the firewall options of the datacenter could not be read: connection refused"},
		{"pve-firewall dead", func(in *FilteredInput) { in.Service = "stopped" }, "pve-firewall is stopped on node pve1"},
		{"service unreadable", func(in *FilteredInput) { in.ServiceErr = boom }, "the state of pve-firewall on node pve1 could not be read: connection refused"},
		{"a foreign card", func(in *FilteredInput) {
			in.Foreign = []ForeignNIC{{Owner: "lxc/9330", NIC: 1, MAC: "06:50:43:4f:76:02"}}
		}, "guest lxc/9330: net1 (06:50:43:4f:76:02) on the vnet is no card pco attached"},
		{"firewall=0 on the card", func(in *FilteredInput) { in.Cards[0].Firewall = false }, "guest qemu/9320: net1 has firewall=0"},
		{"written a moment ago", func(in *FilteredInput) { in.Cards[0].NotEnforced = "pco wrote its firewall 3s ago" },
			"guest qemu/9320: net1 is not yet enforced: pco wrote its firewall 3s ago"},
		{"a stopped guest's card counts", func(in *FilteredInput) { in.Cards[1].Firewall = false }, "guest lxc/9321: net2 has firewall=0"},
		{"guest firewall off", func(in *FilteredInput) { in.Cards[0].Read.Options.Enabled = false }, "guest qemu/9320: the guest firewall is off"},
		{"macfilter off", func(in *FilteredInput) { in.Cards[0].Read.Options.MACFilterOff = true }, "guest qemu/9320: macfilter is off"},
		{"ipset widened to the subnet", func(in *FilteredInput) {
			in.Cards[0].Read.Entries = []pve.IPSetEntry{{CIDR: "10.203.118.0/24"}}
		}, "guest qemu/9320: the ipset ipfilter-net1 holds 10.203.118.0/24, not exactly 10.203.118.2"},
		{"a rule changed", func(in *FilteredInput) { in.Cards[0].Read.Rules[2].Action = "ACCEPT" }, "guest qemu/9320: pco's rules on net1 were changed"},
		{"unreadable firewall", func(in *FilteredInput) { in.Cards[0].Read, in.Cards[0].ReadErr = nil, boom },
			"guest qemu/9320: its firewall could not be read: connection refused"},
		{"not read", func(in *FilteredInput) { in.Cards[0].Read = nil }, "guest qemu/9320: its firewall was not read in this cycle"},
		{"the canary is reset", func(in *FilteredInput) {
			in.Cards[0].Canary = &CanaryResult{Port: 1023, Verdict: CanaryAnswered, Detail: "the connection was reset"}
		}, "guest qemu/9320: the firewall is not enforcing on net1: the connection was reset on port 1023"},
		{"the canary is answered", func(in *FilteredInput) {
			in.Cards[0].Canary = &CanaryResult{Port: 1023, Verdict: CanaryAnswered, Detail: "the connection was accepted"}
		}, "guest qemu/9320: the firewall is not enforcing on net1: the connection was accepted on port 1023"},
		{"no canary for a running guest", func(in *FilteredInput) { in.Cards[0].Canary = nil }, "guest qemu/9320: no canary was sent to net1"},
		{"the canary not sent", func(in *FilteredInput) {
			in.Cards[0].Canary = &CanaryResult{Port: 1023, Verdict: CanaryNotSent, Detail: "the leg is not up"}
		}, "guest qemu/9320: the canary on net1 could not be sent: the leg is not up"},
		{"the vnet changed", func(in *FilteredInput) { in.Vnet = []string{"isolate-ports is on, and in the appliance it is off"} },
			"the vnet pconet: isolate-ports is on, and in the appliance it is off"},
		{"the attachments unread", func(in *FilteredInput) { in.Unread = []string{"the attachments were not read"} },
			"the attachments were not read"},
		{"an unknown MAC", func(in *FilteredInput) { in.Unknown = []string{"bc:24:11:00:00:01"} },
			"a frame from bc:24:11:00:00:01, a MAC no lease knows, was seen on the vnet"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := holding()
			c.change(&in)
			got := Filtered(in, filteredComment)
			require.False(t, got.Holds)
			require.True(t, got.Offered)
			require.Contains(t, got.Why, c.why)
		})
	}
}

// No canary is asked of a stopped guest: its card has no port, and nothing
// answers.
func TestAStoppedGuestNeedsNoCanary(t *testing.T) {
	in := holding()
	require.Nil(t, in.Cards[1].Canary)
	require.True(t, Filtered(in, filteredComment).Holds)
}

// The predicate sees through Classify what the vnet carries that no
// attachment explains: a copy of a card, a lease's MAC on a second guest, the
// leg's MAC on a guest; the appliance's own leg card is not one.
func TestTheCardsClassifyFindsFailFiltered(t *testing.T) {
	self := model.GuestRef{Kind: model.KindLXC, VMID: 9310}
	legMAC := "02:50:43:4f:ff:01"
	atts := map[string]store.Attachment{"qemu/9320": {Owner: "qemu/9320", NIC: 1, MAC: "06:50:43:4f:76:02"}}
	nic := func(i int, mac string) model.NIC { return model.NIC{Index: i, MAC: mac, Bridge: "pconet"} }
	guests := []model.Guest{
		{Ref: self, NICs: []model.NIC{nic(1, legMAC)}},
		{Ref: model.GuestRef{Kind: model.KindQEMU, VMID: 9320}, NICs: []model.NIC{nic(1, "06:50:43:4f:76:02")}},
	}
	in := holding()
	in.Foreign = Classify(guests, atts, self, legMAC)
	require.True(t, Filtered(in, filteredComment).Holds, "the appliance's own leg card is no foreign card")

	for name, extra := range map[string]model.Guest{
		"a lease MAC on two guests": {Ref: model.GuestRef{Kind: model.KindLXC, VMID: 9331}, NICs: []model.NIC{nic(0, "06:50:43:4f:76:02")}},
		"the leg MAC on a guest":    {Ref: model.GuestRef{Kind: model.KindLXC, VMID: 9332}, NICs: []model.NIC{nic(0, legMAC)}},
		"a stopped copy":            {Ref: model.GuestRef{Kind: model.KindQEMU, VMID: 9333}, NICs: []model.NIC{nic(1, "bc:24:11:aa:bb:cc")}},
	} {
		t.Run(name, func(t *testing.T) {
			in := holding()
			in.Foreign = Classify(append(slices.Clone(guests), extra), atts, self, legMAC)
			got := Filtered(in, filteredComment)
			require.False(t, got.Holds)
			require.Contains(t, got.Why[0], "guest "+extra.Ref.String()+": net")
		})
	}
}
