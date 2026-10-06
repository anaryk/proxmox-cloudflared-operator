package gateway

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// dadWindow is how long duplicate-address detection listens: three probes
// are sent within it.
const dadWindow = 300 * time.Millisecond

// Sync makes the legs, the elements of the table, the pins and the sticky
// entries those given, in that order.
func (b *hostBackend) Sync(ctx context.Context, legs []Leg, services []Service, pins []Pin, sticky []Sticky) ([]LegState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sync(ctx, legs, services, pins, sticky, true)
}

// sync is Sync; without withPins the pins of the namespace stay as they are.
func (b *hostBackend) sync(ctx context.Context, legs []Leg, services []Service, pins []Pin, sticky []Sticky, withPins bool) ([]LegState, error) {
	if err := b.namespace(false); err != nil {
		return nil, err
	}
	states := make([]LegState, 0, len(legs))
	valid := validLegs(legs)
	_, refused := validServices(valid, services)
	for _, l := range legs {
		if problem := legProblem(l, legs); problem != "" {
			states = append(states, LegState{ID: l.ID, Iface: l.Iface(), Problem: problem})
			continue
		}
		st, err := b.leg(ctx, l)
		if err != nil {
			return nil, fmt.Errorf("leg %s of network %s: %w", l.Iface(), l.ID, err)
		}
		if p := refused[l.ID]; len(p) > 0 {
			st.Problem = strings.Join(slices.DeleteFunc(append([]string{st.Problem}, p...), func(s string) bool { return s == "" }), "; ")
		}
		states = append(states, st)
	}
	if err := b.removeStrayLegs(valid); err != nil {
		return nil, err
	}
	for id := range b.records {
		if !slices.ContainsFunc(valid, func(l Leg) bool { return l.ID == id }) {
			delete(b.records, id)
		}
	}
	b.legs, b.services = valid, slices.Clone(services)
	if err := b.ensureTable(ctx, true); err != nil {
		return nil, err
	}
	if withPins {
		if err := b.syncPins(valid, pins); err != nil {
			return nil, err
		}
		b.pins = slices.Clone(pins)
	}
	if err := b.syncSticky(sticky, false); err != nil {
		return nil, err
	}
	b.sticky = slices.Clone(sticky)
	return states, nil
}

// legProblem says why a leg cannot be set up at all; the engine never gives
// such a leg, and the backend does not take its word for it.
func legProblem(l Leg, all []Leg) string {
	switch {
	case l.Mark < 1 || l.Mark > MaxMark:
		return fmt.Sprintf("mark %d is outside 1-%d", l.Mark, MaxMark)
	case !l.Prefix.IsValid() || !l.Prefix.Addr().Is4() || l.Prefix.Bits() > 30:
		return fmt.Sprintf("%s is no IPv4 address with a prefix of at most /30", l.Prefix)
	case l.Prefix.Overlaps(ServicePrefix):
		return fmt.Sprintf("%s overlaps the service prefix %s", l.Prefix, ServicePrefix)
	case len(l.MAC) != 6 || l.MAC[0]&1 != 0:
		return fmt.Sprintf("%s is no unicast MAC", l.MAC)
	case l.Bridge == "" || len(l.Bridge) > 15 || strings.ContainsAny(l.Bridge, "/ \t\n\""):
		return fmt.Sprintf("%q is no bridge name", l.Bridge)
	case l.VLAN < 0 || l.VLAN > 4094:
		return fmt.Sprintf("VLAN %d is outside 0-4094", l.VLAN)
	}
	for _, o := range all {
		if o.ID == l.ID {
			break
		}
		if o.Mark == l.Mark {
			return fmt.Sprintf("mark %d is that of network %s too", l.Mark, o.ID)
		}
	}
	return ""
}

// validLegs returns the legs that can be set up.
func validLegs(legs []Leg) []Leg {
	return slices.DeleteFunc(slices.Clone(legs), func(l Leg) bool { return legProblem(l, legs) != "" })
}

// bridgeEnd is where the bridge end of a leg goes.
type bridgeEnd struct {
	master    string
	filtering bool // the bridge filters VLANs
	// vid is the one VLAN the port has on a bridge that filters VLANs,
	// untagged and its own: the leg's tag, and for no tag the bridge's default
	// VLAN, which is what a port enslaved to it gets. 0 leaves the VLANs of
	// the port alone.
	vid int
	mtu int
}

// bridgeOf returns where the bridge end of a leg goes: the leg's bridge, or
// vmbr<X>v<tag> for a tag on a bridge that does not filter VLANs. A bridge
// that is not there is a problem, not an error.
func (b *hostBackend) bridgeOf(bridge string, vlan int) (end bridgeEnd, problem string, err error) {
	br, ok, err := b.sys.link(onHost, bridge)
	switch {
	case err != nil:
		return bridgeEnd{}, "", fmt.Errorf("reading %s: %w", bridge, err)
	case !ok || br.kind != "bridge":
		return bridgeEnd{}, fmt.Sprintf("bridge %s is absent", bridge), nil
	case vlan == 0 || br.vlanFiltering:
		end = bridgeEnd{master: bridge, filtering: br.vlanFiltering, mtu: br.mtu}
		switch {
		case !br.vlanFiltering:
		case vlan > 0:
			end.vid = vlan
		default:
			end.vid = br.defaultPVID
		}
		return end, "", nil
	}
	master := fmt.Sprintf("%sv%d", bridge, vlan)
	vb, ok, err := b.sys.link(onHost, master)
	switch {
	case err != nil:
		return bridgeEnd{}, "", fmt.Errorf("reading %s: %w", master, err)
	case !ok || vb.kind != "bridge":
		return bridgeEnd{}, fmt.Sprintf("bridge %s is absent", master), nil
	}
	return bridgeEnd{master: master, mtu: vb.mtu}, "", nil
}

// leg sets up one leg: the pair with its MACs and the bridge's MTU, IPv6
// off, the bridge end enslaved with its VLAN before it goes up, the sticky
// entry of the leg's MAC, the address (after DAD on an uplink bridge), the
// leg's table and its rule. A pair that was made for another network is made
// anew first.
func (b *hostBackend) leg(ctx context.Context, l Leg) (LegState, error) {
	st := LegState{ID: l.ID, Iface: l.Iface()}
	rec := b.records[l.ID]
	if rec == nil || rec.mark != l.Mark {
		rec = &legRecord{mark: l.Mark}
		b.records[l.ID] = rec
	}
	end, problem, err := b.bridgeOf(l.Bridge, l.VLAN)
	if err != nil {
		return st, err
	}
	if problem != "" {
		st.Problem = problem
		return st, nil
	}
	master, filtering, mtu := end.master, end.filtering, end.mtu
	port, iface := bridgePort(l.Mark), l.Iface()
	moved, err := b.legMoved(l, port, end)
	if err != nil {
		return st, err
	}
	if moved {
		b.log.Info().Str("leg", iface).Str("network", l.ID).Msg("a mark passed to another network; making its pair anew")
		if err := b.removePair(l.Mark); err != nil {
			return st, err
		}
	}
	if err := b.ensurePair(port, bridgeEndMAC(l.MAC), iface, l.MAC, mtu); err != nil {
		return st, err
	}
	if err := b.ensureSysctls(onHost, [][2]string{{"net.ipv6.conf." + port + ".disable_ipv6", "1"}}); err != nil {
		return st, err
	}
	if err := b.ensureSysctls(inNS, ifaceSysctls(iface)); err != nil {
		return st, err
	}
	if err := b.ensureMTU(port, iface, mtu); err != nil {
		return st, err
	}
	if err := b.ensurePort(port, master, end.vid); err != nil {
		return st, err
	}
	if err := b.ensureUp(onHost, port); err != nil {
		return st, err
	}
	if err := b.ensureUp(inNS, iface); err != nil {
		return st, err
	}
	if err := b.ensureFDB(port, l.MAC, fdbVLAN(filtering, l.VLAN)); err != nil {
		return st, err
	}
	rec.created, st.MTU = true, mtu
	raised, problem, err := b.legAddress(ctx, l, rec, master, port)
	if err != nil {
		return st, err
	}
	rec.raised, st.Problem, st.DADAt = raised, problem, rec.dadAt
	if err := b.legRoutes(l, raised); err != nil {
		return st, err
	}
	st.Up = raised
	return st, nil
}

// legMoved reports whether the pair that is there for the leg's mark was set
// up for another network: its bridge end is in another bridge, or in the
// leg's bridge in another VLAN, or its address is in another prefix. A mark
// moves so when the engine gives a freed mark to the network that takes the
// old one's place within one Sync; the kernel's state says so, as a process
// that did not make the pair, as pco netns sync is, has no memory of it.
// What the leg learned in the old network, the neighbours of its addresses,
// is the MACs of guests that are not in the bridge now, and where subnets
// are identical it would be used for the address of a guest there. A bridge
// end that lost its bridge, as an ifreload leaves it, says nothing of its
// bridge or its VLANs, but its address still says which prefix it served.
// The VLAN is the one ensurePort would give the port: the tag, or for the
// untagged network the bridge's default, which is not always 1.
func (b *hostBackend) legMoved(l Leg, port string, end bridgeEnd) (bool, error) {
	iface := l.Iface()
	have, ok, err := b.sys.link(onHost, port)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", port, err)
	}
	if _, peer, err := b.sys.link(inNS, iface); err != nil {
		return false, fmt.Errorf("reading %s in %s: %w", iface, inNS, err)
	} else if !ok || !peer {
		return false, nil
	}
	if have.master != "" && have.master != end.master {
		return true, nil
	}
	if have.master == end.master && end.vid > 0 {
		vlans, err := b.sys.portVLANs(port)
		if err != nil {
			return false, fmt.Errorf("reading the VLANs of %s: %w", port, err)
		}
		if len(vlans) > 0 && !slices.ContainsFunc(vlans, func(v portVLAN) bool { return v.vid == end.vid }) {
			return true, nil
		}
	}
	addrs, err := b.sys.addrs(inNS, iface)
	if err != nil {
		return false, fmt.Errorf("reading the addresses of %s: %w", iface, err)
	}
	return slices.ContainsFunc(addrs, func(a address) bool { return a.prefix.Masked() != l.Prefix.Masked() }), nil
}

// fdbVLAN is the VLAN of a forwarding entry on a leg's port: the tag on a
// bridge that filters VLANs, otherwise none.
func fdbVLAN(filtering bool, vlan int) int {
	if filtering {
		return vlan
	}
	return 0
}

func (b *hostBackend) ensureMTU(port, iface string, mtu int) error {
	for _, e := range []struct {
		s    side
		name string
	}{{onHost, port}, {inNS, iface}} {
		l, _, err := b.sys.link(e.s, e.name)
		if err != nil {
			return fmt.Errorf("reading %s in %s: %w", e.name, e.s, err)
		}
		if l.mtu == mtu || mtu == 0 {
			continue
		}
		if err := b.sys.setMTU(e.s, e.name, mtu); err != nil {
			return fmt.Errorf("setting the MTU of %s in %s: %w", e.name, e.s, err)
		}
	}
	return nil
}

// ensurePort enslaves the bridge end to its bridge, on a bridge that filters
// VLANs with vid as its only VLAN, untagged and the port's own, and keeps it
// from being isolated: guests on an isolating bridge reach the leg through
// it. A vid of 0 leaves the VLANs of the port alone.
func (b *hostBackend) ensurePort(port, master string, vid int) error {
	l, _, err := b.sys.link(onHost, port)
	if err != nil {
		return fmt.Errorf("reading %s: %w", port, err)
	}
	if l.master != master {
		if err := b.sys.setMaster(port, master); err != nil {
			return fmt.Errorf("enslaving %s to %s: %w", port, master, err)
		}
		b.log.Info().Str("port", port).Str("bridge", master).Msg("enslaved a leg to its bridge")
	}
	if vid > 0 {
		have, err := b.sys.portVLANs(port)
		if err != nil {
			return fmt.Errorf("reading the VLANs of %s: %w", port, err)
		}
		want := portVLAN{vid: vid, pvid: true, untagged: true}
		if !slices.Contains(have, want) {
			if err := b.sys.addPortVLAN(port, want); err != nil {
				return fmt.Errorf("adding VLAN %d to %s: %w", vid, port, err)
			}
		}
		for _, v := range have {
			if v.vid == vid {
				continue
			}
			if err := b.sys.delPortVLAN(port, v.vid); err != nil {
				return fmt.Errorf("removing VLAN %d from %s: %w", v.vid, port, err)
			}
		}
	}
	if l.isolated {
		if err := b.sys.setIsolated(port, false); err != nil {
			return fmt.Errorf("making %s a port that is not isolated: %w", port, err)
		}
	}
	return nil
}

// ensureFDB keeps a static sticky entry of the MAC on the port.
func (b *hostBackend) ensureFDB(port string, mac net.HardwareAddr, vlan int) error {
	have, err := b.sys.fdb(port)
	if err != nil {
		return fmt.Errorf("reading the forwarding entries of %s: %w", port, err)
	}
	if slices.ContainsFunc(have, func(e fdbEntry) bool { return stickyOf(e, mac, vlan) }) {
		return nil
	}
	if err := b.sys.addSticky(port, mac, vlan); err != nil {
		return fmt.Errorf("adding the sticky entry of %s on %s: %w", mac, port, err)
	}
	return nil
}

// stickyOf reports whether the entry is the static sticky one of the MAC on
// the VLAN; for no VLAN, on any.
func stickyOf(e fdbEntry, mac net.HardwareAddr, vlan int) bool {
	return macEqual(e.mac, mac) && e.static && e.sticky && (vlan == 0 || e.vlan == vlan)
}

// legAddress gives the leg its address, noprefixroute, so that the leg adds
// no route to the namespace's main table. On an uplink bridge DAD runs
// first: an answer keeps the address off; with no carrier on any other port
// of the bridge it cannot run, the address is added, and the next Sync
// probes again.
func (b *hostBackend) legAddress(ctx context.Context, l Leg, rec *legRecord, master, port string) (raised bool, problem string, err error) {
	iface := l.Iface()
	have, err := b.sys.addrs(inNS, iface)
	if err != nil {
		return false, "", fmt.Errorf("reading the addresses of %s: %w", iface, err)
	}
	holds := slices.Contains(have, address{prefix: l.Prefix, noPrefixRoute: true})
	if l.Uplink && (!holds || rec.unprobed) {
		carrier, err := b.carrier(master, port)
		if err != nil {
			return false, "", err
		}
		if !carrier {
			rec.unprobed, problem = true, "no carrier, DAD not run"
		} else {
			claimed, err := b.claimants(ctx, iface, l.Prefix.Addr(), l.MAC)
			if err != nil {
				return false, "", fmt.Errorf("duplicate-address detection of %s on %s: %w", l.Prefix.Addr(), iface, err)
			}
			if len(claimed) > 0 {
				if holds {
					if err := b.sys.delAddr(inNS, iface, l.Prefix); err != nil {
						return false, "", fmt.Errorf("removing %s from %s: %w", l.Prefix, iface, err)
					}
				}
				b.log.Warn().Str("leg", iface).Stringer("addr", l.Prefix.Addr()).Stringer("by", claimed[0]).
					Msg("the address of the leg is in use; it is not raised")
				return false, "address in use by " + claimed[0].String(), nil
			}
			rec.unprobed, rec.dadAt = false, b.now()
		}
	}
	if err := b.ensureAddr(inNS, iface, l.Prefix, true); err != nil {
		return false, "", err
	}
	if !holds {
		b.log.Info().Str("leg", iface).Stringer("addr", l.Prefix).Str("network", l.ID).Msg("raised the address of a leg")
	}
	return true, problem, nil
}

// carrier reports whether a port of the bridge other than pco's own has a
// carrier: without one, nobody could answer a probe.
func (b *hostBackend) carrier(master, own string) (bool, error) {
	all, err := b.sys.links(onHost)
	if err != nil {
		return false, fmt.Errorf("listing the links: %w", err)
	}
	for _, l := range all {
		if l.master == master && l.name != own && !strings.HasPrefix(l.name, bridgePortPrefix) && l.carrier {
			return true, nil
		}
	}
	return false, nil
}

// claimants probes for addr through an interface of the namespace and
// returns the MACs other than own that answered, sorted.
func (b *hostBackend) claimants(ctx context.Context, iface string, addr netip.Addr, own net.HardwareAddr) ([]net.HardwareAddr, error) {
	macs, err := b.sys.arpProbe(ctx, iface, addr, dadWindow)
	if err != nil {
		return nil, err
	}
	macs = slices.DeleteFunc(macs, func(m net.HardwareAddr) bool { return macEqual(m, own) })
	slices.SortFunc(macs, func(a, c net.HardwareAddr) int { return strings.Compare(a.String(), c.String()) })
	return slices.CompactFunc(macs, macEqual), nil
}

// legRoutes keeps the leg's table and its rule: the connected route of the
// guest network from the leg's address, once it has one, and unreachable
// for everything else, so that a packet with the leg's mark leaves through
// the leg or not at all.
func (b *hostBackend) legRoutes(l Leg, raised bool) error {
	if err := b.ensureRoutes(tableBase+l.Mark, legTable(l, raised)); err != nil {
		return err
	}
	return b.ensureRule(legRule(l))
}

// legTable returns the routes of a leg's table: unreachable for everything,
// and the guest network through the leg once the leg holds its address.
func legTable(l Leg, raised bool) []route {
	table := tableBase + l.Mark
	want := []route{{table: table, unreachable: true}}
	if raised {
		want = append(want, route{table: table, dst: l.Prefix.Masked(), dev: l.Iface(), src: l.Prefix.Addr()})
	}
	return want
}

// legRule is the rule that sends what carries the leg's mark to its table.
func legRule(l Leg) rule {
	return rule{pref: ruleBase + l.Mark, mark: l.Mark, table: tableBase + l.Mark}
}

// ensureRoutes makes the routes of a table of the namespace those wanted.
func (b *hostBackend) ensureRoutes(table int, want []route) error {
	have, err := b.sys.routes(inNS, table)
	if err != nil {
		return fmt.Errorf("reading table %d: %w", table, err)
	}
	for _, r := range want {
		if slices.Contains(have, r) {
			continue
		}
		if err := b.sys.addRoute(inNS, r); err != nil {
			return fmt.Errorf("adding the route %s: %w", r, err)
		}
	}
	for _, r := range have {
		// A route of the key of a wanted one was replaced by it.
		if slices.ContainsFunc(want, func(w route) bool { return w.dst == r.dst && w.metric == r.metric }) {
			continue
		}
		if err := b.sys.delRoute(inNS, r); err != nil {
			return fmt.Errorf("removing the route %s: %w", r, err)
		}
	}
	return nil
}

// ensureRule keeps the rule as the only one at its preference.
func (b *hostBackend) ensureRule(want rule) error {
	have, err := b.sys.rules(inNS)
	if err != nil {
		return fmt.Errorf("reading the rules: %w", err)
	}
	found := false
	for _, r := range have {
		switch {
		case r.pref != want.pref:
		case r == want:
			found = true
		default:
			if err := b.sys.delRule(inNS, r); err != nil {
				return fmt.Errorf("removing %s: %w", r, err)
			}
		}
	}
	if found {
		return nil
	}
	if err := b.sys.addRule(inNS, want); err != nil {
		return fmt.Errorf("adding %s: %w", want, err)
	}
	return nil
}

// removeStrayLegs removes the pairs, rules and tables of marks no leg has.
func (b *hostBackend) removeStrayLegs(legs []Leg) error {
	wanted := map[int]bool{}
	for _, l := range legs {
		wanted[l.Mark] = true
	}
	hostLinks, err := b.sys.links(onHost)
	if err != nil {
		return fmt.Errorf("listing the links: %w", err)
	}
	for _, l := range hostLinks {
		if m, ok := markOfPort(l.name); ok && !wanted[m] {
			if err := b.removeLeg(m); err != nil {
				return err
			}
		}
	}
	nsLinks, err := b.sys.links(inNS)
	if err != nil {
		return fmt.Errorf("listing the links in %s: %w", inNS, err)
	}
	for _, l := range nsLinks {
		if m, ok := markOfIface(l.name); ok && !wanted[m] {
			if err := b.removeLeg(m); err != nil {
				return err
			}
		}
	}
	rules, err := b.sys.rules(inNS)
	if err != nil {
		return fmt.Errorf("reading the rules: %w", err)
	}
	for _, r := range rules {
		if m := r.pref - ruleBase; m >= 1 && m <= MaxMark && !wanted[m] {
			if err := b.removeLeg(m); err != nil {
				return err
			}
		}
	}
	routes, err := b.sys.routes(inNS, 0)
	if err != nil {
		return fmt.Errorf("reading the routes: %w", err)
	}
	for _, r := range routes {
		if m := r.table - tableBase; m >= 1 && m <= MaxMark && !wanted[m] {
			if err := b.removeLeg(m); err != nil {
				return err
			}
		}
	}
	return nil
}

// removePair removes the pair of the leg with a mark, which takes the
// neighbours, the address and the routes through the leg with it; the leg's
// rule and the unreachable route of its table stay, so that what carries the
// mark goes nowhere until the leg is made again. An end may be gone already.
func (b *hostBackend) removePair(mark int) error {
	for _, e := range []struct {
		s    side
		name string
	}{{onHost, bridgePort(mark)}, {inNS, LegIface(mark)}} {
		_, ok, err := b.sys.link(e.s, e.name)
		if err != nil {
			return fmt.Errorf("reading %s in %s: %w", e.name, e.s, err)
		}
		if !ok {
			continue
		}
		if err := b.sys.delLink(e.s, e.name); err != nil {
			return fmt.Errorf("removing %s in %s: %w", e.name, e.s, err)
		}
		b.log.Info().Str("link", e.name).Msg("removed a leg")
	}
	return nil
}

// removeLeg removes what the leg with a mark has: its pair, its rule and the
// routes of its table. Each part may be gone already.
func (b *hostBackend) removeLeg(mark int) error {
	if err := b.removePair(mark); err != nil {
		return err
	}
	if err := b.ensureRoutes(tableBase+mark, nil); err != nil {
		return err
	}
	rules, err := b.sys.rules(inNS)
	if err != nil {
		return fmt.Errorf("reading the rules: %w", err)
	}
	for _, r := range rules {
		if r.pref != ruleBase+mark {
			continue
		}
		if err := b.sys.delRule(inNS, r); err != nil {
			return fmt.Errorf("removing %s: %w", r, err)
		}
	}
	return nil
}

// syncPins makes the permanent neighbours of each leg those given: the
// verified MAC of each target, which no answer from a guest can then change.
func (b *hostBackend) syncPins(legs []Leg, pins []Pin) error {
	for _, l := range legs {
		if rec := b.records[l.ID]; rec == nil || !rec.created {
			continue
		}
		var want []neighbour
		for _, p := range pins {
			if p.Leg == l.ID && p.Addr.Is4() && len(p.MAC) == 6 {
				want = append(want, neighbour{addr: p.Addr, mac: p.MAC})
			}
		}
		iface := l.Iface()
		have, err := b.sys.neighbours(iface)
		if err != nil {
			return fmt.Errorf("reading the neighbours of %s: %w", iface, err)
		}
		for _, n := range want {
			if slices.ContainsFunc(have, func(h neighbour) bool { return h.addr == n.addr && macEqual(h.mac, n.mac) }) {
				continue
			}
			if err := b.sys.setNeighbour(iface, n); err != nil {
				return fmt.Errorf("pinning %s at %s on %s: %w", n.addr, n.mac, iface, err)
			}
		}
		for _, h := range have {
			if slices.ContainsFunc(want, func(n neighbour) bool { return n.addr == h.addr }) {
				continue
			}
			if err := b.sys.delNeighbour(iface, h.addr); err != nil {
				return fmt.Errorf("removing the pin of %s on %s: %w", h.addr, iface, err)
			}
		}
	}
	return nil
}

// managedMAC reports whether a MAC is one pco gives the cards of managed
// guests.
func managedMAC(mac net.HardwareAddr) bool {
	return len(mac) == 6 && mac[0] == 0x06 && mac[1] == 0x50 && mac[2] == 0x43 && mac[3] == 0x4f
}

// syncSticky keeps a static sticky entry of each managed guest's MAC on its
// port, where the port exists; an entry whose port is absent is remembered,
// and the next Sync after the port appears adds it. An entry applied before
// and not given now is removed from its port: only those ports are read, as
// a Sync runs at every cycle and on link events. With every, as Down has it,
// the sticky entries of managed MACs that are not given are removed from
// every port of their bridges, those no Sync of this process made among them.
func (b *hostBackend) syncSticky(sticky []Sticky, every bool) error {
	for _, s := range sticky {
		if s.Port == "" || len(s.MAC) != 6 {
			continue
		}
		if _, ok, err := b.sys.link(onHost, s.Port); err != nil {
			return fmt.Errorf("reading %s: %w", s.Port, err)
		} else if !ok {
			continue
		}
		if err := b.ensureFDB(s.Port, s.MAC, s.VLAN); err != nil {
			return err
		}
	}
	ports, err := b.stalePorts(sticky, every)
	if err != nil {
		return err
	}
	for _, port := range ports {
		have, err := b.sys.fdb(port)
		if err != nil {
			return fmt.Errorf("reading the forwarding entries of %s: %w", port, err)
		}
		for _, e := range have {
			if !e.static || !e.sticky || !managedMAC(e.mac) || slices.ContainsFunc(sticky, func(s Sticky) bool {
				return s.Port == port && stickyOf(e, s.MAC, s.VLAN)
			}) {
				continue
			}
			if err := b.sys.delFDB(port, e); err != nil {
				return fmt.Errorf("removing the sticky entry of %s on %s: %w", e.mac, port, err)
			}
		}
	}
	return nil
}

// stalePorts returns the ports, sorted, that may hold a sticky entry of a
// managed MAC that is not given: those of the entries applied before that are
// not given now, or with every, each port of the managed bridge and of the
// bridges of the entries.
func (b *hostBackend) stalePorts(sticky []Sticky, every bool) ([]string, error) {
	var ports []string
	add := func(port string) {
		if port != "" && !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	if !every {
		for _, old := range b.sticky {
			if !slices.ContainsFunc(sticky, func(s Sticky) bool {
				return s.Port == old.Port && macEqual(s.MAC, old.MAC) && s.VLAN == old.VLAN
			}) {
				add(old.Port)
			}
		}
	} else {
		bridges := []string{ManagedBridge}
		for _, s := range slices.Concat(b.sticky, sticky) {
			if !slices.Contains(bridges, s.Bridge) {
				bridges = append(bridges, s.Bridge)
			}
		}
		all, err := b.sys.links(onHost)
		if err != nil {
			return nil, fmt.Errorf("listing the links: %w", err)
		}
		for _, l := range all {
			if slices.Contains(bridges, l.master) {
				add(l.name)
			}
		}
	}
	slices.Sort(ports)
	present := ports[:0]
	for _, port := range ports {
		if _, ok, err := b.sys.link(onHost, port); err != nil {
			return nil, fmt.Errorf("reading %s: %w", port, err)
		} else if ok {
			present = append(present, port)
		}
	}
	return present, nil
}

// Detach removes the leg of a network and its services.
func (b *hostBackend) Detach(ctx context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := slices.IndexFunc(b.legs, func(l Leg) bool { return l.ID == id })
	if i < 0 {
		return nil
	}
	if err := b.namespace(false); err != nil {
		return err
	}
	if err := b.removeLeg(b.legs[i].Mark); err != nil {
		return err
	}
	b.legs = slices.Delete(slices.Clone(b.legs), i, i+1)
	b.services = slices.DeleteFunc(slices.Clone(b.services), func(s Service) bool { return s.Leg == id })
	b.pins = slices.DeleteFunc(slices.Clone(b.pins), func(p Pin) bool { return p.Leg == id })
	delete(b.records, id)
	return b.ensureTable(ctx, true)
}

// Probe probes for addr on the bridge and VLAN through the leg there, or
// through the temporary port vethpcop with probe0 in the namespace, which it
// removes whatever happens.
func (b *hostBackend) Probe(ctx context.Context, bridge string, vlan int, addr netip.Addr) ([]net.HardwareAddr, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.namespace(false); err != nil {
		return nil, err
	}
	for _, l := range b.legs {
		if rec := b.records[l.ID]; l.Bridge == bridge && l.VLAN == vlan && rec != nil && rec.created {
			return b.claimants(ctx, l.Iface(), addr, l.MAC)
		}
	}
	end, problem, err := b.bridgeOf(bridge, vlan)
	switch {
	case err != nil:
		return nil, err
	case problem != "":
		return nil, fmt.Errorf("%s", problem)
	}
	if err := b.ensurePair(probePort, probeBridgeMAC, probeIface, probeMAC, end.mtu); err != nil {
		return nil, err
	}
	defer func() {
		if err := b.sys.delLink(onHost, probePort); err != nil {
			b.log.Warn().Err(err).Str("link", probePort).Msg("the probe port could not be removed")
		}
	}()
	if err := b.ensureSysctls(onHost, [][2]string{{"net.ipv6.conf." + probePort + ".disable_ipv6", "1"}}); err != nil {
		return nil, err
	}
	if err := b.ensureSysctls(inNS, ifaceSysctls(probeIface)); err != nil {
		return nil, err
	}
	if err := b.ensurePort(probePort, end.master, end.vid); err != nil {
		return nil, err
	}
	if err := b.ensureUp(onHost, probePort); err != nil {
		return nil, err
	}
	if err := b.ensureUp(inNS, probeIface); err != nil {
		return nil, err
	}
	return b.claimants(ctx, probeIface, addr, probeMAC)
}
