package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// guardTable is the host's bridge table that keeps each managed guest to its
// own card on the managed bridge: an IPv4 packet or an ARP frame that enters
// the bridge from a guest's port passes only with the MAC and the address of
// the card pco gave the guest there. The namespace sees frames only once the
// bridge passed them and cannot tell the port they came from; the sticky
// entries keep a MAC's traffic on its port, not a guest from sending with
// another's MAC and address.
const guardTable = "pco_guard"

const (
	setCards        = "cards"
	setCardMACs     = "card_macs"
	counterSpoofed  = "dropped_spoofed"
	guardFamilyName = "bridge " + guardTable
)

// guardDeclarations are the sets of the guard table: each card's port, MAC
// and address, and its port and MAC for the frames a card sends before it
// has its address (a DHCP request, an ARP probe).
var guardDeclarations = []declaration{
	{name: setCards, key: []string{"ifname", "ether_addr", "ipv4_addr"}, decl: "type ifname . ether_addr . ipv4_addr"},
	{name: setCardMACs, key: []string{"ifname", "ether_addr"}, decl: "type ifname . ether_addr"},
}

// guardChain lets everything pass that is not on the managed bridge, and what
// the leg sends; of the rest, IPv4 and ARP pass only from a card's port with
// its MAC and address, or with no address yet as DHCP and ARP probes have it.
// Other protocols pass: IPv6 is off on the bridge and in the namespace, and
// the guests are isolated from each other by the ports.
const guardChain = `	chain prerouting {
		type filter hook prerouting priority -300; policy accept;
		meta ibrname != "@BRIDGE@" accept
		iifname "@LEGPORTS@" accept
		iifname . ether saddr . ip saddr @cards accept
		ip saddr 0.0.0.0 udp sport 68 udp dport 67 iifname . ether saddr @card_macs accept
		iifname . arp saddr ether . arp saddr ip @cards accept
		arp saddr ip 0.0.0.0 iifname . arp saddr ether @card_macs accept
		ether type { ip, arp } counter name "dropped_spoofed" drop
	}
`

// guardListedRules are the rules of the chain as nft -j lists them, nft 1.0.6
// and 1.1.3 alike.
const guardListedRules = `[
[{"match":{"op":"!=","left":{"meta":{"key":"ibrname"}},"right":"@BRIDGE@"}},{"accept":null}],
[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"@LEGPORTS@"}},{"accept":null}],
[{"match":{"op":"==","left":{"concat":[{"meta":{"key":"iifname"}},{"payload":{"protocol":"ether","field":"saddr"}},{"payload":{"protocol":"ip","field":"saddr"}}]},"right":"@cards"}},{"accept":null}],
[{"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"saddr"}},"right":"0.0.0.0"}},{"match":{"op":"==","left":{"payload":{"protocol":"udp","field":"sport"}},"right":68}},{"match":{"op":"==","left":{"payload":{"protocol":"udp","field":"dport"}},"right":67}},{"match":{"op":"==","left":{"concat":[{"meta":{"key":"iifname"}},{"payload":{"protocol":"ether","field":"saddr"}}]},"right":"@card_macs"}},{"accept":null}],
[{"match":{"op":"==","left":{"concat":[{"meta":{"key":"iifname"}},{"payload":{"protocol":"arp","field":"saddr ether"}},{"payload":{"protocol":"arp","field":"saddr ip"}}]},"right":"@cards"}},{"accept":null}],
[{"match":{"op":"==","left":{"payload":{"protocol":"arp","field":"saddr ip"}},"right":"0.0.0.0"}},{"match":{"op":"==","left":{"concat":[{"meta":{"key":"iifname"}},{"payload":{"protocol":"arp","field":"saddr ether"}}]},"right":"@card_macs"}},{"accept":null}],
[{"match":{"op":"==","left":{"payload":{"protocol":"ether","field":"type"}},"right":{"set":["ip","arp"]}}},{"counter":"dropped_spoofed"},{"drop":null}]
]`

// withGuardNames fills in the managed bridge and the names of the bridge
// ends of the legs, which all begin with vethpco.
func withGuardNames(s string) string {
	return strings.NewReplacer("@BRIDGE@", ManagedBridge, "@LEGPORTS@", bridgePortPrefix+"*").Replace(s)
}

// guardElements are the elements of the guard's sets for the sticky entries:
// those of a card on the managed bridge with its port and its address.
func guardElements(sticky []Sticky) elements {
	e := elements{}
	for _, s := range sticky {
		if s.Bridge != ManagedBridge || s.Port == "" || len(s.MAC) != 6 || !s.Addr.Is4() {
			continue
		}
		e[setCards] = append(e[setCards], fmt.Sprintf("%s . %s . %s", quoted(s.Port), s.MAC, s.Addr))
		e[setCardMACs] = append(e[setCardMACs], fmt.Sprintf("%s . %s", quoted(s.Port), s.MAC))
	}
	for name, elems := range e {
		e[name] = slices.Compact(slices.Sorted(slices.Values(elems)))
	}
	return e
}

// renderGuard returns the script that replaces the guard table with one that
// holds e, in one transaction.
func renderGuard(e elements) string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table bridge %[1]s\ndelete table bridge %[1]s\ntable bridge %[1]s {\n", guardTable)
	writeDeclarations(&b, guardDeclarations, e)
	fmt.Fprintf(&b, "\tcounter %s {\n\t}\n", counterSpoofed)
	b.WriteString(withGuardNames(guardChain))
	b.WriteString("}\n")
	return b.String()
}

// guardDifferences says what of a listing of the guard table is not as pco
// loads it with e.
func guardDifferences(l *listing, e elements) []string {
	prio := json.Number("-300")
	hooks := map[string]listedChain{"prerouting": {Type: "filter", Hook: "prerouting", Prio: &prio, Policy: "accept"}}
	rules := wantRules(map[string]string{"prerouting": withGuardNames(guardListedRules)})
	d := tableDifferences(l, "table "+guardFamilyName, hooks, rules, guardDeclarations, []string{counterSpoofed})
	have := listedElements(l)
	for _, name := range []string{setCards, setCardMACs} {
		for _, el := range e[name] {
			if !slices.Contains(have[name], el) {
				d = append(d, fmt.Sprintf("%s of %s lacks %s", name, guardTable, el))
			}
		}
		for _, el := range have[name] {
			if !slices.Contains(e[name], el) {
				d = append(d, fmt.Sprintf("%s of %s holds %s", name, guardTable, el))
			}
		}
	}
	return d
}

// guarded reports whether the guard is wanted: while a leg into the managed
// network is given.
func guarded(legs []Leg) bool {
	return slices.ContainsFunc(legs, func(l Leg) bool { return l.Managed && l.Bridge == ManagedBridge })
}

// ensureGuard loads the guard table with the sticky entries' cards when it
// is not so, or takes it away when no managed leg is given.
func (b *hostBackend) ensureGuard(ctx context.Context) error {
	if !guarded(b.legs) {
		return b.removeGuard(ctx)
	}
	want := guardElements(b.sticky)
	l, err := b.listGuard(ctx)
	if err != nil {
		return err
	}
	if l != nil && len(guardDifferences(l, want)) == 0 {
		return nil
	}
	if err := b.hostNft.Apply(ctx, renderGuard(want)); err != nil {
		return fmt.Errorf("loading table bridge %s: %w", guardTable, err)
	}
	return nil
}

// listGuard reads the guard table: nil when it is missing or cannot be read,
// which loading it again mends.
func (b *hostBackend) listGuard(ctx context.Context) (*listing, error) {
	raw, err := b.hostNft.List(ctx, guardFamilyName)
	switch {
	case errors.Is(err, ErrNotLoaded):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("listing table bridge %s: %w", guardTable, err)
	}
	l, err := parseListing(raw)
	if err != nil {
		b.log.Warn().Err(err).Str("table", guardTable).Msg("the listing of the table cannot be read; loading it again")
		return nil, nil
	}
	return l, nil
}

func (b *hostBackend) removeGuard(ctx context.Context) error {
	l, err := b.listGuard(ctx)
	if err != nil || l == nil {
		return err
	}
	if err := b.hostNft.Apply(ctx, fmt.Sprintf("add table bridge %[1]s\ndelete table bridge %[1]s\n", guardTable)); err != nil {
		return fmt.Errorf("removing table bridge %s: %w", guardTable, err)
	}
	return nil
}

// guard compares the guard table.
func (v *verifier) guard(ctx context.Context) {
	if v.err != nil {
		return
	}
	if !guarded(v.b.legs) {
		return
	}
	raw, err := v.b.hostNft.List(ctx, guardFamilyName)
	switch {
	case errors.Is(err, ErrNotLoaded):
		v.differs("table bridge %s is missing", guardTable)
		return
	case err != nil:
		v.fail(fmt.Errorf("listing table bridge %s: %w", guardTable, err))
		return
	}
	l, err := parseListing(raw)
	if err != nil {
		v.differs("table bridge %s cannot be read: %v", guardTable, err)
		return
	}
	v.d = append(v.d, guardDifferences(l, guardElements(v.b.sticky))...)
}
