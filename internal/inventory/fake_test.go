package inventory

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sync"
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
	moved        map[model.GuestRef]string // guests that live elsewhere than their resource row says
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
		moved:       map[model.GuestRef]string{},
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
	f.update(ref, func(row *pve.Resource) { row.Status = status })
}

func (f *fakeSource) update(ref model.GuestRef, change func(row *pve.Resource)) {
	for i := range f.resources {
		if (model.GuestRef{Kind: f.resources[i].Kind, VMID: f.resources[i].VMID}) == ref {
			change(&f.resources[i])
		}
	}
}

func (f *fakeSource) setOnline(node string, online bool) {
	for i := range f.nodes {
		if f.nodes[i].Name == node {
			f.nodes[i].Online = online
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
	want := f.nodeOf(ref)
	if actual, ok := f.moved[ref]; ok {
		want = actual
		if node != actual {
			return pve.GuestConfig{}, notFoundErr()
		}
	}
	if want != node {
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
