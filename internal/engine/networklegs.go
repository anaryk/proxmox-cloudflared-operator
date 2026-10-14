package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// probeWait is how long a probe, an attach or the setup of the managed
// network on a cluster waits for the agents of the other nodes to answer
// their probes, and freshWait how long it waits for the cycle lock after:
// the answers are judged while they are at most freshWait old, and the
// whole call stays well within the minute the command waits.
const (
	probeWait = 10 * time.Second
	freshWait = 10 * time.Second
)

// spanningZones are the types of the SDN zones whose vnets are one segment
// on every node; a vnet of any other, as a Simple zone, is a bridge of each
// node of its own.
var spanningZones = []string{"vlan", "qinq", "vxlan", "evpn"}

// legNodes is what the registry says of the nodes for the legs of a
// network: whether this node is one of a cluster, the nodes that are not
// removed, and the index of each that has one.
type legNodes struct {
	clustered bool
	known     map[string]bool
	indexes   map[string]int
}

func (e *Engine) legNodes() (legNodes, error) {
	entries, err := e.d.Store.Nodes()
	if err != nil {
		return legNodes{}, fmt.Errorf("reading the registry of the nodes: %w", err)
	}
	out := legNodes{known: map[string]bool{e.d.Node: true}, indexes: map[string]int{}}
	for _, n := range entries {
		if n.State == store.NodeRemoved {
			continue
		}
		out.known[n.Name] = true
		out.clustered = out.clustered || n.Name != e.d.Node
		if n.Index != nil {
			out.indexes[n.Name] = *n.Index
		}
	}
	return out, nil
}

// requestedLegs are the legs a request names, by node: addr on node, or on
// this node, and those of legs. On a cluster the request names the scope; a
// network of scope node has one leg. Each address is one of the prefix, and
// of one node.
func (e *Engine) requestedLegs(ln legNodes, scope, node string, addr netip.Addr, legs map[string]netip.Addr, prefix netip.Prefix) (map[string]netip.Addr, error) {
	switch scope {
	case "", store.ScopeCluster, store.ScopeNode:
	default:
		return nil, fieldError("scope", "scope %s: want cluster or node", scope)
	}
	switch {
	case ln.clustered && scope == "":
		return nil, fieldError("scope", "on a cluster, name the scope of the network: cluster for one segment on every node, "+
			"a bridge with a physical uplink or a vnet of a VLAN, QinQ, VXLAN or EVPN zone, or node for a bridge of one node")
	case node != "" && scope != store.ScopeNode:
		return nil, fieldError("node", "node %s: a node is named for a network of scope node, whose leg is on it", node)
	}
	out := map[string]netip.Addr{}
	maps.Copy(out, legs)
	if addr.IsValid() {
		at := cmp.Or(node, e.d.Node)
		if other, ok := out[at]; ok && other != addr {
			return nil, fieldError("legs", "node %s is given two legs, %s and %s", at, other, addr)
		}
		out[at] = addr
	}
	switch {
	case len(out) == 0:
		return nil, fieldError("address", "give the address of a leg, as 10.77.0.250")
	case scope == store.ScopeNode && len(out) > 1:
		return nil, fieldError("legs", "a network of scope node has one leg, on its node; this names %d", len(out))
	}
	owner := map[netip.Addr]string{}
	for _, n := range slices.Sorted(maps.Keys(out)) {
		a := out[n]
		if !ln.known[n] {
			return nil, fieldError("legs", "node %s is not a node of this install", n)
		}
		if err := checkAddress(prefix, a); err != nil {
			return nil, err
		}
		if other, dup := owner[a]; dup {
			return nil, fieldError("legs", "%s is the leg of two nodes, %s and %s", a, other, n)
		}
		owner[a] = n
	}
	return out, nil
}

// nodeAnswer is what the agent of another node answered to a probe, or why
// there is nothing to go by.
type nodeAnswer struct {
	answer agentproto.ProbeAnswer
	why    string
}

// unasked says why the agents of nodes, other than this one, cannot be
// asked for a probe: a node without a host agent that reports fresh to this
// controller as an Active node.
func (e *Engine) unasked(nodes []string) map[string]string {
	agents := e.writes.agentNodes()
	out := map[string]string{}
	for _, n := range nodes {
		if n != e.d.Node && !agents[n] {
			out[n] = fmt.Sprintf("node %s has no host agent that reports to this controller", n)
		}
	}
	return out
}

// probeNodes has the agent of each node of addrs probe its address on seg,
// a zero address only looking at the node, and waits up to probeWait for
// the answers. An answer is that node's own, and is read only within the
// bounds of the protocol; a node that did not answer, could not probe or
// answered what cannot be read has a why and nothing else.
func (e *Engine) probeNodes(ctx context.Context, seg resolve.Segment, addrs map[string]netip.Addr) map[string]nodeAnswer {
	out := map[string]nodeAnswer{}
	if len(addrs) == 0 {
		return out
	}
	args := map[string]json.RawMessage{}
	for node, addr := range addrs {
		raw, err := json.Marshal(agentproto.Probe{Bridge: seg.Bridge, VLAN: seg.VLAN, Addr: addr})
		if err != nil {
			out[node] = nodeAnswer{why: fmt.Sprintf("writing the probe of node %s: %v", node, err)}
			continue
		}
		args[node] = raw
	}
	answers, err := e.askAgents(ctx, agentproto.ActionProbe, args, probeWait)
	for node := range args {
		res, ok := answers[node]
		switch {
		case err != nil:
			out[node] = nodeAnswer{why: fmt.Sprintf("the agent of node %s was not asked: %v", node, err)}
		case !ok:
			out[node] = nodeAnswer{why: fmt.Sprintf("the agent of node %s did not answer the probe within %s", node, probeWait)}
		case !res.OK:
			out[node] = nodeAnswer{why: fmt.Sprintf("the agent of node %s could not probe: %s", node, clip(res.Error))}
		default:
			a, derr := agentproto.DecodeProbeAnswer(res.Data)
			if derr != nil {
				out[node] = nodeAnswer{why: fmt.Sprintf("the probe answer of the agent of node %s is not read: %s", node, clip(derr.Error()))}
				continue
			}
			out[node] = nodeAnswer{answer: a}
		}
	}
	return out
}

// hostIfaces are the interfaces of a probe answer in the form of the
// prober's.
func hostIfaces(ifaces []agentproto.ProbeIface) []resolve.HostIface {
	out := make([]resolve.HostIface, 0, len(ifaces))
	for _, i := range ifaces {
		out = append(out, resolve.HostIface{Name: i.Name, Master: i.Master, Segment: resolve.Segment{Bridge: i.Bridge, VLAN: i.VLAN},
			Addrs: slices.Clone(i.Addrs), Via: i.Via})
	}
	return out
}

// onNode are the facts of another node: Proxmox's configuration of its
// network as the controller read it, and no interfaces until its agent
// answered. Only the configuration says which bridges it has; what its
// agent says can only take away.
func onNode(f networkFacts, node string, cfg []pve.NodeIface) networkFacts {
	g := f
	g.node, g.remote, g.appliance, g.host, g.config = node, true, false, nil, cfg
	return g
}

// apiFacts is what the controller reads of the other nodes through the
// Proxmox API, before it takes the cycle lock: the network of each node, by
// node, and the SDN zones when the scope of a vnet is judged.
type apiFacts struct {
	configs map[string][]pve.NodeIface
	zones   []pve.SDNZone
}

// readNodes reads the network of each node of nodes but this one, and the
// zones when zones says so.
func (e *Engine) readNodes(ctx context.Context, nodes []string, zones bool) (apiFacts, error) {
	out := apiFacts{configs: map[string][]pve.NodeIface{}}
	for _, node := range slices.Sorted(slices.Values(nodes)) {
		if node == e.d.Node {
			continue
		}
		cfg, err := e.d.Networks.NodeNetwork(ctx, node)
		if err != nil {
			return out, fmt.Errorf("reading the network of node %s: %w", node, err)
		}
		out.configs[node] = cfg
	}
	if zones {
		z, err := e.d.Networks.Zones(ctx)
		if err != nil {
			return out, fmt.Errorf("reading the SDN zones: %w", err)
		}
		out.zones = z
	}
	return out, nil
}

// acquireFresh takes the cycle lock for a write within freshWait of the
// probes of the agents, so that their answers are judged while fresh; a
// cycle that holds it longer has the call refused, to be tried again.
func (e *Engine) acquireFresh(ctx context.Context) error {
	wait, cancel := e.timeout(ctx, freshWait)
	defer cancel()
	if err := e.acquire(wait); err != nil {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("%w: the answers of the agents would be older than %s once it ends; try again", ErrBusy, freshWait)
	}
	if err := e.controllerOnly(ctx); err != nil {
		e.release()
		return err
	}
	return nil
}

// withAnswer is f with the interfaces the agent of its node answered.
func (f networkFacts) withAnswer(a agentproto.ProbeAnswer) networkFacts {
	f.host = hostIfaces(a.Interfaces)
	return f
}

// notOneSegment says why seg is not one segment on every node as Proxmox
// has it configured on the node of f: neither a bridge with a physical port
// nor a vnet of a zone that spans the nodes. It is empty when it is one.
// What an agent says of its node never makes a segment one.
func (f networkFacts) notOneSegment(seg resolve.Segment) string {
	if i := slices.IndexFunc(f.vnets, func(v pve.VNet) bool { return v.Name == seg.Bridge }); i >= 0 {
		zone := f.vnets[i].Zone
		kind := "unknown"
		if j := slices.IndexFunc(f.zones, func(z pve.SDNZone) bool { return z.Name == zone }); j >= 0 {
			kind = f.zones[j].Type
		}
		if slices.Contains(spanningZones, kind) {
			return ""
		}
		return fmt.Sprintf("the vnet %s is in the zone %s of type %s, a bridge of each node of its own", seg.Bridge, zone, kind)
	}
	if cfg, ok := f.configured(seg.Bridge); ok && cfg.Type == "bridge" && f.uplink(seg.Bridge) {
		return ""
	}
	return fmt.Sprintf("%s has no physical uplink on node %s, so it is a bridge of that node alone", seg.Bridge, f.node)
}

// suggestedScope is the scope the configuration of seg on the node of f
// suggests.
func (f networkFacts) suggestedScope(seg resolve.Segment) string {
	if f.notOneSegment(seg) == "" {
		return store.ScopeCluster
	}
	return store.ScopeNode
}

// where is seg as a message names it: of another node, with the node.
func (f networkFacts) where(seg resolve.Segment) string {
	if f.remote {
		return fmt.Sprintf("%s of node %s", seg, f.node)
	}
	return seg.String()
}

// remoteRefusal is why the leg of the node of f at addr is not made, by
// what the agent of the node answered: no answer, the bridge not there,
// the node on it already, a refusal of the leg, or someone answering for
// the address. ext is the network the leg is added to, if any.
func (f networkFacts) remoteRefusal(seg resolve.Segment, r AttachRequest, addr netip.Addr, got nodeAnswer, ext string) (string, error) {
	if got.why != "" {
		return "", fmt.Errorf("%w: %s; no leg is made there", ErrRefused, got.why)
	}
	f = f.withAnswer(got.answer)
	c, err := f.lookup(seg.Bridge, seg.VLAN)
	if err != nil {
		return "", err
	}
	overlap, err := f.legRefusal(c, r, addr, ext)
	if err != nil {
		return "", err
	}
	switch {
	case got.answer.NotProbed != "":
		return "", fmt.Errorf("%w: %s was not probed on %s: %s", ErrRefused, addr, f.where(seg), clip(got.answer.NotProbed))
	case len(got.answer.AnsweredBy) > 0:
		return "", fieldError("address", "%s is in use on %s: %s answered", addr, f.where(seg), strings.Join(got.answer.AnsweredBy, ", "))
	}
	return overlap, nil
}

// legRefusal checks the leg of the node of f at addr against its facts: the
// refusals of Attach in their order, but the probe. ext is the network the
// leg is added to, whose own prefix is no overlap; it returns the route of
// the node the prefix overlaps, allowed by OverlapOK.
func (f networkFacts) legRefusal(c CandidateNetwork, r AttachRequest, addr netip.Addr, ext string) (string, error) {
	seg := resolve.Segment{Bridge: c.Bridge, VLAN: c.VLAN}
	if err := f.usable(c); err != nil {
		return "", err
	}
	if c.Attached != "" && c.Attached != ext {
		return "", fmt.Errorf("%w: network %s is attached already; detach it first to attach it anew", ErrRefused, c.Attached)
	}
	if c.Uplink && !r.ConfirmUplink {
		return "", fieldError("confirmUplink", "%s has a physical uplink: "+uplinkWarning+"; %s", f.where(seg), c.Bridge, UplinkHint)
	}
	if p := gateway.ServicePrefix; r.Prefix.Overlaps(p) {
		return "", fieldError("prefix", "%s overlaps the service prefix %s, whose addresses the connectors dial", r.Prefix, p)
	}
	if p := f.settings.Managed.Subnet; p.IsValid() && r.Prefix.Overlaps(p) {
		return "", fieldError("prefix", "%s overlaps the managed subnet %s (managed.subnet)", r.Prefix, p)
	}
	overlap := f.overlap(r.Prefix, seg)
	if overlap != "" && !r.OverlapOK {
		return "", fieldError("overlapOK", "%s; %s", overlap, OverlapHint)
	}
	for _, id := range slices.Sorted(maps.Keys(f.nets)) {
		if n := f.nets[id]; id != ext && n.Bridge == c.Bridge && n.Prefix.Overlaps(r.Prefix) {
			return "", fieldError("prefix", "%s overlaps %s of network %s on the same bridge %s", r.Prefix, n.Prefix, id, c.Bridge)
		}
	}
	if who, ok := f.taken(c)[addr]; ok {
		return "", fieldError("address", "%s is %s", addr, cmp.Or(who, "an address of the node"))
	}
	return overlap, nil
}

// probeLegs is the probe of the legs of a network on a cluster: of scope
// node, the leg of the node named, or of this one; of scope cluster, the
// legs of this node and of every other node of the install. Each node gets
// an address of its own: the one named, or the highest of the prefix that
// no guest, node or other leg is known to have. This node probes its own
// under the cycle lock; the agents of the others are asked once it is let
// go. A node whose leg would be refused, or that has no such bridge, says
// why in its leg.
func (e *Engine) probeLegs(ctx context.Context, r ProbeRequest) (ProbeResult, error) {
	ln, err := e.legNodes()
	if err != nil {
		return ProbeResult{}, err
	}
	nodes, err := e.probedNodes(ln, r)
	if err != nil {
		return ProbeResult{}, err
	}
	plan, err := e.planLegs(ctx, ln, r, nodes)
	if err != nil {
		return ProbeResult{}, err
	}
	res := plan.res
	others := map[string]netip.Addr{}
	for _, l := range res.Legs {
		if l.Node != e.d.Node && l.Problem == "" {
			others[l.Node] = l.Address
		}
	}
	unasked := e.unasked(slices.Collect(maps.Keys(others)))
	for node := range unasked {
		delete(others, node)
	}
	answers := e.probeNodes(ctx, plan.seg, others)
	for i := range res.Legs {
		l := &res.Legs[i]
		if why, ok := unasked[l.Node]; ok {
			l.Problem, l.Missing = why, true
			continue
		}
		if got, ok := answers[l.Node]; ok {
			plan.facts[l.Node].judge(l, plan.seg, res.Prefix, got)
		}
	}
	if len(res.Legs) > 0 {
		l := res.Legs[0]
		res.Address, res.Free, res.AnsweredBy, res.Uplink, res.Overlap = l.Address, l.Free, l.AnsweredBy, l.Uplink, l.Overlap
		if l.Uplink {
			res.Warning = fmt.Sprintf(uplinkWarning, res.Bridge)
		}
	}
	return res, nil
}

// probedNodes are the nodes a probe of a scope is of, this node first.
func (e *Engine) probedNodes(ln legNodes, r ProbeRequest) ([]string, error) {
	var nodes []string
	switch r.Scope {
	case store.ScopeNode:
		if len(r.Legs) > 0 {
			return nil, fieldError("legs", "a network of scope node has one leg; name its node with node")
		}
		nodes = []string{cmp.Or(r.Node, e.d.Node)}
	case store.ScopeCluster:
		if r.Node != "" {
			return nil, fieldError("node", "node %s: a node is named for a network of scope node, whose leg is on it", r.Node)
		}
		nodes = []string{e.d.Node}
		for _, n := range slices.Sorted(maps.Keys(ln.known)) {
			if n != e.d.Node {
				nodes = append(nodes, n)
			}
		}
	default:
		return nil, fieldError("scope", "scope %q: want cluster or node", r.Scope)
	}
	for _, n := range nodes {
		if !ln.known[n] {
			return nil, fieldError("node", "node %s is not a node of this install", n)
		}
	}
	for n := range r.Legs {
		if !slices.Contains(nodes, n) {
			return nil, fieldError("legs", "node %s is not a node of this install", n)
		}
	}
	return nodes, nil
}

// legPlan is a probe of legs as this node made it: the result so far, with
// a leg of each node, and the facts of the other nodes, which their agents'
// answers complete.
type legPlan struct {
	res   ProbeResult
	seg   resolve.Segment
	facts map[string]networkFacts
}

// planLegs reads the facts of each node, the other nodes' through the API
// before the cycle lock, chooses the prefix and the address of each leg,
// and probes the leg of this node, under the cycle lock.
func (e *Engine) planLegs(ctx context.Context, ln legNodes, r ProbeRequest, nodes []string) (legPlan, error) {
	api, err := e.readNodes(ctx, nodes, ln.clustered)
	if err != nil {
		return legPlan{}, err
	}
	if err := e.acquireAdmin(ctx); err != nil {
		return legPlan{}, err
	}
	defer e.release()
	f, err := e.networkFacts(ctx)
	if err != nil {
		return legPlan{}, err
	}
	f.zones = api.zones
	seg := resolve.Segment{Bridge: r.Bridge, VLAN: r.VLAN}
	plan := legPlan{seg: seg, facts: map[string]networkFacts{}}
	res := ProbeResult{Bridge: r.Bridge, VLAN: r.VLAN, Prefix: r.Prefix, AnsweredBy: []string{}, Clustered: ln.clustered,
		Suggested: f.suggestion(seg), Scope: r.Scope, Legs: []LegProbe{}}
	cands := map[string]CandidateNetwork{}
	for _, node := range nodes {
		g := f
		if node != e.d.Node {
			g = onNode(f, node, api.configs[node])
			plan.facts[node] = g
		}
		if g.knownBridge(seg.Bridge) {
			cands[node] = g.candidate(seg)
		}
	}
	if !res.Prefix.IsValid() {
		i := slices.IndexFunc(nodes, func(n string) bool { c, ok := cands[n]; return ok && len(c.Prefixes) > 0 })
		if i < 0 {
			return legPlan{}, fieldError("prefix", "no guest address is known on %s; give the prefix of the network, as 10.77.0.0/24", seg)
		}
		c := cands[nodes[i]]
		res.Prefix = c.Prefixes[0]
		res.Note = prefixNote(c, seg)
	}
	if err := checkPrefix(res.Prefix); err != nil {
		return legPlan{}, err
	}
	taken := map[netip.Addr]string{}
	for _, node := range nodes {
		if c, ok := cands[node]; ok {
			g := f
			if node != e.d.Node {
				g = plan.facts[node]
			}
			maps.Copy(taken, g.taken(c))
		}
	}
	for _, node := range nodes {
		l := LegProbe{Node: node, AnsweredBy: []string{}}
		switch addr, named := r.Legs[node]; {
		case named:
			l.Address = addr
		case r.Address.IsValid() && node == cmp.Or(r.Node, e.d.Node):
			l.Address = r.Address
		default:
			a, ok := propose(res.Prefix, taken)
			if !ok {
				l.Problem = fmt.Sprintf("every address of %s is taken by a guest, a node or another leg", res.Prefix)
			}
			l.Address = a
		}
		if l.Address.IsValid() {
			if err := checkAddress(res.Prefix, l.Address); err != nil {
				return legPlan{}, err
			}
			taken[l.Address] = "the leg of node " + node
		}
		switch _, known := cands[node]; {
		case l.Problem != "":
		case !known:
			l.Problem, l.Missing = fmt.Sprintf("%s is no bridge or vnet of node %s", seg.Bridge, node), true
		case node == e.d.Node:
			if err := e.probeHere(ctx, f, seg, res.Prefix, &l); err != nil {
				return legPlan{}, err
			}
		}
		res.Legs = append(res.Legs, l)
	}
	plan.res = res
	return plan, nil
}

// probeHere probes the leg l of this node.
func (e *Engine) probeHere(ctx context.Context, f networkFacts, seg resolve.Segment, prefix netip.Prefix, l *LegProbe) error {
	c, err := f.lookup(seg.Bridge, seg.VLAN)
	if err != nil {
		l.Problem, l.Missing = err.Error(), true
		return nil
	}
	if err := f.usable(c); err != nil {
		l.Problem = refusedWhy(err)
		return nil
	}
	l.Uplink, l.Overlap = c.Uplink, f.overlap(prefix, seg)
	if who, ok := f.taken(c)[l.Address]; ok {
		l.Problem = fmt.Sprintf("%s is %s", l.Address, who)
		return nil
	}
	if !f.canProbe(c) {
		l.Problem = fmt.Sprintf(notProbed, seg, l.Address)
		return nil
	}
	answers, err := e.probe(ctx, seg, l.Address)
	if err != nil {
		return err
	}
	l.AnsweredBy, l.Free = answers, len(answers) == 0
	return nil
}

// judge completes the leg l of the node of f with what its agent answered.
func (f networkFacts) judge(l *LegProbe, seg resolve.Segment, prefix netip.Prefix, got nodeAnswer) {
	if got.why != "" {
		l.Problem, l.Missing = got.why, true
		return
	}
	f = f.withAnswer(got.answer)
	c, err := f.lookup(seg.Bridge, seg.VLAN)
	if err != nil {
		l.Problem, l.Missing = err.Error(), true
		return
	}
	if err := f.usable(c); err != nil {
		l.Problem = refusedWhy(err)
		return
	}
	l.Uplink, l.Overlap = c.Uplink, f.overlap(prefix, seg)
	switch who, taken := f.taken(c)[l.Address]; {
	case taken:
		l.Problem = fmt.Sprintf("%s is %s", l.Address, who)
	case got.answer.NotProbed != "":
		l.Problem = fmt.Sprintf("%s was not probed on %s: %s", l.Address, f.where(seg), clip(got.answer.NotProbed))
	default:
		l.AnsweredBy, l.Free = got.answer.AnsweredBy, len(got.answer.AnsweredBy) == 0
	}
}

// knownBridge says that name is a bridge or vnet the node of f has as far as
// the controller knows before its agent answered: one Proxmox has
// configured there, or a vnet; of this node, as isBridge says.
func (f networkFacts) knownBridge(name string) bool {
	if !f.remote {
		return f.isBridge(name)
	}
	c, ok := f.configured(name)
	return ok && c.Type == "bridge" || f.isVnet(name)
}

// refusedWhy is the reason of a refusal without the word that it is one.
func refusedWhy(err error) string {
	return strings.TrimPrefix(err.Error(), ErrRefused.Error()+": ")
}
