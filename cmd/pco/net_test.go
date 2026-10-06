package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
)

// netLink is the network of a container: what Load puts in place is there
// once it was added, and gone again when a test takes it.
type netLink struct {
	have      map[string]bool
	ensureErr error
}

func (n *netLink) ensure(what string) error {
	if n.ensureErr != nil {
		return n.ensureErr
	}
	n.have[what] = true
	return nil
}

func (n *netLink) EnsureDummy(context.Context, string) error { return n.ensure("device") }

func (n *netLink) HasDummy(context.Context, string) (bool, error) { return n.have["device"], nil }

func (n *netLink) EnsureAddr(context.Context, string, netip.Prefix) error { return n.ensure("address") }

func (n *netLink) HasAddr(context.Context, string, netip.Prefix) (bool, error) {
	return n.have["address"], nil
}

func (n *netLink) EnsureRoute(context.Context, netip.Prefix, string, netip.Addr) error {
	return n.ensure("route")
}

func (n *netLink) HasRoute(context.Context, netip.Prefix, string, netip.Addr) (bool, error) {
	return n.have["route"], nil
}

func (n *netLink) EnsureRule(_ context.Context, pref int, _ netip.Addr, _ netip.Prefix, _ bool) error {
	return n.ensure(fmt.Sprint("rule ", pref))
}

func (n *netLink) HasRule(_ context.Context, pref int, _ netip.Addr, _ netip.Prefix, _ bool) (bool, error) {
	return n.have[fmt.Sprint("rule ", pref)], nil
}

// netTableNft holds inet pco_net as nft 1.1.3 lists it once it was loaded.
type netTableNft struct {
	listing string
	live    string
	scripts []string
}

func (n *netTableNft) Apply(_ context.Context, script string) error {
	n.scripts = append(n.scripts, script)
	n.live = n.listing
	return nil
}

func (n *netTableNft) List(context.Context) ([]byte, error) {
	if n.live == "" {
		return nil, egress.ErrNotLoaded
	}
	return []byte(n.live), nil
}

type netRig struct {
	t   *testing.T
	app *app
	env netEnv
	nl  *netLink
	nft *netTableNft
}

// newNetRig runs pco net as root in an appliance whose network has nothing
// of pco-net.service's yet.
func newNetRig(t *testing.T) *netRig {
	listing, err := os.ReadFile(filepath.Join("..", "..", "internal", "appnet", "testdata", "listing-1.1.3.json"))
	require.NoError(t, err)
	profile := filepath.Join(t.TempDir(), "profile")
	require.NoError(t, os.WriteFile(profile, []byte("appliance\n"), 0o644))
	e := testEnv()
	e.profileFile = profile
	r := &netRig{t: t, app: &app{env: e}, nl: &netLink{have: map[string]bool{}}, nft: &netTableNft{listing: string(listing)}}
	r.env = netEnv{nl: r.nl, nft: r.nft, euid: func() int { return 0 }}
	return r
}

func (r *netRig) loaded() *netRig {
	r.t.Helper()
	require.NoError(r.t, r.run("load").err)
	r.nft.scripts = nil
	return r
}

func (r *netRig) run(args ...string) result {
	r.t.Helper()
	cmd := r.app.netCmdWith(r.env)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(unreadable{r.t})
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(r.t.Context())
	return result{out: out.String(), errOut: errOut.String(), err: err}
}

func TestNetIsACommandOfPco(t *testing.T) {
	root := newRootCmdWith(testEnv())

	cmd, _, err := root.Find([]string{"net", "show"})
	require.NoError(t, err)
	require.Equal(t, "show", cmd.Name())
	load, _, err := root.Find([]string{"net", "load"})
	require.NoError(t, err)
	require.True(t, load.Hidden, "run by pco-net.service")
}

func TestNetIsForTheApplianceProfile(t *testing.T) {
	for _, sub := range []string{"load", "show"} {
		t.Run(sub, func(t *testing.T) {
			r := newNetRig(t)
			r.app.profileFile = filepath.Join(t.TempDir(), "none")

			res := r.run(sub)

			require.EqualError(t, res.err, "pco net is for the appliance profile")
			require.Equal(t, 1, exitCode(res.err, io.Discard))
			require.Empty(t, r.nft.scripts)
			require.Empty(t, r.nl.have)
		})
	}
}

func TestNetNeedsRoot(t *testing.T) {
	r := newNetRig(t)
	r.env.euid = func() int { return 1000 }

	res := r.run("show")

	require.EqualError(t, res.err, "pco net needs root: it reads and changes the network of the appliance")
}

func TestNetRefusesJSON(t *testing.T) {
	r := newNetRig(t)
	r.app.json = true

	require.ErrorContains(t, r.run("show").err, "--json has no meaning")
}

func TestNetLoadPutsEverythingInPlace(t *testing.T) {
	r := newNetRig(t)

	res := r.run("load")

	require.NoError(t, res.err)
	require.Equal(t, "Loaded what pco-net.service keeps in place:\n"+
		"  the dummy device pco0\n"+
		"  the address 198.18.0.1/32 on pco0\n"+
		"  the route 198.18.0.0/16 dev pco0 src 198.18.0.1\n"+
		"  the rule 1890: from 198.18.0.1 to 198.18.0.0/16 lookup main\n"+
		"  the rule 1900: from 198.18.0.1 unreachable\n"+
		"  the table inet pco_net\n", res.out)
	require.Len(t, r.nft.scripts, 1)

	t.Run("and nothing a second time", func(t *testing.T) {
		res := r.run("load")

		require.NoError(t, res.err)
		require.Equal(t, "Everything pco-net.service keeps in place is there.\n", res.out)
		require.Len(t, r.nft.scripts, 1)
	})
}

// What pco-net.service runs fails, so that the connectors and the daemon,
// which require the unit, do not start.
func TestNetLoadFailsWhenAStepFails(t *testing.T) {
	r := newNetRig(t)
	r.nl.ensureErr = errors.New("operation not permitted")

	res := r.run("load")

	require.EqualError(t, res.err, "adding the dummy device pco0: operation not permitted")
	require.Equal(t, 1, exitCode(res.err, io.Discard))
}

func TestNetShowSaysAllIsInPlace(t *testing.T) {
	r := newNetRig(t).loaded()
	r.nft.live = strings.Replace(r.nft.live, `"packets": 0, "bytes": 0`, `"packets": 3, "bytes": 180`, 1)

	res := r.run("show")

	require.NoError(t, res.err)
	require.Equal(t, "The service prefix 198.18.0.0/16 stays in the appliance: it is routed to pco0, "+
		"and the table inet pco_net rejects what is still sent to it.\n\n"+
		"  the dummy device pco0                                        in place\n"+
		"  the address 198.18.0.1/32 on pco0                            in place\n"+
		"  the route 198.18.0.0/16 dev pco0 src 198.18.0.1              in place\n"+
		"  the rule 1890: from 198.18.0.1 to 198.18.0.0/16 lookup main  in place\n"+
		"  the rule 1900: from 198.18.0.1 unreachable                   in place\n"+
		"  the table inet pco_net                                       in place\n\n"+
		"Rejected since the table was loaded: 3 packets\n", res.out)
	require.Empty(t, r.nft.scripts, "show changes nothing")
}

// In a container without network, as the smoke test boots the template,
// pco-net.service could not add the device: show says so and exits 1.
func TestNetShowNamesWhatIsMissing(t *testing.T) {
	r := newNetRig(t)

	res := r.run("show")

	require.ErrorIs(t, res.err, errReported)
	require.Equal(t, 1, exitCode(res.err, io.Discard))
	require.Contains(t, res.out, "The service prefix 198.18.0.0/16 is not kept in the appliance as pco-net.service keeps it; "+
		"pco net load puts it back:\n"+
		"  the dummy device pco0 is missing or down\n")
	require.Contains(t, res.out, "  the dummy device pco0                                        missing\n")
	require.Contains(t, res.out, "  the table inet pco_net                                       missing\n")
	require.Contains(t, res.out, "Rejected since the table was loaded: unknown, the table is not loaded\n")
	require.Empty(t, r.nl.have, "show changes nothing")
}

func TestNetShowNamesAChangedTable(t *testing.T) {
	r := newNetRig(t).loaded()
	r.nft.live = strings.Replace(r.nft.live, `"handle": 3}}`, `"handle": 3, "flags": "dormant"}}`, 1)

	res := r.run("show")

	require.ErrorIs(t, res.err, errReported)
	require.Contains(t, res.out, "  the table inet pco_net has flags dormant\n")
	require.Contains(t, res.out, "  the table inet pco_net                                       not as pco loads it\n")
}
