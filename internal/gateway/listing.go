package gateway

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
)

// errUnreadable is a listing that is not what nft prints for a table.
var errUnreadable = errors.New("the listing cannot be read")

// listing is a table as nft -j lists it, read into what is compared: its
// flags, its chains with their hooks, the rules of each chain in order and in
// a canonical form, its sets and maps with their elements, its counters, and
// whatever else is there.
type listing struct {
	flags      string
	chains     map[string]listedChain
	rules      map[string][]string
	sets       map[string]listedSet
	counters   map[string]bool
	unexpected []string
}

type listedChain struct {
	Type   string       `json:"type"`
	Hook   string       `json:"hook"`
	Prio   *json.Number `json:"prio"`
	Policy string       `json:"policy"`
}

func (c listedChain) String() string {
	prio := "none"
	if c.Prio != nil {
		prio = c.Prio.String()
	}
	return fmt.Sprintf("%s hook %s priority %s policy %s", c.Type, c.Hook, prio, c.Policy)
}

// listedSet is a set or a map. nft 1.1 lists the types of a set declared with
// typeof as the expressions, 1.0 as the data types; both are read into the
// data types.
type listedSet struct {
	isMap bool
	key   []string
	data  []string
	flags []string
	elems []string // as a script writes them, sorted
}

func (s listedSet) String() string {
	d := strings.Join(s.key, " . ")
	if s.isMap {
		d = "map " + d + " : " + strings.Join(s.data, " . ")
	}
	if len(s.flags) > 0 {
		d += " flags " + strings.Join(s.flags, ",")
	}
	return d
}

func parseListing(raw []byte) (*listing, error) {
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%w: %w", errUnreadable, err)
	}
	if doc.Nftables == nil {
		return nil, fmt.Errorf("%w: no nftables array", errUnreadable)
	}
	l := &listing{
		chains: map[string]listedChain{}, rules: map[string][]string{},
		sets: map[string]listedSet{}, counters: map[string]bool{},
	}
	for _, entry := range doc.Nftables {
		for _, kind := range slices.Sorted(maps.Keys(entry)) {
			if err := l.add(kind, entry[kind]); err != nil {
				return nil, fmt.Errorf("%w: %s: %w", errUnreadable, kind, err)
			}
		}
	}
	return l, nil
}

func (l *listing) add(kind string, body json.RawMessage) error {
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
	case "table":
		l.flags = strings.Join(stringList(named.Flags), ",")
	case "chain":
		var c listedChain
		if err := json.Unmarshal(body, &c); err != nil {
			return err
		}
		l.chains[named.Name] = c
	case "rule":
		r, err := canonical(named.Expr)
		if err != nil {
			return err
		}
		l.rules[named.Chain] = append(l.rules[named.Chain], r)
	case "set", "map":
		s, err := parseSet(kind == "map", body)
		if err != nil {
			return fmt.Errorf("%s %s: %w", kind, named.Name, err)
		}
		l.sets[named.Name] = s
	case "counter":
		l.counters[named.Name] = true
	default:
		l.unexpected = append(l.unexpected, strings.TrimSpace("an unexpected "+kind+" "+named.Name))
	}
	return nil
}

func parseSet(isMap bool, body json.RawMessage) (listedSet, error) {
	var raw struct {
		Type  json.RawMessage   `json:"type"`
		Map   json.RawMessage   `json:"map"`
		Flags json.RawMessage   `json:"flags"`
		Elem  []json.RawMessage `json:"elem"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return listedSet{}, err
	}
	s := listedSet{isMap: isMap, flags: stringList(raw.Flags)}
	var err error
	if s.key, err = dataTypes(raw.Type); err != nil {
		return listedSet{}, err
	}
	if isMap {
		if s.data, err = dataTypes(raw.Map); err != nil {
			return listedSet{}, err
		}
	}
	for _, e := range raw.Elem {
		text, err := elementText(e)
		if err != nil {
			return listedSet{}, fmt.Errorf("an element pco cannot read: %s: %w", compactJSON(e), err)
		}
		s.elems = append(s.elems, text)
	}
	slices.Sort(s.elems)
	return s, nil
}

// stringList reads a string or a list of strings; anything else is empty.
func stringList(raw json.RawMessage) []string {
	var one string
	var several []string
	switch {
	case len(raw) == 0:
		return nil
	case json.Unmarshal(raw, &one) == nil:
		if one == "" {
			return nil
		}
		return []string{one}
	case json.Unmarshal(raw, &several) == nil:
		return several
	}
	return []string{string(raw)}
}

// dataTypes reads the type of a key or of map data: "ipv4_addr . inet_service",
// a list of the parts, or the expression typeof declared it with.
func dataTypes(raw json.RawMessage) ([]string, error) {
	var typeOf struct {
		Typeof json.RawMessage `json:"typeof"`
	}
	if json.Unmarshal(raw, &typeOf) == nil && typeOf.Typeof != nil {
		return exprTypes(typeOf.Typeof)
	}
	var out []string
	for _, part := range stringList(raw) {
		out = append(out, strings.Split(part, " . ")...)
	}
	return out, nil
}

// exprTypes returns the data types of what a typeof expression reads.
func exprTypes(raw json.RawMessage) ([]string, error) {
	var e struct {
		Concat  []json.RawMessage `json:"concat"`
		Payload *struct {
			Protocol string `json:"protocol"`
			Field    string `json:"field"`
		} `json:"payload"`
		Meta *struct {
			Key string `json:"key"`
		} `json:"meta"`
		CT *struct {
			Key string `json:"key"`
		} `json:"ct"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, err
	}
	switch {
	case e.Concat != nil:
		var out []string
		for _, part := range e.Concat {
			t, err := exprTypes(part)
			if err != nil {
				return nil, err
			}
			out = append(out, t...)
		}
		return out, nil
	case e.Payload != nil && e.Payload.Protocol == "ip" && (e.Payload.Field == "daddr" || e.Payload.Field == "saddr"):
		return []string{"ipv4_addr"}, nil
	case e.Payload != nil && (e.Payload.Field == "dport" || e.Payload.Field == "sport"):
		return []string{"inet_service"}, nil
	case e.Meta != nil && (e.Meta.Key == "iifname" || e.Meta.Key == "oifname"):
		return []string{"ifname"}, nil
	case e.Meta != nil && e.Meta.Key == "mark":
		return []string{"mark"}, nil
	case e.CT != nil && e.CT.Key == "zone":
		return []string{"integer"}, nil
	}
	return []string{"typeof " + compactJSON(raw)}, nil
}

// elementText writes an element as a script does: a map element as its key
// and its value around " : ", a concatenation with " . ", a range with "-",
// an interface name quoted.
func elementText(raw json.RawMessage) (string, error) {
	var wrapped struct {
		Elem *struct {
			Val json.RawMessage `json:"val"`
		} `json:"elem"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && wrapped.Elem != nil {
		raw = wrapped.Elem.Val
	}
	var pair []json.RawMessage
	if json.Unmarshal(raw, &pair) == nil && len(pair) == 2 {
		k, err := valueText(pair[0])
		if err != nil {
			return "", err
		}
		v, err := valueText(pair[1])
		return k + " : " + v, err
	}
	return valueText(raw)
}

func valueText(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	return valueOf(v)
}

func valueOf(v any) (string, error) {
	switch x := v.(type) {
	case string:
		if _, err := netip.ParseAddr(x); err == nil {
			return x, nil
		}
		// A MAC, as nft lists one, is no interface name: those are shorter.
		if m, err := net.ParseMAC(x); err == nil && len(m) == 6 && m.String() == x {
			return x, nil
		}
		return quoted(x), nil
	case json.Number:
		return x.String(), nil
	case map[string]any:
		if parts, ok := x["concat"].([]any); ok && len(x) == 1 {
			out := make([]string, 0, len(parts))
			for _, p := range parts {
				s, err := valueOf(p)
				if err != nil {
					return "", err
				}
				out = append(out, s)
			}
			return strings.Join(out, " . "), nil
		}
		if r, ok := x["range"].([]any); ok && len(x) == 1 && len(r) == 2 {
			lo, err := valueOf(r[0])
			if err != nil {
				return "", err
			}
			hi, err := valueOf(r[1])
			return lo + "-" + hi, err
		}
	}
	return "", errors.New("not a value of the table")
}

// tableDifferences says what of a listing is not as want: its flags, its
// chains and their rules, its sets and maps (not their elements), its
// counters. of names the table.
func tableDifferences(l *listing, of string, chains map[string]listedChain, rules map[string][]string,
	sets []declaration, counters []string,
) []string {
	var d []string
	if l.flags != "" {
		// A dormant table holds its rules and applies none.
		d = append(d, of+" has flags "+l.flags)
	}
	for _, u := range l.unexpected {
		d = append(d, of+" holds "+u)
	}
	for _, name := range slices.Sorted(maps.Keys(l.chains)) {
		if _, ok := chains[name]; !ok {
			d = append(d, of+" holds an unexpected chain "+name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(chains)) {
		got, ok := l.chains[name]
		switch {
		case !ok:
			d = append(d, of+" has no chain "+name)
		case got.String() != chains[name].String():
			d = append(d, fmt.Sprintf("chain %s of %s is %s, want %s", name, of, got, chains[name]))
		default:
			d = append(d, ruleDifferences(of, name, l.rules[name], rules[name])...)
		}
	}
	d = append(d, setDifferences(l, of, sets)...)
	for _, name := range slices.Sorted(maps.Keys(l.counters)) {
		if !slices.Contains(counters, name) {
			d = append(d, of+" holds an unexpected counter "+name)
		}
	}
	for _, name := range counters {
		if !l.counters[name] {
			d = append(d, of+" has no counter "+name)
		}
	}
	return d
}

func ruleDifferences(of, chain string, got, want []string) []string {
	if len(got) != len(want) {
		return []string{fmt.Sprintf("chain %s of %s has %d rules, want %d", chain, of, len(got), len(want))}
	}
	for i := range want {
		if got[i] != want[i] {
			return []string{fmt.Sprintf("chain %s of %s: rule %d is not the one pco loads", chain, of, i+1)}
		}
	}
	return nil
}

func setDifferences(l *listing, of string, sets []declaration) []string {
	var d []string
	for _, name := range slices.Sorted(maps.Keys(l.sets)) {
		if !slices.ContainsFunc(sets, func(s declaration) bool { return s.name == name }) {
			d = append(d, of+" holds an unexpected set "+name)
		}
	}
	for _, s := range sets {
		got, ok := l.sets[s.name]
		want := listedSet{isMap: s.isMap, key: s.key, data: s.data, flags: s.flags}
		switch {
		case !ok:
			d = append(d, of+" has no set "+s.name)
		case got.String() != want.String():
			d = append(d, fmt.Sprintf("set %s of %s is %s, want %s", s.name, of, got, want))
		}
	}
	return d
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

// wantRules returns the canonical form of each rule of each chain, from the
// rules as nft -j lists them. Text that is no list of rules stands for one
// rule that no listing holds, so that the chain is never taken as loaded.
func wantRules(chains map[string]string) map[string][]string {
	out := make(map[string][]string, len(chains))
	for name, text := range chains {
		var exprs []json.RawMessage
		if err := json.Unmarshal([]byte(text), &exprs); err != nil {
			out[name] = []string{"unreadable: " + err.Error()}
			continue
		}
		for _, e := range exprs {
			r, err := canonical(e)
			if err != nil {
				r = "unreadable: " + err.Error()
			}
			out[name] = append(out[name], r)
		}
	}
	return out
}
