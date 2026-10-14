package engine

import (
	"bytes"
	"crypto/sha256"
	"maps"
	"net/netip"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
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
		Targets: c.targetsFor(c.e.d.Node),
		Blocks:  c.snapshotBlocks(),
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

// targetsFor are the winners as the snapshot of node has them. On a node
// alone each is a target the node resolves. In a cluster the resolving node
// of a guest's route is the node that hosts the guest while its agent
// reports, and this node for any other route; every other node verifies the
// address the resolving node chose, with the MACs of the guest's own cards,
// the attestation of the guest's node and the coverage this controller
// found.
func (c *cycleRun) targetsFor(node string) []agentproto.Target {
	out := c.snapshotTargets()
	if c.fleet == nil {
		return out
	}
	out = slices.DeleteFunc(out, func(t agentproto.Target) bool { return !c.listsFor(node, t.Route) })
	for i := range out {
		t := &out[i]
		rt := t.Route
		t.Tunnels = c.tunnelIDsOf(rt)
		resolver := c.resolverOf(rt)
		t.Resolve = resolver == node
		if t.Guest == nil {
			continue
		}
		macs := guestMACs(*t.Guest)
		if !t.Resolve {
			t.Addr, t.MACs = c.chosen(rt, resolver, macs)
		}
		if t.HostingNode != node {
			t.Covered = c.cover.covered
			attested := t.MACs
			if len(attested) == 0 {
				attested = macs
			}
			for _, mac := range attested {
				if at := c.attestation(t.HostingNode, mac); at != nil {
					t.Attestation = at
					break
				}
			}
		}
	}
	return out
}

// tunnelIDsOf are the ids of the tunnels whose rules send a connector to
// the target of a route, as the placement has them: the boot gate of an
// agent starts its shared connector once a target of the shared tunnel
// verified.
func (c *cycleRun) tunnelIDsOf(rt model.Route) []string {
	decided := c.e.place.decided
	if c.placing != nil {
		decided = c.placing.decided
	}
	zone, ok := c.zoneOf(rt.Hostname)
	d, placedAt := decided[rt.Owner()]
	out := []string{}
	if !ok || !placedAt {
		return out
	}
	for _, p := range []*placed{&d.record, d.also} {
		if p == nil {
			continue
		}
		if id := c.e.seenIn(tunnelRef{zone.AccountID, c.tunnelOf(*p)}); id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// resolverOf is the node that resolves the target of a route: the node the
// guest runs on while its agent reports fresh and is served, else this one;
// a manual route's is this node unless it is drained.
func (c *cycleRun) resolverOf(rt model.Route) string {
	if rt.Guest == nil {
		return c.manualResolver()
	}
	g, ok := c.snap.Guest(*rt.Guest)
	if !ok || g.Node == "" {
		return c.e.d.Node
	}
	if f := c.fleet.node(g.Node); g.Node != c.e.d.Node && (!f.fresh || !servedState(f.entry.State)) {
		return c.e.d.Node
	}
	return g.Node
}

// chosen is the address the resolving node chose for a route and the MACs
// it chose it with, as far as they are the guest's own: what its report
// says, and while it says nothing, what the stored placement holds of that
// node's choice. None when neither says.
func (c *cycleRun) chosen(rt model.Route, resolver string, own []string) (netip.Addr, []string) {
	var addr netip.Addr
	var mac string
	if resolver == c.e.d.Node {
		if res, ok := c.results[rt.Hostname]; ok && !res.Target.Withdrawn && !res.Target.Rejected {
			addr = res.Target.Addr
			if res.Binding != nil {
				mac = res.Binding.MAC
			}
		}
	} else if tr, ok := c.resolvedOn(resolver, rt); ok {
		addr = tr.Addr
		if res, err := agent.DecodeResult(tr.Result); err == nil && res.Binding != nil {
			mac = res.Binding.MAC
		}
	}
	if !addr.IsValid() {
		return netip.Addr{}, nil
	}
	if mac == "" || own == nil {
		return addr, own
	}
	if mac, err := model.NormalizeMAC(mac); err == nil && slices.Contains(own, mac) {
		return addr, []string{mac}
	}
	// Chosen with a MAC that is no card of the guest now: it vouches for
	// nothing until the node resolves the route again.
	return netip.Addr{}, nil
}

// resolvedOn is what a node resolved of a route in its last report: only
// a fresh report of this controller's epoch, of a snapshot that listed the
// target, that says it resolved it. Nothing older or stored stands for it.
func (c *cycleRun) resolvedOn(node string, rt model.Route) (agentproto.TargetReport, bool) {
	tr, ok := c.reportOn(node, rt, true)
	if !ok || !tr.Addr.IsValid() || !tr.OK {
		return agentproto.TargetReport{}, false
	}
	return tr, true
}

// reportOn is what a node said of a route in its last report, when that
// report is fresh, of this controller's epoch and of a snapshot that listed
// the target.
func (c *cycleRun) reportOn(node string, rt model.Route, resolve bool) (agentproto.TargetReport, bool) {
	f := c.fleet.node(node)
	tr, ok := f.targets[rt.Hostname]
	st := f.report.Status
	since, listed := c.e.place.listed(node, rt.Hostname, resolve)
	if !ok || !listed || !f.fresh || st.EpochSeen != c.epoch() || st.SeqApplied < since {
		return agentproto.TargetReport{}, false
	}
	return tr, true
}

// guestMACs are the MACs of a guest's own cards.
func guestMACs(g model.Guest) []string {
	var out []string
	for _, n := range g.NICs {
		if mac, err := model.NormalizeMAC(n.MAC); err == nil {
			out = append(out, mac)
		}
	}
	return out
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
// connector of with token, or, without one, keeps as it is; pinned when its
// name is that of a tunnel pinned to a node.
func (c *cycleRun) snapshotTunnel(account, id, name, token string) agentproto.Tunnel {
	t := agentproto.Tunnel{AccountID: account, TunnelID: id, Name: name, Kind: agentproto.TunnelShared}
	if _, pinned := planner.PinnedTunnelNode(c.install.ID, name); pinned {
		t.Kind = agentproto.TunnelPinned
	}
	if token != "" {
		t.Run, t.Token = true, agentproto.NewSecret(token)
	}
	return t
}
