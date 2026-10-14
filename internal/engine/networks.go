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
}

// NetworkView is a network pco reaches through a leg, with how its leg is
// and what it serves.
type NetworkView struct {
	store.Network
	Legs    []LegView `json:"legStates"` // this node only until the cluster milestone
	Targets int       `json:"targets"`   // service addresses in use
	Routes  int       `json:"routes"`    // routes served through it
}

// LegView is the leg of a network on a node, as the gateway last said.
type LegView struct {
	Node    string     `json:"node"`
	Addr    netip.Addr `json:"addr"`
	Up      bool       `json:"up"`
	Problem string     `json:"problem,omitempty"`
}

// NetworkDetail is a network with its service addresses and the hostnames of
// the routes served through it.
type NetworkDetail struct {
	NetworkView
	Services []store.ServiceAddr `json:"services"`
	Served   []string            `json:"served"`
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
type ProbeRequest struct {
	Bridge  string       `json:"bridge"`
	VLAN    int          `json:"vlan,omitempty"`
	Prefix  netip.Prefix `json:"prefix,omitzero"`
	Address netip.Addr   `json:"address,omitzero"`
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
}

// AttachRequest is a network to attach: a bridge and VLAN, the prefix of the
// guests there and the address of the leg on this node.
type AttachRequest struct {
	Bridge        string       `json:"bridge"`
	VLAN          int          `json:"vlan,omitempty"`
	Prefix        netip.Prefix `json:"prefix"`
	Address       netip.Addr   `json:"address"`
	ConfirmUplink bool         `json:"confirmUplink"`
	OverlapOK     bool         `json:"overlapOK"` // the prefix may overlap a route of the node
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
	return networkViews(nets, mappings, e.State().Routes, e.LegStates(), e.d.Node), nil
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
	views := networkViews(map[string]store.Network{id: n}, mappings, routes, e.LegStates(), e.d.Node)
	d := NetworkDetail{NetworkView: views[0], Services: []store.ServiceAddr{}, Served: servedThrough(routes, id)}
	for _, m := range mappings {
		if m.Network == id {
			d.Services = append(d.Services, m)
		}
	}
	slices.SortFunc(d.Services, func(a, b store.ServiceAddr) int { return a.Service.Compare(b.Service) })
	return d, nil
}

func (e *Engine) storedNetworks() (map[string]store.Network, []store.ServiceAddr, error) {
	nets, err := e.d.Store.Networks()
	if err != nil {
		return nil, nil, fmt.Errorf("reading the networks: %w", err)
	}
	mappings, err := e.d.Store.ServiceAddrs()
	if err != nil {
		return nil, nil, fmt.Errorf("reading the service addresses: %w", err)
	}
	return nets, mappings, nil
}

// networkViews are the views of the networks, by id: the leg of node with
// what the gateway said of it, the service addresses of each and the routes
// served through it.
func networkViews(nets map[string]store.Network, mappings []store.ServiceAddr, routes []RouteView, legs []gateway.LegState, node string) []NetworkView {
	out := make([]NetworkView, 0, len(nets))
	for _, id := range slices.Sorted(maps.Keys(nets)) {
		n := nets[id]
		v := NetworkView{Network: n, Legs: []LegView{}, Routes: len(servedThrough(routes, id))}
		if addr, ok := n.Legs[node]; ok {
			leg := LegView{Node: node, Addr: addr, Problem: "the gateway has not reported the leg yet"}
			if i := slices.IndexFunc(legs, func(s gateway.LegState) bool { return s.ID == id }); i >= 0 {
				leg.Up, leg.Problem = legs[i].Up, legs[i].Problem
				if !leg.Up && leg.Problem == "" {
					leg.Problem = "the gateway does not say why"
				}
			}
			v.Legs = append(v.Legs, leg)
		}
		for _, m := range mappings {
			if m.Network == id {
				v.Targets++
			}
		}
		out = append(out, v)
	}
	return out
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
	c.st.Networks = networkViews(nets, mappings, c.st.Routes, c.e.LegStates(), c.e.d.Node)
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
	appliance bool
	self      model.GuestRef // the appliance's own container
	config    []pve.NodeIface
	vnets     []pve.VNet
	host      []resolve.HostIface
	snap      inventory.Snapshot
	nets      map[string]store.Network
	settings  store.Settings
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
// configured on the node, an SDN vnet, or one the node has now.
func (f networkFacts) isBridge(name string) bool {
	c, ok := f.configured(name)
	return ok && c.Type == "bridge" || f.isVnet(name) || f.liveBridge(name)
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
		if r.seg != seg && r.prefix.Overlaps(prefix) {
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
func usable(c CandidateNetwork) error {
	seg := resolve.Segment{Bridge: c.Bridge, VLAN: c.VLAN}
	switch {
	case c.Refused != "":
		return fmt.Errorf("%w: %s", ErrRefused, c.Refused)
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
	if err := usable(c); err != nil {
		return ProbeResult{}, err
	}
	seg := resolve.Segment{Bridge: c.Bridge, VLAN: c.VLAN}
	res := ProbeResult{Bridge: c.Bridge, VLAN: c.VLAN, Prefix: r.Prefix, Address: r.Address, AnsweredBy: []string{}, Uplink: c.Uplink}
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
func (e *Engine) Attach(ctx context.Context, r AttachRequest) (NetworkView, error) {
	if err := probeArgs(r.Bridge, r.VLAN); err != nil {
		return NetworkView{}, err
	}
	if err := checkPrefix(r.Prefix); err != nil {
		return NetworkView{}, err
	}
	if err := checkAddress(r.Prefix, r.Address); err != nil {
		return NetworkView{}, err
	}
	if err := e.networksHere(); err != nil {
		return NetworkView{}, err
	}
	if err := e.acquireWriter(ctx); err != nil {
		return NetworkView{}, err
	}
	defer e.release()

	f, err := e.networkFacts(ctx)
	if err != nil {
		return NetworkView{}, err
	}
	c, err := f.lookup(r.Bridge, r.VLAN)
	if err != nil {
		return NetworkView{}, err
	}
	n, err := e.attachable(ctx, f, c, r)
	if err != nil {
		return NetworkView{}, err
	}
	first := len(f.nets) == 0
	if first {
		if err := e.d.Networks.EnableNamespace(ctx); err != nil {
			return NetworkView{}, fmt.Errorf("enabling pco-netns.service for the first network: %w", err)
		}
	}
	if err := e.d.Store.SaveNetwork(n, 0); err != nil {
		if first {
			e.disableNamespace(ctx)
		}
		if errors.Is(err, store.ErrRevision) {
			return NetworkView{}, fmt.Errorf("%w: network %s was attached meanwhile", ErrRefused, n.ID)
		}
		return NetworkView{}, fmt.Errorf("saving network %s: %w", n.ID, err)
	}
	e.networkEvent(ctx, n.ID, attachedMessage(n, r.Address, f.overlap(n.Prefix, resolve.Segment{Bridge: n.Bridge, VLAN: n.VLAN})))
	e.Trigger()
	nets, mappings, err := e.storedNetworks()
	if err != nil {
		return NetworkView{}, err
	}
	saved, ok := nets[n.ID]
	if !ok {
		return NetworkView{}, fmt.Errorf("network %s is not stored after it was saved", n.ID)
	}
	return networkViews(map[string]store.Network{n.ID: saved}, mappings, nil, e.LegStates(), e.d.Node)[0], nil
}

// attachable checks a request against the facts and returns the network it
// makes: the refusals of Attach in their order, the last of which probes the
// address once more.
func (e *Engine) attachable(ctx context.Context, f networkFacts, c CandidateNetwork, r AttachRequest) (store.Network, error) {
	seg := resolve.Segment{Bridge: c.Bridge, VLAN: c.VLAN}
	if err := usable(c); err != nil {
		return store.Network{}, err
	}
	if c.Attached != "" {
		return store.Network{}, fmt.Errorf("%w: network %s is attached already; detach it first to attach it anew", ErrRefused, c.Attached)
	}
	if c.Uplink && !r.ConfirmUplink {
		return store.Network{}, fieldError("confirmUplink", "%s has a physical uplink: "+uplinkWarning+"; %s", seg, c.Bridge, UplinkHint)
	}
	if p := gateway.ServicePrefix; r.Prefix.Overlaps(p) {
		return store.Network{}, fieldError("prefix", "%s overlaps the service prefix %s, whose addresses the connectors dial", r.Prefix, p)
	}
	if p := f.settings.Managed.Subnet; p.IsValid() && r.Prefix.Overlaps(p) {
		return store.Network{}, fieldError("prefix", "%s overlaps the managed subnet %s (managed.subnet)", r.Prefix, p)
	}
	overlap := f.overlap(r.Prefix, seg)
	if overlap != "" && !r.OverlapOK {
		return store.Network{}, fieldError("overlapOK", "%s; %s", overlap, OverlapHint)
	}
	for _, id := range slices.Sorted(maps.Keys(f.nets)) {
		if n := f.nets[id]; n.Bridge == c.Bridge && n.Prefix.Overlaps(r.Prefix) {
			return store.Network{}, fieldError("prefix", "%s overlaps %s of network %s on the same bridge %s", r.Prefix, n.Prefix, id, c.Bridge)
		}
	}
	if who, ok := f.taken(c)[r.Address]; ok {
		return store.Network{}, fieldError("address", "%s is %s", r.Address, cmp.Or(who, "an address of the node"))
	}
	if f.canProbe(c) {
		answers, err := e.probe(ctx, seg, r.Address)
		if err != nil {
			return store.Network{}, err
		}
		if len(answers) > 0 {
			return store.Network{}, fieldError("address", "%s is in use on %s: %s answered", r.Address, seg, strings.Join(answers, ", "))
		}
	}
	mark, err := e.freeMark(f.nets)
	if err != nil {
		return store.Network{}, err
	}
	return store.Network{
		ID: store.SegmentID(c.Bridge, c.VLAN), Kind: store.NetworkAttach, Bridge: c.Bridge, VLAN: c.VLAN, Prefix: r.Prefix,
		Legs: map[string]netip.Addr{e.d.Node: r.Address}, LegMAC: store.LegMACFor(store.NetworkAttach, mark), Mark: mark,
		Uplink: c.Uplink, OverlapOK: overlap != "", CreatedAt: e.d.Now(),
	}, nil
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
