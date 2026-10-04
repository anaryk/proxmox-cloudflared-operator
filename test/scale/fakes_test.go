//go:build scale

package scale

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

const (
	testNode = "pve1"
	gateTag  = "cf-tunnel"
	zoneName = "example.com"
)

var nodeAddr = netip.MustParseAddr("10.0.0.2")

// clock is the time of the engine and the resolver. It moves only when the
// benchmark moves it, so that grace periods pass without waiting.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// sleep is a wait of the limiter: the clock moves by it.
func (c *clock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.advance(d)
	return nil
}

// inventoryFake answers every refresh with the snapshot the benchmark set.
type inventoryFake struct {
	mu   sync.Mutex
	snap inventory.Snapshot
}

func (f *inventoryFake) Refresh(context.Context) inventory.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *inventoryFake) set(s inventory.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = s
}

// fleet is the guests of one benchmark. The first routes of them carry the
// gate tag and publish one hostname each; the others are plain guests that
// only take part in the identity checks.
type fleet struct {
	guests []model.Guest
	routes int
}

func newFleet(guests, routes int) *fleet {
	f := &fleet{guests: make([]model.Guest, guests), routes: routes}
	for i := range f.guests {
		f.guests[i] = f.guest(i, false)
	}
	return f
}

func vmid(i int) int { return 101 + i }

func guestMAC(i int) string {
	return fmt.Sprintf("bc:24:11:%02x:%02x:%02x", i>>16, (i>>8)&0xff, i&0xff)
}

func guestAddr(i int) netip.Addr {
	return netip.AddrFrom4([4]byte{10, 1, byte(i / 250), byte(i%250 + 1)})
}

func hostname(i int, renamed bool) string {
	if renamed {
		return fmt.Sprintf("r%d.%s", vmid(i), zoneName)
	}
	return fmt.Sprintf("g%d.%s", vmid(i), zoneName)
}

func (f *fleet) guest(i int, renamed bool) model.Guest {
	g := model.Guest{
		Ref:      model.GuestRef{Kind: model.KindQEMU, VMID: vmid(i)},
		Name:     fmt.Sprintf("vm-%d", vmid(i)),
		Node:     testNode,
		Running:  true,
		Identity: fmt.Sprintf("uuid:%d", vmid(i)),
		NICs:     []model.NIC{{Index: 0, MAC: guestMAC(i), Bridge: "vmbr0", Static: []netip.Addr{guestAddr(i)}}},
	}
	if i < f.routes {
		g.Tags = []string{gateTag}
		g.Description = "```cf-tunnel\n" + hostname(i, renamed) + " -> :8080\n```"
	}
	return g
}

// snapshot lists the guests that are not in gone, with the hostnames of
// renamed.
func (f *fleet) snapshot(renamed, gone map[int]bool) inventory.Snapshot {
	guests := make([]model.Guest, 0, len(f.guests))
	for i, g := range f.guests {
		switch {
		case gone[i]:
		case renamed[i]:
			guests = append(guests, f.guest(i, true))
		default:
			guests = append(guests, g)
		}
	}
	return inventory.Snapshot{
		Guests:   guests,
		Nodes:    []inventory.Node{{Name: testNode, Addr: nodeAddr, Online: true, Local: true}},
		Complete: true,
	}
}

// prober answers the resolver as a host would in which every guest owns its
// address on vmbr0 and every MAC is learned on the port of its own guest.
// The latency of an ARP exchange can be set: the real prober waits out an
// ARP window of 600 ms for every address it asks for.
type prober struct {
	arp   atomic.Int64 // nanoseconds
	owner map[netip.Addr]string
	port  map[string]string
	calls atomic.Int64
}

func newProber(f *fleet) *prober {
	p := &prober{owner: make(map[netip.Addr]string, len(f.guests)), port: make(map[string]string, len(f.guests))}
	for i := range f.guests {
		p.owner[guestAddr(i)] = guestMAC(i)
		p.port[guestMAC(i)] = fmt.Sprintf("tap%di0", vmid(i))
	}
	return p
}

func (p *prober) setARPLatency(d time.Duration) { p.arp.Store(int64(d)) }

func (p *prober) Interfaces(context.Context) ([]resolve.HostIface, error) {
	p.calls.Add(1)
	return []resolve.HostIface{{Name: "vmbr0", Addrs: []netip.Prefix{netip.MustParsePrefix("10.0.0.2/8")}}}, nil
}

func (p *prober) Route(context.Context, netip.Addr) (string, bool, error) {
	p.calls.Add(1)
	return "vmbr0", true, nil
}

func (p *prober) ARP(ctx context.Context, _ string, addr netip.Addr) ([]string, error) {
	p.calls.Add(1)
	if d := time.Duration(p.arp.Load()); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []string{p.owner[addr]}, nil
}

func (p *prober) FDBPorts(_ context.Context, _ string, _ int, mac string) ([]string, error) {
	p.calls.Add(1)
	return []string{p.port[mac]}, nil
}

func (p *prober) Dial(context.Context, netip.AddrPort) error {
	p.calls.Add(1)
	return nil
}

// connectors keeps the tokens it was given and says every connector is ready.
type connectors struct {
	mu     sync.Mutex
	tokens map[string]string
}

func (c *connectors) Ensure(_ context.Context, _, id, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[id] = token
	return nil
}

func (c *connectors) PruneInstall(context.Context, string, []string) error { return nil }

func (c *connectors) List(context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(maps.Keys(c.tokens)), nil
}

func (c *connectors) Status(_ context.Context, id string) (connector.Status, error) {
	return connector.Status{TunnelID: id, Active: true, Ready: true, Connections: 4, MetricsAddr: "127.0.0.1:20300", Install: testInstall}, nil
}

func (c *connectors) Token(id string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tokens[id]
	return t, ok, nil
}

// egressFake accepts every set of targets.
type egressFake struct{}

func (egressFake) Set(context.Context, []egress.Target) error { return nil }
func (egressFake) Remove(context.Context, netip.Addr) error   { return nil }
