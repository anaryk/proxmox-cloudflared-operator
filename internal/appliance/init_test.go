package appliance_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/version"
)

const (
	vmid        = 9250
	mac0        = "bc:24:11:00:00:10"
	tokenID     = "pco@pve!vm9250"
	account     = "acc1"
	installID   = "0123456789ab"
	incarnation = "6a1f9c2e-4b7d-4e0a-9c3b-2d5e8f7a1b40/4242"

	// Neither secret may show in anything init prints or returns.
	pveSecret = "pve-secret-must-not-leak-9a8b7c"
	cfToken   = "cf-token-must-not-leak-4f1d2c"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// bootstrapFields are the fields of a bootstrap the installer pushed into
// lxc/9250 on pve1, as JSON takes them.
func bootstrapFields(mode string) map[string]any {
	return map[string]any{
		"schemaVersion":   1,
		"mode":            mode,
		"installId":       "",
		"vmid":            vmid,
		"node":            "pve1",
		"macs":            []string{"BC:24:11:00:00:10"},
		"endpoints":       []map[string]string{{"address": "192.168.4.208:8006", "serverName": "pve1"}},
		"nodeAddrs":       []string{"192.168.4.208", "10.92.0.100"},
		"pveToken":        map[string]string{"tokenId": tokenID, "secret": pveSecret},
		"cloudflareToken": cfToken,
		"gateTag":         "cf-tunnel",
		"manifest":        map[string]any{"vmid": vmid, "pool": "pco"},
	}
}

func writeJSON(t *testing.T, path string, v any, perm os.FileMode) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, b, perm))
	require.NoError(t, os.Chmod(path, perm))
	return path
}

func readAsOwner(path string) (appliance.Bootstrap, error) {
	return appliance.ReadBootstrapOwnedBy(path, os.Getuid())
}

// requireNoSecret fails when either token shows in s.
func requireNoSecret(t *testing.T, s string) {
	t.Helper()
	require.NotContains(t, s, pveSecret)
	require.NotContains(t, s, cfToken)
}

func TestABootstrapIsReadWithItsSecrets(t *testing.T) {
	path := writeJSON(t, filepath.Join(t.TempDir(), "bootstrap.json"), bootstrapFields("install"), 0o600)

	b, err := readAsOwner(path)

	require.NoError(t, err)
	require.Equal(t, appliance.ModeInstall, b.Mode)
	require.Equal(t, vmid, b.VMID)
	require.Equal(t, "pve1", b.Node)
	require.Equal(t, []string{mac0}, b.MACs, "in normal form")
	require.Equal(t, []store.Endpoint{{Address: "192.168.4.208:8006", ServerName: "pve1"}}, b.Endpoints)
	require.Equal(t, []netip.Addr{netip.MustParseAddr("192.168.4.208"), netip.MustParseAddr("10.92.0.100")}, b.NodeAddrs)
	require.Equal(t, tokenID, b.PVEToken.TokenID)
	require.Equal(t, pveSecret, b.PVEToken.Secret.Reveal())
	require.Equal(t, cfToken, b.CloudflareToken.Reveal())
	require.Equal(t, "cf-tunnel", b.GateTag)
	require.JSONEq(t, `{"vmid":9250,"pool":"pco"}`, string(b.Manifest))
	requireNoSecret(t, fmt.Sprintf("%v %+v %#v", b, b, b))
}

func TestABootstrapEncodesAsItIsRead(t *testing.T) {
	dir := t.TempDir()
	b, err := readAsOwner(writeJSON(t, filepath.Join(dir, "one.json"), bootstrapFields("recover"), 0o600))
	require.NoError(t, err)

	data, err := b.Encode()
	require.NoError(t, err)
	require.Contains(t, string(data), `"schemaVersion":1`)
	path := filepath.Join(dir, "two.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	again, err := readAsOwner(path)

	require.NoError(t, err)
	require.Equal(t, b.Mode, again.Mode)
	require.Equal(t, b.MACs, again.MACs)
	require.Equal(t, b.Endpoints, again.Endpoints)
	require.Equal(t, b.NodeAddrs, again.NodeAddrs)
	require.True(t, b.PVEToken.Secret.Equal(again.PVEToken.Secret))
	require.True(t, b.CloudflareToken.Equal(again.CloudflareToken))
	require.JSONEq(t, string(b.Manifest), string(again.Manifest))

	b.VMID = 0
	_, err = b.Encode()
	require.ErrorContains(t, err, "vmid", "an invalid bootstrap is not written")
}

func TestABootstrapThatIsNotRightIsRefused(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(map[string]any)
		want   string
	}{
		{"another schema", func(f map[string]any) { f["schemaVersion"] = 2 }, "schemaVersion 2: want 1"},
		{"no schema", func(f map[string]any) { delete(f, "schemaVersion") }, "schemaVersion 0: want 1"},
		{"another mode", func(f map[string]any) { f["mode"] = "upgrade" }, `mode "upgrade": want install, repair or recover`},
		{"no mode", func(f map[string]any) { delete(f, "mode") }, `mode "": want install, repair or recover`},
		{"no vmid", func(f map[string]any) { delete(f, "vmid") }, "vmid 0: want 100 to 999999999"},
		{"a vmid Proxmox never gives", func(f map[string]any) { f["vmid"] = 99 }, "vmid 99: want 100 to 999999999"},
		{"no node", func(f map[string]any) { delete(f, "node") }, `node "": want the name of a Proxmox VE node`},
		{"a node that is no name", func(f map[string]any) { f["node"] = "pve 1" }, `node "pve 1"`},
		{"no MAC", func(f map[string]any) { f["macs"] = []string{} }, "no MAC"},
		{"a MAC that is none", func(f map[string]any) { f["macs"] = []string{"bc:24:11:00:00"} }, `MAC "bc:24:11:00:00"`},
		{"no endpoint", func(f map[string]any) { delete(f, "endpoints") }, "no endpoint of the Proxmox API"},
		{"an endpoint without a port", func(f map[string]any) {
			f["endpoints"] = []map[string]string{{"address": "192.168.4.208", "serverName": "pve1"}}
		}, `endpoint address "192.168.4.208": want host:port`},
		{"an endpoint with port 0", func(f map[string]any) {
			f["endpoints"] = []map[string]string{{"address": "192.168.4.208:0", "serverName": "pve1"}}
		}, `endpoint address "192.168.4.208:0": want host:port`},
		{"an endpoint without a server name", func(f map[string]any) {
			f["endpoints"] = []map[string]string{{"address": "192.168.4.208:8006"}}
		}, "endpoint 192.168.4.208:8006 has no server name"},
		{"a loopback endpoint", func(f map[string]any) {
			f["endpoints"] = []map[string]string{{"address": "127.0.0.1:8006", "serverName": "pve1"}}
		}, "endpoint 127.0.0.1:8006 is a loopback address"},
		{"no node address", func(f map[string]any) { f["nodeAddrs"] = []string{} }, "no address of the node"},
		{"a node address that is none", func(f map[string]any) { f["nodeAddrs"] = []string{"192.168.4"} }, "192.168.4"},
		{"no token id", func(f map[string]any) { f["pveToken"] = map[string]string{"secret": pveSecret} }, `pveToken.tokenId "": want user@realm!name`},
		{"no token secret", func(f map[string]any) { f["pveToken"] = map[string]string{"tokenId": tokenID} }, "pveToken.secret is empty"},
		{"no gate tag", func(f map[string]any) { delete(f, "gateTag") }, "gateTag is empty"},
		{"no manifest", func(f map[string]any) { delete(f, "manifest") }, "manifest: want a JSON object"},
		{"a manifest that is no object", func(f map[string]any) { f["manifest"] = []int{1} }, "manifest: want a JSON object"},
		{"recover without a Cloudflare token", func(f map[string]any) { f["mode"], f["cloudflareToken"] = "recover", "" },
			"mode recover needs cloudflareToken"},
		{"a Cloudflare token with a line break", func(f map[string]any) { f["cloudflareToken"] = cfToken + "\nx" },
			"cloudflareToken holds white space"},
		{"an install id with mode install", func(f map[string]any) { f["installId"] = installID },
			"installId goes with mode repair or recover"},
		{"an install id that is none", func(f map[string]any) { f["mode"], f["installId"] = "recover", "ABC" },
			`installId "ABC": want 12 lower-case hex characters`},
		{"a field of another version", func(f map[string]any) { f["apiCA"] = "x" }, `unknown field "apiCA"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fields := bootstrapFields("install")
			tt.change(fields)
			path := writeJSON(t, filepath.Join(t.TempDir(), "bootstrap.json"), fields, 0o600)

			_, err := readAsOwner(path)

			require.ErrorContains(t, err, tt.want)
			require.ErrorContains(t, err, path)
			requireNoSecret(t, err.Error())
			require.FileExists(t, path, "a bootstrap that is refused is left for the admin to look at")
		})
	}
}

func TestABootstrapFileThatIsNotRootsAloneIsRefused(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, perm os.FileMode) string {
		return writeJSON(t, filepath.Join(dir, name), bootstrapFields("install"), perm)
	}

	_, err := readAsOwner(write("loose.json", 0o644))
	require.ErrorContains(t, err, "has mode 0644: want 0600")

	_, err = appliance.ReadBootstrapOwnedBy(write("other.json", 0o600), os.Getuid()+1)
	require.ErrorContains(t, err, fmt.Sprintf("is owned by uid %d: want uid %d", os.Getuid(), os.Getuid()+1))

	_, err = appliance.ReadBootstrap(write("mine.json", 0o600))
	if os.Getuid() != 0 {
		require.ErrorContains(t, err, "want uid 0", "the bootstrap is root's")
	}

	link := filepath.Join(dir, "link.json")
	require.NoError(t, os.Symlink(write("target.json", 0o600), link))
	_, err = readAsOwner(link)
	require.ErrorContains(t, err, link, "a link is not followed")

	fields := bootstrapFields("install")
	fields["manifest"] = map[string]any{"padding": strings.Repeat("x", 64<<10)}
	_, err = readAsOwner(writeJSON(t, filepath.Join(dir, "large.json"), fields, 0o600))
	require.ErrorContains(t, err, "larger than 64 KiB")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "two.json"), []byte(`{"mode":"install"} {"mode":"repair"}`), 0o600))
	_, err = readAsOwner(filepath.Join(dir, "two.json"))
	require.ErrorContains(t, err, "more than one JSON object")

	_, err = readAsOwner(filepath.Join(dir, "missing.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// units is the systemd of the container: it notes what it was asked, and
// restarts with the error restart points to, if any.
type units struct {
	log     *[]string
	restart *error
}

func (u units) EnableNow(_ context.Context, unit string) error {
	*u.log = append(*u.log, "enable --now "+unit)
	return nil
}

func (u units) DisableNow(_ context.Context, unit string) error {
	*u.log = append(*u.log, "disable --now "+unit)
	return nil
}

func (u units) Restart(_ context.Context, unit string) error {
	*u.log = append(*u.log, "restart "+unit)
	if u.restart != nil {
		return *u.restart
	}
	return nil
}

func (units) IsActive(context.Context, string) (bool, error)      { return false, nil }
func (units) ListUnits(context.Context, string) ([]string, error) { return nil, nil }

// initEnv is the state volume of lxc/9250, mounted at local with its marker,
// with an empty store, a fake Cloudflare that knows cfToken and the facts of
// the container the bootstrap was made for.
type initEnv struct {
	t          *testing.T
	local      string
	path       string // of the bootstrap
	st         *store.Store
	cf         *cffake.Fake
	clients    map[string]*cffake.Fake // by token
	links      []appliance.NamedLink
	source     appliance.Source
	tokenErr   error
	restartErr error
	lines      []string // what init said
	order      []string // what init asked of the world besides the store

	// beforeInit is set by a test that reads the links before Init does, as the
	// command does, while the bootstrap is still there.
	beforeInit bool
}

func newInitEnv(t *testing.T) *initEnv {
	t.Helper()
	local := filepath.Join(t.TempDir(), "pco")
	require.NoError(t, os.Mkdir(local, 0o755))
	marker := filepath.Join(local, store.VolumeMarker)
	require.NoError(t, os.WriteFile(marker, nil, 0o600))
	st, err := store.Open(store.Paths{
		Cluster: filepath.Join(local, "cluster"), Private: filepath.Join(local, "private"), Local: local,
		MountCheck: marker, Durable: true,
	})
	require.NoError(t, err)
	cf := cffake.New()
	cf.SetNow(func() time.Time { return t0 })
	cf.AddAccount(account, "Main")
	cf.AddZone("zone1", "example.com", account)
	return &initEnv{
		t: t, local: local, path: filepath.Join(local, "bootstrap.json"), st: st, cf: cf,
		clients: map[string]*cffake.Fake{cfToken: cf},
		links:   []appliance.NamedLink{{Name: "eth0", MAC: mac0}, {Name: "eth1", MAC: "bc:24:11:00:00:11"}},
		source:  appliance.Source{Raw: "pcotest/subvol-9250-disk-1 /", VMID: vmid, Volume: "pcotest/subvol-9250-disk-1"},
	}
}

func (e *initEnv) newClient(token string) (cfapi.API, error) {
	if f, ok := e.clients[token]; ok {
		return f, nil
	}
	return nil, errors.New("unknown token")
}

func (e *initEnv) deps() appliance.InitDeps {
	return appliance.InitDeps{
		CheckToken: func(_ context.Context, ep store.Endpoint, caFile string, tok store.PVEToken) error {
			e.order = append(e.order, fmt.Sprintf("check %s at %s as %s with %s", tok.TokenID, ep.Address, ep.ServerName, caFile))
			return e.tokenErr
		},
		NewClient: e.newClient,
		Systemd:   units{log: &e.order, restart: &e.restartErr},
		Now:       func() time.Time { return t0 },
		Rand:      mathrand.NewChaCha8([32]byte{1}),
		Incarnation: func() (string, error) {
			return incarnation, nil
		},
		Links: func() ([]appliance.NamedLink, error) {
			if !e.beforeInit {
				require.NoFileExists(e.t, e.path, "the bootstrap is gone before anything is looked at or written")
			}
			return e.links, nil
		},
		MountSource: func(path string) (appliance.Source, error) {
			require.Equal(e.t, e.local, path)
			return e.source, nil
		},
		RecoverInstall: setup.RecoverInstall,
		Info:           func(format string, args ...any) { e.lines = append(e.lines, fmt.Sprintf(format, args...)) },
	}
}

// bootstrap pushes a bootstrap of mode, changed by change, and reads it.
func (e *initEnv) bootstrap(mode string, change ...func(map[string]any)) appliance.Bootstrap {
	e.t.Helper()
	fields := bootstrapFields(mode)
	for _, c := range change {
		c(fields)
	}
	b, err := readAsOwner(writeJSON(e.t, e.path, fields, 0o600))
	require.NoError(e.t, err)
	return b
}

func (e *initEnv) init(b appliance.Bootstrap) error {
	err := appliance.Init(context.Background(), e.st, b, e.deps())
	if err != nil {
		requireNoSecret(e.t, err.Error())
	}
	requireNoSecret(e.t, strings.Join(e.lines, "\n"))
	return err
}

func (e *initEnv) install() store.Install {
	e.t.Helper()
	inst, found, err := e.st.Install()
	require.NoError(e.t, err)
	require.True(e.t, found)
	return inst
}

func (e *initEnv) writer() planner.Writer {
	e.t.Helper()
	w, found, err := e.st.Writer()
	require.NoError(e.t, err)
	require.True(e.t, found)
	return w
}

func (e *initEnv) credentials() []store.Credential {
	e.t.Helper()
	creds, err := e.st.Credentials()
	require.NoError(e.t, err)
	return creds
}

func (e *initEnv) said(want string) {
	e.t.Helper()
	require.Contains(e.t, strings.Join(e.lines, "\n"), want)
}

func (e *initEnv) block(macs ...string) *store.ApplianceInstall {
	return &store.ApplianceInstall{
		VMID: vmid, Node: "pve1", MACs: macs,
		Endpoints: []store.Endpoint{{Address: "192.168.4.208:8006", ServerName: "pve1"}},
		CAFile:    filepath.Join(e.local, "pve-ca.pem"),
	}
}

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, len(s))
	for i, a := range s {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

// sentinel is the configuration of a tunnel the writer of an install wrote at
// a generation.
func sentinel(install string, generation int) []planner.IngressRule {
	return []planner.IngressRule{
		{Hostname: "www.example.com", Service: "http://10.0.0.5:80"},
		planner.SentinelRule(planner.Writer{InstallID: install, Generation: generation, Nonce: "abcd1234"}),
		planner.CatchAllRule(),
	}
}

func TestInitInModeInstallWritesEveryObject(t *testing.T) {
	e := newInitEnv(t)

	require.NoError(t, e.init(e.bootstrap("install")))

	require.NoFileExists(t, e.path)
	settings, err := e.st.Settings()
	require.NoError(t, err)
	require.Equal(t, string(resolve.LevelObserved), settings.IdentityMinimum)
	require.True(t, settings.ObserveOnly)
	require.Equal(t, "cf-tunnel", settings.GateTag)

	inst := e.install()
	require.Regexp(t, `^[0-9a-f]{12}$`, inst.ID)
	require.Equal(t, store.Install{ID: inst.ID, CreatedAt: t0, Profile: store.ProfileAppliance, Appliance: e.block(mac0)}, inst)
	w := e.writer()
	require.NoError(t, w.Validate())
	require.Equal(t, planner.Writer{InstallID: inst.ID, Generation: 1, Nonce: w.Nonce, Incarnation: incarnation}, w)

	tok, found, err := e.st.PVEToken()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, tokenID, tok.TokenID)
	require.Equal(t, pveSecret, tok.Secret.Reveal())
	nodes, err := e.st.Nodes()
	require.NoError(t, err)
	require.Equal(t, []store.NodeEntry{{Name: "pve1", Version: version.Version, Since: t0}}, nodes)
	saved, err := e.st.NodeAddrs()
	require.NoError(t, err)
	require.Equal(t, addrs("10.92.0.100", "192.168.4.208"), saved)

	manifest := filepath.Join(e.local, "manifest.json")
	b, err := os.ReadFile(manifest)
	require.NoError(t, err)
	require.JSONEq(t, `{"vmid":9250,"pool":"pco"}`, string(b))
	info, err := os.Stat(manifest)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	creds := e.credentials()
	require.Len(t, creds, 1)
	require.Equal(t, "setup", creds[0].Label)
	require.Equal(t, "scoped", creds[0].Kind)
	require.Equal(t, cfToken, creds[0].Token.Reveal())
	require.Empty(t, e.cf.TunnelsIn(account), "the probe tunnel of the deep check is gone")

	require.Equal(t, []string{
		"check pco@pve!vm9250 at 192.168.4.208:8006 as pve1 with " + filepath.Join(e.local, "pve-ca.pem"),
		"restart pco.service",
	}, e.order, "the daemon is restarted last")
	e.said("identity: " + e.local + " is a volume of lxc/9250 (pcotest/subvol-9250-disk-1)")
}

func TestInitWithoutACloudflareTokenStoresNoCredential(t *testing.T) {
	e := newInitEnv(t)

	require.NoError(t, e.init(e.bootstrap("install", func(f map[string]any) { f["cloudflareToken"] = "" })))

	require.Empty(t, e.credentials())
	e.said("pco credential add")
	require.Equal(t, "restart pco.service", e.order[len(e.order)-1])
}

func TestABootstrapForAnotherContainerDoesNothing(t *testing.T) {
	for _, tt := range []struct {
		name   string
		links  []appliance.NamedLink
		source appliance.Source
		want   string
	}{
		{name: "a MAC no link has",
			links:  []appliance.NamedLink{{Name: "eth0", MAC: "bc:24:11:00:00:99"}},
			source: appliance.Source{Raw: "pcotest/subvol-9250-disk-1 /", VMID: vmid},
			want:   "init step identity: the bootstrap names MAC bc:24:11:00:00:10, which no link of this container has (it has bc:24:11:00:00:99): it was pushed into another container"},
		{name: "a volume of another VMID",
			links:  []appliance.NamedLink{{Name: "eth0", MAC: mac0}},
			source: appliance.Source{Raw: "pcotest/subvol-9295-disk-1 /", VMID: 9295, Volume: "pcotest/subvol-9295-disk-1"},
			want:   "is a volume of lxc/9295 (pcotest/subvol-9295-disk-1), but the bootstrap is for lxc/9250: it was pushed into another container"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newInitEnv(t)
			e.links, e.source = tt.links, tt.source

			err := e.init(e.bootstrap("install"))

			require.ErrorContains(t, err, tt.want)
			require.NoFileExists(t, e.path, "the secrets do not stay in the wrong container")
			require.NoDirExists(t, filepath.Join(e.local, "cluster"), "nothing is written")
			require.NoDirExists(t, filepath.Join(e.local, "private"))
			require.Empty(t, e.order)
		})
	}
}

// daemonOf is the daemon of the container with a lock of the node and a log of
// what systemctl was asked. While running it holds the lock, until the unit is
// stopped or stop is called, which is what a stop by hand does.
func daemonOf(t *testing.T, running bool) (d appliance.Daemon, ran *[]string, stop func()) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "daemon.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	stop = func() { require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_UN)) }
	if running {
		require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	}
	ran = new([]string)
	return appliance.Daemon{Lock: f.Name(), Systemctl: func(_ context.Context, args ...string) (string, error) {
		*ran = append(*ran, strings.Join(args, " "))
		switch args[0] {
		case "is-active":
			if running {
				return "active\n", nil
			}
			return "inactive\n", errors.New("exit status 3")
		case "stop":
			stop()
		}
		return "", nil
	}}, ran, stop
}

func TestABootstrapForAnotherContainerStopsNoDaemon(t *testing.T) {
	for _, mode := range []string{appliance.ModeInstall, appliance.ModeRepair, appliance.ModeRecover} {
		t.Run(mode, func(t *testing.T) {
			e := newInitEnv(t)
			e.beforeInit = true
			e.links = []appliance.NamedLink{{Name: "eth0", MAC: "bc:24:11:00:00:99"}}
			d, ran, _ := daemonOf(t, true)

			stopped, err := d.ReadyForInit(context.Background(), e.bootstrap(mode), e.local, e.deps())

			require.ErrorContains(t, err, "it was pushed into another container")
			require.False(t, stopped)
			require.Empty(t, *ran, "the daemon of the other container is neither stopped nor started")
			require.NoFileExists(t, e.path, "the secrets do not stay in the wrong container")
			require.Empty(t, e.lines, "Init says what it found, once")
		})
	}
}

func TestABootstrapStaysWhileTheDaemonRunsInModeInstall(t *testing.T) {
	e := newInitEnv(t)
	e.beforeInit = true
	d, ran, stop := daemonOf(t, true)
	b := e.bootstrap("install")

	stopped, err := d.ReadyForInit(context.Background(), b, e.local, e.deps())

	require.ErrorContains(t, err, "a pco daemon runs (it holds "+d.Lock+"), and an init in mode install starts it itself: stop pco.service first")
	require.ErrorContains(t, err, "and run init again: the bootstrap is kept")
	require.False(t, stopped)
	require.Empty(t, *ran, "nothing is stopped")
	require.FileExists(t, e.path, "the retry the message asks for has its bootstrap")

	stop()
	stopped, err = d.ReadyForInit(context.Background(), b, e.local, e.deps())
	require.NoError(t, err)
	require.False(t, stopped)
	e.beforeInit = false
	require.NoError(t, e.init(b))
	require.NoFileExists(t, e.path)
	require.Equal(t, vmid, e.install().Appliance.VMID)
}

func TestRepairAndRecoverStopTheDaemonBeforeInit(t *testing.T) {
	for _, mode := range []string{appliance.ModeRepair, appliance.ModeRecover} {
		t.Run(mode, func(t *testing.T) {
			e := newInitEnv(t)
			e.beforeInit = true
			d, ran, _ := daemonOf(t, true)

			stopped, err := d.ReadyForInit(context.Background(), e.bootstrap(mode), e.local, e.deps())

			require.NoError(t, err)
			require.True(t, stopped)
			require.Equal(t, []string{"is-active pco.service", "stop pco.service"}, *ran)
			require.FileExists(t, e.path, "Init removes it")
		})
	}

	t.Run("a daemon that is not stopped", func(t *testing.T) {
		e := newInitEnv(t)
		e.beforeInit = true
		d, _, _ := daemonOf(t, true)
		d.Systemctl = func(_ context.Context, args ...string) (string, error) {
			if args[0] == "is-active" {
				return "active\n", nil
			}
			return "", errors.New("exit status 1")
		}

		stopped, err := d.ReadyForInit(context.Background(), e.bootstrap("repair"), e.local, e.deps())

		require.ErrorContains(t, err, "stopping pco.service")
		require.True(t, stopped, "it was running, and the caller starts it again")
		require.FileExists(t, e.path)
	})
}

func TestNoDaemonAtAllIsReadyInEveryMode(t *testing.T) {
	for _, mode := range []string{appliance.ModeInstall, appliance.ModeRepair, appliance.ModeRecover} {
		e := newInitEnv(t)
		e.beforeInit = true
		d, _, _ := daemonOf(t, false)

		stopped, err := d.ReadyForInit(context.Background(), e.bootstrap(mode), e.local, e.deps())

		require.NoError(t, err, mode)
		require.False(t, stopped, mode)
	}
}

func TestAVolumeOfNoKnownFormIsTakenWithANote(t *testing.T) {
	e := newInitEnv(t)
	e.source = appliance.Source{Raw: "/dev/sdz1 /"}

	require.NoError(t, e.init(e.bootstrap("install")))

	e.said("identity: the source of " + e.local + " (/dev/sdz1 /) is of no form pco knows; it is taken for lxc/9250, " +
		"which the installer pushed the bootstrap into")
	require.Equal(t, vmid, e.install().Appliance.VMID)
}

func TestModeInstallNeverReplacesAnInstall(t *testing.T) {
	e := newInitEnv(t)
	require.NoError(t, e.init(e.bootstrap("install")))
	inst, w := e.install(), e.writer()
	e.order = nil

	err := e.init(e.bootstrap("install", func(f map[string]any) {
		f["pveToken"] = map[string]string{"tokenId": tokenID, "secret": "another-secret"}
	}))

	require.EqualError(t, err, "init step store: the volume holds install "+inst.ID+" already, and mode install never "+
		"replaces one: pco appliance repair --vmid 9250 on the node keeps it")
	require.NoFileExists(t, e.path)
	require.Equal(t, inst, e.install())
	require.Equal(t, w, e.writer())
	tok, _, err := e.st.PVEToken()
	require.NoError(t, err)
	require.Equal(t, pveSecret, tok.Secret.Reveal())
	require.Empty(t, e.order)
}

// pending is the file that says an install is begun and not finished.
const pending = "init.pending"

func TestAnInstallThatStoppedPastItsInstallStepIsCompletedByTheNextInit(t *testing.T) {
	for _, tt := range []struct {
		name  string
		fail  func(*initEnv)
		fixed func(*initEnv)
		step  string
	}{
		{"the check of Proxmox",
			func(e *initEnv) { e.tokenErr = errors.New("no route to host") },
			func(e *initEnv) { e.tokenErr = nil }, "init step proxmox"},
		{"the restart",
			func(e *initEnv) { e.restartErr = errors.New("unit failed") },
			func(e *initEnv) { e.restartErr = nil }, "init step service"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newInitEnv(t)
			tt.fail(e)
			require.ErrorContains(t, e.init(e.bootstrap("install")), tt.step)
			inst, w := e.install(), e.writer()
			require.FileExists(t, filepath.Join(e.local, pending))
			tt.fixed(e)
			e.order, e.lines = nil, nil

			require.NoError(t, e.init(e.bootstrap("install", func(f map[string]any) {
				f["pveToken"] = map[string]string{"tokenId": tokenID, "secret": "pve-secret-2"}
			})))

			require.Equal(t, inst, e.install(), "the install the first init began, not a second one")
			require.Equal(t, w, e.writer())
			tok, _, err := e.st.PVEToken()
			require.NoError(t, err)
			require.Equal(t, "pve-secret-2", tok.Secret.Reveal(), "what the bootstrap carries is stored")
			require.Len(t, e.credentials(), 1)
			require.Equal(t, "restart pco.service", e.order[len(e.order)-1])
			e.said("install " + inst.ID + " was begun by an init that did not finish, and this one completes it")
			require.NoFileExists(t, filepath.Join(e.local, pending), "a finished install is marked no more")

			err = e.init(e.bootstrap("install"))
			require.ErrorContains(t, err, "the volume holds install "+inst.ID+" already, and mode install never replaces one")
			require.Equal(t, inst, e.install())
		})
	}
}

func TestAnInstallThatStoppedBeforeItsInstallStepIsBegunAnew(t *testing.T) {
	e := newInitEnv(t)
	require.NoError(t, e.st.Init())
	require.NoError(t, e.st.SaveWriter(planner.Writer{InstallID: "ba9876543210", Generation: 1, Nonce: "aaaa1111", Incarnation: incarnation}))
	require.NoError(t, os.WriteFile(filepath.Join(e.local, pending), []byte("ba9876543210\n"), 0o600))

	require.NoError(t, e.init(e.bootstrap("install")))

	require.NotEqual(t, "ba9876543210", e.install().ID, "a writer without an install is nobody's")
	require.Equal(t, e.install().ID, e.writer().InstallID)
	require.NoFileExists(t, filepath.Join(e.local, pending))
}

func TestAnotherInstallsMarkDoesNotResumeAFinishedInstall(t *testing.T) {
	e := newInitEnv(t)
	require.NoError(t, e.init(e.bootstrap("install")))
	inst := e.install()
	require.NoError(t, os.WriteFile(filepath.Join(e.local, pending), []byte("ba9876543210\n"), 0o600))

	err := e.init(e.bootstrap("install"))

	require.ErrorContains(t, err, "mode install never replaces one")
	require.Equal(t, inst, e.install())
}

func TestARepairCompletesAnInstallThatDidNotFinish(t *testing.T) {
	e := newInitEnv(t)
	e.tokenErr = errors.New("no route to host")
	require.Error(t, e.init(e.bootstrap("install")))
	inst := e.install()
	e.tokenErr = nil

	require.NoError(t, e.init(e.bootstrap("repair", func(f map[string]any) { f["installId"] = inst.ID })))

	require.NoFileExists(t, filepath.Join(e.local, pending))
	require.Len(t, e.credentials(), 1)
	require.ErrorContains(t, e.init(e.bootstrap("install")), "mode install never replaces one")
}

func TestRepairKeepsTheInstallAndTheWriter(t *testing.T) {
	e := newInitEnv(t)
	require.NoError(t, e.init(e.bootstrap("install")))
	inst, w := e.install(), e.writer()
	settings, err := e.st.Settings()
	require.NoError(t, err)
	settings.ObserveOnly = false
	require.NoError(t, e.st.SaveSettings(settings))
	saved, err := e.st.NodeAddrs()
	require.NoError(t, err)
	require.NoError(t, e.st.SaveNodeAddrs(append(saved, netip.MustParseAddr("192.0.2.1"))))
	e.order, e.lines = nil, nil
	// The NIC was replaced, the API is reached on another bridge and the
	// token was made anew.
	const newMAC = "bc:24:11:00:00:20"
	e.links = []appliance.NamedLink{{Name: "eth0", MAC: newMAC}}

	require.NoError(t, e.init(e.bootstrap("repair", func(f map[string]any) {
		f["installId"] = inst.ID
		f["macs"] = []string{newMAC}
		f["endpoints"] = []map[string]string{{"address": "10.92.0.2:8006", "serverName": "pve1"}}
		f["nodeAddrs"] = []string{"10.92.0.2"}
		f["pveToken"] = map[string]string{"tokenId": tokenID, "secret": "pve-secret-2"}
		f["cloudflareToken"] = ""
	})))

	block := e.block(newMAC)
	block.Endpoints = []store.Endpoint{{Address: "10.92.0.2:8006", ServerName: "pve1"}}
	require.Equal(t, store.Install{ID: inst.ID, CreatedAt: inst.CreatedAt, Profile: store.ProfileAppliance, Appliance: block}, e.install())
	require.Equal(t, w, e.writer(), "the writer is kept as it is")
	tok, _, err := e.st.PVEToken()
	require.NoError(t, err)
	require.Equal(t, "pve-secret-2", tok.Secret.Reveal())
	got, err := e.st.Settings()
	require.NoError(t, err)
	require.False(t, got.ObserveOnly, "the settings of the install are kept")
	saved, err = e.st.NodeAddrs()
	require.NoError(t, err)
	require.Equal(t, addrs("10.92.0.2", "10.92.0.100", "192.0.2.1", "192.168.4.208"), saved, "node addresses are never forgotten")
	require.Len(t, e.credentials(), 1)
	require.Equal(t, []string{
		"check pco@pve!vm9250 at 10.92.0.2:8006 as pve1 with " + filepath.Join(e.local, "pve-ca.pem"),
		"restart pco.service",
	}, e.order)
}

func TestRepairSaysWhenTheBootstrapNamesAnotherGateTag(t *testing.T) {
	e := newInitEnv(t)
	require.NoError(t, e.init(e.bootstrap("install")))
	inst := e.install()
	settings, err := e.st.Settings()
	require.NoError(t, err)
	settings.GateTag = "edge"
	require.NoError(t, e.st.SaveSettings(settings))
	e.lines = nil

	require.NoError(t, e.init(e.bootstrap("repair", func(f map[string]any) { f["installId"] = inst.ID })))

	got, err := e.st.Settings()
	require.NoError(t, err)
	require.Equal(t, "edge", got.GateTag, "the settings of the install are kept")
	e.said("settings: the gate tag stays edge, and the bootstrap names cf-tunnel: to change it, set gateTag in the file " +
		"of pco settings show --json and apply it with pco settings apply")

	e.lines = nil
	require.NoError(t, e.init(e.bootstrap("repair", func(f map[string]any) {
		f["installId"], f["gateTag"] = inst.ID, "edge"
	})))
	require.NotContains(t, strings.Join(e.lines, "\n"), "the gate tag stays", "nothing to say when they agree")
}

func TestRepairGoesOnWhenTheGateTagOfTheSettingsCannotBeRead(t *testing.T) {
	e := newInitEnv(t)
	require.NoError(t, e.init(e.bootstrap("install")))
	require.NoError(t, os.WriteFile(filepath.Join(e.local, "cluster", "meta", "settings.json"), []byte("{"), 0o600))
	e.lines = nil

	require.NoError(t, e.init(e.bootstrap("repair", func(f map[string]any) { f["installId"] = e.install().ID })))

	e.said("settings: the gate tag could not be compared with the bootstrap's")
}

func TestRepairNeedsTheInstallItRepairs(t *testing.T) {
	e := newInitEnv(t)

	err := e.init(e.bootstrap("repair"))
	require.EqualError(t, err, "init step store: the volume holds no install to repair: "+
		"pco appliance repair --vmid 9250 --recover on the node adopts one")

	require.NoError(t, e.init(e.bootstrap("install")))
	err = e.init(e.bootstrap("repair", func(f map[string]any) { f["installId"] = "ba9876543210" }))
	require.ErrorContains(t, err, "init step store: the volume holds install "+e.install().ID+", not ba9876543210")
}

func TestRecoverAdoptsTheInstallCloudflareSees(t *testing.T) {
	e := newInitEnv(t)
	e.cf.SeedTunnel(account, "pco-"+installID, sentinel(installID, 5))

	require.NoError(t, e.init(e.bootstrap("recover")))

	require.Equal(t, store.Install{ID: installID, CreatedAt: t0, Profile: store.ProfileAppliance, Appliance: e.block(mac0)}, e.install())
	w := e.writer()
	require.Equal(t, planner.Writer{InstallID: installID, Generation: 6, Nonce: w.Nonce, Incarnation: incarnation}, w)
	require.NotEqual(t, "abcd1234", w.Nonce)
	settings, err := e.st.Settings()
	require.NoError(t, err)
	require.True(t, settings.ObserveOnly)
	require.Equal(t, string(resolve.LevelObserved), settings.IdentityMinimum)
	require.Len(t, e.credentials(), 1)
	require.Equal(t, "restart pco.service", e.order[len(e.order)-1])
	e.said("recovered install " + installID + " with writer generation 6")
}

func TestRecoverTakesTheInstallTheBootstrapNames(t *testing.T) {
	const other = "ba9876543210"
	e := newInitEnv(t)
	e.cf.SeedTunnel(account, "pco-"+installID, sentinel(installID, 5))
	e.cf.SeedTunnel(account, "pco-"+other, sentinel(other, 11))

	err := e.init(e.bootstrap("recover"))
	require.ErrorContains(t, err, "init step writer: the token sees the tunnels of 2 installs")

	require.NoError(t, e.init(e.bootstrap("recover", func(f map[string]any) { f["installId"] = other })))
	require.Equal(t, other, e.install().ID)
	require.Equal(t, 12, e.writer().Generation)
}

func TestAFailingTokenCheckStopsBeforeTheCredential(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"refused", &pve.APIError{Status: http.StatusUnauthorized, Message: "authentication failure"},
			"init step proxmox: the token is refused by Proxmox"},
		{"not reached", errors.New("dial tcp 192.168.4.208:8006: connect: no route to host"),
			"init step proxmox: reading the version of Proxmox VE at 192.168.4.208:8006 as pve1: dial tcp 192.168.4.208:8006: connect: no route to host"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newInitEnv(t)
			e.tokenErr = tt.err

			require.EqualError(t, e.init(e.bootstrap("install")), tt.want)

			require.Empty(t, e.credentials())
			require.NotContains(t, e.order, "restart pco.service")
			require.Equal(t, store.ProfileAppliance, e.install().Profile, "what was written stays")
		})
	}
}

// recoverDeps are the parts of pco appliance recover.
func (e *initEnv) recoverDeps() appliance.RecoverDeps {
	return appliance.RecoverDeps{
		NewClient:      func(c store.Credential) (cfapi.API, error) { return e.newClient(c.Token.Reveal()) },
		RecoverInstall: setup.RecoverInstall,
		Incarnation:    func() (string, error) { return incarnation, nil },
		Now:            func() time.Time { return t0 },
		Rand:           mathrand.NewChaCha8([32]byte{4}),
	}
}

// rolledBack is an appliance whose volume went back to generation 3 of an
// earlier start, while Cloudflare holds a sentinel of generation 7.
func rolledBack(t *testing.T) (*initEnv, store.Install) {
	t.Helper()
	e := newInitEnv(t)
	require.NoError(t, e.init(e.bootstrap("install")))
	inst := e.install()
	require.NoError(t, e.st.SaveWriter(planner.Writer{InstallID: inst.ID, Generation: 3, Nonce: "old3", Incarnation: "earlier/1"}))
	settings, err := e.st.Settings()
	require.NoError(t, err)
	settings.ObserveOnly = false
	require.NoError(t, e.st.SaveSettings(settings))
	e.cf.SeedTunnel(account, "pco-"+inst.ID, sentinel(inst.ID, 7))
	return e, inst
}

func TestRecoverDrawsAnEpochAboveTheSentinel(t *testing.T) {
	e, inst := rolledBack(t)

	w, err := appliance.Recover(context.Background(), e.st, e.recoverDeps())

	require.NoError(t, err)
	require.Equal(t, planner.Writer{InstallID: inst.ID, Generation: 8, Nonce: w.Nonce, Incarnation: incarnation}, w)
	require.NotEqual(t, "old3", w.Nonce)
	require.Equal(t, w, e.writer(), "written before it is returned")
	require.True(t, e.st.Paths().Durable, "on a store that writes durably")
	settings, err := e.st.Settings()
	require.NoError(t, err)
	require.True(t, settings.ObserveOnly)
	require.Equal(t, inst, e.install(), "the install stays as it is")
}

func TestRecoverSeesTheTunnelsOfEveryCredential(t *testing.T) {
	e, inst := rolledBack(t)
	other := cffake.New()
	other.SetNow(func() time.Time { return t0 })
	other.AddAccount("acc2", "Second")
	other.SeedTunnel("acc2", "pco-"+inst.ID, sentinel(inst.ID, 9))
	e.clients["cf-token-2"] = other
	require.NoError(t, e.st.SaveCredential(store.Credential{
		ID: "c2c2c2c2", Label: "second", Kind: "scoped", Token: store.NewSecret("cf-token-2"), AddedAt: t0,
	}))

	w, err := appliance.Recover(context.Background(), e.st, e.recoverDeps())

	require.NoError(t, err)
	require.Equal(t, 10, w.Generation, "above the sentinel in the account only the second credential sees")
}

func TestRecoverNeedsACredential(t *testing.T) {
	e := newInitEnv(t)
	require.NoError(t, e.init(e.bootstrap("install", func(f map[string]any) { f["cloudflareToken"] = "" })))
	w := e.writer()

	_, err := appliance.Recover(context.Background(), e.st, e.recoverDeps())

	require.ErrorIs(t, err, appliance.ErrNoCredential)
	require.ErrorContains(t, err, "pco credential add")
	require.Equal(t, w, e.writer(), "nothing is written")
}

func TestRecoverRefusesWhatItCannotRecover(t *testing.T) {
	t.Run("no install", func(t *testing.T) {
		e := newInitEnv(t)
		require.NoError(t, e.st.Init())

		_, err := appliance.Recover(context.Background(), e.st, e.recoverDeps())

		require.EqualError(t, err, "the volume holds no install of an appliance: "+
			"pco appliance repair --vmid <vmid> --recover on the node adopts one")
	})
	t.Run("a credential that cannot list", func(t *testing.T) {
		e, _ := rolledBack(t)
		e.cf.FailNext("accounts", 1, errors.New("internal server error"))

		_, err := appliance.Recover(context.Background(), e.st, e.recoverDeps())

		require.ErrorContains(t, err, "internal server error")
		require.ErrorContains(t, err, "recovery needs every stored credential to answer, as one that does not could hide "+
			"a higher generation: try again, or remove it with pco credential remove "+e.credentials()[0].ID+" if its token is gone")
		require.Equal(t, 3, e.writer().Generation, "no generation is guessed")
	})
	t.Run("a credential that cannot be used", func(t *testing.T) {
		e, _ := rolledBack(t)
		deps := e.recoverDeps()
		deps.NewClient = func(store.Credential) (cfapi.API, error) { return nil, errors.New("unknown token") }

		_, err := appliance.Recover(context.Background(), e.st, deps)

		require.ErrorContains(t, err, "credential "+e.credentials()[0].ID+" cannot be used: unknown token")
		require.ErrorContains(t, err, "pco credential remove "+e.credentials()[0].ID)
	})
	t.Run("an install the credentials do not see", func(t *testing.T) {
		e := newInitEnv(t)
		require.NoError(t, e.init(e.bootstrap("install")))
		inst := e.install()

		_, err := appliance.Recover(context.Background(), e.st, e.recoverDeps())

		require.EqualError(t, err, "the stored credentials see no tunnel of install "+inst.ID+", the install of this volume, "+
			"so the generation its writer used is unknown: add a credential that sees the account of its tunnel "+
			"(pco credential add), then run pco appliance recover again")
		require.NotContains(t, err.Error(), "check the id", "the id came from the volume")
	})
	t.Run("a store that does not write durably", func(t *testing.T) {
		e, _ := rolledBack(t)
		p := e.st.Paths()
		p.Durable = false
		st, err := store.Open(p)
		require.NoError(t, err)

		_, err = appliance.Recover(context.Background(), st, e.recoverDeps())

		require.EqualError(t, err, "the store of the appliance must write durably, and this one does not")
	})
}

func TestTheDaemonIsStoppedAndStartedAroundTheStore(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "daemon.lock")
	f, err := os.OpenFile(lock, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	byHand := false
	var ran []string
	d := appliance.Daemon{Lock: lock, Systemctl: func(_ context.Context, args ...string) (string, error) {
		ran = append(ran, strings.Join(args, " "))
		switch args[0] {
		case "is-active":
			return "active\n", nil
		case "stop":
			if !byHand {
				require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_UN))
			}
		}
		return "", nil
	}}

	running, err := d.Running()
	require.NoError(t, err)
	require.True(t, running)

	was, err := d.Stop(context.Background())
	require.NoError(t, err)
	require.True(t, was)
	running, err = d.Running()
	require.NoError(t, err)
	require.False(t, running)
	require.NoError(t, d.Start(context.Background()))
	require.Equal(t, []string{"is-active pco.service", "stop pco.service", "start pco.service"}, ran)

	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	byHand = true
	_, err = d.Stop(context.Background())
	require.EqualError(t, err, "a pco daemon runs outside systemd (it holds "+lock+"): stop it first")

	missing := appliance.Daemon{Lock: filepath.Join(t.TempDir(), "daemon.lock")}
	running, err = missing.Running()
	require.NoError(t, err)
	require.False(t, running, "no lock file is no daemon")
	require.NoFileExists(t, missing.Lock, "and none is made")
}

func TestADaemonThatIsNotActiveIsNotStartedAgainByMistake(t *testing.T) {
	var ran []string
	d := appliance.Daemon{Lock: filepath.Join(t.TempDir(), "daemon.lock"), Systemctl: func(_ context.Context, args ...string) (string, error) {
		ran = append(ran, strings.Join(args, " "))
		if args[0] == "is-active" {
			return "failed\n", errors.New("exit status 3")
		}
		return "", nil
	}}

	was, err := d.Stop(context.Background())

	require.NoError(t, err)
	require.False(t, was)
	require.Equal(t, []string{"is-active pco.service", "stop pco.service"}, ran)
	require.False(t, slices.Contains(ran, "start pco.service"))
}
