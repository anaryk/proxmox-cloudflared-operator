package pve

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// NodeFirewallOptions is the part of the firewall options of a node the
// managed network reads: whether the firewall is on, and whether it runs on
// the nftables backend, which installs no guest rules.
type NodeFirewallOptions struct {
	Enabled  bool
	NFTables bool
}

// ClusterFirewallOptions is the part of the firewall options of the
// datacenter the managed network reads: without it no guest rule is in
// force.
type ClusterFirewallOptions struct {
	Enabled bool
}

// GuestFirewallOptions is the part of the firewall options of a guest the
// managed network reads: whether the firewall is on, the filters that apply
// to every card of the guest, the policies, and the digest of the guest's
// firewall configuration, which a write to its options is made under.
type GuestFirewallOptions struct {
	Enabled bool
	// MACFilterOff says that macfilter is set to 0; absent, it is on.
	MACFilterOff bool
	// IPFilter is the guest-wide ipfilter option, which filters every card
	// with firewall=1 to its configured addresses.
	IPFilter  bool
	PolicyIn  string
	PolicyOut string
	Digest    string
}

type nodeFirewallWire struct {
	Enable   flexBool `json:"enable"`
	NFTables flexBool `json:"nftables"`
}

type serviceStateWire struct {
	State string `json:"state"`
}

// NodeFirewallOptions reads the firewall options of a node. An absent
// option is off: a firewall never configured, or the iptables backend.
func (c *Client) NodeFirewallOptions(ctx context.Context, node string) (NodeFirewallOptions, error) {
	endpoint, err := nodeEndpoint(node, "firewall/options")
	if err != nil {
		return NodeFirewallOptions{}, err
	}
	var w nodeFirewallWire
	if err := c.get(ctx, endpoint, nil, &w); err != nil {
		return NodeFirewallOptions{}, fmt.Errorf("fetching the firewall options of node %s: %w", node, err)
	}
	return NodeFirewallOptions{Enabled: bool(w.Enable), NFTables: bool(w.NFTables)}, nil
}

// ClusterFirewallOptions reads the firewall options of the datacenter.
func (c *Client) ClusterFirewallOptions(ctx context.Context) (ClusterFirewallOptions, error) {
	o, err := c.firewall(ctx, "cluster/firewall/options", "the datacenter")
	if err != nil {
		return ClusterFirewallOptions{}, err
	}
	return ClusterFirewallOptions(o), nil
}

// GuestFirewallOptions reads the firewall options of a guest on node.
func (c *Client) GuestFirewallOptions(ctx context.Context, node string, ref model.GuestRef) (GuestFirewallOptions, error) {
	endpoint, err := guestEndpoint(node, ref.Kind, ref.VMID, "firewall/options")
	if err != nil {
		return GuestFirewallOptions{}, err
	}
	var w guestFirewallWire
	if err := c.get(ctx, endpoint, nil, &w); err != nil {
		return GuestFirewallOptions{}, fmt.Errorf("fetching the firewall options of %s: %w", ref, err)
	}
	o := GuestFirewallOptions{Enabled: bool(w.Enable), IPFilter: bool(w.IPFilter), PolicyIn: w.PolicyIn, PolicyOut: w.PolicyOut, Digest: w.Digest}
	o.MACFilterOff = w.MACFilter != nil && !bool(*w.MACFilter)
	return o, nil
}

type guestFirewallWire struct {
	Enable    flexBool  `json:"enable"`
	MACFilter *flexBool `json:"macfilter"`
	IPFilter  flexBool  `json:"ipfilter"`
	PolicyIn  string    `json:"policy_in"`
	PolicyOut string    `json:"policy_out"`
	Digest    string    `json:"digest"`
}

// NodeServiceState reads the state of a service of a node as systemd
// reports it: "running", "stopped", and the others of systemctl.
func (c *Client) NodeServiceState(ctx context.Context, node, name string) (string, error) {
	if !validSegment(name) {
		return "", fmt.Errorf("invalid service name %q", name)
	}
	endpoint, err := nodeEndpoint(node, "services/"+name+"/state")
	if err != nil {
		return "", err
	}
	var w serviceStateWire
	if err := c.get(ctx, endpoint, nil, &w); err != nil {
		return "", fmt.Errorf("fetching the state of %s on node %s: %w", name, node, err)
	}
	if w.State == "" {
		return "", fmt.Errorf("fetching the state of %s on node %s: unexpected response: no state", name, node)
	}
	return w.State, nil
}

// GuestFirewallChange is a change of the firewall options of a guest: Enable
// when set, a policy when not empty, and the options in Delete taken back to
// their defaults. Only these can be changed: the managed network never writes
// ipfilter or macfilter.
type GuestFirewallChange struct {
	Enable    *bool
	PolicyIn  string
	PolicyOut string
	Delete    []string // policy_in, policy_out
}

// SetGuestFirewallOptions changes the firewall options of a guest under the
// digest of its firewall configuration; a stale digest is ErrStaleDigest.
func (c *Client) SetGuestFirewallOptions(ctx context.Context, node string, ref model.GuestRef, change GuestFirewallChange, digest string) error {
	form := url.Values{}
	if change.Enable != nil {
		form.Set("enable", boolValue(*change.Enable))
	}
	for _, p := range []struct{ key, policy string }{{"policy_in", change.PolicyIn}, {"policy_out", change.PolicyOut}} {
		if p.policy == "" {
			continue
		}
		if !validPolicy(p.policy) {
			return fmt.Errorf("%q is no firewall policy", p.policy)
		}
		form.Set(p.key, p.policy)
	}
	for _, key := range change.Delete {
		if key != "policy_in" && key != "policy_out" {
			return fmt.Errorf("the firewall option %q is not one pco takes back", key)
		}
	}
	if len(change.Delete) > 0 {
		form.Set("delete", strings.Join(change.Delete, ","))
	}
	if len(form) == 0 {
		return errors.New("changing no firewall option")
	}
	return c.firewallWrite(ctx, http.MethodPut, node, ref, "firewall/options", form, digest)
}

// FirewallRule is a rule of a guest's firewall. Pos is where the rule is in
// the list, the first rule at 0; the first rule that matches decides.
type FirewallRule struct {
	Pos      int
	Type     string // in, out, group
	Action   string // ACCEPT, DROP, REJECT, or the group of a group rule
	Macro    string
	Iface    string
	Source   string
	Dest     string
	Proto    string
	SPort    string
	DPort    string
	ICMPType string
	Comment  string
	Enable   bool
}

type firewallRuleWire struct {
	Pos      int             `json:"pos"`
	Type     string          `json:"type"`
	Action   string          `json:"action"`
	Macro    string          `json:"macro"`
	Iface    string          `json:"iface"`
	Source   string          `json:"source"`
	Dest     string          `json:"dest"`
	Proto    string          `json:"proto"`
	SPort    json.RawMessage `json:"sport"`
	DPort    json.RawMessage `json:"dport"`
	ICMPType string          `json:"icmp-type"`
	Comment  string          `json:"comment"`
	Enable   flexBool        `json:"enable"`
	Digest   string          `json:"digest"`
}

// GuestFirewallRules lists the rules of a guest's firewall in their order,
// with the digest of the guest's firewall configuration they were read in;
// a list without rules has no digest.
func (c *Client) GuestFirewallRules(ctx context.Context, node string, ref model.GuestRef) ([]FirewallRule, string, error) {
	endpoint, err := guestEndpoint(node, ref.Kind, ref.VMID, "firewall/rules")
	if err != nil {
		return nil, "", err
	}
	var rows []firewallRuleWire
	if err := c.get(ctx, endpoint, nil, &rows); err != nil {
		return nil, "", fmt.Errorf("fetching the firewall rules of %s: %w", ref, err)
	}
	rules := make([]FirewallRule, 0, len(rows))
	var digest string
	for _, w := range rows {
		sport, _ := stringify(w.SPort)
		dport, _ := stringify(w.DPort)
		rules = append(rules, FirewallRule{
			Pos: w.Pos, Type: w.Type, Action: w.Action, Macro: w.Macro, Iface: w.Iface, Source: w.Source, Dest: w.Dest,
			Proto: w.Proto, SPort: sport, DPort: dport, ICMPType: w.ICMPType, Comment: w.Comment, Enable: bool(w.Enable),
		})
		digest = cmp.Or(digest, w.Digest)
	}
	slices.SortStableFunc(rules, func(a, b FirewallRule) int { return a.Pos - b.Pos })
	return rules, digest, nil
}

// AddGuestFirewallRule adds an in or out rule to a guest's firewall under
// the digest of its configuration. Proxmox puts a new rule first in the
// list; Pos is not sent.
func (c *Client) AddGuestFirewallRule(ctx context.Context, node string, ref model.GuestRef, rule FirewallRule, digest string) error {
	if rule.Type != "in" && rule.Type != "out" {
		return fmt.Errorf("a rule of type %q is not one pco adds", rule.Type)
	}
	if rule.Macro != "" || rule.ICMPType != "" {
		return errors.New("pco adds no rule with a macro or an icmp type")
	}
	if digest == "" {
		return errors.New("adding a firewall rule needs the digest of the list")
	}
	form := url.Values{"type": {rule.Type}, "action": {rule.Action}, "enable": {boolValue(rule.Enable)}}
	for _, f := range []struct{ key, value string }{
		{"iface", rule.Iface}, {"source", rule.Source}, {"dest", rule.Dest}, {"proto", rule.Proto},
		{"sport", rule.SPort}, {"dport", rule.DPort}, {"comment", rule.Comment},
	} {
		if f.value != "" {
			form.Set(f.key, f.value)
		}
	}
	return c.firewallWrite(ctx, http.MethodPost, node, ref, "firewall/rules", form, digest)
}

// DeleteGuestFirewallRule deletes the rule at pos under the digest of the
// list it was read in; a rule removed moves those after it up.
func (c *Client) DeleteGuestFirewallRule(ctx context.Context, node string, ref model.GuestRef, pos int, digest string) error {
	if pos < 0 {
		return fmt.Errorf("%d is no position of a rule", pos)
	}
	if digest == "" {
		return errors.New("deleting a firewall rule needs the digest of the list")
	}
	return c.firewallWrite(ctx, http.MethodDelete, node, ref, "firewall/rules/"+strconv.Itoa(pos), url.Values{}, digest)
}

// IPSet is an ipset of a guest's firewall.
type IPSet struct {
	Name    string
	Comment string
}

// IPSetEntry is an entry of an ipset: an address or a prefix, as Proxmox
// spells it, and whether it is an exception.
type IPSetEntry struct {
	CIDR    string
	NoMatch bool
	Comment string
}

type ipsetWire struct {
	Name    string `json:"name"`
	Comment string `json:"comment"`
}

type ipsetEntryWire struct {
	CIDR    string   `json:"cidr"`
	NoMatch flexBool `json:"nomatch"`
	Comment string   `json:"comment"`
	Digest  string   `json:"digest"`
}

// GuestIPSets lists the ipsets of a guest's firewall.
func (c *Client) GuestIPSets(ctx context.Context, node string, ref model.GuestRef) ([]IPSet, error) {
	endpoint, err := guestEndpoint(node, ref.Kind, ref.VMID, "firewall/ipset")
	if err != nil {
		return nil, err
	}
	var rows []ipsetWire
	if err := c.get(ctx, endpoint, nil, &rows); err != nil {
		return nil, fmt.Errorf("fetching the ipsets of %s: %w", ref, err)
	}
	sets := make([]IPSet, 0, len(rows))
	for _, w := range rows {
		sets = append(sets, IPSet(w))
	}
	return sets, nil
}

// GuestIPSet lists the entries of an ipset of a guest's firewall, with the
// digest of the set they were read in; an empty set has none.
func (c *Client) GuestIPSet(ctx context.Context, node string, ref model.GuestRef, name string) ([]IPSetEntry, string, error) {
	if !validIPSetName(name) {
		return nil, "", fmt.Errorf("%q is no ipset name", name)
	}
	endpoint, err := guestEndpoint(node, ref.Kind, ref.VMID, "firewall/ipset/"+name)
	if err != nil {
		return nil, "", err
	}
	var rows []ipsetEntryWire
	if err := c.get(ctx, endpoint, nil, &rows); err != nil {
		return nil, "", fmt.Errorf("fetching the ipset %s of %s: %w", name, ref, err)
	}
	entries := make([]IPSetEntry, 0, len(rows))
	var digest string
	for _, w := range rows {
		entries = append(entries, IPSetEntry{CIDR: w.CIDR, NoMatch: bool(w.NoMatch), Comment: w.Comment})
		digest = cmp.Or(digest, w.Digest)
	}
	return entries, digest, nil
}

// CreateGuestIPSet makes an empty ipset in a guest's firewall.
func (c *Client) CreateGuestIPSet(ctx context.Context, node string, ref model.GuestRef, name, comment string) error {
	if !validIPSetName(name) {
		return fmt.Errorf("%q is no ipset name", name)
	}
	form := url.Values{"name": {name}}
	if comment != "" {
		form.Set("comment", comment)
	}
	return c.firewallWrite(ctx, http.MethodPost, node, ref, "firewall/ipset", form, "")
}

// AddGuestIPSetEntry adds an address or a prefix to an ipset of a guest's
// firewall.
func (c *Client) AddGuestIPSetEntry(ctx context.Context, node string, ref model.GuestRef, name, cidr, comment string) error {
	if !validIPSetName(name) {
		return fmt.Errorf("%q is no ipset name", name)
	}
	if _, err := cidrSegment(cidr); err != nil {
		return err
	}
	form := url.Values{"cidr": {cidr}}
	if comment != "" {
		form.Set("comment", comment)
	}
	return c.firewallWrite(ctx, http.MethodPost, node, ref, "firewall/ipset/"+name, form, "")
}

// DeleteGuestIPSetEntry removes an entry of an ipset of a guest's firewall
// under the digest of the set.
func (c *Client) DeleteGuestIPSetEntry(ctx context.Context, node string, ref model.GuestRef, name, cidr, digest string) error {
	if !validIPSetName(name) {
		return fmt.Errorf("%q is no ipset name", name)
	}
	seg, err := cidrSegment(cidr)
	if err != nil {
		return err
	}
	return c.firewallWrite(ctx, http.MethodDelete, node, ref, "firewall/ipset/"+name+"/"+seg, url.Values{}, digest)
}

// DeleteGuestIPSet removes an empty ipset of a guest's firewall; Proxmox
// refuses one that has entries.
func (c *Client) DeleteGuestIPSet(ctx context.Context, node string, ref model.GuestRef, name string) error {
	if !validIPSetName(name) {
		return fmt.Errorf("%q is no ipset name", name)
	}
	return c.firewallWrite(ctx, http.MethodDelete, node, ref, "firewall/ipset/"+name, url.Values{}, "")
}

// firewallWrite sends a change of a guest's firewall, under digest when it
// is not empty; a stale digest is ErrStaleDigest.
func (c *Client) firewallWrite(ctx context.Context, method, node string, ref model.GuestRef, tail string, form url.Values, digest string) error {
	endpoint, err := guestEndpoint(node, ref.Kind, ref.VMID, tail)
	if err != nil {
		return err
	}
	if digest != "" {
		form.Set("digest", digest)
	}
	if err := c.send(ctx, method, endpoint, form, nil); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusInternalServerError &&
			SaysStaleDigest(apiErr.Message) {
			return fmt.Errorf("changing the firewall of %s: %w: %w", ref, ErrStaleDigest, err)
		}
		return fmt.Errorf("changing the firewall of %s: %w", ref, err)
	}
	return nil
}

// validIPSetName accepts the names Proxmox gives an ipset: a letter, then
// letters, digits, - and _.
func validIPSetName(name string) bool {
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && (r >= '0' && r <= '9' || r == '-' || r == '_'):
		default:
			return false
		}
	}
	return name != ""
}

// cidrSegment is an address or a prefix as one segment of a path, with the
// slash of a prefix escaped.
func cidrSegment(cidr string) (string, error) {
	if addr, err := netip.ParseAddr(cidr); err == nil {
		return addr.String(), nil
	}
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", fmt.Errorf("%q is no cidr of an ipset", cidr)
	}
	return p.Addr().String() + "%2F" + strconv.Itoa(p.Bits()), nil
}

func validPolicy(p string) bool { return p == "ACCEPT" || p == "DROP" || p == "REJECT" }

func boolValue(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
