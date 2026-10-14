package daemon

import (
	"context"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// agentDeps are the parts of the agent of this host node: besides the
// resolver, the connectors and the egress filter, qm and pct for the writes
// of guests' cards the controller hands it, and the gateway, which it drives
// from the networks of its desired state while the controller runs on
// another node.
func agentDeps(res agent.Resolver, conns agent.Connectors, filter agent.Egress, gw *gatewayPart) agent.Deps {
	run := setup.NewHostRunner()
	d := agent.Deps{
		Resolver: res, Connectors: conns, Egress: filter,
		Runner: func(ctx context.Context, argv ...string) (string, error) {
			return run.Run(ctx, argv[0], argv[1:]...)
		},
	}
	if gw != nil {
		d.Gateway = agentGateway{gw}
	}
	return d
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
