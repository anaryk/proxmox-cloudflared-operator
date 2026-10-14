package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
)

// Verify compares the namespace, the transit link, the host routes, the
// legs, both tables with the elements and the sticky entries with what Up
// and Sync last applied. What cannot be read is an error of its own.
func (b *hostBackend) Verify(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	pinned, err := b.sys.pinned()
	if err != nil {
		return fmt.Errorf("looking for the namespace %s: %w", Namespace, err)
	}
	if !pinned {
		return &ChangedError{Differences: []string{"the namespace " + Namespace + " is not pinned"}}
	}
	if err := b.sys.openNamespace(); err != nil {
		return fmt.Errorf("opening the namespace %s: %w", Namespace, err)
	}
	v := &verifier{b: b}
	v.sysctls(inNS, hostSysctls)
	v.up(inNS, "lo")
	v.transit()
	for _, r := range []route{transitRoute(), fallbackRoute()} {
		v.route(onHost, r)
	}
	for _, l := range b.legs {
		v.leg(l)
	}
	v.strayLegs()
	v.tables(ctx)
	for _, s := range b.sticky {
		v.sticky(s)
	}
	v.guard(ctx)
	if _, ok := v.link(onHost, ManagedBridge, false); ok {
		v.sysctls(onHost, [][2]string{{"net.ipv6.conf." + ManagedBridge + ".disable_ipv6", "1"}})
	}
	if v.err != nil {
		return v.err
	}
	if len(v.d) > 0 {
		return &ChangedError{Differences: v.d}
	}
	return nil
}

// verifier gathers the differences, and the first error of a read, after
// which it reads nothing more.
type verifier struct {
	b   *hostBackend
	d   []string
	err error
}

func (v *verifier) fail(err error) {
	if v.err == nil {
		v.err = err
	}
}

func (v *verifier) differs(format string, args ...any) {
	v.d = append(v.d, fmt.Sprintf(format, args...))
}

func (v *verifier) sysctls(s side, kv [][2]string) {
	for _, p := range kv {
		if v.err != nil {
			return
		}
		got, err := v.b.sys.sysctl(s, p[0])
		switch {
		case err != nil:
			v.fail(fmt.Errorf("reading %s in %s: %w", p[0], s, err))
		case got != p[1]:
			v.differs("%s is %s in %s, want %s", p[0], got, s, p[1])
		}
	}
}

// link returns the link; one that is missing is a difference when it must
// be there.
func (v *verifier) link(s side, name string, must bool) (link, bool) {
	if v.err != nil {
		return link{}, false
	}
	l, ok, err := v.b.sys.link(s, name)
	switch {
	case err != nil:
		v.fail(fmt.Errorf("reading %s in %s: %w", name, s, err))
	case !ok && must:
		v.differs("%s is missing in %s", name, s)
	}
	return l, ok && err == nil
}

func (v *verifier) up(s side, name string) {
	if l, ok := v.link(s, name, true); ok && !l.up {
		v.differs("%s is down in %s", name, s)
	}
}

func (v *verifier) addr(s side, dev string, p netip.Prefix, noPrefixRoute bool) {
	if v.err != nil {
		return
	}
	have, err := v.b.sys.addrs(s, dev)
	switch {
	case err != nil:
		v.fail(fmt.Errorf("reading the addresses of %s in %s: %w", dev, s, err))
	case !slices.Contains(have, address{prefix: p, noPrefixRoute: noPrefixRoute}):
		v.differs("%s has not the address %s in %s", dev, p, s)
	}
}

func (v *verifier) route(s side, r route) {
	if v.err != nil {
		return
	}
	have, err := v.b.sys.routes(s, r.table)
	switch {
	case err != nil:
		v.fail(fmt.Errorf("reading table %d in %s: %w", r.table, s, err))
	case !slices.Contains(have, r):
		v.differs("the route %s is missing in %s", r, s)
	}
}

func (v *verifier) transit() {
	v.up(onHost, hostTransit)
	v.up(inNS, nsTransit)
	v.addr(onHost, hostTransit, netip.PrefixFrom(TransitHost, transitBits), false)
	v.addr(inNS, nsTransit, netip.PrefixFrom(TransitNS, transitBits), false)
	v.sysctls(onHost, [][2]string{{"net.ipv6.conf." + hostTransit + ".disable_ipv6", "1"}})
	v.sysctls(inNS, ifaceSysctls(nsTransit))
}

// strayLegs names the pairs of marks no leg has.
func (v *verifier) strayLegs() {
	marks := map[int]bool{}
	for _, l := range v.b.legs {
		marks[l.Mark] = true
	}
	for _, e := range []struct {
		s    side
		mark func(string) (int, bool)
	}{{onHost, markOfPort}, {inNS, markOfIface}} {
		if v.err != nil {
			return
		}
		links, err := v.b.sys.links(e.s)
		if err != nil {
			v.fail(fmt.Errorf("listing the links in %s: %w", e.s, err))
			return
		}
		for _, l := range links {
			if m, ok := e.mark(l.name); ok && !marks[m] {
				v.differs("%s is the leg of no network", l.name)
			}
		}
	}
}

// leg compares what Sync made of a leg: the pair, its bridge, VLAN and MTU,
// the sticky entry, and once the leg held its address, the address, the
// routes of its table and its rule.
func (v *verifier) leg(l Leg) {
	rec := v.b.records[l.ID]
	if rec == nil || !rec.created {
		return
	}
	port, iface := bridgePort(l.Mark), l.Iface()
	end, problem, err := v.b.bridgeOf(l.Bridge, l.VLAN)
	switch {
	case err != nil:
		v.fail(err)
		return
	case problem != "":
		v.differs("leg %s: %s", iface, problem)
		return
	}
	master, filtering, mtu := end.master, end.filtering, end.mtu
	h, hok := v.link(onHost, port, true)
	n, nok := v.link(inNS, iface, true)
	if !hok || !nok {
		return
	}
	switch {
	case !macEqual(n.mac, l.MAC):
		v.differs("%s has the MAC %s, want %s", iface, n.mac, l.MAC)
	case !macEqual(h.mac, bridgeEndMAC(l.MAC)):
		v.differs("%s has the MAC %s, want %s", port, h.mac, bridgeEndMAC(l.MAC))
	case h.master != master:
		v.differs("%s is enslaved to %q, want %s", port, h.master, master)
	case h.isolated:
		v.differs("%s is an isolated port of %s", port, master)
	case !h.up || !n.up:
		v.differs("leg %s is down", iface)
	case h.mtu != mtu || n.mtu != mtu:
		v.differs("leg %s has the MTU %d and %d, want that of %s, %d", iface, h.mtu, n.mtu, master, mtu)
	}
	v.vlans(port, end.vid)
	v.fdb(port, l.MAC, fdbVLAN(filtering, l.VLAN))
	v.sysctls(onHost, [][2]string{{"net.ipv6.conf." + port + ".disable_ipv6", "1"}})
	v.sysctls(inNS, ifaceSysctls(iface))
	if rec.raised {
		v.addr(inNS, iface, l.Prefix, true)
	}
	v.table(tableBase+l.Mark, legTable(l, rec.raised))
	v.rule(legRule(l))
}

// table compares a leg's table whole: a route more is a difference as a
// route less is.
func (v *verifier) table(table int, want []route) {
	if v.err != nil {
		return
	}
	have, err := v.b.sys.routes(inNS, table)
	if err != nil {
		v.fail(fmt.Errorf("reading table %d in %s: %w", table, inNS, err))
		return
	}
	for _, r := range want {
		if !slices.Contains(have, r) {
			v.differs("the route %s is missing in %s", r, inNS)
		}
	}
	for _, r := range have {
		if !slices.Contains(want, r) {
			v.differs("the route %s is not pco's", r)
		}
	}
}

func (v *verifier) vlans(port string, vid int) {
	if v.err != nil || vid == 0 {
		return
	}
	have, err := v.b.sys.portVLANs(port)
	switch {
	case err != nil:
		v.fail(fmt.Errorf("reading the VLANs of %s: %w", port, err))
	case !slices.Equal(have, []portVLAN{{vid: vid, pvid: true, untagged: true}}):
		v.differs("%s is in the VLANs %v, want only %d, untagged", port, vidsOf(have), vid)
	}
}

func vidsOf(vs []portVLAN) []int {
	out := make([]int, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.vid)
	}
	return out
}

func (v *verifier) fdb(port string, mac []byte, vlan int) {
	if v.err != nil {
		return
	}
	have, err := v.b.sys.fdb(port)
	switch {
	case err != nil:
		v.fail(fmt.Errorf("reading the forwarding entries of %s: %w", port, err))
	case !slices.ContainsFunc(have, func(e fdbEntry) bool { return stickyOf(e, mac, vlan) }):
		v.differs("the sticky entry of %x on %s is missing", mac, port)
	}
}

// rule compares the rules of a leg's mark: the leg's own is there, and no
// other is at its preference or on its mark.
func (v *verifier) rule(want rule) {
	if v.err != nil {
		return
	}
	have, err := v.b.sys.rules(inNS)
	if err != nil {
		v.fail(fmt.Errorf("reading the rules: %w", err))
		return
	}
	if !slices.Contains(have, want) {
		v.differs("%s is missing", want)
	}
	for _, r := range have {
		if r != want && (r.pref == want.pref || r.mark == want.mark) {
			v.differs("%s is not pco's", r)
		}
	}
}

func (v *verifier) sticky(s Sticky) {
	if s.Port == "" {
		return
	}
	l, ok := v.link(onHost, s.Port, false)
	if v.err != nil {
		return
	}
	switch portPlace(l, ok, s.Bridge) {
	case portOnBridge:
		v.fdb(s.Port, s.MAC, s.VLAN)
	case portElsewhere:
		v.differs("%s is a port of %s, not of %s", s.Port, l.master, s.Bridge)
	default:
		if park := v.b.parkingPort(s.Bridge); park != "" {
			v.fdb(park, s.MAC, s.VLAN)
		}
	}
}

// tables compares both tables, the namespace's with its elements.
func (v *verifier) tables(ctx context.Context) {
	if v.err != nil {
		return
	}
	l, err := v.b.listTable(ctx)
	switch {
	case err != nil:
		v.fail(err)
	case l == nil:
		v.differs("table inet %s is missing in %s", gwTable, inNS)
	default:
		v.d = append(v.d, gwDifferences(l, v.b.form, v.b.legs)...)
		want := inForm(v.b.form, elementsOf(v.b.legs, v.b.services))
		v.d = append(v.d, elementDifferences(listedElements(l), sortedElements(want))...)
	}
	raw, err := v.b.hostNft.List(ctx, hostTable)
	switch {
	case errors.Is(err, ErrNotLoaded):
		v.differs("table inet %s is missing", hostTable)
	case err != nil:
		v.fail(fmt.Errorf("listing table inet %s: %w", hostTable, err))
	default:
		hl, err := parseListing(raw)
		if err != nil {
			v.differs("table inet %s cannot be read: %v", hostTable, err)
			return
		}
		v.d = append(v.d, hostDifferences(hl)...)
	}
}

// Down removes the legs, the transit link, both routes of the service prefix,
// the host table, the sticky entries of managed guests and the namespace with
// its table.
func (b *hostBackend) Down(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	pinned, err := b.sys.pinned()
	if err != nil {
		return fmt.Errorf("looking for the namespace %s: %w", Namespace, err)
	}
	if pinned {
		if err := b.sys.openNamespace(); err != nil {
			return fmt.Errorf("opening the namespace %s: %w", Namespace, err)
		}
	}
	all, err := b.sys.links(onHost)
	if err != nil {
		return fmt.Errorf("listing the links: %w", err)
	}
	for _, l := range all {
		_, leg := markOfPort(l.name)
		if !leg && l.name != probePort && l.name != hostTransit {
			continue
		}
		if err := b.sys.delLink(onHost, l.name); err != nil {
			return fmt.Errorf("removing %s: %w", l.name, err)
		}
	}
	routes, err := b.sys.routes(onHost, mainTable)
	if err != nil {
		return fmt.Errorf("reading the routes: %w", err)
	}
	for _, r := range routes {
		if r.dst != ServicePrefix {
			continue
		}
		if err := b.sys.delRoute(onHost, r); err != nil {
			return fmt.Errorf("removing the route %s: %w", r, err)
		}
	}
	if err := b.hostNft.Apply(ctx, fmt.Sprintf("add table inet %[1]s\ndelete table inet %[1]s\n", hostTable)); err != nil {
		return fmt.Errorf("removing table inet %s: %w", hostTable, err)
	}
	if err := b.syncSticky(nil, true); err != nil {
		return err
	}
	if err := b.hostNft.Apply(ctx, fmt.Sprintf("add table bridge %[1]s\ndelete table bridge %[1]s\n", guardTable)); err != nil {
		return fmt.Errorf("removing table bridge %s: %w", guardTable, err)
	}
	// Also without a namespace: an empty file a pin that failed left goes.
	if err := b.sys.removeNamespace(); err != nil {
		return fmt.Errorf("removing the namespace %s: %w", Namespace, err)
	}
	b.legs, b.services, b.pins, b.sticky = nil, nil, nil, nil
	b.records = map[string]*legRecord{}
	b.log.Info().Str("namespace", Namespace).Msg("removed the gateway")
	return nil
}
