package gateway

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

const (
	// gwTable is the table of the namespace: zones, marks, DNAT, SNAT and the
	// filter of everything that passes it.
	gwTable = "pco_gw"

	mapSvcZone  = "svc_zone"
	mapSvcMark  = "svc_mark"
	mapLegZone  = "leg_zone"
	mapSvcDNAT  = "svc_dnat"
	mapLegSNAT  = "leg_snat"
	setAllowed  = "allowed"
	setLegs     = "legs"
	setDHCPLegs = "dhcp_legs"
	setLegOK    = "leg_ok"

	counterForward = "dropped_forward"
	counterInput   = "dropped_input"
	counterLegIn   = "dropped_leg_in"
	counterTransit = "dropped_transit"
	counterOIF     = "dropped_oif"
)

// snatForm is how the SNAT through each leg to its address and its block of
// ports is written: as one element of the map leg_snat, or as one rule of its
// own for an nft that refuses range data in a NAT map.
type snatForm int

const (
	snatMap snatForm = iota
	snatRules
)

// theSNATForm is the form the table is loaded in. nft 1.0.6 and 1.1.3 take
// the map, which ruleset_linux_test.go loads to show it.
const theSNATForm = snatMap

// declaration is a set or map of the table: the types of its key and, for a
// map, of its data, as nft lists them, and how the script declares it.
type declaration struct {
	name  string
	isMap bool
	key   []string
	data  []string
	flags []string
	decl  string
}

// declarations are the sets and maps of the table, in the order of the
// script. typeof keeps the data a key or value is read as in the rule that
// uses it; the concatenations of addresses and ports need type, as does the
// interval data of leg_snat.
var declarations = []declaration{
	{name: mapSvcZone, isMap: true, key: []string{"ipv4_addr"}, data: []string{"integer"}, decl: "typeof ip daddr : ct zone"},
	{name: mapSvcMark, isMap: true, key: []string{"ipv4_addr"}, data: []string{"mark"}, decl: "typeof ip daddr : meta mark"},
	{name: mapLegZone, isMap: true, key: []string{"ifname"}, data: []string{"integer"}, decl: "typeof iifname : ct zone"},
	{
		name: mapSvcDNAT, isMap: true, key: []string{"ipv4_addr", "inet_service"}, data: []string{"ipv4_addr", "inet_service"},
		decl: "type ipv4_addr . inet_service : ipv4_addr . inet_service",
	},
	{
		name: mapLegSNAT, isMap: true, key: []string{"ifname"}, data: []string{"ipv4_addr", "inet_service"},
		flags: []string{"interval"}, decl: "type ifname : interval ipv4_addr . inet_service\n\t\tflags interval",
	},
	{name: setAllowed, key: []string{"ifname", "ipv4_addr", "inet_service"}, decl: "typeof oifname . ip daddr . tcp dport"},
	{name: setLegs, key: []string{"ifname"}, decl: "typeof iifname"},
	{name: setDHCPLegs, key: []string{"ifname"}, decl: "typeof iifname"},
	{name: setLegOK, key: []string{"ifname", "integer"}, decl: "typeof oifname . ct zone"},
}

// inForm returns the elements of the sets and maps the table has in a form:
// the form with a rule per leg has no leg_snat.
func inForm(form snatForm, e elements) elements {
	if form == snatRules {
		delete(e, mapLegSNAT)
	}
	return e
}

// declarationsOf returns the sets and maps of the table in a form.
func declarationsOf(form snatForm) []declaration {
	if form == snatMap {
		return declarations
	}
	return slices.DeleteFunc(slices.Clone(declarations), func(d declaration) bool { return d.name == mapLegSNAT })
}

var gwCounters = []string{counterForward, counterInput, counterLegIn, counterTransit, counterOIF}

// elements are what the sets and maps of the table hold, by name, each
// element as a script writes it and in order: "198.18.1.0 . 80 : 10.50.0.10 .
// 80" for a map, "\"leg1\"" for a set.
type elements map[string][]string

// validServices returns the services that can be programmed on their legs,
// and why each other one cannot, by the network id of its leg: the engine
// never gives such a service, and the backend does not take its word for it.
// A service on a leg that is not given is left out without a word.
func validServices(legs []Leg, services []Service) ([]Service, map[string][]string) {
	byID := map[string]Leg{}
	for _, l := range legs {
		byID[l.ID] = l
	}
	var valid []Service
	refused := map[string][]string{}
	first := map[netip.Addr]Service{}
	for _, s := range services {
		l, ok := byID[s.Leg]
		if !ok {
			continue
		}
		problem := serviceProblem(s, l)
		if f, ok := first[s.Service]; ok && problem == "" && (f.Leg != s.Leg || f.Addr != s.Addr) {
			problem = fmt.Sprintf("service address %s is given for %s and for %s", s.Service, f.Addr, s.Addr)
		}
		if problem != "" {
			refused[s.Leg] = append(refused[s.Leg], problem)
			continue
		}
		if _, ok := first[s.Service]; !ok {
			first[s.Service] = s
		}
		valid = append(valid, s)
	}
	return valid, refused
}

// serviceProblem says why a service cannot be programmed on its leg: its
// address must be one of the prefix above the transit link, its target one
// of the leg's network other than the leg itself.
func serviceProblem(s Service, l Leg) string {
	switch {
	case !s.Service.Is4() || !ServicePrefix.Contains(s.Service):
		return fmt.Sprintf("service address %s is outside %s", s.Service, ServicePrefix)
	case netip.PrefixFrom(TransitHost, transitBits).Masked().Contains(s.Service):
		return fmt.Sprintf("service address %s is one of the transit link", s.Service)
	case !s.Addr.Is4() || !l.Prefix.Masked().Contains(s.Addr):
		return fmt.Sprintf("%s of service address %s is outside %s, the network of the leg", s.Addr, s.Service, l.Prefix.Masked())
	case s.Addr == l.Prefix.Addr():
		return fmt.Sprintf("%s of service address %s is the address of the leg", s.Addr, s.Service)
	}
	return ""
}

// elementsOf returns the elements the table holds for the legs and the
// services: a leg's interface in legs, its zone, its address and block of
// source ports, the pair of its interface and zone, and a managed one in
// dhcp_legs; per service its zone and mark, and per port its DNAT and the
// flow forward allows. A service on a leg that is not given has no elements:
// nothing says which leg it would leave through; neither has one that
// validServices refuses.
func elementsOf(legs []Leg, services []Service) elements {
	services, _ = validServices(legs, services)
	e := elements{}
	add := func(set, elem string) { e[set] = append(e[set], elem) }
	marks := map[string]int{}
	for _, l := range sortedLegs(legs) {
		marks[l.ID] = l.Mark
		name := quoted(l.Iface())
		lo, hi := PortBlock(l.Mark)
		add(setLegs, name)
		add(mapLegZone, fmt.Sprintf("%s : %d", name, l.Mark))
		add(mapLegSNAT, fmt.Sprintf("%s : %s . %d-%d", name, l.Prefix.Addr(), lo, hi))
		add(setLegOK, fmt.Sprintf("%s . %d", name, l.Mark))
		if l.Managed {
			add(setDHCPLegs, name)
		}
	}
	for _, s := range sortedServices(services) {
		mark, ok := marks[s.Leg]
		if !ok {
			continue
		}
		add(mapSvcZone, fmt.Sprintf("%s : %d", s.Service, mark))
		add(mapSvcMark, fmt.Sprintf("%s : %d", s.Service, mark))
		for _, p := range s.Ports {
			add(mapSvcDNAT, fmt.Sprintf("%s . %d : %s . %d", s.Service, p, s.Addr, p))
			add(setAllowed, fmt.Sprintf("%s . %s . %d", quoted(LegIface(mark)), s.Addr, p))
		}
	}
	for name, elems := range e {
		e[name] = uniqueByKey(elems)
	}
	return e
}

// uniqueByKey keeps the first element of each key: two services on one
// service address, which the engine never gives, cannot both be loaded.
func uniqueByKey(elems []string) []string {
	seen := map[string]bool{}
	return slices.DeleteFunc(elems, func(el string) bool {
		k := elementKey(el)
		if seen[k] {
			return true
		}
		seen[k] = true
		return false
	})
}

// elementKey is the key of a map element, or a set element itself.
func elementKey(el string) string {
	k, _, _ := strings.Cut(el, " : ")
	return k
}

func quoted(name string) string { return `"` + name + `"` }

func sortedLegs(legs []Leg) []Leg {
	out := slices.Clone(legs)
	slices.SortStableFunc(out, func(a, b Leg) int { return cmp.Compare(a.Mark, b.Mark) })
	return out
}

func sortedServices(services []Service) []Service {
	out := make([]Service, 0, len(services))
	for _, s := range services {
		s.Ports = slices.Clone(s.Ports)
		slices.Sort(s.Ports)
		s.Ports = slices.Compact(s.Ports)
		out = append(out, s)
	}
	slices.SortStableFunc(out, func(a, b Service) int { return a.Service.Compare(b.Service) })
	return out
}

// renderGateway returns the script that replaces the table, whatever it
// holds, with one that holds e, in one transaction: the add makes the delete
// work when there is no table, and the delete takes whatever others added to
// it. legs are those of e, for the form that writes a rule per leg.
func renderGateway(form snatForm, e elements, legs []Leg) string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table inet %[1]s\ndelete table inet %[1]s\ntable inet %[1]s {\n", gwTable)
	for _, d := range declarationsOf(form) {
		kind := "set"
		if d.isMap {
			kind = "map"
		}
		fmt.Fprintf(&b, "\t%s %s {\n\t\t%s\n", kind, d.name, d.decl)
		if elems := e[d.name]; len(elems) > 0 {
			fmt.Fprintf(&b, "\t\telements = {\n\t\t\t%s\n\t\t}\n", strings.Join(elems, ",\n\t\t\t"))
		}
		b.WriteString("\t}\n")
	}
	for _, c := range gwCounters {
		fmt.Fprintf(&b, "\tcounter %s {\n\t}\n", c)
	}
	b.WriteString(withTransit(gwChains))
	b.WriteString("\tchain post_nat {\n\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
	if form == snatMap {
		b.WriteString("\t\tmeta l4proto tcp oifname @legs snat ip to oifname map @leg_snat\n")
	} else {
		for _, l := range sortedLegs(legs) {
			lo, hi := PortBlock(l.Mark)
			fmt.Fprintf(&b, "\t\tmeta l4proto tcp oifname %s snat ip to %s:%d-%d\n", quoted(l.Iface()), l.Prefix.Addr(), lo, hi)
		}
	}
	b.WriteString("\t}\n")
	b.WriteString(withTransit(gwFilterChains))
	b.WriteString("}\n")
	return b.String()
}

// gwChains are the chains before the SNAT.
//
// Transit ingress: only the host's own transit address may send into the
// namespace, so that a host forwarding for other networks is no way in; a
// packet to a service address gets the zone and the mark of its leg before
// conntrack sees it, so that legs into identical subnets keep their flows
// apart and the mark picks the leg's routing table. Leg ingress gets the
// leg's zone, without which a reply to a DNATed flow would land in zone 0 and
// a guest could inject into the namespace's own flows; the namespace's own
// packets to a leg (the DHCP server's) get it on the way out. DNAT maps a
// service address and a published TCP port, nothing else. The oif guard lets
// a DNATed flow leave only through the leg of its zone; it holds for the
// original direction, since the replies leave through the transit link.
const gwChains = `	chain pre_raw {
		type filter hook prerouting priority raw; policy accept;
		iifname "transit0" ip saddr != @TRANSIT_HOST@ counter name "dropped_transit" drop
		iifname "transit0" ct zone set ip daddr map @svc_zone
		iifname "transit0" meta mark set ip daddr map @svc_mark
		iifname @legs ct zone set iifname map @leg_zone
	}
	chain out_raw {
		type filter hook output priority raw; policy accept;
		oifname @legs ct zone set oifname map @leg_zone
	}
	chain pre_nat {
		type nat hook prerouting priority dstnat; policy accept;
		iifname "transit0" ip saddr @TRANSIT_HOST@ meta l4proto tcp dnat ip to ip daddr . tcp dport map @svc_dnat
	}
	chain post_filter {
		type filter hook postrouting priority filter; policy accept;
		ct status dnat ct direction original oifname . ct zone != @leg_ok counter name "dropped_oif" drop
	}
`

// gwFilterChains forward nothing but the DNATed flows from the host to a
// published port of a target, their replies and related ICMP, and accept as
// input nothing from a leg but DHCP on the managed one, in that order: the
// namespace's own services stay out of reach of every guest.
const gwFilterChains = `	chain forward {
		type filter hook forward priority filter; policy drop;
		ct state established,related accept
		ct state invalid drop
		iifname "transit0" ip saddr @TRANSIT_HOST@ ct status dnat ct state new oifname . ip daddr . tcp dport @allowed accept
		counter name "dropped_forward"
	}
	chain input {
		type filter hook input priority filter; policy drop;
		iif "lo" accept
		ct state established,related accept
		ct state invalid drop
		iifname @dhcp_legs udp dport 67 udp sport 68 accept
		iifname @legs counter name "dropped_leg_in" drop
		counter name "dropped_input"
	}
`

// withTransit fills the host's transit address into a text of chains.
func withTransit(chains string) string {
	return strings.ReplaceAll(chains, "@TRANSIT_HOST@", TransitHost.String())
}

// elementScript returns the script that turns the elements have into want in
// one transaction: what is new is added first, a key whose value changed is
// deleted and added with its new value, and what is no longer wanted is
// deleted last. An empty script means nothing changes.
func elementScript(have, want elements) string {
	var adds, changes, deletes strings.Builder
	for _, name := range setNames(have, want) {
		hv, wv := byKey(have[name]), byKey(want[name])
		var added, removed []string
		for _, el := range want[name] {
			k := elementKey(el)
			old, ok := hv[k]
			switch {
			case !ok:
				added = append(added, el)
			case old != el:
				fmt.Fprintf(&changes, "delete element inet %s %s { %s }\n", gwTable, name, k)
				fmt.Fprintf(&changes, "add element inet %s %s { %s }\n", gwTable, name, el)
			}
		}
		for _, el := range have[name] {
			if k := elementKey(el); wv[k] == "" {
				removed = append(removed, k)
			}
		}
		if len(added) > 0 {
			fmt.Fprintf(&adds, "add element inet %s %s { %s }\n", gwTable, name, strings.Join(added, ", "))
		}
		if len(removed) > 0 {
			fmt.Fprintf(&deletes, "delete element inet %s %s { %s }\n", gwTable, name, strings.Join(removed, ", "))
		}
	}
	return adds.String() + changes.String() + deletes.String()
}

// setNames returns the names of the sets of both, in the order of the
// declarations, then any other.
func setNames(a, b elements) []string {
	var names []string
	for _, d := range declarations {
		names = append(names, d.name)
	}
	for _, e := range []elements{a, b} {
		for name := range e {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

func byKey(elems []string) map[string]string {
	m := make(map[string]string, len(elems))
	for _, el := range elems {
		m[elementKey(el)] = el
	}
	return m
}

// elementDifferences says which elements a set lacks and which it holds
// beyond want.
func elementDifferences(have, want elements) []string {
	var d []string
	for _, name := range setNames(have, want) {
		for _, el := range want[name] {
			if !slices.Contains(have[name], el) {
				d = append(d, fmt.Sprintf("%s of %s lacks %s", name, gwTable, el))
			}
		}
		for _, el := range have[name] {
			if !slices.Contains(want[name], el) {
				d = append(d, fmt.Sprintf("%s of %s holds %s", name, gwTable, el))
			}
		}
	}
	return d
}

// gwListedRules are the rules of each chain as nft -j lists them, nft 1.0.6
// and 1.1.3 alike; post_nat's is that of the map form. The listing leaves out
// what a rule's other matches imply, as the TCP of a DNAT keyed on the TCP
// port, and orders the matches on one header by their offset.
var gwListedRules = map[string]string{
	"pre_raw": `[
[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"transit0"}},{"match":{"op":"!=","left":{"payload":{"protocol":"ip","field":"saddr"}},"right":"@TRANSIT_HOST@"}},{"counter":"dropped_transit"},{"drop":null}],
[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"transit0"}},{"mangle":{"key":{"ct":{"key":"zone"}},"value":{"map":{"key":{"payload":{"protocol":"ip","field":"daddr"}},"data":"@svc_zone"}}}}],
[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"transit0"}},{"mangle":{"key":{"meta":{"key":"mark"}},"value":{"map":{"key":{"payload":{"protocol":"ip","field":"daddr"}},"data":"@svc_mark"}}}}],
[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"@legs"}},{"mangle":{"key":{"ct":{"key":"zone"}},"value":{"map":{"key":{"meta":{"key":"iifname"}},"data":"@leg_zone"}}}}]
]`,
	"out_raw": `[
[{"match":{"op":"==","left":{"meta":{"key":"oifname"}},"right":"@legs"}},{"mangle":{"key":{"ct":{"key":"zone"}},"value":{"map":{"key":{"meta":{"key":"oifname"}},"data":"@leg_zone"}}}}]
]`,
	"pre_nat": `[
[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"transit0"}},{"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"saddr"}},"right":"@TRANSIT_HOST@"}},{"dnat":{"family":"ip","addr":{"map":{"key":{"concat":[{"payload":{"protocol":"ip","field":"daddr"}},{"payload":{"protocol":"tcp","field":"dport"}}]},"data":"@svc_dnat"}}}}]
]`,
	"post_filter": `[
[{"match":{"op":"in","left":{"ct":{"key":"status"}},"right":"dnat"}},{"match":{"op":"==","left":{"ct":{"key":"direction"}},"right":"original"}},{"match":{"op":"!=","left":{"concat":[{"meta":{"key":"oifname"}},{"ct":{"key":"zone"}}]},"right":"@leg_ok"}},{"counter":"dropped_oif"},{"drop":null}]
]`,
	"post_nat": `[
[{"match":{"op":"==","left":{"meta":{"key":"l4proto"}},"right":6}},{"match":{"op":"==","left":{"meta":{"key":"oifname"}},"right":"@legs"}},{"snat":{"family":"ip","addr":{"map":{"key":{"meta":{"key":"oifname"}},"data":"@leg_snat"}}}}]
]`,
	"forward": `[
[{"match":{"op":"in","left":{"ct":{"key":"state"}},"right":["established","related"]}},{"accept":null}],
[{"match":{"op":"in","left":{"ct":{"key":"state"}},"right":"invalid"}},{"drop":null}],
[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"transit0"}},{"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"saddr"}},"right":"@TRANSIT_HOST@"}},{"match":{"op":"in","left":{"ct":{"key":"status"}},"right":"dnat"}},{"match":{"op":"in","left":{"ct":{"key":"state"}},"right":"new"}},{"match":{"op":"==","left":{"concat":[{"meta":{"key":"oifname"}},{"payload":{"protocol":"ip","field":"daddr"}},{"payload":{"protocol":"tcp","field":"dport"}}]},"right":"@allowed"}},{"accept":null}],
[{"counter":"dropped_forward"}]
]`,
	"input": `[
[{"match":{"op":"==","left":{"meta":{"key":"iif"}},"right":"lo"}},{"accept":null}],
[{"match":{"op":"in","left":{"ct":{"key":"state"}},"right":["established","related"]}},{"accept":null}],
[{"match":{"op":"in","left":{"ct":{"key":"state"}},"right":"invalid"}},{"drop":null}],
[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"@dhcp_legs"}},{"match":{"op":"==","left":{"payload":{"protocol":"udp","field":"sport"}},"right":68}},{"match":{"op":"==","left":{"payload":{"protocol":"udp","field":"dport"}},"right":67}},{"accept":null}],
[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"@legs"}},{"counter":"dropped_leg_in"},{"drop":null}],
[{"counter":"dropped_input"}]
]`,
}

// snatRuleListed is the rule of one leg in the form that writes a rule per
// leg, as nft -j lists it.
const snatRuleListed = `[{"match":{"op":"==","left":{"meta":{"key":"l4proto"}},"right":6}},{"match":{"op":"==","left":{"meta":{"key":"oifname"}},"right":%q}},{"snat":{"family":"ip","addr":%q,"port":{"range":[%d,%d]}}}]`

// gwChainHooks are the chains of the table with their hooks, as nft lists
// them.
func gwChainHooks() map[string]listedChain {
	prio := func(p string) *json.Number {
		n := json.Number(p)
		return &n
	}
	return map[string]listedChain{
		"pre_raw":     {Type: "filter", Hook: "prerouting", Prio: prio("-300"), Policy: "accept"},
		"out_raw":     {Type: "filter", Hook: "output", Prio: prio("-300"), Policy: "accept"},
		"pre_nat":     {Type: "nat", Hook: "prerouting", Prio: prio("-100"), Policy: "accept"},
		"post_filter": {Type: "filter", Hook: "postrouting", Prio: prio("0"), Policy: "accept"},
		"post_nat":    {Type: "nat", Hook: "postrouting", Prio: prio("100"), Policy: "accept"},
		"forward":     {Type: "filter", Hook: "forward", Prio: prio("0"), Policy: "drop"},
		"input":       {Type: "filter", Hook: "input", Prio: prio("0"), Policy: "drop"},
	}
}

// gwWantRules returns the canonical rules of each chain of the table in a
// form, for the legs given.
func gwWantRules(form snatForm, legs []Leg) map[string][]string {
	chains := make(map[string]string, len(gwListedRules))
	for name, rules := range gwListedRules {
		chains[name] = withTransit(rules)
	}
	if form == snatRules {
		rules := make([]string, 0, len(legs))
		for _, l := range sortedLegs(legs) {
			lo, hi := PortBlock(l.Mark)
			rules = append(rules, fmt.Sprintf(snatRuleListed, l.Iface(), l.Prefix.Addr(), lo, hi))
		}
		chains["post_nat"] = "[" + strings.Join(rules, ",") + "]"
	}
	return wantRules(chains)
}

// gwDifferences says what of a listing of the table, but its elements, is
// not as the table of the form is loaded for the legs.
func gwDifferences(l *listing, form snatForm, legs []Leg) []string {
	return tableDifferences(l, "table inet "+gwTable, gwChainHooks(), gwWantRules(form, legs),
		declarationsOf(form), gwCounters)
}

// listedElements returns the elements of the sets and maps of a listing,
// each set sorted.
func listedElements(l *listing) elements {
	e := elements{}
	for name, s := range l.sets {
		if len(s.elems) > 0 {
			e[name] = s.elems
		}
	}
	return e
}

// sortedElements returns the elements with each set sorted, as a listing
// reads them.
func sortedElements(e elements) elements {
	out := make(elements, len(e))
	for name, elems := range e {
		if len(elems) > 0 {
			out[name] = slices.Sorted(slices.Values(elems))
		}
	}
	return out
}
