package engine

import (
	"maps"
	"net/netip"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// snapshot is the desired state of this node as the cycle has it: every
// winner a target this node resolves, at the level the settings require,
// with its guest and the guests that share a MAC with it; the nodes, the
// denylist, the settings the agent works by; and the tunnels of the
// configurations last verified at Cloudflare, each with the targets it sends
// a connector to, which the node keeps as they are.
func (c *cycleRun) snapshot() agentproto.Snapshot {
	s := agentproto.Snapshot{
		Protocol: agentproto.ProtocolMax,
		Node:     c.e.d.Node,
		State:    agentproto.Active,
		Settings: agentproto.Settings{
			PollInterval:     time.Duration(c.settings.PollInterval),
			IdentityMinimum:  string(c.requiredLevel()),
			TrustStatic:      c.settings.TrustStatic,
			TrustedCIDRs:     slices.Clone(c.settings.TrustedCIDRs),
			ReverifyInterval: time.Duration(c.settings.ReverifyInterval),
		},
		Nodes:   c.snapshotNodes(),
		Deny:    c.snapshotDeny(),
		Targets: c.snapshotTargets(),
		Blocks:  []agentproto.Block{},
		Actions: []agentproto.Action{},
	}
	if c.writerRead {
		s.Epoch = agentproto.Epoch{InstallID: c.install.ID, Generation: c.e.us.Generation, Nonce: c.e.us.Nonce}
	} else {
		s.Epoch = agentproto.Epoch{InstallID: c.install.ID}
	}
	s.Guests = c.snapshotGuests(s.Targets)
	s.Tunnels = c.withVerified(nil)
	return s
}

// snapshotNodes are the nodes of the inventory with every address they are
// configured with.
func (c *cycleRun) snapshotNodes() []agentproto.Node {
	out := make([]agentproto.Node, 0, len(c.snap.Nodes))
	for _, n := range c.snap.Nodes {
		node := agentproto.Node{Name: n.Name, Addr: n.Addr, Online: n.Online, Local: n.Local}
		for _, ifc := range n.Ifaces {
			for _, p := range ifc.Addrs {
				node.Addrs = append(node.Addrs, p.Addr())
			}
		}
		out = append(out, node)
	}
	return out
}

// snapshotDeny is the denylist of the cycle: the addresses of the nodes, also
// those seen before, which only a manual route with allowNode reaches; the
// gateways of the SDN subnets, which no route reaches; and the soft-denied.
func (c *cycleRun) snapshotDeny() agentproto.DenyList {
	text := func(m map[netip.Addr]string) map[string]string {
		if len(m) == 0 {
			return nil
		}
		out := make(map[string]string, len(m))
		for addr, why := range m {
			out[addr.String()] = why
		}
		return out
	}
	return agentproto.DenyList{
		Denied:  slices.Clone(c.e.addrs.list),
		Reasons: text(sdnGateways(c.access.subnets)),
		Soft:    text(softMap(c.soft)),
	}
}

// snapshotTargets are the winners, each a target this node resolves.
func (c *cycleRun) snapshotTargets() []agentproto.Target {
	required := string(c.requiredLevel())
	out := make([]agentproto.Target, 0, len(c.claims.Winners))
	for _, rt := range c.claims.Winners {
		t := agentproto.Target{
			Key:       rt.Hostname,
			Tunnels:   []string{},
			Route:     rt,
			Port:      rt.Target.Port,
			Required:  required,
			Resolve:   true,
			AllowNode: rt.Source == model.SourceManual && rt.Options.AllowNode,
		}
		if rt.Guest != nil {
			if g, ok := c.snap.Guest(*rt.Guest); ok {
				t.Guest, t.HostingNode = &g, g.Node
			}
		}
		out = append(out, t)
	}
	return out
}

// snapshotGuests are the guests besides those of the targets that may run
// with a MAC of a target's guest: resolution then proves nothing of it.
func (c *cycleRun) snapshotGuests(targets []agentproto.Target) []model.Guest {
	macs, own := map[string]bool{}, map[model.GuestRef]bool{}
	for _, t := range targets {
		if t.Guest == nil {
			continue
		}
		own[t.Guest.Ref] = true
		for _, n := range t.Guest.NICs {
			if mac, err := model.NormalizeMAC(n.MAC); err == nil {
				macs[mac] = true
			}
		}
	}
	var out []model.Guest
	for _, g := range c.snap.Guests {
		if own[g.Ref] || !g.Running && !g.StatusUnknown {
			continue
		}
		if slices.ContainsFunc(g.NICs, func(n model.NIC) bool {
			mac, err := model.NormalizeMAC(n.MAC)
			return err == nil && macs[mac]
		}) {
			out = append(out, g)
		}
	}
	return out
}

// withVerified gives each tunnel the targets of its account's configuration
// last verified at Cloudflare, and adds, for every other account with one,
// a tunnel the node keeps as it is: the egress set keeps what they send a
// connector to until a configuration without it is verified.
func (c *cycleRun) withVerified(tunnels []agentproto.Tunnel) []agentproto.Tunnel {
	out := make([]agentproto.Tunnel, 0, len(tunnels)+len(c.e.verified))
	listed := map[string]bool{}
	for _, t := range tunnels {
		t.Verified = toAddrPorts(c.e.verified[t.AccountID])
		listed[t.AccountID] = true
		out = append(out, t)
	}
	for _, account := range slices.Sorted(maps.Keys(c.e.verified)) {
		if listed[account] {
			continue
		}
		out = append(out, agentproto.Tunnel{
			AccountID: account, TunnelID: c.e.seenIn(account), Name: planner.TunnelName(c.install.ID),
			Kind: agentproto.TunnelShared, Verified: toAddrPorts(c.e.verified[account]),
		})
	}
	return out
}

// seenIn is the id of the tunnel of this install seen in an account; empty
// when none was.
func (e *Engine) seenIn(account string) string {
	for _, id := range slices.Sorted(maps.Keys(e.seen)) {
		if e.seen[id].account == account {
			return id
		}
	}
	return ""
}
