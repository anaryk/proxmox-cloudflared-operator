package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Networks is what the admin actions of the networks read and run beside
// the store, the inventory and the gateway.
type Networks interface {
	// NodeNetwork lists the interfaces of a node as Proxmox has them
	// configured: the bridges, their ports and whether they filter VLANs.
	NodeNetwork(ctx context.Context, node string) ([]pve.NodeIface, error)
	// VNets lists the SDN vnets.
	VNets(ctx context.Context) ([]pve.VNet, error)
	// Interfaces are the interfaces of the node as they are now, with their
	// addresses and the segments they are on.
	Interfaces(ctx context.Context) ([]resolve.HostIface, error)
	// StartNamespace brings the gateway namespace up as pco-netns.service
	// does, and returns once it is up. EnableNamespace has the unit start at
	// boot as well; DisableNamespace no longer, which takes nothing down:
	// the unit has no stop action. In the appliance, which has no such
	// unit, they do nothing.
	StartNamespace(ctx context.Context) error
	EnableNamespace(ctx context.Context) error
	DisableNamespace(ctx context.Context) error
	// Zones lists the SDN zones, with their types.
	Zones(ctx context.Context) ([]pve.SDNZone, error)
}

// NetworkView is a network pco reaches through a leg, with how its leg is
// and what it serves.
type NetworkView struct {
	store.Network
	Legs    []LegView `json:"legStates"` // of every node with a leg, this node first
	Targets int       `json:"targets"`   // service addresses in use
	Routes  int       `json:"routes"`    // routes served through it
}

// LegView is the leg of a network on a node, as the gateway of the node last
// said: this node's own, or as the agent of another node reported it.
type LegView struct {
	Node    string     `json:"node"`
	Addr    netip.Addr `json:"addr"`
	MAC     string     `json:"mac,omitempty"`
	Up      bool       `json:"up"`
	Problem string     `json:"problem,omitempty"`
}

// NetworkDetail is a network with its service addresses and the hostnames of
// the routes served through it. NodeServices are the service addresses of
// the guests behind the leg of each node of a network of scope node.
type NetworkDetail struct {
	NetworkView
	Services     []store.ServiceAddr     `json:"services"`
	NodeServices []store.NodeServiceAddr `json:"nodeServices,omitempty"`
	Served       []string                `json:"served"`
}

// CandidateNetwork is a bridge or VLAN the wizard may attach to.
type CandidateNetwork struct {
	Bridge    string         `json:"bridge"`
	VLAN      int            `json:"vlan,omitempty"`
	Vnet      bool           `json:"vnet"`               // an SDN vnet
	VLANAware bool           `json:"vlanAware"`          // the bridge filters VLANs
	Uplink    bool           `json:"uplink"`             // a physical or bond port is enslaved
	Direct    bool           `json:"direct"`             // the node has an address here: nothing to attach
	Attached  string         `json:"attached,omitempty"` // the network id when a leg exists
	Guests    []GuestAddr    `json:"guests"`             // addresses seen on it (static and reported), by guest
	Prefixes  []netip.Prefix `json:"prefixes"`           // the prefixes the guest addresses suggest, most common first
	Refused   string         `json:"refused,omitempty"`  // why this profile cannot attach it
}

// GuestAddr is an address a guest has on a network card.
type GuestAddr struct {
	Guest GuestView  `json:"guest"`
	Addr  netip.Addr `json:"addr"`
	NIC   int        `json:"nic"`
}

// ProbeRequest names what a probe asks for: an address on a bridge and VLAN,
// in a prefix. Without a prefix the one the guest addresses there suggest is
// taken, without an address the highest one in the prefix that no guest has.
// Scope, Node and Legs ask for the legs of a network on a cluster: of scope
// cluster, this node at Address and every other node with an agent, each at
// its address in Legs or one proposed; of scope node, the node Node names,
// or this one, at Address. Without them the probe is of this node alone.
type ProbeRequest struct {
	Bridge  string                `json:"bridge"`
	VLAN    int                   `json:"vlan,omitempty"`
	Prefix  netip.Prefix          `json:"prefix,omitzero"`
	Address netip.Addr            `json:"address,omitzero"`
	Scope   string                `json:"scope,omitempty"`
	Node    string                `json:"node,omitempty"`
	Legs    map[string]netip.Addr `json:"legs,omitempty"`
}

// ProbeResult is who answered for an address on a bridge and VLAN.
type ProbeResult struct {
	Bridge     string       `json:"bridge"`
	VLAN       int          `json:"vlan,omitempty"`
	Prefix     netip.Prefix `json:"prefix"`
	Address    netip.Addr   `json:"address"` // proposed or given
	Free       bool         `json:"free"`
	AnsweredBy []string     `json:"answeredBy"`
	Uplink     bool         `json:"uplink"`
	Warning    string       `json:"warning,omitempty"` // "the leg's MAC becomes visible on the LAN of vmbr0"
	// Note says how the prefix and the address were chosen when the request
	// did not give them.
	Note string `json:"note,omitempty"`
	// Overlap names the route of the node the prefix overlaps, which an
	// attach refuses without overlapOK.
	Overlap string `json:"overlap,omitempty"`
	// NotProbed says why the address was not probed: the appliance has no
	// card on a bridge before the network there is attached. Free is false
	// then, and an attach is not refused for it.
	NotProbed string `json:"notProbed,omitempty"`
	// Clustered says that this node is one of a cluster, where an attach
	// names the scope of the network, and Suggested is the scope the
	// configuration of the bridge suggests: cluster for a bridge with a
	// physical uplink or a vnet of a zone that spans the nodes, node
	// otherwise.
	Clustered bool   `json:"clustered,omitempty"`
	Suggested string `json:"suggested,omitempty"`
	// Scope is the scope the probe was of, and Legs the legs it probed,
	// one for each node, this node first; the fields above are of the
	// first. Without a scope there are none.
	Scope string     `json:"scope,omitempty"`
	Legs  []LegProbe `json:"legs,omitempty"`
}

// LegProbe is what a probe found of the leg of one node.
type LegProbe struct {
	Node       string     `json:"node"`
	Address    netip.Addr `json:"address,omitzero"`
	Free       bool       `json:"free"`
	AnsweredBy []string   `json:"answeredBy"`
	Uplink     bool       `json:"uplink"`
	Overlap    string     `json:"overlap,omitempty"`
	// Problem says why the node gets no leg: it has no such bridge, no
	// agent answered for it, or the leg would be refused there. Missing
	// says that it is one of the first two, which an attach of scope
	// cluster leaves out; the others refuse it.
	Problem string `json:"problem,omitempty"`
	Missing bool   `json:"missing,omitempty"`
}

// AttachRequest is a network to attach: a bridge and VLAN, the prefix of the
// guests there and the address of the leg on this node. On a cluster it
// names the scope, and the legs of the other nodes: Address is the leg of
// the node Node names, or of this node, and Legs those of the others. A
// request for the legs of nodes that have none of a network attached adds
// them to it.
type AttachRequest struct {
	Bridge        string                `json:"bridge"`
	VLAN          int                   `json:"vlan,omitempty"`
	Prefix        netip.Prefix          `json:"prefix"`
	Address       netip.Addr            `json:"address,omitzero"`
	ConfirmUplink bool                  `json:"confirmUplink"`
	OverlapOK     bool                  `json:"overlapOK"` // the prefix may overlap a route of the node
	Scope         string                `json:"scope,omitempty"`
	Node          string                `json:"node,omitempty"`
	Legs          map[string]netip.Addr `json:"legs,omitempty"`
}

// OverlapHint is what the refusal of a prefix that overlaps a route of the
// node says to do when the overlap is meant, and UplinkHint what the refusal
// of a bridge with a physical uplink says.
const (
	OverlapHint = "pass --overlap-ok if the guests there are meant to be reached through the leg"
	UplinkHint  = "confirm it with --confirm-uplink, with an address reserved for pco on that LAN"
)

const (
	// uplinkWarning is what an attach to a bridge with a physical uplink
	// shows: the leg answers on that LAN.
	uplinkWarning = "the leg's MAC becomes visible on the LAN of %s"
	// assumedBits is the length of the prefix a guest address suggests.
	assumedBits = 24
	// guessedOne, guessedMany and proposed say how a probe chose the prefix
	// and the address the request did not give.
	guessedOne  = "a /24 is assumed: only %s is known on %s"
	guessedMany = "%s holds %d of the %d guest addresses known on %s"
	proposed    = "%s is the highest address of %s that no guest is known to have"
)

// The prefixes of the ports a bridge has for its guests and for pco's own
// legs: any other port of a bridge links it to a LAN.
var guestPorts = []string{"tap", "veth", "fwpr", "fwln"}

// firewallBridge is the prefix of the bridges Proxmox makes for the firewall
// of a guest's card.
const firewallBridge = "fwbr"

// vlanBridge is a bridge Proxmox makes for a tag on a bridge that does not
// filter VLANs.
var vlanBridge = regexp.MustCompile(`^vmbr\d+v\d+$`)

// applianceSlots is the last index of a network card the appliance gives a
// leg: net1 to net9.
const applianceSlots = 9

// Networks returns the networks pco reaches through a leg, by id, with what
// the gateway last said of their legs and the routes the last cycle served
// through them.
func (e *Engine) Networks() ([]NetworkView, error) {
	nets, mappings, err := e.storedNetworks()
	if err != nil {
		return nil, err
	}
	return networkViews(nets, mappings, e.State().Routes, e.legsSeen()), nil
}

// Network returns one network with its service addresses and the routes
// served through it.
func (e *Engine) Network(id string) (NetworkDetail, error) {
	nets, mappings, err := e.storedNetworks()
	if err != nil {
		return NetworkDetail{}, err
	}
	n, ok := nets[id]
	if !ok {
		return NetworkDetail{}, fmt.Errorf("%w: network %s is not attached", ErrNotFound, id)
	}
	routes := e.State().Routes
	views := networkViews(map[string]store.Network{id: n}, mappings, routes, e.legsSeen())
	d := NetworkDetail{NetworkView: views[0], Services: []store.ServiceAddr{}, NodeServices: []store.NodeServiceAddr{}, Served: servedThrough(routes, id)}
	for _, m := range mappings.cluster {
		if m.Network == id {
			d.Services = append(d.Services, m)
		}
	}
	for _, m := range mappings.nodes {
		if m.Network == id {
			d.NodeServices = append(d.NodeServices, m)
		}
	}
	slices.SortFunc(d.Services, func(a, b store.ServiceAddr) int { return a.Service.Compare(b.Service) })
	return d, nil
}

// storedMappings are the service addresses of both kinds: of the networks
// that are one on every node, and of the networks of single nodes.
type storedMappings struct {
	cluster []store.ServiceAddr
	nodes   []store.NodeServiceAddr
}

func (e *Engine) storedNetworks() (map[string]store.Network, storedMappings, error) {
	nets, err := e.d.Store.Networks()
	if err != nil {
		return nil, storedMappings{}, fmt.Errorf("reading the networks: %w", err)
	}
	var m storedMappings
	if m.cluster, err = e.d.Store.ServiceAddrs(); err != nil {
		return nil, storedMappings{}, fmt.Errorf("reading the service addresses: %w", err)
	}
	if m.nodes, err = e.d.Store.NodeServiceAddrs(); err != nil {
		return nil, storedMappings{}, fmt.Errorf("reading the service addresses of single nodes: %w", err)
	}
	return nets, m, nil
}

// legsSeen is what the views of the networks go by beyond the store: what
// the gateway of this node said of its legs, what the agents of the other
// nodes said of theirs, and the indexes of the nodes, which make the MACs.
type legsSeen struct {
	node    string
	local   []gateway.LegState
	remote  map[string]map[string]agentproto.LegReport
	indexes map[string]int
}

// legsSeen reads what the views of the networks go by outside a cycle.
func (e *Engine) legsSeen() legsSeen {
	seen := legsSeen{node: e.d.Node, local: e.LegStates()}
	if e.d.Remote != nil {
		seen.remote = reportedLegs(e.d.Node, e.d.Remote.Reports(context.Background(), 0, nil))
	}
	if ln, err := e.legNodes(); err == nil {
		seen.indexes = ln.indexes
	}
	return seen
}

// reportedLegs are the legs each other node's agent said its gateway found,
// by node and network: from fresh reports alone, each of the node that sent
// it.
func reportedLegs(self string, reports map[string]NodeReport) map[string]map[string]agentproto.LegReport {
	out := map[string]map[string]agentproto.LegReport{}
	for node, rep := range reports {
		if node == self || !rep.Fresh || rep.Status.Node != node {
			continue
		}
		legs := map[string]agentproto.LegReport{}
		for _, l := range rep.Status.Legs {
			legs[l.ID] = agentproto.LegReport{ID: l.ID, Up: l.Up, Problem: clip(l.Problem)}
		}
		out[node] = legs
	}
	return out
}

// networkViews are the views of the networks, by id: the leg of each node
// with what its gateway said of it, the service addresses of each and the
// routes served through it.
func networkViews(nets map[string]store.Network, mappings storedMappings, routes []RouteView, seen legsSeen) []NetworkView {
	out := make([]NetworkView, 0, len(nets))
	for _, id := range slices.Sorted(maps.Keys(nets)) {
		n := nets[id]
		v := NetworkView{Network: n, Legs: []LegView{}, Routes: len(servedThrough(routes, id))}
		for _, node := range legOrder(n, seen.node) {
			v.Legs = append(v.Legs, seen.legView(n, node))
		}
		for _, m := range mappings.cluster {
			if m.Network == id {
				v.Targets++
			}
		}
		for _, m := range mappings.nodes {
			if m.Network == id {
				v.Targets++
			}
		}
		out = append(out, v)
	}
	return out
}

// legOrder are the nodes with a leg of n, self first.
func legOrder(n store.Network, self string) []string {
	return slices.SortedFunc(maps.Keys(n.Legs), func(a, b string) int {
		return cmp.Or(-cmp.Compare(boolInt(a == self), boolInt(b == self)), strings.Compare(a, b))
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// legView is the leg of n on node as its gateway last said.
func (seen legsSeen) legView(n store.Network, node string) LegView {
	leg := LegView{Node: node, Addr: n.Legs[node], Problem: "the gateway has not reported the leg yet"}
	if mac, err := n.LegMACOn(node, seen.indexes); err == nil {
		leg.MAC = mac
	} else if node == seen.node {
		leg.MAC = n.LegMAC
	}
	if node != seen.node {
		l, ok := seen.remote[node][n.ID]
		if !ok {
			leg.Problem = fmt.Sprintf("the agent of node %s has not reported the leg yet", node)
			return leg
		}
		leg.Up, leg.Problem = l.Up, l.Problem
	} else if i := slices.IndexFunc(seen.local, func(s gateway.LegState) bool { return s.ID == n.ID }); i >= 0 {
		leg.Up, leg.Problem = seen.local[i].Up, seen.local[i].Problem
	} else {
		return leg
	}
	if !leg.Up && leg.Problem == "" {
		leg.Problem = "the gateway does not say why"
	}
	return leg
}

// servedThrough are the hostnames of the routes whose path goes through the
// leg of network id, sorted.
func servedThrough(routes []RouteView, id string) []string {
	via := viaOf(id)
	out := []string{}
	for _, r := range routes {
		if r.Path != nil && r.Path.Via == via {
			out = append(out, r.Hostname)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// cloneNetworks copies views down to their maps and lists.
func cloneNetworks(views []NetworkView) []NetworkView {
	out := slices.Clone(views)
	for i := range out {
		out[i].Network.Legs = maps.Clone(out[i].Network.Legs)
		out[i].LegMACs = maps.Clone(out[i].LegMACs)
		out[i].Legs = slices.Clone(out[i].Legs)
	}
	return out
}

// showNetworks puts the networks in the state of the cycle. Networks that
// cannot be read leave the state as the last cycle showed it: the gateway
// step says why.
func (c *cycleRun) showNetworks() {
	nets, mappings, err := c.e.storedNetworks()
	if err != nil {
		return
	}
	seen := legsSeen{node: c.e.d.Node, local: c.e.LegStates(), remote: reportedLegs(c.e.d.Node, c.reportsNow()), indexes: c.nodeIndexes()}
	c.st.Networks = networkViews(nets, mappings, c.st.Routes, seen)
}

// legEvents are the events of what the gateway says of the legs now against
// what it said before: a leg that came up, one that is not up for a reason it
// was not before, and one that is up with a problem it did not have.
func legEvents(before, after []gateway.LegState, at time.Time) []Event {
	prev := make(map[string]gateway.LegState, len(before))
	for _, s := range before {
		prev[s.ID] = s
	}
	var out []Event
	add := func(level string, s gateway.LegState, format string, args ...any) {
		out = append(out, Event{At: at, Level: level, Kind: kindNetwork, Subject: s.ID, Message: fmt.Sprintf(format, args...)})
	}
	for _, s := range after {
		p, seen := prev[s.ID]
		changed := !seen || p.Up != s.Up || p.Problem != s.Problem
		switch {
		case !changed:
		case !s.Up:
			add(levelWarn, s, "the leg %s of network %s is not up: %s", s.Iface, s.ID, cmp.Or(s.Problem, "the gateway does not say why"))
		case s.Problem != "":
			add(levelWarn, s, "the leg %s of network %s is up, but %s", s.Iface, s.ID, s.Problem)
		case !seen || !p.Up:
			add(levelInfo, s, "the leg %s of network %s is up", s.Iface, s.ID)
		}
	}
	return out
}

// networkFacts is what the actions of the networks go by: the interfaces
// Proxmox has configured on the node, the SDN vnets, the node's interfaces as
// they are, the guests, the networks stored and the settings.
type networkFacts struct {
	node      string
	remote    bool // of another node, whose interfaces its agent answered
	clustered bool // this node is one of a cluster
	appliance bool
	self      model.GuestRef // the appliance's own container
	config    []pve.NodeIface
	vnets     []pve.VNet
	host      []resolve.HostIface
	snap      inventory.Snapshot
	nets      map[string]store.Network
	settings  store.Settings
	zones     []pve.SDNZone // read on a cluster only
}

// networkFacts reads the facts. The guests are listed afresh; a listing that
// is not complete only leaves out what it did not see.
func (e *Engine) networkFacts(ctx context.Context) (networkFacts, error) {
	f := networkFacts{node: e.d.Node}
	inst, found, err := e.d.Store.Install()
	switch {
	case err != nil:
		return f, fmt.Errorf("reading the install: %w", err)
	case !found:
		return f, fmt.Errorf("%w: pco is not set up on this node; run pco setup", ErrRefused)
	}
	if f.appliance = inst.ProfileName() == store.ProfileAppliance; f.appliance && inst.Appliance != nil {
		f.self = model.GuestRef{Kind: model.KindLXC, VMID: inst.Appliance.VMID}
	}
	if f.settings, err = e.d.Store.Settings(); err != nil {
		return f, fmt.Errorf("reading the settings: %w", err)
	}
	ln, err := e.legNodes()
	if err != nil {
		return f, err
	}
	f.clustered = ln.clustered
	if f.nets, err = e.d.Store.Networks(); err != nil {
		return f, fmt.Errorf("reading the networks: %w", err)
	}
	if f.config, err = e.d.Networks.NodeNetwork(ctx, e.d.Node); err != nil {
		return f, fmt.Errorf("reading the network of node %s: %w", e.d.Node, err)
	}
	if f.vnets, err = e.d.Networks.VNets(ctx); err != nil {
		return f, fmt.Errorf("reading the SDN vnets: %w", err)
	}
	if f.host, err = e.d.Networks.Interfaces(ctx); err != nil {
		return f, fmt.Errorf("reading the interfaces of the node: %w", err)
	}
	look, cancel := e.timeout(ctx, lookTimeout)
	defer cancel()
	f.snap = e.d.Inventory.Refresh(look)
	return f, nil
}

// configured is the interface Proxmox has configured under name.
func (f networkFacts) configured(name string) (pve.NodeIface, bool) {
	i := slices.IndexFunc(f.config, func(c pve.NodeIface) bool { return c.Name == name })
	if i < 0 {
		return pve.NodeIface{}, false
	}
	return f.config[i], true
}

func (f networkFacts) isVnet(name string) bool {
	return slices.ContainsFunc(f.vnets, func(v pve.VNet) bool { return v.Name == name })
}

// isBridge says that name is a bridge guests may be on: one Proxmox has
// configured on the node, an SDN vnet, or one the node has now. Of another
// node it is one Proxmox has configured, or a vnet, that its agent finds
// there: an agent cannot make a bridge.
func (f networkFacts) isBridge(name string) bool {
	c, ok := f.configured(name)
	known := ok && c.Type == "bridge" || f.isVnet(name)
	if f.remote {
		return known && f.liveBridge(name)
	}
	return known || f.liveBridge(name)
}

// liveBridge says that name is a bridge the node has now, as the prober sees
// it, also one made at run time that Proxmox has not configured. The bridges
// Proxmox makes for the firewall of a guest's card are not: no guest is on
// them. Nor is one it makes for a VLAN of a bridge that does not filter
// VLANs (vmbr0v20), which the prober puts on that VLAN of the bridge.
func (f networkFacts) liveBridge(name string) bool {
	return !strings.HasPrefix(name, firewallBridge) && slices.ContainsFunc(f.host, func(h resolve.HostIface) bool {
		return h.Via == "" && h.Name == name && h.Segment == resolve.Segment{Bridge: name}
	})
}

// candidates are the bridges and vnets of the node, and the VLANs of them
// that guests, the node or a network are on, by bridge and VLAN.
func (f networkFacts) candidates() []CandidateNetwork {
	segs := map[resolve.Segment]bool{}
	for _, c := range f.config {
		if c.Type == "bridge" {
			segs[resolve.Segment{Bridge: c.Name}] = true
		}
	}
	for _, v := range f.vnets {
		segs[resolve.Segment{Bridge: v.Name}] = true
	}
	for _, h := range f.host {
		if f.liveBridge(h.Name) {
			segs[resolve.Segment{Bridge: h.Name}] = true
		}
	}
	for _, g := range f.snap.Guests {
		for _, n := range g.NICs {
			if f.isBridge(n.Bridge) {
				segs[resolve.SegmentOf(n)] = true
			}
		}
	}
	for _, h := range f.host {
		if h.Via == "" && h.Segment.VLAN != 0 && f.isBridge(h.Segment.Bridge) {
			segs[h.Segment] = true
		}
	}
	for _, n := range f.nets {
		segs[resolve.Segment{Bridge: n.Bridge, VLAN: n.VLAN}] = true
	}
	order := slices.SortedFunc(maps.Keys(segs), func(a, b resolve.Segment) int {
		return cmp.Or(strings.Compare(a.Bridge, b.Bridge), cmp.Compare(a.VLAN, b.VLAN))
	})
	out := make([]CandidateNetwork, 0, len(order))
	for _, seg := range order {
		out = append(out, f.candidate(seg))
	}
	return out
}

// candidate is what the facts say of one bridge and VLAN.
func (f networkFacts) candidate(seg resolve.Segment) CandidateNetwork {
	c := CandidateNetwork{Bridge: seg.Bridge, VLAN: seg.VLAN, Vnet: f.isVnet(seg.Bridge), Guests: []GuestAddr{}, Prefixes: []netip.Prefix{}}
	if cfg, ok := f.configured(seg.Bridge); ok {
		c.VLANAware = cfg.VLANAware
	}
	c.Uplink = f.uplink(seg.Bridge)
	c.Direct = slices.ContainsFunc(f.host, func(h resolve.HostIface) bool {
		return h.Via == "" && h.Segment == seg && slices.ContainsFunc(h.Addrs, func(p netip.Prefix) bool { return p.Addr().Is4() })
	})
	if n, ok := f.nets[store.SegmentID(seg.Bridge, seg.VLAN)]; ok {
		c.Attached = n.ID
	}
	c.Guests = f.guestsOn(seg, c.Uplink)
	c.Prefixes = suggested(c.Guests)
	c.Refused = f.refusal(c)
	return c
}

// uplink says that a bridge has a port that links it to a LAN: a physical
// port, a bond or a VLAN of one in the ports Proxmox has configured for it,
// or, for a vnet, whose ports SDN makes, and a bridge made at run time, any
// port it has now that is not a guest's or a leg's.
func (f networkFacts) uplink(bridge string) bool {
	cfg, ok := f.configured(bridge)
	if !ok || cfg.Type != "bridge" {
		return slices.ContainsFunc(f.host, func(h resolve.HostIface) bool {
			return h.Master == bridge && !slices.ContainsFunc(guestPorts, func(p string) bool { return strings.HasPrefix(h.Name, p) })
		})
	}
	for _, port := range cfg.Ports {
		if port == "none" {
			continue
		}
		p, listed := f.configured(port)
		if !listed {
			return true // a port Proxmox does not list is taken for a physical one
		}
		switch p.Type {
		case "eth", "bond", "vlan":
			return true
		}
	}
	return false
}

// guestsOn are the addresses of the guests on a bridge and VLAN, static and
// reported, by guest and card. A bridge without an uplink is the node's own,
// and holds the guests of this node; one with an uplink holds those of every
// node on its LAN.
func (f networkFacts) guestsOn(seg resolve.Segment, uplink bool) []GuestAddr {
	out := []GuestAddr{}
	for _, g := range f.snap.Guests {
		if g.Node != f.node && !uplink {
			continue
		}
		for _, n := range g.NICs {
			if resolve.SegmentOf(n) != seg {
				continue
			}
			addrs := slices.Clone(n.Static)
			for _, r := range g.Reported {
				if strings.EqualFold(r.MAC, n.MAC) {
					addrs = append(addrs, r.Addr)
				}
			}
			slices.SortFunc(addrs, netip.Addr.Compare)
			for _, a := range slices.Compact(addrs) {
				if a.Is4() {
					out = append(out, GuestAddr{Guest: GuestView{GuestRef: g.Ref, Name: strings.TrimSpace(g.Name)}, Addr: a, NIC: n.Index})
				}
			}
		}
	}
	return out
}

// suggested are the prefixes the guest addresses suggest: the /24 of each,
// the one with the most addresses first.
func suggested(guests []GuestAddr) []netip.Prefix {
	count := map[netip.Prefix]int{}
	seen := map[netip.Addr]bool{}
	for _, g := range guests {
		if seen[g.Addr] {
			continue
		}
		seen[g.Addr] = true
		p, _ := g.Addr.Prefix(assumedBits)
		count[p]++
	}
	return slices.SortedFunc(maps.Keys(count), func(a, b netip.Prefix) int {
		return cmp.Or(cmp.Compare(count[b], count[a]), a.Addr().Compare(b.Addr()))
	})
}

// refusal says why the profile does not attach a network to a bridge and
// VLAN: never pco's managed network, and in the appliance, whose legs are
// network cards of its own, no tag on a bridge that does not filter VLANs, no
// bridge Proxmox made for a VLAN, and no network beyond its last free card.
func (f networkFacts) refusal(c CandidateNetwork) string {
	switch {
	case c.Bridge == gateway.ManagedBridge:
		return fmt.Sprintf("%s is pco's managed network, which is not attached", c.Bridge)
	case !f.appliance:
		return ""
	case vlanBridge.MatchString(c.Bridge):
		return fmt.Sprintf("bridge %s is one Proxmox makes for a VLAN; attach the network on its bridge with the tag", c.Bridge)
	case c.VLAN != 0 && !c.VLANAware:
		return fmt.Sprintf("bridge %s is not VLAN-aware; the appliance attaches to VLAN %d of a VLAN-aware bridge only", c.Bridge, c.VLAN)
	case c.Attached == "" && !f.freeSlot():
		return fmt.Sprintf("no free NIC slot (net1..net%d): the appliance has a network card at each", applianceSlots)
	}
	return ""
}

// notProbed is why the appliance did not probe an address, with the segment
// and the address.
const notProbed = "the appliance has no network card on %s before the network is attached; " +
	"the leg checks %s when it is raised and keeps it off if anyone answers"

// canProbe says whether an address on the candidate can be probed before an
// attach: the host probes through a port of its own, the appliance only
// through the card of a network attached there already.
func (f networkFacts) canProbe(c CandidateNetwork) bool {
	return !f.appliance || c.Attached != ""
}

// freeSlot says whether the appliance has a card index of net1 to net9 free,
// which a new leg takes; one that is not listed is taken to have one.
func (f networkFacts) freeSlot() bool {
	g, ok := f.snap.Guest(f.self)
	if !ok {
		return true
	}
	for i := 1; i <= applianceSlots; i++ {
		if _, used := g.NIC(i); !used {
			return true
		}
	}
	return false
}

// nodeRoute is a route of the node to a prefix it has an address in, on an
// interface and the segment of it.
type nodeRoute struct {
	prefix netip.Prefix
	iface  string
	seg    resolve.Segment
}

// nodeRoutes are the routes of the node's own addresses, as the interfaces
// are and as Proxmox has them configured, by interface.
func (f networkFacts) nodeRoutes() []nodeRoute {
	var out []nodeRoute
	for _, h := range f.host {
		if h.Via != "" {
			continue
		}
		for _, p := range h.Addrs {
			if p.Addr().Is4() {
				out = append(out, nodeRoute{prefix: p.Masked(), iface: h.Name, seg: h.Segment})
			}
		}
	}
	for _, c := range f.config {
		for _, p := range c.Addrs {
			seg := resolve.Segment{}
			if c.Type == "bridge" {
				seg.Bridge = c.Name
			}
			out = append(out, nodeRoute{prefix: p.Masked(), iface: c.Name, seg: seg})
		}
	}
	slices.SortFunc(out, func(a, b nodeRoute) int {
		return cmp.Or(strings.Compare(a.iface, b.iface), a.prefix.Addr().Compare(b.prefix.Addr()), cmp.Compare(a.prefix.Bits(), b.prefix.Bits()))
	})
	return slices.CompactFunc(out, func(a, b nodeRoute) bool { return a.iface == b.iface && a.prefix == b.prefix })
}

// overlap names the first route of the node on another segment than seg that
// prefix overlaps; empty when there is none.
func (f networkFacts) overlap(prefix netip.Prefix, seg resolve.Segment) string {
	for _, r := range f.nodeRoutes() {
		switch {
		case r.seg == seg || !r.prefix.Overlaps(prefix):
		case f.remote:
			return fmt.Sprintf("%s overlaps the route of node %s on %s", prefix, f.node, r.iface)
		default:
			return fmt.Sprintf("%s overlaps the node's route on %s", prefix, r.iface)
		}
	}
	return ""
}

// lookup finds the bridge and VLAN of a probe or an attach among the
// candidates: any VLAN of a bridge the node has may be asked for.
func (f networkFacts) lookup(bridge string, vlan int) (CandidateNetwork, error) {
	if !f.isBridge(bridge) {
		return CandidateNetwork{}, fieldError("bridge", "%s is no bridge or vnet of node %s", bridge, f.node)
	}
	return f.candidate(resolve.Segment{Bridge: bridge, VLAN: vlan}), nil
}

// suggestion is the scope a probe suggests for seg on a cluster; none on a
// node alone.
func (f networkFacts) suggestion(seg resolve.Segment) string {
	if !f.clustered {
		return ""
	}
	return f.suggestedScope(seg)
}

// probeArgs checks a bridge and a VLAN as a request names them.
func probeArgs(bridge string, vlan int) error {
	switch {
	case bridge == "":
		return fieldError("bridge", "bridge: name a bridge or a vnet")
	case strings.IndexFunc(bridge, func(r rune) bool { return !isBridgeChar(r) }) >= 0:
		return fieldError("bridge", "bridge %q: want letters, digits, - and _", bridge)
	case vlan < 0 || vlan > maxVLAN:
		return fieldError("vlan", "vlan %d: want 0 for untagged, or 1 to %d", vlan, maxVLAN)
	}
	return nil
}

// usable refuses a candidate pco does not attach: one the profile refuses,
// and one the node reaches without a leg.
func (f networkFacts) usable(c CandidateNetwork) error {
	seg := resolve.Segment{Bridge: c.Bridge, VLAN: c.VLAN}
	switch {
	case c.Refused != "":
		return fmt.Errorf("%w: %s", ErrRefused, c.Refused)
	case c.Direct && f.remote:
		return fmt.Errorf("%w: node %s reaches %s directly; nothing to attach", ErrRefused, f.node, seg)
	case c.Direct:
		return fmt.Errorf("%w: the node reaches %s directly; nothing to attach", ErrRefused, seg)
	}
	return nil
}

// checkPrefix refuses a prefix of a network that is no IPv4 network address
// with room for a leg beside its guests.
func checkPrefix(p netip.Prefix) error {
	switch {
	case !p.IsValid() || !p.Addr().Is4():
		return fieldError("prefix", "prefix %s: want an IPv4 prefix such as 10.77.0.0/24", p)
	case p != p.Masked():
		return fieldError("prefix", "prefix %s: want the address of the network, %s", p, p.Masked())
	case p.Bits() > 30:
		return fieldError("prefix", "prefix %s: a network longer than /30 leaves no address for the leg beside its guests", p)
	}
	return nil
}

// checkAddress refuses an address of the leg that is not one of the guests'
// addresses of the prefix.
func checkAddress(p netip.Prefix, a netip.Addr) error {
	switch {
	case !a.Is4():
		return fieldError("address", "address %s: want an IPv4 address in %s", a, p)
	case !p.Contains(a):
		return fieldError("address", "address %s is not in %s", a, p)
	case a == p.Addr() || a == lastAddr(p):
		return fieldError("address", "address %s is the network or the broadcast address of %s", a, p)
	}
	return nil
}

// lastAddr is the broadcast address of an IPv4 prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v |= host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// taken are the addresses on a bridge and VLAN that a leg must not have: the
// guests' and the node's own.
func (f networkFacts) taken(c CandidateNetwork) map[netip.Addr]string {
	out := map[netip.Addr]string{}
	for _, g := range c.Guests {
		out[g.Addr] = "an address of " + OwnerName(g.Guest.String(), &g.Guest)
	}
	for _, h := range f.host {
		for _, p := range h.Addrs {
			if f.remote {
				out[p.Addr()] = fmt.Sprintf("an address of node %s on %s", f.node, h.Name)
				continue
			}
			out[p.Addr()] = "an address of the node on " + h.Name
		}
	}
	return out
}

// propose is the highest address of the prefix below its broadcast address
// that nobody is known to have.
func propose(p netip.Prefix, taken map[netip.Addr]string) (netip.Addr, bool) {
	for a := lastAddr(p).Prev(); p.Contains(a) && a != p.Addr(); a = a.Prev() {
		if _, ok := taken[a]; !ok {
			return a, true
		}
	}
	return netip.Addr{}, false
}

// ProbeNetwork asks who answers for an address on a bridge and VLAN, through
// the leg there or a port of the gateway's own, which is visible on the LAN
// of a bridge with an uplink while it probes. The prefix and the address
// default to what the guests there suggest.
func (e *Engine) ProbeNetwork(ctx context.Context, r ProbeRequest) (ProbeResult, error) {
	if err := probeArgs(r.Bridge, r.VLAN); err != nil {
		return ProbeResult{}, err
	}
	if err := e.networksHere(); err != nil {
		return ProbeResult{}, err
	}
	if r.Scope != "" || r.Node != "" || len(r.Legs) > 0 {
		return e.probeLegs(ctx, r)
	}
	if err := e.acquireAdmin(ctx); err != nil {
		return ProbeResult{}, err
	}
	defer e.release()

	f, err := e.networkFacts(ctx)
	if err != nil {
		return ProbeResult{}, err
	}
	c, err := f.lookup(r.Bridge, r.VLAN)
	if err != nil {
		return ProbeResult{}, err
	}
	if err := f.usable(c); err != nil {
		return ProbeResult{}, err
	}
	seg := resolve.Segment{Bridge: c.Bridge, VLAN: c.VLAN}
	if f.clustered {
		if err := e.readZones(ctx, &f); err != nil {
			return ProbeResult{}, err
		}
	}
	res := ProbeResult{Clustered: f.clustered, Suggested: f.suggestion(seg), Bridge: c.Bridge, VLAN: c.VLAN, Prefix: r.Prefix, Address: r.Address, AnsweredBy: []string{}, Uplink: c.Uplink}
	var notes []string
	if !res.Prefix.IsValid() {
		if len(c.Prefixes) == 0 {
			return ProbeResult{}, fieldError("prefix", "no guest address is known on %s; give the prefix of the network, as 10.77.0.0/24", seg)
		}
		res.Prefix = c.Prefixes[0]
		notes = append(notes, prefixNote(c, seg))
	}
	if err := checkPrefix(res.Prefix); err != nil {
		return ProbeResult{}, err
	}
	if !res.Address.IsValid() {
		a, ok := propose(res.Prefix, f.taken(c))
		if !ok {
			return ProbeResult{}, fieldError("address", "every address of %s is taken by a guest or the node; give one", res.Prefix)
		}
		res.Address = a
		notes = append(notes, fmt.Sprintf(proposed, a, res.Prefix))
	}
	if err := checkAddress(res.Prefix, res.Address); err != nil {
		return ProbeResult{}, err
	}
	res.Note = strings.Join(notes, "; ")
	if c.Uplink {
		res.Warning = fmt.Sprintf(uplinkWarning, c.Bridge)
	}
	res.Overlap = f.overlap(res.Prefix, seg)
	if !f.canProbe(c) {
		res.NotProbed = fmt.Sprintf(notProbed, seg, res.Address)
		return res, nil
	}
	answers, err := e.probe(ctx, seg, res.Address)
	if err != nil {
		return ProbeResult{}, err
	}
	res.AnsweredBy, res.Free = answers, len(answers) == 0
	return res, nil
}

// prefixNote says where the prefix a probe took came from.
func prefixNote(c CandidateNetwork, seg resolve.Segment) string {
	addrs := map[netip.Addr]bool{}
	for _, g := range c.Guests {
		addrs[g.Addr] = true
	}
	if len(addrs) == 1 {
		return fmt.Sprintf(guessedOne, c.Guests[0].Addr, seg)
	}
	in := 0
	for a := range addrs {
		if c.Prefixes[0].Contains(a) {
			in++
		}
	}
	return fmt.Sprintf(guessedMany, c.Prefixes[0], in, len(addrs), seg)
}

// networksHere refuses an action of the networks on a node where no gateway
// runs or the facts cannot be read.
func (e *Engine) networksHere() error {
	if e.d.Gateway == nil || e.d.Networks == nil {
		return fmt.Errorf("%w: %s; no network can be attached here", ErrRefused, gateNoGateway)
	}
	return nil
}

// probe asks who holds addr on a segment, as MACs. The gateway namespace is
// started for it when it is not there yet, as before the first network.
func (e *Engine) probe(ctx context.Context, seg resolve.Segment, addr netip.Addr) ([]string, error) {
	macs, err := e.d.Gateway.Probe(ctx, seg.Bridge, seg.VLAN, addr)
	if errors.Is(err, gateway.ErrNotPinned) {
		if serr := e.d.Networks.StartNamespace(ctx); serr != nil {
			return nil, fmt.Errorf("starting the gateway namespace to probe %s on %s: %w", addr, seg, serr)
		}
		macs, err = e.d.Gateway.Probe(ctx, seg.Bridge, seg.VLAN, addr)
	}
	if err != nil {
		return nil, fmt.Errorf("probing %s on %s: %w", addr, seg, err)
	}
	out := make([]string, 0, len(macs))
	for _, m := range macs {
		out = append(out, m.String())
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// CandidateNetworks lists the bridges and VLANs of the node that a network
// may be attached to, with the guest addresses seen there, and those it may
// not be, with why.
func (e *Engine) CandidateNetworks(ctx context.Context) ([]CandidateNetwork, error) {
	if e.d.Networks == nil {
		return nil, fmt.Errorf("%w: %s; no network can be attached here", ErrRefused, gateNoGateway)
	}
	if err := e.acquireAdmin(ctx); err != nil {
		return nil, err
	}
	defer e.release()
	f, err := e.networkFacts(ctx)
	if err != nil {
		return nil, err
	}
	return f.candidates(), nil
}

// Attach attaches a network: a leg of the gateway into a bridge and VLAN the
// node has no address in, with an address of the guests' prefix that nobody
// answers for. It refuses a bridge the node reaches directly, one with a
// physical uplink unless that is confirmed, a prefix over the service prefix
// or the managed subnet, one over a route of the node on another interface
// unless overlapOK, one over another network's on the same bridge, and an
// address that is taken. The first network has pco-netns.service enabled. The
// leg is raised by the cycle it asks for.
//
// On a cluster the request names the scope and the nodes that get a leg,
// each checked in the same way against the facts of its node: those of
// another node as Proxmox has them configured and as its agent's probe
// answered, which is asked before the cycle lock is taken. A network of
// scope cluster is one segment on every node by the configuration alone. A
// request for legs of nodes that have none of a network attached adds them.
func (e *Engine) Attach(ctx context.Context, r AttachRequest) (NetworkView, error) {
	if err := probeArgs(r.Bridge, r.VLAN); err != nil {
		return NetworkView{}, err
	}
	if err := checkPrefix(r.Prefix); err != nil {
		return NetworkView{}, err
	}
	if len(r.Legs) == 0 {
		if err := checkAddress(r.Prefix, r.Address); err != nil {
			return NetworkView{}, err
		}
	}
	if err := e.networksHere(); err != nil {
		return NetworkView{}, err
	}
	ln, err := e.legNodes()
	if err != nil {
		return NetworkView{}, err
	}
	legs, err := e.requestedLegs(ln, r.Scope, r.Node, r.Address, r.Legs, r.Prefix)
	if err != nil {
		return NetworkView{}, err
	}
	seg := resolve.Segment{Bridge: r.Bridge, VLAN: r.VLAN}
	others := maps.Clone(legs)
	delete(others, e.d.Node)
	if why := e.unasked(slices.Collect(maps.Keys(others))); len(why) > 0 {
		node := slices.Sorted(maps.Keys(why))[0]
		return NetworkView{}, fmt.Errorf("%w: %s; no leg can be checked there", ErrRefused, why[node])
	}
	answers := e.probeNodes(ctx, seg, others)
	if err := e.acquireWriter(ctx); err != nil {
		return NetworkView{}, err
	}
	defer e.release()

	f, err := e.networkFacts(ctx)
	if err != nil {
		return NetworkView{}, err
	}
	if ln.clustered && r.Scope == store.ScopeCluster {
		if err := e.readZones(ctx, &f); err != nil {
			return NetworkView{}, err
		}
	}
	a, err := e.attachable(ctx, f, ln, r, legs, answers)
	if err != nil {
		return NetworkView{}, err
	}
	n := a.network
	first := len(f.nets) == 0 && a.ext == "" && slices.Contains(slices.Collect(maps.Keys(n.Legs)), e.d.Node)
	if first {
		if err := e.d.Networks.EnableNamespace(ctx); err != nil {
			return NetworkView{}, fmt.Errorf("enabling pco-netns.service for the first network: %w", err)
		}
	}
	var indexes map[string]int
	if ln.clustered {
		indexes = ln.indexes
	}
	if err := e.d.Store.SaveNetworkIndexed(n, n.Rev, indexes); err != nil {
		if first {
			e.disableNamespace(ctx)
		}
		if errors.Is(err, store.ErrRevision) {
			return NetworkView{}, fmt.Errorf("%w: network %s was attached or changed meanwhile", ErrRefused, n.ID)
		}
		return NetworkView{}, fmt.Errorf("saving network %s: %w", n.ID, err)
	}
	switch {
	case a.ext != "":
		e.networkEvent(ctx, n.ID, addedMessage(n, legs))
	case ln.clustered:
		e.networkEvent(ctx, n.ID, legsMessage(n, a.overlap))
	default:
		e.networkEvent(ctx, n.ID, attachedMessage(n, r.Address, a.overlap))
	}
	e.Trigger()
	nets, mappings, err := e.storedNetworks()
	if err != nil {
		return NetworkView{}, err
	}
	saved, ok := nets[n.ID]
	if !ok {
		return NetworkView{}, fmt.Errorf("network %s is not stored after it was saved", n.ID)
	}
	return networkViews(map[string]store.Network{n.ID: saved}, mappings, nil, e.legsSeen())[0], nil
}

// attachment is what an attach saves: the network, at the revision it was
// read at when legs are added to it, with ext its id then, and the route of
// a node the prefix overlaps.
type attachment struct {
	network store.Network
	ext     string
	overlap string
}

// attachable checks a request against the facts of the node of each leg and
// returns the network it makes, or the network attached with the legs
// added: the refusals of Attach in their order, the last of which probes
// the address once more, this node's through its gateway and another's by
// what its agent answered. On a cluster a network of scope cluster is first
// checked to be one segment on each node.
func (e *Engine) attachable(ctx context.Context, f networkFacts, ln legNodes, r AttachRequest, legs map[string]netip.Addr,
	answers map[string]nodeAnswer) (attachment, error) {
	seg := resolve.Segment{Bridge: r.Bridge, VLAN: r.VLAN}
	id := store.SegmentID(r.Bridge, r.VLAN)
	var a attachment
	have, attached := f.nets[id]
	if attached && ln.clustered {
		if err := extendable(have, r, legs); err != nil {
			return a, err
		}
		a.ext = id
	}
	uplink := false
	for _, node := range legOrder(store.Network{Legs: legs}, e.d.Node) {
		g := f
		if node != e.d.Node {
			var err error
			if g, err = e.onNode(ctx, f, node); err != nil {
				return a, err
			}
		}
		if r.Scope == store.ScopeCluster && ln.clustered {
			if why := g.notOneSegment(seg); why != "" {
				return a, fmt.Errorf("%w: %s; attach it with the scope node", ErrRefused, why)
			}
		}
		var (
			overlap string
			err     error
		)
		if node == e.d.Node {
			overlap, err = e.localLeg(ctx, f, r, legs[node], a.ext)
		} else {
			overlap, err = g.remoteRefusal(seg, r, legs[node], answers[node], a.ext)
		}
		if err != nil {
			return a, err
		}
		a.overlap = cmp.Or(a.overlap, overlap)
		uplink = uplink || g.uplink(seg.Bridge)
	}
	if a.ext != "" {
		n := have
		n.Legs = maps.Clone(have.Legs)
		maps.Copy(n.Legs, legs)
		n.LegMACs = maps.Clone(have.LegMACs)
		if err := withLegMACs(&n, ln); err != nil {
			return a, err
		}
		a.network = n
		return a, nil
	}
	mark, err := e.freeMark(f.nets)
	if err != nil {
		return a, err
	}
	a.network = store.Network{
		ID: id, Kind: store.NetworkAttach, Bridge: r.Bridge, VLAN: r.VLAN, Prefix: r.Prefix,
		Legs: legs, LegMAC: store.LegMACFor(store.NetworkAttach, mark), Mark: mark,
		Uplink: uplink, OverlapOK: a.overlap != "", Scope: r.Scope, CreatedAt: e.d.Now(),
	}
	if ln.clustered {
		if err := withLegMACs(&a.network, ln); err != nil {
			return a, err
		}
	}
	return a, nil
}

// localLeg checks the leg of this node, as an attach on a node alone does.
func (e *Engine) localLeg(ctx context.Context, f networkFacts, r AttachRequest, addr netip.Addr, ext string) (string, error) {
	c, err := f.lookup(r.Bridge, r.VLAN)
	if err != nil {
		return "", err
	}
	overlap, err := f.legRefusal(c, r, addr, ext)
	if err != nil {
		return "", err
	}
	if f.canProbe(c) {
		seg := resolve.Segment{Bridge: c.Bridge, VLAN: c.VLAN}
		answers, err := e.probe(ctx, seg, addr)
		if err != nil {
			return "", err
		}
		if len(answers) > 0 {
			return "", fieldError("address", "%s is in use on %s: %s answered", addr, seg, strings.Join(answers, ", "))
		}
	}
	return overlap, nil
}

// extendable refuses legs added to a network attached that are not of it:
// another kind, prefix or scope, a node that has its leg, or an address of
// a leg it has.
func extendable(n store.Network, r AttachRequest, legs map[string]netip.Addr) error {
	switch {
	case n.Kind != store.NetworkAttach:
		return fmt.Errorf("%w: %s is pco's managed network, which is not attached", ErrRefused, n.ID)
	case n.Prefix != r.Prefix:
		return fmt.Errorf("%w: network %s is attached already in %s; a leg added to it is in that prefix", ErrRefused, n.ID, n.Prefix)
	case r.Scope != "" && r.Scope != n.ScopeName():
		return fmt.Errorf("%w: network %s is attached already of scope %s; a leg added to it is of that scope", ErrRefused, n.ID, n.ScopeName())
	case n.ScopeName() == store.ScopeNode:
		return fmt.Errorf("%w: network %s is of scope node and has its one leg; detach it first to attach it anew", ErrRefused, n.ID)
	}
	for _, node := range slices.Sorted(maps.Keys(legs)) {
		if _, has := n.Legs[node]; has {
			return fmt.Errorf("%w: network %s is attached already with a leg on node %s; detach it first to attach it anew", ErrRefused, n.ID, node)
		}
		for other, addr := range n.Legs {
			if addr == legs[node] {
				return fieldError("legs", "%s is the leg of node %s on network %s", addr, other, n.ID)
			}
		}
	}
	return nil
}

// withLegMACs gives each leg of n the MAC of its node's index, which an
// admitted node has; a node without one has no MAC, and no leg.
func withLegMACs(n *store.Network, ln legNodes) error {
	if n.LegMACs == nil {
		n.LegMACs = map[string]string{}
	}
	for _, node := range slices.Sorted(maps.Keys(n.Legs)) {
		index, ok := ln.indexes[node]
		if !ok {
			return fmt.Errorf("%w: node %s has no index yet, so the MAC of its leg is not known; attach once it is admitted", ErrRefused, node)
		}
		n.LegMACs[node] = store.NodeLegMAC(index, n.Mark)
	}
	return nil
}

// legsMessage is the event of an attach on a cluster.
func legsMessage(n store.Network, overlap string) string {
	seg := resolve.Segment{Bridge: n.Bridge, VLAN: n.VLAN}
	var legs []string
	for _, node := range slices.Sorted(maps.Keys(n.Legs)) {
		legs = append(legs, fmt.Sprintf("%s on %s", netip.PrefixFrom(n.Legs[node], n.Prefix.Bits()), node))
	}
	msg := fmt.Sprintf("network %s of scope %s is attached: the leg %s on %s with %s, mark %d", n.ID, n.ScopeName(), gateway.LegIface(n.Mark),
		seg, strings.Join(legs, ", "), n.Mark)
	if n.Uplink {
		msg += "; on a bridge with a physical uplink, confirmed: " + fmt.Sprintf(uplinkWarning, n.Bridge)
	}
	if n.OverlapOK {
		msg += "; " + overlap + ", allowed with --overlap-ok"
	}
	return msg + "; the next cycle raises the legs"
}

// addedMessage is the event of legs added to a network attached.
func addedMessage(n store.Network, added map[string]netip.Addr) string {
	var legs []string
	for _, node := range slices.Sorted(maps.Keys(added)) {
		legs = append(legs, fmt.Sprintf("%s on %s", netip.PrefixFrom(added[node], n.Prefix.Bits()), node))
	}
	return fmt.Sprintf("network %s has %s more: %s; the next cycle raises them", n.ID, plural(len(added), "leg", "legs"), strings.Join(legs, ", "))
}

// freeMark is the lowest mark no network has and none had since the gateway
// was last synced: a mark freed and taken again before that would move to
// another bridge in one sync.
func (e *Engine) freeMark(nets map[string]store.Network) (int, error) {
	used := map[int]bool{}
	for _, n := range nets {
		used[n.Mark] = true
	}
	freed := false
	for m := 1; m <= gateway.MaxMark; m++ {
		switch {
		case used[m]:
		case e.freedMarks[m]:
			freed = true
		default:
			return m, nil
		}
	}
	if freed {
		return 0, fmt.Errorf("%w: the marks free now were freed by a detach the gateway has not been synced since; attach again after the next cycle", ErrRefused)
	}
	return 0, fmt.Errorf("%w: this node reaches %d networks already, the most it can: the SNAT through each takes a block of %d source ports",
		ErrRefused, gateway.MaxMark, gateway.PortBlockSize)
}

// attachedMessage is the event of an attach.
func attachedMessage(n store.Network, addr netip.Addr, overlap string) string {
	seg := resolve.Segment{Bridge: n.Bridge, VLAN: n.VLAN}
	msg := fmt.Sprintf("network %s is attached: the leg %s with %s on %s, mark %d", n.ID, gateway.LegIface(n.Mark),
		netip.PrefixFrom(addr, n.Prefix.Bits()), seg, n.Mark)
	if n.Uplink {
		msg += "; on a bridge with a physical uplink, confirmed: " + fmt.Sprintf(uplinkWarning, n.Bridge)
	}
	if n.OverlapOK {
		msg += "; " + overlap + ", allowed with --overlap-ok"
	}
	return msg + "; the next cycle raises the leg"
}

// Detach removes the leg of a network read at revision rev, its service
// addresses and the network. While routes are served through it, it is
// refused unless confirm says that the admin saw them. The last network has
// pco-netns.service disabled, which takes nothing down.
func (e *Engine) Detach(ctx context.Context, id string, rev int64, served []string) error {
	if id == "" {
		return fieldError("id", "name the network to detach, as vmbr1 or vmbr1.20")
	}
	if err := e.acquireWriter(ctx); err != nil {
		return err
	}
	defer e.release()

	nets, err := e.d.Store.Networks()
	if err != nil {
		return fmt.Errorf("reading the networks: %w", err)
	}
	n, ok := nets[id]
	switch {
	case !ok:
		return fmt.Errorf("%w: network %s is not attached", ErrNotFound, id)
	case n.Kind != store.NetworkAttach:
		return fmt.Errorf("%w: %s is pco's managed network, which is not detached this way", ErrRefused, id)
	case n.Rev != rev:
		return fmt.Errorf("%w: network %s changed since it was read at revision %d and is at revision %d now; read it again", ErrRefused, id, rev, n.Rev)
	}
	named := slices.Compact(slices.Sorted(slices.Values(served)))
	served = servedThrough(e.State().Routes, id)
	switch {
	case len(served) == 0:
	case len(named) == 0:
		return fmt.Errorf("%w: network %s serves %s through its leg: %s; detaching it stops serving them that way, name them to detach it all the same",
			ErrRefused, id, plural(len(served), "route", "routes"), strings.Join(served, ", "))
	case !slices.Equal(named, served):
		return fmt.Errorf("%w: network %s serves %s through its leg: %s, not %s as the detach named; read it again and name the routes it serves now",
			ErrRefused, id, plural(len(served), "route", "routes"), strings.Join(served, ", "), strings.Join(named, ", "))
	}
	if e.d.Gateway != nil {
		if err := e.d.Gateway.Detach(ctx, id); err != nil && !errors.Is(err, gateway.ErrNotPinned) {
			return fmt.Errorf("removing the leg of network %s: %w", id, err)
		}
	}
	released, failed := e.releaseNetwork(id)
	if err := e.d.Store.DeleteNetwork(id, rev); err != nil {
		if errors.Is(err, store.ErrRevision) {
			return fmt.Errorf("%w: network %s changed while it was detached; its leg comes back with the next cycle; read it again", ErrRefused, id)
		}
		return fmt.Errorf("removing network %s: %w", id, err)
	}
	if e.freedMarks == nil {
		e.freedMarks = map[int]bool{}
	}
	e.freedMarks[n.Mark] = true
	msg := fmt.Sprintf("network %s is detached: its leg %s is removed and %s released", id, gateway.LegIface(n.Mark), plural(released, "service address", "service addresses"))
	if len(served) > 0 {
		msg += "; it served " + strings.Join(served, ", ")
	}
	if len(failed) > 0 {
		msg += "; releasing " + strings.Join(failed, "; ") + " failed, which the next cycle does"
	}
	if len(nets) == 1 && e.d.Networks != nil {
		if err := e.d.Networks.DisableNamespace(ctx); err != nil {
			msg += fmt.Sprintf("; pco-netns.service could not be disabled: %v", err)
		} else {
			msg += "; no network is left, and pco-netns.service is disabled"
		}
	}
	e.networkEvent(ctx, id, msg)
	e.Trigger()
	return nil
}

// releaseNetwork deletes the service addresses of a network and returns how
// many, and what failed.
func (e *Engine) releaseNetwork(id string) (released int, failed []string) {
	mappings, err := e.d.Store.ServiceAddrs()
	if err != nil {
		return 0, []string{fmt.Sprintf("the service addresses could not be read: %v", err)}
	}
	for _, m := range mappings {
		if m.Network != id {
			continue
		}
		if err := e.d.Store.ReleaseServiceAddr(m.Network, m.Addr); err != nil {
			failed = append(failed, fmt.Sprintf("%s of %s: %v", m.Service, m.Addr, err))
			continue
		}
		released++
	}
	nodes, err := e.d.Store.NodeServiceAddrs()
	if err != nil {
		return released, append(failed, fmt.Sprintf("the service addresses of single nodes could not be read: %v", err))
	}
	for _, m := range nodes {
		if m.Network != id {
			continue
		}
		if err := e.d.Store.ReleaseNodeServiceAddr(m.Network, m.Node, m.Addr); err != nil {
			failed = append(failed, fmt.Sprintf("%s of %s on node %s: %v", m.Service, m.Addr, m.Node, err))
			continue
		}
		released++
	}
	return released, failed
}

// disableNamespace disables pco-netns.service after an attach of the first
// network that did not go through; a failure is logged, the unit runs
// nothing a network needs.
func (e *Engine) disableNamespace(ctx context.Context) {
	if err := e.d.Networks.DisableNamespace(ctx); err != nil {
		e.d.Log.Warn().Err(err).Msg("disabling pco-netns.service after an attach that did not go through failed")
	}
}

// networkEvent records an admin action on a network, with who asked for it.
func (e *Engine) networkEvent(ctx context.Context, id, msg string) {
	e.events.add(Event{At: e.d.Now(), Level: levelInfo, Kind: kindNetwork, Subject: id, Message: msg, Actor: ActorOf(ctx)})
}

// plural is n with the word for one or for more.
func plural(n int, one, more string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, more)
}

// checkManagedSubnet refuses a change of the managed subnet while the managed
// network or a lease exists, and a subnet that overlaps a route of a node, an
// address of a guest, static or reported, or the prefix of an attached
// network, naming what it overlaps. A subnet that stays is not checked, and
// one the store refuses is left to it.
func (e *Engine) checkManagedSubnet(ctx context.Context, old, s store.Settings) error {
	const field = "managed.subnet"
	subnet := s.Managed.Subnet
	if s.Managed == (store.ManagedSettings{}) {
		subnet = store.DefaultSettings().Managed.Subnet
	}
	if subnet == old.Managed.Subnet || !subnet.IsValid() || !subnet.Addr().Is4() {
		return nil
	}
	nets, err := e.d.Store.Networks()
	if err != nil {
		return fmt.Errorf("reading the networks: %w", err)
	}
	if _, ok := nets[gateway.ManagedBridge]; ok {
		return fieldError(field, "%s cannot be changed while the managed network exists: its leg and its leases are in %s", field, old.Managed.Subnet)
	}
	leases, err := e.d.Store.Leases()
	if err != nil {
		return fmt.Errorf("reading the leases: %w", err)
	}
	if n := len(leases); n > 0 {
		return fieldError(field, "%s cannot be changed while %s on the managed network", field, plural(n, "lease exists", "leases exist"))
	}
	look, cancel := e.timeout(ctx, lookTimeout)
	defer cancel()
	return subnetOverlaps(subnet, nets, e.d.Inventory.Refresh(look))
}

// subnetOverlaps refuses a managed subnet that overlaps a route of a node, an
// address of a guest, static or reported, or the prefix of an attached
// network, naming what it overlaps; a snapshot that is not complete cannot
// tell, and is refused as well.
func subnetOverlaps(subnet netip.Prefix, nets map[string]store.Network, snap inventory.Snapshot) error {
	const field = "managed.subnet"
	if !snap.Complete {
		return fmt.Errorf("%w: the inventory is incomplete (%s), so %s cannot be checked against the routes of the nodes and the addresses "+
			"of the guests; try again once it is complete", ErrRefused, strings.Join(snap.Problems, "; "), field)
	}
	if msg := snap.ManagedSubnetConflict(subnet, AttachedPrefixes(nets)); msg != "" {
		return fieldError(field, "%s %s %s", field, subnet, msg)
	}
	return nil
}

// AttachedPrefixes are the prefixes of the attached networks, by id.
func AttachedPrefixes(nets map[string]store.Network) []inventory.AttachedPrefix {
	var out []inventory.AttachedPrefix
	for _, id := range slices.Sorted(maps.Keys(nets)) {
		if n := nets[id]; n.Kind == store.NetworkAttach {
			out = append(out, inventory.AttachedPrefix{ID: id, Prefix: n.Prefix})
		}
	}
	return out
}
