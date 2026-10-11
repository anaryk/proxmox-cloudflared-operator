package gateway

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// counterLocal counts the appliance's own packets that a DNAT sent to an
// address of the appliance itself.
const counterLocal = "dropped_local"

var ctCounters = []string{counterLocal, counterOIF, counterLegIn}

// ctDeclarations are the sets and maps of the appliance's table: those of the
// namespace's on the host but allowed, which only a forward chain reads.
var ctDeclarations = slices.DeleteFunc(slices.Clone(declarations), func(d declaration) bool { return d.name == setAllowed })

// ctElementsOf returns the elements of the appliance's table for the legs and
// the services: those of the namespace's but allowed.
func ctElementsOf(legs []Leg, services []Service) elements {
	e := elementsOf(legs, services)
	delete(e, setAllowed)
	return e
}

// renderContainer returns the script that replaces the appliance's table,
// whatever it holds, with one that holds e, in one transaction.
func renderContainer(e elements) string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table inet %[1]s\ndelete table inet %[1]s\ntable inet %[1]s {\n", gwTable)
	writeDeclarations(&b, ctDeclarations, e)
	for _, c := range ctCounters {
		fmt.Fprintf(&b, "\tcounter %s {\n\t}\n", c)
	}
	b.WriteString(withPrefix(ctChains))
	b.WriteString("}\n")
	return b.String()
}

// ctChains are the chains of the appliance's table. The connectors are
// processes of the appliance, so a request to a service address takes the
// output path: it gets the zone and the mark of its leg before conntrack sees
// it, and the mark picks the leg's routing table once the DNAT has given the
// packet the target's address. The DNAT runs at the NAT hook of the output
// path (-100), before the service prefix's own table (filter - 20) and the
// egress filter (filter - 10): the first sees the target's address and lets
// it pass, the second matches the service address by conntrack's original
// tuple. The NAT chain's priority is written as the number dstnat stands
// for: nft 1.0.6 takes the name in prerouting only, and the kernel runs the
// NAT chains of a hook from its one NAT hook at -100 whatever priority they
// say, so the chain is the same either way. A service address no DNAT maps
// keeps its address, which the service prefix's table rejects. The
// appliance's own packets to a leg, as the DHCP server's answers, get the
// leg's zone on the way out; what comes in on a leg gets it on the way in,
// without which a reply to a DNATed flow would land in zone 0 and a guest
// could inject into the appliance's own flows.
// A DNATed flow to an address of the appliance itself is dropped where the
// output path sees the target's address. The oif guard sits in postrouting:
// in the output hook a DNATed packet still names the device the service
// address was routed to. The input chain drops what a leg sends but replies
// and DHCP on the managed leg, in that order, and leaves net0 alone; nothing
// is forwarded, although forwarding is off.
const ctChains = `	chain out_raw {
		type filter hook output priority raw; policy accept;
		ip daddr @SERVICE_PREFIX@ ct zone set ip daddr map @svc_zone
		ip daddr @SERVICE_PREFIX@ meta mark set ip daddr map @svc_mark
		oifname @legs ct zone set oifname map @leg_zone
	}
	chain out_nat {
		type nat hook output priority -100; policy accept;
		ip daddr @SERVICE_PREFIX@ meta l4proto tcp dnat ip to ip daddr . tcp dport map @svc_dnat
	}
	chain out_guard {
		type filter hook output priority filter; policy accept;
		ct status dnat fib daddr type local counter name "dropped_local" drop
	}
	chain post_filter {
		type filter hook postrouting priority filter; policy accept;
		ct status dnat oifname . ct zone != @leg_ok counter name "dropped_oif" drop
	}
	chain post_nat {
		type nat hook postrouting priority srcnat; policy accept;
		meta l4proto tcp oifname @legs snat ip to oifname map @leg_snat
	}
	chain pre_raw {
		type filter hook prerouting priority raw; policy accept;
		iifname @legs ct zone set iifname map @leg_zone
	}
	chain input {
		type filter hook input priority filter; policy accept;
		iif "lo" accept
		ct state established,related accept
		iifname @legs ct state invalid drop
		iifname @dhcp_legs udp dport 67 udp sport 68 accept
		iifname @legs counter name "dropped_leg_in" drop
	}
	chain forward {
		type filter hook forward priority filter; policy drop;
	}
`

// withPrefix fills the service prefix into a text of chains or rules.
func withPrefix(text string) string {
	return strings.ReplaceAll(text, "@SERVICE_PREFIX@", ServicePrefix.String())
}

// ctListedRules are the rules of each chain of the appliance's table as nft
// 1.1.3 lists them: the DNAT without its match on TCP, which the TCP port of
// its map implies.
var ctListedRules = map[string]string{
	"out_raw": `[
[{"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"daddr"}},"right":{"prefix":{"addr":"@PREFIX_ADDR@","len":@PREFIX_LEN@}}}},{"mangle":{"key":{"ct":{"key":"zone"}},"value":{"map":{"key":{"payload":{"protocol":"ip","field":"daddr"}},"data":"@svc_zone"}}}}],
[{"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"daddr"}},"right":{"prefix":{"addr":"@PREFIX_ADDR@","len":@PREFIX_LEN@}}}},{"mangle":{"key":{"meta":{"key":"mark"}},"value":{"map":{"key":{"payload":{"protocol":"ip","field":"daddr"}},"data":"@svc_mark"}}}}],
[{"match":{"op":"==","left":{"meta":{"key":"oifname"}},"right":"@legs"}},{"mangle":{"key":{"ct":{"key":"zone"}},"value":{"map":{"key":{"meta":{"key":"oifname"}},"data":"@leg_zone"}}}}]
]`,
	"out_nat": `[
[{"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"daddr"}},"right":{"prefix":{"addr":"@PREFIX_ADDR@","len":@PREFIX_LEN@}}}},{"dnat":{"family":"ip","addr":{"map":{"key":{"concat":[{"payload":{"protocol":"ip","field":"daddr"}},{"payload":{"protocol":"tcp","field":"dport"}}]},"data":"@svc_dnat"}}}}]
]`,
	"out_guard": `[
[{"match":{"op":"in","left":{"ct":{"key":"status"}},"right":"dnat"}},{"match":{"op":"==","left":{"fib":{"result":"type","flags":["daddr"]}},"right":"local"}},{"counter":"dropped_local"},{"drop":null}]
]`,
	"post_filter": `[
[{"match":{"op":"in","left":{"ct":{"key":"status"}},"right":"dnat"}},{"match":{"op":"!=","left":{"concat":[{"meta":{"key":"oifname"}},{"ct":{"key":"zone"}}]},"right":"@leg_ok"}},{"counter":"dropped_oif"},{"drop":null}]
]`,
	"post_nat": `[
[{"match":{"op":"==","left":{"meta":{"key":"l4proto"}},"right":6}},{"match":{"op":"==","left":{"meta":{"key":"oifname"}},"right":"@legs"}},{"snat":{"family":"ip","addr":{"map":{"key":{"meta":{"key":"oifname"}},"data":"@leg_snat"}}}}]
]`,
	"pre_raw": `[
[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"@legs"}},{"mangle":{"key":{"ct":{"key":"zone"}},"value":{"map":{"key":{"meta":{"key":"iifname"}},"data":"@leg_zone"}}}}]
]`,
	"input": `[
[{"match":{"op":"==","left":{"meta":{"key":"iif"}},"right":"lo"}},{"accept":null}],
[{"match":{"op":"in","left":{"ct":{"key":"state"}},"right":["established","related"]}},{"accept":null}],
[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"@legs"}},{"match":{"op":"in","left":{"ct":{"key":"state"}},"right":"invalid"}},{"drop":null}],
[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"@dhcp_legs"}},{"match":{"op":"==","left":{"payload":{"protocol":"udp","field":"sport"}},"right":68}},{"match":{"op":"==","left":{"payload":{"protocol":"udp","field":"dport"}},"right":67}},{"accept":null}],
[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"@legs"}},{"counter":"dropped_leg_in"},{"drop":null}]
]`,
	"forward": `[]`,
}

// ctChainHooks are the chains of the appliance's table with their hooks, as
// nft lists them.
func ctChainHooks() map[string]listedChain {
	prio := func(p string) *json.Number {
		n := json.Number(p)
		return &n
	}
	return map[string]listedChain{
		"out_raw":     {Type: "filter", Hook: "output", Prio: prio("-300"), Policy: "accept"},
		"out_nat":     {Type: "nat", Hook: "output", Prio: prio("-100"), Policy: "accept"},
		"out_guard":   {Type: "filter", Hook: "output", Prio: prio("0"), Policy: "accept"},
		"post_filter": {Type: "filter", Hook: "postrouting", Prio: prio("0"), Policy: "accept"},
		"post_nat":    {Type: "nat", Hook: "postrouting", Prio: prio("100"), Policy: "accept"},
		"pre_raw":     {Type: "filter", Hook: "prerouting", Prio: prio("-300"), Policy: "accept"},
		"input":       {Type: "filter", Hook: "input", Prio: prio("0"), Policy: "accept"},
		"forward":     {Type: "filter", Hook: "forward", Prio: prio("0"), Policy: "drop"},
	}
}

// ctDifferences says what of a listing of the appliance's table, but its
// elements, is not as pco loads it.
func ctDifferences(l *listing) []string {
	chains := make(map[string]string, len(ctListedRules))
	for name, rules := range ctListedRules {
		chains[name] = strings.NewReplacer("@PREFIX_ADDR@", ServicePrefix.Addr().String(),
			"@PREFIX_LEN@", fmt.Sprint(ServicePrefix.Bits())).Replace(rules)
	}
	return tableDifferences(l, "table inet "+gwTable, ctChainHooks(), wantRules(chains), ctDeclarations, ctCounters)
}
