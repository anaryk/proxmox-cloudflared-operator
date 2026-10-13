package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// guestFirewall is the firewall configuration of one guest as Proxmox keeps
// it: one digest for all of it, which every write moves on.
type guestFirewall struct {
	opts    pve.GuestFirewallOptions
	rules   []pve.FirewallRule
	sets    []pve.IPSet
	entries map[string][]pve.IPSetEntry
	serial  int
}

// fakeFirewalls is the guest firewall API of Proxmox in memory: a rule added
// goes first, a write under a stale digest changes nothing. Each write is in
// the log as "firewall <guest> <what>".
type fakeFirewalls struct {
	mu     sync.Mutex
	log    *callLog
	guests map[model.GuestRef]*guestFirewall
	reads  int
	fail   error // every write fails with it
}

func newFakeFirewalls(log *callLog) *fakeFirewalls {
	return &fakeFirewalls{log: log, guests: map[model.GuestRef]*guestFirewall{}}
}

func (f *fakeFirewalls) of(ref model.GuestRef) *guestFirewall {
	g, ok := f.guests[ref]
	if !ok {
		g = &guestFirewall{entries: map[string][]pve.IPSetEntry{}}
		f.guests[ref] = g
	}
	return g
}

// set changes the firewall of a guest as an admin would.
func (f *fakeFirewalls) set(ref model.GuestRef, change func(g *guestFirewall)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.of(ref)
	change(g)
	g.serial++
}

// get returns a copy of the firewall of a guest.
func (f *fakeFirewalls) get(ref model.GuestRef) guestFirewall {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := *f.of(ref)
	g.rules = slices.Clone(g.rules)
	g.sets = slices.Clone(g.sets)
	return g
}

func (f *fakeFirewalls) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *fakeFirewalls) write(ref model.GuestRef, digest string, what string, change func(g *guestFirewall) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	g := f.of(ref)
	if digest != "" && digest != "d"+strconv.Itoa(g.serial) {
		return fmt.Errorf("changing the firewall of %s: %w", ref, pve.ErrStaleDigest)
	}
	if err := change(g); err != nil {
		return err
	}
	g.serial++
	f.log.add("firewall " + ref.String() + " " + what)
	return nil
}

func (f *fakeFirewalls) GuestFirewallOptions(_ context.Context, _ string, ref model.GuestRef) (pve.GuestFirewallOptions, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	g := f.of(ref)
	o := g.opts
	o.Digest = "d" + strconv.Itoa(g.serial)
	return o, nil
}

func (f *fakeFirewalls) SetGuestFirewallOptions(_ context.Context, _ string, ref model.GuestRef, c pve.GuestFirewallChange, digest string) error {
	return f.write(ref, digest, "options", func(g *guestFirewall) error {
		if c.Enable != nil {
			g.opts.Enabled = *c.Enable
		}
		g.opts.PolicyIn = cmpOrString(c.PolicyIn, g.opts.PolicyIn)
		g.opts.PolicyOut = cmpOrString(c.PolicyOut, g.opts.PolicyOut)
		for _, d := range c.Delete {
			switch d {
			case "policy_in":
				g.opts.PolicyIn = ""
			case "policy_out":
				g.opts.PolicyOut = ""
			}
		}
		return nil
	})
}

func cmpOrString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (f *fakeFirewalls) GuestFirewallRules(_ context.Context, _ string, ref model.GuestRef) ([]pve.FirewallRule, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	g := f.of(ref)
	out := slices.Clone(g.rules)
	for i := range out {
		out[i].Pos = i
	}
	if len(out) == 0 {
		return out, "", nil
	}
	return out, "d" + strconv.Itoa(g.serial), nil
}

func (f *fakeFirewalls) AddGuestFirewallRule(_ context.Context, _ string, ref model.GuestRef, r pve.FirewallRule, digest string) error {
	return f.write(ref, digest, "add rule", func(g *guestFirewall) error {
		g.rules = append([]pve.FirewallRule{r}, g.rules...)
		return nil
	})
}

func (f *fakeFirewalls) DeleteGuestFirewallRule(_ context.Context, _ string, ref model.GuestRef, pos int, digest string) error {
	return f.write(ref, digest, "delete rule", func(g *guestFirewall) error {
		if pos >= len(g.rules) {
			return errors.New("no rule at that position")
		}
		g.rules = slices.Delete(g.rules, pos, pos+1)
		return nil
	})
}

func (f *fakeFirewalls) GuestIPSets(_ context.Context, _ string, ref model.GuestRef) ([]pve.IPSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return slices.Clone(f.of(ref).sets), nil
}

func (f *fakeFirewalls) GuestIPSet(_ context.Context, _ string, ref model.GuestRef, name string) ([]pve.IPSetEntry, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	g := f.of(ref)
	entries := slices.Clone(g.entries[name])
	if len(entries) == 0 {
		return entries, "", nil
	}
	return entries, "d" + strconv.Itoa(g.serial), nil
}

func (f *fakeFirewalls) CreateGuestIPSet(_ context.Context, _ string, ref model.GuestRef, name, comment string) error {
	return f.write(ref, "", "create "+name, func(g *guestFirewall) error {
		g.sets = append(g.sets, pve.IPSet{Name: name, Comment: comment})
		return nil
	})
}

func (f *fakeFirewalls) AddGuestIPSetEntry(_ context.Context, _ string, ref model.GuestRef, name, cidr, comment string) error {
	return f.write(ref, "", "add "+name+" "+cidr, func(g *guestFirewall) error {
		g.entries[name] = append(g.entries[name], pve.IPSetEntry{CIDR: cidr, Comment: comment})
		return nil
	})
}

func (f *fakeFirewalls) DeleteGuestIPSetEntry(_ context.Context, _ string, ref model.GuestRef, name, cidr, digest string) error {
	return f.write(ref, digest, "delete "+name+" "+cidr, func(g *guestFirewall) error {
		i := slices.IndexFunc(g.entries[name], func(e pve.IPSetEntry) bool { return e.CIDR == cidr })
		if i < 0 {
			return errors.New("no such entry")
		}
		g.entries[name] = slices.Delete(g.entries[name], i, i+1)
		return nil
	})
}

func (f *fakeFirewalls) DeleteGuestIPSet(_ context.Context, _ string, ref model.GuestRef, name string) error {
	return f.write(ref, "", "remove "+name, func(g *guestFirewall) error {
		if len(g.entries[name]) > 0 {
			return errors.New("the ipset is not empty")
		}
		g.sets = slices.DeleteFunc(g.sets, func(s pve.IPSet) bool { return s.Name == name })
		return nil
	})
}
