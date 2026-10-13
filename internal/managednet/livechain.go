package managednet

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// CardDevice is the device of a guest's card that pve-firewall makes its
// chains for on the node: tap<vmid>i<n> for a virtual machine,
// veth<vmid>i<n> for a container. The chains exist while the guest runs.
func CardDevice(ref model.GuestRef, nic int) string {
	prefix := "tap"
	if ref.Kind == model.KindLXC {
		prefix = "veth"
	}
	return prefix + strconv.Itoa(ref.VMID) + "i" + strconv.Itoa(nic)
}

// LiveRule is a rule of a chain of the node's filter table as iptables-save
// prints it: what it matches on, its target, and every other option.
type LiveRule struct {
	Source, Dest, Proto, SPort, DPort string
	// Target is the chain the rule jumps or goes to.
	Target string
	// Other are the options the rule has besides those, its comment and the
	// match module of its protocol; a rule with any of them matches less
	// than the rule of the firewall configuration it would stand for.
	Other []string
}

// LiveChains are the rules of the filter table, by chain, in their order.
type LiveChains map[string][]LiveRule

// ParseIPTablesSave reads the filter table of what iptables-save prints.
// Lines it does not understand are left out: they are of other tables, or
// declarations.
func ParseIPTablesSave(text string) LiveChains {
	chains := LiveChains{}
	table := ""
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "*"):
			table = line[1:]
		case table != "filter":
		case strings.HasPrefix(line, ":"):
			name, _, _ := strings.Cut(line[1:], " ")
			if _, ok := chains[name]; !ok {
				chains[name] = nil
			}
		case strings.HasPrefix(line, "-A "):
			args := fields(line)
			if len(args) < 2 {
				continue
			}
			chains[args[1]] = append(chains[args[1]], parseLiveRule(args[2:]))
		}
	}
	return chains
}

// fields splits a line of iptables-save into its words, a quoted word as
// one.
func fields(line string) []string {
	var out []string
	var cur strings.Builder
	quoted, inWord := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && quoted && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
		case c == '"':
			quoted, inWord = !quoted, true
		case c == ' ' && !quoted:
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

func parseLiveRule(args []string) LiveRule {
	var r LiveRule
	for i := 0; i < len(args); i++ {
		value := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch a := args[i]; a {
		case "-s":
			r.Source = value()
		case "-d":
			r.Dest = value()
		case "-p":
			r.Proto = value()
		case "--sport":
			r.SPort = value()
		case "--dport":
			r.DPort = value()
		case "-j", "-g":
			r.Target = value()
		case "-m":
			if m := value(); m != r.Proto && m != "comment" {
				r.Other = append(r.Other, a, m)
			}
		case "--comment":
			value()
		default:
			r.Other = append(r.Other, a)
		}
	}
	return r
}

// The targets pve-firewall gives a rule's action in a guest's chains: an
// accept in the IN chain is ACCEPT, one in the OUT chain goes to the chain
// that marks the packet accepted.
const acceptMark = "PVEFW-SET-ACCEPT-MARK"

// LiveDrift says how the node's live chains of the card's device differ from
// the rules pco wrote: every rule must be installed, in its order, in the
// chain of its direction; the rules pve-firewall puts around them may be
// between. Nothing when they are all there. A guest that does not run has no
// chains.
func LiveDrift(chains LiveChains, dev string, want []pve.FirewallRule) []string {
	var why []string
	for _, dir := range []string{"in", "out"} {
		chain := dev + "-" + strings.ToUpper(dir)
		live, ok := chains[chain]
		if !ok {
			why = append(why, "the chain "+chain+" is not loaded")
			continue
		}
		next := 0
		for _, w := range want {
			if w.Type != dir {
				continue
			}
			for next < len(live) && !liveMatch(live[next], w) {
				next++
			}
			if next == len(live) {
				why = append(why, fmt.Sprintf("the chain %s lacks pco's rule %s", chain, describe(w)))
				break
			}
			next++
		}
	}
	return why
}

// liveMatch says that a live rule is the rule of the firewall configuration
// as pve-firewall installs it.
func liveMatch(l LiveRule, w pve.FirewallRule) bool {
	target := w.Action
	ok := l.Target == target || w.Action == "ACCEPT" && l.Target == acceptMark
	return ok && len(l.Other) == 0 && sameAddr(l.Source, w.Source) && sameAddr(l.Dest, w.Dest) &&
		l.Proto == w.Proto && l.SPort == w.SPort && l.DPort == w.DPort
}

// sameAddr compares an address of iptables-save, which prints a single
// address as a /32, with one of the firewall configuration.
func sameAddr(live, conf string) bool {
	if live == conf {
		return true
	}
	p, err := netip.ParsePrefix(live)
	a, aerr := netip.ParseAddr(conf)
	return err == nil && aerr == nil && p.Bits() == a.BitLen() && p.Addr() == a
}

// describe is a rule as the firewall configuration of Proxmox writes it.
func describe(r pve.FirewallRule) string {
	parts := []string{strings.ToUpper(r.Type), r.Action}
	for _, f := range []struct{ k, v string }{{"-source", r.Source}, {"-dest", r.Dest}, {"-p", r.Proto}, {"-sport", r.SPort}, {"-dport", r.DPort}} {
		if f.v != "" {
			parts = append(parts, f.k, f.v)
		}
	}
	return strings.Join(parts, " ")
}
