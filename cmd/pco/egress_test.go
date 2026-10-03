package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// liveTable is a listing nft printed for a table of the connector user 999
// with the targets 10.0.0.5:80, 10.0.0.5:8080 and 10.0.0.6:443 and the
// resolver 192.168.1.1, with what its counters counted put in.
func liveTable(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "egress", "testdata", "listing-1.1.3.json"))
	require.NoError(t, err)
	live := string(b)
	for name, counted := range map[string]string{
		`"rejected_local", "table": "pco_egress", "handle": 3`: `"packets": 3, "bytes": 180`,
		`"rejected", "table": "pco_egress", "handle": 4`:       `"packets": 12, "bytes": 720`,
	} {
		before := live
		live = strings.Replace(live, name+`, "packets": 0, "bytes": 0`, name+", "+counted, 1)
		require.NotEqual(t, before, live, "no counter %s in the listing", name)
	}
	return live
}

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
			nft:       n,
			local:     filepath.Join(t.TempDir(), "local"),
			euid:      func() int { return 0 },
			uid:       func() (uint32, error) { return testConnectorUID, nil },
			resolvers: func() ([]netip.Addr, error) { return nil, nil },
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

func TestEgressHelpSaysHowToLoadTheTableAgain(t *testing.T) {
	r := newEgressRig(t)

	res := r.run("--help")

	require.NoError(t, res.err)
	require.Contains(t, res.out, "pco egress load loads the table again")
	require.Contains(t, res.out, "Never restart pco-egress.service for that: every connector restarts\nwith it.")
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
	resolvers := []netip.Addr{netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("10.0.0.53")}
	r.env.resolvers = func() ([]netip.Addr, error) { return resolvers, nil }
	_, err := r.overrides().Block(netip.MustParseAddr("10.0.0.53"))
	require.NoError(t, err)

	res := r.run("load")

	require.NoError(t, res.err)
	require.Equal(t, []string{egress.Base(testConnectorUID, resolvers, []netip.Addr{netip.MustParseAddr("10.0.0.53")})}, r.nft.applied())
	require.Equal(t, "Loaded the egress table: the connectors reach the resolvers of the node and Cloudflare's edge, and nothing else until the daemon adds their verified targets.\n", res.out)
}

func TestEgressLoadWithResolversThatCannotBeReadStillLoads(t *testing.T) {
	r := newEgressRig(t)
	r.env.resolvers = func() ([]netip.Addr, error) {
		return nil, errors.New("reading /etc/resolv.conf: permission denied")
	}

	res := r.run("load")

	require.ErrorIs(t, res.err, errReported, "an exit status of 1")
	require.Equal(t, []string{egress.Base(testConnectorUID, nil, nil)}, r.nft.applied())
	require.Equal(t, "Loaded the egress table: "+loadedText+".\n"+
		"The resolvers could not be read (reading /etc/resolv.conf: permission denied). pco egress load fails for that: "+
		"run by pco-egress.service, it fails the unit, and the connectors, which require the unit, do not start. "+
		"Once the resolvers can be read, start pco-egress.service again.\n", res.out)
}

func TestEgressLoadSaysThatTheNodeNamesNoResolver(t *testing.T) {
	r := newEgressRig(t)

	res := r.run("load")

	require.NoError(t, res.err, "the connectors start, and the daemon lets them reach a resolver once there is one")
	require.Equal(t, []string{egress.Base(testConnectorUID, nil, nil)}, r.nft.applied())
	require.Equal(t, "Loaded the egress table: "+loadedText+".\n"+
		"No name server was found in /etc/resolv.conf: the connectors resolve no names until one is there "+
		"and the daemon's next cycle lets them reach it.\n", res.out)
}

// The boot table that was loaded while the resolvers could not be read heals
// at the next start of the unit.
func TestEgressLoadReplacesATableWithoutResolvers(t *testing.T) {
	r := newEgressRig(t)
	r.nft.listErr = nil
	r.nft.live = strings.Replace(liveTable(t), `"name": "resolvers4", "table": "pco_egress", "type": "ipv4_addr", "handle": 7, "elem": ["192.168.1.1"]}`,
		`"name": "resolvers4", "table": "pco_egress", "type": "ipv4_addr", "handle": 7}`, 1)
	require.NotContains(t, r.nft.live, `"elem": ["192.168.1.1"]`)
	resolvers := []netip.Addr{netip.MustParseAddr("192.168.1.1")}
	r.env.resolvers = func() ([]netip.Addr, error) { return resolvers, nil }

	res := r.run("load")

	require.NoError(t, res.err)
	require.Equal(t, []string{egress.Base(testConnectorUID, resolvers, nil)}, r.nft.applied())
}

func TestEgressLoadWithABlockListThatCannotBeReadLoadsNothing(t *testing.T) {
	r := newEgressRig(t)
	require.NoError(t, os.MkdirAll(r.env.local, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(r.env.local, "egress-blocked.json"), []byte("{"), 0o600))

	res := r.run("load")

	require.ErrorContains(t, res.err, "egress-blocked.json")
	require.Empty(t, r.nft.applied())
}

func TestEgressShowOfATableThatIsNotAsItShouldBe(t *testing.T) {
	r := newEgressRig(t)
	r.nft.listErr = nil
	r.nft.live = strings.Replace(liveTable(t), `"name": "pco_egress", "handle": 3}`, `"name": "pco_egress", "handle": 3, "flags": "dormant"}`, 1)
	require.Contains(t, r.nft.live, "dormant")

	res := r.run("show")

	require.ErrorIs(t, res.err, errReported, "an exit status of 1")
	require.NotContains(t, res.out, "The egress filter is on.")
	requireGolden(t, "egress_show_changed.golden", res.out)
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
	resolvers := []netip.Addr{netip.MustParseAddr("192.168.1.1")}
	r.env.resolvers = func() ([]netip.Addr, error) { return resolvers, nil }

	res := r.run("on")

	require.NoError(t, res.err)
	_, off, err := r.overrides().Off()
	require.NoError(t, err)
	require.False(t, off)
	require.Equal(t, []string{egress.Base(testConnectorUID, resolvers, nil)}, r.nft.applied())
	require.Equal(t, "Switched the egress filter on and loaded its table: "+loadedText+" at its next cycle.\n", res.out)
}

func TestEgressOnWhenItIsOnAlready(t *testing.T) {
	r := newEgressRig(t)
	r.nft.listErr = nil
	r.nft.live = liveTable(t)

	res := r.run("on")

	require.NoError(t, res.err)
	require.Empty(t, r.nft.applied(), "the sets the daemon filled are kept")
	require.Equal(t, "The egress filter was on already, and its table is loaded.\n", res.out)
}

func TestEgressOnReadsTheBlockListBeforeItChangesAnything(t *testing.T) {
	r := newEgressRig(t)
	require.NoError(t, r.overrides().SwitchOff(t0))
	require.NoError(t, os.WriteFile(filepath.Join(r.env.local, "egress-blocked.json"), []byte("{"), 0o600))

	res := r.run("on")

	require.ErrorContains(t, res.err, "egress-blocked.json")
	require.Empty(t, r.nft.applied())
	_, off, err := r.overrides().Off()
	require.NoError(t, err)
	require.True(t, off, "the switch stays off: nothing could be loaded")
}

func TestEgressShowWithoutTheConnectorUserIsAFinding(t *testing.T) {
	r := newEgressRig(t)
	r.nft.listErr, r.nft.live = nil, liveTable(t)
	r.env.uid = func() (uint32, error) { return 0, fmt.Errorf("pco-connector: %w", egress.ErrNoConnectorUser) }

	res := r.run("show")

	require.ErrorIs(t, res.err, errReported, "an exit status of 1")
	require.True(t, strings.HasPrefix(res.out, "The connector user pco-connector does not exist: install the package or run systemd-sysusers."), res.out)
	require.Contains(t, res.out, "  10.0.0.5:8080\n", "what the table holds is shown")
	require.NotContains(t, res.out, "rule 1 differs")

	t.Run("another failure to look the user up", func(t *testing.T) {
		r.env.uid = func() (uint32, error) { return 0, errors.New("looking up user pco-connector: nss: timeout") }
		res := r.run("show")
		require.ErrorContains(t, res.err, "nss: timeout")
	})
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
	r.nft.live = liveTable(t)

	res := r.run("block", "10.0.0.5")

	require.NoError(t, res.err)
	require.Equal(t, []string{"delete element inet pco_egress targets4 { 10.0.0.5 . 80, 10.0.0.5 . 8080 }\n" +
		"add element inet pco_egress blocked4 { 10.0.0.5 }\n"}, r.nft.applied())
	blocked, err := r.overrides().Blocked()
	require.NoError(t, err)
	require.Equal(t, []netip.Addr{netip.MustParseAddr("10.0.0.5")}, blocked)
	require.Equal(t, "Blocked 10.0.0.5.\nTook 2 entries for it out of the egress table, which now rejects it on every port.\n", res.out)
}

func TestEgressBlockPrintsTheAddressAsItParsedIt(t *testing.T) {
	r := newEgressRig(t)
	r.nft.listErr = nil
	r.nft.live = liveTable(t)

	res := r.run("block", "::ffff:10.0.0.6")

	require.NoError(t, res.err)
	require.Equal(t, "Blocked 10.0.0.6.\nTook 1 entry for it out of the egress table, which now rejects it on every port.\n", res.out)
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
	r.nft.live = liveTable(t)
	_, err := r.overrides().Block(netip.MustParseAddr("10.0.0.99"))
	require.NoError(t, err)

	res := r.run("block", "10.0.0.99")

	require.NoError(t, res.err)
	require.Equal(t, []string{"add element inet pco_egress blocked4 { 10.0.0.99 }\n"}, r.nft.applied())
	require.Equal(t, "10.0.0.99 was blocked already.\nThe egress table held no entry for it, and now rejects it on every port.\n", res.out)
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

func TestEgressUnblockTakesTheAddressOutOfTheBlockedSetOfTheLiveTable(t *testing.T) {
	r := newEgressRig(t)
	_, err := r.overrides().Block(netip.MustParseAddr("10.0.0.9"))
	require.NoError(t, err)
	r.nft.listErr = nil
	r.nft.live = strings.Replace(liveTable(t), `"name": "blocked4", "table": "pco_egress", "type": "ipv4_addr", "handle": 9}`,
		`"name": "blocked4", "table": "pco_egress", "type": "ipv4_addr", "handle": 9, "elem": ["10.0.0.9"]}`, 1)
	require.Contains(t, r.nft.live, `"elem": ["10.0.0.9"]`)

	res := r.run("unblock", "10.0.0.9")

	require.NoError(t, res.err)
	require.Equal(t, []string{"delete element inet pco_egress blocked4 { 10.0.0.9 }\n"}, r.nft.applied())
}

func TestEgressShowGolden(t *testing.T) {
	tests := []struct {
		name     string
		golden   string
		findings bool // an exit status of 1
		setup    func(r *egressRig)
	}{
		{name: "on", golden: "egress_show.golden", setup: func(r *egressRig) {
			r.nft.listErr, r.nft.live = nil, liveTable(r.t)
			_, err := r.overrides().Block(netip.MustParseAddr("10.0.0.9"))
			require.NoError(r.t, err)
		}},
		{name: "on without a table", golden: "egress_show_unloaded.golden", findings: true, setup: func(*egressRig) {}},
		{name: "off", golden: "egress_show_off.golden", findings: true, setup: func(r *egressRig) {
			require.NoError(r.t, r.overrides().SwitchOff(t0))
		}},
		{name: "off with a table", golden: "egress_show_off_loaded.golden", findings: true, setup: func(r *egressRig) {
			r.nft.listErr, r.nft.live = nil, liveTable(r.t)
			require.NoError(r.t, r.overrides().SwitchOff(t0))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newEgressRig(t)
			tc.setup(r)

			res := r.run("show")

			if tc.findings {
				require.ErrorIs(t, res.err, errReported)
			} else {
				require.NoError(t, res.err)
			}
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

	require.ErrorIs(t, res.err, errReported)
	require.Contains(t, res.out, "The egress filter is switched off since an unknown time")
}
