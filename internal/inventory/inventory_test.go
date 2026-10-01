package inventory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

var (
	refWeb   = model.GuestRef{Kind: model.KindQEMU, VMID: 101} // watched, running, agent
	refBatch = model.GuestRef{Kind: model.KindQEMU, VMID: 102} // watched, stopped
	refDB    = model.GuestRef{Kind: model.KindLXC, VMID: 200}  // not watched, running
	refApp   = model.GuestRef{Kind: model.KindLXC, VMID: 201}  // watched, running
)

// fakeSource is a Source that counts its calls and fails on demand. Test code
// may change its fields between refreshes, never during one.
type fakeSource struct {
	mu    sync.Mutex
	calls map[string]int

	resources    []pve.Resource
	resourcesErr error
	configs      map[model.GuestRef]pve.GuestConfig
	configErrs   map[model.GuestRef]error
	configGate   func(ctx context.Context)
	ifaces       map[model.GuestRef][]pve.GuestIface
	ifaceErrs    map[model.GuestRef]error
	ifaceHooks   map[model.GuestRef]func(ctx context.Context) ([]pve.GuestIface, error)
	nodes        []pve.ClusterNode
	nodesErr     error
	networks     map[string][]pve.NodeIface
	networkErrs  map[string]error
}

func newFake() *fakeSource {
	f := &fakeSource{
		calls:       map[string]int{},
		configs:     map[model.GuestRef]pve.GuestConfig{},
		configErrs:  map[model.GuestRef]error{},
		ifaces:      map[model.GuestRef][]pve.GuestIface{},
		ifaceErrs:   map[model.GuestRef]error{},
		ifaceHooks:  map[model.GuestRef]func(context.Context) ([]pve.GuestIface, error){},
		networks:    map[string][]pve.NodeIface{},
		networkErrs: map[string]error{},
	}
	f.addGuest(
		pve.Resource{Kind: model.KindQEMU, VMID: 101, Name: "web-1", Node: "pve1", Status: "running", Tags: []string{"cf-tunnel", "prod"}},
		map[string]string{
			"name":        "web-1",
			"description": "front end",
			"net0":        "virtio=BC:24:11:00:AA:01,bridge=vmbr0",
			"smbios1":     "uuid=80e8ef19-34bf-4cb3-b314-96870ca14d1e",
		},
		[]pve.GuestIface{iface("eth0", "bc:24:11:00:aa:01", "10.20.0.15")},
	)
	f.addGuest(
		pve.Resource{Kind: model.KindQEMU, VMID: 102, Name: "batch-1", Node: "pve1", Status: "stopped", Tags: []string{"cf-tunnel"}},
		map[string]string{
			"name":    "batch-1",
			"net0":    "virtio=BC:24:11:00:AA:02,bridge=vmbr0",
			"smbios1": "uuid=4f9d2b7e-8c1a-4e63-9d55-0a1b2c3d4e5f",
		},
		nil,
	)
	f.addGuest(
		pve.Resource{Kind: model.KindLXC, VMID: 200, Name: "db-1", Node: "pve2", Status: "running"},
		map[string]string{
			"hostname": "db-1",
			"net0":     "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:BB:01,ip=10.20.0.30/24",
		},
		[]pve.GuestIface{iface("eth0", "bc:24:11:00:bb:01", "10.20.0.30")},
	)
	f.addGuest(
		pve.Resource{Kind: model.KindLXC, VMID: 201, Name: "app-1", Node: "pve2", Status: "running", Tags: []string{"cf-tunnel"}},
		map[string]string{
			"hostname": "app-1",
			"net0":     "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:BB:02,ip=10.20.0.31/24",
		},
		[]pve.GuestIface{iface("eth0", "bc:24:11:00:bb:02", "10.20.0.31")},
	)
	f.nodes = []pve.ClusterNode{
		{Name: "pve1", Addr: ip("10.20.0.2"), Online: true, Local: true},
		{Name: "pve2", Addr: ip("10.20.0.3"), Online: true},
	}
	f.networks["pve1"] = []pve.NodeIface{
		{Name: "vmbr0", Type: "bridge", Active: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.20.0.2/24")}, Ports: []string{"eno1"}},
	}
	f.networks["pve2"] = []pve.NodeIface{
		{Name: "vmbr0", Type: "bridge", Active: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.20.0.3/24")}, Ports: []string{"eno1"}},
		{Name: "vmbr1", Type: "bridge", Active: true, Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.50.3/24")}},
	}
	return f
}

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

func iface(name, mac string, addrs ...string) pve.GuestIface {
	out := pve.GuestIface{Name: name, MAC: mac}
	for _, a := range addrs {
		out.Addrs = append(out.Addrs, ip(a))
	}
	return out
}

func notFoundErr() error {
	return fmt.Errorf("fetching config: %w", &pve.APIError{Status: 500, Message: "Configuration file 'nodes/pve1/qemu-server/101.conf' does not exist"})
}

func serverErr() error {
	return fmt.Errorf("fetching: %w", &pve.APIError{Status: 500, Message: "internal error"})
}

func forbiddenErr() error {
	return fmt.Errorf("fetching: %w", &pve.APIError{Status: 403, Message: "Permission check failed (/vms/101, VM.GuestAgent.Audit)"})
}

func agentErr(message string) error {
	return fmt.Errorf("fetching agent interfaces of qemu/101: %w: %w", pve.ErrAgentUnavailable, &pve.APIError{Status: 500, Message: message})
}

func (f *fakeSource) addGuest(row pve.Resource, values map[string]string, ifaces []pve.GuestIface) {
	ref := model.GuestRef{Kind: row.Kind, VMID: row.VMID}
	f.resources = append(f.resources, row)
	f.configs[ref] = pve.GuestConfig{Values: values, Digest: "d1g3st"}
	f.ifaces[ref] = ifaces
}

func (f *fakeSource) removeGuest(ref model.GuestRef) {
	f.resources = slices.DeleteFunc(f.resources, func(r pve.Resource) bool {
		return model.GuestRef{Kind: r.Kind, VMID: r.VMID} == ref
	})
}

func (f *fakeSource) setStatus(ref model.GuestRef, status string) {
	for i := range f.resources {
		if (model.GuestRef{Kind: f.resources[i].Kind, VMID: f.resources[i].VMID}) == ref {
			f.resources[i].Status = status
		}
	}
}

func (f *fakeSource) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[call]++
}

func (f *fakeSource) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[call]
}

func (f *fakeSource) nodeOf(ref model.GuestRef) string {
	for _, r := range f.resources {
		if (model.GuestRef{Kind: r.Kind, VMID: r.VMID}) == ref {
			return r.Node
		}
	}
	return ""
}

func (f *fakeSource) Resources(context.Context) ([]pve.Resource, error) {
	f.record("resources")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resourcesErr != nil {
		return nil, f.resourcesErr
	}
	return slices.Clone(f.resources), nil
}

func (f *fakeSource) GuestConfig(ctx context.Context, node string, ref model.GuestRef) (pve.GuestConfig, error) {
	f.record("config " + ref.String())
	if f.configGate != nil {
		f.configGate(ctx)
	}
	if err := ctx.Err(); err != nil {
		return pve.GuestConfig{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if want := f.nodeOf(ref); want != node {
		return pve.GuestConfig{}, fmt.Errorf("config of %s asked from node %q, guest is on %q", ref, node, want)
	}
	if err := f.configErrs[ref]; err != nil {
		return pve.GuestConfig{}, err
	}
	return f.configs[ref], nil
}

func (f *fakeSource) AgentInterfaces(ctx context.Context, node string, vmid int) ([]pve.GuestIface, error) {
	return f.interfaces(ctx, node, model.GuestRef{Kind: model.KindQEMU, VMID: vmid})
}

func (f *fakeSource) LXCInterfaces(ctx context.Context, node string, vmid int) ([]pve.GuestIface, error) {
	return f.interfaces(ctx, node, model.GuestRef{Kind: model.KindLXC, VMID: vmid})
}

func (f *fakeSource) interfaces(ctx context.Context, node string, ref model.GuestRef) ([]pve.GuestIface, error) {
	f.record("iface " + ref.String())
	f.mu.Lock()
	hook := f.ifaceHooks[ref]
	want := f.nodeOf(ref)
	err := f.ifaceErrs[ref]
	out := f.ifaces[ref]
	f.mu.Unlock()
	if want != node {
		return nil, fmt.Errorf("interfaces of %s asked from node %q, guest is on %q", ref, node, want)
	}
	if hook != nil {
		return hook(ctx)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (f *fakeSource) ClusterNodes(context.Context) ([]pve.ClusterNode, error) {
	f.record("nodes")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nodesErr != nil {
		return nil, f.nodesErr
	}
	return slices.Clone(f.nodes), nil
}

func (f *fakeSource) NodeNetwork(_ context.Context, node string) ([]pve.NodeIface, error) {
	f.record("network " + node)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.networkErrs[node]; err != nil {
		return nil, err
	}
	return slices.Clone(f.networks[node]), nil
}

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newInventory(src Source, opts Options) (*Inventory, *testClock) {
	clk := &testClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	if opts.GateTags == nil {
		opts.GateTags = []string{"cf-tunnel"}
	}
	return New(src, opts, clk.now, zerolog.Nop()), clk
}

func refs(snap Snapshot) []model.GuestRef {
	out := make([]model.GuestRef, 0, len(snap.Guests))
	for _, g := range snap.Guests {
		out = append(out, g.Ref)
	}
	return out
}

func guestOf(t *testing.T, snap Snapshot, ref model.GuestRef) model.Guest {
	t.Helper()
	g, ok := snap.Guest(ref)
	require.True(t, ok, "guest %s missing from snapshot", ref)
	return g
}

func TestRefreshFirstFetchesEveryConfig(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.True(t, snap.Complete)
	require.Empty(t, snap.Problems)
	require.Equal(t, clk.t, snap.TakenAt)
	require.Equal(t, []model.GuestRef{refDB, refApp, refWeb, refBatch}, refs(snap))
	for _, ref := range []model.GuestRef{refWeb, refBatch, refDB, refApp} {
		require.Equal(t, 1, src.count("config "+ref.String()), ref.String())
	}

	web := guestOf(t, snap, refWeb)
	require.Equal(t, "web-1", web.Name)
	require.Equal(t, "pve1", web.Node)
	require.True(t, web.Running)
	require.Equal(t, []string{"cf-tunnel", "prod"}, web.Tags)
	require.Equal(t, "front end", web.Description)
	require.Equal(t, "uuid:80e8ef19-34bf-4cb3-b314-96870ca14d1e", web.Identity)
	require.Len(t, web.NICs, 1)
	require.Equal(t, []model.ReportedAddr{{Iface: "eth0", MAC: "bc:24:11:00:aa:01", Addr: ip("10.20.0.15")}}, web.Reported)
}

func TestRefreshSecondFetchesOnlyWatchedConfigs(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())
	clk.advance(10 * time.Second)

	snap := inv.Refresh(t.Context())

	require.True(t, snap.Complete)
	require.Len(t, snap.Guests, 4)
	for _, ref := range []model.GuestRef{refWeb, refBatch, refApp} {
		require.Equal(t, 2, src.count("config "+ref.String()), "watched %s", ref)
	}
	require.Equal(t, 1, src.count("config "+refDB.String()), "unwatched guest comes from the cache")
	require.Equal(t, "db-1", guestOf(t, snap, refDB).Name)
	require.Equal(t, "bc:24:11:00:bb:01", guestOf(t, snap, refDB).NICs[0].MAC)
}

func TestRefreshFullSweepRefetchesUnwatchedConfigs(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())

	clk.advance(5*time.Minute - time.Second)
	inv.Refresh(t.Context())
	require.Equal(t, 1, src.count("config "+refDB.String()), "just before the sweep")

	clk.advance(time.Second)
	inv.Refresh(t.Context())
	require.Equal(t, 2, src.count("config "+refDB.String()), "at the sweep")

	clk.advance(time.Minute)
	inv.Refresh(t.Context())
	require.Equal(t, 2, src.count("config "+refDB.String()), "the sweep restarts the interval")
}

func TestRefreshHonoursConfiguredSweepInterval(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{FullSweepEvery: time.Minute})
	inv.Refresh(t.Context())

	clk.advance(time.Minute)
	inv.Refresh(t.Context())

	require.Equal(t, 2, src.count("config "+refDB.String()))
}

func TestRefreshUsesCachedConfigWithCurrentResourceRow(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())
	src.setStatus(refDB, "stopped")
	clk.advance(10 * time.Second)

	snap := inv.Refresh(t.Context())

	require.False(t, guestOf(t, snap, refDB).Running)
	require.Equal(t, 1, src.count("config "+refDB.String()))
}

func TestRefreshGuestBecomingWatchedIsFetchedAtOnce(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())
	for i := range src.resources {
		if src.resources[i].VMID == refDB.VMID {
			src.resources[i].Tags = []string{"cf-tunnel"}
		}
	}
	clk.advance(10 * time.Second)

	snap := inv.Refresh(t.Context())

	require.Equal(t, 2, src.count("config "+refDB.String()))
	require.Equal(t, 1, src.count("iface "+refDB.String()))
	require.NotEmpty(t, guestOf(t, snap, refDB).Reported)
}

func TestRefreshGateTagsAreComparedExactly(t *testing.T) {
	src := newFake()
	src.addGuest(
		pve.Resource{Kind: model.KindQEMU, VMID: 103, Name: "shout-1", Node: "pve1", Status: "running", Tags: []string{"CF-Tunnel"}},
		map[string]string{"name": "shout-1", "net0": "virtio=BC:24:11:00:AA:03,bridge=vmbr0"},
		[]pve.GuestIface{iface("eth0", "bc:24:11:00:aa:03", "10.20.0.16")},
	)
	ref := model.GuestRef{Kind: model.KindQEMU, VMID: 103}
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())
	clk.advance(10 * time.Second)

	snap := inv.Refresh(t.Context())

	require.Equal(t, 1, src.count("config "+ref.String()))
	require.Zero(t, src.count("iface "+ref.String()))
	require.Empty(t, guestOf(t, snap, ref).Reported)
}

func TestRefreshWithoutGateTagsWatchesNothing(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{GateTags: []string{}})
	inv.Refresh(t.Context())
	clk.advance(10 * time.Second)

	inv.Refresh(t.Context())

	for _, ref := range []model.GuestRef{refWeb, refBatch, refDB, refApp} {
		require.Equal(t, 1, src.count("config "+ref.String()), ref.String())
		require.Zero(t, src.count("iface "+ref.String()), ref.String())
	}
}

func TestRefreshAgentUnavailableStaysComplete(t *testing.T) {
	var sawDeadline atomic.Bool
	tests := []struct {
		name  string
		setup func(src *fakeSource, inv *Inventory)
	}{
		{"agent missing", func(src *fakeSource, _ *Inventory) {
			src.ifaceErrs[refWeb] = agentErr("No QEMU guest agent configured")
		}},
		{"agent not running", func(src *fakeSource, _ *Inventory) {
			src.ifaceErrs[refWeb] = agentErr("QEMU guest agent is not running")
		}},
		{"agent times out", func(src *fakeSource, inv *Inventory) {
			inv.callTimeout = time.Millisecond
			src.ifaceHooks[refWeb] = func(ctx context.Context) ([]pve.GuestIface, error) {
				if _, ok := ctx.Deadline(); ok {
					sawDeadline.Store(true)
				}
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("fetching agent interfaces of qemu/101: %w", ctx.Err())
				case <-time.After(2 * time.Second): // only reached when no timeout is applied
					return nil, errors.New("agent call was never cut short")
				}
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFake()
			inv, _ := newInventory(src, Options{})
			tt.setup(src, inv)

			snap := inv.Refresh(t.Context())

			require.True(t, snap.Complete)
			require.Empty(t, snap.Problems)
			require.Len(t, snap.Guests, 4)
			require.Empty(t, guestOf(t, snap, refWeb).Reported)
			require.Equal(t, "web-1", guestOf(t, snap, refWeb).Name)
			require.NotEmpty(t, guestOf(t, snap, refApp).Reported, "other guests are unaffected")
		})
	}
	require.True(t, sawDeadline.Load(), "interface calls carry their own deadline")
}

func TestRefreshCachesUnavailableAgentForTTL(t *testing.T) {
	src := newFake()
	src.ifaceErrs[refWeb] = agentErr("QEMU guest agent is not running")
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())

	clk.advance(30 * time.Second)
	snap := inv.Refresh(t.Context())
	require.Equal(t, 1, src.count("iface "+refWeb.String()))
	require.True(t, snap.Complete)

	clk.advance(30 * time.Second)
	delete(src.ifaceErrs, refWeb)
	snap = inv.Refresh(t.Context())
	require.Equal(t, 2, src.count("iface "+refWeb.String()))
	require.NotEmpty(t, guestOf(t, snap, refWeb).Reported)
}

func TestRefreshForbiddenInterfacesAreOneAdvisoryProblem(t *testing.T) {
	src := newFake()
	src.ifaceErrs[refWeb] = forbiddenErr()
	src.ifaceErrs[refApp] = forbiddenErr()
	inv, clk := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.True(t, snap.Complete)
	require.Len(t, snap.Problems, 1)
	require.Contains(t, snap.Problems[0], "guest-agent privilege")
	require.Empty(t, guestOf(t, snap, refWeb).Reported)
	require.Len(t, snap.Guests, 4)

	clk.advance(10 * time.Second)
	snap = inv.Refresh(t.Context())
	require.True(t, snap.Complete)
	require.Len(t, snap.Problems, 1, "the line stays while the privilege is missing")
	require.Equal(t, 1, src.count("iface "+refWeb.String()), "forbidden answers are cached too")
}

func TestRefreshOtherInterfaceFailureMarksIncomplete(t *testing.T) {
	t.Run("nothing cached", func(t *testing.T) {
		src := newFake()
		src.ifaceErrs[refWeb] = serverErr()
		inv, _ := newInventory(src, Options{})

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Len(t, snap.Problems, 1)
		require.Contains(t, snap.Problems[0], "qemu/101")
		require.Empty(t, guestOf(t, snap, refWeb).Reported)
		require.Len(t, snap.Guests, 4)
	})

	t.Run("stale answer is served", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		require.True(t, first.Complete)
		src.ifaceErrs[refWeb] = serverErr()
		clk.advance(time.Minute)

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Equal(t, guestOf(t, first, refWeb).Reported, guestOf(t, snap, refWeb).Reported)

		clk.advance(10 * time.Second)
		snap = inv.Refresh(t.Context())
		require.Equal(t, 3, src.count("iface "+refWeb.String()), "a failed fetch is retried at once")
		require.False(t, snap.Complete)
	})
}

func TestRefreshConfigFailureMarksIncomplete(t *testing.T) {
	t.Run("cached guest is served from the cache", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		src.configErrs[refWeb] = serverErr()
		clk.advance(10 * time.Second)

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Len(t, snap.Problems, 1)
		require.Contains(t, snap.Problems[0], "qemu/101")
		require.Len(t, snap.Guests, 4, "other guests are still returned")
		require.Equal(t, guestOf(t, first, refWeb), guestOf(t, snap, refWeb))
	})

	t.Run("guest without a cache is left out", func(t *testing.T) {
		src := newFake()
		src.configErrs[refDB] = serverErr()
		inv, _ := newInventory(src, Options{})

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Len(t, snap.Problems, 1)
		require.Contains(t, snap.Problems[0], "lxc/200")
		require.Equal(t, []model.GuestRef{refApp, refWeb, refBatch}, refs(snap))
	})

	t.Run("failed sweep keeps the cache and retries next time", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		inv.Refresh(t.Context())
		src.configErrs[refDB] = serverErr()
		clk.advance(5 * time.Minute)

		snap := inv.Refresh(t.Context())
		require.False(t, snap.Complete)
		require.Equal(t, "db-1", guestOf(t, snap, refDB).Name)

		delete(src.configErrs, refDB)
		clk.advance(10 * time.Second)
		snap = inv.Refresh(t.Context())
		require.True(t, snap.Complete)
		require.Equal(t, 3, src.count("config "+refDB.String()))
	})

	t.Run("other watched guests keep their reported addresses", func(t *testing.T) {
		src := newFake()
		src.configErrs[refWeb] = serverErr()
		inv, _ := newInventory(src, Options{})

		snap := inv.Refresh(t.Context())

		require.NotEmpty(t, guestOf(t, snap, refApp).Reported)
	})
}

func TestRefreshVanishedGuestIsDroppedSilently(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())
	src.configErrs[refWeb] = notFoundErr()
	clk.advance(10 * time.Second)

	snap := inv.Refresh(t.Context())

	require.True(t, snap.Complete)
	require.Empty(t, snap.Problems)
	require.Equal(t, []model.GuestRef{refDB, refApp, refBatch}, refs(snap))

	delete(src.configErrs, refWeb)
	clk.advance(10 * time.Second)
	snap = inv.Refresh(t.Context())
	require.Len(t, snap.Guests, 4)
	require.Equal(t, 2, src.count("iface "+refWeb.String()), "the reported cache went with the guest")
}

func TestRefreshResourcesFailureKeepsPreviousGuests(t *testing.T) {
	t.Run("after a good refresh", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		src.resourcesErr = serverErr()
		clk.advance(10 * time.Second)

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Len(t, snap.Problems, 1)
		require.Equal(t, first.Guests, snap.Guests)
		require.Equal(t, first.Nodes, snap.Nodes)
		require.Equal(t, clk.t, snap.TakenAt)
		require.Equal(t, 1, src.count("nodes"), "node calls are skipped")
		require.Equal(t, 1, src.count("config "+refWeb.String()), "so are config calls")

		src.resourcesErr = nil
		snap = inv.Refresh(t.Context())
		require.True(t, snap.Complete)
		require.Empty(t, snap.Problems)
	})

	t.Run("on the first refresh", func(t *testing.T) {
		src := newFake()
		src.resourcesErr = serverErr()
		inv, _ := newInventory(src, Options{})

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Empty(t, snap.Guests)
		require.NotEmpty(t, snap.Problems)
	})
}

func TestRefreshEvictsGuestsThatDisappear(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())
	removed := slices.Clone(src.resources)
	src.removeGuest(refDB)
	src.removeGuest(refWeb)
	clk.advance(10 * time.Second)

	snap := inv.Refresh(t.Context())

	require.True(t, snap.Complete)
	require.Equal(t, []model.GuestRef{refApp, refBatch}, refs(snap))

	src.resources = removed
	clk.advance(10 * time.Second)
	snap = inv.Refresh(t.Context())
	require.Len(t, snap.Guests, 4)
	require.Equal(t, 2, src.count("config "+refDB.String()), "unwatched guest is read again, not served from the cache")
	require.Equal(t, 2, src.count("iface "+refWeb.String()), "reported cache is gone as well")
}

func TestRefreshReportedCacheHonoursTTL(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())
	require.Equal(t, 1, src.count("iface "+refWeb.String()))
	require.Equal(t, 1, src.count("iface "+refApp.String()))

	clk.advance(59 * time.Second)
	snap := inv.Refresh(t.Context())
	require.Equal(t, 1, src.count("iface "+refWeb.String()))
	require.NotEmpty(t, guestOf(t, snap, refWeb).Reported, "served from the cache")

	src.ifaces[refWeb] = []pve.GuestIface{iface("eth0", "bc:24:11:00:aa:01", "10.20.0.99")}
	clk.advance(time.Second)
	snap = inv.Refresh(t.Context())
	require.Equal(t, 2, src.count("iface "+refWeb.String()))
	require.Equal(t, 2, src.count("iface "+refApp.String()))
	require.Equal(t, ip("10.20.0.99"), guestOf(t, snap, refWeb).Reported[0].Addr)
}

func TestRefreshHonoursConfiguredReportedTTL(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{ReportedTTL: 5 * time.Second})
	inv.Refresh(t.Context())

	clk.advance(5 * time.Second)
	inv.Refresh(t.Context())

	require.Equal(t, 2, src.count("iface "+refWeb.String()))
}

func TestRefreshStoppedOrUnwatchedGuestsGetNoInterfaceCalls(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})

	for range 3 {
		snap := inv.Refresh(t.Context())
		require.Empty(t, guestOf(t, snap, refBatch).Reported)
		require.Empty(t, guestOf(t, snap, refDB).Reported)
		clk.advance(2 * time.Minute)
	}

	require.Zero(t, src.count("iface "+refBatch.String()), "stopped")
	require.Zero(t, src.count("iface "+refDB.String()), "not watched")
}

func TestRefreshStoppingAGuestDropsItsReportedAddresses(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())

	src.setStatus(refWeb, "stopped")
	clk.advance(10 * time.Second)
	snap := inv.Refresh(t.Context())
	require.False(t, guestOf(t, snap, refWeb).Running)
	require.Empty(t, guestOf(t, snap, refWeb).Reported)

	src.setStatus(refWeb, "running")
	clk.advance(10 * time.Second)
	snap = inv.Refresh(t.Context())
	require.Equal(t, 2, src.count("iface "+refWeb.String()), "no answer from before the stop is reused")
	require.NotEmpty(t, guestOf(t, snap, refWeb).Reported)
}

func TestRefreshReplacedGuestDropsReportedAddresses(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	inv.Refresh(t.Context())

	src.configs[refWeb].Values["smbios1"] = "uuid=11111111-2222-4333-8444-555555555555"
	src.ifaces[refWeb] = []pve.GuestIface{iface("eth0", "bc:24:11:00:aa:09", "10.20.0.77")}
	clk.advance(10 * time.Second)
	snap := inv.Refresh(t.Context())

	require.Equal(t, 2, src.count("iface "+refWeb.String()))
	require.Equal(t, ip("10.20.0.77"), guestOf(t, snap, refWeb).Reported[0].Addr)
}

func TestRefreshFiltersReportedAddresses(t *testing.T) {
	src := newFake()
	src.ifaces[refWeb] = []pve.GuestIface{
		iface("lo", "", "127.0.0.1"),
		iface("eth0", "bc:24:11:00:aa:01", "10.20.0.15", "127.0.0.2", "169.254.7.7"),
		iface("ens19", "", "10.20.1.15"),
		iface("docker0", "02:42:ac:11:00:01", "172.17.0.1"),
		iface("br-1a2b3c", "02:42:ac:12:00:01", "172.18.0.1"),
		iface("veth9c1", "", "172.19.0.1"),
		iface("cni0", "", "10.244.0.1"),
		iface("cali1234", "", "10.244.1.1"),
		iface("flannel.1", "", "10.244.2.0"),
		iface("virbr0", "", "192.168.122.1"),
		iface("lxcbr0", "", "10.0.3.1"),
		iface("podman0", "", "10.88.0.1"),
		iface("kube-ipvs0", "", "10.96.0.1"),
		iface("tailscale0", "", "100.64.0.5"),
		iface("wg0", "", "10.9.0.2"),
		iface("Local Area Connection", "", "10.20.2.15"),
	}
	inv, _ := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.True(t, snap.Complete)
	require.Equal(t, []model.ReportedAddr{
		{Iface: "eth0", MAC: "bc:24:11:00:aa:01", Addr: ip("10.20.0.15")},
		{Iface: "ens19", MAC: "", Addr: ip("10.20.1.15")},
		{Iface: "Local Area Connection", MAC: "", Addr: ip("10.20.2.15")},
	}, guestOf(t, snap, refWeb).Reported)
}

func TestRefreshIgnoresIPv6ReportedAddresses(t *testing.T) {
	src := newFake()
	src.ifaces[refApp] = []pve.GuestIface{{
		Name:  "eth0",
		MAC:   "bc:24:11:00:bb:02",
		Addrs: []netip.Addr{ip("fd00::31"), ip("10.20.0.31")},
	}}
	inv, _ := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.Equal(t, []model.ReportedAddr{
		{Iface: "eth0", MAC: "bc:24:11:00:bb:02", Addr: ip("10.20.0.31")},
	}, guestOf(t, snap, refApp).Reported)
}

func TestRefreshReadsContainerInterfacesFromLXC(t *testing.T) {
	src := newFake()
	inv, _ := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.Equal(t, 1, src.count("iface "+refApp.String()))
	require.Equal(t, []model.ReportedAddr{
		{Iface: "eth0", MAC: "bc:24:11:00:bb:02", Addr: ip("10.20.0.31")},
	}, guestOf(t, snap, refApp).Reported)
}

func TestRefreshBuildsNodes(t *testing.T) {
	src := newFake()
	src.nodes = []pve.ClusterNode{
		{Name: "pve3", Addr: ip("10.20.0.4"), Online: false},
		{Name: "pve2", Addr: ip("10.20.0.3"), Online: true},
		{Name: "pve1", Addr: ip("10.20.0.2"), Online: true, Local: true},
	}
	inv, _ := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.True(t, snap.Complete)
	require.Equal(t, []Node{
		{Name: "pve1", Addr: ip("10.20.0.2"), Online: true, Local: true, Ifaces: src.networks["pve1"]},
		{Name: "pve2", Addr: ip("10.20.0.3"), Online: true, Ifaces: src.networks["pve2"]},
		{Name: "pve3", Addr: ip("10.20.0.4")},
	}, snap.Nodes)
	require.Zero(t, src.count("network pve3"), "offline nodes are not asked")
	require.Equal(t, 1, src.count("network pve1"))
	require.Equal(t, 1, src.count("network pve2"))
}

func TestRefreshNodeFailuresMarkIncomplete(t *testing.T) {
	t.Run("cluster status fails", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		src.nodesErr = serverErr()
		clk.advance(10 * time.Second)

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Len(t, snap.Problems, 1)
		require.Equal(t, first.Nodes, snap.Nodes)
		require.Len(t, snap.Guests, 4)
	})

	t.Run("network of one node fails", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		src.networkErrs["pve2"] = serverErr()
		clk.advance(10 * time.Second)

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Len(t, snap.Problems, 1)
		require.Contains(t, snap.Problems[0], "pve2")
		require.Equal(t, first.Nodes, snap.Nodes, "the failing node keeps what was known")
	})

	t.Run("network fails on the first refresh", func(t *testing.T) {
		src := newFake()
		src.networkErrs["pve2"] = serverErr()
		inv, _ := newInventory(src, Options{})

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Len(t, snap.Nodes, 2)
		require.Equal(t, "pve2", snap.Nodes[1].Name)
		require.Empty(t, snap.Nodes[1].Ifaces)
	})
}

func TestSnapshotNodeAddrs(t *testing.T) {
	snap := Snapshot{Nodes: []Node{
		{Name: "pve2", Addr: ip("10.20.0.3"), Online: true, Ifaces: []pve.NodeIface{
			{Name: "vmbr0", Addrs: []netip.Prefix{netip.MustParsePrefix("10.20.0.3/24")}},
			{Name: "vmbr1", Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.50.3/24")}},
			{Name: "vmbr6", Addrs: []netip.Prefix{netip.MustParsePrefix("fd00::3/64")}},
			{Name: "eno1"},
		}},
		{Name: "pve1", Addr: ip("10.20.0.2"), Online: true, Ifaces: []pve.NodeIface{
			{Name: "vmbr0", Addrs: []netip.Prefix{netip.MustParsePrefix("10.20.0.2/24")}},
		}},
		{Name: "pve3", Addr: ip("10.20.0.4")},
		{Name: "pve4"},
	}}

	require.Equal(t, []netip.Addr{
		ip("10.20.0.2"), ip("10.20.0.3"), ip("10.20.0.4"), ip("192.168.50.3"),
	}, snap.NodeAddrs())
	require.Empty(t, Snapshot{}.NodeAddrs())
}

func TestRefreshNodeAddrsCoverEveryNode(t *testing.T) {
	src := newFake()
	inv, _ := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.Equal(t, []netip.Addr{ip("10.20.0.2"), ip("10.20.0.3"), ip("192.168.50.3")}, snap.NodeAddrs())
}

func TestSnapshotGuest(t *testing.T) {
	snap := Snapshot{Guests: []model.Guest{
		{Ref: refDB, Name: "db-1"},
		{Ref: refWeb, Name: "web-1"},
	}}

	g, ok := snap.Guest(refWeb)
	require.True(t, ok)
	require.Equal(t, "web-1", g.Name)

	_, ok = snap.Guest(model.GuestRef{Kind: model.KindLXC, VMID: 101})
	require.False(t, ok)
	_, ok = Snapshot{}.Guest(refWeb)
	require.False(t, ok)
}

func TestRefreshOrderIsDeterministic(t *testing.T) {
	build := func() *fakeSource {
		src := newFake()
		for vmid := 300; vmid < 320; vmid++ {
			kind, name := model.KindQEMU, fmt.Sprintf("vm-%d", vmid)
			values := map[string]string{"name": name, "net0": fmt.Sprintf("virtio=BC:24:11:00:CC:%02X,bridge=vmbr0", vmid-300)}
			if vmid%2 == 0 {
				kind = model.KindLXC
				values = map[string]string{"hostname": name, "net0": fmt.Sprintf("name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:DD:%02X", vmid-300)}
			}
			row := pve.Resource{Kind: kind, VMID: vmid, Name: name, Node: "pve1", Status: "running"}
			if vmid%3 == 0 {
				row.Tags = []string{"cf-tunnel"}
			}
			src.addGuest(row, values, []pve.GuestIface{iface("eth0", "", fmt.Sprintf("10.20.1.%d", vmid-300))})
		}
		src.nodes = append(src.nodes, pve.ClusterNode{Name: "pve0", Online: true}, pve.ClusterNode{Name: "pve9"})
		src.networks["pve0"] = []pve.NodeIface{{Name: "vmbr0", Type: "bridge"}}
		return src
	}
	want := func() Snapshot {
		inv, _ := newInventory(build(), Options{Concurrency: 4})
		return inv.Refresh(t.Context())
	}()
	require.True(t, want.Complete)
	require.Len(t, want.Guests, 24)
	require.True(t, slices.IsSortedFunc(want.Guests, func(a, b model.Guest) int {
		return strings.Compare(a.Ref.String(), b.Ref.String())
	}))

	rng := rand.New(rand.NewPCG(7, 11))
	for range 10 {
		src := build()
		rng.Shuffle(len(src.resources), func(i, j int) { src.resources[i], src.resources[j] = src.resources[j], src.resources[i] })
		rng.Shuffle(len(src.nodes), func(i, j int) { src.nodes[i], src.nodes[j] = src.nodes[j], src.nodes[i] })
		inv, _ := newInventory(src, Options{Concurrency: 4})

		got := inv.Refresh(t.Context())

		require.Equal(t, want.Guests, got.Guests)
		require.Equal(t, want.Nodes, got.Nodes)
		require.Equal(t, want.Problems, got.Problems)
	}
}

func TestRefreshBoundsConcurrency(t *testing.T) {
	const limit = 3
	src := newFake()
	for vmid := 300; vmid < 309; vmid++ {
		src.addGuest(
			pve.Resource{Kind: model.KindQEMU, VMID: vmid, Name: fmt.Sprintf("vm-%d", vmid), Node: "pve1", Status: "running"},
			map[string]string{"name": "vm", "net0": fmt.Sprintf("virtio=BC:24:11:00:CC:%02X,bridge=vmbr0", vmid-300)},
			nil,
		)
	}
	var (
		mu       sync.Mutex
		once     sync.Once
		inflight int
		peak     int
		release  = make(chan struct{})
	)
	src.configGate = func(ctx context.Context) {
		mu.Lock()
		inflight++
		peak = max(peak, inflight)
		if inflight == limit {
			once.Do(func() { close(release) })
		}
		mu.Unlock()
		// The first calls wait until "limit" of them are in flight at once,
		// which shows that the pool really runs that many.
		select {
		case <-release:
		case <-ctx.Done():
		}
		mu.Lock()
		inflight--
		mu.Unlock()
	}
	inv, _ := newInventory(src, Options{Concurrency: limit})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	snap := inv.Refresh(ctx)

	require.True(t, snap.Complete)
	require.Len(t, snap.Guests, 13)
	require.Equal(t, limit, peak)
}

func TestRefreshCancelledReturnsPreviousGuests(t *testing.T) {
	t.Run("during the refresh", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		ctx, cancel := context.WithCancel(t.Context())
		src.configGate = func(context.Context) { cancel() }
		clk.advance(10 * time.Second)

		snap := inv.Refresh(ctx)

		require.False(t, snap.Complete)
		require.Len(t, snap.Problems, 1)
		require.Contains(t, snap.Problems[0], "cancelled")
		require.Equal(t, first.Guests, snap.Guests)
		require.Equal(t, first.Nodes, snap.Nodes)
		require.Equal(t, 1, src.count("nodes"), "nothing is asked after the cancellation")

		src.configGate = nil
		snap = inv.Refresh(t.Context())
		require.True(t, snap.Complete)
		require.Empty(t, snap.Problems)
	})

	t.Run("before it starts", func(t *testing.T) {
		src := newFake()
		inv, _ := newInventory(src, Options{})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		snap := inv.Refresh(ctx)

		require.False(t, snap.Complete)
		require.Contains(t, snap.Problems[0], "cancelled")
		require.Empty(t, snap.Guests)
		require.Zero(t, src.count("resources"))
	})

	t.Run("a deadline of the caller is not an agent timeout", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		ctx, cancel := context.WithCancel(t.Context())
		src.ifaceHooks[refWeb] = func(ctx context.Context) ([]pve.GuestIface, error) {
			cancel()
			<-ctx.Done()
			return nil, fmt.Errorf("fetching: %w", context.DeadlineExceeded)
		}
		clk.advance(time.Minute)

		snap := inv.Refresh(ctx)

		require.False(t, snap.Complete)
		require.Contains(t, snap.Problems[0], "cancelled")
		require.Equal(t, first.Guests, snap.Guests)
	})
}

func TestRefreshReportsDuplicateResources(t *testing.T) {
	src := newFake()
	dup := src.resources[0]
	dup.Node = "pve2"
	src.resources = append(src.resources, dup)
	inv, _ := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.False(t, snap.Complete)
	require.Len(t, snap.Problems, 1)
	require.Contains(t, snap.Problems[0], "qemu/101 is listed on both pve1 and pve2")
	require.Len(t, snap.Guests, 4)
	require.Equal(t, 1, src.count("config "+refWeb.String()))
}

func TestRefreshNeverLogsDescriptions(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(zerolog.SyncWriter(&buf)).Level(zerolog.DebugLevel)
	src := newFake()
	src.configs[refWeb].Values["description"] = "SECRET-NOTE"
	clk := &testClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	inv := New(src, Options{GateTags: []string{"cf-tunnel"}}, clk.now, log)

	inv.Refresh(t.Context())
	src.configErrs[refApp] = serverErr()
	src.configErrs[refBatch] = notFoundErr()
	src.ifaceErrs[refWeb] = errors.New("boom")
	clk.advance(time.Minute)
	inv.Refresh(t.Context())

	require.NotEmpty(t, buf.String())
	require.NotContains(t, buf.String(), "SECRET-NOTE")
}
