package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/vishvananda/netns"
)

// ErrNotPinned is the namespace missing where this process cannot pin one:
// the daemon runs in a mount namespace of its own, where a pin would be seen
// by nobody else. pco-netns.service has no stop action, so a restart only runs
// pco netns up again.
var ErrNotPinned = errors.New("the gateway namespace is not pinned; systemctl restart pco-netns")

// hostSysctls are those of the namespace, set before its first leg: it
// forwards between the transit link and the legs, takes no route from what it
// receives, answers ARP only for an address of the interface asked on, and
// has no IPv6.
var hostSysctls = [][2]string{
	{"net.ipv6.conf.all.disable_ipv6", "1"},
	{"net.ipv6.conf.default.disable_ipv6", "1"},
	{"net.ipv6.conf.all.accept_ra", "0"},
	{"net.ipv6.conf.default.accept_ra", "0"},
	{"net.ipv4.conf.all.rp_filter", "0"},
	{"net.ipv4.conf.default.rp_filter", "0"},
	{"net.ipv4.conf.all.arp_ignore", "1"},
	{"net.ipv4.conf.all.arp_announce", "2"},
	{"net.ipv4.conf.all.send_redirects", "0"},
	{"net.ipv4.conf.default.send_redirects", "0"},
	{"net.ipv4.conf.all.accept_redirects", "0"},
	{"net.ipv4.conf.default.accept_redirects", "0"},
	{"net.ipv4.ip_forward", "1"},
}

// dialTimeout bounds a connection through a leg.
const dialTimeout = 2 * time.Second

// hostBackend is the backend of the host profile: the legs live in the
// network namespace pco, which a veth pair links to the host.
type hostBackend struct {
	sys     system
	nsNft   Nft
	hostNft Nft
	log     zerolog.Logger
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error
	form    snatForm

	mu sync.Mutex
	// What Sync last applied, or what the cache says it did before this
	// process; Up loads the table with its elements.
	legs     []Leg
	services []Service
	pins     []Pin
	sticky   []Sticky
	records  map[string]*legRecord // by network id
	// pendingSince is, by port, since when a guest's port is there and not
	// a port of its bridge.
	pendingSince map[string]time.Time
	// erred are the cards whose entry the last placing could not give: the
	// guard admits nothing of theirs until it can.
	erred []Sticky
}

// legRecord is what Sync did with a leg.
type legRecord struct {
	mark    int
	created bool // the pair exists and is set up
	raised  bool // the leg holds its address
	dadAt   time.Time
	// unprobed: the address was added without DAD, for want of a carrier.
	unprobed bool
	// misnamed is the name the appliance's card came up with, when it was
	// not the leg's.
	misnamed string
	// announce: the address was raised and is not announced yet.
	announce bool
}

func newHost(sys system, nsNft, hostNft Nft, log zerolog.Logger, now func() time.Time) *hostBackend {
	return &hostBackend{
		sys: sys, nsNft: nsNft, hostNft: hostNft, log: log, now: now, sleep: sleepCtx, form: theSNATForm,
		records: map[string]*legRecord{},
	}
}

func (b *hostBackend) Netns() netns.NsHandle { return b.sys.handle() }

// Up brings up, in this order, the fallback route of the service prefix,
// the host's table, the namespace with its sysctls and its table, the transit
// link, the route through it, and the sysctl of the managed bridge. It reads each before it changes it, and changes only what differs.
func (b *hostBackend) Up(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.up(ctx)
}

// Both tables are loaded before the transit link is made, so that nothing
// passes it unfiltered, also after a flush of the ruleset; they name the
// interfaces and load without them.
func (b *hostBackend) up(ctx context.Context) error {
	if err := b.ensureRoute(onHost, fallbackRoute()); err != nil {
		return err
	}
	if err := b.ensureHostTable(ctx); err != nil {
		return err
	}
	if err := b.ensureGuard(ctx); err != nil {
		return err
	}
	if err := b.namespace(true); err != nil {
		return err
	}
	if err := b.ensureSysctls(inNS, hostSysctls); err != nil {
		return err
	}
	if err := b.ensureUp(inNS, "lo"); err != nil {
		return err
	}
	if err := b.ensureTable(ctx, false); err != nil {
		return err
	}
	if err := b.transit(); err != nil {
		return err
	}
	if err := b.ensureRoute(onHost, transitRoute()); err != nil {
		return err
	}
	return b.pconetSysctl()
}

// namespace opens the pinned namespace; when there is none and create, it
// makes and pins one, where this process can.
func (b *hostBackend) namespace(create bool) error {
	pinned, err := b.sys.pinned()
	if err != nil {
		return fmt.Errorf("looking for the namespace %s: %w", Namespace, err)
	}
	if !pinned {
		if !create || !b.sys.mayPin() {
			return ErrNotPinned
		}
		if err := b.sys.createNamespace(); err != nil {
			return fmt.Errorf("creating the namespace %s: %w", Namespace, err)
		}
		b.log.Info().Str("namespace", Namespace).Msg("created the gateway namespace")
	}
	if err := b.sys.openNamespace(); err != nil {
		return fmt.Errorf("opening the namespace %s: %w", Namespace, err)
	}
	return nil
}

func (b *hostBackend) ensureSysctls(s side, kv [][2]string) error {
	for _, p := range kv {
		got, err := b.sys.sysctl(s, p[0])
		if err != nil {
			return fmt.Errorf("reading %s in %s: %w", p[0], s, err)
		}
		if got == p[1] {
			continue
		}
		if err := b.sys.setSysctl(s, p[0], p[1]); err != nil {
			return fmt.Errorf("setting %s in %s: %w", p[0], s, err)
		}
	}
	return nil
}

func (b *hostBackend) ensureUp(s side, name string) error {
	l, ok, err := b.sys.link(s, name)
	switch {
	case err != nil:
		return fmt.Errorf("reading %s in %s: %w", name, s, err)
	case !ok:
		return fmt.Errorf("%s in %s is missing", name, s)
	case l.up:
		return nil
	}
	if err := b.sys.setUp(s, name); err != nil {
		return fmt.Errorf("setting %s up in %s: %w", name, s, err)
	}
	return nil
}

// transit makes the veth pair between the host and the namespace with their
// addresses, without IPv6. One end without the other, as after the
// namespace was made anew, is removed and the pair made again.
func (b *hostBackend) transit() error {
	if err := b.ensurePair(hostTransit, nil, nsTransit, nil, 0); err != nil {
		return err
	}
	if err := b.ensureSysctls(onHost, [][2]string{{"net.ipv6.conf." + hostTransit + ".disable_ipv6", "1"}}); err != nil {
		return err
	}
	if err := b.ensureSysctls(inNS, ifaceSysctls(nsTransit)); err != nil {
		return err
	}
	if err := b.ensureAddr(onHost, hostTransit, netip.PrefixFrom(TransitHost, transitBits), false); err != nil {
		return err
	}
	if err := b.ensureAddr(inNS, nsTransit, netip.PrefixFrom(TransitNS, transitBits), false); err != nil {
		return err
	}
	if err := b.ensureUp(onHost, hostTransit); err != nil {
		return err
	}
	return b.ensureUp(inNS, nsTransit)
}

// ifaceSysctls are those of an interface of the namespace.
func ifaceSysctls(name string) [][2]string {
	return [][2]string{
		{"net.ipv6.conf." + name + ".disable_ipv6", "1"},
		{"net.ipv4.conf." + name + ".rp_filter", "0"},
	}
}

// ensurePair makes the pair unless both ends are there as wanted. An end
// whose peer is missing, or with another MAC or type, goes first.
func (b *hostBackend) ensurePair(hostName string, hostMAC net.HardwareAddr, nsName string, nsMAC net.HardwareAddr, mtu int) error {
	h, hok, err := b.sys.link(onHost, hostName)
	if err != nil {
		return fmt.Errorf("reading %s: %w", hostName, err)
	}
	n, nok, err := b.sys.link(inNS, nsName)
	if err != nil {
		return fmt.Errorf("reading %s in %s: %w", nsName, inNS, err)
	}
	intact := hok && nok && h.kind == "veth" && n.kind == "veth" &&
		(hostMAC == nil || macEqual(h.mac, hostMAC)) && (nsMAC == nil || macEqual(n.mac, nsMAC))
	if intact {
		return nil
	}
	if hok {
		if err := b.sys.delLink(onHost, hostName); err != nil {
			return fmt.Errorf("removing %s: %w", hostName, err)
		}
	}
	// The namespace end went with its peer, unless it had none.
	if _, ok, err := b.sys.link(inNS, nsName); err != nil {
		return fmt.Errorf("reading %s in %s: %w", nsName, inNS, err)
	} else if ok {
		if err := b.sys.delLink(inNS, nsName); err != nil {
			return fmt.Errorf("removing %s in %s: %w", nsName, inNS, err)
		}
	}
	if err := b.sys.addVeth(hostName, hostMAC, nsName, nsMAC, mtu); err != nil {
		return fmt.Errorf("creating the pair %s and %s: %w", hostName, nsName, err)
	}
	b.log.Info().Str("link", hostName).Str("peer", nsName).Msg("created a veth pair into the gateway namespace")
	return nil
}

// ensureAddr gives the interface the address and no other IPv4 address.
func (b *hostBackend) ensureAddr(s side, dev string, p netip.Prefix, noPrefixRoute bool) error {
	have, err := b.sys.addrs(s, dev)
	if err != nil {
		return fmt.Errorf("reading the addresses of %s in %s: %w", dev, s, err)
	}
	found := false
	for _, a := range have {
		if a.prefix == p && a.noPrefixRoute == noPrefixRoute {
			found = true
			continue
		}
		if err := b.sys.delAddr(s, dev, a.prefix); err != nil {
			return fmt.Errorf("removing %s from %s in %s: %w", a.prefix, dev, s, err)
		}
	}
	if found {
		return nil
	}
	if err := b.sys.addAddr(s, dev, address{prefix: p, noPrefixRoute: noPrefixRoute}); err != nil {
		return fmt.Errorf("adding %s to %s in %s: %w", p, dev, s, err)
	}
	return nil
}

func transitRoute() route {
	return route{table: mainTable, dst: ServicePrefix, gw: TransitNS, dev: hostTransit, src: TransitHost}
}

func fallbackRoute() route {
	return route{table: mainTable, dst: ServicePrefix, unreachable: true, metric: fallbackMetric}
}

// ensureRoute adds the route unless it is there.
func (b *hostBackend) ensureRoute(s side, r route) error {
	have, err := b.sys.routes(s, r.table)
	if err != nil {
		return fmt.Errorf("reading the routes of table %d in %s: %w", r.table, s, err)
	}
	if slices.Contains(have, r) {
		return nil
	}
	if err := b.sys.addRoute(s, r); err != nil {
		return fmt.Errorf("adding the route %s in %s: %w", r, s, err)
	}
	return nil
}

// ensureTable loads the namespace's table with the elements of what was
// applied, in one transaction, unless it is there as pco loads it. With
// elements it also makes the elements of a table that is there those of what
// was applied; without, as in Up, it leaves them to Sync.
func (b *hostBackend) ensureTable(ctx context.Context, elements bool) error {
	want := inForm(b.form, elementsOf(b.legs, b.services))
	l, err := b.listTable(ctx)
	if err != nil {
		return err
	}
	if l != nil && len(gwDifferences(l, b.form, b.legs)) == 0 {
		if !elements {
			return nil
		}
		script := elementScript(listedElements(l), sortedElements(want))
		if script == "" {
			return nil
		}
		if err := b.nsNft.Apply(ctx, script); err != nil {
			return fmt.Errorf("changing the elements of table inet %s: %w", gwTable, err)
		}
		return nil
	}
	if err := b.nsNft.Apply(ctx, renderGateway(b.form, want, b.legs)); err != nil {
		return fmt.Errorf("loading table inet %s: %w", gwTable, err)
	}
	b.log.Info().Str("table", gwTable).Int("legs", len(b.legs)).Int("services", len(b.services)).
		Msg("loaded the table of the gateway namespace")
	return nil
}

// listTable reads the namespace's table: nil when it is missing or cannot be
// read, which loading it again mends.
func (b *hostBackend) listTable(ctx context.Context) (*listing, error) {
	raw, err := b.nsNft.List(ctx, gwTable)
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

func (b *hostBackend) ensureHostTable(ctx context.Context) error {
	raw, err := b.hostNft.List(ctx, hostTable)
	switch {
	case errors.Is(err, ErrNotLoaded):
	case err != nil:
		return fmt.Errorf("listing table inet %s: %w", hostTable, err)
	default:
		if l, err := parseListing(raw); err == nil && len(hostDifferences(l)) == 0 {
			return nil
		}
	}
	if err := b.hostNft.Apply(ctx, renderHost()); err != nil {
		return fmt.Errorf("loading table inet %s: %w", hostTable, err)
	}
	b.log.Info().Str("table", hostTable).Msg("loaded the host's table of the gateway")
	return nil
}

// pconetSysctl keeps IPv6 off the managed bridge, whose link-local address
// would reach the node's services; the bridge gets it back from ifreload and
// from an apply of the SDN configuration.
func (b *hostBackend) pconetSysctl() error {
	if _, ok, err := b.sys.link(onHost, ManagedBridge); err != nil {
		return fmt.Errorf("reading %s: %w", ManagedBridge, err)
	} else if !ok {
		return nil
	}
	return b.ensureSysctls(onHost, [][2]string{{"net.ipv6.conf." + ManagedBridge + ".disable_ipv6", "1"}})
}

// Dial opens a TCP connection through the leg, from a socket in the
// namespace with the leg's mark and bound to the leg's address.
func (b *hostBackend) Dial(ctx context.Context, leg Leg, addr netip.AddrPort) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	return b.sys.dial(ctx, leg.Mark, leg.Prefix.Addr(), addr)
}

// seed takes what a cache says was applied before this process, for Up to
// load the table with when it has to.
func (b *hostBackend) seed(legs []Leg, services []Service, sticky []Sticky) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.legs, b.services, b.sticky = validLegs(legs), slices.Clone(services), slices.Clone(sticky)
}

// expect takes a cache as what Sync applied: each leg made and holding its
// address.
func (b *hostBackend) expect(legs []Leg, services []Service, sticky []Sticky) {
	b.seed(legs, services, sticky)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, l := range b.legs {
		b.records[l.ID] = &legRecord{mark: l.Mark, created: true, raised: true}
	}
}

func macEqual(a, b net.HardwareAddr) bool { return slices.Equal(a, b) }
