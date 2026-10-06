package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/access"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/upgrade"
)

const (
	testVMID  = 9240
	pcoToken  = "pco@pve!vm9240"
	apiAddr   = "10.92.0.1:8006"
	pcoPrivs9 = "Pool.Audit SDN.Audit Sys.Audit VM.Audit VM.GuestAgent.Audit"
	pcoPrivs8 = "Pool.Audit SDN.Audit Sys.Audit VM.Audit VM.Monitor"
)

// fakeApp is the appliance's part of the host: it answers from its fields.
type fakeApp struct {
	self Self

	config     map[string]string
	configErr  error
	pending    map[string]string
	pendingErr error
	unit       map[string]string // the file state of a unit; a unit not in it is not found
	unitErr    error
	snapshots  []pve.Snapshot
	snapErr    error
	jobs       []pve.ReplicationJob
	jobsErr    error
	perms      map[string][]string
	permsErr   error
	data       access.Data
	dataErr    error
	datacenter bool
	node       bool
	firewall   error
	volumeErr  error
	verifyErr  error
	netErr     error
	leaked     uint64
	leakedErr  error
	probes     map[string]error // by address; nil is a connection
	cfDate     time.Time
	cfErr      error
	holds      []string
	holdsErr   error
	unattended bool
	upgradeErr error
	nodeNet    []pve.NodeIface
	nodeNetErr error
	disk       map[string][2]uint64 // used and total by path
	diskErr    error
	journal    uint64
	journalErr error
	pressure   float64
	pressErr   error
	pco, cloud string
	debian     string
	versionErr error
	manifest   upgrade.Manifest
	manifestEr error
	resolvers  []netip.Addr
	resolveErr error
	lookupErr  error
	etcPVE     bool
	rmem       int
	rmemErr    error

	mu     sync.Mutex
	probed []string
}

func healthyApp() *fakeApp {
	return &fakeApp{
		self: Self{VMID: testVMID, Node: "pve1", Address: apiAddr, ServerName: "pve1", User: "pco@pve", Token: pcoToken},
		config: map[string]string{
			"features":   "nesting=1",
			"protection": "1",
			"mp0":        "local-zfs:subvol-9240-disk-1,backup=0,mp=/var/lib/pco,size=2G",
			"net0":       "name=eth0,bridge=vmbr1,hwaddr=BC:24:11:00:92:40,ip=dhcp,type=veth",
			"memory":     "512",
		},
		pending: map[string]string{
			"features":   "nesting=1",
			"protection": "1",
			"mp0":        "local-zfs:subvol-9240-disk-1,backup=0,mp=/var/lib/pco,size=2G",
			"net0":       "name=eth0,bridge=vmbr1,hwaddr=BC:24:11:00:92:40,ip=dhcp,type=veth",
		},
		unit:       map[string]string{"nftables.service": "masked"},
		perms:      map[string][]string{"/": strings.Fields(pcoPrivs9)},
		data:       applianceAccess(),
		datacenter: true,
		node:       true,
		probes:     map[string]error{apiAddr: fmt.Errorf("dial tcp %s: %w", apiAddr, ErrRefused)},
		cfDate:     now,
		holds:      []string{"cloudflared", "pco"},
		unattended: true,
		nodeNet: []pve.NodeIface{
			{Name: "vmbr0", Type: "bridge", Active: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.92.0.1/24")}},
			{Name: "vmbr1", Type: "bridge", Active: true},
		},
		disk:      map[string][2]uint64{"/": {1 << 30, 4 << 30}, "/var/lib/pco": {100 << 20, 2 << 30}},
		journal:   8 << 20,
		pressure:  0.4,
		pco:       "1.0.0",
		cloud:     "2026.9.3",
		debian:    "13.1",
		manifest:  upgrade.Manifest{Updated: now.AddDate(0, 0, -10), Deny: []upgrade.Denial{{Version: "2026.9.0", Reason: "a bug"}}},
		resolvers: []netip.Addr{netip.MustParseAddr("10.92.0.1")},
		rmem:      7500000,
	}
}

// applianceAccess is the access control of a cluster with root, pco's user and
// token, the roles of a network grant and a user of the test. Entries are added
// to what pco has.
func applianceAccess(extra ...pve.ACLEntry) access.Data {
	return access.Data{
		ACL: append([]pve.ACLEntry{
			{Path: "/", Type: "user", UGID: "root@pam", RoleID: "Administrator", Propagate: true},
			{Path: "/", Type: "user", UGID: "pco@pve", RoleID: "PCO", Propagate: true},
			{Path: "/", Type: "token", UGID: pcoToken, RoleID: "PCO", Propagate: true},
		}, extra...),
		Users: []pve.User{
			{ID: "alice@pve", Enabled: true, Tokens: map[string]bool{"sep": true, "shared": false}},
			{ID: "pco@pve", Enabled: true, Tokens: map[string]bool{"vm9240": true}},
			{ID: "pcotest@pve", Enabled: true},
			{ID: "root@pam", Enabled: true},
		},
		Roles: []pve.Role{
			{ID: "Administrator", Privs: []string{"Permissions.Modify", "Pool.Allocate", "SDN.Use", "Sys.Modify", "VM.Audit", "VM.Config.Network", "VM.Console", "VM.PowerMgmt"}},
			{ID: "PCO", Privs: strings.Fields(pcoPrivs9)},
			{ID: "PCOSDN", Privs: []string{"SDN.Use"}},
			{ID: "PCOManaged", Privs: []string{"VM.Config.Network"}},
			{ID: "PVEVMUser", Privs: []string{"VM.Audit", "VM.Console", "VM.PowerMgmt"}},
			{ID: "PVESDNUser", Privs: []string{"SDN.Audit", "SDN.Use"}},
			{ID: "NoAccess"},
		},
	}
}

func (f *fakeApp) Self() Self { return f.self }

func (f *fakeApp) OwnConfig(context.Context) (pve.GuestConfig, error) {
	return pve.GuestConfig{Values: f.config}, f.configErr
}

func (f *fakeApp) OwnPending(context.Context) (map[string]string, error) {
	return f.pending, f.pendingErr
}

func (f *fakeApp) UnitFileState(_ context.Context, unit string) (string, error) {
	if state, ok := f.unit[unit]; ok {
		return state, f.unitErr
	}
	return "not-found", f.unitErr
}

func (f *fakeApp) Snapshots(context.Context) ([]pve.Snapshot, error) { return f.snapshots, f.snapErr }

func (f *fakeApp) Replication(context.Context) ([]pve.ReplicationJob, error) {
	return f.jobs, f.jobsErr
}

func (f *fakeApp) OwnPermissions(context.Context) (map[string][]string, error) {
	return f.perms, f.permsErr
}

func (f *fakeApp) AccessData(context.Context) (access.Data, error) { return f.data, f.dataErr }

func (f *fakeApp) DatacenterFirewall(context.Context) (bool, error) { return f.datacenter, f.firewall }
func (f *fakeApp) NodeFirewall(context.Context) (bool, error)       { return f.node, f.firewall }
func (f *fakeApp) VolumeMounted() error                             { return f.volumeErr }
func (f *fakeApp) VerifyError() error                               { return f.verifyErr }
func (f *fakeApp) NetVerify(context.Context) error                  { return f.netErr }

func (f *fakeApp) NetLeaked(context.Context) (uint64, error) { return f.leaked, f.leakedErr }

func (f *fakeApp) ProbeAsConnector(_ context.Context, addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probed = append(f.probed, addr)
	return f.probes[addr]
}

func (f *fakeApp) CloudflareDate(context.Context) (time.Time, error) { return f.cfDate, f.cfErr }
func (f *fakeApp) Holds(context.Context) ([]string, error)           { return f.holds, f.holdsErr }

func (f *fakeApp) UnattendedUpgrades(context.Context) (bool, error) {
	return f.unattended, f.upgradeErr
}

func (f *fakeApp) NodeNetwork(context.Context) ([]pve.NodeIface, error) {
	return f.nodeNet, f.nodeNetErr
}

func (f *fakeApp) DiskUse(path string) (used, total uint64, err error) {
	u := f.disk[path]
	return u[0], u[1], f.diskErr
}

func (f *fakeApp) JournalUse(context.Context) (uint64, error) { return f.journal, f.journalErr }
func (f *fakeApp) MemoryPressure() (float64, error)           { return f.pressure, f.pressErr }

func (f *fakeApp) Versions(context.Context) (pco, cloudflared, debian string, err error) {
	return f.pco, f.cloud, f.debian, f.versionErr
}

func (f *fakeApp) Manifest() (upgrade.Manifest, error)  { return f.manifest, f.manifestEr }
func (f *fakeApp) Resolvers() ([]netip.Addr, error)     { return f.resolvers, f.resolveErr }
func (f *fakeApp) Lookup(context.Context, string) error { return f.lookupErr }
func (f *fakeApp) EtcPVE() bool                         { return f.etcPVE }
func (f *fakeApp) RmemMax() (int, error)                { return f.rmem, f.rmemErr }

// applianceEnv is a host that has an appliance's part.
type applianceEnv struct {
	*fakeEnv
	app *fakeApp
}

func (e *applianceEnv) Appliance() ApplianceEnv { return e.app }

func healthyApplianceEnv() *applianceEnv {
	env := healthyEnv()
	env.version = "cloudflared version 2026.9.3 (built 2026-09-20-1200 UTC)"
	return &applianceEnv{fakeEnv: env, app: healthyApp()}
}

func healthyApplianceState() engine.State {
	st := healthyState()
	st.Profile = store.ProfileAppliance
	st.Identity = &engine.IdentityView{VMID: testVMID, Node: "pve1", OK: true, CheckedAt: now.Add(-5 * time.Second)}
	return st
}

// applianceChecks are the checks of the appliance, besides those of every
// profile.
var applianceChecks = []string{
	"access", "api", "clock", "cloudflare api", "cmode", "disk", "dns", "egress probe", "etc-pve", "features", "firewall", "holds", "identity",
	"journal", "memory", "net", "network grants", "protection", "quic buffer", "segment access", "snapshots", "token", "unattended-upgrades",
	"versions", "volume",
}

// found runs the doctor on an appliance changed by the two functions, and
// returns the finding of one check.
func found(t *testing.T, check string, state func(*engine.State), app func(*fakeApp, *fakeEnv)) Finding {
	t.Helper()
	env, st := healthyApplianceEnv(), healthyApplianceState()
	if state != nil {
		state(&st)
	}
	if app != nil {
		app(env.app, env.fakeEnv)
	}
	findings := Run(t.Context(), st, env)
	i := slices.IndexFunc(findings, func(f Finding) bool { return f.Check == check })
	require.GreaterOrEqual(t, i, 0, "no finding of %q in %v", check, findings)
	return findings[i]
}

func TestAHealthyApplianceHasAFindingForEveryCheck(t *testing.T) {
	env := healthyApplianceEnv()

	findings := Run(t.Context(), healthyApplianceState(), env)

	have := map[string]Finding{}
	for _, f := range findings {
		have[f.Check] = f
	}
	for _, check := range applianceChecks {
		f, ok := have[check]
		require.True(t, ok, "no finding of %q", check)
		require.Equal(t, LevelOK, f.Level, "%s: %s (%s)", check, f.Detail, f.Fix)
		require.Empty(t, f.Fix, check)
	}
	require.False(t, Failed(findings))
	require.Equal(t, []string{"tcp region1.v2.argotunnel.com:7844"}, env.dialed)
	require.ElementsMatch(t, []string{"region1.v2.argotunnel.com:7844", apiAddr}, env.app.probed)
	require.True(t, slices.IsSortedFunc(findings, compareChecks))
}

func TestAHealthyApplianceSaysWhatItFound(t *testing.T) {
	env := healthyApplianceEnv()

	findings := Run(t.Context(), healthyApplianceState(), env)

	have := map[string]Finding{}
	for _, f := range findings {
		have[f.Check] = f
	}
	for check, detail := range map[string]string{
		"features":            "nesting=1, in the current and in the pending configuration",
		"protection":          "the container is protected against removal",
		"cmode":               "the console is a login on a tty, now and at the next start",
		"volume":              "mp0 is a mount of its own at /var/lib/pco with the volume marker, and is kept out of backups",
		"snapshots":           "the container has no snapshot, and no replication job copies it",
		"identity":            "this container is lxc/9240 on pve1, checked 5s ago",
		"token":               "the token holds exactly the privileges of role PCO on Proxmox VE 9: Pool.Audit, SDN.Audit, Sys.Audit, VM.Audit, VM.GuestAgent.Audit",
		"access":              "no principal other than an admin and pco's own can reach into the appliance",
		"api":                 "10.92.0.1:8006 answers, and its certificate verifies under pve1",
		"firewall":            "the datacenter firewall is on; the firewall of node pve1 is on",
		"egress probe":        "as pco-connector, region1.v2.argotunnel.com:7844 connects and 10.92.0.1:8006 is refused",
		"cloudflare api":      "api.cloudflare.com answers",
		"dns":                 "1 name server (10.92.0.1) answers for region1.v2.argotunnel.com",
		"clock":               "the clock is within 0s of Cloudflare's",
		"holds":               "pco and cloudflared are held",
		"unattended-upgrades": "unattended-upgrades is enabled and installs the security updates of Debian",
		"disk":                "/ is 25% full (1.0 GiB of 4.0 GiB); /var/lib/pco is 4% full (100.0 MiB of 2.0 GiB)",
		"journal":             "the journal takes 8.0 MiB",
		"memory":              "memory pressure is 0.4%",
		"versions":            "pco 1.0.0, cloudflared 2026.9.3, Debian 13.1",
		"net":                 "the service prefix is kept in place, and nothing was sent to it without a mapping",
		"etc-pve":             "/etc/pve is not in the container",
		"quic buffer":         "net.core.rmem_max is 7500000",
		"network grants":      "pco@pve and its token may put no card on a network but their own",
		"segment access":      "no principal other than an admin and pco's own may use /sdn/zones/localnetwork/vmbr1, the segment of net0",
		"nftables":            "nftables.service is masked",
	} {
		require.Equal(t, Finding{Check: check, Level: LevelOK, Detail: detail}, have[check], check)
	}
}

func TestNoApplianceChecksOnTheHost(t *testing.T) {
	env := healthyApplianceEnv()
	st := healthyState()
	st.Profile = store.ProfileHost

	findings := Run(t.Context(), st, env)

	for _, f := range findings {
		require.NotContains(t, applianceChecks, f.Check, "a host has no check of the appliance")
	}
	require.Empty(t, env.app.probed)
	require.Contains(t, checks(findings), "nftables", "the check of nftables.service runs on the host as well")
	require.Equal(t, Finding{Check: "nftables", Level: LevelOK, Detail: "nftables.service is not enabled"}, only(findings, "nftables"),
		"on the host it asks whether the unit starts at boot")
}

func TestNoApplianceChecksWithoutThePartOfTheAppliance(t *testing.T) {
	st := healthyApplianceState()

	findings := Run(t.Context(), st, healthyEnv())

	for _, f := range findings {
		require.NotContains(t, applianceChecks, f.Check)
	}
}

func checks(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.Check)
	}
	return out
}

func only(findings []Finding, check string) Finding {
	for _, f := range findings {
		if f.Check == check {
			return f
		}
	}
	return Finding{}
}

func TestBeforeTheFirstCycleTheApplianceChecksItsOwnThings(t *testing.T) {
	env := healthyApplianceEnv()

	findings := Run(t.Context(), engine.State{Mode: "observe", WriterVerdict: "ok"}, env)

	require.Equal(t, Finding{Check: "identity", Level: LevelWarn, Detail: "not known until the first cycle", Fix: "wait for the first cycle"}, only(findings, "identity"))
	require.Equal(t, LevelOK, only(findings, "features").Level)
	require.Equal(t, LevelOK, only(findings, "token").Level)
	require.Empty(t, only(findings, "epoch").Check)
}

func TestWhatTheDoctorFindsInAnAppliance(t *testing.T) {
	const repair = "run pco appliance repair --vmid 9240 on the node"
	tokenFix := func(privs string) string {
		return "on the node: pveum role modify PCO --privs " + strings.ReplaceAll(privs, " ", ",") +
			", and grant it on / to the user and the token: pveum acl modify / --users pco@pve --roles PCO; " +
			"pveum acl modify / --tokens pco@pve!vm9240 --roles PCO"
	}
	granted := func(path, role string) pve.ACLEntry {
		return pve.ACLEntry{Path: path, Type: "user", UGID: "pcotest@pve", RoleID: role, Propagate: true}
	}
	for _, tt := range []struct {
		name   string
		check  string
		state  func(*engine.State)
		app    func(*fakeApp, *fakeEnv)
		expect Finding
	}{
		// features
		{"features that are more than nesting", "features", nil, func(a *fakeApp, _ *fakeEnv) {
			a.config["features"], a.pending["features"] = "nesting=1,keyctl=1", "nesting=1,keyctl=1"
		}, Finding{Check: "features", Level: LevelFail,
			Detail: "the features are nesting=1,keyctl=1 now and nesting=1,keyctl=1 at the next start; only nesting=1 is allowed",
			Fix:    "on the node: pct set 9240 --features nesting=1, then restart the container"}},
		{"features that differ in the pending configuration alone", "features", nil, func(a *fakeApp, _ *fakeEnv) {
			a.pending["features"] = "nesting=1,keyctl=1"
		}, Finding{Check: "features", Level: LevelFail,
			Detail: "the features are nesting=1 now, but the pending configuration has nesting=1,keyctl=1, which applies at the next start of the container",
			Fix:    "on the node: pct set 9240 --revert features"}},
		{"features that are wrong now and put right in the pending configuration", "features", nil, func(a *fakeApp, _ *fakeEnv) {
			a.config["features"] = "keyctl=1,nesting=1"
		}, Finding{Check: "features", Level: LevelFail,
			Detail: "the features are keyctl=1,nesting=1 now; the pending configuration has nesting=1, which applies at the next start of the container",
			Fix:    "restart the container"}},
		{"features that are not there", "features", nil, func(a *fakeApp, _ *fakeEnv) {
			delete(a.config, "features")
			delete(a.pending, "features")
		}, Finding{Check: "features", Level: LevelFail,
			Detail: "the features are none now and none at the next start; only nesting=1 is allowed",
			Fix:    "on the node: pct set 9240 --features nesting=1, then restart the container"}},
		{"features that cannot be read", "features", nil, func(a *fakeApp, _ *fakeEnv) { a.configErr = errors.New("500 boom") },
			Finding{Check: "features", Level: LevelWarn, Detail: "the configuration of the container could not be read: 500 boom",
				Fix: "the api and proxmox checks say why Proxmox does not answer"}},
		// protection
		{"protection off", "protection", nil, func(a *fakeApp, _ *fakeEnv) { a.config["protection"] = "0" },
			Finding{Check: "protection", Level: LevelFail,
				Detail: "the container is not protected: it can be destroyed, with the volume of its state, by one command",
				Fix:    "on the node: pct set 9240 --protection 1"}},
		{"protection missing", "protection", nil, func(a *fakeApp, _ *fakeEnv) { delete(a.config, "protection") },
			Finding{Check: "protection", Level: LevelFail,
				Detail: "the container is not protected: it can be destroyed, with the volume of its state, by one command",
				Fix:    "on the node: pct set 9240 --protection 1"}},
		// cmode
		{"cmode shell now", "cmode", nil, func(a *fakeApp, _ *fakeEnv) { a.config["cmode"], a.pending["cmode"] = "shell", "shell" },
			Finding{Check: "cmode", Level: LevelFail,
				Detail: "the console mode is shell now and shell at the next start; the console of the container is then a root shell without a password",
				Fix:    "on the node: pct set 9240 --cmode tty, then restart the container"}},
		{"cmode shell pending", "cmode", nil, func(a *fakeApp, _ *fakeEnv) { a.pending["cmode"] = "shell" },
			Finding{Check: "cmode", Level: LevelFail,
				Detail: "the console mode is tty now, but the pending configuration has shell: the next start of the container makes its console a root shell without a password, " +
					"and any one VM.Config privilege sets it",
				Fix: "on the node: pct set 9240 --revert cmode"}},
		{"cmode console pending", "cmode", nil, func(a *fakeApp, _ *fakeEnv) { a.pending["cmode"] = "console" },
			Finding{Check: "cmode", Level: LevelFail,
				Detail: "the console mode is tty now, but the pending configuration has console: the next start of the container makes its console a root shell without a password, " +
					"and any one VM.Config privilege sets it",
				Fix: "on the node: pct set 9240 --revert cmode"}},
		{"cmode tty", "cmode", nil, func(a *fakeApp, _ *fakeEnv) { a.config["cmode"], a.pending["cmode"] = "tty", "tty" },
			Finding{Check: "cmode", Level: LevelOK, Detail: "the console is a login on a tty, now and at the next start"}},
		{"cmode unreadable pending", "cmode", nil, func(a *fakeApp, _ *fakeEnv) { a.pendingErr = errors.New("500 boom") },
			Finding{Check: "cmode", Level: LevelWarn, Detail: "the pending configuration of the container could not be read: 500 boom",
				Fix: "the api and proxmox checks say why Proxmox does not answer"}},
		// volume
		{"a volume that is not mounted", "volume", nil, func(a *fakeApp, _ *fakeEnv) {
			a.volumeErr = fmt.Errorf("/var/lib/pco %w", appliance.ErrNotMountPoint)
		}, Finding{Check: "volume", Level: LevelFail,
			Detail: "/var/lib/pco is not a mount point of its own (a restore without the volume?)", Fix: repair}},
		{"a volume without its marker", "volume", nil, func(a *fakeApp, _ *fakeEnv) {
			a.volumeErr = fmt.Errorf("/var/lib/pco %w", appliance.ErrNoMarker)
		}, Finding{Check: "volume", Level: LevelFail,
			Detail: "/var/lib/pco has no pco volume marker (a restore, or a volume that is not pco's?)", Fix: repair}},
		{"a volume that cannot be read", "volume", nil, func(a *fakeApp, _ *fakeEnv) { a.volumeErr = errors.New("stat /var/lib/pco: input/output error") },
			Finding{Check: "volume", Level: LevelFail, Detail: "cannot read /var/lib/pco: stat /var/lib/pco: input/output error", Fix: repair}},
		{"a volume in the backups", "volume", nil, func(a *fakeApp, _ *fakeEnv) {
			a.config["mp0"] = "local-zfs:subvol-9240-disk-1,mp=/var/lib/pco,size=2G"
		}, Finding{Check: "volume", Level: LevelFail,
			Detail: "mp0 is not kept out of backups: a backup of the container would carry the Cloudflare credentials and the keys of the appliance",
			Fix:    "on the node: pct set 9240 --mp0 local-zfs:subvol-9240-disk-1,mp=/var/lib/pco,size=2G,backup=0"}},
		{"a volume with backup on", "volume", nil, func(a *fakeApp, _ *fakeEnv) {
			a.config["mp0"] = "local-zfs:subvol-9240-disk-1,backup=1,mp=/var/lib/pco"
		}, Finding{Check: "volume", Level: LevelFail,
			Detail: "mp0 is not kept out of backups: a backup of the container would carry the Cloudflare credentials and the keys of the appliance",
			Fix:    "on the node: pct set 9240 --mp0 local-zfs:subvol-9240-disk-1,backup=0,mp=/var/lib/pco"}},
		{"no mp0", "volume", nil, func(a *fakeApp, _ *fakeEnv) { delete(a.config, "mp0") },
			Finding{Check: "volume", Level: LevelFail, Detail: "the container has no mp0: the volume of its state is not part of it", Fix: repair}},
		{"mp0 at another path", "volume", nil, func(a *fakeApp, _ *fakeEnv) {
			a.config["mp0"] = "local-zfs:subvol-9240-disk-1,backup=0,mp=/mnt/pco"
		}, Finding{Check: "volume", Level: LevelFail, Detail: "mp0 is mounted at /mnt/pco, not at /var/lib/pco", Fix: repair}},
		// snapshots
		{"a snapshot of any name", "snapshots", nil, func(a *fakeApp, _ *fakeEnv) {
			a.snapshots = []pve.Snapshot{{Name: "before-upgrade", Time: now.Add(-time.Hour)}}
		}, Finding{Check: "snapshots", Level: LevelWarn,
			Detail: "1 snapshot of the container holds a copy of the volume of its state, with the Cloudflare credentials and the keys: before-upgrade",
			Fix:    "on the node: pct delsnapshot 9240 before-upgrade"}},
		{"a pre-upgrade snapshot of today", "snapshots", nil, func(a *fakeApp, _ *fakeEnv) {
			a.snapshots = []pve.Snapshot{{Name: "pco-pre-upgrade-20261001", Time: now.Add(-time.Hour)}}
		}, Finding{Check: "snapshots", Level: LevelOK, Detail: "the only snapshot is pco-pre-upgrade-20261001, taken before an upgrade, 1 hour ago, and no replication job copies the container"}},
		{"a pre-upgrade snapshot six days old", "snapshots", nil, func(a *fakeApp, _ *fakeEnv) {
			a.snapshots = []pve.Snapshot{{Name: "pco-pre-upgrade-20260925", Time: now.Add(-6*24*time.Hour - time.Hour)}}
		}, Finding{Check: "snapshots", Level: LevelOK, Detail: "the only snapshot is pco-pre-upgrade-20260925, taken before an upgrade, 6 days ago, and no replication job copies the container"}},
		{"a pre-upgrade snapshot eight days old", "snapshots", nil, func(a *fakeApp, _ *fakeEnv) {
			a.snapshots = []pve.Snapshot{{Name: "pco-pre-upgrade-20260923", Time: now.Add(-8 * 24 * time.Hour)}}
		}, Finding{Check: "snapshots", Level: LevelWarn,
			Detail: "1 snapshot of the container holds a copy of the volume of its state, with the Cloudflare credentials and the keys: " +
				"pco-pre-upgrade-20260923 (8 days old: a snapshot before an upgrade is kept for 7 days)",
			Fix: "on the node: pct delsnapshot 9240 pco-pre-upgrade-20260923"}},
		{"a snapshot named like a pre-upgrade one with no date", "snapshots", nil, func(a *fakeApp, _ *fakeEnv) {
			a.snapshots = []pve.Snapshot{{Name: "pco-pre-upgrade-soon", Time: now}}
		}, Finding{Check: "snapshots", Level: LevelWarn,
			Detail: "1 snapshot of the container holds a copy of the volume of its state, with the Cloudflare credentials and the keys: pco-pre-upgrade-soon",
			Fix:    "on the node: pct delsnapshot 9240 pco-pre-upgrade-soon"}},
		{"two snapshots and a job", "snapshots", nil, func(a *fakeApp, _ *fakeEnv) {
			a.snapshots = []pve.Snapshot{{Name: "a", Time: now}, {Name: "pco-pre-upgrade-20261001", Time: now}}
			a.jobs = []pve.ReplicationJob{
				{ID: "9240-0", Guest: model.GuestRef{Kind: model.KindLXC, VMID: testVMID}, Target: "pve2"},
				{ID: "100-0", Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 100}, Target: "pve2"},
			}
		}, Finding{Check: "snapshots", Level: LevelWarn,
			Detail: "1 snapshot of the container holds a copy of the volume of its state, with the Cloudflare credentials and the keys: a; " +
				"the replication job 9240-0 copies the container to pve2",
			Fix: "on the node: pct delsnapshot 9240 a; pvesr delete 9240-0"}},
		{"a snapshot without a time and a stale name", "snapshots", nil, func(a *fakeApp, _ *fakeEnv) {
			a.snapshots = []pve.Snapshot{{Name: "pco-pre-upgrade-20260920"}}
		}, Finding{Check: "snapshots", Level: LevelWarn,
			Detail: "1 snapshot of the container holds a copy of the volume of its state, with the Cloudflare credentials and the keys: " +
				"pco-pre-upgrade-20260920 (11 days old: a snapshot before an upgrade is kept for 7 days)",
			Fix: "on the node: pct delsnapshot 9240 pco-pre-upgrade-20260920"}},
		{"snapshots that cannot be read", "snapshots", nil, func(a *fakeApp, _ *fakeEnv) { a.snapErr = errors.New("403") },
			Finding{Check: "snapshots", Level: LevelWarn, Detail: "the snapshots of the container could not be read: 403",
				Fix: "the api and proxmox checks say why Proxmox does not answer"}},
		// identity
		{"an identity that did not pass", "identity", func(st *engine.State) {
			st.Identity = &engine.IdentityView{VMID: testVMID, Node: "pve1", Why: "the uptimes of the guests could not be read; self-identification waits for them"}
		}, nil, Finding{Check: "identity", Level: LevelFail,
			Detail: "the uptimes of the guests could not be read; self-identification waits for them", Fix: fixProblems}},
		{"a copy", "identity", func(st *engine.State) {
			st.Identity = &engine.IdentityView{VMID: testVMID, Node: "pve1", Copy: true,
				Why: "the volume at /var/lib/pco is subvol-9241-disk-1, a volume of VMID 9241 and not of lxc/9240: this container is a copy"}
		}, nil, Finding{Check: "identity", Level: LevelFail,
			Detail: "the volume at /var/lib/pco is subvol-9241-disk-1, a volume of VMID 9241 and not of lxc/9240: this container is a copy; " +
				"the connectors are stopped and the egress filter is empty",
			Fix: "stop this container if it is a copy; a restored container becomes the appliance with pco appliance repair --vmid <its vmid> on the node, once the original is gone"}},
		{"no identity yet", "identity", func(st *engine.State) { st.Identity = nil }, nil,
			Finding{Check: "identity", Level: LevelWarn, Detail: "no self-identification has run yet", Fix: fixProblems}},
		// token
		{"a token with the privileges of PVE 8 on PVE 9", "token", nil, func(a *fakeApp, _ *fakeEnv) {
			a.perms = map[string][]string{"/": strings.Fields(pcoPrivs8)}
		}, Finding{Check: "token", Level: LevelFail,
			Detail: "the token lacks VM.GuestAgent.Audit of role PCO on Proxmox VE 9, and holds VM.Monitor besides",
			Fix:    tokenFix(pcoPrivs9)}},
		{"a token with the privileges of PVE 9 on PVE 8", "token", nil, func(a *fakeApp, env *fakeEnv) { env.pve = "8.4" },
			Finding{Check: "token", Level: LevelFail,
				Detail: "the token lacks VM.Monitor of role PCO on Proxmox VE 8, and holds VM.GuestAgent.Audit besides",
				Fix:    tokenFix(pcoPrivs8)}},
		{"a token with the privileges of PVE 8 on PVE 8", "token", nil, func(a *fakeApp, env *fakeEnv) {
			env.pve = "8.4"
			a.perms = map[string][]string{"/": strings.Fields(pcoPrivs8)}
		}, Finding{Check: "token", Level: LevelOK,
			Detail: "the token holds exactly the privileges of role PCO on Proxmox VE 8: Pool.Audit, SDN.Audit, Sys.Audit, VM.Audit, VM.Monitor"}},
		{"a token that holds more", "token", nil, func(a *fakeApp, _ *fakeEnv) {
			a.perms = map[string][]string{"/": append(strings.Fields(pcoPrivs9), "VM.Console")}
		}, Finding{Check: "token", Level: LevelWarn,
			Detail: "the token holds VM.Console besides the privileges of role PCO on Proxmox VE 9",
			Fix:    "take what role PCO does not give from the token and from pco@pve on /"}},
		{"a token that holds less", "token", nil, func(a *fakeApp, _ *fakeEnv) { a.perms = map[string][]string{"/": {"Sys.Audit", "VM.Audit"}} },
			Finding{Check: "token", Level: LevelFail,
				Detail: "the token lacks Pool.Audit, SDN.Audit and VM.GuestAgent.Audit of role PCO on Proxmox VE 9",
				Fix:    tokenFix(pcoPrivs9)}},
		{"a token with no permissions on /", "token", nil, func(a *fakeApp, _ *fakeEnv) { a.perms = map[string][]string{"/access": {"Sys.Audit"}} },
			Finding{Check: "token", Level: LevelFail,
				Detail: "the token lacks Pool.Audit, SDN.Audit, Sys.Audit, VM.Audit and VM.GuestAgent.Audit of role PCO on Proxmox VE 9",
				Fix:    tokenFix(pcoPrivs9)}},
		{"a token whose permissions cannot be read", "token", nil, func(a *fakeApp, _ *fakeEnv) { a.permsErr = errors.New("401 invalid token") },
			Finding{Check: "token", Level: LevelFail, Detail: "the permissions of the token could not be read: 401 invalid token",
				Fix: "check that the token pco@pve!vm9240 exists and is not expired; " + repair}},
		// access
		{"a user who reaches into the appliance", "access", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(pve.ACLEntry{Path: "/vms/9240", Type: "user", UGID: "pcotest@pve", RoleID: "PVEVMUser", Propagate: true})
		}, Finding{Check: "access", Level: LevelFail,
			Detail: "pcotest@pve holds VM.Console, VM.PowerMgmt on the appliance lxc/9240; the connectors are stopped while a principal other than an admin can reach into it",
			Fix:    "pveum acl modify /vms/9240 --users pcotest@pve --roles NoAccess"}},
		{"a privilege-separated token", "access", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(
				pve.ACLEntry{Path: "/", Type: "user", UGID: "boss@pve", RoleID: "Administrator", Propagate: true},
				pve.ACLEntry{Path: "/vms/9240", Type: "token", UGID: "boss@pve!ci", RoleID: "PVEVMUser", Propagate: true})
			a.data.Users = append(a.data.Users, pve.User{ID: "boss@pve", Enabled: true, Tokens: map[string]bool{"ci": true}})
		}, Finding{Check: "access", Level: LevelFail,
			Detail: "boss@pve!ci holds VM.Console, VM.PowerMgmt on the appliance lxc/9240; the connectors are stopped while a principal other than an admin can reach into it",
			Fix:    "pveum acl modify /vms/9240 --tokens boss@pve!ci --roles NoAccess"}},
		{"a token without privilege separation", "access", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(pve.ACLEntry{Path: "/vms/9240", Type: "user", UGID: "alice@pve", RoleID: "PVEVMUser", Propagate: true})
		}, Finding{Check: "access", Level: LevelFail,
			Detail: "alice@pve holds VM.Console, VM.PowerMgmt on the appliance lxc/9240; alice@pve!shared holds VM.Console, VM.PowerMgmt on the appliance lxc/9240; " +
				"the connectors are stopped while a principal other than an admin can reach into it",
			Fix: "pveum acl modify /vms/9240 --users alice@pve --roles NoAccess " +
				"(alice@pve!shared is not privilege-separated: it holds the roles of alice@pve, so the command names the user)"}},
		{"unreadable access control", "access", nil, func(a *fakeApp, _ *fakeEnv) { a.dataErr = errors.New("pco's token does not hold Sys.Audit on /access") },
			Finding{Check: "access", Level: LevelFail,
				Detail: "the access control of Proxmox cannot be read: pco's token does not hold Sys.Audit on /access; no connector starts until it is read, " +
					"and nothing says that no principal can reach into the appliance",
				Fix: "check that the token pco@pve!vm9240 holds role PCO on / (pveum acl list)"}},
		// segment access
		{"a user with SDN.Use on the segment", "segment access", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(granted("/sdn/zones/localnetwork/vmbr1", "PVESDNUser"))
		}, Finding{Check: "segment access", Level: LevelWarn,
			Detail: "pcotest@pve holds SDN.Use on /sdn/zones/localnetwork/vmbr1, the segment of net0 of the appliance: a guest put on it with the MAC of the appliance cuts off its inbound traffic",
			Fix:    "put the appliance on a segment only admins may use, or take SDN.Use on /sdn/zones/localnetwork/vmbr1 from the principals named"}},
		{"a user with SDN.Use on a zone above the segment", "segment access", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(granted("/sdn/zones/localnetwork", "PVESDNUser"))
		}, Finding{Check: "segment access", Level: LevelWarn,
			Detail: "pcotest@pve holds SDN.Use on /sdn/zones/localnetwork/vmbr1, the segment of net0 of the appliance: a guest put on it with the MAC of the appliance cuts off its inbound traffic",
			Fix:    "put the appliance on a segment only admins may use, or take SDN.Use on /sdn/zones/localnetwork/vmbr1 from the principals named"}},
		{"a user with SDN.Use on another bridge", "segment access", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(granted("/sdn/zones/localnetwork/vmbr0", "PVESDNUser"))
		}, Finding{Check: "segment access", Level: LevelOK,
			Detail: "no principal other than an admin and pco's own may use /sdn/zones/localnetwork/vmbr1, the segment of net0"}},
		{"an admin with SDN.Use on the segment", "segment access", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr1", Type: "user", UGID: "root@pam", RoleID: "PVESDNUser", Propagate: true})
		}, Finding{Check: "segment access", Level: LevelOK,
			Detail: "no principal other than an admin and pco's own may use /sdn/zones/localnetwork/vmbr1, the segment of net0"}},
		{"pco itself with SDN.Use on the segment", "segment access", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr1", Type: "token", UGID: pcoToken, RoleID: "PCOSDN", Propagate: true})
		}, Finding{Check: "segment access", Level: LevelOK,
			Detail: "no principal other than an admin and pco's own may use /sdn/zones/localnetwork/vmbr1, the segment of net0"}},
		{"a user with SDN.Use on the VLAN of the segment", "segment access", nil, func(a *fakeApp, _ *fakeEnv) {
			a.config["net0"] = "name=eth0,bridge=vmbr1,hwaddr=BC:24:11:00:92:40,ip=dhcp,tag=20,type=veth"
			a.data = applianceAccess(granted("/sdn/zones/localnetwork/vmbr1/20", "PVESDNUser"))
		}, Finding{Check: "segment access", Level: LevelWarn,
			Detail: "pcotest@pve holds SDN.Use on /sdn/zones/localnetwork/vmbr1/20, the segment of net0 of the appliance: a guest put on it with the MAC of the appliance cuts off its inbound traffic",
			Fix:    "put the appliance on a segment only admins may use, or take SDN.Use on /sdn/zones/localnetwork/vmbr1/20 from the principals named"}},
		{"a user with SDN.Use on another VLAN", "segment access", nil, func(a *fakeApp, _ *fakeEnv) {
			a.config["net0"] = "name=eth0,bridge=vmbr1,hwaddr=BC:24:11:00:92:40,ip=dhcp,tag=20,type=veth"
			a.data = applianceAccess(granted("/sdn/zones/localnetwork/vmbr1/30", "PVESDNUser"))
		}, Finding{Check: "segment access", Level: LevelOK,
			Detail: "no principal other than an admin and pco's own may use /sdn/zones/localnetwork/vmbr1/20, the segment of net0"}},
		{"a segment whose vnet has a zone of its own", "segment access", nil, func(a *fakeApp, _ *fakeEnv) {
			a.config["net0"] = "name=eth0,bridge=vnet1,hwaddr=BC:24:11:00:92:40,ip=dhcp,type=veth"
			a.data = applianceAccess(granted("/sdn/zones/zone1/vnet1", "PVESDNUser"))
		}, Finding{Check: "segment access", Level: LevelWarn,
			Detail: "pcotest@pve holds SDN.Use on /sdn/zones/zone1/vnet1, the segment of net0 of the appliance: a guest put on it with the MAC of the appliance cuts off its inbound traffic",
			Fix:    "put the appliance on a segment only admins may use, or take SDN.Use on /sdn/zones/zone1/vnet1 from the principals named"}},
		{"a segment of a configuration without net0", "segment access", nil, func(a *fakeApp, _ *fakeEnv) { delete(a.config, "net0") },
			Finding{Check: "segment access", Level: LevelWarn, Detail: "the configuration of the container has no net0: the segment of the appliance is not known",
				Fix: repair}},
		{"a segment of unreadable access control", "segment access", nil, func(a *fakeApp, _ *fakeEnv) { a.dataErr = errors.New("403") },
			Finding{Check: "segment access", Level: LevelWarn, Detail: "who may use the segment of the appliance is not known: the access control of Proxmox cannot be read: 403",
				Fix: "the access check says why"}},
		// api
		{"a certificate that does not verify", "api", nil, func(a *fakeApp, env *fakeEnv) {
			a.verifyErr = errors.New("x509: certificate signed by unknown authority")
			env.pveErr = errors.New("tls: failed to verify certificate")
		}, Finding{Check: "api", Level: LevelFail,
			Detail: "the certificate of 10.92.0.1:8006 no longer verifies under pve1 (the cluster CA or the pveproxy certificate changed?): x509: certificate signed by unknown authority",
			Fix:    repair}},
		{"an API that does not answer", "api", nil, func(a *fakeApp, env *fakeEnv) { env.pveErr = errors.New("connection refused") },
			Finding{Check: "api", Level: LevelFail,
				Detail: "the API at 10.92.0.1:8006 does not answer under pve1: connection refused",
				Fix:    "check that pveproxy runs on the node and that the appliance reaches it on port 8006"}},
		// firewall
		{"firewalls that are off", "firewall", nil, func(a *fakeApp, _ *fakeEnv) { a.datacenter, a.node = false, false },
			Finding{Check: "firewall", Level: LevelOK, Detail: "the datacenter firewall is off; the firewall of node pve1 is off"}},
		{"a firewall that cannot be read", "firewall", nil, func(a *fakeApp, _ *fakeEnv) { a.firewall = errors.New("403") },
			Finding{Check: "firewall", Level: LevelWarn, Detail: "the firewall options could not be read: 403",
				Fix: "the api and proxmox checks say why Proxmox does not answer"}},
		// egress probe
		{"an edge the connector does not reach", "egress probe", nil, func(a *fakeApp, _ *fakeEnv) {
			a.probes["region1.v2.argotunnel.com:7844"] = errors.New("dial tcp: i/o timeout")
		}, Finding{Check: "egress probe", Level: LevelFail,
			Detail: "as pco-connector, region1.v2.argotunnel.com:7844 does not connect: dial tcp: i/o timeout; the connectors cannot reach Cloudflare",
			Fix:    "the appliance needs a way out to Cloudflare on port 7844, TCP and UDP; pco egress show shows the table"}},
		{"an API the connector reaches", "egress probe", nil, func(a *fakeApp, _ *fakeEnv) { a.probes[apiAddr] = nil },
			Finding{Check: "egress probe", Level: LevelFail,
				Detail: "as pco-connector, 10.92.0.1:8006 connects: the egress filter does not confine the connectors",
				Fix:    fixTable}},
		{"an API the connector does not get an answer from", "egress probe", nil, func(a *fakeApp, _ *fakeEnv) {
			a.probes[apiAddr] = errors.New("dial tcp 10.92.0.1:8006: i/o timeout")
		}, Finding{Check: "egress probe", Level: LevelFail,
			Detail: "as pco-connector, 10.92.0.1:8006 is not refused, as the egress filter refuses it, but fails otherwise: dial tcp 10.92.0.1:8006: i/o timeout",
			Fix:    fixTable}},
		{"both probes wrong", "egress probe", nil, func(a *fakeApp, _ *fakeEnv) {
			a.probes["region1.v2.argotunnel.com:7844"] = ErrRefused
			a.probes[apiAddr] = nil
		}, Finding{Check: "egress probe", Level: LevelFail,
			Detail: "as pco-connector, region1.v2.argotunnel.com:7844 does not connect: connection refused; the connectors cannot reach Cloudflare; " +
				"as pco-connector, 10.92.0.1:8006 connects: the egress filter does not confine the connectors",
			Fix: "the appliance needs a way out to Cloudflare on port 7844, TCP and UDP; pco egress show shows the table; " + fixTable}},
		// cloudflare api and clock
		{"api.cloudflare.com that does not answer", "cloudflare api", nil, func(a *fakeApp, _ *fakeEnv) { a.cfErr = errors.New("i/o timeout") },
			Finding{Check: "cloudflare api", Level: LevelWarn, Detail: "api.cloudflare.com does not answer for pco: i/o timeout",
				Fix: "let the appliance reach api.cloudflare.com on port 443: every change at Cloudflare needs it"}},
		{"a clock that cannot be compared", "clock", nil, func(a *fakeApp, _ *fakeEnv) { a.cfErr = errors.New("i/o timeout") },
			Finding{Check: "clock", Level: LevelWarn, Detail: "the clock was not compared with Cloudflare's: api.cloudflare.com does not answer: i/o timeout",
				Fix: "see the cloudflare api check"}},
		{"a clock 29 seconds ahead", "clock", nil, func(a *fakeApp, _ *fakeEnv) { a.cfDate = now.Add(-29 * time.Second) },
			Finding{Check: "clock", Level: LevelOK, Detail: "the clock is within 29s of Cloudflare's"}},
		{"a clock 31 seconds ahead", "clock", nil, func(a *fakeApp, _ *fakeEnv) { a.cfDate = now.Add(-31 * time.Second) },
			Finding{Check: "clock", Level: LevelWarn, Detail: "the clock is 31s ahead of Cloudflare's",
				Fix: "set the clock of the node, which the container shares: check its time synchronisation (chrony or systemd-timesyncd)"}},
		{"a clock four minutes behind", "clock", nil, func(a *fakeApp, _ *fakeEnv) { a.cfDate = now.Add(4 * time.Minute) },
			Finding{Check: "clock", Level: LevelWarn, Detail: "the clock is 4m0s behind Cloudflare's",
				Fix: "set the clock of the node, which the container shares: check its time synchronisation (chrony or systemd-timesyncd)"}},
		{"a clock six minutes behind", "clock", nil, func(a *fakeApp, _ *fakeEnv) { a.cfDate = now.Add(6 * time.Minute) },
			Finding{Check: "clock", Level: LevelFail, Detail: "the clock is 6m0s behind Cloudflare's",
				Fix: "set the clock of the node, which the container shares: check its time synchronisation (chrony or systemd-timesyncd)"}},
		// dns
		{"no resolver", "dns", nil, func(a *fakeApp, _ *fakeEnv) { a.resolvers = nil },
			Finding{Check: "dns", Level: LevelFail, Detail: "no name server is configured: the connectors cannot resolve the names of Cloudflare's edge",
				Fix: "on the node: pct set 9240 --nameserver <address>, then restart the container"}},
		{"resolvers that cannot be read", "dns", nil, func(a *fakeApp, _ *fakeEnv) { a.resolveErr = errors.New("permission denied") },
			Finding{Check: "dns", Level: LevelFail, Detail: "the name servers cannot be read: permission denied",
				Fix: "check /etc/resolv.conf in the appliance"}},
		{"resolvers that do not answer", "dns", nil, func(a *fakeApp, _ *fakeEnv) { a.lookupErr = errors.New("i/o timeout") },
			Finding{Check: "dns", Level: LevelFail,
				Detail: "the name servers (10.92.0.1) do not answer for region1.v2.argotunnel.com: i/o timeout; the connectors cannot resolve the edge",
				Fix:    "let the appliance reach its name servers; pct config 9240 on the node shows what it was given"}},
		// holds
		{"nothing held", "holds", nil, func(a *fakeApp, _ *fakeEnv) { a.holds = nil },
			Finding{Check: "holds", Level: LevelWarn,
				Detail: "pco and cloudflared are not held: apt or unattended-upgrades may replace them",
				Fix:    "apt-mark hold pco cloudflared"}},
		{"cloudflared not held", "holds", nil, func(a *fakeApp, _ *fakeEnv) { a.holds = []string{"pco", "tzdata"} },
			Finding{Check: "holds", Level: LevelWarn,
				Detail: "cloudflared is not held: apt or unattended-upgrades may replace it",
				Fix:    "apt-mark hold cloudflared"}},
		{"holds that cannot be read", "holds", nil, func(a *fakeApp, _ *fakeEnv) { a.holdsErr = errors.New("apt-mark: not found") },
			Finding{Check: "holds", Level: LevelWarn, Detail: "the held packages could not be read: apt-mark: not found", Fix: "apt-mark showhold"}},
		// unattended-upgrades
		{"unattended-upgrades off", "unattended-upgrades", nil, func(a *fakeApp, _ *fakeEnv) { a.unattended = false },
			Finding{Check: "unattended-upgrades", Level: LevelWarn,
				Detail: "unattended-upgrades is not enabled and configured: the security updates of Debian are not installed on their own",
				Fix:    "systemctl enable unattended-upgrades.service, and keep APT::Periodic::Unattended-Upgrade \"1\" in /etc/apt/apt.conf.d"}},
		{"unattended-upgrades unreadable", "unattended-upgrades", nil, func(a *fakeApp, _ *fakeEnv) { a.upgradeErr = errors.New("apt-config: not found") },
			Finding{Check: "unattended-upgrades", Level: LevelWarn, Detail: "whether unattended-upgrades is set up could not be read: apt-config: not found",
				Fix: "systemctl status unattended-upgrades.service"}},
		// disk
		{"a root disk at 80 percent", "disk", nil, func(a *fakeApp, _ *fakeEnv) { a.disk["/"] = [2]uint64{80, 100} },
			Finding{Check: "disk", Level: LevelWarn, Detail: "/ is 80% full (80 B of 100 B); /var/lib/pco is 4% full (100.0 MiB of 2.0 GiB)",
				Fix: "free space on /: apt-get clean, or grow it on the node: pct resize 9240 rootfs +1G"}},
		{"a root disk at 79 percent", "disk", nil, func(a *fakeApp, _ *fakeEnv) { a.disk["/"] = [2]uint64{79, 100} },
			Finding{Check: "disk", Level: LevelOK, Detail: "/ is 79% full (79 B of 100 B); /var/lib/pco is 4% full (100.0 MiB of 2.0 GiB)"}},
		{"a state volume at 90 percent", "disk", nil, func(a *fakeApp, _ *fakeEnv) { a.disk["/var/lib/pco"] = [2]uint64{90, 100} },
			Finding{Check: "disk", Level: LevelFail, Detail: "/ is 25% full (1.0 GiB of 4.0 GiB); /var/lib/pco is 90% full (90 B of 100 B)",
				Fix: "grow /var/lib/pco on the node: pct resize 9240 mp0 +1G"}},
		{"both disks over", "disk", nil, func(a *fakeApp, _ *fakeEnv) {
			a.disk["/"] = [2]uint64{85, 100}
			a.disk["/var/lib/pco"] = [2]uint64{95, 100}
		}, Finding{Check: "disk", Level: LevelFail, Detail: "/ is 85% full (85 B of 100 B); /var/lib/pco is 95% full (95 B of 100 B)",
			Fix: "free space on /: apt-get clean, or grow it on the node: pct resize 9240 rootfs +1G; grow /var/lib/pco on the node: pct resize 9240 mp0 +1G"}},
		{"a disk that cannot be read", "disk", nil, func(a *fakeApp, _ *fakeEnv) { a.diskErr = errors.New("statfs: io error") },
			Finding{Check: "disk", Level: LevelWarn,
				Detail: "the use of / could not be read: statfs: io error; the use of /var/lib/pco could not be read: statfs: io error", Fix: "df -h"}},
		// journal
		{"a journal of 64 MiB", "journal", nil, func(a *fakeApp, _ *fakeEnv) { a.journal = 64 << 20 },
			Finding{Check: "journal", Level: LevelWarn, Detail: "the journal takes 64.0 MiB, where the appliance keeps it to 64 MiB",
				Fix: "journalctl --vacuum-size=32M, and look in journalctl -p warning for what writes so much"}},
		{"a journal of 63 MiB", "journal", nil, func(a *fakeApp, _ *fakeEnv) { a.journal = 63 << 20 },
			Finding{Check: "journal", Level: LevelOK, Detail: "the journal takes 63.0 MiB"}},
		{"a journal that cannot be read", "journal", nil, func(a *fakeApp, _ *fakeEnv) { a.journalErr = errors.New("permission denied") },
			Finding{Check: "journal", Level: LevelWarn, Detail: "the size of the journal could not be read: permission denied", Fix: "journalctl --disk-usage"}},
		// memory
		{"memory pressure of 10", "memory", nil, func(a *fakeApp, _ *fakeEnv) { a.pressure = 10 },
			Finding{Check: "memory", Level: LevelOK, Detail: "memory pressure is 10.0%"}},
		{"memory pressure above 10", "memory", nil, func(a *fakeApp, _ *fakeEnv) { a.pressure = 10.1 },
			Finding{Check: "memory", Level: LevelWarn,
				Detail: "memory pressure is 10.1%: tasks of the appliance waited for memory for that share of the last 10 seconds",
				Fix:    "on the node: pct set 9240 --memory 1024"}},
		{"pressure the kernel does not report", "memory", nil, func(a *fakeApp, _ *fakeEnv) { a.pressErr = fs.ErrNotExist },
			Finding{Check: "memory", Level: LevelOK, Detail: "the kernel reports no memory pressure to this container"}},
		{"pressure that cannot be read", "memory", nil, func(a *fakeApp, _ *fakeEnv) { a.pressErr = errors.New("bad format") },
			Finding{Check: "memory", Level: LevelWarn, Detail: "memory pressure could not be read: bad format", Fix: "cat /proc/pressure/memory"}},
		// versions
		{"a cloudflared the manifest denies", "versions", nil, func(a *fakeApp, _ *fakeEnv) { a.cloud = "2026.9.0" },
			Finding{Check: "versions", Level: LevelFail,
				Detail: "pco 1.0.0, cloudflared 2026.9.0, Debian 13.1; cloudflared 2026.9.0 is denied by the list of vetted versions: a bug",
				Fix:    "pco upgrade cloudflared"}},
		{"a manifest 89 days old", "versions", nil, func(a *fakeApp, _ *fakeEnv) { a.manifest.Updated = now.AddDate(0, 0, -89) },
			Finding{Check: "versions", Level: LevelOK, Detail: "pco 1.0.0, cloudflared 2026.9.3, Debian 13.1"}},
		{"a manifest 91 days old", "versions", nil, func(a *fakeApp, _ *fakeEnv) { a.manifest.Updated = now.AddDate(0, 0, -91) },
			Finding{Check: "versions", Level: LevelWarn,
				Detail: "pco 1.0.0, cloudflared 2026.9.3, Debian 13.1; the list of vetted cloudflared versions is from 2026-07-02; a newer pco release ships a newer one",
				Fix:    "pco upgrade --check says whether a newer release is out"}},
		{"a cloudflared more than ten months old", "versions", nil, func(a *fakeApp, _ *fakeEnv) { a.cloud = "2025.11.2" },
			Finding{Check: "versions", Level: LevelWarn,
				Detail: "pco 1.0.0, cloudflared 2025.11.2, Debian 13.1; cloudflared 2025.11.2 is more than ten months old",
				Fix:    "pco upgrade cloudflared"}},
		{"a cloudflared less than ten months old", "versions", nil, func(a *fakeApp, _ *fakeEnv) { a.cloud = "2026.1.0" },
			Finding{Check: "versions", Level: LevelOK, Detail: "pco 1.0.0, cloudflared 2026.1.0, Debian 13.1"}},
		{"versions that cannot be read", "versions", nil, func(a *fakeApp, _ *fakeEnv) { a.versionErr = errors.New("cloudflared does not run") },
			Finding{Check: "versions", Level: LevelWarn, Detail: "the versions could not be read: cloudflared does not run", Fix: "pco version; cloudflared --version"}},
		{"a manifest that cannot be read", "versions", nil, func(a *fakeApp, _ *fakeEnv) { a.manifestEr = errors.New("no such file") },
			Finding{Check: "versions", Level: LevelWarn,
				Detail: "pco 1.0.0, cloudflared 2026.9.3, Debian 13.1; the list of vetted cloudflared versions could not be read: no such file",
				Fix:    "pco upgrade pco installs a release with its list"}},
		// net
		{"a service prefix that is not in place", "net", nil, func(a *fakeApp, _ *fakeEnv) { a.netErr = errors.New("the dummy device pco0 is missing or down") },
			Finding{Check: "net", Level: LevelFail,
				Detail: "what pco-net.service loads is not in place: the dummy device pco0 is missing or down",
				Fix:    "pco net show says what differs; the daemon loads it again within 30 seconds"}},
		{"packets that leaked", "net", nil, func(a *fakeApp, _ *fakeEnv) { a.leaked = 12 },
			Finding{Check: "net", Level: LevelWarn,
				Detail: "12 packets were sent to the service prefix without a mapping and rejected since the table was loaded",
				Fix:    "pco net show shows the count: a few after a route was withdrawn are usual, a count that grows is not"}},
		{"one packet that leaked", "net", nil, func(a *fakeApp, _ *fakeEnv) { a.leaked = 1 },
			Finding{Check: "net", Level: LevelWarn,
				Detail: "1 packet was sent to the service prefix without a mapping and rejected since the table was loaded",
				Fix:    "pco net show shows the count: a few after a route was withdrawn are usual, a count that grows is not"}},
		{"a counter that cannot be read", "net", nil, func(a *fakeApp, _ *fakeEnv) { a.leakedErr = errors.New("nft: not found") },
			Finding{Check: "net", Level: LevelWarn, Detail: "what the service prefix rejected could not be counted: nft: not found", Fix: "pco net show"}},
		// etc-pve
		{"a bind mount of /etc/pve", "etc-pve", nil, func(a *fakeApp, _ *fakeEnv) { a.etcPVE = true },
			Finding{Check: "etc-pve", Level: LevelFail,
				Detail: "/etc/pve exists in the container: a bind mount of the cluster filesystem is in the container; remove it",
				Fix: "remove the mount point whose mp is /etc/pve (pct config 9240 on the node shows it, pct set 9240 --delete mpN removes it): " +
					"with the node's www-data group mapped in, it lets the container read the TLS keys of the node"}},
		{"a bind mount of /etc/pve in mp1", "etc-pve", nil, func(a *fakeApp, _ *fakeEnv) {
			a.etcPVE = true
			a.config["mp1"] = "/etc/pve,mp=/etc/pve"
		}, Finding{Check: "etc-pve", Level: LevelFail,
			Detail: "/etc/pve exists in the container: a bind mount of the cluster filesystem is in the container; remove it",
			Fix: "on the node: pct set 9240 --delete mp1; with the node's www-data group mapped in, " +
				"a bind mount of /etc/pve lets the container read the TLS keys of the node"}},
		// quic buffer
		{"a small receive buffer", "quic buffer", nil, func(a *fakeApp, _ *fakeEnv) { a.rmem = 4194304 },
			Finding{Check: "quic buffer", Level: LevelWarn,
				Detail: "net.core.rmem_max is 4194304, below the 7500000 bytes cloudflared asks for its QUIC connections; it is a setting of the node, which a container cannot change",
				Fix:    "if you want it, on the node: echo net.core.rmem_max=7500000 >> /etc/sysctl.d/90-pco.conf, then sysctl --system"}},
		{"a buffer of exactly 7500000", "quic buffer", nil, func(a *fakeApp, _ *fakeEnv) { a.rmem = 7500000 },
			Finding{Check: "quic buffer", Level: LevelOK, Detail: "net.core.rmem_max is 7500000"}},
		{"a buffer that cannot be read", "quic buffer", nil, func(a *fakeApp, _ *fakeEnv) { a.rmemErr = errors.New("no such file") },
			Finding{Check: "quic buffer", Level: LevelWarn, Detail: "net.core.rmem_max could not be read: no such file", Fix: "sysctl net.core.rmem_max"}},
		// nftables
		{"nftables.service enabled", "nftables", nil, func(a *fakeApp, _ *fakeEnv) { a.unit["nftables.service"] = "enabled" },
			Finding{Check: "nftables", Level: LevelFail,
				Detail: "nftables.service is enabled: when it starts, its ruleset flushes both tables of pco, and the connectors are not confined until the daemon loads them again",
				Fix:    "systemctl mask nftables.service"}},
		{"nftables.service enabled by hand", "nftables", nil, func(a *fakeApp, _ *fakeEnv) { a.unit["nftables.service"] = "enabled-runtime" },
			Finding{Check: "nftables", Level: LevelFail,
				Detail: "nftables.service is enabled: when it starts, its ruleset flushes both tables of pco, and the connectors are not confined until the daemon loads them again",
				Fix:    "systemctl mask nftables.service"}},
		{"nftables.service disabled", "nftables", nil, func(a *fakeApp, _ *fakeEnv) { a.unit["nftables.service"] = "disabled" },
			Finding{Check: "nftables", Level: LevelWarn,
				Detail: "nftables.service is disabled, not masked: a package or an admin that starts or enables it flushes both tables of pco",
				Fix:    "systemctl mask nftables.service"}},
		{"nftables.service not installed", "nftables", nil, func(a *fakeApp, _ *fakeEnv) { delete(a.unit, "nftables.service") },
			Finding{Check: "nftables", Level: LevelOK, Detail: "nftables.service is not installed"}},
		{"nftables.service that systemd does not tell of", "nftables", nil, func(a *fakeApp, _ *fakeEnv) { a.unitErr = errors.New("Failed to connect to bus") },
			Finding{Check: "nftables", Level: LevelWarn, Detail: "systemd did not say whether nftables.service is masked: Failed to connect to bus",
				Fix: "systemctl is-enabled nftables.service"}},
		// epoch
		{"an epoch drawn at this start", "epoch", func(st *engine.State) { st.EpochDrawnAt = now.Add(-time.Minute) }, nil,
			Finding{Check: "epoch", Level: LevelOK,
				Detail: "a new epoch was drawn at 2026-10-01T11:59:00Z after a container start; the state is the volume's: " +
					"after a rollback, approvals, acknowledged segments and credentials made after the snapshot are gone"}},
		// network grants
		{"a grant of a bridge", "network grants", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(
				pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr1", Type: "user", UGID: "pco@pve", RoleID: "PCOSDN", Propagate: true},
				pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr1", Type: "token", UGID: pcoToken, RoleID: "PCOSDN", Propagate: true},
				pve.ACLEntry{Path: "/vms/9240", Type: "token", UGID: pcoToken, RoleID: "PCOManaged", Propagate: true},
			)
		}, Finding{Check: "network grants", Level: LevelOK, Detail: "pco@pve and its token may put cards on /sdn/zones/localnetwork/vmbr1"}},
		{"grants of two networks", "network grants", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(
				pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr1/20", Type: "user", UGID: "pco@pve", RoleID: "PCOSDN", Propagate: true},
				pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr1/20", Type: "token", UGID: pcoToken, RoleID: "PCOSDN", Propagate: true},
				pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr2", Type: "user", UGID: "pco@pve", RoleID: "PCOSDN", Propagate: true},
				pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr2", Type: "token", UGID: pcoToken, RoleID: "PCOSDN", Propagate: true},
			)
		}, Finding{Check: "network grants", Level: LevelOK,
			Detail: "pco@pve and its token may put cards on /sdn/zones/localnetwork/vmbr1/20, /sdn/zones/localnetwork/vmbr2"}},
		{"a grant of a bridge with the address of the node", "network grants", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(
				pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr0", Type: "user", UGID: "pco@pve", RoleID: "PCOSDN", Propagate: true},
				pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr0", Type: "token", UGID: pcoToken, RoleID: "PCOSDN", Propagate: true},
			)
		}, Finding{Check: "network grants", Level: LevelWarn,
			Detail: "pco@pve and its token may put cards on /sdn/zones/localnetwork/vmbr0; the bridge vmbr0 carries 10.92.0.1/24, an address of the node: " +
				"a card there is on the segment of the node's own services",
			Fix: "on the node: pco appliance revoke-network --vmid 9240 --bridge vmbr0"}},
		{"a grant of a VLAN of a bridge with the address of the node", "network grants", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(
				pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr0/20", Type: "user", UGID: "pco@pve", RoleID: "PCOSDN", Propagate: true},
				pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr0/20", Type: "token", UGID: pcoToken, RoleID: "PCOSDN", Propagate: true},
			)
		}, Finding{Check: "network grants", Level: LevelWarn,
			Detail: "pco@pve and its token may put cards on /sdn/zones/localnetwork/vmbr0/20; the bridge vmbr0 carries 10.92.0.1/24, an address of the node: " +
				"a card there is on the segment of the node's own services",
			Fix: "on the node: pco appliance revoke-network --vmid 9240 --bridge vmbr0 --vlan 20"}},
		{"a grant of a zone", "network grants", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(
				pve.ACLEntry{Path: "/sdn/zones/localnetwork", Type: "user", UGID: "pco@pve", RoleID: "PCOSDN", Propagate: true},
				pve.ACLEntry{Path: "/sdn/zones/localnetwork", Type: "token", UGID: pcoToken, RoleID: "PCOSDN", Propagate: true},
			)
		}, Finding{Check: "network grants", Level: LevelWarn,
			Detail: "pco@pve and its token may put cards on /sdn/zones/localnetwork; /sdn/zones/localnetwork is a whole zone: every bridge or vnet of it, present and future",
			Fix: "on the node: pveum acl delete /sdn/zones/localnetwork --roles PCOSDN --users pco@pve; " +
				"pveum acl delete /sdn/zones/localnetwork --roles PCOSDN --tokens pco@pve!vm9240; then grant the networks one by one with pco appliance grant-network"}},
		{"a grant of every zone", "network grants", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(
				pve.ACLEntry{Path: "/sdn", Type: "user", UGID: "pco@pve", RoleID: "PCOSDN", Propagate: true},
				pve.ACLEntry{Path: "/sdn", Type: "token", UGID: pcoToken, RoleID: "PCOSDN", Propagate: true},
			)
		}, Finding{Check: "network grants", Level: LevelWarn,
			Detail: "pco@pve and its token may put cards on /sdn; /sdn is a whole zone: every bridge or vnet of it, present and future",
			Fix: "on the node: pveum acl delete /sdn --roles PCOSDN --users pco@pve; " +
				"pveum acl delete /sdn --roles PCOSDN --tokens pco@pve!vm9240; then grant the networks one by one with pco appliance grant-network"}},
		{"a grant to the user alone", "network grants", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr0", Type: "user", UGID: "pco@pve", RoleID: "PCOSDN", Propagate: true})
		}, Finding{Check: "network grants", Level: LevelOK, Detail: "pco@pve and its token may put no card on a network but their own"}},
		{"a role that holds no SDN.Use", "network grants", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(
				pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr0", Type: "user", UGID: "pco@pve", RoleID: "PCO", Propagate: true},
				pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr0", Type: "token", UGID: pcoToken, RoleID: "PCO", Propagate: true},
			)
		}, Finding{Check: "network grants", Level: LevelOK, Detail: "pco@pve and its token may put no card on a network but their own"}},
		{"a grant to someone else", "network grants", nil, func(a *fakeApp, _ *fakeEnv) {
			a.data = applianceAccess(granted("/sdn/zones/localnetwork/vmbr0", "PVESDNUser"))
		}, Finding{Check: "network grants", Level: LevelOK, Detail: "pco@pve and its token may put no card on a network but their own"}},
		{"network grants of a node network that cannot be read", "network grants", nil, func(a *fakeApp, _ *fakeEnv) {
			a.nodeNetErr = errors.New("403")
			a.data = applianceAccess(
				pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr1", Type: "user", UGID: "pco@pve", RoleID: "PCOSDN", Propagate: true},
				pve.ACLEntry{Path: "/sdn/zones/localnetwork/vmbr1", Type: "token", UGID: pcoToken, RoleID: "PCOSDN", Propagate: true},
			)
		}, Finding{Check: "network grants", Level: LevelWarn,
			Detail: "pco@pve and its token may put cards on /sdn/zones/localnetwork/vmbr1; the addresses of the node could not be read to compare them: 403",
			Fix:    "the api and proxmox checks say why Proxmox does not answer"}},
		{"network grants of unreadable access control", "network grants", nil, func(a *fakeApp, _ *fakeEnv) { a.dataErr = errors.New("403") },
			Finding{Check: "network grants", Level: LevelWarn,
				Detail: "what pco@pve may put cards on is not known: the access control of Proxmox cannot be read: 403", Fix: "the access check says why"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expect, found(t, tt.check, tt.state, tt.app))
		})
	}
}

func TestTheProbeIsAskedAboutTheEdgeAndTheAPI(t *testing.T) {
	env := healthyApplianceEnv()
	env.app.self.Address = "pve1.example.net:8006"
	env.app.probes = map[string]error{"pve1.example.net:8006": ErrRefused}

	findings := Run(t.Context(), healthyApplianceState(), env)

	require.Equal(t, LevelOK, only(findings, "egress probe").Level, only(findings, "egress probe").Detail)
	require.ElementsMatch(t, []string{"region1.v2.argotunnel.com:7844", "pve1.example.net:8006"}, env.app.probed)
}

func TestTheDoctorOfAnApplianceAnswersWhenTheAPIDoesNot(t *testing.T) {
	env := healthyApplianceEnv()
	broken := errors.New("500 boom")
	env.app.configErr, env.app.pendingErr, env.app.snapErr, env.app.jobsErr, env.app.permsErr = broken, broken, broken, broken, broken
	env.app.dataErr, env.app.nodeNetErr, env.app.firewall = broken, broken, broken
	env.pveErr = broken

	findings := Run(t.Context(), healthyApplianceState(), env)

	have := map[string]Level{}
	for _, f := range findings {
		have[f.Check] = f.Level
	}
	for _, check := range applianceChecks {
		_, ok := have[check]
		require.True(t, ok, "no finding of %q", check)
	}
	require.Equal(t, LevelFail, have["api"])
	require.Equal(t, LevelFail, have["access"])
	require.Equal(t, LevelWarn, have["features"])
}

// meeting makes the calls that reach it wait for each other: they all end
// only when every one of them is in at once.
type meeting struct {
	left    atomic.Int32
	all     chan struct{}
	timeout time.Duration
	met     atomic.Bool
}

func newMeeting(n int) *meeting {
	m := &meeting{all: make(chan struct{}), timeout: 5 * time.Second}
	m.left.Store(int32(n))
	return m
}

func (m *meeting) meet() {
	if m.left.Add(-1) == 0 {
		m.met.Store(true)
		close(m.all)
	}
	select {
	case <-m.all:
	case <-time.After(m.timeout):
	}
}

// meetingApp has two checks and the two probes of a third wait for each other.
type meetingApp struct {
	*fakeApp
	m *meeting
}

func (a meetingApp) Holds(ctx context.Context) ([]string, error) {
	a.m.meet()
	return a.fakeApp.Holds(ctx)
}

func (a meetingApp) Snapshots(ctx context.Context) ([]pve.Snapshot, error) {
	a.m.meet()
	return a.fakeApp.Snapshots(ctx)
}

func (a meetingApp) ProbeAsConnector(ctx context.Context, addr string) error {
	a.m.meet()
	return a.fakeApp.ProbeAsConnector(ctx, addr)
}

type meetingEnv struct {
	*fakeEnv
	app meetingApp
}

func (e *meetingEnv) Appliance() ApplianceEnv { return e.app }

func TestTheChecksOfTheApplianceRunTogether(t *testing.T) {
	m := newMeeting(4)
	env := &meetingEnv{fakeEnv: healthyEnv(), app: meetingApp{fakeApp: healthyApp(), m: m}}

	findings := Run(t.Context(), healthyApplianceState(), env)

	require.True(t, m.met.Load(), "two checks and the two probes of a third were not in at once")
	require.Equal(t, LevelOK, only(findings, "holds").Level)
	require.Equal(t, LevelOK, only(findings, "egress probe").Level)
}

type panickingApp struct{ *fakeApp }

func (panickingApp) Holds(context.Context) ([]string, error) { panic("boom") }

type panickingEnv struct {
	*fakeEnv
	app panickingApp
}

func (e *panickingEnv) Appliance() ApplianceEnv { return e.app }

func TestACheckThatPanicsIsAFailureAndTheOthersAnswer(t *testing.T) {
	env := &panickingEnv{fakeEnv: healthyEnv(), app: panickingApp{healthyApp()}}

	findings := Run(t.Context(), healthyApplianceState(), env)

	require.Equal(t, LevelFail, only(findings, "holds").Level)
	require.Contains(t, only(findings, "holds").Detail, "stopped on an internal error")
	require.Equal(t, LevelOK, only(findings, "features").Level)
}

func TestTheWriterOfAnApplianceFixesWithItsOwnCommands(t *testing.T) {
	for _, tt := range []struct {
		name    string
		profile string
		verdict string
		expect  Finding
	}{
		{"stale in an appliance", store.ProfileAppliance, engine.VerdictStale,
			Finding{Check: "writer", Level: LevelFail, Detail: "a newer generation of this install writes the tunnel configuration",
				Fix: "run pco appliance recover in the appliance that should write"}},
		{"stale on a host", store.ProfileHost, engine.VerdictStale,
			Finding{Check: "writer", Level: LevelFail, Detail: "a newer generation of this install writes the tunnel configuration",
				Fix: "run pco setup --recover on the node that should write"}},
		{"unknown in an appliance", store.ProfileAppliance, engine.VerdictUnknown,
			Finding{Check: "writer", Level: LevelFail, Detail: "leader.json could not be used", Fix: "pco appliance recover"}},
		{"unknown on a host", store.ProfileHost, engine.VerdictUnknown,
			Finding{Check: "writer", Level: LevelFail, Detail: "leader.json could not be used", Fix: "pco setup --recover"}},
		{"behind in an appliance", store.ProfileAppliance, engine.VerdictBehind,
			Finding{Check: "writer", Level: LevelFail,
				Detail: "the state of this appliance is older than its last write at Cloudflare (rollback or restore)",
				Fix:    "pct exec 9240 -- pco appliance recover"}},
		{"foreign in an appliance", store.ProfileAppliance, engine.VerdictForeign,
			Finding{Check: "writer", Level: LevelFail,
				Detail: "a writer of this install that leader.json does not know wrote the tunnel configuration: " +
					"another installation with this install's id, or a sentinel written with a stolen Cloudflare token",
				Fix: "if no other installation runs with this install id, replace the Cloudflare token, run pco tunnel rotate " +
					"and pco appliance recover in the appliance, then pco apply; otherwise stop the other installation"}},
		{"foreign on a host", store.ProfileHost, engine.VerdictForeign,
			Finding{Check: "writer", Level: LevelFail,
				Detail: "a writer of this install that leader.json does not know wrote the tunnel configuration: " +
					"another installation with this install's id, or a sentinel written with a stolen Cloudflare token",
				Fix: "if no other node runs pco with this install, replace the Cloudflare token, run pco tunnel rotate " +
					"and pco setup --recover on this node, then pco apply; otherwise stop the other installation"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := healthyApplianceState()
			st.Profile, st.WriterVerdict = tt.profile, tt.verdict

			require.Equal(t, tt.expect, only(Run(t.Context(), st, healthyEnv()), "writer"))
		})
	}
}

func TestABehindWriterNamesNoOtherInstallation(t *testing.T) {
	st := healthyApplianceState()
	st.WriterVerdict = engine.VerdictBehind

	f := only(Run(t.Context(), st, healthyEnv()), "writer")

	require.NotContains(t, f.Detail+f.Fix, "other installation")
	require.NotContains(t, f.Detail+f.Fix, "stop")
}

func TestABehindWriterWithoutAnIdentityKeepsAPlaceholder(t *testing.T) {
	st := healthyApplianceState()
	st.WriterVerdict, st.Identity = engine.VerdictBehind, nil

	f := only(Run(t.Context(), st, healthyEnv()), "writer")

	require.Equal(t, "pct exec <vmid> -- pco appliance recover", f.Fix)
}

func TestTheStoreOfAnApplianceNamesTheRepair(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		fix  string
	}{
		{"a volume that is no mount", fmt.Errorf("/var/lib/pco %w", appliance.ErrNotMountPoint), "run pco appliance repair --vmid 9240 on the node"},
		{"a volume without a marker", fmt.Errorf("/var/lib/pco %w", appliance.ErrNoMarker), "run pco appliance repair --vmid 9240 on the node"},
		{"a store that is not set up", errors.New("pco is not set up on this node; run pco setup"), "run pco appliance repair --vmid 9240 on the node"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := healthyApplianceEnv()
			env.storeErr = tt.err

			f := only(Run(t.Context(), healthyApplianceState(), env), "store")

			require.Equal(t, Finding{Check: "store", Level: LevelFail, Detail: tt.err.Error(), Fix: tt.fix}, f)
		})
	}
}

func TestTheStoreOfAHostKeepsItsFix(t *testing.T) {
	env := healthyEnv()
	env.storeErr = fmt.Errorf("%w: /etc/pve", store.ErrNotMounted)
	st := healthyState()
	st.Profile = store.ProfileHost

	f := only(Run(t.Context(), st, env), "store")

	require.Equal(t, "systemctl status pve-cluster", f.Fix)
}

func TestTheCloudflaredAgeIsOneRuleForBothChecks(t *testing.T) {
	env := healthyApplianceEnv()
	env.version = "cloudflared version 2025.11.2 (built 2025-11-30)"
	env.app.cloud = "2025.11.2"

	findings := Run(t.Context(), healthyApplianceState(), env)

	require.Equal(t, Finding{Check: "cloudflared", Level: LevelWarn, Detail: "cloudflared 2025.11.2 is more than ten months old", Fix: "update cloudflared"},
		only(findings, "cloudflared"), "the check of both profiles says as it did")
	require.Equal(t, LevelWarn, only(findings, "versions").Level)
}

func TestTheApplianceDoctorSortsItsFindingsWithTheOthers(t *testing.T) {
	findings := Run(t.Context(), healthyApplianceState(), healthyApplianceEnv())

	require.True(t, slices.IsSortedFunc(findings, compareChecks))
	require.Contains(t, checks(findings), "cycle")
	require.Contains(t, checks(findings), "features")
}
