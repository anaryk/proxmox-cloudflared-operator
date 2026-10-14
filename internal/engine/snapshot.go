package engine

import (
	"bytes"
	"crypto/sha256"
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
// a connector to, which the node keeps as they are. It is what the cycle's
// pass of the agent begins with.
func (c *cycleRun) snapshot() agentproto.Snapshot {
	s := c.desired()
	s.Tunnels = c.withVerified(nil, c.e.d.Node)
	return s
}

// keptSnapshot is the snapshot the cycle's pass of the agent ends with,
// after the tunnel run: the tunnels whose connectors the node keeps, with the
// run token of those it runs and the targets of their configurations as last
// verified; with prune they are every tunnel of the install whose connector
// the node keeps.
func (c *cycleRun) keptSnapshot(tunnels []agentproto.Tunnel, prune bool) agentproto.Snapshot {
	s := c.desired()
	s.Tunnels, s.Prune = c.withVerified(tunnels, ""), prune
	return s
}

// desired is the snapshot of the cycle without its tunnels.
func (c *cycleRun) desired() agentproto.Snapshot {
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

// withVerified gives each tunnel the targets of its configuration last
// verified at Cloudflare, and for a node adds every other tunnel with one
// that the node may run, which it keeps as it is: the shared tunnel of an
// account and the one pinned to that node. The egress set keeps what they
// send a connector to until a configuration without it is verified; a
// tunnel pinned to another node vouches for nothing on this one. An empty
// node adds none.
func (c *cycleRun) withVerified(tunnels []agentproto.Tunnel, node string) []agentproto.Tunnel {
	own := ""
	if node != "" {
		own, _ = planner.PinnedTunnelName(c.install.ID, node)
	}
	others := func(ref tunnelRef) bool {
		return node != "" && (ref.name == planner.TunnelName(c.install.ID) || own != "" && ref.name == own)
	}
	out := make([]agentproto.Tunnel, 0, len(tunnels)+len(c.e.verified))
	listed := map[tunnelRef]bool{}
	for _, t := range tunnels {
		ref := tunnelRef{t.AccountID, t.Name}
		t.Verified = toAddrPorts(c.e.verified[ref])
		listed[ref] = true
		out = append(out, t)
	}
	for _, ref := range slices.SortedFunc(maps.Keys(c.e.verified), compareTunnelRefs) {
		if listed[ref] || !others(ref) {
			continue
		}
		t := agentproto.Tunnel{
			AccountID: ref.account, TunnelID: c.e.seenIn(ref), Name: ref.name,
			Kind: agentproto.TunnelShared, Verified: toAddrPorts(c.e.verified[ref]),
		}
		if _, pinned := planner.PinnedTunnelNode(c.install.ID, ref.name); pinned {
			t.Kind = agentproto.TunnelPinned
		}
		out = append(out, t)
	}
	return out
}

// seenIn is the id of a tunnel of this install seen in its account; empty
// when none was.
func (e *Engine) seenIn(ref tunnelRef) string {
	for _, id := range slices.Sorted(maps.Keys(e.seen)) {
		if t := e.seen[id]; t.account == ref.account && t.name == ref.name {
			return id
		}
	}
	return ""
}

// seqs numbers the snapshots of each node within an epoch: the seq moves only
// when the snapshot's content changes, so that an agent writes nothing for a
// snapshot it has, and never two contents under one seq.
type seqs struct {
	byNode map[string]*seqOf
}

type seqOf struct {
	epoch agentproto.Epoch
	seq   uint64
	sum   [sha256.Size]byte
}

// floor is the least seq a snapshot of s's epoch for s's node gets from
// stamp, given what the node's agent applied.
func (q *seqs) floor(s agentproto.Snapshot, applied agentproto.Epoch, appliedSeq uint64) uint64 {
	var seq uint64
	if applied == s.Epoch {
		seq = appliedSeq
	}
	if cur := q.byNode[s.Node]; cur != nil && cur.epoch == s.Epoch {
		seq = max(seq, cur.seq)
	}
	return seq
}

// stamp gives s its seq. applied is what the node's agent applied last,
// epoch and seq: after a start of the engine within one epoch, the seqs go on
// above it.
func (q *seqs) stamp(s *agentproto.Snapshot, applied agentproto.Epoch, appliedSeq uint64) {
	if q.byNode == nil {
		q.byNode = map[string]*seqOf{}
	}
	cur := q.byNode[s.Node]
	if cur == nil || cur.epoch != s.Epoch {
		cur = &seqOf{epoch: s.Epoch}
		if applied == s.Epoch {
			cur.seq = appliedSeq
		}
		q.byNode[s.Node] = cur
	}
	s.Seq = 0
	var buf bytes.Buffer
	sum := [sha256.Size]byte{}
	if err := agentproto.EncodeSnapshot(&buf, *s); err == nil {
		sum = sha256.Sum256(buf.Bytes())
	}
	// One that does not encode, which no snapshot does, gets a seq of its own.
	if sum == ([sha256.Size]byte{}) || sum != cur.sum || cur.seq == 0 {
		cur.sum = sum
		cur.seq++
	}
	s.Seq = cur.seq
}

// snapshotTunnel is a tunnel as a snapshot names it: one the node runs a
// connector of with token, or, without one, keeps as it is.
func snapshotTunnel(account, id, name, token string) agentproto.Tunnel {
	t := agentproto.Tunnel{AccountID: account, TunnelID: id, Name: name, Kind: agentproto.TunnelShared}
	if token != "" {
		t.Run, t.Token = true, agentproto.NewSecret(token)
	}
	return t
}
