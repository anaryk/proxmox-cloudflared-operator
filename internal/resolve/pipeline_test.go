package resolve

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

var (
	pipelineZone   = planner.Zone{ID: "z-example", Name: "example.com", AccountID: "acc1", CredentialID: "cred1"}
	pipelineWriter = planner.Writer{InstallID: "a1b2c3", Generation: 1, Nonce: "n0nce1"}
	errSocket      = errors.New("socket closed")
)

// pipelineSource is the Proxmox API as the inventory reads it. Its fields
// change between refreshes, never during one.
type pipelineSource struct {
	rows      []pve.Resource
	configs   map[model.GuestRef]map[string]string
	configErr map[model.GuestRef]error
	nodes     []pve.ClusterNode
}

var _ inventory.Source = (*pipelineSource)(nil)

func newPipelineSource() *pipelineSource {
	return &pipelineSource{
		configs:   map[model.GuestRef]map[string]string{},
		configErr: map[model.GuestRef]error{},
		nodes: []pve.ClusterNode{
			{Name: "pve1", Addr: ip("10.20.0.2"), Online: true, Local: true},
			{Name: "pve2", Addr: ip("10.20.0.3"), Online: true},
		},
	}
}

// addGuest lists a running guest tagged for the tunnel whose Notes route host
// to port.
func (s *pipelineSource) addGuest(ref model.GuestRef, node, host, port string, values map[string]string) {
	s.rows = append(s.rows, pve.Resource{Kind: ref.Kind, VMID: ref.VMID, Name: ref.String(), Node: node, Status: "running", Tags: []string{"cf-tunnel"}})
	values["description"] = "```cf-tunnel\n" + host + " -> :" + port + "\n```"
	s.configs[ref] = values
}

func (s *pipelineSource) setStatus(ref model.GuestRef, status string) {
	for i := range s.rows {
		if s.rows[i].Kind == ref.Kind && s.rows[i].VMID == ref.VMID {
			s.rows[i].Status = status
		}
	}
}

func (s *pipelineSource) setOnline(node string, online bool) {
	for i := range s.nodes {
		if s.nodes[i].Name == node {
			s.nodes[i].Online = online
		}
	}
}

func (s *pipelineSource) Resources(context.Context) ([]pve.Resource, error) {
	return slices.Clone(s.rows), nil
}

func (s *pipelineSource) GuestConfig(_ context.Context, _ string, ref model.GuestRef) (pve.GuestConfig, error) {
	if err := s.configErr[ref]; err != nil {
		return pve.GuestConfig{}, err
	}
	return pve.GuestConfig{Values: maps.Clone(s.configs[ref])}, nil
}

func (s *pipelineSource) AgentInterfaces(context.Context, string, int) ([]pve.GuestIface, error) {
	return nil, nil
}

func (s *pipelineSource) LXCInterfaces(context.Context, string, int) ([]pve.GuestIface, error) {
	return nil, nil
}

func (s *pipelineSource) ClusterNodes(context.Context) ([]pve.ClusterNode, error) {
	return slices.Clone(s.nodes), nil
}

func (s *pipelineSource) NodeNetwork(context.Context, string) ([]pve.NodeIface, error) {
	return nil, nil
}

// pipeline runs the inventory, the resolver and the planner the way the
// operator does on pve1, keeping bindings and claims between cycles.
type pipeline struct {
	src      *pipelineSource
	prober   *fakeProber
	clock    *clock
	inv      *inventory.Inventory
	resolver *Resolver
	bindings map[string]*Binding // by hostname
	claims   map[string]planner.Claim
}

func newPipeline(src *pipelineSource, prober *fakeProber) *pipeline {
	clk := &clock{t: t0}
	return &pipeline{
		src:      src,
		prober:   prober,
		clock:    clk,
		inv:      inventory.New(src, inventory.Options{}, clk.now, zerolog.Nop()),
		resolver: NewResolver(prober, Settings{LocalNode: "pve1"}, clk.now),
		bindings: map[string]*Binding{},
	}
}

// cycle is what one run of the pipeline produced.
type cycle struct {
	snap    inventory.Snapshot
	results map[string]Result // by hostname
	plan    planner.Plan
}

func (c cycle) status(t *testing.T, host string) planner.RouteStatus {
	t.Helper()
	i := slices.IndexFunc(c.plan.Routes, func(st planner.RouteStatus) bool { return st.Hostname == host })
	require.GreaterOrEqual(t, i, 0, "no status for %s", host)
	return c.plan.Routes[i]
}

func (c cycle) rule(t *testing.T, host string) planner.IngressRule {
	t.Helper()
	require.Len(t, c.plan.Tunnels, 1)
	rules := c.plan.Tunnels[0].Rules
	i := slices.IndexFunc(rules, func(r planner.IngressRule) bool { return r.Hostname == host })
	require.GreaterOrEqual(t, i, 0, "no rule for %s", host)
	return rules[i]
}

func (c cycle) hasRecord(host string) bool {
	return slices.ContainsFunc(c.plan.Records, func(r planner.RecordPlan) bool { return r.Name == host })
}

// requireActive asserts that host is served on addr:port.
func (c cycle) requireActive(t *testing.T, host, target string) {
	t.Helper()
	st := c.status(t, host)
	require.Equal(t, planner.StateActive, st.State, st.Reason)
	require.Equal(t, "http://"+target, st.Service)
	require.Equal(t, planner.IngressRule{Hostname: host, Service: "http://" + target}, c.rule(t, host))
	require.True(t, c.hasRecord(host))
}

// requireBlocked asserts that host is not served, with the state and reason
// given, and whether its DNS record is kept. A kept record points at a
// tunnel that answers 503 for host.
func (c cycle) requireBlocked(t *testing.T, host string, state planner.RouteState, reason string, record bool) {
	t.Helper()
	st := c.status(t, host)
	require.Equal(t, state, st.State)
	require.Equal(t, reason, st.Reason)
	require.Empty(t, st.Service)
	for _, tunnel := range c.plan.Tunnels {
		for _, r := range tunnel.Rules {
			if r.Hostname == host {
				require.Equal(t, "http_status:503", r.Service)
			}
		}
	}
	require.Equal(t, record, c.hasRecord(host))
	if record {
		require.Equal(t, planner.IngressRule{Hostname: host, Service: "http_status:503"}, c.rule(t, host))
	}
}

func (p *pipeline) advance(d time.Duration) { p.clock.t = p.clock.t.Add(d) }

// restart starts the inventory and the resolver afresh, as a restarted
// operator does; only the stored bindings and claims survive.
func (p *pipeline) restart() {
	p.inv = inventory.New(p.src, inventory.Options{}, p.clock.now, zerolog.Nop())
	p.resolver = NewResolver(p.prober, Settings{LocalNode: "pve1"}, p.clock.now)
}

func (p *pipeline) run(t *testing.T) cycle {
	t.Helper()
	snap := p.inv.Refresh(t.Context())
	col := planner.Collect(snap.Guests, nil, planner.Settings{})
	deny, err := NewDenylist(snap.NodeAddrs(), nil)
	require.NoError(t, err)

	results := make(map[string]Result, len(col.Routes))
	targets := make(map[string]planner.ResolvedTarget, len(col.Routes))
	for _, route := range col.Routes {
		res := p.resolver.Resolve(t.Context(), route, snap, p.bindings[route.Hostname], deny, LevelPort, nil)
		results[route.Hostname], targets[route.Hostname] = res, res.Target
		p.bindings[route.Hostname] = nil
		if res.Binding != nil {
			p.bindings[route.Hostname] = throughJSON(t, res.Binding)
		}
	}

	identity := make(map[string]string, len(snap.Guests))
	for _, g := range snap.Guests {
		identity[g.Ref.String()] = g.Identity
	}
	claims := planner.ResolveClaims(planner.ClaimInput{
		Routes: col.Routes, Held: col.Held, Claims: p.claims, Identity: identity, Now: p.clock.t, Grace: time.Minute,
	})
	p.claims = claims.Claims
	plan := planner.Build(planner.BuildInput{
		Winners:   claims.Winners,
		Conflicts: claims.Conflicts,
		Claims:    claims.Claims,
		Targets:   targets,
		Zones:     []planner.Zone{pipelineZone},
		Writer:    pipelineWriter,
	})
	return cycle{snap: snap, results: results, plan: plan}
}

const webMAC = "bc:24:11:00:01:01"

// webPipeline has web-1 (qemu/101) on pve1 with net0 tagged 30 behind the
// Proxmox firewall and the static address 10.30.0.11, which pve1 reaches on
// vmbr0.30. Its MAC is learned on the firewall bridge port fwpr101p0.
func webPipeline() (*pipeline, model.GuestRef) {
	ref := model.GuestRef{Kind: model.KindQEMU, VMID: 101}
	src := newPipelineSource()
	src.addGuest(ref, "pve1", "web.example.com", "8080", map[string]string{
		"name":      "web-1",
		"net0":      "virtio=BC:24:11:00:01:01,bridge=vmbr0,tag=30,firewall=1",
		"ipconfig0": "ip=10.30.0.11/24,gw=10.30.0.1",
		"smbios1":   "uuid=6a1f3c2e-0b7d-4e59-9f21-3c4d5e6f7a8b",
	})
	p := newFakeProber()
	p.ifaces = []HostIface{
		{Name: "vmbr0", Addrs: prefixes("10.20.0.2/24")},
		{Name: "vmbr0.30", Addrs: prefixes("10.30.0.2/24")},
	}
	p.arp[arpKey("vmbr0.30", "10.30.0.11")] = []string{webMAC}
	p.fdb[fdbKey("vmbr0", 30, webMAC)] = []string{"fwpr101p0"}
	return newPipeline(src, p), ref
}

func TestPipelineFirewalledGuestOnAVLAN(t *testing.T) {
	p, _ := webPipeline()

	c := p.run(t)

	require.True(t, c.snap.Complete)
	require.Empty(t, c.snap.Problems)
	c.requireActive(t, "web.example.com", "10.30.0.11:8080")
	require.Equal(t, &Binding{
		Owner: "qemu/101", Hostname: "web.example.com", Guest: "qemu/101",
		Addr: ip("10.30.0.11"), MAC: webMAC, VerifiedAt: t0, Level: LevelPort,
		Since: t0, Bridge: "vmbr0", Port: "fwpr101p0", Ports: map[string]string{webMAC: "fwpr101p0"},
		Segment: Segment{Bridge: "vmbr0", VLAN: 30},
	}, p.bindings["web.example.com"], "the firewall bridge's port on the VLAN-aware bridge")
	require.Equal(t, []probeCall{
		{op: "interfaces"},
		{op: "route", addr: ip("10.30.0.11")},
		{op: "arp", iface: "vmbr0.30", addr: ip("10.30.0.11")},
		{op: "fdb", iface: "vmbr0", vlan: 30, mac: webMAC},
		{op: "dial", addr: ip("10.30.0.11"), port: 8080},
	}, p.prober.calls)
}

func TestPipelineUnknownStatusKeepsServingWhileTheWireProvesIt(t *testing.T) {
	p, ref := webPipeline()
	p.run(t)
	p.src.setStatus(ref, "unknown")

	for range 8 {
		p.advance(time.Minute)

		c := p.run(t)

		require.True(t, c.snap.Complete)
		require.Equal(t, []string{"status of 1 guests is unknown; using last known state"}, c.snap.Problems)
		c.requireActive(t, "web.example.com", "10.30.0.11:8080")
		require.Equal(t, p.clock.t, p.bindings["web.example.com"].VerifiedAt, "each cycle proves the address anew")
	}

	delete(p.prober.arp, arpKey("vmbr0.30", "10.30.0.11"))
	p.advance(time.Minute)

	c := p.run(t)

	c.requireBlocked(t, "web.example.com", planner.StateWithdrawn, "no ARP answer on vmbr0.30", true)
}

func TestPipelineRestartWhileTheStatusIsUnknown(t *testing.T) {
	p, ref := webPipeline()
	p.run(t)
	proven := p.clock.t
	p.src.setStatus(ref, "unknown")
	p.restart()
	p.prober.calls = nil

	for _, at := range []time.Duration{10 * time.Second, 5 * time.Minute} {
		p.clock.t = proven.Add(at)

		c := p.run(t)

		require.True(t, c.snap.Complete)
		g := c.snap.Guests[0]
		require.True(t, g.StatusUnknown, "nothing was read before the restart")
		require.False(t, g.Running)
		st := c.status(t, "web.example.com")
		require.Equal(t, planner.StateUnreachable, st.State, "after %s", at)
		require.Equal(t, "guest state unknown", st.Reason)
		require.Equal(t, planner.IngressRule{Hostname: "web.example.com", Service: "http://10.30.0.11:8080"}, c.rule(t, "web.example.com"))
		require.Equal(t, proven, p.bindings["web.example.com"].VerifiedAt, "an unknown state proves nothing")
	}
	require.Empty(t, p.prober.calls, "a guest whose state is unknown is not probed")

	p.clock.t = proven.Add(5*time.Minute + time.Second)

	c := p.run(t)

	c.requireBlocked(t, "web.example.com", planner.StateWithdrawn, "identity not confirmed for 5m1s", true)

	p.src.setStatus(ref, "running")
	p.advance(10 * time.Second)

	c = p.run(t)

	c.requireActive(t, "web.example.com", "10.30.0.11:8080")
	require.Equal(t, p.clock.t, p.bindings["web.example.com"].VerifiedAt)
}

func TestPipelineGuestOnAnOfflineNode(t *testing.T) {
	const appMAC = "bc:24:11:00:02:01"
	ref := model.GuestRef{Kind: model.KindLXC, VMID: 201}
	src := newPipelineSource()
	src.addGuest(ref, "pve2", "app.example.com", "3000", map[string]string{
		"hostname": "app-1",
		"net0":     "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:02:01,ip=10.20.0.31/24",
	})
	prober := newFakeProber()
	prober.ifaces = []HostIface{{Name: "vmbr0", Addrs: prefixes("10.20.0.2/24")}}
	prober.arp[arpKey("vmbr0", "10.20.0.31")] = []string{appMAC}
	prober.fdb[fdbKey("vmbr0", 0, appMAC)] = []string{"eno1"}
	p := newPipeline(src, prober)

	c := p.run(t)
	c.requireActive(t, "app.example.com", "10.20.0.31:3000")

	// Proxmox reports the guests of a node it cannot reach as unknown.
	src.setOnline("pve2", false)
	src.setStatus(ref, "unknown")
	p.advance(10 * time.Second)
	proven := p.clock.t

	c = p.run(t)

	require.True(t, c.snap.Complete)
	require.Equal(t, []string{
		"node pve2 is offline; using cached data for its guests",
		"status of 1 guests is unknown; using last known state",
	}, c.snap.Problems)
	c.requireActive(t, "app.example.com", "10.20.0.31:3000")
	require.Equal(t, proven, p.bindings["app.example.com"].VerifiedAt)

	// The host can no longer check the address; it stays served on the last
	// proof until that proof is MaxProofAge old.
	prober.arpErr[arpKey("vmbr0", "10.20.0.31")] = errSocket
	for _, at := range []time.Duration{2 * time.Minute, 5 * time.Minute} {
		p.clock.t = proven.Add(at)

		c = p.run(t)

		st := c.status(t, "app.example.com")
		require.Equal(t, planner.StateUnreachable, st.State, "after %s", at)
		require.Equal(t, "ARP on vmbr0: socket closed", st.Reason)
		require.Equal(t, planner.IngressRule{Hostname: "app.example.com", Service: "http://10.20.0.31:3000"}, c.rule(t, "app.example.com"))
	}

	p.clock.t = proven.Add(5*time.Minute + time.Second)

	c = p.run(t)

	c.requireBlocked(t, "app.example.com", planner.StateWithdrawn, "identity not confirmed for 5m1s", true)
}

func TestPipelineFailedConfigFetchKeepsTheBinding(t *testing.T) {
	p, ref := webPipeline()
	p.run(t)
	p.src.configErr[ref] = errors.New("proxmox api: HTTP 500: internal error")
	p.advance(10 * time.Second)

	c := p.run(t)

	require.False(t, c.snap.Complete)
	require.Len(t, c.snap.Problems, 1)
	require.Contains(t, c.snap.Problems[0], "config not refreshed")
	c.requireActive(t, "web.example.com", "10.30.0.11:8080")
	b := p.bindings["web.example.com"]
	require.Equal(t, webMAC, b.MAC)
	require.Equal(t, p.clock.t, b.VerifiedAt, "the cached guest is still checked on the wire")
}

func TestPipelineReusedVMIDDoesNotInheritTheBinding(t *testing.T) {
	const oldMAC, newMAC = "bc:24:11:00:03:01", "bc:24:11:00:03:02"
	ref := model.GuestRef{Kind: model.KindQEMU, VMID: 102}
	src := newPipelineSource()
	src.addGuest(ref, "pve1", "db.example.com", "5432", map[string]string{
		"name":      "db-1",
		"net0":      "virtio=BC:24:11:00:03:01,bridge=vmbr0",
		"ipconfig0": "ip=10.20.0.40/24",
		"smbios1":   "uuid=0f1e2d3c-4b5a-4968-8776-a5b4c3d2e1f0",
	})
	prober := newFakeProber()
	prober.ifaces = []HostIface{{Name: "vmbr0", Addrs: prefixes("10.20.0.2/24")}}
	prober.arp[arpKey("vmbr0", "10.20.0.40")] = []string{oldMAC}
	prober.fdb[fdbKey("vmbr0", 0, oldMAC)] = []string{"tap102i0"}
	p := newPipeline(src, prober)

	c := p.run(t)
	c.requireActive(t, "db.example.com", "10.20.0.40:5432")
	oldIdentity := c.snap.Guests[0].Identity

	// The guest is destroyed and another one is created under the same VMID,
	// with the same Notes and address, while the host cannot check it yet.
	src.configs[ref]["smbios1"] = "uuid=9a8b7c6d-5e4f-4321-8fed-cba987654321"
	src.configs[ref]["net0"] = "virtio=BC:24:11:00:03:02,bridge=vmbr0"
	prober.arpErr[arpKey("vmbr0", "10.20.0.40")] = errSocket
	p.advance(10 * time.Second)

	c = p.run(t)

	require.NotEqual(t, oldIdentity, c.snap.Guests[0].Identity)
	res := c.results["db.example.com"]
	require.Equal(t, planner.ResolvedTarget{Reason: "ARP on vmbr0: socket closed"}, res.Target, "the old proof does not serve the new guest")
	require.Nil(t, res.Binding)
	require.Nil(t, p.bindings["db.example.com"])
	c.requireBlocked(t, "db.example.com", planner.StateUnreachable, "ARP on vmbr0: socket closed", false)

	delete(prober.arpErr, arpKey("vmbr0", "10.20.0.40"))
	prober.arp[arpKey("vmbr0", "10.20.0.40")] = []string{newMAC}
	prober.fdb[fdbKey("vmbr0", 0, newMAC)] = []string{"tap102i0"}
	p.advance(10 * time.Second)

	c = p.run(t)

	c.requireActive(t, "db.example.com", "10.20.0.40:5432")
	require.Equal(t, newMAC, p.bindings["db.example.com"].MAC)
	require.Equal(t, p.clock.t, p.bindings["db.example.com"].VerifiedAt)
}
