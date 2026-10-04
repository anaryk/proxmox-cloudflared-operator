package egress

import (
	"context"
	"encoding/json"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "write the golden files of the tests")

// testUID is the uid of the connector user in the tests and in the listings
// of testdata.
const testUID = 999

// fakeNft records the scripts it is given and answers List from its fields.
type fakeNft struct {
	mu      sync.Mutex
	scripts []string
	apply   func(script string) error // what Apply answers; nil applies every script
	live    []byte
	listErr error
}

func (f *fakeNft) Apply(_ context.Context, script string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, script)
	if f.apply != nil {
		return f.apply(script)
	}
	return nil
}

func (f *fakeNft) List(context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live, f.listErr
}

func (f *fakeNft) applied() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.scripts)
}

func (f *fakeNft) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = nil
}

func (f *fakeNft) setLive(raw []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live, f.listErr = raw, nil
}

// resolvers is a resolver function whose answer the test changes.
type resolvers struct {
	mu    sync.Mutex
	addrs []netip.Addr
	err   error
	calls int
}

func (r *resolvers) get() ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return slices.Clone(r.addrs), r.err
}

func (r *resolvers) set(addrs ...netip.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addrs = addrs
}

// newTestFilter returns a filter over a fake nft, with resolvers the test
// sets and its overrides in a directory of its own.
func newTestFilter(t *testing.T) (*Filter, *fakeNft, *resolvers, *Overrides) {
	t.Helper()
	n := &fakeNft{}
	r := &resolvers{}
	ov := NewOverrides(t.TempDir())
	return New(n, testUID, r.get, ov), n, r, ov
}

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func target(s string) Target {
	ap := netip.MustParseAddrPort(s)
	return Target{Addr: ap.Addr(), Port: ap.Port()}
}

func targets(ss ...string) []Target {
	out := make([]Target, 0, len(ss))
	for _, s := range ss {
		out = append(out, target(s))
	}
	return out
}

// requireGolden compares got with a file of testdata; run the tests with
// -update to write them.
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

// elementsOf returns the elements a script gives a set, in its order.
func elementsOf(t *testing.T, script, set string) []string {
	t.Helper()
	_, block, ok := strings.Cut(script, "\tset "+set+" {\n")
	require.True(t, ok, "no set %s in the script", set)
	block, _, _ = strings.Cut(block, "\n\t}\n")
	_, elems, ok := strings.Cut(block, "elements = {\n")
	if !ok {
		return nil
	}
	var out []string
	for line := range strings.Lines(strings.TrimSuffix(elems, "\n\t\t}")) {
		out = append(out, strings.TrimSuffix(strings.TrimSpace(line), ","))
	}
	return out
}

// listing is a table as nft -j lists it: the entries under "nftables".
type listing []map[string]any

// realListing reads a listing that an nft of the given version printed for
// the table of testdata/listed.nft. The kernel they were taken on had no fib
// expression for inet tables, so the fib match in them is the one the same
// nft printed for an ip table; the Linux test compares a real one.
func realListing(t *testing.T, version string) listing {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "listing-"+version+".json"))
	require.NoError(t, err)
	var doc struct {
		Nftables listing `json:"nftables"`
	}
	require.NoError(t, json.Unmarshal(b, &doc))
	return doc.Nftables
}

// bytes encodes the listing as nft prints it.
func (l listing) bytes(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"nftables": []map[string]any(l)})
	require.NoError(t, err)
	return b
}

// with returns the listing with the elements of its sets replaced by the
// targets and resolvers given.
func (l listing) with(t *testing.T, tg []Target, rs []netip.Addr) listing {
	t.Helper()
	elems := map[string][]any{}
	for _, x := range tg {
		name := targetSet(x)
		elems[name] = append(elems[name], map[string]any{"concat": []any{x.Addr.String(), x.Port}})
	}
	for _, a := range rs {
		name := setResolvers4
		if a.Is6() {
			name = setResolvers6
		}
		elems[name] = append(elems[name], a.String())
	}
	return l.edit(t, func(entries listing) listing {
		for _, e := range entries {
			set, ok := e["set"].(map[string]any)
			if !ok {
				continue
			}
			delete(set, "elem")
			if v := elems[set["name"].(string)]; len(v) > 0 {
				set["elem"] = v
			}
		}
		return entries
	})
}

// withBlocked returns the listing with the blocked sets holding addrs.
func (l listing) withBlocked(t *testing.T, addrs ...netip.Addr) listing {
	t.Helper()
	elems := map[string][]any{}
	for _, a := range addrs {
		name := setBlocked4
		if a.Is6() {
			name = setBlocked6
		}
		elems[name] = append(elems[name], a.String())
	}
	return l.edit(t, func(entries listing) listing {
		for _, e := range entries {
			set, ok := e["set"].(map[string]any)
			if !ok || (set["name"] != setBlocked4 && set["name"] != setBlocked6) {
				continue
			}
			delete(set, "elem")
			if v := elems[set["name"].(string)]; len(v) > 0 {
				set["elem"] = v
			}
		}
		return entries
	})
}

// edit returns a deep copy of the listing changed by fn.
func (l listing) edit(t *testing.T, fn func(listing) listing) listing {
	t.Helper()
	b, err := json.Marshal([]map[string]any(l))
	require.NoError(t, err)
	var c listing
	require.NoError(t, json.Unmarshal(b, &c))
	return fn(c)
}

// object returns the entry of a kind and name, such as a chain or a set.
func (l listing) object(t *testing.T, kind, name string) map[string]any {
	t.Helper()
	for _, e := range l {
		if o, ok := e[kind].(map[string]any); ok && o["name"] == name {
			return o
		}
	}
	t.Fatalf("no %s %s in the listing", kind, name)
	return nil
}

// rules returns the indexes of the rule entries of a chain, in order.
func (l listing) rules(chain string) []int {
	var idx []int
	for i, e := range l {
		if r, ok := e["rule"].(map[string]any); ok && r["chain"] == chain {
			idx = append(idx, i)
		}
	}
	return idx
}
