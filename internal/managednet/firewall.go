package managednet

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// FirewallCommentPrefix starts the comment of every rule and ipset pco puts
// in a guest's firewall, pco:<install id>. pco changes and removes what
// carries the comment of its own install, and nothing else.
const FirewallCommentPrefix = "pco:"

// FirewallComment is the comment of the rules and the ipset of an install.
func FirewallComment(installID string) string { return FirewallCommentPrefix + installID }

// The policy pco gives a guest firewall it turns on: the guest's other cards
// pass what they passed before.
const policyAccept = "ACCEPT"

// FirewallCard is a card on the managed network whose firewall pco writes in
// the appliance: its index, the leg's address, the lease's, and the TCP ports
// the leg may open on it, those the guest's routes publish.
type FirewallCard struct {
	NIC   int
	Leg   netip.Addr
	Lease netip.Addr
	Ports []uint16
}

// Iface is the name the firewall gives the card, net<N>.
func (c FirewallCard) Iface() string { return "net" + strconv.Itoa(c.NIC) }

// IPSetName is the ipset that filters the source addresses of the card
// net<N> whether or not the guest-wide ipfilter option is on.
func IPSetName(nic int) string { return "ipfilter-net" + strconv.Itoa(nic) }

// WantRules are the rules pco writes on the card, in their order: DHCP
// replies from the leg (conntrack does not match a broadcast DHCP answer),
// the leg to each published port, nothing else in; out, the guest's own
// address to the leg and a DHCP request, nothing else. The guest reaches the
// leg and the DHCP server through the card, and nothing else, not even the
// node's bridge device.
//
// The DHCP request is not bound to the lease's address: a DISCOVER comes from
// 0.0.0.0, and when the guest's dhcp option is on, pve-firewall lets DHCP out
// in a prelude that runs before its MAC and address filters. What answers it
// is pco's DHCP server alone, which drops a frame whose Ethernet source is
// not the client's chaddr.
func WantRules(card FirewallCard, comment string) []pve.FirewallRule {
	iface, leg := card.Iface(), card.Leg.String()
	rule := func(typ, action string) pve.FirewallRule {
		return pve.FirewallRule{Type: typ, Action: action, Iface: iface, Comment: comment, Enable: true}
	}
	dhcpIn := rule("in", "ACCEPT")
	dhcpIn.Source, dhcpIn.Proto, dhcpIn.SPort, dhcpIn.DPort = leg, "udp", "67", "68"
	out := []pve.FirewallRule{dhcpIn}
	for _, port := range slices.Compact(slices.Sorted(slices.Values(card.Ports))) {
		r := rule("in", "ACCEPT")
		r.Source, r.Proto, r.DPort = leg, "tcp", strconv.Itoa(int(port))
		out = append(out, r)
	}
	toLeg := rule("out", "ACCEPT")
	toLeg.Source, toLeg.Dest = card.Lease.String(), leg
	dhcpOut := rule("out", "ACCEPT")
	dhcpOut.Proto, dhcpOut.SPort, dhcpOut.DPort = "udp", "68", "67"
	return append(out, rule("in", "DROP"), toLeg, dhcpOut, rule("out", "DROP"))
}

// GuestFirewall is what was read of a guest's firewall for one card.
type GuestFirewall struct {
	Options pve.GuestFirewallOptions
	Rules   []pve.FirewallRule
	// RulesDigest is the digest the rules were read in.
	RulesDigest string
	// Set is the card's ipset, nil when there is none; Entries are its
	// entries and EntriesDigest the digest they were read in.
	Set           *pve.IPSet
	Entries       []pve.IPSetEntry
	EntriesDigest string
}

// Drift says how the guest's firewall differs from what isolates the card as
// pco writes it; nothing when it does not. The firewall must be on, macfilter
// not off, the card's ipset pco's and holding exactly the lease, and pco's
// rules the first that apply to the card, as written and in order.
func (g GuestFirewall) Drift(card FirewallCard, comment string) []string {
	var why []string
	if !g.Options.Enabled {
		why = append(why, "the guest firewall is off")
	}
	if g.Options.MACFilterOff {
		why = append(why, "macfilter is off")
	}
	why = append(why, g.setDrift(card, comment)...)
	if d := rulesDrift(g.Rules, WantRules(card, comment)); d != "" {
		why = append(why, d)
	}
	return why
}

func (g GuestFirewall) setDrift(card FirewallCard, comment string) []string {
	name := IPSetName(card.NIC)
	switch {
	case g.Set == nil:
		return []string{"the ipset " + name + " is missing"}
	case g.Set.Comment != comment:
		return []string{"the ipset " + name + " is not pco's"}
	}
	if !exactly(g.Entries, card.Lease) {
		return []string{fmt.Sprintf("the ipset %s holds %s, not exactly %s", name, entriesText(g.Entries), card.Lease)}
	}
	return nil
}

// exactly says that the entries are the one address, as Proxmox spells a
// single address or as a /32.
func exactly(entries []pve.IPSetEntry, addr netip.Addr) bool {
	return len(entries) == 1 && !entries[0].NoMatch && entryIs(entries[0], addr)
}

func entryIs(e pve.IPSetEntry, addr netip.Addr) bool {
	if a, err := netip.ParseAddr(e.CIDR); err == nil {
		return a == addr
	}
	p, err := netip.ParsePrefix(e.CIDR)
	return err == nil && p.Bits() == addr.BitLen() && p.Addr() == addr
}

func entriesText(entries []pve.IPSetEntry) string {
	if len(entries) == 0 {
		return "nothing"
	}
	out := ""
	for i, e := range entries {
		if i > 0 {
			out += ", "
		}
		if e.NoMatch {
			out += "!"
		}
		out += e.CIDR
	}
	return out
}

// applies says that a rule takes part in deciding the card's traffic: it is
// on, and it names the card or no card at all.
func applies(r pve.FirewallRule, iface string) bool {
	return r.Enable && (r.Iface == "" || r.Iface == iface)
}

// rulesDrift says how the rules that apply to the card differ from want:
// want must be the first of them, as written and in order.
func rulesDrift(rules, want []pve.FirewallRule) string {
	iface := want[0].Iface
	var first []pve.FirewallRule
	for _, r := range rules {
		if applies(r, iface) {
			first = append(first, r)
		}
		if len(first) == len(want) {
			break
		}
	}
	for i, w := range want {
		switch {
		case i >= len(first):
			return fmt.Sprintf("pco's rules on %s are not all there", iface)
		case !sameRule(first[i], w) && first[i].Comment == w.Comment:
			return fmt.Sprintf("pco's rules on %s were changed", iface)
		case !sameRule(first[i], w):
			return fmt.Sprintf("a rule that is not pco's comes before pco's rules on %s", iface)
		}
	}
	return ""
}

// sameRule compares two rules on what they match and do.
func sameRule(a, b pve.FirewallRule) bool {
	a.Pos, b.Pos = 0, 0
	return a == b
}

// ours says that a rule is one this install put on the card.
func ours(r pve.FirewallRule, iface, comment string) bool {
	return r.Iface == iface && r.Comment == comment
}

// FirewallAPI is how the appliance reads and changes the firewall of a
// guest; *pve.Client is one.
type FirewallAPI interface {
	GuestFirewallOptions(ctx context.Context, node string, ref model.GuestRef) (pve.GuestFirewallOptions, error)
	SetGuestFirewallOptions(ctx context.Context, node string, ref model.GuestRef, change pve.GuestFirewallChange, digest string) error
	GuestFirewallRules(ctx context.Context, node string, ref model.GuestRef) ([]pve.FirewallRule, string, error)
	AddGuestFirewallRule(ctx context.Context, node string, ref model.GuestRef, rule pve.FirewallRule, digest string) error
	DeleteGuestFirewallRule(ctx context.Context, node string, ref model.GuestRef, pos int, digest string) error
	GuestIPSets(ctx context.Context, node string, ref model.GuestRef) ([]pve.IPSet, error)
	GuestIPSet(ctx context.Context, node string, ref model.GuestRef, name string) ([]pve.IPSetEntry, string, error)
	CreateGuestIPSet(ctx context.Context, node string, ref model.GuestRef, name, comment string) error
	AddGuestIPSetEntry(ctx context.Context, node string, ref model.GuestRef, name, cidr, comment string) error
	DeleteGuestIPSetEntry(ctx context.Context, node string, ref model.GuestRef, name, cidr, digest string) error
	DeleteGuestIPSet(ctx context.Context, node string, ref model.GuestRef, name string) error
}

var _ FirewallAPI = (*pve.Client)(nil)

// ErrNotOurs is the error of a write that would change an object of the
// guest's firewall that pco did not make.
var ErrNotOurs = errors.New("it is not pco's")

// Firewall writes what isolates a guest's card on the managed network in the
// appliance, and takes it away again, under the comment of one install.
type Firewall struct {
	api     FirewallAPI
	comment string
}

// NewFirewall returns the writer of the install's rules and ipsets.
func NewFirewall(api FirewallAPI, installID string) *Firewall {
	return &Firewall{api: api, comment: FirewallComment(installID)}
}

// Comment is the comment of the rules and ipsets this writer makes.
func (f *Firewall) Comment() string { return f.comment }

// Read reads the guest's firewall for the card net<nic>: its options, its
// rules, and the card's ipset with its entries.
func (f *Firewall) Read(ctx context.Context, g model.Guest, nic int) (GuestFirewall, error) {
	var out GuestFirewall
	var err error
	if out.Options, err = f.api.GuestFirewallOptions(ctx, g.Node, g.Ref); err != nil {
		return GuestFirewall{}, err
	}
	if out.Rules, out.RulesDigest, err = f.api.GuestFirewallRules(ctx, g.Node, g.Ref); err != nil {
		return GuestFirewall{}, err
	}
	set, err := f.set(ctx, g, nic)
	if err != nil || set == nil {
		return out, err
	}
	out.Set = set
	if out.Entries, out.EntriesDigest, err = f.api.GuestIPSet(ctx, g.Node, g.Ref, set.Name); err != nil {
		return GuestFirewall{}, err
	}
	return out, nil
}

func (f *Firewall) set(ctx context.Context, g model.Guest, nic int) (*pve.IPSet, error) {
	sets, err := f.api.GuestIPSets(ctx, g.Node, g.Ref)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(sets, func(s pve.IPSet) bool { return s.Name == IPSetName(nic) })
	if i < 0 {
		return nil, nil
	}
	return &sets[i], nil
}

// Before is what pco records of the guest firewall before it first writes
// to it, which Restore puts back: whether it was on and, when it was off,
// the policies it had set, which pco's turning it on replaces; and how many
// rules that are on and not this install's it had, by which Restore tells
// whether an admin has taken the firewall up since.
func (f *Firewall) Before(read GuestFirewall) store.FirewallBefore {
	b := store.FirewallBefore{Enabled: read.Options.Enabled, Rules: f.othersOn(read.Rules)}
	if !b.Enabled {
		b.PolicyIn, b.PolicyOut = read.Options.PolicyIn, read.Options.PolicyOut
	}
	return b
}

// othersOn counts the rules that are on and not this install's.
func (f *Firewall) othersOn(rules []pve.FirewallRule) int {
	n := 0
	for _, r := range rules {
		if r.Enable && r.Comment != f.comment {
			n++
		}
	}
	return n
}

// Apply writes what isolates the card: the card's ipset holding exactly the
// lease, pco's rules first on the card, and the guest firewall turned on with
// the accepting policies when it is off. A guest firewall that is on keeps its
// options. Each step leaves the card no less isolated than before it: the
// ipset gets the lease before anything else leaves it, a new block of rules
// goes on top before the old one goes, and the firewall is turned on last.
// An ipset of the card that pco did not make is ErrNotOurs, and nothing is
// written then.
func (f *Firewall) Apply(ctx context.Context, g model.Guest, card FirewallCard) error {
	if err := f.Keep(ctx, g, card); err != nil {
		return err
	}
	o, err := f.api.GuestFirewallOptions(ctx, g.Node, g.Ref)
	if err != nil {
		return err
	}
	if o.Enabled {
		return nil
	}
	on := true
	change := pve.GuestFirewallChange{Enable: &on, PolicyIn: policyAccept, PolicyOut: policyAccept}
	if err := f.api.SetGuestFirewallOptions(ctx, g.Node, g.Ref, change, o.Digest); err != nil {
		return fmt.Errorf("turning the firewall of %s on: %w", g.Ref, err)
	}
	return nil
}

// Keep writes the card's ipset and pco's rules on it as Apply does, and
// leaves the guest firewall's options alone: an admin who turned the
// firewall off is not overruled; filtered fails for the card instead.
func (f *Firewall) Keep(ctx context.Context, g model.Guest, card FirewallCard) error {
	if err := f.applySet(ctx, g, card); err != nil {
		return fmt.Errorf("the ipset %s of %s: %w", IPSetName(card.NIC), g.Ref, err)
	}
	if err := f.applyRules(ctx, g, card); err != nil {
		return fmt.Errorf("the firewall rules of %s on %s: %w", g.Ref, card.Iface(), err)
	}
	return nil
}

// OpenPorts are the TCP ports pco's rules on the card let the leg open now,
// whatever their place in the list.
func (g GuestFirewall) OpenPorts(card FirewallCard, comment string) []uint16 {
	var out []uint16
	for _, r := range g.Rules {
		if !ours(r, card.Iface(), comment) || r.Type != "in" || r.Action != "ACCEPT" || r.Proto != "tcp" || r.Source != card.Leg.String() {
			continue
		}
		if p, err := strconv.ParseUint(r.DPort, 10, 16); err == nil {
			out = append(out, uint16(p))
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(out)))
}

func (f *Firewall) applySet(ctx context.Context, g model.Guest, card FirewallCard) error {
	name := IPSetName(card.NIC)
	set, err := f.set(ctx, g, card.NIC)
	switch {
	case err != nil:
		return err
	case set == nil:
		if err := f.api.CreateGuestIPSet(ctx, g.Node, g.Ref, name, f.comment); err != nil {
			return err
		}
	case set.Comment != f.comment:
		return ErrNotOurs
	}
	entries, _, err := f.api.GuestIPSet(ctx, g.Node, g.Ref, name)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(entries, func(e pve.IPSetEntry) bool { return !e.NoMatch && entryIs(e, card.Lease) }) {
		if err := f.api.AddGuestIPSetEntry(ctx, g.Node, g.Ref, name, card.Lease.String(), f.comment); err != nil {
			return err
		}
	}
	for range len(entries) + 1 {
		entries, digest, err := f.api.GuestIPSet(ctx, g.Node, g.Ref, name)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(entries, func(e pve.IPSetEntry) bool { return e.NoMatch || !entryIs(e, card.Lease) })
		if i < 0 {
			return nil
		}
		if err := f.api.DeleteGuestIPSetEntry(ctx, g.Node, g.Ref, name, entries[i].CIDR, digest); err != nil {
			return err
		}
	}
	return errors.New("entries besides the lease are still listed")
}

// ErrRulesDiffer is the error of a write round after which pco's rules on
// the card still differ from what pco writes: Proxmox reads them back in
// another form, or did not put a new rule first. Of pco's rules on the card
// those that can no longer decide anything are removed; the caller writes
// them again only once they changed.
var ErrRulesDiffer = errors.New("pco's rules on the card still differ from what pco writes after they were written")

// applyRules puts pco's rules first on the card when they are not, one at a
// time from the last, each on top of the list under the digest it was read
// in, and then removes every other rule of pco's on the card. pco's rules on
// the card never grow beyond one old block and the new one: before and after
// a write round, those that come after pco's own IN DROP and OUT DROP, which
// no packet of the card reaches, are removed.
func (f *Firewall) applyRules(ctx context.Context, g model.Guest, card FirewallCard) error {
	want := WantRules(card, f.comment)
	iface := card.Iface()
	rules, digest, err := f.api.GuestFirewallRules(ctx, g.Node, g.Ref)
	if err != nil {
		return err
	}
	if rulesDrift(rules, want) != "" {
		if err := f.removeRules(ctx, g, iface, f.unreachable(iface), len(rules)); err != nil {
			return err
		}
		if rules, digest, err = f.api.GuestFirewallRules(ctx, g.Node, g.Ref); err != nil {
			return err
		}
		for i := len(want) - 1; i >= 0; i-- {
			if digest == "" {
				if _, digest, err = f.options(ctx, g); err != nil {
					return err
				}
			}
			if err := f.api.AddGuestFirewallRule(ctx, g.Node, g.Ref, want[i], digest); err != nil {
				return err
			}
			if rules, digest, err = f.api.GuestFirewallRules(ctx, g.Node, g.Ref); err != nil {
				return err
			}
		}
	}
	if blockEnd(rules, want) == 0 {
		if err := f.removeRules(ctx, g, iface, f.unreachable(iface), len(rules)); err != nil {
			return err
		}
		return ErrRulesDiffer
	}
	// pco's block on top is never removed: a list that changed so that it
	// is not on top any more stops the removal.
	return f.removeRules(ctx, g, iface, func(rules []pve.FirewallRule, i int) bool {
		end := blockEnd(rules, want)
		return end > 0 && i >= end
	}, len(rules))
}

// unreachable says which of pco's rules on iface no packet of the card
// reaches: those after pco's own IN DROP and OUT DROP on it, both on.
// Removing them never lets anything through that did not pass before.
func (f *Firewall) unreachable(iface string) func(rules []pve.FirewallRule, i int) bool {
	return func(rules []pve.FirewallRule, i int) bool {
		in, out := false, false
		for _, r := range rules[:i] {
			if !ours(r, iface, f.comment) || !r.Enable || r.Action != "DROP" || !matchesAll(r) {
				continue
			}
			in, out = in || r.Type == "in", out || r.Type == "out"
		}
		return in && out
	}
}

// matchesAll says that a rule matches every packet of its direction.
func matchesAll(r pve.FirewallRule) bool {
	return r.Macro == "" && r.Source == "" && r.Dest == "" && r.Proto == "" && r.SPort == "" && r.DPort == "" && r.ICMPType == ""
}

// blockEnd is the position after pco's block of want at the top of the
// rules that apply to its card, or 0 when it is not there.
func blockEnd(rules, want []pve.FirewallRule) int {
	if rulesDrift(rules, want) != "" {
		return 0
	}
	n := 0
	for i, r := range rules {
		if applies(r, want[0].Iface) {
			n++
		}
		if n == len(want) {
			return i + 1
		}
	}
	return 0
}

// options reads the guest firewall's options and the digest of its
// configuration, which an empty list of rules does not give.
func (f *Firewall) options(ctx context.Context, g model.Guest) (pve.GuestFirewallOptions, string, error) {
	o, err := f.api.GuestFirewallOptions(ctx, g.Node, g.Ref)
	if err != nil {
		return pve.GuestFirewallOptions{}, "", err
	}
	if o.Digest == "" {
		return o, "", errors.New("the firewall configuration has no digest")
	}
	return o, o.Digest, nil
}

// removeRules removes pco's rules on iface that drop says go, one at a time
// from the last, each under the digest of the list it was read in, reading
// the list again after each. It stops after as many rounds as the list had
// rules when it began.
func (f *Firewall) removeRules(ctx context.Context, g model.Guest, iface string, drop func(rules []pve.FirewallRule, i int) bool, bound int) error {
	for range bound + 1 {
		rules, digest, err := f.api.GuestFirewallRules(ctx, g.Node, g.Ref)
		if err != nil {
			return err
		}
		last := -1
		for i, r := range rules {
			if ours(r, iface, f.comment) && drop(rules, i) {
				last = i
			}
		}
		if last < 0 {
			return nil
		}
		if err := f.api.DeleteGuestFirewallRule(ctx, g.Node, g.Ref, rules[last].Pos, digest); err != nil {
			return err
		}
	}
	return fmt.Errorf("pco's rules on %s are still listed", iface)
}

// Restore takes away what Apply wrote for the card net<nic>: pco's rules on
// it, its ipset when it is pco's, and, when pco turned the guest firewall on,
// the firewall and the policies it had. A rule or an ipset that is not pco's
// stays as it is. The firewall is turned off only while what pco set is
// still what it is: both policies ACCEPT and no more rules on that are not
// this install's than it had. Otherwise an admin has taken it up since, and
// it stays on; kept says why.
func (f *Firewall) Restore(ctx context.Context, g model.Guest, nic int, before *store.FirewallBefore) (kept string, err error) {
	iface, name := "net"+strconv.Itoa(nic), IPSetName(nic)
	rules, _, err := f.api.GuestFirewallRules(ctx, g.Node, g.Ref)
	if err != nil {
		return "", err
	}
	if err := f.removeRules(ctx, g, iface, func([]pve.FirewallRule, int) bool { return true }, len(rules)); err != nil {
		return "", fmt.Errorf("removing pco's rules of %s on %s: %w", g.Ref, iface, err)
	}
	if err := f.removeSet(ctx, g, nic); err != nil {
		return "", fmt.Errorf("removing the ipset %s of %s: %w", name, g.Ref, err)
	}
	if before == nil || before.Enabled {
		return "", nil
	}
	o, err := f.api.GuestFirewallOptions(ctx, g.Node, g.Ref)
	if err != nil || !o.Enabled {
		return "", err
	}
	if rules, _, err = f.api.GuestFirewallRules(ctx, g.Node, g.Ref); err != nil {
		return "", err
	}
	switch n := f.othersOn(rules); {
	case o.PolicyIn != policyAccept || o.PolicyOut != policyAccept:
		return "its policies were changed since pco turned it on", nil
	case n > before.Rules:
		return fmt.Sprintf("it has %d rules more than when pco turned it on", n-before.Rules), nil
	}
	off := false
	change := pve.GuestFirewallChange{Enable: &off, PolicyIn: before.PolicyIn, PolicyOut: before.PolicyOut}
	if before.PolicyIn == "" {
		change.Delete = append(change.Delete, "policy_in")
	}
	if before.PolicyOut == "" {
		change.Delete = append(change.Delete, "policy_out")
	}
	if err := f.api.SetGuestFirewallOptions(ctx, g.Node, g.Ref, change, o.Digest); err != nil {
		return "", fmt.Errorf("turning the firewall of %s off again: %w", g.Ref, err)
	}
	return "", nil
}

func (f *Firewall) removeSet(ctx context.Context, g model.Guest, nic int) error {
	set, err := f.set(ctx, g, nic)
	if err != nil || set == nil || set.Comment != f.comment {
		return err
	}
	entries, _, err := f.api.GuestIPSet(ctx, g.Node, g.Ref, set.Name)
	if err != nil {
		return err
	}
	for range len(entries) {
		entries, digest, err := f.api.GuestIPSet(ctx, g.Node, g.Ref, set.Name)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			break
		}
		last := entries[len(entries)-1]
		if err := f.api.DeleteGuestIPSetEntry(ctx, g.Node, g.Ref, set.Name, last.CIDR, digest); err != nil {
			return err
		}
	}
	return f.api.DeleteGuestIPSet(ctx, g.Node, g.Ref, set.Name)
}
