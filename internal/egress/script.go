package egress

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

const (
	tableName = "pco_egress"
	table     = "inet " + tableName

	chainOutput    = "output"
	chainConnector = "connector"

	setTargets4   = "targets4"
	setTargets6   = "targets6"
	setResolvers4 = "resolvers4"
	setResolvers6 = "resolvers6"

	typeTarget4   = "ipv4_addr . inet_service"
	typeTarget6   = "ipv6_addr . inet_service"
	typeResolver4 = "ipv4_addr"
	typeResolver6 = "ipv6_addr"

	counterLocal = "rejected_local"
	counterOther = "rejected"

	// priority puts the chain after conntrack (-200), whose state the rules
	// read, and after the destination NAT of the output hook (-100), so that
	// the address checked is the one the packet goes to; and before the
	// filter chains at 0, among them those of the Proxmox firewall. An accept
	// in another base chain does not end the walk through the hook, so none
	// of them can let a connector's packet past this chain; running first
	// only makes the reject the answer the connector sees.
	priority = -10

	// The rules after the declaration of the sets. A connector's packet is
	// one whose socket belongs to the connector user; every other packet,
	// including those the kernel sends without a socket, for which meta skuid
	// matches nothing, leaves the output chain under its policy.
	chains = `	chain output {
		type filter hook output priority filter - 10; policy accept;
		meta skuid %d jump connector
	}
	chain connector {
		ct state invalid drop
		ct direction reply accept
		ip daddr . tcp dport @targets4 accept
		ip6 daddr . tcp dport @targets6 accept
		ip daddr @resolvers4 meta l4proto { tcp, udp } th dport 53 accept
		ip6 daddr @resolvers6 meta l4proto { tcp, udp } th dport 53 accept
		fib daddr type local counter name "rejected_local" reject with icmpx admin-prohibited
		ip daddr != { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16, 100.64.0.0/10, 127.0.0.0/8, 198.18.0.0/15 } meta l4proto { tcp, udp } th dport 7844 accept
		ip6 daddr != { fc00::/7, fe80::/10, ::1 } meta l4proto { tcp, udp } th dport 7844 accept
		ip daddr { 1.1.1.1, 1.0.0.1 } tcp dport 853 accept
		counter name "rejected" reject with icmpx admin-prohibited
	}
}
`
)

// contents is what the sets of the table hold.
type contents struct {
	targets   []Target     // sorted and distinct, of both families
	resolvers []netip.Addr // sorted and distinct, of both families
}

func (c contents) equal(o contents) bool {
	return slices.Equal(c.targets, o.targets) && slices.Equal(c.resolvers, o.resolvers)
}

// Base renders the table with empty sets: what is loaded at boot. Until the
// daemon fills the sets, a connector reaches nothing but Cloudflare's edge.
func Base(connectorUID uint32) string { return render(connectorUID, contents{}) }

// render returns the script that replaces the table, whatever it holds, with
// one that holds c, in one transaction: the add makes the delete work when
// there is no table yet, and the delete takes the chains, sets and elements
// that others may have added, which a flush of the table would leave.
func render(uid uint32, c contents) string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table %s\ndelete table %s\ntable %s {\n", table, table, table)
	fmt.Fprintf(&b, "\tcounter %s {\n\t}\n\tcounter %s {\n\t}\n", counterLocal, counterOther)
	t4, t6 := targetElements(c.targets)
	r4, r6 := addrElements(c.resolvers)
	writeSet(&b, setTargets4, typeTarget4, t4)
	writeSet(&b, setTargets6, typeTarget6, t6)
	writeSet(&b, setResolvers4, typeResolver4, r4)
	writeSet(&b, setResolvers6, typeResolver6, r6)
	fmt.Fprintf(&b, chains, uid)
	return b.String()
}

func writeSet(b *strings.Builder, name, typ string, elems []string) {
	fmt.Fprintf(b, "\tset %s {\n\t\ttype %s\n", name, typ)
	if len(elems) > 0 {
		fmt.Fprintf(b, "\t\telements = {\n\t\t\t%s\n\t\t}\n", strings.Join(elems, ",\n\t\t\t"))
	}
	b.WriteString("\t}\n")
}

// deleteScript returns the script that takes the given targets and resolvers
// out of the live table, one statement per set, in one transaction.
func deleteScript(tg []Target, rs []netip.Addr) string {
	var b strings.Builder
	t4, t6 := targetElements(tg)
	r4, r6 := addrElements(rs)
	for _, s := range []struct {
		name  string
		elems []string
	}{{setTargets4, t4}, {setTargets6, t6}, {setResolvers4, r4}, {setResolvers6, r6}} {
		if len(s.elems) > 0 {
			fmt.Fprintf(&b, "delete element %s %s { %s }\n", table, s.name, strings.Join(s.elems, ", "))
		}
	}
	return b.String()
}

// targetElements writes targets as set elements, by family. They are written
// from parsed addresses, never from text that came from elsewhere.
func targetElements(tg []Target) (v4, v6 []string) {
	for _, t := range tg {
		e := t.Addr.String() + " . " + strconv.Itoa(int(t.Port))
		if t.Addr.Is4() {
			v4 = append(v4, e)
		} else {
			v6 = append(v6, e)
		}
	}
	return v4, v6
}

func addrElements(addrs []netip.Addr) (v4, v6 []string) {
	for _, a := range addrs {
		if a.Is4() {
			v4 = append(v4, a.String())
		} else {
			v6 = append(v6, a.String())
		}
	}
	return v4, v6
}
