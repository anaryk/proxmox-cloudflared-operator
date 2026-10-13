package managednet

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// fakeGuestFirewall is the firewall configuration of one guest as Proxmox
// keeps it: one digest for all of it, which every write moves on.
type fakeGuestFirewall struct {
	opts    pve.GuestFirewallOptions
	rules   []pve.FirewallRule
	sets    []pve.IPSet
	entries map[string][]pve.IPSetEntry
	serial  int
}

func (g *fakeGuestFirewall) digest() string { return "d" + strconv.Itoa(g.serial) }

// fakeFirewall is the guest firewall API of Proxmox, in memory, with the
// semantics pco relies on: a rule added goes first, a write under a stale
// digest changes nothing, an ipset with entries is not deleted.
type fakeFirewall struct {
	mu     sync.Mutex
	guests map[model.GuestRef]*fakeGuestFirewall
	writes []string
	reads  int
	// fail makes the call of that name fail.
	fail map[string]error
	// after runs after every write, with the guest's state as it is then.
	after func(ref model.GuestRef, g *fakeGuestFirewall)
	// normalize changes each rule as it is read back; appendRules puts a
	// new rule last instead of first.
	normalize   func(pve.FirewallRule) pve.FirewallRule
	appendRules bool
}

func newFakeFirewall() *fakeFirewall {
	return &fakeFirewall{guests: map[model.GuestRef]*fakeGuestFirewall{}, fail: map[string]error{}}
}

func (f *fakeFirewall) guest(ref model.GuestRef) *fakeGuestFirewall {
	g, ok := f.guests[ref]
	if !ok {
		g = &fakeGuestFirewall{entries: map[string][]pve.IPSetEntry{}}
		f.guests[ref] = g
	}
	return g
}

func (f *fakeFirewall) wrote(ref model.GuestRef, g *fakeGuestFirewall, what string) {
	g.serial++
	f.writes = append(f.writes, what)
	if f.after != nil {
		f.after(ref, g)
	}
}

func (f *fakeFirewall) stale(g *fakeGuestFirewall, digest string) error {
	if digest != g.digest() {
		return fmt.Errorf("changing the firewall: %w", pve.ErrStaleDigest)
	}
	return nil
}

func (f *fakeFirewall) failed(name string) error { return f.fail[name] }

func (f *fakeFirewall) GuestFirewallOptions(_ context.Context, _ string, ref model.GuestRef) (pve.GuestFirewallOptions, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if err := f.failed("options"); err != nil {
		return pve.GuestFirewallOptions{}, err
	}
	g := f.guest(ref)
	o := g.opts
	o.Digest = g.digest()
	return o, nil
}

func (f *fakeFirewall) SetGuestFirewallOptions(_ context.Context, _ string, ref model.GuestRef, c pve.GuestFirewallChange, digest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failed("set-options"); err != nil {
		return err
	}
	g := f.guest(ref)
	if err := f.stale(g, digest); err != nil {
		return err
	}
	var parts []string
	if c.Enable != nil {
		g.opts.Enabled = *c.Enable
		parts = append(parts, "enable="+strconv.FormatBool(*c.Enable))
	}
	if c.PolicyIn != "" {
		g.opts.PolicyIn = c.PolicyIn
		parts = append(parts, "policy_in="+c.PolicyIn)
	}
	if c.PolicyOut != "" {
		g.opts.PolicyOut = c.PolicyOut
		parts = append(parts, "policy_out="+c.PolicyOut)
	}
	for _, d := range c.Delete {
		switch d {
		case "policy_in":
			g.opts.PolicyIn = ""
		case "policy_out":
			g.opts.PolicyOut = ""
		}
		parts = append(parts, "delete="+d)
	}
	f.wrote(ref, g, "options "+strings.Join(parts, " "))
	return nil
}

func (f *fakeFirewall) GuestFirewallRules(_ context.Context, _ string, ref model.GuestRef) ([]pve.FirewallRule, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if err := f.failed("rules"); err != nil {
		return nil, "", err
	}
	g := f.guest(ref)
	out := slices.Clone(g.rules)
	for i := range out {
		out[i].Pos = i
		if f.normalize != nil {
			out[i] = f.normalize(out[i])
		}
	}
	if len(out) == 0 {
		return out, "", nil
	}
	return out, g.digest(), nil
}

func (f *fakeFirewall) AddGuestFirewallRule(_ context.Context, _ string, ref model.GuestRef, r pve.FirewallRule, digest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failed("add-rule"); err != nil {
		return err
	}
	g := f.guest(ref)
	if err := f.stale(g, digest); err != nil {
		return err
	}
	r.Pos = 0
	if f.appendRules {
		g.rules = append(g.rules, r)
	} else {
		g.rules = append([]pve.FirewallRule{r}, g.rules...)
	}
	f.wrote(ref, g, "add "+ruleText(r))
	return nil
}

func (f *fakeFirewall) DeleteGuestFirewallRule(_ context.Context, _ string, ref model.GuestRef, pos int, digest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failed("delete-rule"); err != nil {
		return err
	}
	g := f.guest(ref)
	if err := f.stale(g, digest); err != nil {
		return err
	}
	if pos >= len(g.rules) {
		return errors.New("no rule at that position")
	}
	r := g.rules[pos]
	g.rules = slices.Delete(g.rules, pos, pos+1)
	f.wrote(ref, g, "delete "+ruleText(r))
	return nil
}

func (f *fakeFirewall) GuestIPSets(_ context.Context, _ string, ref model.GuestRef) ([]pve.IPSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if err := f.failed("ipsets"); err != nil {
		return nil, err
	}
	return slices.Clone(f.guest(ref).sets), nil
}

func (f *fakeFirewall) GuestIPSet(_ context.Context, _ string, ref model.GuestRef, name string) ([]pve.IPSetEntry, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	g := f.guest(ref)
	if !slices.ContainsFunc(g.sets, func(s pve.IPSet) bool { return s.Name == name }) {
		return nil, "", errors.New("no such IPSet")
	}
	entries := slices.Clone(g.entries[name])
	if len(entries) == 0 {
		return entries, "", nil
	}
	return entries, g.digest(), nil
}

func (f *fakeFirewall) CreateGuestIPSet(_ context.Context, _ string, ref model.GuestRef, name, comment string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failed("create-set"); err != nil {
		return err
	}
	g := f.guest(ref)
	if slices.ContainsFunc(g.sets, func(s pve.IPSet) bool { return s.Name == name }) {
		return errors.New("IPSet already exists")
	}
	g.sets = append(g.sets, pve.IPSet{Name: name, Comment: comment})
	f.wrote(ref, g, "create "+name)
	return nil
}

func (f *fakeFirewall) AddGuestIPSetEntry(_ context.Context, _ string, ref model.GuestRef, name, cidr, comment string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.guest(ref)
	if !slices.ContainsFunc(g.sets, func(s pve.IPSet) bool { return s.Name == name }) {
		return errors.New("no such IPSet")
	}
	g.entries[name] = append(g.entries[name], pve.IPSetEntry{CIDR: cidr, Comment: comment})
	f.wrote(ref, g, "add "+name+" "+cidr)
	return nil
}

func (f *fakeFirewall) DeleteGuestIPSetEntry(_ context.Context, _ string, ref model.GuestRef, name, cidr, digest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.guest(ref)
	if err := f.stale(g, digest); err != nil {
		return err
	}
	i := slices.IndexFunc(g.entries[name], func(e pve.IPSetEntry) bool { return e.CIDR == cidr })
	if i < 0 {
		return errors.New("no such entry")
	}
	g.entries[name] = slices.Delete(g.entries[name], i, i+1)
	f.wrote(ref, g, "delete "+name+" "+cidr)
	return nil
}

func (f *fakeFirewall) DeleteGuestIPSet(_ context.Context, _ string, ref model.GuestRef, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.guest(ref)
	if len(g.entries[name]) > 0 {
		return errors.New("IPSet is not empty")
	}
	i := slices.IndexFunc(g.sets, func(s pve.IPSet) bool { return s.Name == name })
	if i < 0 {
		return errors.New("no such IPSet")
	}
	g.sets = slices.Delete(g.sets, i, i+1)
	delete(g.entries, name)
	f.wrote(ref, g, "remove "+name)
	return nil
}

func ruleText(r pve.FirewallRule) string {
	parts := []string{strings.ToUpper(r.Type), r.Action}
	for _, f := range []struct{ k, v string }{{"-iface", r.Iface}, {"-source", r.Source}, {"-dest", r.Dest}, {"-p", r.Proto}, {"-sport", r.SPort}, {"-dport", r.DPort}} {
		if f.v != "" {
			parts = append(parts, f.k, f.v)
		}
	}
	if !r.Enable {
		parts = append(parts, "(off)")
	}
	return strings.Join(parts, " ")
}

// packet is what the evaluator decides on.
type packet struct {
	dir      string // in, out
	src, dst netip.Addr
	proto    string
	sport    uint16
	dport    uint16
}

// chainVerdict decides a packet on a card as pve-firewall's guest chain does with
// the rules of the configuration: the first rule that applies to the card
// and matches decides; none, the policy of the direction, DROP in and ACCEPT
// out by default. A guest firewall that is off filters nothing.
func chainVerdict(g *fakeGuestFirewall, iface string, p packet) string {
	if !g.opts.Enabled {
		return "ACCEPT"
	}
	for _, r := range g.rules {
		if !applies(r, iface) || r.Type != p.dir || !matches(r, p) {
			continue
		}
		return r.Action
	}
	if p.dir == "in" {
		return cmpOr(g.opts.PolicyIn, "DROP")
	}
	return cmpOr(g.opts.PolicyOut, "ACCEPT")
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func matches(r pve.FirewallRule, p packet) bool {
	addr := func(spec string, a netip.Addr) bool {
		if spec == "" {
			return true
		}
		if x, err := netip.ParseAddr(spec); err == nil {
			return x == a
		}
		pr, err := netip.ParsePrefix(spec)
		return err == nil && pr.Contains(a)
	}
	port := func(spec string, n uint16) bool { return spec == "" || spec == strconv.Itoa(int(n)) }
	return addr(r.Source, p.src) && addr(r.Dest, p.dst) && (r.Proto == "" || r.Proto == p.proto) && port(r.SPort, p.sport) && port(r.DPort, p.dport)
}
