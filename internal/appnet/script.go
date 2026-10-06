package appnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
)

const (
	tableName = "pco_net"
	chain     = "output"
	counter   = "leaked"

	// priority runs the chain before the egress filter's (filter - 10) and
	// after the destination NAT of the output hook: an address of the
	// prefix that NAT turned into another one is no longer the prefix's.
	priority = -20
)

// Script renders the table: chain output, type filter hook output priority
// filter - 20, the named counter leaked, and the rule that rejects everything
// sent to the service prefix but to an address of the appliance itself. It
// rejects rather than drops: a dropped request waits out cloudflared's origin
// timeout of 30 s, a rejected one fails at once. That takes the exception: a
// request through pco0 has the source ServiceSource, which is in the prefix,
// and the answer of the reject goes there. The add before the delete makes
// the delete work when there is no table; the delete takes whatever others
// added to it.
func Script() string {
	return fmt.Sprintf(`# The service prefix never leaves the appliance. The route through %[1]s keeps
# it off net0; this table rejects whatever is still sent to it, keyed on the
# address so that it holds without the route too. A request to the prefix
# fails at once, where a dropped one would wait out cloudflared's origin
# timeout. An address of the appliance itself, as %[7]s, is no way
# out, and the reject answers the request there.
add table %[2]s
delete table %[2]s
table %[2]s {
	counter %[3]s {
	}
	chain %[4]s {
		type filter hook output priority filter - %[5]d; policy accept;
		ip daddr %[6]s fib daddr type != local counter name %[3]s reject
	}
}
`, Device, Table, counter, chain, -priority, ServicePrefix, ServiceSource)
}

// wantRules are the forms nft lists the rule of Script in: 1.0.6 and 1.1.3
// name the type of the reject, which an older one may leave out.
func wantRules() []string {
	match := fmt.Sprintf(`{"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"daddr"}},`+
		`"right":{"prefix":{"addr":%q,"len":%d}}}},`+
		`{"match":{"op":"!=","left":{"fib":{"result":"type","flags":["daddr"]}},"right":"local"}}`,
		ServicePrefix.Addr(), ServicePrefix.Bits())
	var out []string
	for _, reject := range []string{`{"type":"icmp","expr":"port-unreachable"}`, `null`} {
		r, _ := canonical(json.RawMessage(fmt.Sprintf(`[%s,{"counter":%q},{"reject":%s}]`, match, counter, reject)))
		out = append(out, r)
	}
	return out
}

// listed is a listing of the table, read into what is compared.
type listed struct {
	flags      string
	chains     map[string]listedChain
	rules      map[string][]string
	counters   map[string]egress.Counter
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

var errUnreadable = errors.New("the listing cannot be read")

// list reads the table, or returns egress.ErrNotLoaded without one.
func list(ctx context.Context, nft egress.Nft) (*listed, error) {
	raw, err := nft.List(ctx)
	switch {
	case errors.Is(err, egress.ErrNotLoaded):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("listing the table %s: %w", Table, err)
	}
	return parse(raw)
}

func parse(raw []byte) (*listed, error) {
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%w: %w", errUnreadable, err)
	}
	if doc.Nftables == nil {
		return nil, fmt.Errorf("%w: no nftables array", errUnreadable)
	}
	l := &listed{chains: map[string]listedChain{}, rules: map[string][]string{}, counters: map[string]egress.Counter{}}
	for _, entry := range doc.Nftables {
		for _, kind := range slices.Sorted(maps.Keys(entry)) {
			if err := l.add(kind, entry[kind]); err != nil {
				return nil, fmt.Errorf("%w: %s: %w", errUnreadable, kind, err)
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
	case "table":
		l.flags = flagsText(named.Flags)
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
	case "counter":
		var c egress.Counter
		if err := json.Unmarshal(body, &c); err != nil {
			return err
		}
		l.counters[named.Name] = c
	default:
		l.unexpected = append(l.unexpected, strings.TrimSpace("an unexpected "+kind+" "+named.Name))
	}
	return nil
}

// flagsText is the flags of the table as nft lists them, empty for none.
// Any flag counts, whatever it is called: nft 1.0.6 prints the dormant flag
// as some other word.
func flagsText(raw json.RawMessage) string {
	var one string
	var several []string
	switch {
	case len(raw) == 0 || string(raw) == "null":
		return ""
	case json.Unmarshal(raw, &one) == nil:
		return one
	case json.Unmarshal(raw, &several) == nil:
		return strings.Join(several, ",")
	}
	return string(raw)
}

// canonical is a JSON value with its keys sorted and without white space.
func canonical(raw json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// tablePart is the table as Inspect reports it: what of it is not as Script
// loads it.
func tablePart(ctx context.Context, nft egress.Nft) (Part, error) {
	p := Part{Name: "the table " + Table}
	l, err := list(ctx, nft)
	switch {
	case errors.Is(err, egress.ErrNotLoaded):
		p.Missing, p.Differences = true, []string{p.Name + " is not loaded"}
	case errors.Is(err, errUnreadable):
		p.Differences = []string{fmt.Sprintf("the listing of the table %s cannot be read: %s",
			Table, strings.TrimPrefix(err.Error(), errUnreadable.Error()+": "))}
	case err != nil:
		return Part{}, err
	default:
		p.Differences = l.differences()
	}
	return p, nil
}

func (l *listed) differences() []string {
	of := "the table " + Table
	var d []string
	if l.flags != "" {
		// A dormant table holds its rule and rejects nothing.
		d = append(d, of+" has flags "+l.flags)
	}
	for _, u := range l.unexpected {
		d = append(d, of+" holds "+u)
	}
	for _, name := range slices.Sorted(maps.Keys(l.chains)) {
		if name != chain {
			d = append(d, of+" holds an unexpected chain "+name)
		}
	}
	prio := json.Number(fmt.Sprint(priority))
	want := listedChain{Type: "filter", Hook: "output", Prio: &prio, Policy: "accept"}
	got, ok := l.chains[chain]
	switch rules := l.rules[chain]; {
	case !ok:
		d = append(d, of+" has no chain "+chain)
	case got.String() != want.String():
		d = append(d, fmt.Sprintf("chain %s of %s is %s, want %s", chain, of, got, want))
	case len(rules) != 1:
		d = append(d, fmt.Sprintf("chain %s of %s has %d rules, want 1", chain, of, len(rules)))
	case !slices.Contains(wantRules(), rules[0]):
		d = append(d, fmt.Sprintf("the rule of chain %s of %s is not the one pco loads", chain, of))
	}
	for _, name := range slices.Sorted(maps.Keys(l.counters)) {
		if name != counter {
			d = append(d, of+" holds an unexpected counter "+name)
		}
	}
	if _, ok := l.counters[counter]; !ok {
		d = append(d, of+" has no counter "+counter)
	}
	return d
}

// Leaked returns what the counter of the reject rule counted since the table
// was loaded: packets that were sent to the service prefix. Without the
// table it returns egress.ErrNotLoaded.
func Leaked(ctx context.Context, nft egress.Nft) (egress.Counter, error) {
	l, err := list(ctx, nft)
	if err != nil {
		return egress.Counter{}, err
	}
	c, ok := l.counters[counter]
	if !ok {
		return egress.Counter{}, fmt.Errorf("the table %s has no counter %s", Table, counter)
	}
	return c, nil
}
