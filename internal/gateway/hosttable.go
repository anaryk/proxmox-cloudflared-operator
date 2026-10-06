package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
)

// hostTable is the table of the host's own namespace. It keeps the node out
// of reach of the managed network and of the transit link, which nothing of
// the namespace's table can do: both meet the host before the namespace.
const hostTable = "pco_host"

var hostCounters = []string{"from_pconet", "from_transit", "routed_from_pconet", "to_transit"}

// renderHost returns the script that replaces the host table with the one pco
// loads, in one transaction.
//
// from_pconet keeps the node's services off the managed bridge: isolated
// ports are kept from each other, not from the bridge device. from_transit
// keeps the guests behind the legs from reaching the node's services through
// the transit link while the namespace's table is missing; the replies to the
// host's own flows are not new. routed_from_pconet keeps a managed guest from
// using the node as its router to the LAN and the Internet, which an ARP for
// the node's LAN address and the node's forwarding would allow; frames bridged
// between two managed guests reach the forward hook with the bridge as both
// interfaces where bridge-nf-call-iptables is on, and pass. to_transit keeps a
// node that forwards or masquerades for other networks from passing their
// packets into the transit link, where a masqueraded source would be the
// host's transit address that the namespace accepts.
func renderHost() string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table inet %[1]s\ndelete table inet %[1]s\ntable inet %[1]s {\n", hostTable)
	for _, c := range hostCounters {
		fmt.Fprintf(&b, "\tcounter %s {\n\t}\n", c)
	}
	fmt.Fprintf(&b, `	chain input {
		type filter hook input priority filter - 10; policy accept;
		iifname %[1]q counter name "from_pconet" drop
		iifname %[2]q ct state new counter name "from_transit" drop
	}
	chain forward {
		type filter hook forward priority filter - 10; policy accept;
		iifname %[1]q oifname != %[1]q counter name "routed_from_pconet" drop
		oifname %[2]q counter name "to_transit" drop
	}
}
`, ManagedBridge, hostTransit)
	return b.String()
}

// hostListedRules are the rules of the host table as nft -j lists them.
const hostListedRules = `{
"input": [
[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"pconet"}},{"counter":"from_pconet"},{"drop":null}],
[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"pcotr0"}},{"match":{"op":"in","left":{"ct":{"key":"state"}},"right":"new"}},{"counter":"from_transit"},{"drop":null}]
],
"forward": [
[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"pconet"}},{"match":{"op":"!=","left":{"meta":{"key":"oifname"}},"right":"pconet"}},{"counter":"routed_from_pconet"},{"drop":null}],
[{"match":{"op":"==","left":{"meta":{"key":"oifname"}},"right":"pcotr0"}},{"counter":"to_transit"},{"drop":null}]
]
}`

// hostDifferences says what of a listing of the host table is not as pco
// loads it.
func hostDifferences(l *listing) []string {
	var listed map[string]json.RawMessage
	chains := map[string]string{}
	if err := json.Unmarshal([]byte(hostListedRules), &listed); err == nil {
		for name, rules := range listed {
			chains[name] = string(rules)
		}
	}
	prio := json.Number("-10")
	hooks := map[string]listedChain{
		"input":   {Type: "filter", Hook: "input", Prio: &prio, Policy: "accept"},
		"forward": {Type: "filter", Hook: "forward", Prio: &prio, Policy: "accept"},
	}
	return tableDifferences(l, "table inet "+hostTable, hooks, wantRules(chains), nil, hostCounters)
}
