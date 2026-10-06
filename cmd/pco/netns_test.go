package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
)

// fakeGateway is a backend that remembers what it was asked.
type fakeGateway struct {
	calls     []string
	legs      []gateway.Leg
	services  []gateway.Service
	pins      []gateway.Pin
	sticky    []gateway.Sticky
	states    []gateway.LegState
	verifyErr error
	upErr     error
}

func (g *fakeGateway) Up(context.Context) error {
	g.calls = append(g.calls, "up")
	return g.upErr
}

func (g *fakeGateway) Sync(_ context.Context, legs []gateway.Leg, services []gateway.Service, pins []gateway.Pin, sticky []gateway.Sticky) ([]gateway.LegState, error) {
	g.calls = append(g.calls, "sync")
	g.legs, g.services, g.pins, g.sticky = legs, services, pins, sticky
	return g.states, nil
}

func (g *fakeGateway) Verify(context.Context) error {
	g.calls = append(g.calls, "verify")
	return g.verifyErr
}

func (g *fakeGateway) Probe(context.Context, string, int, netip.Addr) ([]net.HardwareAddr, error) {
	return nil, nil
}

func (g *fakeGateway) Detach(context.Context, string) error { return nil }

func (g *fakeGateway) Down(context.Context) error {
	g.calls = append(g.calls, "down")
	return nil
}

func (g *fakeGateway) Netns() netns.NsHandle { return netns.None() }

func (g *fakeGateway) Dial(context.Context, gateway.Leg, netip.AddrPort) (net.Conn, error) {
	return nil, errors.New("no dial in this test")
}

type netnsRig struct {
	t       *testing.T
	app     *app
	env     netnsEnv
	gw      *fakeGateway
	running int
	daemon  bool
}

func newNetnsRig(t *testing.T) *netnsRig {
	r := &netnsRig{t: t, app: &app{env: testEnv()}, gw: &fakeGateway{}}
	r.env = netnsEnv{
		backend:    func(zerolog.Logger) gateway.Backend { return r.gw },
		local:      t.TempDir(),
		euid:       func() int { return 0 },
		now:        func() time.Time { return t0 },
		connectors: func(context.Context) (int, error) { return r.running, nil },
		daemon:     func(context.Context) (bool, error) { return r.daemon, nil },
	}
	return r
}

func (r *netnsRig) run(args ...string) result {
	r.t.Helper()
	cmd := r.app.netnsCmdWith(r.env)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(unreadable{r.t})
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(r.t.Context())
	return result{out: out.String(), errOut: errOut.String(), err: err}
}

var (
	netnsLegA = gateway.Leg{
		ID: "vmbr1", Mark: 1, Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.50.0.250/24"),
		MAC: net.HardwareAddr{0x02, 0x50, 0x43, 0x4f, 0x00, 0x01},
	}
	netnsLegB = gateway.Leg{
		ID: "vmbr0.20", Mark: 2, Bridge: "vmbr0", VLAN: 20, Prefix: netip.MustParsePrefix("10.50.0.250/24"),
		MAC: net.HardwareAddr{0x02, 0x50, 0x43, 0x4f, 0x00, 0x02},
	}
	netnsServices = []gateway.Service{
		{Leg: "vmbr1", Service: netip.MustParseAddr("198.18.1.0"), Addr: netip.MustParseAddr("10.50.0.10"), Ports: []uint16{80, 443}},
		{Leg: "vmbr0.20", Service: netip.MustParseAddr("198.18.1.1"), Addr: netip.MustParseAddr("10.50.0.10"), Ports: []uint16{80}},
	}
	netnsPins = []gateway.Pin{{Leg: "vmbr1", Addr: netip.MustParseAddr("10.50.0.10"), MAC: net.HardwareAddr{0xbc, 0x24, 0x11, 0, 0, 1}}}
)

func (r *netnsRig) cache(pinsAt time.Time) gateway.Cache {
	r.t.Helper()
	c := gateway.Cache{Legs: []gateway.Leg{netnsLegA, netnsLegB}, Services: netnsServices, Pins: netnsPins, PinsAt: pinsAt}
	require.NoError(r.t, gateway.WriteCache(r.env.local, c))
	return c
}

func TestNetnsIsACommandOfPco(t *testing.T) {
	root := newRootCmdWith(testEnv())
	for _, tt := range []struct {
		name   string
		hidden bool
	}{{"up", true}, {"sync", true}, {"down", true}, {"status", false}} {
		cmd, _, err := root.Find([]string{"netns", tt.name})
		require.NoError(t, err)
		require.Equal(t, tt.name, cmd.Name())
		require.Equal(t, tt.hidden, cmd.Hidden, tt.name)
	}
}

// pco netns up brings the namespace up, then the cached legs and services,
// with the pins while they are fresh, and prints a line for each leg.
func TestNetnsUpRestoresTheCache(t *testing.T) {
	r := newNetnsRig(t)
	c := r.cache(t0.Add(-time.Minute))
	r.gw.states = []gateway.LegState{
		{ID: "vmbr1", Up: true, Iface: "leg1"},
		{ID: "vmbr0.20", Iface: "leg2", Problem: "bridge vmbr0v20 is absent"},
	}
	res := r.run("up")
	require.NoError(t, res.err)
	require.Equal(t, []string{"up", "sync"}, r.gw.calls)
	require.Equal(t, c.Legs, r.gw.legs)
	require.Equal(t, c.Services, r.gw.services)
	require.Equal(t, c.Pins, r.gw.pins)
	require.Equal(t, "leg leg1 up 10.50.0.250/24 on vmbr1\n"+
		"leg leg2 down on vmbr0 VLAN 20: bridge vmbr0v20 is absent\n"+
		"service addresses: 2, with 3 published ports\n"+
		"pinned neighbours: 1\n", res.out)
}

func TestNetnsUpSkipsStalePins(t *testing.T) {
	r := newNetnsRig(t)
	r.cache(t0.Add(-gateway.PinsFresh))
	res := r.run("up")
	require.NoError(t, res.err)
	require.Nil(t, r.gw.pins)
	require.Contains(t, res.out, "pinned neighbours: those in the cache are older than five minutes; the daemon pins them anew\n")
}

func TestNetnsUpWithoutACacheBringsTheBaseOnly(t *testing.T) {
	r := newNetnsRig(t)
	res := r.run("up")
	require.NoError(t, res.err)
	require.Equal(t, []string{"up", "sync"}, r.gw.calls)
	require.Empty(t, r.gw.legs)
	require.Equal(t, "No network is attached: the namespace has its transit link and its tables only.\n"+
		"service addresses: 0, with 0 published ports\n", res.out)
}

// A cache that cannot be read never leaves the node without the namespace,
// its tables and the unreachable route of the service prefix: Up runs without
// legs or service addresses, which the daemon's next Sync restores, and the
// unit fails so that the error is seen.
func TestNetnsUpWithACacheItCannotReadBringsTheBaseUp(t *testing.T) {
	for _, tt := range []struct {
		name  string
		write func(t *testing.T, path string)
		want  string
	}{
		{"a field this version does not know", func(t *testing.T, path string) {
			require.NoError(t, os.WriteFile(path, []byte(`{"legs": [], "services": [], "routes": []}`), 0o600))
		}, `json: unknown field "routes"`},
		{"no JSON", func(t *testing.T, path string) {
			require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
		}, "unexpected end of JSON input"},
		{"no file", func(t *testing.T, path string) {
			require.NoError(t, os.Mkdir(path, 0o700))
		}, "is a directory"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newNetnsRig(t)
			path := filepath.Join(r.env.local, gateway.CacheFile)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			tt.write(t, path)
			res := r.run("up")
			require.Equal(t, []string{"up"}, r.gw.calls, "Up, and no Sync, which would remove the legs that are up")
			require.ErrorContains(t, res.err, "the gateway namespace is up without legs or service addresses until the daemon restores them: ")
			require.ErrorContains(t, res.err, path)
			require.ErrorContains(t, res.err, tt.want)
		})
	}
}

func TestNetnsUpThatFailsFailsTheUnit(t *testing.T) {
	r := newNetnsRig(t)
	r.gw.upErr = errors.New("no namespace")
	res := r.run("up")
	require.EqualError(t, res.err, "bringing up the gateway namespace: no namespace")
	require.Equal(t, []string{"up"}, r.gw.calls)
}

func TestNetnsNeedsRootAndTheHostProfile(t *testing.T) {
	r := newNetnsRig(t)
	r.env.euid = func() int { return 1000 }
	require.EqualError(t, r.run("up").err, "pco netns needs root: it reads and changes the network namespaces of the node")

	r = newNetnsRig(t)
	profile := filepath.Join(t.TempDir(), "profile")
	require.NoError(t, os.WriteFile(profile, []byte("appliance\n"), 0o644))
	r.app.profileFile = profile
	require.EqualError(t, r.run("status").err,
		"pco netns is for the host profile: in the appliance the legs are network cards of the container")
	require.Empty(t, r.gw.calls)
}

// Down without --uninstall refuses, so that nobody makes it a stop action
// of a unit.
func TestNetnsDownRefusesWithoutUninstall(t *testing.T) {
	r := newNetnsRig(t)
	res := r.run("down")
	require.EqualError(t, res.err, "pco netns down removes the gateway; run pco uninstall")
	require.Empty(t, r.gw.calls)
}

func TestNetnsDownRefusesWhileConnectorsServeUnlessYes(t *testing.T) {
	r := newNetnsRig(t)
	r.cache(t0)
	r.running = 2
	res := r.run("down", "--uninstall")
	require.EqualError(t, res.err, "2 connectors run and may serve the 2 service addresses of the gateway: "+
		"stop them first, as pco uninstall does, or pass --yes")
	require.Empty(t, r.gw.calls)

	res = r.run("down", "--uninstall", "--yes")
	require.NoError(t, res.err)
	require.Equal(t, []string{"down"}, r.gw.calls)

	r.gw.calls, r.running = nil, 0
	require.NoError(t, r.run("down", "--uninstall").err)
	require.Equal(t, []string{"down"}, r.gw.calls)
}

func TestNetnsStatus(t *testing.T) {
	r := newNetnsRig(t)
	r.cache(t0)
	res := r.run("status")
	require.NoError(t, res.err)
	require.Equal(t, []string{"verify"}, r.gw.calls)
	require.Contains(t, res.out, "Legs:\n  leg1  vmbr1     vmbr1  VLAN -   10.50.0.250/24  02:50:43:4f:00:01\n")
	require.Contains(t, res.out, "The namespace pco is as pco left it.\n")

	r.gw.verifyErr = &gateway.ChangedError{Differences: []string{"vethpco1 is missing in the host"}}
	res = r.run("status")
	require.ErrorIs(t, res.err, errReported)
	require.Contains(t, res.out, "The namespace pco is not as pco left it; systemctl restart pco-netns brings back what is missing:\n"+
		"  vethpco1 is missing in the host\n")

	r.gw.verifyErr = errors.New("netlink: permission denied")
	require.EqualError(t, r.run("status").err, "netlink: permission denied")
}

// sync --from applies a file as the daemon would, and keeps it as the cache
// that pco netns up restores.
// While the daemon runs it owns the namespace: the two would undo each
// other's changes.
func TestNetnsSyncRefusesWhileTheDaemonRunsUnlessYes(t *testing.T) {
	r := newNetnsRig(t)
	r.daemon = true
	file := filepath.Join(t.TempDir(), "legs.json")
	require.NoError(t, os.WriteFile(file, []byte(`{"legs": [], "services": []}`), 0o644))
	res := r.run("sync", "--from", file)
	require.EqualError(t, res.err, "pco.service runs and keeps the gateway as the store says: stop it first, or pass --yes")
	require.Empty(t, r.gw.calls)

	require.NoError(t, r.run("sync", "--from", file, "--yes").err)
	require.Equal(t, []string{"up", "sync"}, r.gw.calls)
}

// Down for an uninstall does not need the cache: with a cache it cannot read
// it still refuses while connectors run.
func TestNetnsDownWithACacheItCannotRead(t *testing.T) {
	r := newNetnsRig(t)
	path := filepath.Join(r.env.local, gateway.CacheFile)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
	r.running = 1
	require.ErrorContains(t, r.run("down", "--uninstall").err, "1 connectors run and may serve the service addresses of the gateway")
	require.Empty(t, r.gw.calls)
	r.running = 0
	require.NoError(t, r.run("down", "--uninstall").err)
	require.Equal(t, []string{"down"}, r.gw.calls)
}

func TestNetnsSyncFromAFile(t *testing.T) {
	r := newNetnsRig(t)
	file := filepath.Join(t.TempDir(), "legs.json")
	require.NoError(t, os.WriteFile(file, []byte(`{
  "legs": [{"id": "vmbr1", "mark": 1, "bridge": "vmbr1", "prefix": "10.50.0.250/24", "mac": "02:50:43:4f:00:01", "uplink": true}],
  "services": [{"leg": "vmbr1", "service": "198.18.1.0", "addr": "10.50.0.10", "ports": [80]}],
  "pins": [{"leg": "vmbr1", "addr": "10.50.0.10", "mac": "bc:24:11:00:00:01"}]
}`), 0o644))
	r.gw.states = []gateway.LegState{{ID: "vmbr1", Up: true, Iface: "leg1", Problem: "no carrier, DAD not run"}}
	res := r.run("sync", "--from", file)
	require.NoError(t, res.err)
	require.Equal(t, []string{"up", "sync"}, r.gw.calls)
	require.Len(t, r.gw.legs, 1)
	require.True(t, r.gw.legs[0].Uplink)
	require.Equal(t, "leg leg1 up 10.50.0.250/24 on vmbr1: no carrier, DAD not run\n"+
		"service addresses: 1, with 1 published ports\npinned neighbours: 1\n", res.out)
	c, ok, err := gateway.ReadCache(r.env.local)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, t0, c.PinsAt)
	require.Equal(t, r.gw.legs, c.Legs)

	require.NoError(t, os.WriteFile(file, []byte(`{"legs": [], "services": [], "extra": 1}`), 0o644))
	require.ErrorContains(t, r.run("sync", "--from", file).err, `unknown field "extra"`)
}

// The unit has no stop action: a stop or a restart of it, by an upgrade or by
// hand, leaves the namespace, the legs and the routes of the service prefix
// as they are, and a restart only runs pco netns up again.
func TestTheUnitOfTheGatewayNamespace(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "systemd", "pco-netns.service"))
	require.NoError(t, err)
	require.Equal(t, `[Unit]
Description=pco gateway network namespace
Documentation=https://github.com/anaryk/proxmox-cloudflared-operator
After=network-online.target pve-cluster.service
Wants=network-online.target
Before=pco.service pve-guests.service pve-ha-lrm.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/bin/pco netns up

[Install]
WantedBy=multi-user.target
`, string(b))
	require.NotContains(t, string(b), "ExecStop")

	// The daemon and the connectors start after it where it runs, and
	// require nothing of it: a node without an attached network never
	// enables it.
	for _, name := range []string{"pco.service", "pco-cloudflared@.service"} {
		u := parseUnit(t, filepath.Join("..", "..", "packaging", "systemd", name))
		require.Contains(t, strings.Fields(u.one(t, "Unit", "After")), "pco-netns.service", name)
		for _, req := range append(u["Unit"]["Requires"], u["Unit"]["Wants"]...) {
			require.NotContains(t, strings.Fields(req), "pco-netns.service", name)
		}
	}
}

// The package scripts never stop or restart the unit; an upgrade runs
// pco netns up where it is enabled, before the daemon restarts.
func TestThePackageScriptsLeaveTheGatewayNamespaceRunning(t *testing.T) {
	for _, name := range []string{"postinstall.sh", "preremove.sh", "postremove.sh"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "scripts", name))
		require.NoError(t, err)
		for line := range strings.Lines(string(b)) {
			if strings.HasPrefix(strings.TrimSpace(line), "#") || !strings.Contains(line, "pco-netns") {
				continue
			}
			require.NotRegexp(t, `\b(stop|restart|try-restart|disable|kill)\b`, line, "%s: %s", name, line)
		}
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "scripts", "postinstall.sh"))
	require.NoError(t, err)
	up := strings.Index(string(b), "/usr/bin/pco netns up")
	require.Positive(t, up)
	require.Less(t, up, strings.Index(string(b), "systemctl try-restart pco.service"))
}
