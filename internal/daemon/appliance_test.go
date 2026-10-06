package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	applianceVMID = 9250
	applianceMAC  = "bc:24:11:00:92:50"
	bootID        = "6c3f2a8e-1b4d-4e0f-9a2b-7d5c8e1f3a6b"
	// incarnation is the start of the container in the /proc of the tests.
	incarnation = bootID + "/123456789"
	statOfPID1  = "1 (systemd) S 0 1 1 0 -1 4194560 12971 3058829 103 1393 41 87 22411 8399 20 0 1 0 123456789 23302144 2969 18446744073709551615 1 1 0 0\n"
)

// applianceWorld is a world whose daemon runs in the appliance lxc/9250: its
// volume is mounted at the local root, its /proc and /sys are in the test's
// directory, and Proxmox lists the appliance besides the guest.
type applianceWorld struct {
	*world
	sys    appliance.System
	flag   string
	net    *fakeAppNet
	netNft *fakeNetNft
}

// fakeAppNet is the network of the container: everything pco-net.service
// loads is there but what a test takes away.
type fakeAppNet struct {
	mu      sync.Mutex
	missing map[string]bool
	added   []string
}

func (f *fakeAppNet) take(what string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.missing[what] = true
}

func (f *fakeAppNet) has(what string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.missing[what], nil
}

func (f *fakeAppNet) ensure(what string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.missing, what)
	f.added = append(f.added, what)
	return nil
}

func (f *fakeAppNet) addedSoFar() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.added)
}

func (f *fakeAppNet) EnsureDummy(context.Context, string) error { return f.ensure("device") }

func (f *fakeAppNet) HasDummy(context.Context, string) (bool, error) { return f.has("device") }

func (f *fakeAppNet) EnsureAddr(context.Context, string, netip.Prefix) error {
	return f.ensure("address")
}

func (f *fakeAppNet) HasAddr(context.Context, string, netip.Prefix) (bool, error) {
	return f.has("address")
}

func (f *fakeAppNet) EnsureRoute(context.Context, netip.Prefix, string, netip.Addr) error {
	return f.ensure("route")
}

func (f *fakeAppNet) HasRoute(context.Context, netip.Prefix, string, netip.Addr) (bool, error) {
	return f.has("route")
}

func (f *fakeAppNet) EnsureRule(_ context.Context, pref int, _ netip.Addr, _ netip.Prefix, _ bool) error {
	return f.ensure(fmt.Sprint("rule ", pref))
}

func (f *fakeAppNet) HasRule(_ context.Context, pref int, _ netip.Addr, _ netip.Prefix, _ bool) (bool, error) {
	return f.has(fmt.Sprint("rule ", pref))
}

// fakeNetNft holds inet pco_net as nft lists it once it was loaded.
type fakeNetNft struct {
	mu      sync.Mutex
	listing []byte
	live    []byte
}

func (f *fakeNetNft) Apply(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live = f.listing
	return nil
}

func (f *fakeNetNft) List(context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.live == nil {
		return nil, egress.ErrNotLoaded
	}
	return f.live, nil
}

func newApplianceWorld(t *testing.T) *applianceWorld {
	t.Helper()
	w := newWorld(t)
	listing, err := os.ReadFile(filepath.Join("..", "appnet", "testdata", "listing-1.1.3.json"))
	require.NoError(t, err)
	a := &applianceWorld{
		world:  w,
		sys:    appliance.System{Proc: filepath.Join(w.dir, "proc"), Sys: filepath.Join(w.dir, "sys")},
		flag:   filepath.Join(w.dir, "run", "pco-appliance", "identity-ok"),
		net:    &fakeAppNet{missing: map[string]bool{}},
		netNft: &fakeNetNft{listing: listing, live: listing},
	}
	a.mountVolumeOf(applianceVMID)
	a.proc("uptime", "3600.00 7000.00\n")
	a.proc("sys/kernel/random/boot_id", bootID+"\n")
	a.proc("1/stat", statOfPID1)
	a.proc("net/route", "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n"+
		"eth0\t00000000\t0100140A\t0003\t0\t0\t0\t00000000\t0\t0\t0\n")
	for name, content := range map[string]string{"address": applianceMAC, "ifindex": "2", "iflink": "23"} {
		a.write(filepath.Join(a.sys.Sys, "class", "net", "eth0", name), content+"\n")
	}

	require.NoError(t, w.store.SaveInstall(store.Install{ID: testInstall, CreatedAt: t0, Profile: store.ProfileAppliance, Appliance: &store.ApplianceInstall{
		VMID: applianceVMID, Node: testNode, MACs: []string{applianceMAC},
		Endpoints: []store.Endpoint{{Address: strings.TrimPrefix(w.pve.srv.URL, "https://"), ServerName: testNode}},
		CAFile:    w.cfg.PVECAFile,
	}}))
	require.NoError(t, w.store.SaveWriter(planner.Writer{InstallID: testInstall, Generation: 1, Nonce: "n1", Incarnation: incarnation}))
	w.pve.answer("/api2/json/cluster/resources", []map[string]any{
		{"type": "qemu", "vmid": 101, "name": "web-1", "node": testNode, "status": "running", "template": 0, "tags": "cf-tunnel"},
		{"type": "lxc", "vmid": applianceVMID, "name": "pco", "node": testNode, "status": "running", "template": 0, "pool": "pco", "uptime": 3600},
	})
	w.pve.answer("/api2/json/nodes/pve1/lxc/9250/config", map[string]any{
		"hostname": "pco",
		"net0":     "name=eth0,bridge=vmbr1,hwaddr=" + strings.ToUpper(applianceMAC) + ",ip=dhcp,type=veth",
		"digest":   "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c",
	})
	// The access control, which the identity flag waits for: root and pco.
	w.pve.answer("/api2/json/access/permissions", map[string]any{"/access": map[string]int{"Sys.Audit": 1}, "/access/groups": map[string]int{"Sys.Audit": 1}})
	w.pve.answer("/api2/json/access/acl", []map[string]any{{"path": "/", "type": "token", "ugid": pveTokenID, "roleid": "PCO", "propagate": 1}})
	w.pve.answer("/api2/json/access/users", []map[string]any{
		{"userid": "root@pam", "enable": 1},
		{"userid": "pco@pve", "enable": 1, "tokens": []map[string]any{{"tokenid": "pco", "privsep": 1}}},
	})
	w.pve.answer("/api2/json/access/groups", []map[string]any{})
	w.pve.answer("/api2/json/access/roles", []map[string]any{{"roleid": "PCO", "privs": "Pool.Audit,SDN.Audit,Sys.Audit,VM.Audit"}})

	w.cfg.Profile = store.ProfileAppliance
	w.cfg.Node = "pco" // the host name of the container, which --node defaults to
	w.cfg.PVEURL = DefaultPVEURL
	w.deps.Appliance = ApplianceDeps{
		Volume:     func(string, string) error { return nil },
		System:     a.sys,
		Flag:       a.flag,
		StateRetry: time.Millisecond,
		Netlink:    a.net,
		NetNft:     a.netNft,
		Hostname:   func() (string, error) { return "pco", nil },
		Net0:       filepath.Join(w.dir, "etc", "pco", "net0"),
	}
	// The web interface of the container, as pco appliance install sets it.
	w.deps.WebDir = filepath.Join(w.dir, "etc", "pco", "web")
	w.deps.WebEnv = filepath.Join(w.dir, "etc", "default", "pco-web")
	w.deps.WebLoaded = filepath.Join(w.dir, "run", "credentials", "pco-web.service")
	w.deps.RestartWeb = func(context.Context) error { return nil }
	a.write(w.deps.Appliance.Net0, "10.92.0.150\n")
	a.write(w.deps.WebEnv, "PCO_WEB_LISTEN=10.92.0.150:8643\n")
	w.deps.Sleep = func(ctx context.Context, _ time.Duration) error {
		select {
		case <-time.After(5 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return a
}

// bounded is a context that ends after a few seconds: a daemon that should
// have ended at once and serves instead fails the test by what it returns.
func bounded(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func (a *applianceWorld) write(path, content string) {
	a.t.Helper()
	require.NoError(a.t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(a.t, os.WriteFile(path, []byte(content), 0o644))
}

func (a *applianceWorld) proc(rel, content string) { a.write(filepath.Join(a.sys.Proc, rel), content) }

// mountVolumeOf mounts a volume of vmid at the local root.
func (a *applianceWorld) mountVolumeOf(vmid int) {
	a.proc("self/mountinfo", fmt.Sprintf("674 500 0:74 / / rw,relatime shared:501 - zfs pcotestpool/subvol-%d-disk-0 rw\n"+
		"675 674 0:80 / %s rw,relatime shared:502 - zfs pcotestpool/subvol-%d-disk-1 rw\n", applianceVMID, a.paths.Local, vmid))
}

func TestTheApplianceRunsAsTheNodeOfItsInstall(t *testing.T) {
	a := newApplianceWorld(t)
	d := a.start()

	st := d.await(func(st engine.State) bool { return st.Identity != nil && st.Identity.OK })

	require.Equal(t, testNode, st.Node, "the install's node, not the host name of the container")
	require.Equal(t, store.ProfileAppliance, st.Profile)
	require.Equal(t, &engine.IdentityView{VMID: applianceVMID, Node: testNode, OK: true, CheckedAt: st.Identity.CheckedAt}, st.Identity)
	require.FileExists(t, a.flag)
	require.True(t, st.EpochDrawnAt.IsZero(), "the epoch of this start is kept")
	require.NoError(t, d.stop())
	require.Contains(t, a.logs.String(), `"profile":"appliance"`)
	require.Contains(t, a.logs.String(), `"installProfile":"appliance"`)
}

func TestWithoutItsVolumeTheApplianceEndsBeforeTheLock(t *testing.T) {
	a := newApplianceWorld(t)
	a.deps.Appliance.Volume = func(path, _ string) error { return fmt.Errorf("%s %w", path, appliance.ErrNoMarker) }

	err := Run(bounded(t), a.cfg, a.deps)

	var none NoVolumeError
	require.ErrorAs(t, err, &none)
	require.Equal(t, appliance.ExitNoVolume, none.ExitCode())
	require.Equal(t, a.paths.Local+" has no pco volume marker (restore, or a volume that is not pco's?): "+
		"run pco appliance repair --vmid 9250 on the node", err.Error())
	require.NoFileExists(t, store.Paths{Local: a.paths.Local}.NodeLock(), "the lock was not taken")
	require.Empty(t, a.notify.sent())
	require.Contains(t, a.logs.String(), "has no pco volume marker")
}

func TestTheVolumeIsCheckedBeforeTheLock(t *testing.T) {
	a := newApplianceWorld(t)
	var checked []string
	a.deps.Appliance.Volume = func(path, marker string) error {
		lock, err := lockNode(path)
		require.NoError(t, err, "nobody holds the lock yet")
		lock.release()
		checked = append(checked, filepath.Join(path, marker))
		return nil
	}

	d := a.start()
	require.NoError(t, d.stop())

	require.Equal(t, []string{filepath.Join(a.paths.Local, store.VolumeMarker)}, checked)
}

func TestWithoutStateTheSocketSaysWhatToDoAndNoCycleRuns(t *testing.T) {
	a := newApplianceWorld(t)
	require.NoError(t, os.Remove(filepath.Join(a.paths.Cluster, "meta", "install.json")))
	const line = "pco has no state on its volume (restore?): run pco appliance repair --vmid 9250 on the node"

	d := a.start()
	st := d.state()

	require.Equal(t, []string{line}, st.Problems)
	require.Equal(t, store.ProfileAppliance, st.Profile)
	_, err := d.client.Apply(t.Context(), false, "")
	require.ErrorIs(t, err, engine.ErrRefused)
	require.ErrorContains(t, err, line)
	_, err = d.client.Settings(t.Context())
	require.ErrorContains(t, err, line, "no settings without a state")
	require.ErrorIs(t, d.client.Restart(t.Context()), engine.ErrRefused)
	require.Empty(t, a.pve.authorizations(), "no cycle")
	require.NoFileExists(t, a.flag)

	// A repair puts the state back: the daemon starts on it.
	require.NoError(t, a.store.SaveInstall(store.Install{ID: testInstall, CreatedAt: t0, Profile: store.ProfileAppliance, Appliance: &store.ApplianceInstall{
		VMID: applianceVMID, Node: testNode, MACs: []string{applianceMAC},
		Endpoints: []store.Endpoint{{Address: strings.TrimPrefix(a.pve.srv.URL, "https://"), ServerName: testNode}},
		CAFile:    a.cfg.PVECAFile,
	}}))
	d.await(func(st engine.State) bool { return st.Identity != nil && st.Identity.OK })
}

func TestACopyIsStoppedBeforeTheFirstCycle(t *testing.T) {
	a := newApplianceWorld(t)
	m := connector.NewManager(a.sysd, filepath.Join(a.paths.Local, tunnelsDir), nil, a.cfg.Log)
	require.NoError(t, m.Ensure(t.Context(), testInstall, tunnelID, "token"))
	require.NotEmpty(t, a.sysd.units())
	require.NoError(t, appliance.WriteFlag(a.flag))
	a.mountVolumeOf(9295)
	var units, scripts []string
	var asked int
	a.notify.onReady = func() {
		units, asked, scripts = a.sysd.units(), len(a.pve.authorizations()), a.nft.applied()
	}

	d := a.start()

	require.Empty(t, units, "stopped before the API answered")
	require.Zero(t, asked, "and before any cycle")
	require.NoFileExists(t, a.flag)
	require.Len(t, scripts, 1)
	require.Equal(t, egress.Base(testConnectorUID, []netip.Addr{netip.MustParseAddr("10.20.0.1")}, nil), scripts[0],
		"the egress filter was given no target")
	st := d.await(func(st engine.State) bool { return st.Identity != nil })
	require.True(t, st.Identity.Copy)
	require.Equal(t, incarnationOf(t, a.store), incarnation, "no epoch drawn")
	require.Equal(t, 1, strings.Count(a.logs.String(), `"message":"stopped connector"`),
		"the first cycle does not stop the connectors again")
}

func TestBeforeTheFirstCycleNothingButTheMountIsDecided(t *testing.T) {
	a := newApplianceWorld(t)
	a.mountVolumeOf(applianceVMID)
	var flagAtReady bool
	a.notify.onReady = func() {
		_, err := os.Stat(a.flag)
		flagAtReady = err == nil
	}

	d := a.start()

	require.False(t, flagAtReady, "the flag waits for the first cycle")
	d.await(func(st engine.State) bool { return st.Identity != nil && st.Identity.OK })
	require.FileExists(t, a.flag)
}

// A route someone deleted inside the container comes back with the next
// check, which a change of the ruleset starts as well; the event says so.
func TestTheApplianceLoadsTheServicePrefixAgain(t *testing.T) {
	a := newApplianceWorld(t)
	d := a.start()
	d.await(func(st engine.State) bool { return st.Identity != nil && st.Identity.OK })
	a.net.take("route")
	a.netNft.mu.Lock()
	a.netNft.live = nil
	a.netNft.mu.Unlock()

	a.rules.change(t)

	require.Eventually(t, func() bool { return slices.Equal(a.net.addedSoFar(), []string{"route"}) }, 10*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		events, err := d.client.Events(t.Context(), time.Time{})
		require.NoError(t, err)
		for _, ev := range events {
			if ev.Message == "the service-prefix route or table was changed outside pco and was loaded again" {
				return true
			}
		}
		return false
	}, 10*time.Second, 5*time.Millisecond)
	require.NoError(t, d.client.Sync(t.Context()))
	line := "the service-prefix route or table was changed outside pco and was loaded again " +
		"(the route 198.18.0.0/16 dev pco0 src 198.18.0.1 is missing; the table inet pco_net is not loaded)"
	d.await(func(st engine.State) bool { return slices.Contains(st.Problems, line) })
}

func incarnationOf(t *testing.T, st *store.Store) string {
	t.Helper()
	w, _, err := st.Writer()
	require.NoError(t, err)
	return w.Incarnation
}

func TestAnErrorOfTheVolumeOtherThanItsAbsenceIsSaidAsSuch(t *testing.T) {
	a := newApplianceWorld(t)
	a.deps.Appliance.Volume = func(string, string) error { return errors.New("input/output error") }

	err := Run(bounded(t), a.cfg, a.deps)

	require.EqualError(t, err, "cannot read "+a.paths.Local+": input/output error")
	require.True(t, slices.Equal(a.notify.sent(), nil))
}

// doctorOfTheAppliance is what a test of the doctor of the appliance gives the
// daemon: what it asks of the container's own system answered from a directory
// and from functions, and a Cloudflare API with a date.
type doctorOfTheAppliance struct {
	a      *applianceWorld
	mu     sync.Mutex
	probed []string
	cf     *httptest.Server
}

func newDoctorOfTheAppliance(t *testing.T, a *applianceWorld) *doctorOfTheAppliance {
	t.Helper()
	d := &doctorOfTheAppliance{a: a}
	d.cf = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(d.cf.Close)
	a.deps.CloudflareURL = d.cf.URL
	a.deps.Dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("no network in this test")
	}
	a.deps.Cloudflared = filepath.Join(a.dir, "cloudflared")
	require.NoError(t, os.WriteFile(a.deps.Cloudflared, []byte("#!/bin/sh\necho 'cloudflared version 2026.9.3 (built 2026-09-20-1200 UTC)'\n"), 0o700))
	a.deps.HostTimeout = time.Minute
	a.deps.UnitFileState = func(_ context.Context, unit string) (string, error) {
		if unit == "nftables.service" {
			return "masked", nil
		}
		return "enabled", nil
	}
	a.deps.Appliance.Doctor = func(h *doctor.ApplianceHost) {
		h.Root = a.dir
		h.Probe = func(_ context.Context, addr string) error {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.probed = append(d.probed, addr)
			if strings.HasSuffix(addr, ":7844") {
				return nil
			}
			return doctor.ErrRefused
		}
		h.Run = func(_ context.Context, name string, _ ...string) (string, error) {
			if strings.HasSuffix(name, "apt-mark") {
				return "cloudflared\npco\n", nil
			}
			return "APT::Periodic::Unattended-Upgrade \"1\";\n", nil
		}
		h.Resolve = func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("10.20.0.1")}, nil }
		h.LookupHost = func(context.Context, string) ([]string, error) { return []string{"198.41.192.7"}, nil }
	}
	a.pve.answer("/api2/json/nodes/pve1/lxc/9250/config", map[string]any{
		"hostname": "pco", "features": "nesting=1", "protection": 1, "memory": 512,
		"mp0":    "local-zfs:subvol-9250-disk-1,backup=0,mp=/var/lib/pco,size=2G",
		"net0":   "name=eth0,bridge=vmbr1,hwaddr=" + strings.ToUpper(applianceMAC) + ",ip=dhcp,type=veth",
		"digest": "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c",
	})
	a.pve.answer("/api2/json/nodes/pve1/lxc/9250/pending", []map[string]any{
		{"key": "features", "value": "nesting=1"}, {"key": "protection", "value": 1},
	})
	a.pve.answer("/api2/json/nodes/pve1/lxc/9250/snapshot", []map[string]any{{"name": "current"}})
	a.pve.answer("/api2/json/cluster/replication", []map[string]any{})
	a.pve.answer("/api2/json/access/permissions", map[string]any{
		"/":              map[string]int{"Pool.Audit": 1, "SDN.Audit": 1, "Sys.Audit": 1, "VM.Audit": 1, "VM.GuestAgent.Audit": 1},
		"/access":        map[string]int{"Sys.Audit": 1},
		"/access/groups": map[string]int{"Sys.Audit": 1},
	})
	a.pve.answer("/api2/json/cluster/firewall/options", map[string]any{"enable": 1})
	a.pve.answer("/api2/json/nodes/pve1/firewall/options", map[string]any{"enable": 1})
	return d
}

var applianceDoctorChecks = []string{
	"access", "api", "clock", "cloudflare api", "cmode", "disk", "dns", "egress probe", "etc-pve", "features", "firewall", "holds", "identity",
	"journal", "memory", "net", "network grants", "nftables", "protection", "quic buffer", "segment access", "snapshots", "token",
	"unattended-upgrades", "versions", "volume",
}

func TestTheDoctorOfTheApplianceChecksWhatItDependsOn(t *testing.T) {
	a := newApplianceWorld(t)
	probe := newDoctorOfTheAppliance(t, a)
	d := a.start()
	d.await(func(st engine.State) bool { return st.Identity != nil && st.Identity.OK })

	findings, err := d.client.Doctor(t.Context())

	require.NoError(t, err)
	byCheck := map[string]doctor.Finding{}
	for _, f := range findings {
		byCheck[f.Check] = f
	}
	for _, check := range applianceDoctorChecks {
		require.Contains(t, byCheck, check)
	}
	// What each reads comes from a part of the daemon.
	require.Equal(t, doctor.Finding{Check: "nftables", Level: doctor.LevelOK, Detail: "nftables.service is masked"}, byCheck["nftables"],
		"the file state of the unit is asked, not whether it is enabled")
	for _, check := range []string{"features", "protection", "cmode", "volume", "snapshots", "token", "holds", "unattended-upgrades", "api", "firewall", "dns", "cloudflare api"} {
		require.Equal(t, doctor.LevelOK, byCheck[check].Level, "%s: %s (%s)", check, byCheck[check].Detail, byCheck[check].Fix)
	}
	require.Equal(t, doctor.LevelOK, byCheck["identity"].Level)
	require.Contains(t, byCheck["identity"].Detail, "this container is lxc/9250 on pve1")
	require.Equal(t, doctor.LevelOK, byCheck["store"].Level, byCheck["store"].Detail)
	require.Equal(t, doctor.LevelOK, byCheck["egress probe"].Level, byCheck["egress probe"].Detail)
	probe.mu.Lock()
	require.ElementsMatch(t, []string{"region1.v2.argotunnel.com:7844", strings.TrimPrefix(a.pve.srv.URL, "https://")}, probe.probed,
		"the edge, and the endpoint of the API of the install")
	probe.mu.Unlock()
	require.Contains(t, byCheck["clock"].Detail, "of Cloudflare's", "the date of the Cloudflare API the daemon was given")
	require.Equal(t, "the container has no snapshot, and no replication job copies it", byCheck["snapshots"].Detail)
}

func TestTheStoreCheckOfAnApplianceReadsTheVolumeMarker(t *testing.T) {
	a := newApplianceWorld(t)
	newDoctorOfTheAppliance(t, a)
	var gone atomic.Bool
	a.deps.Appliance.Volume = func(path, _ string) error {
		if gone.Load() {
			return fmt.Errorf("%s %w", path, appliance.ErrNoMarker)
		}
		return nil
	}
	d := a.start()
	d.await(func(st engine.State) bool { return st.Identity != nil && st.Identity.OK })
	gone.Store(true)

	findings, err := d.client.Doctor(t.Context())

	require.NoError(t, err)
	i := slices.IndexFunc(findings, func(f doctor.Finding) bool { return f.Check == "store" })
	require.GreaterOrEqual(t, i, 0)
	require.Equal(t, doctor.Finding{Check: "store", Level: doctor.LevelFail,
		Detail: a.paths.Local + " has no pco volume marker", Fix: "run pco appliance repair --vmid 9250 on the node"}, findings[i])
	j := slices.IndexFunc(findings, func(f doctor.Finding) bool { return f.Check == "volume" })
	require.Equal(t, doctor.LevelFail, findings[j].Level)
}

func TestTheDoctorOfAHostHasNoCheckOfTheAppliance(t *testing.T) {
	w := newWorld(t)
	w.deps.HostTimeout = time.Minute
	d := w.start()
	d.await(func(st engine.State) bool { return st.At.After(time.Time{}) })

	findings, err := d.client.Doctor(t.Context())

	require.NoError(t, err)
	for _, f := range findings {
		if f.Check != "nftables" {
			require.NotContains(t, applianceDoctorChecks, f.Check, "a host runs none of the checks of the appliance, bar the nftables one both have")
		}
	}
}
