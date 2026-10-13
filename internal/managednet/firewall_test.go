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

const installID = "5f0c1a2b"

var (
	fwGuest = model.Guest{Ref: model.GuestRef{Kind: model.KindQEMU, VMID: 9320}, Node: "pve1"}
	fwCard  = FirewallCard{NIC: 1, Leg: netip.MustParseAddr("10.203.118.1"), Lease: netip.MustParseAddr("10.203.118.2"), Ports: []uint16{443, 80}}
)

func TestWantRulesAreTheSixRulesAndAPortEach(t *testing.T) {
	got := WantRules(fwCard, "pco:"+installID)
	var text []string
	for _, r := range got {
		require.Equal(t, "net1", r.Iface, "every rule is scoped to the card")
		require.Equal(t, "pco:"+installID, r.Comment)
		require.True(t, r.Enable)
		text = append(text, ruleText(r))
	}
	require.Equal(t, []string{
		"IN ACCEPT -iface net1 -source 10.203.118.1 -p udp -sport 67 -dport 68",
		"IN ACCEPT -iface net1 -source 10.203.118.1 -p tcp -dport 80",
		"IN ACCEPT -iface net1 -source 10.203.118.1 -p tcp -dport 443",
		"IN DROP -iface net1",
		"OUT ACCEPT -iface net1 -source 10.203.118.2 -dest 10.203.118.1",
		"OUT ACCEPT -iface net1 -p udp -sport 68 -dport 67",
		"OUT DROP -iface net1",
	}, text)

	noPorts := fwCard
	noPorts.Ports = nil
	require.Len(t, WantRules(noPorts, "pco:x"), 5, "without a published port the leg opens nothing")
}

func TestTheCommentIsTheInstallOfPco(t *testing.T) {
	require.Equal(t, "pco:"+installID, FirewallComment(installID))
	require.Equal(t, "pco:"+installID, NewFirewall(newFakeFirewall(), installID).Comment())
}

func TestApplyTurnsAFirewallThatWasOffOnWithAcceptingPolicies(t *testing.T) {
	api := newFakeFirewall()
	fw := NewFirewall(api, installID)
	require.NoError(t, fw.Apply(t.Context(), fwGuest, fwCard))

	g := api.guests[fwGuest.Ref]
	require.Equal(t, []string{
		"create ipfilter-net1",
		"add ipfilter-net1 10.203.118.2",
		"add OUT DROP -iface net1",
		"add OUT ACCEPT -iface net1 -p udp -sport 68 -dport 67",
		"add OUT ACCEPT -iface net1 -source 10.203.118.2 -dest 10.203.118.1",
		"add IN DROP -iface net1",
		"add IN ACCEPT -iface net1 -source 10.203.118.1 -p tcp -dport 443",
		"add IN ACCEPT -iface net1 -source 10.203.118.1 -p tcp -dport 80",
		"add IN ACCEPT -iface net1 -source 10.203.118.1 -p udp -sport 67 -dport 68",
		"options enable=true policy_in=ACCEPT policy_out=ACCEPT",
	}, api.writes, "the ipset and the rules first, the firewall on last; never ipfilter, never macfilter")
	require.Equal(t, []pve.IPSet{{Name: "ipfilter-net1", Comment: "pco:" + installID}}, g.sets)
	require.Equal(t, []pve.IPSetEntry{{CIDR: "10.203.118.2", Comment: "pco:" + installID}}, g.entries["ipfilter-net1"])
	require.False(t, g.opts.IPFilter)
	require.False(t, g.opts.MACFilterOff)

	read, err := fw.Read(t.Context(), fwGuest, 1)
	require.NoError(t, err)
	require.Empty(t, read.Drift(fwCard, fw.Comment()))

	api.writes = nil
	require.NoError(t, fw.Apply(t.Context(), fwGuest, fwCard))
	require.Empty(t, api.writes, "what is as written is not written again")
}

func TestApplyLeavesTheOptionsOfAFirewallThatWasOn(t *testing.T) {
	api := newFakeFirewall()
	api.guest(fwGuest.Ref).opts = pve.GuestFirewallOptions{Enabled: true, PolicyIn: "DROP"}
	fw := NewFirewall(api, installID)
	require.NoError(t, fw.Apply(t.Context(), fwGuest, fwCard))
	for _, w := range api.writes {
		require.NotContains(t, w, "options")
	}
	require.Equal(t, "DROP", api.guests[fwGuest.Ref].opts.PolicyIn)
}

// The guest's other card has firewall=1 and the guest firewall was off: once
// pco turned it on, that card's chain has an accepting policy, no address
// filter and none of pco's rules, so it passes what it passed before.
func TestApplyLeavesTheOtherCardsOfTheGuestAsTheyWere(t *testing.T) {
	api := newFakeFirewall()
	fw := NewFirewall(api, installID)
	require.NoError(t, fw.Apply(t.Context(), fwGuest, fwCard))
	g := api.guests[fwGuest.Ref]

	require.Equal(t, "ACCEPT", g.opts.PolicyIn)
	require.Equal(t, "ACCEPT", g.opts.PolicyOut)
	require.False(t, g.opts.IPFilter, "no guest-wide address filter")
	require.False(t, slices.ContainsFunc(g.sets, func(s pve.IPSet) bool { return s.Name == "ipfilter-net0" }), "no address set for net0")
	for _, r := range g.rules {
		require.False(t, applies(r, "net0"), "rule %s applies to net0", ruleText(r))
	}
	lan := netip.MustParseAddr("192.168.4.50")
	other := netip.MustParseAddr("192.168.4.9")
	require.Equal(t, "ACCEPT", chainVerdict(g, "net0", packet{dir: "in", src: other, dst: lan, proto: "tcp", dport: 22}))
	require.Equal(t, "ACCEPT", chainVerdict(g, "net0", packet{dir: "out", src: lan, dst: other, proto: "udp", sport: 68, dport: 67}))
	require.Equal(t, "ACCEPT", chainVerdict(g, "net0", packet{dir: "out", src: netip.MustParseAddr("192.168.4.77"), dst: other, proto: "tcp", dport: 80}),
		"a DHCP card that has not its address yet is not filtered to one")
}

// What isolates the card: from the leg the DHCP reply and the published
// ports only; from anywhere else nothing; out to the leg from the lease
// only, a DHCP request, and nothing else.
func TestTheRulesIsolateTheCard(t *testing.T) {
	api := newFakeFirewall()
	require.NoError(t, NewFirewall(api, installID).Apply(t.Context(), fwGuest, fwCard))
	g := api.guests[fwGuest.Ref]
	leg, lease := fwCard.Leg, fwCard.Lease
	other := netip.MustParseAddr("10.203.118.3")
	node := netip.MustParseAddr("192.168.4.207")
	zero := netip.MustParseAddr("0.0.0.0")
	bcast := netip.MustParseAddr("255.255.255.255")
	for _, c := range []struct {
		p    packet
		want string
	}{
		{packet{dir: "in", src: leg, dst: lease, proto: "udp", sport: 67, dport: 68}, "ACCEPT"},
		{packet{dir: "in", src: leg, dst: lease, proto: "tcp", dport: 443}, "ACCEPT"},
		{packet{dir: "in", src: leg, dst: lease, proto: "tcp", dport: 22}, "DROP"},
		{packet{dir: "in", src: other, dst: lease, proto: "tcp", dport: 443}, "DROP"},
		{packet{dir: "in", src: other, dst: lease, proto: "udp", sport: 67, dport: 68}, "DROP"},
		{packet{dir: "out", src: lease, dst: leg, proto: "tcp", sport: 443}, "ACCEPT"},
		{packet{dir: "out", src: zero, dst: bcast, proto: "udp", sport: 68, dport: 67}, "ACCEPT"},
		{packet{dir: "out", src: lease, dst: other, proto: "tcp", dport: 22}, "DROP"},
		{packet{dir: "out", src: lease, dst: node, proto: "tcp", dport: 8006}, "DROP"},
		{packet{dir: "out", src: other, dst: leg, proto: "tcp", dport: 80}, "DROP"},
	} {
		require.Equal(t, c.want, chainVerdict(g, "net1", c.p), "%+v", c.p)
	}
}

// A port the guest stops publishing goes, one it starts publishing comes:
// at no point between the writes does the card let in what neither the old
// nor the new rules let in.
func TestChangingThePortsNeverOpensTheCard(t *testing.T) {
	api := newFakeFirewall()
	fw := NewFirewall(api, installID)
	require.NoError(t, fw.Apply(t.Context(), fwGuest, fwCard))
	other := netip.MustParseAddr("10.203.118.3")
	checked := 0
	api.after = func(_ model.GuestRef, g *fakeGuestFirewall) {
		checked++
		require.Equal(t, "DROP", chainVerdict(g, "net1", packet{dir: "in", src: other, dst: fwCard.Lease, proto: "tcp", dport: 443}))
		require.Equal(t, "DROP", chainVerdict(g, "net1", packet{dir: "in", src: fwCard.Leg, dst: fwCard.Lease, proto: "tcp", dport: 22}))
		require.Equal(t, "DROP", chainVerdict(g, "net1", packet{dir: "out", src: fwCard.Lease, dst: other, proto: "tcp", dport: 22}))
	}
	changed := fwCard
	changed.Ports = []uint16{8443}
	require.NoError(t, fw.Apply(t.Context(), fwGuest, changed))
	require.Positive(t, checked)

	g := api.guests[fwGuest.Ref]
	require.Len(t, g.rules, 6, "the old block is gone")
	require.Equal(t, "DROP", chainVerdict(g, "net1", packet{dir: "in", src: fwCard.Leg, dst: fwCard.Lease, proto: "tcp", dport: 443}))
	require.Equal(t, "ACCEPT", chainVerdict(g, "net1", packet{dir: "in", src: fwCard.Leg, dst: fwCard.Lease, proto: "tcp", dport: 8443}))
}

func TestApplyPutsADriftedFirewallRight(t *testing.T) {
	admin := pve.FirewallRule{Type: "in", Action: "ACCEPT", Proto: "tcp", DPort: "22", Enable: true, Comment: "ssh for all"}
	for name, drift := range map[string]func(g *fakeGuestFirewall){
		"the ipset widened": func(g *fakeGuestFirewall) {
			g.entries["ipfilter-net1"] = append(g.entries["ipfilter-net1"], pve.IPSetEntry{CIDR: "10.203.118.0/24"})
		},
		"a rule of pco's changed": func(g *fakeGuestFirewall) { g.rules[1].DPort = "1-65535" },
		"a rule before pco's":     func(g *fakeGuestFirewall) { g.rules = append([]pve.FirewallRule{admin}, g.rules...) },
		"a rule of pco's gone":    func(g *fakeGuestFirewall) { g.rules = slices.Delete(g.rules, 3, 4) },
		"a rule of pco's off":     func(g *fakeGuestFirewall) { g.rules[3].Enable = false },
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeFirewall()
			fw := NewFirewall(api, installID)
			require.NoError(t, fw.Apply(t.Context(), fwGuest, fwCard))
			drift(api.guests[fwGuest.Ref])
			read, err := fw.Read(t.Context(), fwGuest, 1)
			require.NoError(t, err)
			require.NotEmpty(t, read.Drift(fwCard, fw.Comment()))

			require.NoError(t, fw.Apply(t.Context(), fwGuest, fwCard))
			read, err = fw.Read(t.Context(), fwGuest, 1)
			require.NoError(t, err)
			require.Empty(t, read.Drift(fwCard, fw.Comment()))
			g := api.guests[fwGuest.Ref]
			if name == "a rule before pco's" {
				require.True(t, slices.ContainsFunc(g.rules, func(r pve.FirewallRule) bool { return r.Comment == "ssh for all" }), "the admin's rule stays")
			}
		})
	}
}

func TestApplyLeavesAnIPSetThatIsNotPcos(t *testing.T) {
	api := newFakeFirewall()
	g := api.guest(fwGuest.Ref)
	g.sets = []pve.IPSet{{Name: "ipfilter-net1", Comment: "the admin's"}}
	g.entries["ipfilter-net1"] = []pve.IPSetEntry{{CIDR: "10.203.118.0/24"}}
	err := NewFirewall(api, installID).Apply(t.Context(), fwGuest, fwCard)
	require.ErrorIs(t, err, ErrNotOurs)
	require.Empty(t, api.writes)
}

func TestApplyStopsAtAWriteThatFails(t *testing.T) {
	api := newFakeFirewall()
	api.fail["add-rule"] = errors.New("permission denied")
	err := NewFirewall(api, installID).Apply(t.Context(), fwGuest, fwCard)
	require.ErrorContains(t, err, "permission denied")
	require.False(t, api.guests[fwGuest.Ref].opts.Enabled, "the firewall is not turned on without the rules")
}

func TestDriftNamesWhatIsNotAsWritten(t *testing.T) {
	comment := "pco:" + installID
	good := func() GuestFirewall {
		return GuestFirewall{
			Options: pve.GuestFirewallOptions{Enabled: true},
			Rules:   WantRules(fwCard, comment),
			Set:     &pve.IPSet{Name: "ipfilter-net1", Comment: comment},
			Entries: []pve.IPSetEntry{{CIDR: "10.203.118.2"}},
		}
	}
	require.Empty(t, good().Drift(fwCard, comment))
	slash32 := good()
	slash32.Entries = []pve.IPSetEntry{{CIDR: "10.203.118.2/32"}}
	require.Empty(t, slash32.Drift(fwCard, comment), "a /32 is the address")
	elsewhere := good()
	elsewhere.Rules = append([]pve.FirewallRule{{Type: "in", Action: "ACCEPT", Iface: "net0", Enable: true}}, elsewhere.Rules...)
	require.Empty(t, elsewhere.Drift(fwCard, comment), "a rule of another card does not touch this one")
	offAbove := good()
	offAbove.Rules = append([]pve.FirewallRule{{Type: "in", Action: "ACCEPT", Enable: false}}, offAbove.Rules...)
	require.Empty(t, offAbove.Drift(fwCard, comment), "a rule that is off decides nothing")

	for _, c := range []struct {
		name   string
		change func(*GuestFirewall)
		why    string
	}{
		{"firewall off", func(g *GuestFirewall) { g.Options.Enabled = false }, "the guest firewall is off"},
		{"macfilter off", func(g *GuestFirewall) { g.Options.MACFilterOff = true }, "macfilter is off"},
		{"no ipset", func(g *GuestFirewall) { g.Set, g.Entries = nil, nil }, "the ipset ipfilter-net1 is missing"},
		{"an admin's ipset", func(g *GuestFirewall) { g.Set.Comment = "" }, "the ipset ipfilter-net1 is not pco's"},
		{"ipset /24", func(g *GuestFirewall) { g.Entries = []pve.IPSetEntry{{CIDR: "10.203.118.0/24"}} },
			"the ipset ipfilter-net1 holds 10.203.118.0/24, not exactly 10.203.118.2"},
		{"ipset with more", func(g *GuestFirewall) { g.Entries = append(g.Entries, pve.IPSetEntry{CIDR: "10.203.118.1"}) },
			"the ipset ipfilter-net1 holds 10.203.118.2, 10.203.118.1, not exactly 10.203.118.2"},
		{"ipset empty", func(g *GuestFirewall) { g.Entries = nil }, "the ipset ipfilter-net1 holds nothing, not exactly 10.203.118.2"},
		{"ipset nomatch", func(g *GuestFirewall) { g.Entries[0].NoMatch = true }, "the ipset ipfilter-net1 holds !10.203.118.2, not exactly 10.203.118.2"},
		{"rule changed", func(g *GuestFirewall) { g.Rules[3].Action = "ACCEPT" }, "pco's rules on net1 were changed"},
		{"rule added before", func(g *GuestFirewall) {
			g.Rules = append([]pve.FirewallRule{{Type: "in", Action: "ACCEPT", Iface: "net1", Enable: true}}, g.Rules...)
		}, "a rule that is not pco's comes before pco's rules on net1"},
		{"group before", func(g *GuestFirewall) {
			g.Rules = append([]pve.FirewallRule{{Type: "group", Action: "web", Enable: true}}, g.Rules...)
		}, "a rule that is not pco's comes before pco's rules on net1"},
		{"rules missing", func(g *GuestFirewall) { g.Rules = g.Rules[:4] }, "pco's rules on net1 are not all there"},
		{"another install's rules", func(g *GuestFirewall) { g.Rules = WantRules(fwCard, "pco:other") }, "a rule that is not pco's comes before pco's rules on net1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := good()
			g.Rules = slices.Clone(g.Rules)
			g.Entries = slices.Clone(g.Entries)
			set := *g.Set
			g.Set = &set
			c.change(&g)
			require.Contains(t, g.Drift(fwCard, comment), c.why)
		})
	}
}

func TestRestoreTakesAwayWhatPcoWroteAndNothingElse(t *testing.T) {
	api := newFakeFirewall()
	fw := NewFirewall(api, installID)
	g := api.guest(fwGuest.Ref)
	g.opts = pve.GuestFirewallOptions{PolicyIn: "DROP"}
	admin := pve.FirewallRule{Type: "in", Action: "ACCEPT", Iface: "net1", Proto: "tcp", DPort: "22", Enable: true, Comment: "admin"}
	otherInstall := pve.FirewallRule{Type: "in", Action: "DROP", Iface: "net1", Enable: true, Comment: "pco:other"}
	g.rules = []pve.FirewallRule{admin, otherInstall}
	read, err := fw.Read(t.Context(), fwGuest, 1)
	require.NoError(t, err)
	before := fw.Before(read)
	require.Equal(t, store.FirewallBefore{PolicyIn: "DROP", Rules: 2}, before)
	require.NoError(t, fw.Apply(t.Context(), fwGuest, fwCard))

	kept, err := fw.Restore(t.Context(), fwGuest, 1, &before)
	require.NoError(t, err)
	require.Empty(t, kept)
	require.Equal(t, []pve.FirewallRule{admin, otherInstall}, clearPos(g.rules), "only this install's rules go")
	require.Empty(t, g.sets)
	require.False(t, g.opts.Enabled)
	require.Equal(t, "DROP", g.opts.PolicyIn, "the policy it had is back")
	require.Empty(t, g.opts.PolicyOut, "the one it had not is taken back")
}

func clearPos(rules []pve.FirewallRule) []pve.FirewallRule {
	out := slices.Clone(rules)
	for i := range out {
		out[i].Pos = 0
	}
	return out
}

// A firewall that an admin took up after pco turned it on stays on: only
// pco's rules and ipset go.
func TestRestoreLeavesAFirewallAnAdminTookUp(t *testing.T) {
	for name, change := range map[string]func(g *fakeGuestFirewall){
		"a policy changed": func(g *fakeGuestFirewall) { g.opts.PolicyIn = "DROP" },
		"a rule added": func(g *fakeGuestFirewall) {
			g.rules = append(g.rules, pve.FirewallRule{Type: "in", Action: "ACCEPT", Iface: "net0", Proto: "tcp", DPort: "443", Enable: true})
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeFirewall()
			fw := NewFirewall(api, installID)
			read, err := fw.Read(t.Context(), fwGuest, 1)
			require.NoError(t, err)
			before := fw.Before(read)
			require.NoError(t, fw.Apply(t.Context(), fwGuest, fwCard))
			g := api.guests[fwGuest.Ref]
			change(g)

			kept, err := fw.Restore(t.Context(), fwGuest, 1, &before)
			require.NoError(t, err)
			require.NotEmpty(t, kept)
			require.True(t, g.opts.Enabled, "left on")
			require.Empty(t, g.sets)
			for _, r := range g.rules {
				require.NotEqual(t, fw.Comment(), r.Comment)
			}
		})
	}
}

// Proxmox reads a rule back in another form than pco wrote it: the rules
// never match, and every round would add a block. pco's rules stay one
// block, the card stays isolated, and the write says so.
func TestARuleReadBackOtherwiseNeverGrowsTheList(t *testing.T) {
	api := newFakeFirewall()
	api.normalize = func(r pve.FirewallRule) pve.FirewallRule {
		if r.Source != "" {
			r.Source += "/32"
		}
		return r
	}
	fw := NewFirewall(api, installID)
	for range 5 {
		err := fw.Apply(t.Context(), fwGuest, fwCard)
		require.ErrorIs(t, err, ErrRulesDiffer)
		g := api.guests[fwGuest.Ref]
		require.LessOrEqual(t, len(g.rules), 2*len(WantRules(fwCard, fw.Comment())), "at most one old block and the new one during a round")
		require.Len(t, g.rules, len(WantRules(fwCard, fw.Comment())), "one block after it")
		on := *g
		on.opts.Enabled = true
		other := netip.MustParseAddr("10.203.118.3")
		require.Equal(t, "DROP", chainVerdict(&on, "net1", packet{dir: "in", src: other, dst: fwCard.Lease, proto: "tcp", dport: 443}),
			"the block that stays isolates the card once the firewall is on")
	}
	require.False(t, api.guests[fwGuest.Ref].opts.Enabled, "the firewall is not turned on while pco's rules are not as written")
}

// Proxmox puts a new rule last, not first: the new block is not on top, the
// old one stays and keeps the card isolated, and what the new one added
// beyond the old block's drops goes again.
func TestARuleThatDoesNotLandFirstNeverOpensOrGrowsTheList(t *testing.T) {
	api := newFakeFirewall()
	fw := NewFirewall(api, installID)
	admin := pve.FirewallRule{Type: "in", Action: "ACCEPT", Iface: "net0", Proto: "tcp", DPort: "22", Enable: true, Comment: "admin"}
	api.guest(fwGuest.Ref).rules = []pve.FirewallRule{admin}
	require.NoError(t, fw.Apply(t.Context(), fwGuest, fwCard))
	api.appendRules = true
	changed := fwCard
	changed.Ports = []uint16{8443}
	other := netip.MustParseAddr("10.203.118.3")
	api.after = func(_ model.GuestRef, g *fakeGuestFirewall) {
		require.Equal(t, "DROP", chainVerdict(g, "net1", packet{dir: "in", src: other, dst: fwCard.Lease, proto: "tcp", dport: 443}))
		require.Equal(t, "DROP", chainVerdict(g, "net1", packet{dir: "out", src: fwCard.Lease, dst: other, proto: "tcp", dport: 22}))
	}
	for range 3 {
		require.ErrorIs(t, fw.Apply(t.Context(), fwGuest, changed), ErrRulesDiffer)
		require.Len(t, api.guests[fwGuest.Ref].rules, 1+len(WantRules(fwCard, fw.Comment())), "the old block and the admin's rule")
	}
}

func TestRestoreLeavesAFirewallThatWasOnAndAnIPSetThatIsNotPcos(t *testing.T) {
	api := newFakeFirewall()
	fw := NewFirewall(api, installID)
	g := api.guest(fwGuest.Ref)
	g.opts = pve.GuestFirewallOptions{Enabled: true}
	g.sets = []pve.IPSet{{Name: "ipfilter-net1", Comment: "the admin's"}}
	g.entries["ipfilter-net1"] = []pve.IPSetEntry{{CIDR: "192.0.2.1"}}
	g.rules = WantRules(fwCard, fw.Comment())

	kept, err := fw.Restore(t.Context(), fwGuest, 1, &store.FirewallBefore{Enabled: true})
	require.NoError(t, err)
	require.Empty(t, kept)
	require.Empty(t, g.rules)
	require.Len(t, g.sets, 1)
	require.True(t, g.opts.Enabled)
	for _, w := range api.writes {
		require.NotContains(t, w, "options")
	}
}
