package gateway

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// fakeCT is the network of an appliance in memory. Every change is a line of
// calls, in order, beside those of the fake nft and the fake API.
type fakeCT struct {
	n     *fakeNet
	calls []string
	order []string
	// answers are the MACs that answer a probe for an address.
	answers map[netip.Addr][]net.HardwareAddr
}

// newFakeCT is the appliance as it booted: net0, and what pco-net.service
// keeps, with systemd's rp_filter and the kernel's other defaults.
func newFakeCT() *fakeCT {
	f := &fakeCT{n: newFakeNet(), answers: map[netip.Addr][]net.HardwareAddr{}}
	f.n.links["lo"].up = true
	f.n.links["net0"] = &fakeLink{link: link{name: "net0", kind: "veth", mac: mac("bc:24:11:92:50:00"), mtu: 1500, up: true}}
	f.n.links[DummyDevice] = &fakeLink{link: link{name: DummyDevice, kind: "dummy", up: true}}
	f.n.addrs[DummyDevice] = []address{{prefix: baseAddr()}}
	f.n.routes = append(f.n.routes, baseRoute())
	f.n.rules = append(f.n.rules, baseRules()...)
	for _, kv := range containerSysctls {
		f.n.sysctls[kv[0]] = map[bool]string{true: "1", false: "0"}[kv[1] == "0"]
	}
	f.n.sysctls["net.ipv4.conf.all.rp_filter"] = "2"
	return f
}

// card puts a card into the appliance, as Proxmox does.
func (f *fakeCT) card(name string, m net.HardwareAddr) {
	f.n.links[name] = &fakeLink{link: link{name: name, kind: "veth", mac: m, mtu: 1500}}
	for _, k := range []string{"net.ipv6.conf." + name + ".disable_ipv6", "net.ipv4.conf." + name + ".rp_filter",
		"net.ipv4.conf." + name + ".send_redirects", "net.ipv4.conf." + name + ".accept_redirects"} {
		f.n.sysctls[k] = map[bool]string{true: "0", false: "1"}[strings.HasSuffix(k, ".disable_ipv6")]
	}
}

func (f *fakeCT) change(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	f.calls = append(f.calls, line)
	f.order = append(f.order, line)
}

func (f *fakeCT) handle() netns.NsHandle { return netns.None() }

func (f *fakeCT) sysctl(key string) (string, error) {
	v, ok := f.n.sysctls[key]
	if !ok {
		return "", fmt.Errorf("%s: no such file or directory", key)
	}
	return v, nil
}

func (f *fakeCT) setSysctl(key, value string) error {
	f.change("sysctl %s=%s", key, value)
	f.n.sysctls[key] = value
	return nil
}

func (f *fakeCT) link(name string) (link, bool, error) {
	l, ok := f.n.links[name]
	if !ok {
		return link{}, false, nil
	}
	return l.link, true, nil
}

func (f *fakeCT) links() ([]link, error) {
	var out []link
	for _, name := range slices.Sorted(maps.Keys(f.n.links)) {
		out = append(out, f.n.links[name].link)
	}
	return out, nil
}

func (f *fakeCT) addDummy(name string) error {
	f.change("dummy %s", name)
	f.n.links[name] = &fakeLink{link: link{name: name, kind: "dummy"}}
	return nil
}

func (f *fakeCT) rename(name, to string) error {
	f.change("rename %s %s", name, to)
	l := f.n.links[name]
	delete(f.n.links, name)
	l.name = to
	f.n.links[to] = l
	for k, v := range f.n.sysctls {
		if rest, ok := strings.CutSuffix(k, "."+name+"."+lastPart(k)); ok {
			delete(f.n.sysctls, k)
			f.n.sysctls[rest+"."+to+"."+lastPart(k)] = v
		}
	}
	return nil
}

func lastPart(key string) string { return key[strings.LastIndex(key, ".")+1:] }

func (f *fakeCT) setUp(name string) error {
	f.change("up %s", name)
	f.n.links[name].up = true
	return nil
}

func (f *fakeCT) setDown(name string) error {
	f.change("down %s", name)
	f.n.links[name].up = false
	return nil
}

func (f *fakeCT) addrs(dev string) ([]address, error) { return slices.Clone(f.n.addrs[dev]), nil }

func (f *fakeCT) addAddr(dev string, a address) error {
	f.change("addr add %s %s%s", dev, a.prefix, flagIf(a.noPrefixRoute, " noprefixroute"))
	f.n.addrs[dev] = append(f.n.addrs[dev], a)
	return nil
}

func (f *fakeCT) delAddr(dev string, p netip.Prefix) error {
	f.change("addr del %s %s", dev, p)
	f.n.addrs[dev] = slices.DeleteFunc(f.n.addrs[dev], func(a address) bool { return a.prefix == p })
	f.n.routes = slices.DeleteFunc(f.n.routes, func(r route) bool { return r.src == p.Addr() && r.dev == dev })
	return nil
}

func (f *fakeCT) routes(table int) ([]route, error) {
	return slices.DeleteFunc(slices.Clone(f.n.routes), func(r route) bool { return table != 0 && r.table != table }), nil
}

func (f *fakeCT) addRoute(r route) error {
	if r.dev != "" {
		if _, ok := f.n.links[r.dev]; !ok {
			return fmt.Errorf("%s: no such link", r.dev)
		}
	}
	f.change("route add %s", r)
	f.n.routes = append(slices.DeleteFunc(f.n.routes, func(o route) bool {
		return o.table == r.table && o.dst == r.dst && o.metric == r.metric
	}), r)
	return nil
}

func (f *fakeCT) delRoute(r route) error {
	f.change("route del %s", r)
	f.n.routes = slices.DeleteFunc(f.n.routes, func(o route) bool { return o == r })
	return nil
}

func (f *fakeCT) rules() ([]rule, error) { return slices.Clone(f.n.rules), nil }

func (f *fakeCT) addRule(r rule) error {
	f.change("rule add %s", r)
	f.n.rules = append(f.n.rules, r)
	return nil
}

func (f *fakeCT) delRule(r rule) error {
	f.change("rule del %d", r.pref)
	i := slices.IndexFunc(f.n.rules, func(o rule) bool { return o.pref == r.pref })
	if i >= 0 {
		f.n.rules = slices.Delete(f.n.rules, i, i+1)
	}
	return nil
}

func (f *fakeCT) neighbours(dev string) ([]neighbour, error) {
	return slices.Clone(f.n.neigh[dev]), nil
}

func (f *fakeCT) setNeighbour(dev string, n neighbour) error {
	f.change("neigh %s %s %s", dev, n.addr, n.mac)
	f.n.neigh[dev] = append(slices.DeleteFunc(f.n.neigh[dev], func(o neighbour) bool { return o.addr == n.addr }), n)
	return nil
}

func (f *fakeCT) delNeighbour(dev string, a netip.Addr) error {
	f.change("neigh del %s %s", dev, a)
	f.n.neigh[dev] = slices.DeleteFunc(f.n.neigh[dev], func(o neighbour) bool { return o.addr == a })
	return nil
}

func (f *fakeCT) arpProbe(_ context.Context, dev string, a netip.Addr, window time.Duration) ([]net.HardwareAddr, error) {
	if l, ok := f.n.links[dev]; !ok || !l.up {
		return nil, fmt.Errorf("%s is not up", dev)
	}
	f.calls = append(f.calls, fmt.Sprintf("probe %s %s %s", dev, a, window))
	return slices.Clone(f.answers[a]), nil
}

func (f *fakeCT) dial(_ context.Context, mark int, src netip.Addr, dst netip.AddrPort) (net.Conn, error) {
	f.calls = append(f.calls, fmt.Sprintf("dial %d %s %s", mark, src, dst))
	c, s := net.Pipe()
	_ = s.Close()
	return c, nil
}

// changes are the calls that changed something: all but probes and dials.
func (f *fakeCT) changes() []string {
	return slices.DeleteFunc(slices.Clone(f.calls), func(c string) bool {
		return strings.HasPrefix(c, "probe ") || strings.HasPrefix(c, "dial ")
	})
}

// fakeNICs is the Proxmox API of the appliance's own configuration. A card it
// adds appears in the container, as Proxmox's hot-plug makes it.
type fakeNICs struct {
	ct        *fakeCT
	nics      map[int]string
	digest    int
	vlanAware map[string]bool
	// stale makes the next writes fail as a configuration changed meanwhile.
	stale int
	// noLink keeps a card that is added from appearing in the container.
	noLink bool
	fail   error
	calls  []string
}

func newFakeNICs(ct *fakeCT) *fakeNICs {
	return &fakeNICs{
		ct: ct, digest: 1, vlanAware: map[string]bool{"vmbr2": true},
		nics: map[int]string{0: "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:92:50:00,ip=dhcp,type=veth"},
	}
}

func (a *fakeNICs) call(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	a.calls = append(a.calls, line)
	a.ct.order = append(a.ct.order, "api "+line)
}

func (a *fakeNICs) digestText() string { return "d" + strconv.Itoa(a.digest) }

func (a *fakeNICs) NICs(_ context.Context, node string, ref model.GuestRef) (map[int]string, string, error) {
	a.call("read %s %s", node, ref)
	return maps.Clone(a.nics), a.digestText(), nil
}

func (a *fakeNICs) VLANAware(_ context.Context, node, bridge string) (bool, error) {
	a.call("vlan-aware %s %s", node, bridge)
	return a.vlanAware[bridge], nil
}

func (a *fakeNICs) write(digest string) error {
	switch {
	case a.fail != nil:
		return a.fail
	case a.stale > 0:
		a.stale--
		a.digest++
		return fmt.Errorf("changing the config: %w", pve.ErrStaleDigest)
	case digest != a.digestText():
		return errors.New("the test gave an old digest that is not stale")
	}
	a.digest++
	return nil
}

func (a *fakeNICs) AddNIC(_ context.Context, node string, ref model.GuestRef, index int, spec, digest string) error {
	a.call("add %s %s net%d %s %s", node, ref, index, spec, digest)
	if err := a.write(digest); err != nil {
		return err
	}
	a.nics[index] = spec
	if a.noLink {
		return nil
	}
	var name string
	var m net.HardwareAddr
	for part := range strings.SplitSeq(spec, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "name":
			name = v
		case "hwaddr":
			m = mac(v)
		}
	}
	a.ct.card(name, m)
	return nil
}

func (a *fakeNICs) RemoveNIC(_ context.Context, node string, ref model.GuestRef, index int, digest string) error {
	a.call("remove %s %s net%d %s", node, ref, index, digest)
	if err := a.write(digest); err != nil {
		return err
	}
	for name, l := range a.ct.n.links {
		if _, ok := nicWithMAC(map[int]string{index: a.nics[index]}, l.mac); ok {
			delete(a.ct.n.links, name)
		}
	}
	delete(a.nics, index)
	return nil
}

var applianceRef = model.GuestRef{Kind: model.KindLXC, VMID: 9250}

// ctHarness is a container backend on fakes.
type ctHarness struct {
	sys       *fakeCT
	nft       *fakeNft
	api       *fakeNICs
	b         *containerBackend
	announced []string
	now       time.Time
}

func newCTHarness(t *testing.T) *ctHarness {
	h := &ctHarness{sys: newFakeCT(), nft: newFakeNft(t), now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	h.api = newFakeNICs(h.sys)
	h.nft.gwListing = "container-listing-1.1.3.json"
	h.nft.order = &h.sys.order
	h.nft.tables[netTable] = &fakeTable{elems: map[string][]string{}}
	h.b = newContainer(h.sys, h.nft, h.api, applianceRef, "pve1", func(m net.HardwareAddr) {
		h.announced = append(h.announced, m.String())
		h.sys.order = append(h.sys.order, "announce "+m.String())
	}, zerolog.Nop(), func() time.Time { return h.now })
	h.b.sleep = func(context.Context, time.Duration) error { return nil }
	return h
}

// bootPath is the harness without the API, as pco net load runs it.
func (h *ctHarness) bootPath() *ctHarness {
	h.b.api = nil
	return h
}

func (h *ctHarness) reset() {
	h.sys.calls, h.nft.scripts, h.api.calls, h.announced, h.sys.order = nil, nil, nil, nil, nil
}

func (h *ctHarness) up(t *testing.T) {
	t.Helper()
	require.NoError(t, h.b.Up(t.Context()))
	h.reset()
}

func (h *ctHarness) sync(t *testing.T, legs []Leg, services []Service) []LegState {
	t.Helper()
	states, err := h.b.Sync(t.Context(), legs, services, nil, nil)
	require.NoError(t, err)
	return states
}

func TestUpSetsEverySysctlOfTheAppliance(t *testing.T) {
	h := newCTHarness(t)
	require.NoError(t, h.b.Up(t.Context()))
	for _, kv := range containerSysctls {
		require.Equal(t, kv[1], h.sys.n.sysctls[kv[0]], kv[0])
	}
	require.Contains(t, h.sys.calls, "sysctl net.ipv4.conf.all.rp_filter=0", "systemd's rp_filter=2 is undone")
	require.Contains(t, h.sys.calls, "sysctl net.ipv4.conf.all.arp_ignore=1")
	require.Empty(t, Problems(h.b), "pco-net.service left nothing out")

	h.reset()
	require.NoError(t, h.b.Up(t.Context()))
	require.Empty(t, h.sys.changes(), "a second Up changes nothing")
	require.Empty(t, h.nft.scripts)
}

// What pco-net.service keeps and the gateway relies on is put back by Up,
// each part with a line that says so, in the order the kernel takes them.
func TestUpPutsBackWhatPcoNetServiceLeftOut(t *testing.T) {
	h := newCTHarness(t)
	h.sys.n.addrs[DummyDevice] = nil
	h.sys.n.routes = []route{{table: mainTable, dst: ServicePrefix, dev: DummyDevice}}
	h.sys.n.rules = nil
	require.NoError(t, h.b.Up(t.Context()))

	require.Equal(t, []string{
		"pco-net.service left the address 198.18.0.1/32 on pco0 out; added by the gateway",
		"pco-net.service left the route 198.18.0.0/16 dev pco0 src 198.18.0.1 out; added by the gateway",
		"pco-net.service left the rule 1900: from 198.18.0.1 unreachable out; added by the gateway",
		"pco-net.service left the rule 1890: from 198.18.0.1 to 198.18.0.0/16 lookup main out; added by the gateway",
	}, Problems(h.b))
	changes := h.sys.changes()
	require.Less(t, index(t, changes, "addr add pco0 198.18.0.1/32"), index(t, changes, "route add 198.18.0.0/16 dev pco0 src 198.18.0.1"))
	require.Less(t, index(t, changes, "rule add the rule 1900: from 198.18.0.1 unreachable"),
		index(t, changes, "rule add the rule 1890: from 198.18.0.1 to 198.18.0.0/16 lookup main"))
	require.Contains(t, h.sys.n.routes, baseRoute())

	h.reset()
	require.NoError(t, h.b.Up(t.Context()))
	require.Empty(t, Problems(h.b))
	require.Empty(t, h.sys.changes())
}

func TestUpMakesTheDummyDeviceWhenThereIsNone(t *testing.T) {
	h := newCTHarness(t)
	delete(h.sys.n.links, DummyDevice)
	h.sys.n.addrs[DummyDevice] = nil
	h.sys.n.routes = nil
	require.NoError(t, h.b.Up(t.Context()))
	require.Equal(t, []string{"dummy pco0", "up pco0", "addr add pco0 198.18.0.1/32", "route add 198.18.0.0/16 dev pco0 src 198.18.0.1"},
		slices.DeleteFunc(h.sys.changes(), func(c string) bool { return strings.HasPrefix(c, "sysctl ") }))
	require.Contains(t, Problems(h.b), "pco-net.service left the dummy device pco0 out; added by the gateway")

	h = newCTHarness(t)
	h.sys.n.links[DummyDevice].up = false
	require.NoError(t, h.b.Up(t.Context()))
	require.Equal(t, []string{"pco-net.service left the dummy device pco0 down; set up by the gateway"}, Problems(h.b))
	require.True(t, h.sys.n.links[DummyDevice].up)

	h = newCTHarness(t)
	h.sys.n.links[DummyDevice].kind = "veth"
	require.ErrorContains(t, h.b.Up(t.Context()), "pco0 is a device of the type veth, not a dummy one")
}

func TestTheTableOfTheServicePrefixIsLoadedOnlyWithItsScript(t *testing.T) {
	h := newCTHarness(t)
	delete(h.nft.tables, netTable)
	require.NoError(t, h.b.Up(t.Context()))
	require.Equal(t, []string{"pco-net.service left the table inet pco_net out; pco net load puts it back"}, Problems(h.b))
	require.NotContains(t, h.nft.tables, netTable)

	h = newCTHarness(t)
	delete(h.nft.tables, netTable)
	WithNetTable(func() string { return "add table inet pco_net\ndelete table inet pco_net\ntable inet pco_net {\n}\n" })(h.b)
	require.NoError(t, h.b.Up(t.Context()))
	require.Equal(t, []string{"pco-net.service left the table inet pco_net out; added by the gateway"}, Problems(h.b))
	require.Contains(t, h.nft.tables, netTable)
}

func TestUpLoadsTheTableOfTheAppliance(t *testing.T) {
	h := newCTHarness(t)
	require.NoError(t, h.b.Up(t.Context()))
	require.Len(t, h.nft.scripts, 1)
	require.Equal(t, renderContainer(ctElementsOf(nil, nil)), h.nft.scripts[0])
}

// The card of a leg is added through the API: named after the leg, on its
// bridge and tag, with its MAC, without the firewall and an address, at the
// lowest free index, under the digest of the configuration read, the MAC
// announced before the call. Then it is configured in the container.
func TestTheCardOfALegIsAddedAtTheLowestFreeIndex(t *testing.T) {
	h := newCTHarness(t)
	h.api.nics[1] = "name=eth1,bridge=vmbr5,hwaddr=BC:24:11:92:50:01,type=veth" // an admin's
	h.api.nics[3] = "name=eth3,bridge=vmbr6,hwaddr=BC:24:11:92:50:03,type=veth"
	h.up(t)
	states := h.sync(t, []Leg{legA, legB, legM}, testServices)
	for _, s := range states {
		require.True(t, s.Up, "%s: %s", s.Iface, s.Problem)
		require.Empty(t, s.Problem)
	}
	require.Equal(t, []string{
		"read pve1 lxc/9250",
		"add pve1 lxc/9250 net2 name=leg1,bridge=vmbr1,hwaddr=02:50:43:4f:00:01,firewall=0 d1",
		"vlan-aware pve1 vmbr2",
		"read pve1 lxc/9250",
		"add pve1 lxc/9250 net4 name=leg2,bridge=vmbr2,tag=20,hwaddr=02:50:43:4f:00:02,firewall=0 d2",
		"read pve1 lxc/9250",
		"add pve1 lxc/9250 net5 name=leg3,bridge=pconet,hwaddr=02:50:43:4f:ff:01,firewall=0 d3",
	}, h.api.calls)
	require.Equal(t, []string{"02:50:43:4f:00:01", "02:50:43:4f:00:02", "02:50:43:4f:ff:01"}, h.announced)
	for _, m := range h.announced {
		require.Less(t, index(t, h.sys.order, "announce "+m), index(t, h.sys.order, "api add pve1 lxc/9250 net"+map[string]string{
			"02:50:43:4f:00:01": "2", "02:50:43:4f:00:02": "4", "02:50:43:4f:ff:01": "5",
		}[m]), "the MAC is announced before the card is added")
	}
}

// Each leg's card is set up in the container: IPv6 off, no source check or
// redirects, before it goes up; its source guard before its address; the
// address noprefixroute; its table and rule.
func TestALegsCardIsSetUpInTheContainer(t *testing.T) {
	h := newCTHarness(t)
	h.up(t)
	h.sync(t, []Leg{legA, legB}, []Service{svcA, svcB})

	for _, l := range []Leg{legA, legB} {
		iface := l.Iface()
		for _, kv := range cardSysctls(iface) {
			require.Equal(t, kv[1], h.sys.n.sysctls[kv[0]], kv[0])
		}
		require.True(t, h.sys.n.links[iface].up)
		require.Equal(t, []address{{prefix: l.Prefix, noPrefixRoute: true}}, h.sys.n.addrs[iface])
		table := tableBase + l.Mark
		routes, err := h.sys.routes(table)
		require.NoError(t, err)
		require.ElementsMatch(t, []route{
			{table: table, unreachable: true},
			{table: table, dst: prefix("10.50.0.0/24"), dev: iface, src: addr("10.50.0.250")},
		}, routes)
		require.Contains(t, h.sys.n.rules, rule{pref: 1000 + l.Mark, mark: l.Mark, table: table})
		require.Contains(t, h.sys.n.rules, rule{pref: 1900 + l.Mark, src: addr("10.50.0.250"), unreachable: true},
			"one source guard per leg at its own preference, the same address twice")

		changes := h.sys.changes()
		require.Less(t, index(t, changes, "sysctl net.ipv6.conf."+iface+".disable_ipv6=1"), index(t, changes, "up "+iface))
		require.Less(t, index(t, changes, fmt.Sprintf("rule add the rule %d: from 10.50.0.250 unreachable", 1900+l.Mark)),
			index(t, changes, "addr add "+iface+" 10.50.0.250/24 noprefixroute"))
	}
}

// The SNAT of each leg takes the leg's address and its own block of ports;
// the zones, marks and DNAT are those of the namespace's table.
func TestTheTableHoldsEachLegsBlockOfPorts(t *testing.T) {
	h := newCTHarness(t)
	h.up(t)
	h.sync(t, []Leg{legA, legB, legM}, testServices)
	tb := h.nft.tables[gwTable]
	require.Equal(t, []string{snatElement(legA), snatElement(legB), snatElement(legM)}, tb.elems[mapLegSNAT])
	require.Equal(t, sortedElements(ctElementsOf(testLegs, testServices)), sortedElements(tb.elems))
	require.NoError(t, h.b.Verify(t.Context()))
}

// A card that is there with the MAC of a leg is configured without a call to
// the API: after a restart, at boot, or one an admin added by hand.
func TestACardThatIsThereIsConfiguredWithoutTheAPI(t *testing.T) {
	h := newCTHarness(t)
	h.sys.card("leg1", legA.MAC)
	h.up(t)
	states := h.sync(t, []Leg{legA}, []Service{svcA})
	require.True(t, states[0].Up, states[0].Problem)
	require.Empty(t, h.api.calls, "no read and no write")
	require.Empty(t, h.announced)
}

// A card an admin added with pco's MAC under another name is named after its
// leg.
func TestACardWithTheMACOfALegIsNamedAfterIt(t *testing.T) {
	h := newCTHarness(t)
	h.sys.card("eth1", legA.MAC)
	h.sys.n.links["eth1"].up = true
	h.up(t)
	states := h.sync(t, []Leg{legA}, nil)
	require.True(t, states[0].Up, states[0].Problem)
	require.Less(t, index(t, h.sys.changes(), "down eth1"), index(t, h.sys.changes(), "rename eth1 leg1"))
	require.Contains(t, h.sys.n.links, "leg1")
	require.NotContains(t, h.sys.n.links, "eth1")

	h = newCTHarness(t)
	h.sys.card("eth1", legA.MAC)
	h.sys.card("leg1", mac("bc:24:11:00:00:01"))
	h.up(t)
	states = h.sync(t, []Leg{legA}, nil)
	require.False(t, states[0].Up)
	require.Equal(t, "leg1 is another network card of the appliance, with the MAC bc:24:11:00:00:01", states[0].Problem)
}

func TestTheBootPathConfiguresTheCardsThatAreThereAndAddsNone(t *testing.T) {
	h := newCTHarness(t).bootPath()
	h.sys.card("leg1", legA.MAC)
	h.up(t)
	states := h.sync(t, []Leg{legA, legB}, []Service{svcA, svcB})
	require.True(t, states[0].Up, states[0].Problem)
	require.False(t, states[1].Up)
	require.Equal(t, "no network card of the appliance has the MAC 02:50:43:4f:00:02", states[1].Problem)
	require.Empty(t, h.api.calls)
	require.Equal(t, sortedElements(ctElementsOf([]Leg{legA, legB}, []Service{svcA, svcB})), sortedElements(h.nft.tables[gwTable].elems),
		"the service of a leg without its card keeps its elements; the source guard keeps its requests in the appliance")
}

func TestWhatTheApplianceRefusesToAttach(t *testing.T) {
	full := map[int]string{}
	for i := range 10 {
		full[i] = fmt.Sprintf("name=eth%d,bridge=vmbr0,hwaddr=BC:24:11:92:50:%02X,type=veth", i, i)
	}
	for _, tt := range []struct {
		name string
		leg  Leg
		nics map[int]string
		want string
	}{
		{"a bridge Proxmox makes for a VLAN", Leg{ID: "vmbr0v20", Mark: 4, Bridge: "vmbr0v20", Prefix: prefix("10.60.0.250/24"), MAC: mac("02:50:43:4f:00:04")}, nil,
			"bridge vmbr0v20 is one Proxmox makes for a VLAN; attach the network on its bridge with the tag"},
		{"a tag on a bridge that is not VLAN-aware", Leg{ID: "vmbr1.30", Mark: 4, Bridge: "vmbr1", VLAN: 30, Prefix: prefix("10.60.0.250/24"), MAC: mac("02:50:43:4f:00:04")}, nil,
			"bridge vmbr1 is not VLAN-aware; the appliance attaches to VLAN 30 of a VLAN-aware bridge only"},
		{"no free slot", legA, full, "no free NIC slot (net1..net9)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newCTHarness(t)
			if tt.nics != nil {
				h.api.nics = tt.nics
			}
			h.up(t)
			states := h.sync(t, []Leg{tt.leg}, nil)
			require.False(t, states[0].Up)
			require.Equal(t, tt.want, states[0].Problem)
			require.False(t, slices.ContainsFunc(h.api.calls, func(c string) bool { return strings.HasPrefix(c, "add ") }))
			require.Empty(t, h.announced)
		})
	}
}

// A digest that went stale between the read and the write is read again
// once; a second stale one is the leg's problem.
func TestAStaleDigestIsReadAgainOnce(t *testing.T) {
	h := newCTHarness(t)
	h.api.stale = 1
	h.up(t)
	states := h.sync(t, []Leg{legA}, nil)
	require.True(t, states[0].Up, states[0].Problem)
	require.Equal(t, []string{
		"read pve1 lxc/9250",
		"add pve1 lxc/9250 net1 name=leg1,bridge=vmbr1,hwaddr=02:50:43:4f:00:01,firewall=0 d1",
		"read pve1 lxc/9250",
		"add pve1 lxc/9250 net1 name=leg1,bridge=vmbr1,hwaddr=02:50:43:4f:00:01,firewall=0 d2",
	}, h.api.calls)

	h = newCTHarness(t)
	h.api.stale = 2
	h.up(t)
	states = h.sync(t, []Leg{legA}, nil)
	require.False(t, states[0].Up)
	require.Contains(t, states[0].Problem, "adding net1 to lxc/9250 failed: ")
	require.Contains(t, states[0].Problem, "the configuration changed since it was read")
	require.Len(t, h.api.calls, 4)
}

func TestACardThatNeverAppearsIsTheLegsProblem(t *testing.T) {
	h := newCTHarness(t)
	h.api.noLink = true
	h.up(t)
	states := h.sync(t, []Leg{legA}, nil)
	require.False(t, states[0].Up)
	require.Equal(t, "the network card with the MAC 02:50:43:4f:00:01 did not appear in the appliance within 10s", states[0].Problem)

	h.api.calls = nil
	h.api.noLink = false
	h.sys.card("leg1", legA.MAC)
	states = h.sync(t, []Leg{legA}, nil)
	require.True(t, states[0].Up, states[0].Problem)
	require.Empty(t, h.api.calls, "the card the configuration has is not added twice")
}

func TestARefusedWriteIsTheLegsProblem(t *testing.T) {
	h := newCTHarness(t)
	h.api.fail = errors.New("proxmox api: HTTP 403: Permission check failed (/vms/9250, VM.Config.Network)")
	h.up(t)
	states := h.sync(t, []Leg{legA}, nil)
	require.False(t, states[0].Up)
	require.Equal(t, "adding net1 to lxc/9250 failed: proxmox api: HTTP 403: Permission check failed (/vms/9250, VM.Config.Network)", states[0].Problem)
}

func TestASecondSyncChangesNothing(t *testing.T) {
	h := newCTHarness(t)
	h.up(t)
	h.sync(t, []Leg{legA, legB, legM}, testServices)
	h.reset()
	require.NoError(t, h.b.Up(t.Context()))
	h.sync(t, []Leg{legA, legB, legM}, testServices)
	require.Empty(t, h.sys.changes())
	require.Empty(t, h.nft.scripts)
	require.Empty(t, h.api.calls)
}

// Verify compares the sysctls, the base, the legs and the table: a sysctl
// that systemd or an admin changed is a difference like a missing rule.
func TestVerifyNamesWhatDiffersInTheAppliance(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(h *ctHarness)
		want   string
	}{
		{"rp_filter of all", func(h *ctHarness) { h.sys.n.sysctls["net.ipv4.conf.all.rp_filter"] = "2" },
			"net.ipv4.conf.all.rp_filter is 2 in the appliance, want 0"},
		{"arp_ignore", func(h *ctHarness) { h.sys.n.sysctls["net.ipv4.conf.all.arp_ignore"] = "0" },
			"net.ipv4.conf.all.arp_ignore is 0 in the appliance, want 1"},
		{"rp_filter of a leg", func(h *ctHarness) { h.sys.n.sysctls["net.ipv4.conf.leg1.rp_filter"] = "1" },
			"net.ipv4.conf.leg1.rp_filter is 1 in the appliance, want 0"},
		{"IPv6 on a leg", func(h *ctHarness) { h.sys.n.sysctls["net.ipv6.conf.leg2.disable_ipv6"] = "0" },
			"net.ipv6.conf.leg2.disable_ipv6 is 0 in the appliance, want 1"},
		{"redirects of a leg", func(h *ctHarness) { h.sys.n.sysctls["net.ipv4.conf.leg1.accept_redirects"] = "1" },
			"net.ipv4.conf.leg1.accept_redirects is 1 in the appliance, want 0"},
		{"the dummy device down", func(h *ctHarness) { h.sys.n.links[DummyDevice].up = false }, "the dummy device pco0 is missing or down"},
		{"the source of the route", func(h *ctHarness) { h.sys.n.routes[0].src = netip.Addr{} },
			"the route 198.18.0.0/16 dev pco0 src 198.18.0.1 is missing"},
		{"the rule to the prefix", func(h *ctHarness) {
			h.sys.n.rules = slices.DeleteFunc(h.sys.n.rules, func(r rule) bool { return r.pref == PrefToPrefix })
		}, "the rule 1890: from 198.18.0.1 to 198.18.0.0/16 lookup main is missing"},
		{"a source guard", func(h *ctHarness) {
			h.sys.n.rules = slices.DeleteFunc(h.sys.n.rules, func(r rule) bool { return r.pref == 1902 })
		}, "the rule 1902: from 10.50.0.250 unreachable is missing"},
		{"the table of the service prefix", func(h *ctHarness) { delete(h.nft.tables, netTable) }, "table inet pco_net is missing"},
		{"the gateway's table", func(h *ctHarness) { delete(h.nft.tables, gwTable) }, "table inet pco_gw is missing"},
		{"the local guard", func(h *ctHarness) { h.nft.dropRule(gwTable, "out_guard", 0) }, "chain out_guard of table inet pco_gw has 0 rules, want 1"},
		{"an element", func(h *ctHarness) {
			tb := h.nft.tables[gwTable]
			tb.elems[mapSvcDNAT] = tb.elems[mapSvcDNAT][1:]
		}, "svc_dnat of pco_gw lacks 198.18.1.0 . 443 : 10.50.0.10 . 443"},
		{"a leg's address", func(h *ctHarness) { h.sys.n.addrs["leg1"] = nil }, "leg1 has not the address 10.50.0.250/24"},
		{"a leg's route", func(h *ctHarness) {
			h.sys.n.routes = append(h.sys.n.routes, route{table: 101, dst: prefix("10.50.0.0/24"), dev: "leg2"})
		}, "the route 10.50.0.0/24 dev leg2 table 101 is not pco's"},
		{"a card down", func(h *ctHarness) { h.sys.n.links["leg2"].up = false }, "leg leg2 is down"},
		{"a card gone", func(h *ctHarness) { delete(h.sys.n.links, "leg2") }, "the network card of leg leg2, with the MAC 02:50:43:4f:00:02, is missing"},
		{"a card of no leg", func(h *ctHarness) { h.sys.card("leg9", mac("02:50:43:4f:00:09")) }, "leg9 is the network card of no leg"},
		{"a rule of no leg", func(h *ctHarness) {
			h.sys.n.rules = append(h.sys.n.rules, rule{pref: 1905, src: addr("10.9.9.9"), unreachable: true})
		}, "the rule 1905: from 10.9.9.9 unreachable is not pco's"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newCTHarness(t)
			h.up(t)
			h.sync(t, []Leg{legA, legB}, []Service{svcA, svcB})
			require.NoError(t, h.b.Verify(t.Context()))
			tt.change(h)
			err := h.b.Verify(t.Context())
			require.ErrorIs(t, err, ErrChanged)
			var changed *ChangedError
			require.ErrorAs(t, err, &changed)
			require.Contains(t, changed.Differences, tt.want)
		})
	}
}

// Up and Sync mend what Verify found.
func TestUpAndSyncMendWhatVerifyFound(t *testing.T) {
	h := newCTHarness(t)
	h.up(t)
	h.sync(t, []Leg{legA, legB}, []Service{svcA, svcB})
	h.sys.n.sysctls["net.ipv4.conf.all.rp_filter"] = "2"
	h.sys.n.sysctls["net.ipv4.conf.leg1.rp_filter"] = "2"
	h.nft.dropRule(gwTable, "input", 2)
	h.sys.n.rules = slices.DeleteFunc(h.sys.n.rules, func(r rule) bool { return r.pref == 1901 })
	require.ErrorIs(t, h.b.Verify(t.Context()), ErrChanged)
	require.NoError(t, h.b.Up(t.Context()))
	h.sync(t, []Leg{legA, legB}, []Service{svcA, svcB})
	require.NoError(t, h.b.Verify(t.Context()))
}

func TestDADKeepsAnAddressInUseOffAnUplinkLeg(t *testing.T) {
	uplink := legA
	uplink.Uplink = true
	h := newCTHarness(t)
	h.sys.answers[addr("10.50.0.250")] = []net.HardwareAddr{mac("52:54:00:12:34:56")}
	h.up(t)
	states := h.sync(t, []Leg{uplink}, nil)
	require.False(t, states[0].Up)
	require.Equal(t, "address in use by 52:54:00:12:34:56", states[0].Problem)
	require.Empty(t, h.sys.n.addrs["leg1"])
	require.Contains(t, h.sys.n.rules, guardRule(uplink), "the source guard stands all the same")

	delete(h.sys.answers, addr("10.50.0.250"))
	states = h.sync(t, []Leg{uplink}, nil)
	require.True(t, states[0].Up, states[0].Problem)
	require.Equal(t, h.now, states[0].DADAt)
}

// Detach takes the leg's card out of service and away through the API, the
// MAC announced first, and removes its rules, its table and its elements.
func TestDetachRemovesTheCardOfALeg(t *testing.T) {
	h := newCTHarness(t)
	h.up(t)
	h.sync(t, []Leg{legA, legB}, []Service{svcA, svcB})
	h.reset()
	require.NoError(t, h.b.Detach(t.Context(), legA.ID))
	require.Equal(t, []string{"read pve1 lxc/9250", "remove pve1 lxc/9250 net1 d3"}, h.api.calls)
	require.Less(t, index(t, h.sys.order, "announce 02:50:43:4f:00:01"), index(t, h.sys.order, "api remove"))
	require.Less(t, index(t, h.sys.order, "addr del leg1 10.50.0.250/24"), index(t, h.sys.order, "api remove"))
	require.NotContains(t, h.sys.n.links, "leg1")
	for _, r := range h.sys.n.rules {
		require.NotContains(t, []int{1001, 1901}, r.pref)
	}
	routes, err := h.sys.routes(101)
	require.NoError(t, err)
	require.Empty(t, routes)
	require.Equal(t, sortedElements(ctElementsOf([]Leg{legB}, []Service{svcB})), sortedElements(h.nft.tables[gwTable].elems))
	require.NoError(t, h.b.Verify(t.Context()))
}

// A card with a MAC of pco that no leg given has is taken out of service and
// away; one of an admin's is left alone.
func TestSyncRemovesTheCardsOfLegsNoLongerGiven(t *testing.T) {
	h := newCTHarness(t)
	h.up(t)
	h.sync(t, []Leg{legA, legB}, []Service{svcA, svcB})
	h.sys.card("eth5", mac("bc:24:11:92:50:05"))
	h.reset()
	h.sync(t, []Leg{legB}, []Service{svcB})
	require.Contains(t, h.api.calls, "remove pve1 lxc/9250 net1 d3")
	require.Equal(t, []string{"02:50:43:4f:00:01"}, h.announced)
	require.NotContains(t, h.sys.n.links, "leg1")
	require.Contains(t, h.sys.n.links, "eth5")
	for _, r := range h.sys.n.rules {
		require.NotContains(t, []int{1001, 1901}, r.pref)
	}
	require.NoError(t, h.b.Verify(t.Context()))

	h = newCTHarness(t).bootPath()
	h.sys.card("leg1", legA.MAC)
	h.sys.n.addrs["leg1"] = []address{{prefix: legA.Prefix, noPrefixRoute: true}}
	h.up(t)
	h.sync(t, nil, nil)
	require.Empty(t, h.sys.n.addrs["leg1"], "out of service")
	require.Contains(t, h.sys.n.links, "leg1", "the boot path removes no card")
}

// The appliance probes a bridge through its leg there; without one it
// cannot, and says so.
func TestTheApplianceProbesThroughALegOnly(t *testing.T) {
	h := newCTHarness(t)
	h.up(t)
	h.sync(t, []Leg{legA}, nil)
	h.sys.answers[addr("10.50.0.10")] = []net.HardwareAddr{mac("bc:24:11:93:01:00"), legA.MAC}
	macs, err := h.b.Probe(t.Context(), "vmbr1", 0, addr("10.50.0.10"))
	require.NoError(t, err)
	require.Equal(t, []net.HardwareAddr{mac("bc:24:11:93:01:00")}, macs)
	require.Contains(t, h.sys.calls, "probe leg1 10.50.0.10 300ms")

	_, err = h.b.Probe(t.Context(), "vmbr3", 0, addr("10.70.0.10"))
	require.EqualError(t, err, "attach first; the appliance cannot probe a bridge it has no NIC on")
}

func TestTheApplianceDialsThroughALegWithItsMarkAndAddress(t *testing.T) {
	h := newCTHarness(t)
	c, err := h.b.Dial(t.Context(), legB, netip.MustParseAddrPort("10.50.0.10:80"))
	require.NoError(t, err)
	_ = c.Close()
	require.Equal(t, []string{"dial 2 10.50.0.250 10.50.0.10:80"}, h.sys.calls)
	require.Equal(t, netns.None(), h.b.Netns())
}

func TestThePinsOfALegAreItsPermanentNeighbours(t *testing.T) {
	h := newCTHarness(t)
	h.up(t)
	pin := Pin{Leg: legA.ID, Addr: addr("10.50.0.10"), MAC: mac("bc:24:11:93:01:00")}
	_, err := h.b.Sync(t.Context(), []Leg{legA}, []Service{svcA}, []Pin{pin}, nil)
	require.NoError(t, err)
	require.Equal(t, []neighbour{{addr: pin.Addr, mac: pin.MAC}}, h.sys.n.neigh["leg1"])
	_, err = h.b.Sync(t.Context(), []Leg{legA}, []Service{svcA}, nil, nil)
	require.NoError(t, err)
	require.Empty(t, h.sys.n.neigh["leg1"])
}

// A cache restores the appliance's legs at boot without the API: Up loads the
// table with the cached elements in one transaction, Sync configures the
// cards that are there.
func TestACacheRestoresTheAppliancesLegs(t *testing.T) {
	h := newCTHarness(t).bootPath()
	h.sys.card("leg1", legA.MAC)
	h.sys.card("leg2", legB.MAC)
	c := Cache{Legs: []Leg{legA, legB}, Services: []Service{svcA, svcB}}
	states, err := Restore(t.Context(), h.b, c, h.now)
	require.NoError(t, err)
	for _, s := range states {
		require.True(t, s.Up, "%s: %s", s.Iface, s.Problem)
	}
	require.Len(t, h.nft.scripts, 1, "the table and its elements in one transaction")
	require.Equal(t, renderContainer(ctElementsOf([]Leg{legA, legB}, []Service{svcA, svcB})), h.nft.scripts[0])
	require.NoError(t, h.b.Verify(t.Context()))

	fresh := newContainer(h.sys, h.nft, nil, applianceRef, "pve1", nil, zerolog.Nop(), func() time.Time { return h.now })
	Expect(fresh, c)
	require.NoError(t, fresh.Verify(t.Context()), "a process that did not apply it compares with the cache")
}

func TestDownRemovesTheGatewayAndLeavesTheBase(t *testing.T) {
	h := newCTHarness(t)
	h.up(t)
	h.sync(t, []Leg{legA, legB}, []Service{svcA, svcB})
	require.NoError(t, h.b.Down(t.Context()))
	require.NotContains(t, h.nft.tables, gwTable)
	require.Contains(t, h.nft.tables, netTable)
	require.Equal(t, baseRules(), h.sys.n.rules)
	require.Equal(t, []route{baseRoute()}, h.sys.n.routes)
	require.NotContains(t, h.sys.n.links, "leg1")
	require.NotContains(t, h.sys.n.links, "leg2")
}

func TestTheSpecOfALegsCard(t *testing.T) {
	require.Equal(t, "name=leg1,bridge=vmbr1,hwaddr=02:50:43:4f:00:01,firewall=0", nicSpec(legA))
	require.Equal(t, "name=leg2,bridge=vmbr2,tag=20,hwaddr=02:50:43:4f:00:02,firewall=0", nicSpec(legB))
	require.Equal(t, "name=leg3,bridge=pconet,hwaddr=02:50:43:4f:ff:01,firewall=0", nicSpec(legM))
	i, ok := nicWithMAC(map[int]string{0: "name=eth0,hwaddr=BC:24:11:92:50:00", 4: "name=leg2,bridge=vmbr2,hwaddr=02:50:43:4F:00:02"}, legB.MAC)
	require.True(t, ok)
	require.Equal(t, 4, i)
	require.Equal(t, 2, freeSlot(map[int]string{0: "", 1: "", 3: ""}))
}

// snatElement is the element of leg_snat of a leg: its address and its own
// block of ports.
func snatElement(l Leg) string {
	lo, hi := PortBlock(l.Mark)
	return fmt.Sprintf("%q : %s . %d-%d", l.Iface(), l.Prefix.Addr(), lo, hi)
}

// A rule reads as ip rule shows it.
func TestARuleReadsAsIPRuleShowsIt(t *testing.T) {
	require.Equal(t, "the rule 1001: fwmark 1 lookup 101", legRule(legA).String())
	require.Equal(t, "the rule 1901: from 10.50.0.250 unreachable", guardRule(legA).String())
	require.Equal(t, "the rule 1890: from 198.18.0.1 to 198.18.0.0/16 lookup main", baseRules()[1].String())
}
