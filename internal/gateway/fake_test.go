package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"
)

var update = flag.Bool("update", false, "write the golden files of the tests")

func requireGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), got, "the output changed; run the test with -update when that is intended")
}

func mac(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return m
}

func prefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

// fakeNet is one network namespace of the fake.
type fakeNet struct {
	links   map[string]*fakeLink
	sysctls map[string]string
	addrs   map[string][]address
	routes  []route
	rules   []rule
	neigh   map[string][]neighbour
	// learned are the neighbours the kernel learned: not permanent, so
	// neighbours does not list them, and only a new device drops them.
	learned map[string][]neighbour
}

type fakeLink struct {
	link
	peer  string // of a veth, on the other side
	vlans []portVLAN
	fdb   []fdbEntry
}

func newFakeNet() *fakeNet {
	n := &fakeNet{
		links: map[string]*fakeLink{}, sysctls: map[string]string{},
		addrs: map[string][]address{}, neigh: map[string][]neighbour{}, learned: map[string][]neighbour{},
	}
	n.links["lo"] = &fakeLink{link: link{name: "lo", kind: "loopback", mtu: 65536}}
	return n
}

// fakeSystem is the node's network in memory. Every change is a line of
// calls, in order; probes and dials are too.
type fakeSystem struct {
	host, ns *fakeNet
	canPin   bool
	calls    []string
	// order is every change in order, those of the fake nft among them.
	order []string
	// answers are the MACs that answer a probe for an address.
	answers map[netip.Addr][]net.HardwareAddr
	// fail makes the change whose line starts with a key fail.
	fail map[string]error
	// fdbReads counts the reads of the forwarding entries of each port.
	fdbReads map[string]int
	// leftoverPin is an empty file where the pin would be, left by a pin
	// that failed: no namespace, and not pinned.
	leftoverPin bool
}

func newFakeSystem() *fakeSystem {
	f := &fakeSystem{host: newFakeNet(), canPin: true, answers: map[netip.Addr][]net.HardwareAddr{}, fail: map[string]error{}}
	f.host.links["lo"].up = true
	return f
}

// bridge adds a bridge to the host, with ports that have a carrier.
func (f *fakeSystem) bridge(name string, filtering bool, mtu int, ports ...string) {
	f.host.links[name] = &fakeLink{link: link{name: name, kind: "bridge", mtu: mtu, up: true, carrier: true, vlanFiltering: filtering}}
	if filtering {
		f.host.links[name].defaultPVID = 1
	}
	for _, p := range ports {
		f.host.links[p] = &fakeLink{link: link{name: p, kind: "device", mtu: mtu, up: true, carrier: true, master: name}}
	}
}

func (f *fakeSystem) net(s side) *fakeNet {
	if s == onHost {
		return f.host
	}
	return f.ns
}

func (f *fakeSystem) change(format string, args ...any) error {
	line := fmt.Sprintf(format, args...)
	for k, err := range f.fail {
		if strings.HasPrefix(line, k) {
			return err
		}
	}
	f.calls = append(f.calls, line)
	f.order = append(f.order, line)
	return nil
}

// changes are the calls that changed something: all but probes and dials.
func (f *fakeSystem) changes() []string {
	return slices.DeleteFunc(slices.Clone(f.calls), func(c string) bool {
		return strings.HasPrefix(c, "probe ") || strings.HasPrefix(c, "dial ")
	})
}

func (f *fakeSystem) pinned() (bool, error) { return f.ns != nil, nil }
func (f *fakeSystem) mayPin() bool          { return f.canPin }

func (f *fakeSystem) createNamespace() error {
	if err := f.change("create namespace"); err != nil {
		return err
	}
	f.ns = newFakeNet()
	return nil
}

func (f *fakeSystem) openNamespace() error {
	if f.ns == nil {
		return errors.New("no namespace")
	}
	return nil
}

func (f *fakeSystem) removeNamespace() error {
	if f.ns == nil {
		if !f.leftoverPin {
			return nil
		}
		f.leftoverPin = false
		return f.change("remove pin")
	}
	if err := f.change("remove namespace"); err != nil {
		return err
	}
	for name, l := range f.ns.links {
		if l.peer != "" {
			f.dropLink(onHost, l.peer)
		}
		delete(f.ns.links, name)
	}
	f.ns = nil
	return nil
}

func (f *fakeSystem) handle() netns.NsHandle { return netns.None() }

// sysctl reads a key; an interface's key that was never set is the
// namespace's default, as the kernel gives a new interface.
func (f *fakeSystem) sysctl(s side, key string) (string, error) {
	n := f.net(s)
	if n == nil {
		return "", errors.New("no namespace")
	}
	if v, ok := n.sysctls[key]; ok {
		return v, nil
	}
	parts := strings.Split(key, ".")
	if len(parts) == 5 && parts[3] != "all" && parts[3] != "default" {
		if _, ok := n.links[parts[3]]; !ok {
			return "", fmt.Errorf("%s: no such interface", key)
		}
		parts[3] = "default"
		if v, ok := n.sysctls[strings.Join(parts, ".")]; ok {
			return v, nil
		}
	}
	return "0", nil
}

func (f *fakeSystem) setSysctl(s side, key, value string) error {
	if err := f.change("sysctl %s %s=%s", sideName(s), key, value); err != nil {
		return err
	}
	f.net(s).sysctls[key] = value
	return nil
}

func sideName(s side) string {
	if s == onHost {
		return "host"
	}
	return "ns"
}

func (f *fakeSystem) link(s side, name string) (link, bool, error) {
	n := f.net(s)
	if n == nil {
		return link{}, false, errors.New("no namespace")
	}
	l, ok := n.links[name]
	if !ok {
		return link{}, false, nil
	}
	return l.link, true, nil
}

func (f *fakeSystem) links(s side) ([]link, error) {
	n := f.net(s)
	if n == nil {
		return nil, errors.New("no namespace")
	}
	var out []link
	for _, name := range slices.Sorted(maps.Keys(n.links)) {
		out = append(out, n.links[name].link)
	}
	return out, nil
}

func (f *fakeSystem) addVeth(hostName string, hostMAC net.HardwareAddr, nsName string, nsMAC net.HardwareAddr, mtu int) error {
	if f.ns == nil {
		return errors.New("no namespace")
	}
	if _, ok := f.host.links[hostName]; ok {
		return fmt.Errorf("%s exists", hostName)
	}
	if _, ok := f.ns.links[nsName]; ok {
		return fmt.Errorf("%s exists in the namespace", nsName)
	}
	if err := f.change("veth %s %s %s %s mtu %d", hostName, macText(hostMAC), nsName, macText(nsMAC), mtu); err != nil {
		return err
	}
	if mtu == 0 {
		mtu = 1500
	}
	if hostMAC == nil {
		hostMAC = mac("aa:00:00:00:00:01")
	}
	if nsMAC == nil {
		nsMAC = mac("aa:00:00:00:00:02")
	}
	f.host.links[hostName] = &fakeLink{link: link{name: hostName, kind: "veth", mac: hostMAC, mtu: mtu}, peer: nsName}
	f.ns.links[nsName] = &fakeLink{link: link{name: nsName, kind: "veth", mac: nsMAC, mtu: mtu}, peer: hostName}
	return nil
}

// macText is a MAC of a call; the kernel's choice for none.
func macText(m net.HardwareAddr) string {
	if m == nil {
		return "auto"
	}
	return m.String()
}

func (f *fakeSystem) delLink(s side, name string) error {
	n := f.net(s)
	l, ok := n.links[name]
	if !ok {
		return fmt.Errorf("%s: no such link", name)
	}
	if err := f.change("del %s %s", sideName(s), name); err != nil {
		return err
	}
	f.dropLink(s, name)
	if l.peer != "" {
		other := onHost
		if s == onHost {
			other = inNS
		}
		if f.net(other) != nil {
			f.dropLink(other, l.peer)
		}
	}
	return nil
}

// dropLink takes a link and all that hangs on it out of a namespace.
func (f *fakeSystem) dropLink(s side, name string) {
	n := f.net(s)
	delete(n.links, name)
	delete(n.addrs, name)
	delete(n.neigh, name)
	delete(n.learned, name)
	n.routes = slices.DeleteFunc(n.routes, func(r route) bool { return r.dev == name })
	for k := range n.sysctls {
		if strings.Contains(k, "."+name+".") {
			delete(n.sysctls, k)
		}
	}
	for _, l := range n.links {
		if l.master == name {
			l.master, l.vlans = "", nil
		}
	}
}

func (f *fakeSystem) setMTU(s side, name string, mtu int) error {
	if err := f.change("mtu %s %s %d", sideName(s), name, mtu); err != nil {
		return err
	}
	f.net(s).links[name].mtu = mtu
	return nil
}

func (f *fakeSystem) setMaster(name, master string) error {
	br, ok := f.host.links[master]
	if !ok || br.kind != "bridge" {
		return fmt.Errorf("%s is no bridge", master)
	}
	if err := f.change("master %s %s", name, master); err != nil {
		return err
	}
	l := f.host.links[name]
	l.master, l.vlans, l.fdb = master, nil, nil
	if br.vlanFiltering && br.defaultPVID > 0 {
		l.vlans = []portVLAN{{vid: br.defaultPVID, pvid: true, untagged: true}}
	}
	return nil
}

func (f *fakeSystem) setUp(s side, name string) error {
	if err := f.change("up %s %s", sideName(s), name); err != nil {
		return err
	}
	f.net(s).links[name].up = true
	f.net(s).links[name].carrier = true
	return nil
}

func (f *fakeSystem) setIsolated(name string, isolated bool) error {
	if err := f.change("isolated %s %t", name, isolated); err != nil {
		return err
	}
	f.host.links[name].isolated = isolated
	return nil
}

func (f *fakeSystem) portVLANs(port string) ([]portVLAN, error) {
	return slices.Clone(f.host.links[port].vlans), nil
}

func (f *fakeSystem) addPortVLAN(port string, v portVLAN) error {
	if err := f.change("vlan add %s %d", port, v.vid); err != nil {
		return err
	}
	l := f.host.links[port]
	if v.pvid {
		for i := range l.vlans {
			l.vlans[i].pvid = false
		}
	}
	l.vlans = append(slices.DeleteFunc(l.vlans, func(o portVLAN) bool { return o.vid == v.vid }), v)
	return nil
}

func (f *fakeSystem) delPortVLAN(port string, vid int) error {
	if err := f.change("vlan del %s %d", port, vid); err != nil {
		return err
	}
	l := f.host.links[port]
	l.vlans = slices.DeleteFunc(l.vlans, func(o portVLAN) bool { return o.vid == vid })
	return nil
}

func (f *fakeSystem) fdb(port string) ([]fdbEntry, error) {
	l, ok := f.host.links[port]
	if !ok {
		return nil, fmt.Errorf("%s: no such link", port)
	}
	if f.fdbReads != nil {
		f.fdbReads[port]++
	}
	return slices.Clone(l.fdb), nil
}

func (f *fakeSystem) addSticky(port string, m net.HardwareAddr, vlan int) error {
	if err := f.change("sticky %s %s vlan %d", port, m, vlan); err != nil {
		return err
	}
	l := f.host.links[port]
	l.fdb = append(slices.DeleteFunc(l.fdb, func(e fdbEntry) bool { return macEqual(e.mac, m) && e.vlan == vlan }),
		fdbEntry{mac: m, vlan: vlan, static: true, sticky: true})
	return nil
}

func (f *fakeSystem) delFDB(port string, e fdbEntry) error {
	if err := f.change("fdb del %s %s vlan %d", port, e.mac, e.vlan); err != nil {
		return err
	}
	l := f.host.links[port]
	l.fdb = slices.DeleteFunc(l.fdb, func(o fdbEntry) bool { return macEqual(o.mac, e.mac) && o.vlan == e.vlan })
	return nil
}

func (f *fakeSystem) addrs(s side, dev string) ([]address, error) {
	n := f.net(s)
	if _, ok := n.links[dev]; !ok {
		return nil, fmt.Errorf("%s: no such link", dev)
	}
	return slices.Clone(n.addrs[dev]), nil
}

func (f *fakeSystem) addAddr(s side, dev string, a address) error {
	if err := f.change("addr add %s %s %s%s", sideName(s), dev, a.prefix, flagIf(a.noPrefixRoute, " noprefixroute")); err != nil {
		return err
	}
	n := f.net(s)
	n.addrs[dev] = append(n.addrs[dev], a)
	if !a.noPrefixRoute {
		n.routes = append(n.routes, route{table: mainTable, dst: a.prefix.Masked(), dev: dev, src: a.prefix.Addr()})
	}
	return nil
}

func flagIf(on bool, flag string) string {
	if on {
		return flag
	}
	return ""
}

func (f *fakeSystem) delAddr(s side, dev string, p netip.Prefix) error {
	if err := f.change("addr del %s %s %s", sideName(s), dev, p); err != nil {
		return err
	}
	n := f.net(s)
	n.addrs[dev] = slices.DeleteFunc(n.addrs[dev], func(a address) bool { return a.prefix == p })
	n.routes = slices.DeleteFunc(n.routes, func(r route) bool { return r.src == p.Addr() && r.dev == dev })
	return nil
}

func (f *fakeSystem) routes(s side, table int) ([]route, error) {
	n := f.net(s)
	if n == nil {
		return nil, errors.New("no namespace")
	}
	return slices.DeleteFunc(slices.Clone(n.routes), func(r route) bool { return table != 0 && r.table != table }), nil
}

func (f *fakeSystem) addRoute(s side, r route) error {
	n := f.net(s)
	if r.dev != "" {
		if _, ok := n.links[r.dev]; !ok {
			return fmt.Errorf("%s: no such link", r.dev)
		}
	}
	if err := f.change("route add %s %s", sideName(s), r); err != nil {
		return err
	}
	n.routes = append(slices.DeleteFunc(n.routes, func(o route) bool {
		return o.table == r.table && o.dst == r.dst && o.metric == r.metric
	}), r)
	return nil
}

func (f *fakeSystem) delRoute(s side, r route) error {
	if err := f.change("route del %s %s", sideName(s), r); err != nil {
		return err
	}
	n := f.net(s)
	n.routes = slices.DeleteFunc(n.routes, func(o route) bool { return o == r })
	return nil
}

func (f *fakeSystem) rules(s side) ([]rule, error) { return slices.Clone(f.net(s).rules), nil }

func (f *fakeSystem) addRule(s side, r rule) error {
	if err := f.change("rule add %s %d fwmark %d table %d", sideName(s), r.pref, r.mark, r.table); err != nil {
		return err
	}
	f.net(s).rules = append(f.net(s).rules, r)
	return nil
}

func (f *fakeSystem) delRule(s side, r rule) error {
	if err := f.change("rule del %s %d", sideName(s), r.pref); err != nil {
		return err
	}
	f.net(s).rules = slices.DeleteFunc(f.net(s).rules, func(o rule) bool { return o == r })
	return nil
}

func (f *fakeSystem) neighbours(dev string) ([]neighbour, error) {
	return slices.Clone(f.ns.neigh[dev]), nil
}

func (f *fakeSystem) setNeighbour(dev string, n neighbour) error {
	if err := f.change("neigh %s %s %s", dev, n.addr, n.mac); err != nil {
		return err
	}
	f.ns.neigh[dev] = append(slices.DeleteFunc(f.ns.neigh[dev], func(o neighbour) bool { return o.addr == n.addr }), n)
	return nil
}

func (f *fakeSystem) delNeighbour(dev string, a netip.Addr) error {
	if err := f.change("neigh del %s %s", dev, a); err != nil {
		return err
	}
	f.ns.neigh[dev] = slices.DeleteFunc(f.ns.neigh[dev], func(o neighbour) bool { return o.addr == a })
	return nil
}

func (f *fakeSystem) arpProbe(_ context.Context, dev string, a netip.Addr, window time.Duration) ([]net.HardwareAddr, error) {
	if l, ok := f.ns.links[dev]; !ok || !l.up {
		return nil, fmt.Errorf("%s is not up", dev)
	}
	f.calls = append(f.calls, fmt.Sprintf("probe %s %s %s", dev, a, window))
	return slices.Clone(f.answers[a]), nil
}

func (f *fakeSystem) dial(_ context.Context, mark int, src netip.Addr, dst netip.AddrPort) (net.Conn, error) {
	f.calls = append(f.calls, fmt.Sprintf("dial %d %s %s", mark, src, dst))
	c, s := net.Pipe()
	_ = s.Close()
	return c, nil
}

// fakeNft holds tables as nft would after the scripts it was given, and
// lists them in the JSON of nft 1.1.3, from the listings of real nft in
// testdata with the elements put in.
type fakeNft struct {
	t       *testing.T
	order   *[]string // where its transactions go among the changes of the system
	tables  map[string]*fakeTable
	scripts []string
	fail    error
	// gwListing is the listing in testdata that table pco_gw lists as; the
	// host's when empty.
	gwListing string
}

type fakeTable struct {
	base  []map[string]json.RawMessage // the listing nft printed for the table without elements
	elems map[string][]string
}

func newFakeNft(t *testing.T) *fakeNft { return &fakeNft{t: t, tables: map[string]*fakeTable{}} }

// baseListing returns the entries of a listing in testdata, for a table
// without elements.
func baseListing(t *testing.T, name string) []map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	for _, e := range doc.Nftables {
		for kind, body := range e {
			if kind != "set" && kind != "map" {
				continue
			}
			var m map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(body, &m))
			delete(m, "elem")
			e[kind], err = json.Marshal(m)
			require.NoError(t, err)
		}
	}
	return doc.Nftables
}

func (n *fakeNft) Apply(_ context.Context, script string) error {
	if n.fail != nil {
		return n.fail
	}
	next := map[string]*fakeTable{}
	for name, tb := range n.tables {
		next[name] = &fakeTable{base: tb.base, elems: maps.Clone(tb.elems)}
	}
	lines := strings.Split(script, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		f := strings.Fields(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "add table inet "):
			if next[f[3]] == nil {
				next[f[3]] = &fakeTable{elems: map[string][]string{}}
			}
		case strings.HasPrefix(line, "delete table inet "):
			if next[f[3]] == nil {
				return fmt.Errorf("delete table inet %s: No such file or directory", f[3])
			}
			delete(next, f[3])
		case strings.HasPrefix(line, "table inet "):
			tb, end := n.parseTable(f[2], lines[i+1:])
			next[f[2]] = tb
			i += end + 1
		case strings.HasPrefix(line, "add element inet ") || strings.HasPrefix(line, "delete element inet "):
			if err := elementLine(next, line); err != nil {
				return err
			}
		default:
			return fmt.Errorf("the fake cannot read %q", line)
		}
	}
	n.tables = next
	n.scripts = append(n.scripts, script)
	if n.order != nil {
		first, _, _ := strings.Cut(strings.TrimPrefix(script, "add table inet "), "\n")
		*n.order = append(*n.order, "nft "+first)
	}
	return nil
}

// parseTable reads the block of a table up to its closing brace and returns
// it with the base listing of its name, and the index of the closing line.
func (n *fakeNft) parseTable(name string, lines []string) (*fakeTable, int) {
	tb := &fakeTable{elems: map[string][]string{}}
	switch name {
	case gwTable:
		tb.base = baseListing(n.t, cmp.Or(n.gwListing, "listing-1.1.3.json"))
	case hostTable:
		tb.base = baseListing(n.t, "host-1.1.3.json")
	case netTable:
		// Only whether it is there is read.
	default:
		n.t.Fatalf("the fake has no listing of table %s", name)
	}
	var set string
	inElements := false
	for i, line := range lines {
		l := strings.TrimSpace(line)
		switch {
		case line == "}":
			return tb, i
		case strings.HasPrefix(l, "set ") || strings.HasPrefix(l, "map "):
			set = strings.Fields(l)[1]
		case l == "elements = {":
			inElements = true
		case inElements && l == "}":
			inElements = false
		case inElements:
			tb.elems[set] = append(tb.elems[set], strings.TrimSuffix(l, ","))
		}
	}
	n.t.Fatalf("table %s has no end", name)
	return nil, 0
}

func elementLine(tables map[string]*fakeTable, line string) error {
	f := strings.Fields(line)
	verb, table, set := f[0], f[3], f[4]
	tb := tables[table]
	if tb == nil {
		return fmt.Errorf("%s: No such file or directory", line)
	}
	_, body, _ := strings.Cut(line, "{ ")
	body = strings.TrimSuffix(body, " }")
	for _, el := range strings.Split(body, ", ") {
		k := elementKey(el)
		i := slices.IndexFunc(tb.elems[set], func(o string) bool { return elementKey(o) == k })
		switch {
		case verb == "add" && i >= 0 && tb.elems[set][i] != el:
			return fmt.Errorf("%s: File exists", line)
		case verb == "add" && i < 0:
			tb.elems[set] = append(tb.elems[set], el)
		case verb == "delete" && i < 0:
			return fmt.Errorf("%s: No such file or directory", line)
		case verb == "delete":
			tb.elems[set] = slices.Delete(tb.elems[set], i, i+1)
		}
	}
	return nil
}

func (n *fakeNft) List(_ context.Context, table string) ([]byte, error) {
	tb := n.tables[table]
	if tb == nil {
		return nil, fmt.Errorf("table inet %s: %w", table, ErrNotLoaded)
	}
	var out []map[string]json.RawMessage
	for _, e := range tb.base {
		entry := maps.Clone(e)
		for kind, body := range e {
			if kind != "set" && kind != "map" {
				continue
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(body, &m); err != nil {
				return nil, err
			}
			var name string
			_ = json.Unmarshal(m["name"], &name)
			if elems := tb.elems[name]; len(elems) > 0 {
				var list []any
				for _, el := range elems {
					list = append(list, elementJSON(el, kind == "map"))
				}
				m["elem"], _ = json.Marshal(list)
			}
			entry[kind], _ = json.Marshal(m)
		}
		out = append(out, entry)
	}
	return json.Marshal(map[string]any{"nftables": out})
}

// elementJSON writes an element of a script as nft -j lists it.
func elementJSON(el string, isMap bool) any {
	if isMap {
		k, v, _ := strings.Cut(el, " : ")
		return []any{partJSON(k), partJSON(v)}
	}
	return partJSON(el)
}

func partJSON(s string) any {
	parts := strings.Split(s, " . ")
	if len(parts) == 1 {
		return atomJSON(s)
	}
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		out = append(out, atomJSON(p))
	}
	return map[string]any{"concat": out}
}

func atomJSON(s string) any {
	if unq, err := strconv.Unquote(s); err == nil {
		return unq
	}
	if lo, hi, ok := strings.Cut(s, "-"); ok {
		a, _ := strconv.Atoi(lo)
		b, _ := strconv.Atoi(hi)
		return map[string]any{"range": []int{a, b}}
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return s
}

// dropRule removes a rule of a chain from the base listing of a table, as a
// change of the table outside pco would.
func (n *fakeNft) dropRule(table, chain string, index int) {
	t := n.t
	t.Helper()
	tb := n.tables[table]
	require.NotNil(t, tb, "table %s", table)
	seen := 0
	for i, e := range tb.base {
		body, ok := e["rule"]
		if !ok {
			continue
		}
		var r struct {
			Chain string `json:"chain"`
		}
		require.NoError(t, json.Unmarshal(body, &r))
		if r.Chain != chain {
			continue
		}
		if seen == index {
			tb.base = slices.Delete(slices.Clone(tb.base), i, i+1)
			return
		}
		seen++
	}
	t.Fatalf("chain %s of %s has no rule %d", chain, table, index)
}

// harness is a host backend on fakes.
type harness struct {
	sys     *fakeSystem
	nsNft   *fakeNft
	hostNft *fakeNft
	b       *hostBackend
	now     time.Time
}

func newHarness(t *testing.T) *harness {
	h := &harness{sys: newFakeSystem(), nsNft: newFakeNft(t), hostNft: newFakeNft(t), now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	h.nsNft.order, h.hostNft.order = &h.sys.order, &h.sys.order
	h.b = newHost(h.sys, h.nsNft, h.hostNft, zerolog.Nop(), func() time.Time { return h.now })
	return h
}

// reset forgets the calls and scripts so far.
func (h *harness) reset() {
	h.sys.calls, h.nsNft.scripts, h.hostNft.scripts = nil, nil, nil
}

// up runs Up and forgets what it did.
func (h *harness) up(t *testing.T) {
	t.Helper()
	require.NoError(t, h.b.Up(t.Context()))
	h.reset()
}

func (h *harness) sync(t *testing.T, legs []Leg, services []Service, pins []Pin, sticky []Sticky) []LegState {
	t.Helper()
	states, err := h.b.Sync(t.Context(), legs, services, pins, sticky)
	require.NoError(t, err)
	return states
}

// index returns the position of the first call that starts with prefix.
func index(t *testing.T, calls []string, prefix string) int {
	t.Helper()
	i := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
	require.GreaterOrEqual(t, i, 0, "no call %q in %s", prefix, strings.Join(calls, "\n"))
	return i
}

// The legs and services of the tests: two legs into identical subnets, one
// of them tagged on a bridge that filters VLANs, and the managed leg.
var (
	legA = Leg{ID: "vmbr1", Mark: 1, Bridge: "vmbr1", Prefix: prefix("10.50.0.250/24"), MAC: mac("02:50:43:4f:00:01")}
	legB = Leg{ID: "vmbr2.20", Mark: 2, Bridge: "vmbr2", VLAN: 20, Prefix: prefix("10.50.0.250/24"), MAC: mac("02:50:43:4f:00:02")}
	legM = Leg{ID: ManagedBridge, Mark: 3, Bridge: ManagedBridge, Prefix: prefix("10.203.118.1/24"), MAC: ManagedLegMAC, Managed: true}

	svcA = Service{Leg: "vmbr1", Service: addr("198.18.1.0"), Addr: addr("10.50.0.10"), Ports: []uint16{443, 80}}
	svcB = Service{Leg: "vmbr2.20", Service: addr("198.18.1.1"), Addr: addr("10.50.0.10"), Ports: []uint16{80}}
	svcM = Service{Leg: ManagedBridge, Service: addr("198.18.1.2"), Addr: addr("10.203.118.5"), Ports: []uint16{8080}}
)

// node gives the fake the bridges of the legs of the tests.
func (h *harness) node() *harness {
	h.sys.bridge("vmbr1", false, 1500, "eno1")
	h.sys.bridge("vmbr2", true, 9000, "eno2")
	h.sys.bridge(ManagedBridge, false, 1500)
	return h
}
