package pve

import (
	"context"
	"fmt"
	"slices"
)

// ClusterFirewallRules lists the rules of the datacenter firewall in their
// order.
func (c *Client) ClusterFirewallRules(ctx context.Context) ([]FirewallRule, error) {
	var rows []firewallRuleWire
	if err := c.get(ctx, "cluster/firewall/rules", nil, &rows); err != nil {
		return nil, fmt.Errorf("fetching the rules of the datacenter firewall: %w", err)
	}
	rules, _ := rulesOf(rows)
	return rules, nil
}

// ClusterIPSet lists the entries of an ipset of the datacenter firewall;
// found is false when the datacenter has no such ipset.
func (c *Client) ClusterIPSet(ctx context.Context, name string) (entries []IPSetEntry, found bool, err error) {
	if !validIPSetName(name) {
		return nil, false, fmt.Errorf("%q is no ipset name", name)
	}
	var sets []ipsetWire
	if err := c.get(ctx, "cluster/firewall/ipset", nil, &sets); err != nil {
		return nil, false, fmt.Errorf("fetching the ipsets of the datacenter firewall: %w", err)
	}
	if !slices.ContainsFunc(sets, func(s ipsetWire) bool { return s.Name == name }) {
		return nil, false, nil
	}
	var rows []ipsetEntryWire
	if err := c.get(ctx, "cluster/firewall/ipset/"+name, nil, &rows); err != nil {
		return nil, false, fmt.Errorf("fetching the ipset %s of the datacenter firewall: %w", name, err)
	}
	entries = make([]IPSetEntry, 0, len(rows))
	for _, w := range rows {
		entries = append(entries, IPSetEntry{CIDR: w.CIDR, NoMatch: bool(w.NoMatch), Comment: w.Comment})
	}
	return entries, true, nil
}
