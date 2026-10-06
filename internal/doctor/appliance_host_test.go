package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/appnet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// fakePVE is the Proxmox client of the appliance host: it remembers where it
// was asked and with which deadline.
type fakePVE struct {
	mu    sync.Mutex
	asked []string
	perms map[string][]string
	// deadline is the deadline of the last call.
	deadline time.Time
	verify   error
}

func (f *fakePVE) note(ctx context.Context, what string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, what)
	f.deadline, _ = ctx.Deadline()
}

func (f *fakePVE) ACL(ctx context.Context) ([]pve.ACLEntry, error) {
	f.note(ctx, "acl")
	return []pve.ACLEntry{{Path: "/", Type: "user", UGID: "pco@pve", RoleID: "PCO", Propagate: true}}, nil
}

func (f *fakePVE) Users(ctx context.Context) ([]pve.User, error) {
	f.note(ctx, "users")
	return []pve.User{{ID: "pco@pve", Enabled: true}}, nil
}

func (f *fakePVE) Groups(ctx context.Context) ([]pve.Group, error) {
	f.note(ctx, "groups")
	return nil, nil
}

func (f *fakePVE) Roles(ctx context.Context) ([]pve.Role, error) {
	f.note(ctx, "roles")
	return []pve.Role{{ID: "PCO", Privs: []string{"Sys.Audit"}}}, nil
}

func (f *fakePVE) Permissions(ctx context.Context, userid, path string) (map[string][]string, error) {
	f.note(ctx, fmt.Sprintf("permissions %q %q", userid, path))
	if f.perms != nil {
		return f.perms, nil
	}
	return map[string][]string{"/": {"Sys.Audit"}, "/access": {"Sys.Audit"}, "/access/groups": {"Sys.Audit"}}, nil
}

func (f *fakePVE) Subnets(context.Context) ([]pve.Subnet, error)        { return nil, nil }
func (f *fakePVE) NodeDNS(context.Context, string) (pve.NodeDNS, error) { return pve.NodeDNS{}, nil }

func (f *fakePVE) GuestConfig(ctx context.Context, node string, ref model.GuestRef) (pve.GuestConfig, error) {
	f.note(ctx, fmt.Sprintf("config %s %s", node, ref))
	return pve.GuestConfig{Values: map[string]string{"protection": "1"}}, nil
}

func (f *fakePVE) PendingConfig(ctx context.Context, node string, ref model.GuestRef) (map[string]string, error) {
	f.note(ctx, fmt.Sprintf("pending %s %s", node, ref))
	return map[string]string{"protection": "1"}, nil
}

func (f *fakePVE) Snapshots(ctx context.Context, node string, ref model.GuestRef) ([]pve.Snapshot, error) {
	f.note(ctx, fmt.Sprintf("snapshots %s %s", node, ref))
	return []pve.Snapshot{{Name: "a"}}, nil
}

func (f *fakePVE) Replication(ctx context.Context) ([]pve.ReplicationJob, error) {
	f.note(ctx, "replication")
	return []pve.ReplicationJob{{ID: "9240-0"}}, nil
}

func (f *fakePVE) DatacenterFirewall(ctx context.Context) (pve.FirewallOptions, error) {
	f.note(ctx, "datacenter firewall")
	return pve.FirewallOptions{Enabled: true}, nil
}

func (f *fakePVE) NodeFirewall(ctx context.Context, node string) (pve.FirewallOptions, error) {
	f.note(ctx, "node firewall "+node)
	return pve.FirewallOptions{}, nil
}

func (f *fakePVE) NodeNetwork(ctx context.Context, node string) ([]pve.NodeIface, error) {
	f.note(ctx, "network "+node)
	return []pve.NodeIface{{Name: "vmbr0"}}, nil
}

func (f *fakePVE) VNets(ctx context.Context) ([]pve.VNet, error) {
	f.note(ctx, "vnets")
	return []pve.VNet{{Name: "vnet1", Zone: "zone1"}}, nil
}

func (f *fakePVE) LastVerifyError() error { return f.verify }

var testInstall = Self{VMID: testVMID, Node: "pve1", Address: apiAddr, ServerName: "pve1", User: "pco@pve", Token: pcoToken}

func newHost(t *testing.T) (*ApplianceHost, *fakePVE) {
	t.Helper()
	px := &fakePVE{}
	env, _, _ := hostEnv(t)
	return &ApplianceHost{Host: env, Proxmox: px, Install: testInstall, Root: t.TempDir()}, px
}

func TestTheApplianceHostAsksProxmoxAboutItsOwnContainer(t *testing.T) {
	h, px := newHost(t)
	ctx := t.Context()

	cfg, err := h.OwnConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, "1", cfg.Values["protection"])
	_, err = h.OwnPending(ctx)
	require.NoError(t, err)
	snaps, err := h.Snapshots(ctx)
	require.NoError(t, err)
	require.Equal(t, []pve.Snapshot{{Name: "a"}}, snaps)
	_, err = h.Replication(ctx)
	require.NoError(t, err)
	perms, err := h.OwnPermissions(ctx)
	require.NoError(t, err)
	require.Contains(t, perms, "/")
	datacenter, err := h.DatacenterFirewall(ctx)
	require.NoError(t, err)
	require.True(t, datacenter)
	node, err := h.NodeFirewall(ctx)
	require.NoError(t, err)
	require.False(t, node)
	ifaces, err := h.NodeNetwork(ctx)
	require.NoError(t, err)
	require.Equal(t, "vmbr0", ifaces[0].Name)
	vnets, err := h.VNets(ctx)
	require.NoError(t, err)
	require.Equal(t, []pve.VNet{{Name: "vnet1", Zone: "zone1"}}, vnets)

	require.Equal(t, []string{
		"config pve1 lxc/9240", "pending pve1 lxc/9240", "snapshots pve1 lxc/9240", "replication", `permissions "" "/"`,
		"datacenter firewall", "node firewall pve1", "network pve1", "vnets",
	}, px.asked)
	require.Equal(t, testInstall, h.Self())
}

func TestEveryQuestionToProxmoxHasADeadline(t *testing.T) {
	h, px := newHost(t)
	h.Host.Timeout = time.Minute
	for name, ask := range map[string]func() error{
		"config":      func() error { _, err := h.OwnConfig(t.Context()); return err },
		"pending":     func() error { _, err := h.OwnPending(t.Context()); return err },
		"snapshots":   func() error { _, err := h.Snapshots(t.Context()); return err },
		"replication": func() error { _, err := h.Replication(t.Context()); return err },
		"permissions": func() error { _, err := h.OwnPermissions(t.Context()); return err },
		"access":      func() error { _, err := h.AccessData(t.Context()); return err },
		"datacenter":  func() error { _, err := h.DatacenterFirewall(t.Context()); return err },
		"node":        func() error { _, err := h.NodeFirewall(t.Context()); return err },
		"network":     func() error { _, err := h.NodeNetwork(t.Context()); return err },
		"vnets":       func() error { _, err := h.VNets(t.Context()); return err },
	} {
		px.deadline = time.Time{}
		require.NoError(t, ask(), name)
		require.False(t, px.deadline.IsZero(), name)
		require.LessOrEqual(t, time.Until(px.deadline), time.Minute, name)
	}
}

func TestTheAccessControlIsReadAsTheEngineReadsIt(t *testing.T) {
	h, px := newHost(t)

	d, err := h.AccessData(t.Context())

	require.NoError(t, err)
	require.Len(t, d.ACL, 1)
	require.Equal(t, []string{"pco@pve"}, []string{d.Users[0].ID})
	require.Contains(t, px.asked, `permissions "" "/access"`, "the token must see all of it")

	px.perms = map[string][]string{"/access": {"VM.Audit"}}
	_, err = h.AccessData(t.Context())
	require.ErrorContains(t, err, "does not hold Sys.Audit on /access")
}

func TestTheApplianceHostGivesTheVerifyErrorOfTheClient(t *testing.T) {
	h, px := newHost(t)
	require.NoError(t, h.VerifyError())

	px.verify = errors.New("x509: certificate signed by unknown authority")

	require.EqualError(t, h.VerifyError(), "x509: certificate signed by unknown authority")
}

func TestTheVolumeIsCheckedAtTheStateDirectory(t *testing.T) {
	h, _ := newHost(t)
	var path, marker string
	h.StateDir = "/somewhere"
	h.Volume = func(p, m string) error {
		path, marker = p, m
		return fmt.Errorf("%s %w", p, appliance.ErrNoMarker)
	}

	err := h.VolumeMounted()

	require.ErrorIs(t, err, appliance.ErrNoMarker)
	require.Equal(t, "/somewhere", path)
	require.Equal(t, ".volume", marker)
}

func TestTheStateDirectoryIsThatOfTheAppliance(t *testing.T) {
	h, _ := newHost(t)
	require.Equal(t, "/var/lib/pco", h.stateDir())
}

func TestTheDiskIsReadWhereRootIs(t *testing.T) {
	h, _ := newHost(t)
	require.NoError(t, os.MkdirAll(filepath.Join(h.Root, "var", "lib", "pco"), 0o755))

	used, total, err := h.DiskUse("/var/lib/pco")

	require.NoError(t, err)
	require.NotZero(t, total)
	require.LessOrEqual(t, used, total)
	_, _, err = h.DiskUse("/not/there")
	require.Error(t, err)
}

func TestTheJournalIsTheSizeOfItsFiles(t *testing.T) {
	h, _ := newHost(t)
	write := func(rel string, size int) {
		path := filepath.Join(h.Root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, make([]byte, size), 0o644))
	}

	used, err := h.JournalUse(t.Context())
	require.NoError(t, err)
	require.Zero(t, used, "no journal directory is no journal")

	write("var/log/journal/abc/system.journal", 1000)
	write("var/log/journal/abc/user-1000.journal", 24)
	write("run/log/journal/def/system.journal", 500)

	used, err = h.JournalUse(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1524, used)
}

const (
	someTwelve = "some avg10=12.34 avg60=1.00 avg300=0.10 total=123456\nfull avg10=99.00 avg60=0.00 avg300=0.00 total=0\n"
	someOne    = "some avg10=1.50 avg60=1.00 avg300=0.10 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"
)

func writeUnder(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// The pressure of the container is that of its own cgroup, and the node's only
// where the cgroup does not say.
func TestMemoryPressureIsThatOfTheContainerWhereItIsReported(t *testing.T) {
	h, _ := newHost(t)
	_, err := h.MemoryPressure()
	require.ErrorIs(t, err, fs.ErrNotExist, "nothing reports it")

	writeUnder(t, h.Root, "proc/pressure/memory", someTwelve)
	m, err := h.MemoryPressure()
	require.NoError(t, err)
	require.Equal(t, MemoryState{Some10: 12.34}, m, "the node's, which is said not to be the container's")

	writeUnder(t, h.Root, "sys/fs/cgroup/memory.pressure", someOne)
	writeUnder(t, h.Root, "sys/fs/cgroup/memory.current", "125829120\n")
	writeUnder(t, h.Root, "sys/fs/cgroup/memory.max", "536870912\n")
	m, err = h.MemoryPressure()
	require.NoError(t, err)
	require.Equal(t, MemoryState{Some10: 1.5, Own: true, Used: 125829120, Limit: 536870912}, m, "the container's own")

	writeUnder(t, h.Root, "sys/fs/cgroup/memory.max", "max\n")
	m, err = h.MemoryPressure()
	require.NoError(t, err)
	require.Equal(t, MemoryState{Some10: 1.5, Own: true, Used: 125829120}, m, "no limit is a limit of none")
}

func TestMemoryPressureOfAContainerThatItsCgroupDoesNotGive(t *testing.T) {
	h, _ := newHost(t)
	writeUnder(t, h.Root, "sys/fs/cgroup/memory.pressure", "garbage\n")
	writeUnder(t, h.Root, "proc/pressure/memory", someTwelve)

	m, err := h.MemoryPressure()

	require.NoError(t, err, "a cgroup whose pressure cannot be read leaves the node's")
	require.Equal(t, MemoryState{Some10: 12.34}, m)

	writeUnder(t, h.Root, "proc/pressure/memory", "garbage\n")
	_, err = h.MemoryPressure()
	require.ErrorContains(t, err, "has no line of some tasks")
}

func TestTheUseOfMemoryIsGivenWithoutThePressureOfTheCgroup(t *testing.T) {
	h, _ := newHost(t)
	writeUnder(t, h.Root, "proc/pressure/memory", someOne)
	writeUnder(t, h.Root, "sys/fs/cgroup/memory.current", "1000\n")

	m, err := h.MemoryPressure()

	require.NoError(t, err)
	require.Equal(t, MemoryState{Some10: 1.5}, m, "what is of the container alone is not shown beside the pressure of the node")
}

func TestPressureOfALine(t *testing.T) {
	for line, want := range map[string]float64{
		"some avg10=0.00 avg60=0.00 avg300=0.00 total=0":  0,
		"some avg10=10.50 avg60=1.00 avg300=0.10 total=1": 10.5,
	} {
		got, ok := pressureOf(line)
		require.True(t, ok, line)
		require.InDelta(t, want, got, 0.001, line)
	}
	for _, line := range []string{"", "full avg10=1.00", "some avg60=1.00", "some avg10=x"} {
		_, ok := pressureOf(line)
		require.False(t, ok, line)
	}
}

func TestTheSettingsOfTheNodeAreReadFromTheProcFiles(t *testing.T) {
	h, _ := newHost(t)
	write := func(rel, content string) {
		path := filepath.Join(h.Root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	_, err := h.RmemMax()
	require.Error(t, err)
	there, err := h.EtcPVE()
	require.NoError(t, err)
	require.False(t, there)

	write("proc/sys/net/core/rmem_max", "4194304\n")
	write("etc/pve/.version", "1")

	n, err := h.RmemMax()
	require.NoError(t, err)
	require.Equal(t, 4194304, n)
	there, err = h.EtcPVE()
	require.NoError(t, err)
	require.True(t, there)

	write("proc/sys/net/core/rmem_max", "many\n")
	_, err = h.RmemMax()
	require.Error(t, err)
}

// stalled makes the reads of an ApplianceHost block, as a read of storage that
// does not answer does, until the test ends.
func stalled(t *testing.T, h *ApplianceHost) (reads func() []string) {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var mu sync.Mutex
	var asked []string
	h.Reading = func(what string) {
		mu.Lock()
		asked = append(asked, what)
		mu.Unlock()
		<-release
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

// Every read of the container's own system is bounded: a stalled mount under
// /var/lib/pco or at /etc/pve ends in an error, and does not hold the doctor.
func TestEveryLocalReadHasADeadline(t *testing.T) {
	for name, read := range map[string]func(h *ApplianceHost) error{
		"the volume":   func(h *ApplianceHost) error { return h.VolumeMounted() },
		"the disk":     func(h *ApplianceHost) error { _, _, err := h.DiskUse("/"); return err },
		"etc/pve":      func(h *ApplianceHost) error { _, err := h.EtcPVE(); return err },
		"the journal":  func(h *ApplianceHost) error { _, err := h.JournalUse(t.Context()); return err },
		"memory":       func(h *ApplianceHost) error { _, err := h.MemoryPressure(); return err },
		"rmem_max":     func(h *ApplianceHost) error { _, err := h.RmemMax(); return err },
		"the versions": func(h *ApplianceHost) error { _, _, _, err := h.Versions(t.Context()); return err },
		"the manifest": func(h *ApplianceHost) error { _, err := h.Manifest(); return err },
		"the resolvers": func(h *ApplianceHost) error {
			_, err := h.Resolvers()
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := newHost(t)
			h.Host.Timeout = 50 * time.Millisecond
			h.Host.Binary = fakeBinary(t, "echo cloudflared version 2026.9.3")
			h.Resolve = func() ([]netip.Addr, error) { return nil, nil }
			reads := stalled(t, h)
			done := make(chan error, 1)

			go func() { done <- read(h) }()

			select {
			case err := <-done:
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.ErrorContains(t, err, "no answer within 50ms")
			case <-time.After(10 * time.Second):
				t.Fatal("the read is not bounded: a stalled storage holds it for good")
			}
			require.NotEmpty(t, reads())
		})
	}
}

func TestAStalledReadIsNotStartedAgainWhileItRuns(t *testing.T) {
	h, _ := newHost(t)
	h.Host.Timeout = 20 * time.Millisecond
	reads := stalled(t, h)

	_, _, err := h.DiskUse("/")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	start := time.Now()
	_, _, err = h.DiskUse("/")

	require.ErrorContains(t, err, "an earlier read of it has not returned yet")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 15*time.Millisecond, "it fails at once, and does not wait the deadline again")
	require.Len(t, reads(), 1, "one read waits on the storage, not one for every doctor run")
}

// Only a read that was given up on holds the next one off: two doctors that
// read the same thing at once, both well within the deadline, both get it.
func TestTwoReadsOfOneThingAtOnceDoNotHoldEachOtherOff(t *testing.T) {
	h, _ := newHost(t)
	h.Host.Timeout = 10 * time.Second
	var inside atomic.Int32
	read := func(ctx context.Context) error {
		inside.Add(1)
		for inside.Load() < 2 {
			if err := ctx.Err(); err != nil {
				return err
			}
			time.Sleep(time.Millisecond)
		}
		return nil
	}
	errs := make(chan error, 2)

	for range 2 {
		go func() { errs <- h.local("the use of /", read) }()
	}

	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
}

func TestAReadThatReturnsLateIsReadAgain(t *testing.T) {
	h, _ := newHost(t)
	h.Host.Timeout = 20 * time.Millisecond
	release := make(chan struct{})
	var first atomic.Bool
	h.Reading = func(string) {
		if first.CompareAndSwap(false, true) {
			<-release
		}
	}
	require.NoError(t, os.MkdirAll(filepath.Join(h.Root, "etc", "pve"), 0o755))

	_, err := h.EtcPVE()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	close(release)

	require.Eventually(t, func() bool {
		there, err := h.EtcPVE()
		return err == nil && there
	}, 5*time.Second, 5*time.Millisecond, "once the stalled read is over, the next one is made")
}

func TestAReadThatPanicsIsAnError(t *testing.T) {
	h, _ := newHost(t)
	h.Reading = func(string) { panic("boom") }

	_, err := h.EtcPVE()

	require.ErrorContains(t, err, "boom")
}

func TestTheVersionsAreThoseOfPcoCloudflaredAndDebian(t *testing.T) {
	h, _ := newHost(t)
	h.Host.Timeout = time.Minute
	h.Host.Binary = fakeBinary(t, `echo "cloudflared version 2026.9.3 (built 2026-09-20-1200 UTC)"`)
	require.NoError(t, os.MkdirAll(filepath.Join(h.Root, "etc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(h.Root, "etc", "debian_version"), []byte("13.1\n"), 0o644))

	pco, cloudflared, debian, err := h.Versions(t.Context())

	require.NoError(t, err)
	require.Equal(t, "dev", pco, "a build without a version")
	require.Equal(t, "2026.9.3", cloudflared)
	require.Equal(t, "13.1", debian)
}

func TestVersionsWithoutDebianVersionOrCloudflared(t *testing.T) {
	h, _ := newHost(t)
	// A shell script may start slowly on a busy machine; the timeout is not
	// what this tests.
	h.Host.Timeout = 30 * time.Second
	h.Host.Binary = filepath.Join(t.TempDir(), "missing")

	_, _, _, err := h.Versions(t.Context())

	require.Error(t, err, "a cloudflared that does not run is no version")

	h.Host.Binary = fakeBinary(t, `echo "something else"`)
	_, cloudflared, debian, err := h.Versions(t.Context())
	require.NoError(t, err)
	require.Equal(t, "something else", cloudflared, "what it printed, when no version can be read out of it")
	require.Equal(t, "unknown", debian)
}

func TestTheManifestIsReadFromThePackage(t *testing.T) {
	h, _ := newHost(t)
	_, err := h.Manifest()
	require.Error(t, err)

	path := filepath.Join(t.TempDir(), "cloudflared-versions.json")
	h.ManifestPath = path
	require.NoError(t, os.WriteFile(path, []byte(`{"schemaVersion":1,"updated":"2026-10-05","versions":[{"version":"2026.9.3",
		"amd64":{"url":"https://example.net/a.deb","sha256":"`+strings.Repeat("a", 64)+`"},
		"arm64":{"url":"https://example.net/b.deb","sha256":"`+strings.Repeat("b", 64)+`"}}],
		"deny":[{"version":"2026.9.0","reason":"a bug"}]}`), 0o644))

	m, err := h.Manifest()

	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), m.Updated)
	reason, denied := m.Denied("2026.9.0")
	require.True(t, denied)
	require.Equal(t, "a bug", reason)
}

func TestTheNameServersAreAskedForAName(t *testing.T) {
	h, _ := newHost(t)
	h.Resolve = func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil }
	var asked string
	var deadline time.Time
	h.LookupHost = func(ctx context.Context, name string) ([]string, error) {
		asked, deadline = name, deadlineOf(ctx)
		return []string{"198.41.192.7"}, nil
	}

	servers, err := h.Resolvers()
	require.NoError(t, err)
	require.Equal(t, []netip.Addr{netip.MustParseAddr("10.0.0.1")}, servers)
	require.NoError(t, h.Lookup(t.Context(), "region1.v2.argotunnel.com"))
	require.Equal(t, "region1.v2.argotunnel.com", asked)
	require.False(t, deadline.IsZero())

	h.LookupHost = func(context.Context, string) ([]string, error) { return nil, nil }
	require.ErrorContains(t, h.Lookup(t.Context(), "x"), "without an address")
	h.LookupHost = func(context.Context, string) ([]string, error) { return nil, errors.New("i/o timeout") }
	require.ErrorContains(t, h.Lookup(t.Context(), "x"), "i/o timeout")
}

func TestCloudflaresDateIsTheDateOfItsAnswer(t *testing.T) {
	date := time.Date(2026, 10, 1, 12, 0, 7, 0, time.UTC)
	var path, agent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, agent = r.URL.Path, r.UserAgent()
		w.Header().Set("Date", date.Format(http.TimeFormat))
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	h, _ := newHost(t)
	h.CloudflareURL = srv.URL + "/"

	got, err := h.CloudflareDate(t.Context())

	require.NoError(t, err, "whatever its status: it answered")
	require.True(t, date.Equal(got), "%s", got)
	require.Equal(t, "/client/v4/ips", path)
	require.Equal(t, "pco-doctor", agent)
}

func TestCloudflaresAnswerWithoutADate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Date"] = nil
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	h, _ := newHost(t)
	h.CloudflareURL = srv.URL

	_, err := h.CloudflareDate(t.Context())

	require.ErrorContains(t, err, "without a usable Date header")
}

func TestCloudflaresAnswerThatDoesNotCome(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h, _ := newHost(t)
	h.CloudflareURL = srv.URL
	srv.Close()

	_, err := h.CloudflareDate(t.Context())

	require.Error(t, err)
}

func TestTheHeldPackagesAreTheOnesAptMarkListsAsHeld(t *testing.T) {
	h, _ := newHost(t)
	var name string
	var args []string
	h.Run = func(_ context.Context, n string, a ...string) (string, error) {
		name, args = n, a
		return "cloudflared\npco\n", nil
	}

	held, err := h.Holds(t.Context())

	require.NoError(t, err)
	require.Equal(t, []string{"cloudflared", "pco"}, held)
	require.Equal(t, "/usr/bin/apt-mark", name)
	require.Equal(t, []string{"showhold"}, args)
}

func TestUnattendedUpgradesAreOnWhenTheUnitIsEnabledAndAptIsToldToRun(t *testing.T) {
	for _, tt := range []struct {
		name   string
		state  string
		apt    string
		aptErr error
		want   bool
		ran    bool
	}{
		{"enabled and on", "enabled", "APT::Periodic::Unattended-Upgrade \"1\";\n", nil, true, true},
		{"enabled for this boot and on", "enabled-runtime", "APT::Periodic::Unattended-Upgrade \"1\";\n", nil, true, true},
		{"enabled and told to run weekly", "enabled", "APT::Periodic::Unattended-Upgrade \"7\";\n", nil, true, true},
		{"enabled and off", "enabled", "APT::Periodic::Unattended-Upgrade \"0\";\n", nil, false, true},
		{"enabled and not set", "enabled", "", nil, false, true},
		{"disabled", "disabled", "APT::Periodic::Unattended-Upgrade \"1\";\n", nil, false, false},
		{"masked", "masked", "", nil, false, false},
		{"apt-config that fails", "enabled", "", errors.New("exit status 100"), false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newHost(t)
			h.Host.FileState = func(_ context.Context, unit string) (string, error) {
				require.Equal(t, "unattended-upgrades.service", unit)
				return tt.state, nil
			}
			var ran bool
			h.Run = func(_ context.Context, name string, args ...string) (string, error) {
				ran = true
				require.Equal(t, "/usr/bin/apt-config", name)
				require.Equal(t, []string{"dump", "APT::Periodic::Unattended-Upgrade"}, args)
				return tt.apt, tt.aptErr
			}

			got, err := h.UnattendedUpgrades(t.Context())

			require.Equal(t, tt.aptErr, err)
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.ran, ran)
		})
	}
}

func TestTheUnitFileStateIsAskedWithADeadline(t *testing.T) {
	h, _ := newHost(t)
	var deadline time.Time
	h.Host.FileState = func(ctx context.Context, unit string) (string, error) {
		deadline = deadlineOf(ctx)
		return "masked", nil
	}

	state, err := h.UnitFileState(t.Context(), "nftables.service")

	require.NoError(t, err)
	require.Equal(t, "masked", state)
	require.False(t, deadline.IsZero())
	require.LessOrEqual(t, time.Until(deadline), hostTimeout)
}

func TestWhatSystemctlAnswersAboutAUnitFile(t *testing.T) {
	exit1 := errors.New("exit status 1")
	for _, tt := range []struct {
		name           string
		stdout, stderr string
		err            error
		state          string
		wantErr        string
	}{
		{"enabled", "enabled\n", "", nil, "enabled", ""},
		{"masked, which it says with a failure", "masked\n", "", exit1, "masked", ""},
		{"disabled, which it says with a failure", "disabled\n", "", exit1, "disabled", ""},
		{"no such unit", "", "Failed to get unit file state for x.service: No such file or directory\n", exit1, "not-found", ""},
		{"a failure it explains", "", "Failed to connect to bus: No medium found\n", exit1, "", "exit status 1: Failed to connect to bus: No medium found"},
		{"a failure it does not explain", "", "", exit1, "", "exit status 1"},
		{"nothing at all", "", "", nil, "", "systemctl is-enabled printed nothing"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state, err := unitFileState(tt.stdout, tt.stderr, tt.err)

			require.Equal(t, tt.state, state)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, tt.wantErr)
			}
		})
	}
}

func TestWhatAptConfigSaysOfThePeriodicRun(t *testing.T) {
	for out, want := range map[string]bool{
		"APT::Periodic::Unattended-Upgrade \"1\";\n":      true,
		"APT::Periodic::Unattended-Upgrade \"always\";\n": false,
		"APT::Periodic::Unattended-Upgrade \"0\";\n":      false,
		"": false,
		"APT::Periodic::Update-Package-Lists \"1\";\n":                                           false,
		"APT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n": true,
	} {
		require.Equal(t, want, periodicOn(out), out)
	}
}

// fakeNetlink has all of what pco-net.service loads.
type fakeNetlink struct{}

func (fakeNetlink) EnsureDummy(context.Context, string) error                           { return nil }
func (fakeNetlink) HasDummy(context.Context, string) (bool, error)                      { return true, nil }
func (fakeNetlink) EnsureAddr(context.Context, string, netip.Prefix) error              { return nil }
func (fakeNetlink) HasAddr(context.Context, string, netip.Prefix) (bool, error)         { return true, nil }
func (fakeNetlink) EnsureRoute(context.Context, netip.Prefix, string, netip.Addr) error { return nil }
func (fakeNetlink) HasRoute(context.Context, netip.Prefix, string, netip.Addr) (bool, error) {
	return true, nil
}
func (fakeNetlink) EnsureRule(context.Context, int, netip.Addr, netip.Prefix, bool) error { return nil }
func (fakeNetlink) HasRule(context.Context, int, netip.Addr, netip.Prefix, bool) (bool, error) {
	return true, nil
}

var _ appnet.Netlink = fakeNetlink{}

// listingNft lists a table as pco-net.service loaded it.
type listingNft struct {
	listing []byte
	err     error
}

func (listingNft) Apply(context.Context, string) error { return nil }

func (n listingNft) List(context.Context) ([]byte, error) { return n.listing, n.err }

func TestTheServicePrefixIsVerifiedAndItsRejectedPacketsCounted(t *testing.T) {
	listing, err := os.ReadFile(filepath.Join("..", "appnet", "testdata", "listing-1.1.3.json"))
	require.NoError(t, err)
	h, _ := newHost(t)
	h.Netlink, h.NetNft = fakeNetlink{}, listingNft{listing: listing}

	require.NoError(t, h.NetVerify(t.Context()))
	n, err := h.NetLeaked(t.Context())
	require.NoError(t, err)
	require.Zero(t, n)

	counted := strings.Replace(string(listing), `"packets": 0, "bytes": 0`, `"packets": 7, "bytes": 420`, 1)
	require.NotEqual(t, string(listing), counted)
	h.NetNft = listingNft{listing: []byte(counted)}
	n, err = h.NetLeaked(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 7, n)

	h.NetNft = listingNft{err: egress.ErrNotLoaded}
	require.Error(t, h.NetVerify(t.Context()))
	_, err = h.NetLeaked(t.Context())
	require.ErrorIs(t, err, egress.ErrNotLoaded)
}

func TestTheProbeIsRunByTheHostOrTheOneItIsGiven(t *testing.T) {
	h, _ := newHost(t)
	var asked string
	h.Probe = func(_ context.Context, addr string) error {
		asked = addr
		return ErrRefused
	}

	err := h.ProbeAsConnector(t.Context(), "10.92.0.1:8006")

	require.ErrorIs(t, err, ErrRefused)
	require.Equal(t, "10.92.0.1:8006", asked)
}

func TestWhatPcoEgressProbeAnswers(t *testing.T) {
	exit1 := errors.New("exit status 1")
	for _, tt := range []struct {
		name           string
		stdout, stderr string
		err            error
		want           string
		refused        bool
	}{
		{"a connection", "connected", "", nil, "", false},
		{"a refusal", "refused", "", exit1, "", true},
		{"a failure", "failed: dial tcp 10.0.0.1:80: i/o timeout", "", exit1, "dial tcp 10.0.0.1:80: i/o timeout", false},
		{"a failure with more lines", "failed: no route\nmore", "", exit1, "no route", false},
		{"a probe that did not run", "", "permission denied", exit1, "pco egress probe did not run: exit status 1: permission denied", false},
		{"a probe that did not run and said nothing", "", "", exit1, "pco egress probe did not run: exit status 1", false},
		{"an answer that is none", "hello", "", nil, `pco egress probe answered "hello"`, false},
		{"a connection and a failure", "connected", "", exit1, "pco egress probe did not run: exit status 1", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := probeResult(tt.stdout, tt.stderr, tt.err)

			switch {
			case tt.refused:
				require.ErrorIs(t, err, ErrRefused)
			case tt.want == "":
				require.NoError(t, err)
			default:
				require.EqualError(t, err, tt.want)
				require.NotErrorIs(t, err, ErrRefused)
			}
		})
	}
}

func TestTheProbeNeedsTheConnectorUser(t *testing.T) {
	if _, err := user.Lookup(egress.ConnectorUser); err == nil {
		t.Skip("this machine has " + egress.ConnectorUser)
	}

	h, _ := newHost(t)

	err := h.ProbeAsConnector(t.Context(), "10.0.0.1:80")

	require.ErrorIs(t, err, egress.ErrNoConnectorUser)
}

func TestTheProbeRunsPcoEgressProbeWithTheAddressAndNothingElse(t *testing.T) {
	exe := fakeBinary(t, `[ "$1 $2" = "egress probe" ] || { echo "failed: wrong arguments $*"; exit 1; }
[ "$3" = "10.0.0.1:80" ] || { echo "failed: wrong address $3"; exit 1; }
[ "$(pwd)" = "/" ] || { echo "failed: in $(pwd)"; exit 1; }
[ -z "$HOME" ] || { echo "failed: HOME is $HOME"; exit 1; }
echo connected`)

	// A shell script may start slowly on a busy machine; the limit is not what
	// this tests.
	require.NoError(t, runProbe(t.Context(), exe, "10.0.0.1:80", nil, 30*time.Second))
	require.ErrorContains(t, runProbe(t.Context(), exe, "10.0.0.2:80", nil, 30*time.Second), "wrong address 10.0.0.2:80")
}

func TestAProbeThatDoesNotEnd(t *testing.T) {
	exe := fakeBinary(t, "exec sleep 30")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := runProbe(ctx, exe, "10.0.0.1:80", nil, probeTimeout)

	require.Error(t, err)
	require.Less(t, time.Since(start), 10*time.Second)
}

// A package upgrade replaces the binary of the daemon, and Linux then names
// its path with " (deleted)": the probe runs the pco that is at the path now.
func TestThePcoAProbeRuns(t *testing.T) {
	there := func(paths ...string) func(string) bool {
		return func(path string) bool { return slices.Contains(paths, path) }
	}
	for _, tt := range []struct {
		name   string
		exe    string
		err    error
		exists func(string) bool
		want   string
	}{
		{"the running one", "/usr/bin/pco", nil, there(), "/usr/bin/pco"},
		{"one that was replaced", "/usr/bin/pco (deleted)", nil, there("/usr/bin/pco"), "/usr/bin/pco"},
		{"one that was replaced elsewhere", "/opt/pco/bin/pco (deleted)", nil, there("/opt/pco/bin/pco"), "/opt/pco/bin/pco"},
		{"one that was replaced by nothing", "/opt/pco/bin/pco (deleted)", nil, there(), defaultPco},
		{"one that is not told", "", errors.New("readlink /proc/self/exe: no such file"), there(), defaultPco},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, probeExecutable(tt.exe, tt.err, tt.exists))
		})
	}
}

func TestTheApplianceHostIsTheEnvOfAnAppliance(t *testing.T) {
	h, _ := newHost(t)
	env, _, _ := hostEnv(t)
	require.Nil(t, env.Appliance(), "a host has none")

	env.App = h

	require.Same(t, h, env.Appliance())
	require.True(t, slices.Contains(applianceNames(), "net"))
}

// applianceNames are the names of the checks the appliance runs.
func applianceNames() []string {
	var names []string
	for _, c := range applianceRuns {
		names = append(names, c.name)
	}
	return names
}

// The checks of the appliance are those the tests above pin and the docs
// list: one in this list and not in the other is a check nobody pinned.
func TestTheChecksOfTheApplianceAreTheOnesTheTestsPin(t *testing.T) {
	want := append(slices.Clone(applianceChecks), "epoch")
	slices.Sort(want)

	got := applianceNames()
	slices.Sort(got)

	require.Equal(t, want, got)
}
