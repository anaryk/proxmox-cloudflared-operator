package appnet

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
)

// fakeNetlink is the network of a container as Load and Verify see it. Like
// the kernel, it takes the addresses and routes of a device that goes, a
// route through a device that is down, and refuses a route whose source is
// no address of the device.
type fakeNetlink struct {
	dummy, up bool
	addrs     map[netip.Prefix]bool
	routes    map[string]bool
	rules     map[string]bool
	ensured   []string
	ensureErr error
	hasErr    error
}

func newFakeNetlink() *fakeNetlink {
	return &fakeNetlink{addrs: map[netip.Prefix]bool{}, routes: map[string]bool{}, rules: map[string]bool{}}
}

func routeKey(prefix netip.Prefix, dev string, src netip.Addr) string {
	return fmt.Sprintf("%s dev %s src %s", prefix, dev, src)
}

func ruleKey(pref int, src netip.Addr, dst netip.Prefix, unreachable bool) string {
	return fmt.Sprintf("%d from %s to %s unreachable %t", pref, src, dst, unreachable)
}

func (f *fakeNetlink) EnsureDummy(_ context.Context, name string) error {
	f.ensured = append(f.ensured, "dummy "+name)
	if f.ensureErr != nil {
		return f.ensureErr
	}
	if !f.dummy {
		f.addrs, f.routes = map[netip.Prefix]bool{}, map[string]bool{}
	}
	f.dummy, f.up = true, true
	return nil
}

func (f *fakeNetlink) HasDummy(_ context.Context, name string) (bool, error) {
	return name == Device && f.dummy && f.up, f.hasErr
}

func (f *fakeNetlink) EnsureAddr(_ context.Context, dev string, addr netip.Prefix) error {
	f.ensured = append(f.ensured, "addr "+addr.String())
	if !f.dummy || dev != Device {
		return errors.New("no such device")
	}
	f.addrs[addr] = true
	return nil
}

func (f *fakeNetlink) HasAddr(_ context.Context, dev string, addr netip.Prefix) (bool, error) {
	return dev == Device && f.dummy && f.addrs[addr], f.hasErr
}

func (f *fakeNetlink) EnsureRoute(_ context.Context, prefix netip.Prefix, dev string, src netip.Addr) error {
	f.ensured = append(f.ensured, "route "+routeKey(prefix, dev, src))
	switch {
	case !f.dummy || !f.up:
		return errors.New("network is down")
	case !f.addrs[netip.PrefixFrom(src, src.BitLen())]:
		return errors.New("invalid prefsrc address")
	}
	f.routes[routeKey(prefix, dev, src)] = true
	return nil
}

func (f *fakeNetlink) HasRoute(_ context.Context, prefix netip.Prefix, dev string, src netip.Addr) (bool, error) {
	return f.dummy && f.up && f.routes[routeKey(prefix, dev, src)], f.hasErr
}

func (f *fakeNetlink) EnsureRule(_ context.Context, pref int, src netip.Addr, dst netip.Prefix, unreachable bool) error {
	f.ensured = append(f.ensured, "rule "+ruleKey(pref, src, dst, unreachable))
	f.rules[ruleKey(pref, src, dst, unreachable)] = true
	return nil
}

func (f *fakeNetlink) HasRule(_ context.Context, pref int, src netip.Addr, dst netip.Prefix, unreachable bool) (bool, error) {
	return f.rules[ruleKey(pref, src, dst, unreachable)], f.hasErr
}

// fakeNft holds the table as nft 1.1.3 lists it once Script was applied.
type fakeNft struct {
	t        *testing.T
	live     []byte
	listErr  error
	applyErr error
	scripts  []string
}

func (f *fakeNft) Apply(_ context.Context, script string) error {
	f.scripts = append(f.scripts, script)
	if f.applyErr != nil {
		return f.applyErr
	}
	require.Equal(f.t, Script(), script)
	f.live = listing(f.t, "listing-1.1.3.json")
	return nil
}

func (f *fakeNft) List(context.Context) ([]byte, error) {
	switch {
	case f.listErr != nil:
		return nil, f.listErr
	case f.live == nil:
		return nil, egress.ErrNotLoaded
	}
	return f.live, nil
}

func listing(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return b
}

// edited is the listing of nft 1.1.3 with old replaced by new, once.
func edited(t *testing.T, old, new string) []byte {
	t.Helper()
	l := string(listing(t, "listing-1.1.3.json"))
	require.Contains(t, l, old)
	return []byte(strings.Replace(l, old, new, 1))
}

const (
	nameDevice  = "the dummy device pco0"
	nameAddr    = "the address 198.18.0.1/32 on pco0"
	nameRoute   = "the route 198.18.0.0/16 dev pco0 src 198.18.0.1"
	nameRule    = "the rule 1890: from 198.18.0.1 to 198.18.0.0/16 lookup main"
	nameUnreach = "the rule 1900: from 198.18.0.1 unreachable"
	nameTable   = "the table inet pco_net"
)

// complete is a container in which pco-net.service ran.
func complete(t *testing.T) (*fakeNetlink, *fakeNft) {
	t.Helper()
	nl, nft := newFakeNetlink(), &fakeNft{t: t}
	_, err := Load(t.Context(), nl, nft)
	require.NoError(t, err)
	nl.ensured, nft.scripts = nil, nil
	return nl, nft
}

// The two values are plan 3's gateway.ServicePrefix and gateway.TransitHost,
// which replace them; until then nothing may make them differ.
func TestTheServicePrefixAndItsSourceAreTheGatewaysValues(t *testing.T) {
	require.Equal(t, "198.18.0.0/16", ServicePrefix.String())
	require.Equal(t, "198.18.0.1", ServiceSource.String())
	require.True(t, ServicePrefix.Contains(ServiceSource))
	require.Equal(t, "pco0", Device)
	require.Equal(t, "inet pco_net", Table)
	require.Equal(t, 1890, PrefToPrefix)
	require.Equal(t, 1900, PrefUnreachable)
}

func TestTheScriptRejectsThePrefixWithACounter(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "net.nft"))
	require.NoError(t, err)

	require.Equal(t, string(want), Script())
	// The reject answers a request through pco0 at its source, 198.18.0.1,
	// which is in the prefix: without the exception for the appliance's own
	// addresses the rule rejects that answer too, and the request waits.
	require.Contains(t, Script(), "\t\tip daddr 198.18.0.0/16 fib daddr type != local counter name leaked reject\n")
	require.Contains(t, Script(), "# The service prefix never leaves the appliance.")
	require.NotRegexp(t, `(?m)^\t.* drop$`, Script(), "a dropped request waits out cloudflared's origin timeout")
}

func TestLoadPutsEverythingInPlaceOnAnEmptySystem(t *testing.T) {
	nl, nft := newFakeNetlink(), &fakeNft{t: t}

	changed, err := Load(t.Context(), nl, nft)

	require.NoError(t, err)
	require.Equal(t, []string{nameDevice, nameAddr, nameRoute, nameRule, nameUnreach, nameTable}, changed)
	require.Equal(t, []string{
		"dummy pco0",
		"addr 198.18.0.1/32",
		"route 198.18.0.0/16 dev pco0 src 198.18.0.1",
		"rule 1890 from 198.18.0.1 to 198.18.0.0/16 unreachable false",
		"rule 1900 from 198.18.0.1 to invalid Prefix unreachable true",
	}, nl.ensured, "in an order the kernel accepts: the source of the route is an address of the device")
	require.Equal(t, []string{Script()}, nft.scripts)
	require.NoError(t, Verify(t.Context(), nl, nft))
}

func TestLoadChangesNothingOnACompleteSystem(t *testing.T) {
	nl, nft := complete(t)

	changed, err := Load(t.Context(), nl, nft)

	require.NoError(t, err)
	require.Empty(t, changed)
	require.Empty(t, nl.ensured)
	require.Empty(t, nft.scripts, "the table and its counter are kept")
}

func TestLoadPutsBackWhatIsMissingAndNothingElse(t *testing.T) {
	tests := []struct {
		name    string
		spoil   func(t *testing.T, nl *fakeNetlink, nft *fakeNft)
		changed []string
		ensured []string
		scripts int
	}{
		{
			name:    "the route",
			spoil:   func(_ *testing.T, nl *fakeNetlink, _ *fakeNft) { nl.routes = map[string]bool{} },
			changed: []string{nameRoute},
			ensured: []string{"route 198.18.0.0/16 dev pco0 src 198.18.0.1"},
		},
		{
			name: "the rule to the prefix",
			spoil: func(_ *testing.T, nl *fakeNetlink, _ *fakeNft) {
				delete(nl.rules, ruleKey(PrefToPrefix, ServiceSource, ServicePrefix, false))
			},
			changed: []string{nameRule},
			ensured: []string{"rule 1890 from 198.18.0.1 to 198.18.0.0/16 unreachable false"},
		},
		{
			name: "the unreachable rule",
			spoil: func(_ *testing.T, nl *fakeNetlink, _ *fakeNft) {
				delete(nl.rules, ruleKey(PrefUnreachable, ServiceSource, netip.Prefix{}, true))
			},
			changed: []string{nameUnreach},
			ensured: []string{"rule 1900 from 198.18.0.1 to invalid Prefix unreachable true"},
		},
		{
			name: "the device, which took its address and route",
			spoil: func(_ *testing.T, nl *fakeNetlink, _ *fakeNft) {
				nl.dummy, nl.up = false, false
			},
			changed: []string{nameDevice, nameAddr, nameRoute},
			ensured: []string{"dummy pco0", "addr 198.18.0.1/32", "route 198.18.0.0/16 dev pco0 src 198.18.0.1"},
		},
		{
			name: "the table",
			spoil: func(_ *testing.T, _ *fakeNetlink, nft *fakeNft) {
				nft.live = nil
			},
			changed: []string{nameTable},
			scripts: 1,
		},
		{
			name: "a dormant table",
			spoil: func(t *testing.T, _ *fakeNetlink, nft *fakeNft) {
				nft.live = edited(t, `"handle": 3}}`, `"handle": 3, "flags": "dormant"}}`)
			},
			changed: []string{nameTable},
			scripts: 1,
		},
		{
			name: "a listing that cannot be read",
			spoil: func(_ *testing.T, _ *fakeNetlink, nft *fakeNft) {
				nft.live = []byte(`{"nftables": [{"table": `)
			},
			changed: []string{nameTable},
			scripts: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nl, nft := complete(t)
			tc.spoil(t, nl, nft)

			changed, err := Load(t.Context(), nl, nft)

			require.NoError(t, err)
			require.Equal(t, tc.changed, changed)
			require.Equal(t, tc.ensured, nl.ensured)
			require.Len(t, nft.scripts, tc.scripts)
			require.NoError(t, Verify(t.Context(), nl, nft))
		})
	}
}

func TestLoadSaysWhatFailed(t *testing.T) {
	t.Run("the device", func(t *testing.T) {
		nl, nft := newFakeNetlink(), &fakeNft{t: t}
		nl.ensureErr = errors.New("operation not permitted")

		changed, err := Load(t.Context(), nl, nft)

		require.EqualError(t, err, "adding the dummy device pco0: operation not permitted")
		require.Empty(t, changed)
		require.Empty(t, nft.scripts, "nothing after the step that failed")
	})
	t.Run("a look at the network", func(t *testing.T) {
		nl, nft := complete(t)
		nl.hasErr = errors.New("netlink: message truncated")

		_, err := Load(t.Context(), nl, nft)

		require.EqualError(t, err, "reading the dummy device pco0: netlink: message truncated")
	})
	t.Run("the table", func(t *testing.T) {
		nl, nft := complete(t)
		nft.live, nft.applyErr = nil, errors.New("nft -f -: exit status 1")

		changed, err := Load(t.Context(), nl, nft)

		require.EqualError(t, err, "loading the table inet pco_net: nft -f -: exit status 1")
		require.Empty(t, changed)
	})
	t.Run("the listing", func(t *testing.T) {
		nl, nft := complete(t)
		nft.listErr = errors.New("nft -j list table inet pco_net: signal: killed")

		_, err := Load(t.Context(), nl, nft)

		require.EqualError(t, err, "listing the table inet pco_net: nft -j list table inet pco_net: signal: killed")
		require.Empty(t, nft.scripts)
	})
}

func TestVerifyNamesWhatIsMissing(t *testing.T) {
	tests := []struct {
		name  string
		spoil func(t *testing.T, nl *fakeNetlink, nft *fakeNft)
		want  []string
	}{
		{
			name:  "the device",
			spoil: func(_ *testing.T, nl *fakeNetlink, _ *fakeNft) { nl.dummy = false },
			want: []string{
				"the dummy device pco0 is missing or down",
				"the address 198.18.0.1/32 on pco0 is missing",
				"the route 198.18.0.0/16 dev pco0 src 198.18.0.1 is missing",
			},
		},
		{
			name:  "a device that is down",
			spoil: func(_ *testing.T, nl *fakeNetlink, _ *fakeNft) { nl.up = false },
			want: []string{
				"the dummy device pco0 is missing or down",
				"the route 198.18.0.0/16 dev pco0 src 198.18.0.1 is missing",
			},
		},
		{
			name:  "the address",
			spoil: func(_ *testing.T, nl *fakeNetlink, _ *fakeNft) { nl.addrs = map[netip.Prefix]bool{} },
			want:  []string{"the address 198.18.0.1/32 on pco0 is missing"},
		},
		{
			name:  "the route",
			spoil: func(_ *testing.T, nl *fakeNetlink, _ *fakeNft) { nl.routes = map[string]bool{} },
			want:  []string{"the route 198.18.0.0/16 dev pco0 src 198.18.0.1 is missing"},
		},
		{
			name:  "both rules",
			spoil: func(_ *testing.T, nl *fakeNetlink, _ *fakeNft) { nl.rules = map[string]bool{} },
			want: []string{
				"the rule 1890: from 198.18.0.1 to 198.18.0.0/16 lookup main is missing",
				"the rule 1900: from 198.18.0.1 unreachable is missing",
			},
		},
		{
			name:  "the table",
			spoil: func(_ *testing.T, _ *fakeNetlink, nft *fakeNft) { nft.live = nil },
			want:  []string{"the table inet pco_net is not loaded"},
		},
		{
			name: "a dormant table",
			spoil: func(t *testing.T, _ *fakeNetlink, nft *fakeNft) {
				nft.live = edited(t, `"handle": 3}}`, `"handle": 3, "flags": "dormant"}}`)
			},
			want: []string{"the table inet pco_net has flags dormant"},
		},
		{
			// nft 1.0.6 prints the dormant flag as another word.
			name: "a table with any flag",
			spoil: func(t *testing.T, _ *fakeNetlink, nft *fakeNft) {
				nft.live = edited(t, `"handle": 3}}`, `"handle": 3, "flags": "leaked"}}`)
			},
			want: []string{"the table inet pco_net has flags leaked"},
		},
		{
			name: "a chain at another priority",
			spoil: func(t *testing.T, _ *fakeNetlink, nft *fakeNft) {
				nft.live = edited(t, `"prio": -20`, `"prio": 10`)
			},
			want: []string{"chain output of the table inet pco_net is filter hook output priority 10 policy accept, " +
				"want filter hook output priority -20 policy accept"},
		},
		{
			name: "a rule that accepts",
			spoil: func(t *testing.T, _ *fakeNetlink, nft *fakeNft) {
				nft.live = edited(t, `{"reject": {"type": "icmp", "expr": "port-unreachable"}}`, `{"accept": null}`)
			},
			want: []string{"the rule of chain output of the table inet pco_net is not the one pco loads"},
		},
		{
			name: "a rule without the counter",
			spoil: func(t *testing.T, _ *fakeNetlink, nft *fakeNft) {
				nft.live = edited(t, `{"counter": "leaked"}, `, ``)
			},
			want: []string{"the rule of chain output of the table inet pco_net is not the one pco loads"},
		},
		{
			name: "a rule before it",
			spoil: func(t *testing.T, _ *fakeNetlink, nft *fakeNft) {
				nft.live = edited(t, `{"rule": {`,
					`{"rule": {"family": "inet", "table": "pco_net", "chain": "output", "handle": 4, "expr": [{"accept": null}]}}, {"rule": {`)
			},
			want: []string{"chain output of the table inet pco_net has 2 rules, want 1"},
		},
		{
			name: "no chain, no counter, and a set",
			spoil: func(t *testing.T, _ *fakeNetlink, nft *fakeNft) {
				nft.live = []byte(`{"nftables": [{"table": {"family": "inet", "name": "pco_net", "handle": 3}}, ` +
					`{"set": {"family": "inet", "name": "s", "table": "pco_net", "type": "ipv4_addr", "handle": 2}}]}`)
			},
			want: []string{
				"the table inet pco_net holds an unexpected set s",
				"the table inet pco_net has no chain output",
				"the table inet pco_net has no counter leaked",
			},
		},
		{
			name: "a listing that cannot be read",
			spoil: func(_ *testing.T, _ *fakeNetlink, nft *fakeNft) {
				nft.live = []byte(`{"tables": []}`)
			},
			want: []string{"the listing of the table inet pco_net cannot be read: no nftables array"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nl, nft := complete(t)
			tc.spoil(t, nl, nft)

			err := Verify(t.Context(), nl, nft)

			require.ErrorIs(t, err, egress.ErrChanged)
			var changed *ChangedError
			require.ErrorAs(t, err, &changed)
			require.Equal(t, tc.want, changed.Differences)
			require.Equal(t, strings.Join(tc.want, "; "), err.Error(), "the differences, not the words of the egress table")
		})
	}
}

func TestVerifyPassesTheListingsOfBothVersionsOfNft(t *testing.T) {
	for _, name := range []string{"listing-1.0.6.json", "listing-1.1.3.json"} {
		t.Run(name, func(t *testing.T) {
			nl, nft := complete(t)
			nft.live = listing(t, name)

			require.NoError(t, Verify(t.Context(), nl, nft))
		})
	}
	t.Run("and a reject printed without its type", func(t *testing.T) {
		nl, nft := complete(t)
		nft.live = edited(t, `{"reject": {"type": "icmp", "expr": "port-unreachable"}}`, `{"reject": null}`)

		require.NoError(t, Verify(t.Context(), nl, nft))
	})
}

// What cannot be read is no difference: Verify says so as an error of its
// own, which loads nothing again.
func TestVerifyFailsWhenItCannotLook(t *testing.T) {
	t.Run("the network", func(t *testing.T) {
		nl, nft := complete(t)
		nl.hasErr = errors.New("netlink: message truncated")

		err := Verify(t.Context(), nl, nft)

		require.EqualError(t, err, "reading the dummy device pco0: netlink: message truncated")
		require.NotErrorIs(t, err, egress.ErrChanged)
	})
	t.Run("the table", func(t *testing.T) {
		nl, nft := complete(t)
		nft.listErr = errors.New("signal: killed")

		err := Verify(t.Context(), nl, nft)

		require.EqualError(t, err, "listing the table inet pco_net: signal: killed")
		require.NotErrorIs(t, err, egress.ErrChanged)
	})
}

func TestInspectShowsEachPart(t *testing.T) {
	nl, nft := complete(t)
	nl.routes = map[string]bool{}

	parts, err := Inspect(t.Context(), nl, nft)

	require.NoError(t, err)
	var names []string
	for _, p := range parts {
		names = append(names, p.Name)
		require.Equal(t, p.Name != nameRoute, p.OK(), p.Name)
	}
	require.Equal(t, []string{nameDevice, nameAddr, nameRoute, nameRule, nameUnreach, nameTable}, names)
}

func TestLeakedReadsTheCounter(t *testing.T) {
	_, nft := complete(t)
	nft.live = edited(t, `"packets": 0, "bytes": 0`, `"packets": 7, "bytes": 420`)

	c, err := Leaked(t.Context(), nft)

	require.NoError(t, err)
	require.Equal(t, egress.Counter{Packets: 7, Bytes: 420}, c)

	t.Run("not without the table", func(t *testing.T) {
		nft.live = nil
		_, err := Leaked(t.Context(), nft)
		require.ErrorIs(t, err, egress.ErrNotLoaded)
	})
	t.Run("nor without the counter", func(t *testing.T) {
		nft.live = []byte(`{"nftables": [{"table": {"family": "inet", "name": "pco_net", "handle": 3}}]}`)
		_, err := Leaked(t.Context(), nft)
		require.EqualError(t, err, "the table inet pco_net has no counter leaked")
	})
}
