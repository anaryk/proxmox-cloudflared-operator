package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/vishvananda/netns"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// NICWriter is the slice of the Proxmox API the appliance uses on itself:
// the network cards of its own configuration, whether a bridge of its node
// filters VLANs, and the adding and removing of a card under the digest of
// the configuration it was read with.
type NICWriter interface {
	// NICs returns the network cards of the guest's configuration, the value
	// of net<N> by N, and the digest of the configuration.
	NICs(ctx context.Context, node string, ref model.GuestRef) (map[int]string, string, error)
	// VLANAware reports whether a bridge of the node filters VLANs.
	VLANAware(ctx context.Context, node, bridge string) (bool, error)
	AddNIC(ctx context.Context, node string, ref model.GuestRef, index int, spec string, digest string) error
	RemoveNIC(ctx context.Context, node string, ref model.GuestRef, index int, digest string) error
}

const (
	// DummyDevice takes the service prefix in the appliance: the route of the
	// prefix leads to it, and its address, TransitHost, is the source of
	// every request to the prefix.
	DummyDevice = "pco0"
	// PrefToPrefix and PrefSourceGuard are the preferences of the policy
	// rules on the appliance's sources: a packet from TransitHost to the
	// prefix is looked up in main, which routes it to the dummy device; any
	// other packet from TransitHost goes nowhere, and so does one from a
	// leg's address, by the rule at PrefSourceGuard plus the leg's mark.
	// Without them a packet of an open flow whose DNAT outlived the table of
	// the gateway, or the appliance's own answer to a guest behind a leg,
	// would leave through net0. The rule to the prefix is needed beside the
	// unreachable one: connect() looks the route up again with the source it
	// chose and without a mark.
	PrefToPrefix    = 1890
	PrefSourceGuard = 1900

	// nicSlots is the last index of a card the appliance gives a leg: net1
	// to net9, which the cards an admin added count against.
	nicSlots = 9
	// netTable is the table of the service prefix, which pco-net.service
	// loads.
	netTable = "pco_net"

	// linkWait is how long a card that Proxmox added is waited for in the
	// container, and linkPoll how often it is looked for.
	linkWait = 10 * time.Second
	linkPoll = 100 * time.Millisecond
)

// containerSysctls are those of the appliance, set before its first leg and
// again whenever they differ: it forwards nothing, answers ARP only for an
// address of the interface asked on and from it, so that no leg answers for
// net0's address, takes no route from what it receives, and checks no source
// by its routes, which the tables of the legs and the source guards would
// fail. systemd-sysctl sets rp_filter to 2 at boot.
var containerSysctls = [][2]string{
	{"net.ipv4.ip_forward", "0"},
	{"net.ipv4.conf.all.arp_ignore", "1"},
	{"net.ipv4.conf.all.arp_announce", "2"},
	{"net.ipv4.conf.all.rp_filter", "0"},
	{"net.ipv4.conf.default.rp_filter", "0"},
	{"net.ipv4.conf.all.send_redirects", "0"},
	{"net.ipv4.conf.default.send_redirects", "0"},
	{"net.ipv4.conf.all.accept_redirects", "0"},
	{"net.ipv4.conf.default.accept_redirects", "0"},
}

// cardSysctls are those of a leg's card: no IPv6, no source check, no
// redirects.
func cardSysctls(iface string) [][2]string {
	return [][2]string{
		{"net.ipv6.conf." + iface + ".disable_ipv6", "1"},
		{"net.ipv4.conf." + iface + ".rp_filter", "0"},
		{"net.ipv4.conf." + iface + ".send_redirects", "0"},
		{"net.ipv4.conf." + iface + ".accept_redirects", "0"},
	}
}

// vlanBridge is a bridge Proxmox makes for a tag on a bridge that does not
// filter VLANs.
var vlanBridge = regexp.MustCompile(`^vmbr\d+v\d+$`)

// containerSystem is what the container backend reads and changes of the
// appliance's own network namespace.
type containerSystem interface {
	handle() netns.NsHandle
	sysctl(key string) (string, error)
	setSysctl(key, value string) error
	link(name string) (link, bool, error)
	links() ([]link, error)
	addDummy(name string) error
	rename(name, to string) error
	setUp(name string) error
	setDown(name string) error
	addrs(dev string) ([]address, error)
	addAddr(dev string, a address) error
	delAddr(dev string, p netip.Prefix) error
	routes(table int) ([]route, error)
	addRoute(r route) error
	delRoute(r route) error
	rules() ([]rule, error)
	addRule(r rule) error
	delRule(r rule) error
	neighbours(dev string) ([]neighbour, error)
	setNeighbour(dev string, n neighbour) error
	delNeighbour(dev string, addr netip.Addr) error
	arpProbe(ctx context.Context, dev string, addr netip.Addr, window time.Duration) ([]net.HardwareAddr, error)
	dial(ctx context.Context, mark int, src netip.Addr, dst netip.AddrPort) (net.Conn, error)
}

// ContainerOption sets what NewContainer leaves out by default.
type ContainerOption func(*containerBackend)

// WithNetTable gives the backend the script of the service prefix's table,
// which Up then loads when pco-net.service left it out.
func WithNetTable(script func() string) ContainerOption {
	return func(b *containerBackend) { b.netTable = script }
}

// containerBackend is the backend of the appliance: each leg is a network
// card of the container itself, net1 to net9, which it adds and removes
// through the Proxmox API, and the DNAT runs in the output path of the
// container, where the connectors are.
type containerBackend struct {
	sys      containerSystem
	nft      Nft
	api      NICWriter // nil: no card is added or removed
	self     model.GuestRef
	node     string
	announce func(mac net.HardwareAddr)
	log      zerolog.Logger
	now      func() time.Time
	sleep    func(ctx context.Context, d time.Duration) error
	netTable func() string

	mu sync.Mutex
	// What Sync last applied, or what a cache says it did before this
	// process; Up loads the table with its elements.
	legs     []Leg
	services []Service
	pins     []Pin
	sticky   []Sticky
	records  map[string]*legRecord
	// problems are the lines of what the last Up found missing of what
	// pco-net.service keeps.
	problems []string
}

func newContainer(sys containerSystem, nft Nft, api NICWriter, self model.GuestRef, node string,
	expect func(mac net.HardwareAddr), log zerolog.Logger, now func() time.Time, opts ...ContainerOption,
) *containerBackend {
	b := &containerBackend{
		sys: sys, nft: nft, api: api, self: self, node: node, announce: expect, log: log, now: now,
		sleep: sleepCtx, records: map[string]*legRecord{},
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (b *containerBackend) Netns() netns.NsHandle { return b.sys.handle() }

// Up sets the sysctls of the appliance, puts back what pco-net.service keeps
// and the gateway relies on, each with a line that says so, and loads the
// table of the gateway unless it is there as pco loads it.
func (b *containerBackend) Up(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.up(ctx)
}

func (b *containerBackend) up(ctx context.Context) error {
	b.problems = nil
	if err := b.ensureSysctls(containerSysctls); err != nil {
		return err
	}
	if err := b.ensureBase(ctx); err != nil {
		return err
	}
	return b.ensureTable(ctx, false)
}

func (b *containerBackend) ensureSysctls(kv [][2]string) error {
	for _, p := range kv {
		got, err := b.sys.sysctl(p[0])
		if err != nil {
			return fmt.Errorf("reading %s: %w", p[0], err)
		}
		if got == p[1] {
			continue
		}
		if err := b.sys.setSysctl(p[0], p[1]); err != nil {
			return fmt.Errorf("setting %s: %w", p[0], err)
		}
	}
	return nil
}

func baseAddr() netip.Prefix { return netip.PrefixFrom(TransitHost, TransitHost.BitLen()) }

func baseRoute() route {
	return route{table: mainTable, dst: ServicePrefix, dev: DummyDevice, src: TransitHost}
}

// baseRules are the rules on the source of the requests to the prefix, the
// unreachable one first: a load cut short between the two leaves that source
// going nowhere rather than anywhere main routes it.
func baseRules() []rule {
	return []rule{
		{pref: PrefSourceGuard, src: TransitHost, unreachable: true},
		{pref: PrefToPrefix, src: TransitHost, dst: ServicePrefix, table: mainTable},
	}
}

// ensureBase puts back what of pco-net.service's the gateway relies on: the
// dummy device, its address, the route of the prefix through it with its
// source, the two rules on that source and the table of the prefix.
func (b *containerBackend) ensureBase(ctx context.Context) error {
	l, ok, err := b.sys.link(DummyDevice)
	switch {
	case err != nil:
		return fmt.Errorf("reading %s: %w", DummyDevice, err)
	case !ok:
		if err := b.sys.addDummy(DummyDevice); err != nil {
			return fmt.Errorf("adding the dummy device %s: %w", DummyDevice, err)
		}
		b.leftOut("the dummy device " + DummyDevice)
	case l.kind != "dummy":
		return fmt.Errorf("%s is a device of the type %s, not a dummy one", DummyDevice, l.kind)
	}
	if !ok || !l.up {
		if err := b.sys.setUp(DummyDevice); err != nil {
			return fmt.Errorf("setting %s up: %w", DummyDevice, err)
		}
		if ok {
			line := "pco-net.service left the dummy device " + DummyDevice + " down; set up by the gateway"
			b.problems = append(b.problems, line)
			b.log.Warn().Msg(line)
		}
	}
	have, err := b.sys.addrs(DummyDevice)
	if err != nil {
		return fmt.Errorf("reading the addresses of %s: %w", DummyDevice, err)
	}
	if !slices.ContainsFunc(have, func(a address) bool { return a.prefix == baseAddr() }) {
		if err := b.sys.addAddr(DummyDevice, address{prefix: baseAddr()}); err != nil {
			return fmt.Errorf("adding %s to %s: %w", baseAddr(), DummyDevice, err)
		}
		b.leftOut(fmt.Sprintf("the address %s on %s", baseAddr(), DummyDevice))
	}
	routes, err := b.sys.routes(mainTable)
	if err != nil {
		return fmt.Errorf("reading the routes: %w", err)
	}
	if !slices.Contains(routes, baseRoute()) {
		if err := b.sys.addRoute(baseRoute()); err != nil {
			return fmt.Errorf("adding the route %s: %w", baseRoute(), err)
		}
		b.leftOut("the route " + baseRoute().String())
	}
	rules, err := b.sys.rules()
	if err != nil {
		return fmt.Errorf("reading the rules: %w", err)
	}
	for _, r := range baseRules() {
		if slices.Contains(rules, r) {
			continue
		}
		if err := b.sys.addRule(r); err != nil {
			return fmt.Errorf("adding %s: %w", r, err)
		}
		b.leftOut(r.String())
	}
	return b.ensureNetTable(ctx)
}

// ensureNetTable loads the table of the service prefix when it is missing
// and the backend has its script; without the script it says so.
func (b *containerBackend) ensureNetTable(ctx context.Context) error {
	_, err := b.nft.List(ctx, netTable)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, ErrNotLoaded):
		return fmt.Errorf("listing table inet %s: %w", netTable, err)
	case b.netTable == nil:
		line := "pco-net.service left the table inet " + netTable + " out; pco net load puts it back"
		b.problems = append(b.problems, line)
		b.log.Warn().Msg(line)
		return nil
	}
	if err := b.nft.Apply(ctx, b.netTable()); err != nil {
		return fmt.Errorf("loading table inet %s: %w", netTable, err)
	}
	b.leftOut("the table inet " + netTable)
	return nil
}

// leftOut keeps the line of a part of pco-net.service's that the gateway put
// back.
func (b *containerBackend) leftOut(what string) {
	line := fmt.Sprintf("pco-net.service left %s out; added by the gateway", what)
	b.problems = append(b.problems, line)
	b.log.Warn().Msg(line)
}

func (b *containerBackend) problemLines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.problems)
}

// ensureTable loads the gateway's table with the elements of what was
// applied, in one transaction, unless it is there as pco loads it; with
// elements it also makes the elements of a table that is there those of what
// was applied.
func (b *containerBackend) ensureTable(ctx context.Context, elements bool) error {
	want := ctElementsOf(b.legs, b.services)
	l, err := b.listTable(ctx)
	if err != nil {
		return err
	}
	if l != nil && len(ctDifferences(l)) == 0 {
		if !elements {
			return nil
		}
		script := elementScript(listedElements(l), sortedElements(want))
		if script == "" {
			return nil
		}
		if err := b.nft.Apply(ctx, script); err != nil {
			return fmt.Errorf("changing the elements of table inet %s: %w", gwTable, err)
		}
		return nil
	}
	return b.loadTable(ctx, b.legs, b.services)
}

// loadTable loads the gateway's table with the elements of the legs and
// services, in one transaction, in place of whatever was there.
func (b *containerBackend) loadTable(ctx context.Context, legs []Leg, services []Service) error {
	if err := b.nft.Apply(ctx, renderContainer(ctElementsOf(legs, services))); err != nil {
		return fmt.Errorf("loading table inet %s: %w", gwTable, err)
	}
	b.log.Info().Str("table", gwTable).Int("legs", len(legs)).Int("services", len(services)).
		Msg("loaded the table of the gateway in the appliance")
	return nil
}

func (b *containerBackend) listTable(ctx context.Context) (*listing, error) {
	raw, err := b.nft.List(ctx, gwTable)
	switch {
	case errors.Is(err, ErrNotLoaded):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("listing table inet %s: %w", gwTable, err)
	}
	l, err := parseListing(raw)
	if err != nil {
		b.log.Warn().Err(err).Str("table", gwTable).Msg("the listing of the table cannot be read; loading it again")
		return nil, nil
	}
	return l, nil
}

// Sync makes the legs, the elements of the table and the pins those given.
// A leg's card is found by its MAC and configured as it is; one that is not
// there is added through the Proxmox API when the backend has it. The sticky
// entries are the host's to keep: the appliance has no port of a bridge.
func (b *containerBackend) Sync(ctx context.Context, legs []Leg, services []Service, pins []Pin, sticky []Sticky) ([]LegState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sync(ctx, legs, services, pins, sticky, true)
}

func (b *containerBackend) sync(ctx context.Context, legs []Leg, services []Service, pins []Pin, sticky []Sticky, withPins bool) ([]LegState, error) {
	states := make([]LegState, 0, len(legs))
	valid := validLegs(legs)
	_, refused := validServices(valid, services)
	if err := b.addLegElements(ctx, valid, services); err != nil {
		return nil, err
	}
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
	if err := b.removeStrayLegs(ctx, valid); err != nil {
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
	b.sticky = slices.Clone(sticky)
	return states, nil
}

// addLegElements puts the legs into the sets of the table before any of them
// raises its address: the input chain accepts what comes in on net0, so a
// packet a guest sends to a leg's address must be the leg's from the first
// one, in the leg's zone and dropped there. What changed of a leg is changed
// with it; what is no longer wanted leaves after the legs, with the rest of
// the elements. A table that is missing, or not as pco loads it, is loaded
// with everything wanted, in one transaction.
func (b *containerBackend) addLegElements(ctx context.Context, legs []Leg, services []Service) error {
	l, err := b.listTable(ctx)
	if err != nil {
		return err
	}
	if l == nil || len(ctDifferences(l)) > 0 {
		return b.loadTable(ctx, legs, services)
	}
	have := listedElements(l)
	script := elementScript(have, withElements(have, sortedElements(ctElementsOf(legs, nil))))
	if script == "" {
		return nil
	}
	if err := b.nft.Apply(ctx, script); err != nil {
		return fmt.Errorf("adding the legs to table inet %s: %w", gwTable, err)
	}
	return nil
}

// withElements returns have with the elements of add, an element of add in
// place of one of have with its key.
func withElements(have, add elements) elements {
	out := make(elements, len(have))
	for name, elems := range have {
		out[name] = slices.Clone(elems)
	}
	for name, elems := range add {
		for _, el := range elems {
			k := elementKey(el)
			out[name] = slices.DeleteFunc(out[name], func(o string) bool { return elementKey(o) == k })
			out[name] = append(out[name], el)
		}
		slices.Sort(out[name])
	}
	return out
}

// leg sets up one leg: its card, found by the MAC or added, named leg<mark>,
// without IPv6 and with its sysctls, up; the source guard of its address,
// then the address, after DAD on an uplink bridge; the leg's table and its
// rule.
func (b *containerBackend) leg(ctx context.Context, l Leg) (LegState, error) {
	st := LegState{ID: l.ID, Iface: l.Iface()}
	rec := b.records[l.ID]
	if rec == nil || rec.mark != l.Mark {
		rec = &legRecord{mark: l.Mark}
		b.records[l.ID] = rec
	}
	card, ok, err := b.card(l.MAC)
	if err != nil {
		return st, err
	}
	if !ok {
		if b.api == nil {
			st.Problem = fmt.Sprintf("no network card of the appliance has the MAC %s", l.MAC)
			return st, nil
		}
		if problem := b.attach(ctx, l); problem != "" {
			st.Problem = problem
			return st, nil
		}
		if card, ok, err = b.waitForCard(ctx, l.MAC); err != nil {
			return st, err
		} else if !ok {
			st.Problem = fmt.Sprintf("the network card with the MAC %s did not appear in the appliance within %s", l.MAC, linkWait)
			return st, nil
		}
	}
	iface := l.Iface()
	if problem, err := b.name(card, iface); err != nil || problem != "" {
		st.Problem = problem
		return st, err
	}
	if err := b.ensureSysctls(cardSysctls(iface)); err != nil {
		return st, err
	}
	if err := b.ensureUp(iface); err != nil {
		return st, err
	}
	rec.created, st.MTU = true, card.mtu
	if err := b.ensureRule(guardRule(l)); err != nil {
		return st, err
	}
	raised, problem, err := b.legAddress(ctx, l, rec)
	if err != nil {
		return st, err
	}
	rec.raised, st.Problem, st.DADAt = raised, problem, rec.dadAt
	if err := b.ensureRoutes(tableBase+l.Mark, legTable(l, raised)); err != nil {
		return st, err
	}
	if err := b.ensureRule(legRule(l)); err != nil {
		return st, err
	}
	st.Up = raised
	return st, nil
}

// guardRule is the source guard of a leg: nothing from the leg's address
// leaves but through the leg's table, which the mark picks.
func guardRule(l Leg) rule {
	return rule{pref: PrefSourceGuard + l.Mark, src: l.Prefix.Addr(), unreachable: true}
}

// card returns the card of the container with the MAC.
func (b *containerBackend) card(mac net.HardwareAddr) (link, bool, error) {
	all, err := b.sys.links()
	if err != nil {
		return link{}, false, fmt.Errorf("listing the links: %w", err)
	}
	for _, l := range all {
		if macEqual(l.mac, mac) {
			return l, true, nil
		}
	}
	return link{}, false, nil
}

// waitForCard waits up to linkWait for the card with the MAC to appear in
// the container.
func (b *containerBackend) waitForCard(ctx context.Context, mac net.HardwareAddr) (link, bool, error) {
	polls := int(linkWait / linkPoll)
	for i := 0; ; i++ {
		l, ok, err := b.card(mac)
		if err != nil || ok || i == polls {
			return l, ok, err
		}
		if err := b.sleep(ctx, linkPoll); err != nil {
			return link{}, false, err
		}
	}
}

// name gives the card the name of its leg. Proxmox names a card it adds
// after the name in its configuration, which is that of the leg; a card an
// admin added with the MAC of a leg may have another.
func (b *containerBackend) name(card link, iface string) (string, error) {
	if card.name == iface {
		return "", nil
	}
	other, taken, err := b.sys.link(iface)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", iface, err)
	}
	if taken {
		return fmt.Sprintf("%s is another network card of the appliance, with the MAC %s", iface, other.mac), nil
	}
	if card.up {
		if err := b.sys.setDown(card.name); err != nil {
			return "", fmt.Errorf("setting %s down to rename it: %w", card.name, err)
		}
	}
	if err := b.sys.rename(card.name, iface); err != nil {
		return "", fmt.Errorf("renaming %s to %s: %w", card.name, iface, err)
	}
	b.log.Info().Str("card", card.name).Str("leg", iface).Msg("named a network card of the appliance after its leg")
	return "", nil
}

func (b *containerBackend) ensureUp(name string) error {
	l, ok, err := b.sys.link(name)
	switch {
	case err != nil:
		return fmt.Errorf("reading %s: %w", name, err)
	case !ok:
		return fmt.Errorf("%s is missing", name)
	case l.up:
		return nil
	}
	if err := b.sys.setUp(name); err != nil {
		return fmt.Errorf("setting %s up: %w", name, err)
	}
	return nil
}

// attach adds the card of a leg to the appliance's configuration, at the
// lowest index that is free, under the digest of the configuration it read;
// a digest that went stale meanwhile is read again once. The MAC is
// announced before the call, so that self-identification takes the card for
// the appliance's own. It returns why the card is not added; nothing when it
// was, or when the configuration has it already and its link is awaited.
func (b *containerBackend) attach(ctx context.Context, l Leg) string {
	if vlanBridge.MatchString(l.Bridge) {
		return fmt.Sprintf("bridge %s is one Proxmox makes for a VLAN; attach the network on its bridge with the tag", l.Bridge)
	}
	if l.VLAN != 0 {
		aware, err := b.api.VLANAware(ctx, b.node, l.Bridge)
		switch {
		case err != nil:
			return fmt.Sprintf("whether bridge %s is VLAN-aware cannot be read: %v", l.Bridge, err)
		case !aware:
			return fmt.Sprintf("bridge %s is not VLAN-aware; the appliance attaches to VLAN %d of a VLAN-aware bridge only", l.Bridge, l.VLAN)
		}
	}
	for attempt := 0; ; attempt++ {
		nics, digest, err := b.api.NICs(ctx, b.node, b.self)
		if err != nil {
			return fmt.Sprintf("the network cards of %s cannot be read: %v", b.self, err)
		}
		if _, ok := nicWithMAC(nics, l.MAC); ok {
			return ""
		}
		index := freeSlot(nics)
		if index == 0 {
			return fmt.Sprintf("no free NIC slot (net1..net%d)", nicSlots)
		}
		if b.announce != nil {
			b.announce(l.MAC)
		}
		err = b.api.AddNIC(ctx, b.node, b.self, index, nicSpec(l), digest)
		switch {
		case err == nil:
			b.log.Info().Str("leg", l.Iface()).Int("net", index).Str("bridge", l.Bridge).Int("vlan", l.VLAN).
				Msg("added a network card to the appliance")
			return ""
		case errors.Is(err, pve.ErrStaleDigest) && attempt == 0:
			continue
		}
		return fmt.Sprintf("adding net%d to %s failed: %v", index, b.self, err)
	}
}

// nicSpec is the value of the card of a leg in the appliance's
// configuration: named after the leg, which Proxmox requires and names the
// interface after; on the leg's bridge and tag; with the leg's MAC; without
// the guest firewall, and without an address, which the backend gives it.
func nicSpec(l Leg) string {
	spec := "name=" + l.Iface() + ",bridge=" + l.Bridge
	if l.VLAN != 0 {
		spec += ",tag=" + strconv.Itoa(l.VLAN)
	}
	return spec + ",hwaddr=" + l.MAC.String() + ",firewall=0"
}

// nicWithMAC returns the index of the card of a configuration with the MAC.
func nicWithMAC(nics map[int]string, mac net.HardwareAddr) (int, bool) {
	for index, spec := range nics {
		for part := range strings.SplitSeq(spec, ",") {
			k, v, _ := strings.Cut(part, "=")
			if k != "hwaddr" {
				continue
			}
			if m, err := net.ParseMAC(v); err == nil && macEqual(m, mac) {
				return index, true
			}
		}
	}
	return 0, false
}

// freeSlot returns the lowest index from 1 to nicSlots that no card of the
// configuration has, or 0.
func freeSlot(nics map[int]string) int {
	for i := 1; i <= nicSlots; i++ {
		if _, taken := nics[i]; !taken {
			return i
		}
	}
	return 0
}

// legAddress gives the leg its address, noprefixroute, so that the leg adds
// no route to the main table, where net0's routes are. On an uplink bridge
// DAD runs first, and an answer keeps the address off.
func (b *containerBackend) legAddress(ctx context.Context, l Leg, rec *legRecord) (raised bool, problem string, err error) {
	iface := l.Iface()
	have, err := b.sys.addrs(iface)
	if err != nil {
		return false, "", fmt.Errorf("reading the addresses of %s: %w", iface, err)
	}
	holds := slices.Contains(have, address{prefix: l.Prefix, noPrefixRoute: true})
	if l.Uplink && !holds {
		macs, err := b.sys.arpProbe(ctx, iface, l.Prefix.Addr(), dadWindow)
		if err != nil {
			return false, "", fmt.Errorf("duplicate-address detection of %s on %s: %w", l.Prefix.Addr(), iface, err)
		}
		macs = slices.DeleteFunc(macs, func(m net.HardwareAddr) bool { return macEqual(m, l.MAC) })
		if len(macs) > 0 {
			slices.SortFunc(macs, func(a, c net.HardwareAddr) int { return strings.Compare(a.String(), c.String()) })
			b.log.Warn().Str("leg", iface).Stringer("addr", l.Prefix.Addr()).Stringer("by", macs[0]).
				Msg("the address of the leg is in use; it is not raised")
			for _, a := range have {
				if err := b.sys.delAddr(iface, a.prefix); err != nil {
					return false, "", fmt.Errorf("removing %s from %s: %w", a.prefix, iface, err)
				}
			}
			return false, "address in use by " + macs[0].String(), nil
		}
		rec.dadAt = b.now()
	}
	for _, a := range have {
		if a.prefix == l.Prefix && a.noPrefixRoute {
			continue
		}
		if err := b.sys.delAddr(iface, a.prefix); err != nil {
			return false, "", fmt.Errorf("removing %s from %s: %w", a.prefix, iface, err)
		}
	}
	if !holds {
		if err := b.sys.addAddr(iface, address{prefix: l.Prefix, noPrefixRoute: true}); err != nil {
			return false, "", fmt.Errorf("adding %s to %s: %w", l.Prefix, iface, err)
		}
		b.log.Info().Str("leg", iface).Stringer("addr", l.Prefix).Str("network", l.ID).Msg("raised the address of a leg")
	}
	return true, "", nil
}

// ensureRoutes makes the routes of a table those wanted.
func (b *containerBackend) ensureRoutes(table int, want []route) error {
	have, err := b.sys.routes(table)
	if err != nil {
		return fmt.Errorf("reading table %d: %w", table, err)
	}
	for _, r := range want {
		if slices.Contains(have, r) {
			continue
		}
		if err := b.sys.addRoute(r); err != nil {
			return fmt.Errorf("adding the route %s: %w", r, err)
		}
	}
	for _, r := range have {
		if slices.ContainsFunc(want, func(w route) bool { return w.dst == r.dst && w.metric == r.metric }) {
			continue
		}
		if err := b.sys.delRoute(r); err != nil {
			return fmt.Errorf("removing the route %s: %w", r, err)
		}
	}
	return nil
}

// ensureRule keeps the rule as the only one at its preference.
func (b *containerBackend) ensureRule(want rule) error {
	have, err := b.sys.rules()
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
			if err := b.sys.delRule(r); err != nil {
				return fmt.Errorf("removing %s: %w", r, err)
			}
		}
	}
	if found {
		return nil
	}
	if err := b.sys.addRule(want); err != nil {
		return fmt.Errorf("adding %s: %w", want, err)
	}
	return nil
}

// legMarkOf returns the mark of a leg's rule or table: the leg rule at
// 1000+mark, the source guard at PrefSourceGuard+mark, table 100+mark.
func legMarkOf(n, base int) (int, bool) {
	m := n - base
	return m, m >= 1 && m <= MaxMark
}

// pcoCard reports whether a card has a MAC pco gives the cards of legs.
func pcoCard(mac net.HardwareAddr) bool {
	return len(mac) == 6 && mac[0] == 0x02 && mac[1] == 0x50 && mac[2] == 0x43 && mac[3] == 0x4f
}

// removeStrayLegs takes the cards with a MAC of pco that no leg given has
// out of service, and away through the API when the backend has it, and
// removes the rules and tables of marks no leg has.
func (b *containerBackend) removeStrayLegs(ctx context.Context, legs []Leg) error {
	marks := map[int]bool{}
	for _, l := range legs {
		marks[l.Mark] = true
	}
	all, err := b.sys.links()
	if err != nil {
		return fmt.Errorf("listing the links: %w", err)
	}
	for _, c := range all {
		if !pcoCard(c.mac) || slices.ContainsFunc(legs, func(l Leg) bool { return macEqual(l.MAC, c.mac) }) {
			continue
		}
		if err := b.dropCard(c.name); err != nil {
			return err
		}
		if b.api != nil {
			if err := b.removeNIC(ctx, c.mac); err != nil {
				b.log.Warn().Err(err).Str("card", c.name).Msg("the network card of a leg no network has could not be removed")
			}
		}
	}
	return b.removeRoutingOf(func(mark int) bool { return !marks[mark] })
}

// dropCard takes a card out of service: its addresses go, so that nothing is
// sent from them.
func (b *containerBackend) dropCard(name string) error {
	have, err := b.sys.addrs(name)
	if err != nil {
		return fmt.Errorf("reading the addresses of %s: %w", name, err)
	}
	for _, a := range have {
		if err := b.sys.delAddr(name, a.prefix); err != nil {
			return fmt.Errorf("removing %s from %s: %w", a.prefix, name, err)
		}
	}
	b.log.Info().Str("card", name).Msg("took the network card of a leg out of service")
	return nil
}

// removeRoutingOf removes the rules and the routes of the tables of the marks
// stray says are no leg's.
func (b *containerBackend) removeRoutingOf(stray func(mark int) bool) error {
	rules, err := b.sys.rules()
	if err != nil {
		return fmt.Errorf("reading the rules: %w", err)
	}
	for _, r := range rules {
		m, leg := legMarkOf(r.pref, ruleBase)
		g, guard := legMarkOf(r.pref, PrefSourceGuard)
		if (leg && stray(m)) || (guard && stray(g)) {
			if err := b.sys.delRule(r); err != nil {
				return fmt.Errorf("removing %s: %w", r, err)
			}
		}
	}
	routes, err := b.sys.routes(0)
	if err != nil {
		return fmt.Errorf("reading the routes: %w", err)
	}
	for _, r := range routes {
		if m, ok := legMarkOf(r.table, tableBase); ok && stray(m) {
			if err := b.sys.delRoute(r); err != nil {
				return fmt.Errorf("removing the route %s: %w", r, err)
			}
		}
	}
	return nil
}

// removeNIC removes the card with the MAC from the appliance's configuration,
// under the digest it read, once more on a digest that went stale meanwhile.
// The MAC is announced before the call.
func (b *containerBackend) removeNIC(ctx context.Context, mac net.HardwareAddr) error {
	for attempt := 0; ; attempt++ {
		nics, digest, err := b.api.NICs(ctx, b.node, b.self)
		if err != nil {
			return fmt.Errorf("reading the network cards of %s: %w", b.self, err)
		}
		index, ok := nicWithMAC(nics, mac)
		if !ok {
			return nil
		}
		if b.announce != nil {
			b.announce(mac)
		}
		err = b.api.RemoveNIC(ctx, b.node, b.self, index, digest)
		switch {
		case err == nil:
			b.log.Info().Int("net", index).Str("mac", mac.String()).Msg("removed a network card of the appliance")
			return nil
		case errors.Is(err, pve.ErrStaleDigest) && attempt == 0:
			continue
		}
		return fmt.Errorf("removing net%d from %s: %w", index, b.self, err)
	}
}

// syncPins makes the permanent neighbours of each leg those given.
func (b *containerBackend) syncPins(legs []Leg, pins []Pin) error {
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

// Detach removes the leg of a network and its services: its card is taken
// out of service and, with the API, away; its rules and table go.
func (b *containerBackend) Detach(ctx context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := slices.IndexFunc(b.legs, func(l Leg) bool { return l.ID == id })
	if i < 0 {
		return nil
	}
	l := b.legs[i]
	card, ok, err := b.card(l.MAC)
	if err != nil {
		return err
	}
	if ok {
		if err := b.dropCard(card.name); err != nil {
			return err
		}
	}
	if b.api != nil {
		if err := b.removeNIC(ctx, l.MAC); err != nil {
			return err
		}
	}
	if err := b.removeRoutingOf(func(mark int) bool { return mark == l.Mark }); err != nil {
		return err
	}
	b.legs = slices.Delete(slices.Clone(b.legs), i, i+1)
	b.services = slices.DeleteFunc(slices.Clone(b.services), func(s Service) bool { return s.Leg == id })
	b.pins = slices.DeleteFunc(slices.Clone(b.pins), func(p Pin) bool { return p.Leg == id })
	delete(b.records, id)
	return b.ensureTable(ctx, true)
}

// Verify compares the sysctls of the appliance, what of pco-net.service's
// the gateway relies on, the cards of the legs with their sysctls, addresses,
// tables and rules, and the gateway's table with its elements with what Up
// and Sync last applied. What cannot be read is an error of its own.
func (b *containerBackend) Verify(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := &ctVerifier{b: b}
	v.sysctls(containerSysctls)
	v.base(ctx)
	for _, l := range b.legs {
		v.leg(l)
	}
	v.strays()
	v.table(ctx)
	if v.err != nil {
		return v.err
	}
	if len(v.d) > 0 {
		return &ChangedError{Differences: v.d}
	}
	return nil
}

// ctVerifier gathers the differences, and the first error of a read, after
// which it reads nothing more.
type ctVerifier struct {
	b   *containerBackend
	d   []string
	err error
}

func (v *ctVerifier) fail(err error) {
	if v.err == nil {
		v.err = err
	}
}

func (v *ctVerifier) differs(format string, args ...any) {
	v.d = append(v.d, fmt.Sprintf(format, args...))
}

func (v *ctVerifier) sysctls(kv [][2]string) {
	for _, p := range kv {
		if v.err != nil {
			return
		}
		got, err := v.b.sys.sysctl(p[0])
		switch {
		case err != nil:
			v.fail(fmt.Errorf("reading %s: %w", p[0], err))
		case got != p[1]:
			v.differs("%s is %s in the appliance, want %s", p[0], got, p[1])
		}
	}
}

func (v *ctVerifier) base(ctx context.Context) {
	if v.err != nil {
		return
	}
	l, ok, err := v.b.sys.link(DummyDevice)
	switch {
	case err != nil:
		v.fail(fmt.Errorf("reading %s: %w", DummyDevice, err))
		return
	case !ok || l.kind != "dummy" || !l.up:
		v.differs("the dummy device %s is missing or down", DummyDevice)
	default:
		v.addr(DummyDevice, address{prefix: baseAddr()}, func(a address) bool { return a.prefix == baseAddr() })
	}
	v.routes(mainTable, []route{baseRoute()}, false)
	v.rules(baseRules()...)
	if v.err != nil {
		return
	}
	switch _, err := v.b.nft.List(ctx, netTable); {
	case errors.Is(err, ErrNotLoaded):
		v.differs("table inet %s is missing", netTable)
	case err != nil:
		v.fail(fmt.Errorf("listing table inet %s: %w", netTable, err))
	}
}

func (v *ctVerifier) addr(dev string, want address, match func(address) bool) {
	if v.err != nil {
		return
	}
	have, err := v.b.sys.addrs(dev)
	switch {
	case err != nil:
		v.fail(fmt.Errorf("reading the addresses of %s: %w", dev, err))
	case !slices.ContainsFunc(have, match):
		v.differs("%s has not the address %s", dev, want.prefix)
	}
}

// routes compares the routes of a table; whole, a route more is a difference
// as a route less is.
func (v *ctVerifier) routes(table int, want []route, whole bool) {
	if v.err != nil {
		return
	}
	have, err := v.b.sys.routes(table)
	if err != nil {
		v.fail(fmt.Errorf("reading table %d: %w", table, err))
		return
	}
	for _, r := range want {
		if !slices.Contains(have, r) {
			v.differs("the route %s is missing", r)
		}
	}
	if !whole {
		return
	}
	for _, r := range have {
		if !slices.Contains(want, r) {
			v.differs("the route %s is not pco's", r)
		}
	}
}

// rules compares rules: each is there, and no other is at its preference or,
// for a rule on a mark, on its mark.
func (v *ctVerifier) rules(want ...rule) {
	if v.err != nil {
		return
	}
	have, err := v.b.sys.rules()
	if err != nil {
		v.fail(fmt.Errorf("reading the rules: %w", err))
		return
	}
	for _, w := range want {
		if !slices.Contains(have, w) {
			v.differs("%s is missing", w)
		}
		for _, r := range have {
			if r != w && (r.pref == w.pref || (w.mark > 0 && r.mark == w.mark)) {
				v.differs("%s is not pco's", r)
			}
		}
	}
}

// leg compares what Sync made of a leg: its card, named after the leg and up,
// its sysctls, and once it held its address, the address, the routes of its
// table, its rule and its source guard.
func (v *ctVerifier) leg(l Leg) {
	rec := v.b.records[l.ID]
	if v.err != nil || rec == nil || !rec.created {
		return
	}
	iface := l.Iface()
	card, ok, err := v.b.card(l.MAC)
	switch {
	case err != nil:
		v.fail(err)
		return
	case !ok:
		v.differs("the network card of leg %s, with the MAC %s, is missing", iface, l.MAC)
		return
	case card.name != iface:
		v.differs("the network card with the MAC %s is %s, want %s", l.MAC, card.name, iface)
		return
	case !card.up:
		v.differs("leg %s is down", iface)
	}
	v.sysctls(cardSysctls(iface))
	if rec.raised {
		want := address{prefix: l.Prefix, noPrefixRoute: true}
		v.addr(iface, want, func(a address) bool { return a == want })
	}
	v.routes(tableBase+l.Mark, legTable(l, rec.raised), true)
	v.rules(legRule(l), guardRule(l))
}

// strays names the cards of pco that no leg has, and the rules and routes of
// marks no leg has.
func (v *ctVerifier) strays() {
	if v.err != nil {
		return
	}
	marks := map[int]bool{}
	for _, l := range v.b.legs {
		marks[l.Mark] = true
	}
	all, err := v.b.sys.links()
	if err != nil {
		v.fail(fmt.Errorf("listing the links: %w", err))
		return
	}
	for _, c := range all {
		if pcoCard(c.mac) && !slices.ContainsFunc(v.b.legs, func(l Leg) bool { return macEqual(l.MAC, c.mac) }) {
			v.differs("%s is the network card of no leg", c.name)
		}
	}
	rules, err := v.b.sys.rules()
	if err != nil {
		v.fail(fmt.Errorf("reading the rules: %w", err))
		return
	}
	for _, r := range rules {
		m, leg := legMarkOf(r.pref, ruleBase)
		g, guard := legMarkOf(r.pref, PrefSourceGuard)
		if (leg && !marks[m]) || (guard && !marks[g]) {
			v.differs("%s is not pco's", r)
		}
	}
	routes, err := v.b.sys.routes(0)
	if err != nil {
		v.fail(fmt.Errorf("reading the routes: %w", err))
		return
	}
	for _, r := range routes {
		if m, ok := legMarkOf(r.table, tableBase); ok && !marks[m] {
			v.differs("the route %s is not pco's", r)
		}
	}
}

func (v *ctVerifier) table(ctx context.Context) {
	if v.err != nil {
		return
	}
	l, err := v.b.listTable(ctx)
	switch {
	case err != nil:
		v.fail(err)
	case l == nil:
		v.differs("table inet %s is missing", gwTable)
	default:
		v.d = append(v.d, ctDifferences(l)...)
		v.d = append(v.d, elementDifferences(listedElements(l), sortedElements(ctElementsOf(v.b.legs, v.b.services)))...)
	}
}

// Probe probes for addr through the leg on the bridge and VLAN. The
// appliance has no port of a bridge it has no card on, so a network is
// probed once it is attached, and DAD runs when the leg is raised.
func (b *containerBackend) Probe(ctx context.Context, bridge string, vlan int, addr netip.Addr) ([]net.HardwareAddr, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, l := range b.legs {
		if rec := b.records[l.ID]; l.Bridge == bridge && l.VLAN == vlan && rec != nil && rec.created {
			macs, err := b.sys.arpProbe(ctx, l.Iface(), addr, dadWindow)
			if err != nil {
				return nil, err
			}
			macs = slices.DeleteFunc(macs, func(m net.HardwareAddr) bool { return macEqual(m, l.MAC) })
			slices.SortFunc(macs, func(a, c net.HardwareAddr) int { return strings.Compare(a.String(), c.String()) })
			return slices.CompactFunc(macs, macEqual), nil
		}
	}
	return nil, errors.New("attach first; the appliance cannot probe a bridge it has no NIC on")
}

// Dial opens a TCP connection through the leg, from a socket of the
// appliance with the leg's mark and bound to the leg's address.
func (b *containerBackend) Dial(ctx context.Context, leg Leg, addr netip.AddrPort) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	return b.sys.dial(ctx, leg.Mark, leg.Prefix.Addr(), addr)
}

// Down removes what Up and Sync made of the gateway: the cards of the legs
// go out of service and, with the API, away, their rules and tables and the
// gateway's table go. What pco-net.service keeps and the sysctls stay.
func (b *containerBackend) Down(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	all, err := b.sys.links()
	if err != nil {
		return fmt.Errorf("listing the links: %w", err)
	}
	for _, c := range all {
		if !pcoCard(c.mac) {
			continue
		}
		if err := b.dropCard(c.name); err != nil {
			return err
		}
		if b.api != nil {
			if err := b.removeNIC(ctx, c.mac); err != nil {
				return err
			}
		}
	}
	if err := b.removeRoutingOf(func(int) bool { return true }); err != nil {
		return err
	}
	if err := b.nft.Apply(ctx, fmt.Sprintf("add table inet %[1]s\ndelete table inet %[1]s\n", gwTable)); err != nil {
		return fmt.Errorf("removing table inet %s: %w", gwTable, err)
	}
	b.legs, b.services, b.pins, b.sticky = nil, nil, nil, nil
	b.records = map[string]*legRecord{}
	b.log.Info().Msg("removed the gateway of the appliance")
	return nil
}

func (b *containerBackend) seed(legs []Leg, services []Service, sticky []Sticky) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.legs, b.services, b.sticky = validLegs(legs), slices.Clone(services), slices.Clone(sticky)
}

func (b *containerBackend) expect(legs []Leg, services []Service, sticky []Sticky) {
	b.seed(legs, services, sticky)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, l := range b.legs {
		b.records[l.ID] = &legRecord{mark: l.Mark, created: true, raised: true}
	}
}

func (b *containerBackend) syncKeepingPins(ctx context.Context, legs []Leg, services []Service, sticky []Sticky) ([]LegState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sync(ctx, legs, services, nil, sticky, false)
}

// reporter is a backend that keeps lines of what it found and mended.
type reporter interface {
	problemLines() []string
}

// Problems returns the lines of what the backend's last Up found missing and
// put back, or could not: the appliance's parts of pco-net.service; none on
// the host.
func Problems(b Backend) []string {
	if r, ok := b.(reporter); ok {
		return r.problemLines()
	}
	return nil
}

// ProxmoxNICs is the NICWriter of a Proxmox API client.
func ProxmoxNICs(c *pve.Client) NICWriter { return proxmoxNICs{c: c} }

type proxmoxNICs struct{ c *pve.Client }

func (p proxmoxNICs) NICs(ctx context.Context, node string, ref model.GuestRef) (map[int]string, string, error) {
	cfg, err := p.c.GuestConfig(ctx, node, ref)
	if err != nil {
		return nil, "", err
	}
	nics := map[int]string{}
	for key, value := range cfg.Values {
		rest, ok := strings.CutPrefix(key, "net")
		if !ok {
			continue
		}
		if i, err := strconv.Atoi(rest); err == nil && strconv.Itoa(i) == rest {
			nics[i] = value
		}
	}
	return nics, cfg.Digest, nil
}

func (p proxmoxNICs) VLANAware(ctx context.Context, node, bridge string) (bool, error) {
	ifaces, err := p.c.NodeNetwork(ctx, node)
	if err != nil {
		return false, err
	}
	for _, i := range ifaces {
		if i.Name == bridge {
			return i.VLANAware, nil
		}
	}
	return false, fmt.Errorf("node %s has no bridge %s", node, bridge)
}

func (p proxmoxNICs) AddNIC(ctx context.Context, node string, ref model.GuestRef, index int, spec, digest string) error {
	return p.c.SetGuestConfig(ctx, node, ref, map[string]string{"net" + strconv.Itoa(index): spec}, digest)
}

func (p proxmoxNICs) RemoveNIC(ctx context.Context, node string, ref model.GuestRef, index int, digest string) error {
	return p.c.DeleteGuestConfigKeys(ctx, node, ref, []string{"net" + strconv.Itoa(index)}, digest)
}
