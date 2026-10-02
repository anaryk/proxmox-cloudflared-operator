package daemon

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

const (
	testNode     = "pve1"
	testInstall  = "abc123"
	testCred     = "cred1"
	pveTokenID   = "pco@pve!pco"
	pveSecret    = "pve-secret-0123456789-do-not-log"
	cfToken      = "cf-api-token-0123456789-do-not-log"
	guestMAC     = "bc:24:11:00:aa:b5"
	guestAddress = "10.20.0.15"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// fakePVE is a Proxmox API with one node and one guest: web-1, qemu/101,
// tagged for publishing, with the route www.example.com -> :8080.
type fakePVE struct {
	srv *httptest.Server

	mu   sync.Mutex
	auth []string // the Authorization header of every request
}

func newFakePVE(t *testing.T) *fakePVE {
	t.Helper()
	f := &fakePVE{}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// caFile writes the certificate of the server to dir and returns its path.
func (f *fakePVE) caFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "pve-ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	require.NoError(t, os.WriteFile(path, pemBytes, 0o600))
	return path
}

func (f *fakePVE) authorizations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.auth)
}

func (f *fakePVE) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "PVEAPIToken="+pveTokenID+"="+pveSecret {
		http.Error(w, `{"message":"authentication failure"}`, http.StatusUnauthorized)
		return
	}
	var data any
	switch r.URL.Path {
	case "/api2/json/version":
		data = map[string]any{"release": "9.0", "version": "9.0.3", "repoid": "ad1f0e1a"}
	case "/api2/json/cluster/resources":
		data = []map[string]any{{
			"type": "qemu", "vmid": 101, "name": "web-1", "node": testNode, "status": "running",
			"template": 0, "tags": "cf-tunnel",
		}}
	case "/api2/json/cluster/status":
		data = []map[string]any{
			{"type": "cluster", "name": "lab", "nodes": 1, "quorate": 1},
			{"type": "node", "name": testNode, "ip": "10.20.0.2", "online": 1, "local": 1},
		}
	case "/api2/json/nodes/pve1/network":
		data = []map[string]any{
			{"iface": "vmbr0", "type": "bridge", "active": 1, "cidr": "10.20.0.2/24", "bridge_ports": "nic3"},
			{"iface": "nic3", "type": "eth", "active": 1},
		}
	case "/api2/json/nodes/pve1/qemu/101/config":
		data = map[string]any{
			"name":        "web-1",
			"description": "```cf-tunnel\nwww.example.com -> :8080\n```\n",
			"net0":        "virtio=" + guestMAC + ",bridge=vmbr0",
			"ipconfig0":   "ip=" + guestAddress + "/24,gw=10.20.0.1",
			"smbios1":     "uuid=80e8ef19-34bf-4cb3-b314-96870ca14d1e",
			"meta":        "creation-qemu=10.1.2,ctime=1790847599",
			"tags":        "cf-tunnel",
			"digest":      "fb4a8e8f8e7b16bb727fc538bd3b9c5244e2c57e",
		}
	case "/api2/json/nodes/pve1/qemu/101/agent/network-get-interfaces":
		http.Error(w, `{"message":"QEMU guest agent is not running"}`, http.StatusInternalServerError)
		return
	default:
		http.Error(w, `{"message":"no such endpoint"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// scriptedProber sees the node of fakePVE: vmbr0 with 10.20.0.2/24, and the
// guest answering on its address from its own port.
type scriptedProber struct{}

func (scriptedProber) Interfaces(context.Context) ([]resolve.HostIface, error) {
	return []resolve.HostIface{{Name: "vmbr0", Addrs: []netip.Prefix{netip.MustParsePrefix("10.20.0.2/24")}}}, nil
}

func (scriptedProber) Route(_ context.Context, addr netip.Addr) (string, bool, error) {
	return "vmbr0", addr.String() == guestAddress, nil
}

func (scriptedProber) ARP(_ context.Context, _ string, addr netip.Addr) ([]string, error) {
	if addr.String() == guestAddress {
		return []string{guestMAC}, nil
	}
	return nil, nil
}

func (scriptedProber) FDBPorts(_ context.Context, _ string, _ int, mac string) ([]string, error) {
	if mac == guestMAC {
		return []string{"tap101i0"}, nil
	}
	return nil, nil
}

func (scriptedProber) Dial(context.Context, netip.AddrPort) error { return nil }

// fakeSystemd keeps the units a connector manager enabled.
type fakeSystemd struct {
	mu      sync.Mutex
	enabled map[string]bool
}

func (f *fakeSystemd) EnableNow(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled[unit] = true
	return nil
}

func (f *fakeSystemd) DisableNow(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.enabled, unit)
	return nil
}

func (f *fakeSystemd) Restart(context.Context, string) error { return nil }

func (f *fakeSystemd) IsActive(_ context.Context, unit string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled[unit], nil
}

func (f *fakeSystemd) ListUnits(_ context.Context, pattern string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for unit := range f.enabled {
		if ok, _ := path.Match(pattern, unit); ok {
			out = append(out, unit)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (f *fakeSystemd) units() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.enabled))
	for unit := range f.enabled {
		out = append(out, unit)
	}
	slices.Sort(out)
	return out
}

// fakeNotifier records what the daemon tells systemd.
type fakeNotifier struct {
	mu      sync.Mutex
	events  []string
	ready   chan struct{} // closed by the first Ready
	onReady func()        // runs first in Ready
}

func newFakeNotifier() *fakeNotifier { return &fakeNotifier{ready: make(chan struct{})} }

func (n *fakeNotifier) Ready() error {
	if n.onReady != nil {
		n.onReady()
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, "READY=1")
	select {
	case <-n.ready:
	default:
		close(n.ready)
	}
	return nil
}

func (n *fakeNotifier) Stopping() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, "STOPPING=1")
	return nil
}

func (n *fakeNotifier) sent() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.events)
}

// fakeAccounts is a system with the users and groups a test gives it.
type fakeAccounts struct {
	user  *user.User
	group *user.Group
}

func (f fakeAccounts) LookupUser(name string) (*user.User, error) {
	if f.user == nil {
		return nil, user.UnknownUserError(name)
	}
	return f.user, nil
}

func (f fakeAccounts) LookupGroup(name string) (*user.Group, error) {
	if f.group == nil {
		return nil, user.UnknownGroupError(name)
	}
	return f.group, nil
}

// webAccounts has pco-web as the user who runs the test.
func webAccounts() fakeAccounts {
	return fakeAccounts{
		user:  &user.User{Uid: strconv.Itoa(os.Getuid()), Gid: strconv.Itoa(os.Getgid()), Username: webName},
		group: &user.Group{Gid: strconv.Itoa(os.Getgid()), Name: webName},
	}
}

// world is a node: a store that is set up, a Proxmox, a Cloudflare and what
// the daemon needs to run against them.
type world struct {
	t      *testing.T
	dir    string
	mount  string // the file whose existence is the cluster filesystem
	paths  store.Paths
	store  *store.Store
	pve    *fakePVE
	cf     *cffake.Fake
	sysd   *fakeSystemd
	notify *fakeNotifier
	logs   *testutil.SyncBuffer
	// cycles counts the reads of the clock, which every cycle starts with: it
	// says whether a cycle has begun.
	cycles atomic.Int64
	cfg    Config
	deps   Deps
}

func newWorld(t *testing.T) *world {
	t.Helper()
	dir := testutil.ShortDir(t)
	w := &world{
		t:      t,
		dir:    dir,
		mount:  filepath.Join(dir, "mounted"),
		pve:    newFakePVE(t),
		cf:     cffake.New(),
		sysd:   &fakeSystemd{enabled: map[string]bool{}},
		notify: newFakeNotifier(),
		logs:   &testutil.SyncBuffer{},
	}
	require.NoError(t, os.WriteFile(w.mount, nil, 0o600))
	w.paths = store.Paths{
		Cluster:    filepath.Join(dir, "cluster"),
		Private:    filepath.Join(dir, "private"),
		Local:      filepath.Join(dir, "local"),
		MountCheck: w.mount,
	}
	s, err := store.Open(w.paths)
	require.NoError(t, err)
	require.NoError(t, s.Init())
	require.NoError(t, s.SaveInstall(store.Install{ID: testInstall, CreatedAt: t0}))
	require.NoError(t, s.SaveNode(store.NodeEntry{Name: testNode, Since: t0}))
	require.NoError(t, s.SaveWriter(planner.Writer{InstallID: testInstall, Generation: 1, Nonce: "n1"}))
	require.NoError(t, s.SavePVEToken(store.PVEToken{TokenID: pveTokenID, Secret: store.NewSecret(pveSecret)}))
	require.NoError(t, s.SaveCredential(store.Credential{ID: testCred, Label: "main", Kind: "scoped", Token: store.NewSecret(cfToken), AddedAt: t0}))
	w.store = s

	w.cf.AddAccount("acc1", "Main")
	w.cf.AddZone("zone1", "example.com", "acc1")

	w.cfg = Config{
		Version:    "1.2.3",
		PVEURL:     w.pve.srv.URL,
		PVECAFile:  w.pve.caFile(t, dir),
		Node:       testNode,
		SocketPath: filepath.Join(dir, "pco", "pco.sock"),
		Paths:      w.paths,
		Log:        zerolog.New(w.logs).Level(zerolog.DebugLevel),
	}
	w.deps = Deps{
		Prober:    scriptedProber{},
		Systemd:   w.sysd,
		NewClient: func(store.Credential) (cfapi.API, error) { return w.cf, nil },
		Notifier:  w.notify,
		Accounts:  webAccounts(),
		Sleep:     func(context.Context, time.Duration) error { return nil },
		Now:       func() time.Time { w.cycles.Add(1); return time.Now() },
	}
	return w
}

// running is a daemon that was started by a test.
type running struct {
	w      *world
	client *apiclient.Client
	cancel context.CancelFunc
	done   chan error

	once   sync.Once
	result error
}

// start runs the daemon and returns once it says that it is ready.
func (w *world) start() *running {
	w.t.Helper()
	ctx, cancel := context.WithCancel(w.t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, w.cfg, w.deps) }()
	select {
	case <-w.notify.ready:
	case err := <-done:
		cancel()
		w.t.Fatalf("the daemon ended before it was ready: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		w.t.Fatal("the daemon was not ready in time")
	}
	r := &running{w: w, client: apiclient.New(w.cfg.SocketPath), cancel: cancel, done: done}
	w.t.Cleanup(func() { _ = r.stop() })
	return r
}

// stop ends the daemon and returns what Run returned.
func (r *running) stop() error {
	r.once.Do(func() {
		r.cancel()
		r.result = <-r.done
	})
	return r.result
}

// stopWithin ends the daemon and returns what Run returned, failing the test
// when it takes longer than limit.
func (r *running) stopWithin(limit time.Duration) error {
	r.w.t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		r.once.Do(func() { r.result = err })
		return err
	case <-time.After(limit):
		r.w.t.Fatalf("the daemon did not stop within %s", limit)
		return nil
	}
}

// await polls the state of the daemon until ok says so. The cycles run in the
// daemon's own goroutines, so a test can only look.
func (r *running) await(ok func(engine.State) bool) engine.State {
	r.w.t.Helper()
	var last engine.State
	require.Eventually(r.w.t, func() bool {
		st, err := r.client.Status(r.w.t.Context())
		if err != nil {
			return false
		}
		last = st
		return ok(st)
	}, 10*time.Second, 5*time.Millisecond)
	return last
}

// state returns the state the daemon has now.
func (r *running) state() engine.State {
	r.w.t.Helper()
	st, err := r.client.Status(r.w.t.Context())
	require.NoError(r.w.t, err)
	return st
}
