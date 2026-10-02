package main

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
)

// fakeNft is the nft of the egress commands in the tests: it records the
// scripts and lists what live holds.
type fakeNft struct {
	mu       sync.Mutex
	scripts  []string
	live     string
	listErr  error
	applyErr error
}

func (f *fakeNft) Apply(_ context.Context, script string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, script)
	return f.applyErr
}

func (f *fakeNft) List(context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return []byte(f.live), nil
}

func (f *fakeNft) applied() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.scripts)
}

// liveTable is a listing of a loaded table with targets, a resolver and what
// its counters counted. The commands read the sets and counters of a listing;
// whether its rules are right is for Verify and load.
const liveTable = `{"nftables": [
{"metainfo": {"version": "1.1.3", "json_schema_version": 1}},
{"table": {"family": "inet", "name": "pco_egress", "handle": 3}},
{"counter": {"family": "inet", "name": "rejected_local", "table": "pco_egress", "handle": 3, "packets": 3, "bytes": 180}},
{"counter": {"family": "inet", "name": "rejected", "table": "pco_egress", "handle": 4, "packets": 12, "bytes": 720}},
{"set": {"family": "inet", "name": "targets4", "table": "pco_egress", "type": ["ipv4_addr", "inet_service"], "handle": 5,
  "elem": [{"concat": ["10.0.0.6", 443]}, {"concat": ["10.0.0.5", 80]}, {"concat": ["10.0.0.5", 8080]}]}},
{"set": {"family": "inet", "name": "targets6", "table": "pco_egress", "type": ["ipv6_addr", "inet_service"], "handle": 6}},
{"set": {"family": "inet", "name": "resolvers4", "table": "pco_egress", "type": "ipv4_addr", "handle": 7, "elem": ["192.168.1.1"]}},
{"set": {"family": "inet", "name": "resolvers6", "table": "pco_egress", "type": "ipv6_addr", "handle": 8}}
]}`

const testConnectorUID = 999

// egressRig runs the egress commands as root on a node whose nft and local
// state are the test's.
type egressRig struct {
	t   *testing.T
	app *app
	env egressEnv
	nft *fakeNft
}

func newEgressRig(t *testing.T) *egressRig {
	n := &fakeNft{listErr: egress.ErrNotLoaded}
	return &egressRig{
		t:   t,
		app: &app{env: testEnv()},
		nft: n,
		env: egressEnv{
			nft:   n,
			local: filepath.Join(t.TempDir(), "local"),
			euid:  func() int { return 0 },
			uid:   func() (uint32, error) { return testConnectorUID, nil },
		},
	}
}

func (r *egressRig) run(args ...string) result {
	r.t.Helper()
	cmd := r.app.egressCmdWith(r.env)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(unreadable{r.t})
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(r.t.Context())
	return result{out: out.String(), errOut: errOut.String(), err: err}
}

func (r *egressRig) overrides() *egress.Overrides { return egress.NewOverrides(r.env.local) }

func TestEgressIsACommandOfPco(t *testing.T) {
	root := newRootCmdWith(testEnv())

	cmd, _, err := root.Find([]string{"egress"})
	require.NoError(t, err)
	require.Equal(t, "egress", cmd.Name())
	var visible, hidden []string
	for _, c := range cmd.Commands() {
		if c.Hidden {
			hidden = append(hidden, c.Name())
		} else {
			visible = append(visible, c.Name())
		}
	}
	require.ElementsMatch(t, []string{"show", "block", "unblock", "off", "on"}, visible)
	require.Equal(t, []string{"load"}, hidden, "the boot unit runs load")
}

func TestEgressCommandsNeedRoot(t *testing.T) {
	for _, args := range [][]string{
		{"load"}, {"show"}, {"block", "10.0.0.5"}, {"unblock", "10.0.0.5"}, {"off"}, {"on"},
	} {
		t.Run(args[0], func(t *testing.T) {
			r := newEgressRig(t)
			r.env.euid = func() int { return 1000 }

			res := r.run(args...)

			require.ErrorContains(t, res.err, "pco egress needs root")
			require.Empty(t, res.out)
			require.Empty(t, r.nft.applied())
			require.NoDirExists(t, r.env.local)
		})
	}
}

func TestEgressCommandsPrintNoJSON(t *testing.T) {
	r := newEgressRig(t)
	r.app.json = true

	res := r.run("show")

	require.ErrorContains(t, res.err, "--json has no meaning")
	require.Empty(t, r.nft.applied())
}

func TestEgressLoadLoadsTheBaseTable(t *testing.T) {
	r := newEgressRig(t)

	res := r.run("load")

	require.NoError(t, res.err)
	require.Equal(t, []string{egress.Base(testConnectorUID)}, r.nft.applied())
	require.Equal(t, "Loaded the egress table: the connectors reach Cloudflare's edge and nothing else until the daemon adds their verified targets.\n", res.out)
}

func TestEgressLoadDoesNotLoadWhileTheFilterIsOff(t *testing.T) {
	r := newEgressRig(t)
	require.NoError(t, r.overrides().SwitchOff(t0))
	r.env.uid = func() (uint32, error) { return 0, errors.New("not asked") }

	res := r.run("load")

	require.NoError(t, res.err, "the boot unit must not fail, or no connector starts")
	require.Empty(t, r.nft.applied())
	require.Equal(t, "The egress filter is switched off since 2026-10-01T14:00:00+02:00; its table was not loaded. pco egress on switches it back on.\n", res.out)
}

func TestEgressLoadWithoutTheConnectorUserFails(t *testing.T) {
	r := newEgressRig(t)
	r.env.uid = func() (uint32, error) {
		return 0, errors.New("user pco-connector does not exist: install the pco package or run systemd-sysusers")
	}

	res := r.run("load")

	require.ErrorContains(t, res.err, "systemd-sysusers")
	require.Empty(t, r.nft.applied())
}

func TestEgressLoadReturnsAFailedApply(t *testing.T) {
	r := newEgressRig(t)
	r.nft.applyErr = errors.New("nft -f -: exit status 1: Error: Could not process rule")

	res := r.run("load")

	require.ErrorContains(t, res.err, "Could not process rule")
}

func TestEgressOffRemovesTheTableAndRecordsTheTime(t *testing.T) {
	r := newEgressRig(t)

	res := r.run("off")

	require.NoError(t, res.err)
	require.Equal(t, []string{"add table inet pco_egress\ndelete table inet pco_egress\n"}, r.nft.applied())
	since, off, err := r.overrides().Off()
	require.NoError(t, err)
	require.True(t, off)
	require.True(t, since.Equal(t0))
	require.Equal(t, "Switched the egress filter off: the connectors are not confined until pco egress on.\n", res.out)
}

func TestEgressOffTwiceKeepsTheFirstTime(t *testing.T) {
	r := newEgressRig(t)
	require.NoError(t, r.overrides().SwitchOff(t0.Add(-time.Hour)))

	res := r.run("off")

	require.NoError(t, res.err)
	since, _, err := r.overrides().Off()
	require.NoError(t, err)
	require.True(t, since.Equal(t0.Add(-time.Hour)))
	require.Len(t, r.nft.applied(), 1, "the table goes all the same")
	require.Equal(t, "The egress filter was off already, since 2026-10-01T13:00:00+02:00; its table is removed.\n", res.out)
}

func TestEgressOnLoadsTheTableAgain(t *testing.T) {
	r := newEgressRig(t)
	require.NoError(t, r.overrides().SwitchOff(t0))

	res := r.run("on")

	require.NoError(t, res.err)
	_, off, err := r.overrides().Off()
	require.NoError(t, err)
	require.False(t, off)
	require.Equal(t, []string{egress.Base(testConnectorUID)}, r.nft.applied())
	require.Equal(t, "Switched the egress filter on and loaded its table: the connectors reach Cloudflare's edge and nothing else until the daemon adds their verified targets at its next cycle.\n", res.out)
}

func TestEgressOnWhenItIsOnAlready(t *testing.T) {
	r := newEgressRig(t)
	r.nft.listErr = nil
	// A listing nft printed for a table of the connector user 999.
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "egress", "testdata", "listing-1.1.3.json"))
	require.NoError(t, err)
	r.nft.live = string(b)

	res := r.run("on")

	require.NoError(t, res.err)
	require.Empty(t, r.nft.applied(), "the sets the daemon filled are kept")
	require.Equal(t, "The egress filter was on already, and its table is loaded.\n", res.out)
}

func TestEgressOnLooksUpTheUserBeforeItChangesAnything(t *testing.T) {
	r := newEgressRig(t)
	require.NoError(t, r.overrides().SwitchOff(t0))
	r.env.uid = func() (uint32, error) { return 0, errors.New("no such user") }

	res := r.run("on")

	require.ErrorContains(t, res.err, "no such user")
	_, off, err := r.overrides().Off()
	require.NoError(t, err)
	require.True(t, off)
}

func TestEgressBlockTakesTheAddressOutOfTheLiveTable(t *testing.T) {
	r := newEgressRig(t)
	r.nft.listErr = nil
	r.nft.live = liveTable

	res := r.run("block", "10.0.0.5")

	require.NoError(t, res.err)
	require.Equal(t, []string{"delete element inet pco_egress targets4 { 10.0.0.5 . 80, 10.0.0.5 . 8080 }\n"}, r.nft.applied())
	blocked, err := r.overrides().Blocked()
	require.NoError(t, err)
	require.Equal(t, []netip.Addr{netip.MustParseAddr("10.0.0.5")}, blocked)
	require.Equal(t, "Blocked 10.0.0.5.\nTook 2 entries for it out of the egress table.\n", res.out)
}

func TestEgressBlockPrintsTheAddressAsItParsedIt(t *testing.T) {
	r := newEgressRig(t)
	r.nft.listErr = nil
	r.nft.live = liveTable

	res := r.run("block", "::ffff:10.0.0.6")

	require.NoError(t, res.err)
	require.Equal(t, "Blocked 10.0.0.6.\nTook 1 entry for it out of the egress table.\n", res.out)
}

func TestEgressBlockRefusesWhatIsNoAddress(t *testing.T) {
	for _, arg := range []string{"nonsense", "10.0.0.0/8", "10.0.0.5:80", "10.0.0.5; flush ruleset", ""} {
		t.Run(arg, func(t *testing.T) {
			r := newEgressRig(t)

			res := r.run("block", arg)

			require.ErrorContains(t, res.err, "is not an IP address")
			require.Empty(t, r.nft.applied())
			require.NoDirExists(t, r.env.local)
		})
	}
}

func TestEgressBlockWhileTheFilterIsOff(t *testing.T) {
	r := newEgressRig(t)
	require.NoError(t, r.overrides().SwitchOff(t0))

	res := r.run("block", "10.0.0.5")

	require.NoError(t, res.err)
	require.Empty(t, r.nft.applied())
	require.Equal(t, "Blocked 10.0.0.5.\nThe egress filter is off; the block applies once it is switched on.\n", res.out)
}

func TestEgressBlockWithoutATable(t *testing.T) {
	r := newEgressRig(t)

	res := r.run("block", "10.0.0.5")

	require.NoError(t, res.err)
	require.Equal(t, "Blocked 10.0.0.5.\nThe egress table is not loaded.\n", res.out)
}

func TestEgressBlockOfAnAddressTheTableDoesNotHold(t *testing.T) {
	r := newEgressRig(t)
	r.nft.listErr = nil
	r.nft.live = liveTable
	_, err := r.overrides().Block(netip.MustParseAddr("10.0.0.99"))
	require.NoError(t, err)

	res := r.run("block", "10.0.0.99")

	require.NoError(t, res.err)
	require.Empty(t, r.nft.applied())
	require.Equal(t, "10.0.0.99 was blocked already.\nThe egress table held no entry for it.\n", res.out)
}

func TestEgressUnblock(t *testing.T) {
	r := newEgressRig(t)
	_, err := r.overrides().Block(netip.MustParseAddr("10.0.0.5"))
	require.NoError(t, err)

	res := r.run("unblock", "10.0.0.5")
	require.NoError(t, res.err)
	require.Equal(t, "Unblocked 10.0.0.5: the daemon puts it back into the egress table at its next cycle if it is still a verified target.\n", res.out)

	res = r.run("unblock", "10.0.0.5")
	require.NoError(t, res.err)
	require.Equal(t, "10.0.0.5 was not blocked.\n", res.out)
	require.Empty(t, r.nft.applied())
}

func TestEgressShowGolden(t *testing.T) {
	tests := []struct {
		name   string
		golden string
		setup  func(r *egressRig)
	}{
		{name: "on", golden: "egress_show.golden", setup: func(r *egressRig) {
			r.nft.listErr, r.nft.live = nil, liveTable
			_, err := r.overrides().Block(netip.MustParseAddr("10.0.0.9"))
			require.NoError(r.t, err)
		}},
		{name: "on without a table", golden: "egress_show_unloaded.golden", setup: func(*egressRig) {}},
		{name: "off", golden: "egress_show_off.golden", setup: func(r *egressRig) {
			require.NoError(r.t, r.overrides().SwitchOff(t0))
		}},
		{name: "off with a table", golden: "egress_show_off_loaded.golden", setup: func(r *egressRig) {
			r.nft.listErr, r.nft.live = nil, liveTable
			require.NoError(r.t, r.overrides().SwitchOff(t0))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newEgressRig(t)
			tc.setup(r)

			res := r.run("show")

			require.NoError(t, res.err)
			require.Empty(t, res.errOut)
			require.Empty(t, r.nft.applied())
			requireGolden(t, tc.golden, res.out)
		})
	}
}

func TestEgressShowReturnsAFailureToList(t *testing.T) {
	r := newEgressRig(t)
	r.nft.listErr = errors.New("nft -j list table inet pco_egress: exit status 1: Error: Operation not permitted")

	res := r.run("show")

	require.ErrorContains(t, res.err, "Operation not permitted")
}

func TestEgressShowWithAnOffSwitchThatCannotBeRead(t *testing.T) {
	r := newEgressRig(t)
	require.NoError(t, os.MkdirAll(r.env.local, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(r.env.local, "egress-off.json"), []byte("garbage"), 0o600))

	res := r.run("show")

	require.NoError(t, res.err)
	require.Contains(t, res.out, "The egress filter is switched off since an unknown time")
}
