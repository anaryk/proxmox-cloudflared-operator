package egress

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
)

// The rules of the two chains as nft -j lists them, in order: what a listing
// is compared with. The first takes the uid of the connector user. The script
// in script.go says the same in the language of nft; the tests hold the two
// against listings that nft 1.0.6 and 1.1.3 printed.
const (
	wantOutputRules = `[
[{"match":{"op":"==","left":{"meta":{"key":"skuid"}},"right":%d}},{"jump":{"target":"connector"}}]
]`
	wantConnectorRules = `[
[{"match":{"op":"in","left":{"ct":{"key":"state"}},"right":"invalid"}},{"drop":null}],
[{"match":{"op":"==","left":{"ct":{"key":"direction"}},"right":"reply"}},{"accept":null}],
[{"match":{"op":"==","left":{"concat":[{"payload":{"protocol":"ip","field":"daddr"}},{"payload":{"protocol":"tcp","field":"dport"}}]},"right":"@targets4"}},{"accept":null}],
[{"match":{"op":"==","left":{"concat":[{"payload":{"protocol":"ip6","field":"daddr"}},{"payload":{"protocol":"tcp","field":"dport"}}]},"right":"@targets6"}},{"accept":null}],
[{"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"daddr"}},"right":"@resolvers4"}},{"match":{"op":"==","left":{"meta":{"key":"l4proto"}},"right":{"set":[6,17]}}},{"match":{"op":"==","left":{"payload":{"protocol":"th","field":"dport"}},"right":53}},{"accept":null}],
[{"match":{"op":"==","left":{"payload":{"protocol":"ip6","field":"daddr"}},"right":"@resolvers6"}},{"match":{"op":"==","left":{"meta":{"key":"l4proto"}},"right":{"set":[6,17]}}},{"match":{"op":"==","left":{"payload":{"protocol":"th","field":"dport"}},"right":53}},{"accept":null}],
[{"match":{"op":"==","left":{"fib":{"result":"type","flags":["daddr"]}},"right":"local"}},{"counter":"rejected_local"},{"reject":{"type":"icmpx","expr":"admin-prohibited"}}],
[{"match":{"op":"!=","left":{"payload":{"protocol":"ip","field":"daddr"}},"right":{"set":[{"prefix":{"addr":"10.0.0.0","len":8}},{"prefix":{"addr":"100.64.0.0","len":10}},{"prefix":{"addr":"127.0.0.0","len":8}},{"prefix":{"addr":"169.254.0.0","len":16}},{"prefix":{"addr":"172.16.0.0","len":12}},{"prefix":{"addr":"192.168.0.0","len":16}},{"prefix":{"addr":"198.18.0.0","len":15}}]}}},{"match":{"op":"==","left":{"meta":{"key":"l4proto"}},"right":{"set":[6,17]}}},{"match":{"op":"==","left":{"payload":{"protocol":"th","field":"dport"}},"right":7844}},{"accept":null}],
[{"match":{"op":"!=","left":{"payload":{"protocol":"ip6","field":"daddr"}},"right":{"set":["::1",{"prefix":{"addr":"fc00::","len":7}},{"prefix":{"addr":"fe80::","len":10}}]}}},{"match":{"op":"==","left":{"meta":{"key":"l4proto"}},"right":{"set":[6,17]}}},{"match":{"op":"==","left":{"payload":{"protocol":"th","field":"dport"}},"right":7844}},{"accept":null}],
[{"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"daddr"}},"right":{"set":["1.0.0.1","1.1.1.1"]}}},{"match":{"op":"==","left":{"payload":{"protocol":"tcp","field":"dport"}},"right":853}},{"accept":null}],
[{"counter":"rejected"},{"reject":{"type":"icmpx","expr":"admin-prohibited"}}]
]`
)

// Counter is what a counter of the table counted since the table was loaded.
type Counter struct {
	Packets uint64 `json:"packets"`
	Bytes   uint64 `json:"bytes"`
}

// Live is what the table loaded on the node holds.
type Live struct {
	Targets       []Target
	Resolvers     []netip.Addr
	RejectedLocal Counter // packets to an address of the node
	Rejected      Counter // packets to anything else no rule allows
	// Differences says what of the table, but the elements of its sets, is
	// not as pco loads it; a table with any is not to be trusted.
	Differences []string
}

// ReadLive reads the table loaded on the node, or returns ErrNotLoaded, or
// ErrUnreadable for a listing it cannot read.
func ReadLive(ctx context.Context, n Nft, connectorUID uint32) (Live, error) {
	l, err := list(ctx, n)
	if err != nil {
		return Live{}, err
	}
	c, unreadable := l.contents()
	if len(unreadable) > 0 {
		return Live{}, fmt.Errorf("%w: %s", ErrUnreadable, unreadable[0])
	}
	return Live{
		Targets: c.targets, Resolvers: c.resolvers,
		RejectedLocal: l.counters[counterLocal], Rejected: l.counters[counterOther],
		Differences: l.differences(connectorUID, nil),
	}, nil
}

// Load loads Base with the resolvers and blocked addresses given, unless the
// table is there with everything but the elements as it should be: the sets
// of a table the daemon filled stay as they are. It reports whether it loaded
// the table.
func Load(ctx context.Context, n Nft, connectorUID uint32, resolvers, blocked []netip.Addr) (bool, error) {
	l, err := list(ctx, n)
	switch {
	case errors.Is(err, ErrNotLoaded), errors.Is(err, ErrUnreadable):
	case err != nil:
		return false, err
	case len(l.differences(connectorUID, nil)) == 0:
		return false, nil
	}
	if err := n.Apply(ctx, Base(connectorUID, resolvers, blocked)); err != nil {
		return false, fmt.Errorf("loading the egress table: %w", err)
	}
	return true, nil
}

// Unload removes the table, if there is one.
func Unload(ctx context.Context, n Nft) error {
	if err := n.Apply(ctx, fmt.Sprintf("add table %s\ndelete table %s\n", table, table)); err != nil {
		return fmt.Errorf("removing the egress table: %w", err)
	}
	return nil
}

// Drop takes every element of an address out of the sets of the live table,
// targets and resolvers alike, and returns how many it took out. It is what
// pco egress block does at once; the next script the daemon renders leaves
// the address out by itself.
func Drop(ctx context.Context, n Nft, addr netip.Addr) (int, error) {
	addr = normalizeAddr(addr)
	l, err := list(ctx, n)
	if err != nil {
		return 0, err
	}
	c, _ := l.contents()
	tg := slices.DeleteFunc(c.targets, func(t Target) bool { return t.Addr != addr })
	rs := slices.DeleteFunc(c.resolvers, func(a netip.Addr) bool { return a != addr })
	if len(tg)+len(rs) == 0 {
		return 0, nil
	}
	if err := n.Apply(ctx, deleteScript(tg, rs)); err != nil {
		return 0, fmt.Errorf("taking %s out of the egress table: %w", addr, err)
	}
	return len(tg) + len(rs), nil
}

// ErrUnreadable is a listing that is not what nft prints for the table. nft
// 1.0.6 can print one for a table with flags.
var ErrUnreadable = errors.New("the listing of the egress table cannot be read")

func list(ctx context.Context, n Nft) (*listed, error) {
	raw, err := n.List(ctx)
	if err != nil {
		return nil, err
	}
	return parseListing(raw)
}

// listed is a listing of the table, read into what is compared: the chains
// with their hooks, the rules of each chain in order and in a canonical form,
// the sets with their elements, the counters, and whatever else is there.
type listed struct {
	flags      string // of the table, as listed; dormant is one
	chains     map[string]listedChain
	rules      map[string][]string
	sets       map[string]listedSet
	counters   map[string]Counter
	unexpected []string
}

type listedChain struct {
	Type   string       `json:"type"`
	Hook   string       `json:"hook"`
	Prio   *json.Number `json:"prio"`
	Policy string       `json:"policy"`
}

func (c listedChain) String() string {
	if c.Hook == "" && c.Type == "" {
		return "a regular chain"
	}
	prio := "none"
	if c.Prio != nil {
		prio = c.Prio.String()
	}
	return fmt.Sprintf("%s hook %s priority %s policy %s", c.Type, c.Hook, prio, c.Policy)
}

type listedSet struct {
	Type  json.RawMessage   `json:"type"`
	Flags json.RawMessage   `json:"flags"`
	Elem  []json.RawMessage `json:"elem"`
}

func (s listedSet) String() string {
	d := compactJSON(s.Type)
	if len(s.Flags) > 0 {
		d += " with flags " + compactJSON(s.Flags)
	}
	return d
}

func parseListing(raw []byte) (*listed, error) {
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	if doc.Nftables == nil {
		return nil, fmt.Errorf("%w: no nftables array", ErrUnreadable)
	}
	l := &listed{
		chains: map[string]listedChain{}, rules: map[string][]string{},
		sets: map[string]listedSet{}, counters: map[string]Counter{},
	}
	for _, entry := range doc.Nftables {
		for kind, body := range entry {
			if err := l.add(kind, body); err != nil {
				return nil, fmt.Errorf("%w: %s: %w", ErrUnreadable, kind, err)
			}
		}
	}
	return l, nil
}

func (l *listed) add(kind string, body json.RawMessage) error {
	var named struct {
		Name  string          `json:"name"`
		Chain string          `json:"chain"`
		Expr  json.RawMessage `json:"expr"`
		Flags json.RawMessage `json:"flags"`
	}
	if err := json.Unmarshal(body, &named); err != nil {
		return err
	}
	switch kind {
	case "metainfo":
		return nil
	case "table":
		// Any flag counts, whatever it is called: nft 1.0.6 prints the
		// dormant flag as some other word.
		switch f := compactJSON(named.Flags); f {
		case "?", `""`, "[]", "null", "0":
		default:
			l.flags = f
		}
		return nil
	case "chain":
		var c listedChain
		err := json.Unmarshal(body, &c)
		l.chains[named.Name] = c
		return err
	case "rule":
		r, err := canonical(named.Expr)
		l.rules[named.Chain] = append(l.rules[named.Chain], r)
		return err
	case "set":
		var s listedSet
		err := json.Unmarshal(body, &s)
		l.sets[named.Name] = s
		return err
	case "counter":
		var c Counter
		err := json.Unmarshal(body, &c)
		l.counters[named.Name] = c
		return err
	}
	l.unexpected = append(l.unexpected, strings.TrimSpace("an unexpected "+kind+" "+named.Name))
	return nil
}

// setType returns the type of each set of the table, by name.
func setType(name string) (string, bool) {
	switch name {
	case setTargets4:
		return typeTarget4, true
	case setTargets6:
		return typeTarget6, true
	case setResolvers4:
		return typeResolver4, true
	case setResolvers6:
		return typeResolver6, true
	}
	return "", false
}

// differences says what in the listing is not as the table should be: its
// chains, their rules and sets and counters, and, unless want is nil, the
// elements of its sets.
func (l *listed) differences(uid uint32, want *contents) []string {
	var d []string
	if l.flags != "" {
		// A dormant table holds every rule and filters nothing.
		d = append(d, "the table has flags "+l.flags)
	}
	d = append(d, l.unexpected...)
	for _, name := range slices.Sorted(maps.Keys(l.chains)) {
		if name != chainOutput && name != chainConnector {
			d = append(d, "an unexpected chain "+name)
		}
	}
	prio := json.Number(fmt.Sprint(priority))
	for _, c := range []struct {
		name string
		want listedChain
	}{
		{chainOutput, listedChain{Type: "filter", Hook: "output", Prio: &prio, Policy: "accept"}},
		{chainConnector, listedChain{}},
	} {
		got, ok := l.chains[c.name]
		switch {
		case !ok:
			d = append(d, "no chain "+c.name)
		case got.String() != c.want.String():
			d = append(d, fmt.Sprintf("chain %s is %s, want %s", c.name, got, c.want))
		}
	}
	d = append(d, l.ruleDifferences(chainOutput, fmt.Sprintf(wantOutputRules, uid))...)
	d = append(d, l.ruleDifferences(chainConnector, wantConnectorRules)...)
	d = append(d, l.setDifferences(want)...)
	for _, name := range slices.Sorted(maps.Keys(l.counters)) {
		if name != counterLocal && name != counterOther {
			d = append(d, "an unexpected counter "+name)
		}
	}
	for _, name := range []string{counterLocal, counterOther} {
		if _, ok := l.counters[name]; !ok {
			d = append(d, "no counter "+name)
		}
	}
	return d
}

func (l *listed) ruleDifferences(chain, wantJSON string) []string {
	var exprs []json.RawMessage
	if err := json.Unmarshal([]byte(wantJSON), &exprs); err != nil {
		return []string{fmt.Sprintf("the rules of chain %s cannot be compared: %v", chain, err)}
	}
	got := l.rules[chain]
	if len(got) != len(exprs) {
		return []string{fmt.Sprintf("chain %s has %d rules, want %d", chain, len(got), len(exprs))}
	}
	for i, e := range exprs {
		want, err := canonical(e)
		if err != nil || got[i] != want {
			return []string{fmt.Sprintf("chain %s: rule %d differs", chain, i+1)}
		}
	}
	return nil
}

func (l *listed) setDifferences(want *contents) []string {
	var d []string
	for _, name := range slices.Sorted(maps.Keys(l.sets)) {
		if _, ok := setType(name); !ok {
			d = append(d, "an unexpected set "+name)
		}
	}
	for _, name := range []string{setTargets4, setTargets6, setResolvers4, setResolvers6} {
		typ, _ := setType(name)
		got, ok := l.sets[name]
		wantSet := listedSet{Type: typeJSON(typ)}
		switch {
		case !ok:
			d = append(d, "no set "+name)
		case got.String() != wantSet.String():
			d = append(d, fmt.Sprintf("set %s is %s, want %s", name, got, wantSet))
		}
	}
	if want == nil {
		return d
	}
	have, unreadable := l.contents()
	d = append(d, unreadable...)
	d = append(d, elementDifferences(have.targets, want.targets, targetSet, Target.String)...)
	d = append(d, elementDifferences(have.resolvers, want.resolvers, resolverSet, netip.Addr.String)...)
	return d
}

// elementDifferences says which elements of the sets have is missing and
// which it holds beyond want. Both are sorted.
func elementDifferences[T comparable](have, want []T, set func(T) string, text func(T) string) []string {
	var d []string
	for _, w := range want {
		if !slices.Contains(have, w) {
			d = append(d, fmt.Sprintf("set %s lacks %s", set(w), text(w)))
		}
	}
	for _, h := range have {
		if !slices.Contains(want, h) {
			d = append(d, fmt.Sprintf("set %s holds %s", set(h), text(h)))
		}
	}
	return d
}

func targetSet(t Target) string {
	if t.Addr.Is4() {
		return setTargets4
	}
	return setTargets6
}

func resolverSet(a netip.Addr) string {
	if a.Is4() {
		return setResolvers4
	}
	return setResolvers6
}

// contents returns the elements of the sets, and says which elements could
// not be read as what their set holds.
func (l *listed) contents() (contents, []string) {
	var c contents
	var unreadable []string
	for _, name := range []string{setTargets4, setTargets6} {
		for _, raw := range l.sets[name].Elem {
			t, ok := parseTarget(raw)
			if !ok {
				unreadable = append(unreadable, fmt.Sprintf("set %s holds an element pco cannot read: %s", name, compactJSON(raw)))
				continue
			}
			c.targets = append(c.targets, t)
		}
	}
	for _, name := range []string{setResolvers4, setResolvers6} {
		for _, raw := range l.sets[name].Elem {
			a, ok := parseAddr(raw)
			if !ok {
				unreadable = append(unreadable, fmt.Sprintf("set %s holds an element pco cannot read: %s", name, compactJSON(raw)))
				continue
			}
			c.resolvers = append(c.resolvers, a)
		}
	}
	c.targets = sortTargets(c.targets)
	c.resolvers = normalizeAddrs(c.resolvers)
	return c, unreadable
}

// element returns the value of a set element: as it is, or out of the object
// nft writes for an element with fields of its own, such as a comment.
func element(raw json.RawMessage) json.RawMessage {
	var wrapped struct {
		Elem *struct {
			Val json.RawMessage `json:"val"`
		} `json:"elem"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && wrapped.Elem != nil {
		return wrapped.Elem.Val
	}
	return raw
}

func parseTarget(raw json.RawMessage) (Target, bool) {
	var v struct {
		Concat []json.RawMessage `json:"concat"`
	}
	if json.Unmarshal(element(raw), &v) != nil || len(v.Concat) != 2 {
		return Target{}, false
	}
	a, ok := parseAddr(v.Concat[0])
	var port uint16
	if !ok || json.Unmarshal(v.Concat[1], &port) != nil || port == 0 {
		return Target{}, false
	}
	return Target{Addr: a, Port: port}, true
}

func parseAddr(raw json.RawMessage) (netip.Addr, bool) {
	var s string
	if json.Unmarshal(element(raw), &s) != nil {
		return netip.Addr{}, false
	}
	a, err := netip.ParseAddr(s)
	return a, err == nil
}

// typeJSON is the type of a set as nft -j lists it: a string, or a list of
// the parts of a concatenation.
func typeJSON(typ string) json.RawMessage {
	parts := strings.Split(typ, " . ")
	var b []byte
	if len(parts) == 1 {
		b, _ = json.Marshal(parts[0])
	} else {
		b, _ = json.Marshal(parts)
	}
	return b
}

// canonical returns the expressions of a rule in a form in which the ways
// different versions of nft write the same rule are the same: one form of a
// match on flags, numbers for protocols, a list of one as the one, and the
// members of an anonymous set in one order.
func canonical(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	b, err := json.Marshal(canon(v))
	return string(b), err
}

func canon(v any) any {
	switch x := v.(type) {
	case []any:
		if len(x) == 1 {
			return canon(x[0])
		}
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = canon(e)
		}
		return out
	case map[string]any:
		if isL4proto(x["left"]) {
			x["right"] = protocolNumbers(x["right"])
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = canon(e)
		}
		if out["op"] == "in" {
			out["op"] = "=="
		}
		if members, ok := out["set"].([]any); ok {
			slices.SortFunc(members, func(a, b any) int { return cmp.Compare(jsonText(a), jsonText(b)) })
		}
		return out
	}
	return v
}

func isL4proto(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	meta, ok := m["meta"].(map[string]any)
	return ok && meta["key"] == "l4proto"
}

func protocolNumbers(v any) any {
	switch x := v.(type) {
	case string:
		switch x {
		case "tcp":
			return json.Number("6")
		case "udp":
			return json.Number("17")
		}
	case []any:
		for i, e := range x {
			x[i] = protocolNumbers(e)
		}
	case map[string]any:
		for k, e := range x {
			x[k] = protocolNumbers(e)
		}
	}
	return v
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// compactJSON writes raw on one line and cuts it short, for a message.
func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return "?"
	}
	const most = 80
	if s := b.String(); len(s) > most {
		return s[:most] + "..."
	}
	return b.String()
}
