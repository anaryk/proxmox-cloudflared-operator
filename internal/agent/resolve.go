package agent

import (
	"cmp"
	"context"
	"fmt"
	"hash/fnv"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

const reasonBelowMinimum = "identity level %s is below the required %s"

// inventoryOf is what resolution reads of the cluster, made of the snapshot:
// the guests of the targets and the others it names, in the order of the
// inventory, and the nodes with their addresses. The controller sends a
// snapshot only from a complete inventory.
func inventoryOf(s agentproto.Snapshot) inventory.Snapshot {
	inv := inventory.Snapshot{Complete: true}
	seen := map[model.GuestRef]bool{}
	add := func(g model.Guest) {
		if !seen[g.Ref] {
			seen[g.Ref] = true
			inv.Guests = append(inv.Guests, g)
		}
	}
	for _, t := range s.Targets {
		if t.Guest != nil {
			add(*t.Guest)
		}
	}
	for _, g := range s.Guests {
		add(g)
	}
	slices.SortFunc(inv.Guests, func(a, b model.Guest) int { return model.CompareOwners(a.Ref.String(), b.Ref.String()) })
	for _, n := range s.Nodes {
		node := inventory.Node{Name: n.Name, Addr: n.Addr, Online: n.Online, Local: n.Local}
		if len(n.Addrs) > 0 {
			ifc := pve.NodeIface{}
			for _, addr := range n.Addrs {
				ifc.Addrs = append(ifc.Addrs, netip.PrefixFrom(addr, addr.BitLen()))
			}
			node.Ifaces = []pve.NodeIface{ifc}
		}
		inv.Nodes = append(inv.Nodes, node)
	}
	slices.SortFunc(inv.Nodes, func(a, b inventory.Node) int { return cmp.Compare(a.Name, b.Name) })
	return inv
}

// denylistOf is the denylist of the snapshot: the addresses of the nodes,
// which a manual route with allowNode reaches, those denied for a reason of
// their own, and the soft-denied ones.
func denylistOf(d agentproto.DenyList) (resolve.Denylist, error) {
	deny, err := resolve.NewDenylist(d.Denied, nil)
	if err != nil {
		return resolve.Denylist{}, err
	}
	reasons, err := addrMap(d.Reasons)
	if err != nil {
		return resolve.Denylist{}, err
	}
	soft, err := addrMap(d.Soft)
	if err != nil {
		return resolve.Denylist{}, err
	}
	return deny.WithDenied(reasons).WithSoft(soft), nil
}

func addrMap(m map[string]string) (map[netip.Addr]string, error) {
	out := make(map[netip.Addr]string, len(m))
	for k, v := range m {
		addr, err := netip.ParseAddr(k)
		if err != nil {
			return nil, fmt.Errorf("denylist entry %q is not an address", k)
		}
		out[addr] = v
	}
	return out, nil
}

// resolveAll resolves every target this node chooses the address of, with
// the stored binding of its hostname, at most resolveConcurrency at a time and
// each within resolveTimeout, so that one guest that reports many addresses
// does not hold up the others. The calls share one proof of each address of a
// guest's card, and a proof the watch of the network vouches for stands
// without being made again.
func (p *pass) resolveAll() {
	var targets []agentproto.Target
	for _, t := range p.s.Targets {
		if t.Resolve {
			targets = append(targets, t)
		}
	}
	p.a.vouch.see(p.inv, p.now)
	every := p.s.Settings.ReverifyInterval
	share := resolve.NewShared(func(b resolve.Binding) bool { return p.a.vouch.vouches(b, p.now, every) })
	out := make([]resolve.Result, len(targets))
	slots := make(chan struct{}, resolveConcurrency)
	var wg sync.WaitGroup
	started := 0
	for i, t := range targets {
		select {
		case slots <- struct{}{}:
		case <-p.ctx.Done():
		}
		if p.ctx.Err() != nil {
			break
		}
		var prev *resolve.Binding
		if b, ok := p.bindings[t.Route.Hostname]; ok {
			prev = &b
		}
		started++
		wg.Go(func() {
			defer func() { <-slots }()
			ctx, cancel := context.WithTimeout(p.ctx, resolveTimeout)
			defer cancel()
			out[i] = p.a.d.Resolver.Resolve(ctx, t.Route, p.inv, prev, p.deny, resolve.Level(t.Required), share)
		})
	}
	wg.Wait()
	for i, t := range targets[:started] {
		p.results[t.Key] = out[i]
		p.served[t.Key] = cloneResult(out[i])
	}
}

// cloneResult is a copy of res whose binding the pass may change.
func cloneResult(res resolve.Result) resolve.Result {
	if res.Binding != nil {
		b := *res.Binding
		res.Binding = &b
	}
	return res
}

// holdBelowMinimum takes the address from the served target of every guest
// whose address stands on a proof below the level its target requires, as the
// engine does: it is then no verified target. A route without a guest has no
// identity to prove and is never held back. The resolver's result stays as it
// was, for the controller to judge.
func (p *pass) holdBelowMinimum() {
	for _, t := range p.s.Targets {
		res, ok := p.served[t.Key]
		if !ok || t.Route.Guest == nil {
			continue
		}
		level, published := standsOn(res)
		required := resolve.Level(t.Required)
		if !published || level.AtLeast(required) {
			continue
		}
		why := fmt.Sprintf(reasonBelowMinimum, level, required)
		if res.Target.Withdrawn {
			res.Target.Addr = netip.Addr{}
		} else {
			res.Target = planner.ResolvedTarget{Reason: why}
		}
		p.served[t.Key] = res
		p.held[t.Key] = why
	}
}

// standsOn returns the level of the proof a target would be served on, and
// false when nothing would be served: no address, or one that must never be.
// A withdrawn address stands on its binding's last proof.
func standsOn(res resolve.Result) (resolve.Level, bool) {
	t := res.Target
	switch {
	case !t.Addr.IsValid() || t.Rejected:
		return "", false
	case !t.Withdrawn:
		return res.Level, true
	case res.Binding == nil:
		return "", false
	}
	return res.Binding.Proven(), true
}

// markLegs marks the target of every binding on a leg. Such a target is
// dialed at the service address the controller gave its address on that
// network, never at its own address: without one it is not served. A binding
// of the address its stored binding had through the same leg keeps the
// service address that was published under, so that the egress set can take
// it out by it.
func (p *pass) markLegs() {
	for _, t := range p.s.Targets {
		res, ok := p.served[t.Key]
		if !ok || res.Binding == nil {
			continue
		}
		b := res.Binding
		if b.Via == "" {
			b.Service = netip.Addr{}
			continue
		}
		if prev, bound := p.bindings[t.Route.Hostname]; bound && prev.Via == b.Via && prev.Addr == b.Addr {
			b.Service = prev.Service
		}
		res.Target.Leg, res.Target.Via = true, netip.Addr{}
		if t.Service.IsValid() && t.Addr == res.Target.Addr && serves(t, res) {
			res.Target.Via, b.Service = t.Service, t.Service
		}
		p.served[t.Key] = res
	}
}

// serves reports whether a rule would send a connector to the target.
func serves(t agentproto.Target, res resolve.Result) bool {
	rt := res.Target
	return rt.Addr.IsValid() && !rt.Withdrawn && !rt.Rejected && (rt.Owner == "" || rt.Owner == t.Route.Owner())
}

// saveBindings keeps the binding of every target this node resolved, and
// drops those of the routes no longer listed.
func (p *pass) saveBindings() {
	next := make(map[string]resolve.Binding, len(p.served))
	for _, t := range p.s.Targets {
		if res, ok := p.served[t.Key]; ok && res.Binding != nil {
			next[t.Route.Hostname] = *res.Binding
		}
	}
	if err := p.a.d.Store.SaveBindings(next); err != nil {
		p.problem("saving the bindings: %v", err)
	}
}

// setAddr is the address the egress set holds for the target of a binding:
// its service address for one on a leg, and its own address otherwise.
func setAddr(b resolve.Binding) netip.Addr {
	if b.Via == "" {
		return b.Addr
	}
	return b.Service
}

// vouching is what the agent knows of the watch of the network for the proofs
// of identity a pass may reuse: whether the watch runs, since when it has
// watched each address with the pin it has now, when it last reported an
// address moved, and since when each guest has run on the same node with the
// same configuration. It is safe for concurrent use.
type vouching struct {
	mu       sync.Mutex
	watching time.Time
	pinned   map[netip.Addr]pinnedSince
	fired    map[netip.Addr]time.Time
	guests   map[string]guestSeen
}

type pinnedSince struct {
	mac, bridge, port string
	since             time.Time
}

type guestSeen struct {
	digest, node     string
	running, unknown bool
	since            time.Time
}

// Watching tells the agent whether the watch of the network runs. Until it
// does, and again after it stopped, every pass proves every address anew.
//
// The daemon says so as it starts the watch, before the watch subscribed to
// the tables of the kernel. That is sound: the watch reads both tables whole
// when it subscribes, so what moved before then is reported then; and a proof
// stands only when the watch ran, and had the address pinned, before the
// proof was made.
func (a *Agent) Watching(on bool) {
	v := &a.vouch
	v.mu.Lock()
	defer v.mu.Unlock()
	if !on {
		v.watching = time.Time{}
		return
	}
	if v.watching.IsZero() {
		v.watching = a.d.Now()
	}
}

func (v *vouching) moved(addr netip.Addr, at time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.fired == nil {
		v.fired = make(map[netip.Addr]time.Time)
	}
	v.fired[addr] = at
}

// pin notes the pins the watch is given now. A pin that is the one the watch
// had keeps the time it got it; an address no longer pinned is forgotten,
// with its report.
func (v *vouching) pin(pins map[netip.Addr]egress.Pin, at time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	next := make(map[netip.Addr]pinnedSince, len(pins))
	for addr, p := range pins {
		cur := pinnedSince{mac: p.MAC.String(), bridge: p.Bridge, port: p.Port, since: at}
		if old, ok := v.pinned[addr]; ok && old.mac == cur.mac && old.bridge == cur.bridge && old.port == cur.port {
			cur.since = old.since
		}
		next[addr] = cur
	}
	for addr := range v.fired {
		if _, ok := next[addr]; !ok {
			delete(v.fired, addr)
		}
	}
	v.pinned = next
}

// see notes the guests of a pass that began at now.
func (v *vouching) see(snap inventory.Snapshot, now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	next := make(map[string]guestSeen, len(snap.Guests))
	for _, g := range snap.Guests {
		cur := guestSeen{digest: g.Digest, node: g.Node, running: g.Running, unknown: g.StatusUnknown, since: now}
		if old, ok := v.guests[g.Ref.String()]; ok &&
			old.digest == cur.digest && old.node == cur.node && old.running == cur.running && old.unknown == cur.unknown {
			cur.since = old.since
		}
		next[g.Ref.String()] = cur
	}
	v.guests = next
}

// vouches reports whether the proof b carries may stand at now without being
// made again: the watch ran when the proof was made and has not stopped
// since, it watched the address with the MAC of b from before then and has
// not reported it moved since, the guest has run on the same node with the
// same configuration from before then, and the proof is not due. Whatever
// happened at the very time of the proof counts as after it.
func (v *vouching) vouches(b resolve.Binding, now time.Time, every time.Duration) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	at := b.VerifiedAt
	key := setAddr(b)
	pin, pinned := v.pinned[key]
	g, seen := v.guests[b.Guest]
	fired, moved := v.fired[key]
	switch {
	case !key.IsValid() || every <= 0:
		return false
	case v.watching.IsZero() || !v.watching.Before(at):
		return false
	case !pinned || !pin.since.Before(at) || !sameHardware(pin.mac, b.MAC):
		return false
	case moved && !fired.Before(at):
		return false
	case !seen || g.since.After(at) || !g.running:
		return false
	}
	return !due(b.Addr, at, now, every)
}

// due reports whether a proof of addr made at at is to be made again at now:
// once the time of the address in each interval came between the two, a time
// the address takes from its hash, so that the proofs of one pass come due
// over a whole interval. One dated after now is due.
func due(addr netip.Addr, at, now time.Time, every time.Duration) bool {
	if now.Before(at) {
		return true
	}
	h := fnv.New64a()
	b := addr.As16()
	_, _ = h.Write(b[:])
	offset := int64(h.Sum64() % uint64(every))
	interval := func(t time.Time) int64 { return (t.UnixNano() - offset) / int64(every) }
	return interval(now) != interval(at)
}

func sameHardware(a, b string) bool {
	x, errA := net.ParseMAC(a)
	y, errB := net.ParseMAC(b)
	return errA == nil && errB == nil && x.String() == y.String()
}
