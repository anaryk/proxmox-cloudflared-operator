package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Card is a network card of a guest on a vnet.
type Card struct {
	Owner string // as model.GuestRef prints it: qemu/101, lxc/102
	Node  string
	NIC   int
	MAC   string // in normal form; empty when the card names none
}

// Owners lists the guests of cards, each once, in their order.
func Owners(cards []Card) []string {
	var out []string
	for _, c := range cards {
		if !slices.Contains(out, c.Owner) {
			out = append(out, c.Owner)
		}
	}
	return out
}

// clusterGuest is a guest as /cluster/resources lists it.
type clusterGuest struct {
	Type string `json:"type"`
	VMID pveInt `json:"vmid"`
	Node string `json:"node"`
}

func (g clusterGuest) ref() (model.GuestRef, bool) {
	ref := model.GuestRef{Kind: model.GuestKind(g.Type), VMID: int(g.VMID)}
	if (ref.Kind != model.KindQEMU && ref.Kind != model.KindLXC) || ref.VMID < 1 || !nodeName(g.Node) {
		return model.GuestRef{}, false
	}
	return ref, true
}

// nodeName accepts the names of nodes, which go into paths.
func nodeName(s string) bool {
	return s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-.") == "" && strings.Trim(s, ".") != ""
}

func guestPath(node string, ref model.GuestRef) string {
	return fmt.Sprintf("/nodes/%s/%s/%d", node, ref.Kind, ref.VMID)
}

// guestConfig reads the configuration of a guest as it runs, without what
// waits for its next start: a card whose removal is pending is still there.
func guestConfig(ctx context.Context, r Runner, node string, ref model.GuestRef) (map[string]string, error) {
	var raw map[string]json.RawMessage
	if err := query(ctx, r, &raw, "pvesh", "get", guestPath(node, ref)+"/config", "--current", "1", "--output-format", "json"); err != nil {
		return nil, err
	}
	cfg := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if json.Unmarshal(v, &s) != nil {
			s = string(v)
		}
		cfg[k] = s
	}
	return cfg, nil
}

// cardsIn returns the cards of a configuration on vnet.
func cardsIn(cfg map[string]string, vnet string) []Card {
	var out []Card
	for key, value := range cfg {
		n, ok := strings.CutPrefix(key, "net")
		if !ok {
			continue
		}
		index, err := strconv.Atoi(n)
		if err != nil || strconv.Itoa(index) != n {
			continue
		}
		var bridge, mac string
		for part := range strings.SplitSeq(value, ",") {
			k, v, _ := strings.Cut(part, "=")
			switch {
			case k == "bridge":
				bridge = v
			case k == "hwaddr" || k == "macaddr" || mac == "" && strings.Count(v, ":") == 5:
				if norm, err := model.NormalizeMAC(v); err == nil {
					mac = norm
				}
			}
		}
		if bridge == vnet {
			out = append(out, Card{NIC: index, MAC: mac})
		}
	}
	slices.SortFunc(out, func(a, b Card) int { return a.NIC - b.NIC })
	return out
}

// CardsOn reads the configuration of every guest of the cluster, templates
// among them, and returns their cards on vnet. A guest that went while it was
// read has none.
func CardsOn(ctx context.Context, r Runner, vnet string) ([]Card, error) {
	var guests []clusterGuest
	if err := query(ctx, r, &guests, "pvesh", "get", "/cluster/resources", "--type", "vm", "--output-format", "json"); err != nil {
		return nil, fmt.Errorf("listing the guests of the cluster: %w", err)
	}
	slices.SortFunc(guests, func(a, b clusterGuest) int { return int(a.VMID - b.VMID) })
	var out []Card
	for _, g := range guests {
		ref, ok := g.ref()
		if !ok {
			continue
		}
		cfg, err := guestConfig(ctx, r, g.Node, ref)
		if err != nil {
			if gone(err) {
				continue
			}
			return nil, fmt.Errorf("reading the configuration of %s: %w", ref, err)
		}
		for _, c := range cardsIn(cfg, vnet) {
			c.Owner, c.Node = ref.String(), g.Node
			out = append(out, c)
		}
	}
	return out, nil
}

// gone reports whether the error of a command says that what it was asked
// about does not exist.
func gone(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "does not exist") || strings.Contains(msg, "no such")
}

// The bound of the wait for the configurations of the guests to drop the
// cards that were removed.
const (
	detachPolls = 30
	detachPoll  = 2 * time.Second
)

// Detacher removes pco's cards on the managed network from the guests they
// were attached to, as root through pvesh, which reaches the guests of every
// node of the cluster.
type Detacher struct {
	Run   Runner
	Ask   Prompter
	Sleep func(ctx context.Context, d time.Duration) error
}

// Detach removes the card of each attachment from its guest, where the card
// at that index on the managed vnet still has the attachment's MAC, and puts
// back what the appliance changed of the guest's firewall: its rules on the
// card, the card's ipset and, when pco turned it on, the firewall itself. It
// waits, bounded, for the configurations to drop the cards, and returns those
// still there, as a card of a VM without network hotplug is until it restarts.
func (d Detacher) Detach(ctx context.Context, attachments []store.Attachment) ([]Card, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	var guests []clusterGuest
	if err := query(ctx, d.Run, &guests, "pvesh", "get", "/cluster/resources", "--type", "vm", "--output-format", "json"); err != nil {
		return nil, fmt.Errorf("listing the guests of the cluster: %w", err)
	}
	nodes := map[string]string{}
	for _, g := range guests {
		if ref, ok := g.ref(); ok {
			nodes[ref.String()] = g.Node
		}
	}
	var removed []Card
	var failed []string
	for _, a := range attachments {
		ref, err := model.ParseGuestRef(a.Owner)
		if err != nil {
			continue
		}
		node, ok := nodes[a.Owner]
		if !ok {
			d.Ask.Info("managed card of %s: the guest is gone", a.Owner)
			continue
		}
		c, err := d.detach(ctx, node, ref, a)
		switch {
		case err != nil:
			d.Ask.Warn("%v", err)
			failed = append(failed, a.Owner)
		case c != nil:
			removed = append(removed, *c)
		}
	}
	left, err := d.wait(ctx, removed)
	if err != nil {
		return nil, err
	}
	if len(failed) > 0 {
		return left, fmt.Errorf("the managed cards of %s could not be removed", strings.Join(failed, ", "))
	}
	return left, nil
}

// FirewallComment starts the comment of every firewall rule pco puts on a
// guest's card, pco:<install id>; the detach removes rules with it and no
// other.
const FirewallComment = "pco:"

// detach removes the card of one attachment and puts back the guest's
// firewall; nil when the card is not there. The firewall is only touched for
// a card this run found with pco's MAC and removed: a card that is gone was
// removed by somebody else, and its index may be an admin's card by now.
func (d Detacher) detach(ctx context.Context, node string, ref model.GuestRef, a store.Attachment) (*Card, error) {
	cfg, err := guestConfig(ctx, d.Run, node, ref)
	if err != nil {
		return nil, fmt.Errorf("reading the configuration of %s: %w", ref, err)
	}
	cards := cardsIn(cfg, ManagedVnet)
	i := slices.IndexFunc(cards, func(c Card) bool { return c.NIC == a.NIC })
	switch {
	case i < 0:
		d.goneCard(node, ref, a)
		return nil, nil
	case cards[i].MAC != a.MAC:
		d.Ask.Warn("managed card of %s: net%d on %s has the MAC %s, not pco's %s; left as it is", a.Owner, a.NIC, ManagedVnet, cards[i].MAC, a.MAC)
		return nil, nil
	}
	// Under the digest of the configuration the card was found in: one
	// changed since, as by an admin who put another card there, is not
	// written over.
	digest := cfg["digest"]
	if digest == "" {
		return nil, fmt.Errorf("removing net%d of %s: its configuration has no digest", a.NIC, ref)
	}
	if _, err := d.Run.Run(ctx, "pvesh", "set", guestPath(node, ref)+"/config", "--delete", "net"+strconv.Itoa(a.NIC), "--digest", digest); err != nil {
		return nil, fmt.Errorf("removing net%d of %s: %w", a.NIC, ref, err)
	}
	d.Ask.Info("managed card of %s: removed net%d", a.Owner, a.NIC)
	if a.Firewall != nil {
		if err := d.restoreFirewall(ctx, node, ref, a); err != nil {
			return nil, err
		}
	}
	return &Card{Owner: a.Owner, Node: node, NIC: a.NIC, MAC: a.MAC}, nil
}

// goneCard says that the card of an attachment is not there any more, and
// names the commands that put the guest's firewall back, which the run does
// not run itself.
func (d Detacher) goneCard(node string, ref model.GuestRef, a store.Attachment) {
	if a.Firewall == nil {
		d.Ask.Info("managed card of %s: net%d is gone already", a.Owner, a.NIC)
		return
	}
	fw, iface := guestPath(node, ref)+"/firewall", "net"+strconv.Itoa(a.NIC)
	d.Ask.Warn("managed card of %s: %s is gone already, so its firewall is left as it is; pco's rules on %s carry the comment "+
		"%s<install id>, and these put the firewall back by hand:", a.Owner, iface, iface, FirewallComment)
	d.Ask.Info("  pvesh get %s/rules --output-format json, then pvesh delete %s/rules/<pos> for each rule on %s with that comment, from the last",
		fw, fw, iface)
	d.Ask.Info("  pvesh delete %s/ipset/ipfilter-%s --force 1", fw, iface)
	if !a.Firewall.Enabled {
		d.Ask.Info("  pvesh set %s/options --enable 0", fw)
	}
}

// firewallRule is a rule of a guest's firewall as pvesh lists it, with the
// digest of the list it is in.
type firewallRule struct {
	Pos     pveInt `json:"pos"`
	Iface   string `json:"iface"`
	Comment string `json:"comment"`
	Enable  pveInt `json:"enable"`
	Digest  string `json:"digest"`
}

// ipset is an ipset of a guest's firewall as pvesh lists it.
type ipset struct {
	Name string `json:"name"`
}

// restoreFirewall takes the guest's firewall back to what it was before pco
// attached it: pco's rules on the card, told by their comment, the card's
// ipset, and the firewall itself when pco turned it on. A rule on the card
// without the comment is an admin's and stays.
func (d Detacher) restoreFirewall(ctx context.Context, node string, ref model.GuestRef, a store.Attachment) error {
	fw := guestPath(node, ref) + "/firewall"
	iface := "net" + strconv.Itoa(a.NIC)
	if err := d.removeRules(ctx, fw, iface, ref); err != nil {
		return err
	}
	var sets []ipset
	if err := query(ctx, d.Run, &sets, "pvesh", "get", fw+"/ipset", "--output-format", "json"); err != nil {
		return fmt.Errorf("reading the ipsets of %s: %w", ref, err)
	}
	set := "ipfilter-" + iface
	if slices.ContainsFunc(sets, func(s ipset) bool { return s.Name == set }) {
		if _, err := d.Run.Run(ctx, "pvesh", "delete", fw+"/ipset/"+set, "--force", "1"); err != nil {
			return fmt.Errorf("removing the ipset %s of %s: %w", set, ref, err)
		}
	}
	if !a.Firewall.Enabled {
		kept, err := d.turnOff(ctx, fw, ref, a.Firewall)
		if err != nil {
			return err
		}
		if kept != "" {
			d.Ask.Warn("managed card of %s: the firewall stays on, which pco turned on for the card: %s; pco's rules and ipset on %s are removed",
				a.Owner, kept, iface)
			return nil
		}
	}
	d.Ask.Info("managed card of %s: the firewall is as it was before pco attached the card", a.Owner)
	return nil
}

// firewallOptions are the options of a guest's firewall as pvesh lists them.
type firewallOptions struct {
	Enable    pveInt `json:"enable"`
	PolicyIn  string `json:"policy_in"`
	PolicyOut string `json:"policy_out"`
	Digest    string `json:"digest"`
}

// turnOff turns a guest firewall that pco turned on off again, with the
// policies it had, but only while it is still pco's to turn off, by the rule
// the appliance's detach goes by (managednet.TakenUp). Otherwise an admin has
// taken it up since, and kept says why it stays on. Options or rules that
// cannot be read leave it on, with the error.
func (d Detacher) turnOff(ctx context.Context, fw string, ref model.GuestRef, before *store.FirewallBefore) (kept string, err error) {
	var o firewallOptions
	if err := query(ctx, d.Run, &o, "pvesh", "get", fw+"/options", "--output-format", "json"); err != nil {
		return "", fmt.Errorf("reading the firewall options of %s, so it stays on: %w", ref, err)
	}
	if o.Enable != 1 {
		return "", nil
	}
	var listed []firewallRule
	if err := query(ctx, d.Run, &listed, "pvesh", "get", fw+"/rules", "--output-format", "json"); err != nil {
		return "", fmt.Errorf("reading the firewall rules of %s, so it stays on: %w", ref, err)
	}
	rules := make([]pve.FirewallRule, 0, len(listed))
	for _, r := range listed {
		rules = append(rules, pve.FirewallRule{Iface: r.Iface, Comment: r.Comment, Enable: r.Enable == 1})
	}
	if why := managednet.TakenUp(pve.GuestFirewallOptions{Enabled: true, PolicyIn: o.PolicyIn, PolicyOut: o.PolicyOut}, rules, *before); why != "" {
		return why, nil
	}
	args := []string{"set", fw + "/options", "--enable", "0"}
	for _, p := range []struct{ key, value string }{{"policy_in", before.PolicyIn}, {"policy_out", before.PolicyOut}} {
		if p.value == "" {
			args = append(args, "--delete", p.key)
		} else {
			args = append(args, "--"+p.key, p.value)
		}
	}
	if o.Digest != "" {
		args = append(args, "--digest", o.Digest)
	}
	if _, err := d.Run.Run(ctx, "pvesh", args...); err != nil {
		return "", fmt.Errorf("turning the firewall of %s off again: %w", ref, err)
	}
	return "", nil
}

// removeRules removes pco's rules on iface, told by their comment, one at a
// time: the last of them under the digest of the list it was read in, and
// the list read again, as a rule removed moves those after it up and the
// list may have changed meanwhile. It stops after as many rounds as the
// first read had rules.
func (d Detacher) removeRules(ctx context.Context, fw, iface string, ref model.GuestRef) error {
	ours := func(r firewallRule) bool { return r.Iface == iface && strings.HasPrefix(r.Comment, FirewallComment) }
	for round, limit := 0, -1; limit < 0 || round <= limit; round++ {
		var rules []firewallRule
		if err := query(ctx, d.Run, &rules, "pvesh", "get", fw+"/rules", "--output-format", "json"); err != nil {
			return fmt.Errorf("reading the firewall rules of %s: %w", ref, err)
		}
		if limit < 0 {
			limit = len(rules)
		}
		last := -1
		for i, r := range rules {
			if ours(r) && (last < 0 || r.Pos > rules[last].Pos) {
				last = i
			}
		}
		if last < 0 {
			return nil
		}
		rule := rules[last]
		if rule.Digest == "" {
			return fmt.Errorf("removing the firewall rule %d of %s: the list of its rules has no digest", rule.Pos, ref)
		}
		if _, err := d.Run.Run(ctx, "pvesh", "delete", fmt.Sprintf("%s/rules/%d", fw, rule.Pos), "--digest", rule.Digest); err != nil {
			return fmt.Errorf("removing the firewall rule %d of %s: %w", rule.Pos, ref, err)
		}
	}
	return fmt.Errorf("removing the firewall rules of %s: pco's rules on %s are still listed", ref, iface)
}

// wait reads the configurations of the guests whose cards were removed until
// none lists them, at most detachPolls times, and returns those still there.
func (d Detacher) wait(ctx context.Context, removed []Card) ([]Card, error) {
	left := removed
	for poll := 0; len(left) > 0; poll++ {
		var still []Card
		for _, c := range left {
			ref, _ := model.ParseGuestRef(c.Owner)
			cfg, err := guestConfig(ctx, d.Run, c.Node, ref)
			if err != nil {
				if gone(err) {
					continue
				}
				return nil, fmt.Errorf("reading the configuration of %s: %w", ref, err)
			}
			if slices.ContainsFunc(cardsIn(cfg, ManagedVnet), func(o Card) bool { return o.NIC == c.NIC && o.MAC == c.MAC }) {
				still = append(still, c)
			}
		}
		left = still
		if len(left) == 0 || poll+1 >= detachPolls {
			break
		}
		if err := d.Sleep(ctx, detachPoll); err != nil {
			return nil, err
		}
	}
	for _, c := range left {
		d.Ask.Warn("managed card of %s: net%d is still in its configuration; a VM without network hotplug drops it when it restarts", c.Owner, c.NIC)
	}
	return left, nil
}
