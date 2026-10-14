package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// agentDeps are the parts of the agent of this host node: besides the
// resolver, the connectors and the egress filter, qm and pct for the writes
// of guests' cards the controller hands it, and the gateway, which it drives
// from the networks of its desired state while the controller runs on
// another node. doctor makes the checks of the node a doctor action of the
// controller asks for, and probe answers its probe actions.
func agentDeps(res agent.Resolver, conns agent.Connectors, filter agent.Egress, gw *gatewayPart, probe nodeProbe,
	doctor func(context.Context) (json.RawMessage, error)) agent.Deps {
	run := setup.NewHostRunner()
	d := agent.Deps{
		Resolver: res, Connectors: conns, Egress: filter, Doctor: doctor,
		Runner: func(ctx context.Context, argv ...string) (string, error) {
			return run.Run(ctx, argv[0], argv[1:]...)
		},
	}
	if gw != nil {
		d.Gateway = agentGateway{gw}
		if probe.gw != nil && probe.prober != nil {
			d.Interfaces, d.ProbeAddr = probe.prober.Interfaces, probe.addr
		}
	}
	return d
}

// nodeProbe is what a probe action of the controller is answered from: the
// interfaces the prober sees and the gateway of the node, whose namespace
// the unit starts when no leg needed it yet.
type nodeProbe struct {
	gw     *gatewayPart
	prober resolve.Prober
	unit   Netns
}

// addr asks who answers for addr on a bridge and VLAN of the node, as the
// engine's probe does where it runs.
func (n nodeProbe) addr(ctx context.Context, bridge string, vlan int, addr netip.Addr) ([]net.HardwareAddr, error) {
	macs, err := n.gw.Probe(ctx, bridge, vlan, addr)
	if errors.Is(err, gateway.ErrNotPinned) && n.unit != nil {
		if serr := n.unit.Start(ctx); serr != nil {
			return nil, fmt.Errorf("starting the gateway namespace: %w", serr)
		}
		macs, err = n.gw.Probe(ctx, bridge, vlan, addr)
	}
	return macs, err
}

// agentGateway is the gateway as the agent drives it: what it is given is
// kept as the cache that pco netns up restores the namespace from at boot,
// before the guests start, as the engine keeps it on the node it runs on.
type agentGateway struct{ gw *gatewayPart }

func (a agentGateway) Sync(ctx context.Context, legs []gateway.Leg, services []gateway.Service, pins []gateway.Pin, sticky []gateway.Sticky) ([]gateway.LegState, error) {
	states, err := a.gw.Sync(ctx, legs, services, pins, sticky)
	if err != nil {
		return nil, err
	}
	c := gateway.Cache{Legs: legs, Services: services, Pins: pins, Sticky: sticky, PinsAt: a.gw.now()}
	if err := gateway.WriteCache(a.gw.local, c); err != nil {
		a.gw.log.Warn().Err(err).Msg("saving the cache of the gateway failed; a boot before it is saved brings the gateway back as it was before")
	}
	return states, nil
}
