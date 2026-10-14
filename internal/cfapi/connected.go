package cfapi

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// Connected says, for each tunnel of an install that api sees, the shared
// one and those pinned to a node, which connectors are connected and from
// where, one line each. A listing that fails is an error, never fewer lines:
// a caller that refuses while anything is connected must not take an
// unread account for an empty one.
func Connected(ctx context.Context, api API, installID string) ([]string, error) {
	accounts, err := AccountsOf(ctx, api)
	if err != nil {
		return nil, err
	}
	shared := planner.TunnelName(installID)
	var out []string
	for _, account := range accounts {
		tunnels, err := api.Tunnels(ctx, account, shared)
		if err != nil {
			return nil, fmt.Errorf("listing the tunnels of account %s: %w", account, err)
		}
		for _, t := range tunnels {
			if t.Name != shared && !strings.HasPrefix(t.Name, shared+"-") {
				continue
			}
			conns, err := api.Connectors(ctx, account, t.ID)
			if err != nil {
				return nil, fmt.Errorf("listing the connectors of tunnel %s (%s): %w", t.Name, t.ID, err)
			}
			for _, c := range conns {
				if c.Connections > 0 {
					out = append(out, fmt.Sprintf("tunnel %s has connector %s connected from %s", t.Name, c.ID, cmp.Or(c.OriginIP, "an unknown address")))
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// AccountsOf are the accounts a credential lists and those of its zones,
// sorted.
func AccountsOf(ctx context.Context, api API) ([]string, error) {
	listed, err := api.Accounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the accounts: %w", err)
	}
	zones, err := api.Zones(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the zones: %w", err)
	}
	accounts := map[string]bool{}
	for _, a := range listed {
		accounts[a.ID] = true
	}
	for _, z := range zones {
		accounts[z.AccountID] = true
	}
	return slices.Sorted(maps.Keys(accounts)), nil
}
