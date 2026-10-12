package apifake

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Networks returns the networks of the state.
func (e *Engine) Networks() ([]engine.NetworkView, error) {
	if err := e.begin(context.Background(), "Networks", noArgs); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.state.Networks), nil
}

// Network returns a network of the state, with the routes of the state that
// go through it; the scenario keeps no service addresses.
func (e *Engine) Network(id string) (engine.NetworkDetail, error) {
	if err := e.begin(context.Background(), "Network", idArgs{id}); err != nil {
		return engine.NetworkDetail{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	i := slices.IndexFunc(e.state.Networks, func(v engine.NetworkView) bool { return v.ID == id })
	if i < 0 {
		return engine.NetworkDetail{}, fmt.Errorf("%w: network %s is not attached", engine.ErrNotFound, id)
	}
	return engine.NetworkDetail{NetworkView: e.state.Networks[i], Services: []store.ServiceAddr{}, Served: e.servedThrough(id)}, nil
}

type idArgs struct {
	ID string `json:"id"`
}

// servedThrough are the hostnames of the routes of the state whose path goes
// through a network. The caller holds mu.
func (e *Engine) servedThrough(id string) []string {
	out := []string{}
	for _, r := range e.state.Routes {
		if r.Path != nil && r.Path.Via == engine.ViaAttach+id {
			out = append(out, r.Hostname)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// CandidateNetworks lists the networks of the state, each attached: the
// scenario knows no other bridge.
func (e *Engine) CandidateNetworks(ctx context.Context) ([]engine.CandidateNetwork, error) {
	if err := e.begin(ctx, "CandidateNetworks", noArgs); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]engine.CandidateNetwork, 0, len(e.state.Networks))
	for _, n := range e.state.Networks {
		out = append(out, engine.CandidateNetwork{
			Bridge: n.Bridge, VLAN: n.VLAN, Uplink: n.Uplink, Attached: n.ID, Guests: []engine.GuestAddr{}, Prefixes: []netip.Prefix{n.Prefix},
		})
	}
	return out, nil
}

// ProbeNetwork finds every address free: nobody answers in a scenario. The
// prefix defaults to that of the network on the bridge, the address to the
// highest of the prefix.
func (e *Engine) ProbeNetwork(ctx context.Context, r engine.ProbeRequest) (engine.ProbeResult, error) {
	if err := e.begin(ctx, "ProbeNetwork", r); err != nil {
		return engine.ProbeResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	res := engine.ProbeResult{Bridge: r.Bridge, VLAN: r.VLAN, Prefix: r.Prefix, Address: r.Address, Free: true, AnsweredBy: []string{}}
	if !res.Prefix.IsValid() {
		i := slices.IndexFunc(e.state.Networks, func(v engine.NetworkView) bool { return v.Bridge == r.Bridge && v.VLAN == r.VLAN })
		if i < 0 {
			return engine.ProbeResult{}, &engine.FieldError{Field: "prefix", Err: fmt.Errorf("no guest address is known on %s; give the prefix of the network", r.Bridge)}
		}
		res.Prefix = e.state.Networks[i].Prefix
	}
	if !res.Address.IsValid() {
		res.Address = highest(res.Prefix)
	}
	if !res.Prefix.Contains(res.Address) {
		return engine.ProbeResult{}, &engine.FieldError{Field: "address", Err: fmt.Errorf("address %s is not in %s", res.Address, res.Prefix)}
	}
	return res, nil
}

// highest is the address below the broadcast address of an IPv4 prefix.
func highest(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr()
	for next := a.Next(); p.Contains(next); next = next.Next() {
		a = next
	}
	return a.Prev()
}

// Attach adds a network to the state, with its leg up and nothing served
// through it yet.
func (e *Engine) Attach(ctx context.Context, r engine.AttachRequest) (engine.NetworkView, error) {
	if err := e.begin(ctx, "Attach", r); err != nil {
		return engine.NetworkView{}, err
	}
	switch {
	case r.Bridge == "":
		return engine.NetworkView{}, &engine.FieldError{Field: "bridge", Err: fmt.Errorf("bridge: name a bridge or a vnet")}
	case !r.Prefix.IsValid() || !r.Prefix.Addr().Is4() || r.Prefix != r.Prefix.Masked():
		return engine.NetworkView{}, &engine.FieldError{Field: "prefix", Err: fmt.Errorf("prefix %s: want the IPv4 address of a network", r.Prefix)}
	case !r.Prefix.Contains(r.Address):
		return engine.NetworkView{}, &engine.FieldError{Field: "address", Err: fmt.Errorf("address %s is not in %s", r.Address, r.Prefix)}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	id := store.SegmentID(r.Bridge, r.VLAN)
	used := map[int]bool{}
	for _, n := range e.state.Networks {
		if n.ID == id {
			return engine.NetworkView{}, fmt.Errorf("%w: network %s is attached already; detach it first to attach it anew", engine.ErrRefused, id)
		}
		used[n.Mark] = true
	}
	mark := 1
	for used[mark] {
		mark++
	}
	if mark > gateway.MaxMark {
		return engine.NetworkView{}, fmt.Errorf("%w: this node reaches %d networks already, the most it can", engine.ErrRefused, gateway.MaxMark)
	}
	v := engine.NetworkView{
		Network: store.Network{
			ID: id, Kind: store.NetworkAttach, Bridge: r.Bridge, VLAN: r.VLAN, Prefix: r.Prefix,
			Legs: map[string]netip.Addr{e.state.Node: r.Address}, LegMAC: store.LegMACFor(store.NetworkAttach, mark), Mark: mark,
			Uplink: r.ConfirmUplink, OverlapOK: r.OverlapOK, CreatedAt: e.now(), Rev: 1,
		},
		Legs: []engine.LegView{{Node: e.state.Node, Addr: r.Address, Up: true}},
	}
	nets := append(slices.Clone(e.state.Networks), v)
	slices.SortFunc(nets, func(a, b engine.NetworkView) int { return cmp.Compare(a.ID, b.ID) })
	e.state.Networks = nets
	e.networkEvent(ctx, id, fmt.Sprintf("network %s is attached: the leg %s with %s on %s, mark %d; the next cycle raises the leg",
		id, gateway.LegIface(mark), netip.PrefixFrom(r.Address, r.Prefix.Bits()), engine.SegmentArg(r.Bridge, r.VLAN), mark))
	e.changed()
	return v, nil
}

type detachArgs struct {
	ID     string   `json:"id"`
	Rev    int64    `json:"rev"`
	Served []string `json:"served"`
}

// Detach takes a network out of the state, at the revision it was read at;
// one that routes of the state go through only when they are named.
func (e *Engine) Detach(ctx context.Context, id string, rev int64, served []string) error {
	if err := e.begin(ctx, "Detach", detachArgs{id, rev, served}); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	i := slices.IndexFunc(e.state.Networks, func(v engine.NetworkView) bool { return v.ID == id })
	switch {
	case i < 0:
		return fmt.Errorf("%w: network %s is not attached", engine.ErrNotFound, id)
	case e.state.Networks[i].Rev != rev:
		return fmt.Errorf("%w: network %s changed since it was read at revision %d and is at revision %d now; read it again",
			engine.ErrRefused, id, rev, e.state.Networks[i].Rev)
	}
	now := e.servedThrough(id)
	if len(now) > 0 && !slices.Equal(now, slices.Compact(slices.Sorted(slices.Values(served)))) {
		return fmt.Errorf("%w: network %s serves routes through its leg: %s; name them to detach it all the same",
			engine.ErrRefused, id, strings.Join(now, ", "))
	}
	e.state.Networks = slices.Delete(slices.Clone(e.state.Networks), i, i+1)
	e.networkEvent(ctx, id, fmt.Sprintf("network %s is detached: its leg is removed", id))
	e.changed()
	return nil
}

// networkEvent records an admin action on a network, with who asked for it.
// The caller holds mu.
func (e *Engine) networkEvent(ctx context.Context, id, msg string) {
	e.emit(engine.Event{Level: "info", Kind: "network", Subject: id, Message: msg, Actor: engine.ActorOf(ctx)})
}
