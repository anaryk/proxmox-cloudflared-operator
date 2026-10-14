package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// guestPorts are the prefixes of the ports a bridge has for the guests: the
// controller judges a bridge by its other ports, so these are left out of a
// probe's answer.
var guestPorts = []string{"tap", "veth", "fwpr", "fwln"}

// probe answers a probe action of the controller: the interfaces of this
// node, of which the controller judges whether the bridge is here and a
// leg may go on it, and who answers for the address when the probe names
// one. It is bounded by probeTimeout. The agent decides nothing here: the
// controller reads the answer as facts of this node alone.
func (a *Agent) probe(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if a.d.Kind != pki.KindHost {
		return nil, errors.New("an appliance agent has no gateway of its own; the controller's appliance carries the legs")
	}
	if a.d.Interfaces == nil || a.d.ProbeAddr == nil {
		return nil, errors.New("this node runs no gateway, so it probes nothing")
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	var p agentproto.Probe
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("reading the probe: %w", err)
	}
	if err := p.Check(); err != nil {
		return nil, err
	}
	ctx, cancel := a.timeout(ctx, probeTimeout)
	defer cancel()
	ifaces, err := a.d.Interfaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the interfaces of the node: %w", err)
	}
	answer := agentproto.ProbeAnswer{Interfaces: probeIfaces(ifaces), AnsweredBy: []string{}}
	if p.Addr.IsValid() {
		macs, err := a.d.ProbeAddr(ctx, p.Bridge, p.VLAN, p.Addr)
		if err != nil {
			return nil, fmt.Errorf("probing %s on %s: %w", p.Addr, resolve.Segment{Bridge: p.Bridge, VLAN: p.VLAN}, err)
		}
		for _, m := range macs {
			answer.AnsweredBy = append(answer.AnsweredBy, m.String())
		}
		slices.Sort(answer.AnsweredBy)
		answer.AnsweredBy = slices.Compact(answer.AnsweredBy)
	}
	// An answer cut short could leave out an uplink or an address of the
	// node, which the controller refuses a leg for: none is sent instead.
	if err := answer.Check(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(answer)
	if err != nil {
		return nil, fmt.Errorf("writing the answer: %w", err)
	}
	if len(data) > agentproto.MaxProbeAnswerBytes {
		return nil, fmt.Errorf("the answer is %d bytes, over the %d the controller reads", len(data), agentproto.MaxProbeAnswerBytes)
	}
	return data, nil
}

// probeIfaces are the interfaces of the node a probe answers with: all but
// the ports of the guests, each with its IPv4 addresses.
func probeIfaces(ifaces []resolve.HostIface) []agentproto.ProbeIface {
	out := []agentproto.ProbeIface{}
	for _, h := range ifaces {
		if slices.ContainsFunc(guestPorts, func(p string) bool { return strings.HasPrefix(h.Name, p) }) {
			continue
		}
		i := agentproto.ProbeIface{Name: h.Name, Master: h.Master, Bridge: h.Segment.Bridge, VLAN: h.Segment.VLAN, Via: h.Via}
		for _, a := range h.Addrs {
			if a.Addr().Is4() {
				i.Addrs = append(i.Addrs, netip.PrefixFrom(a.Addr(), a.Bits()))
			}
		}
		out = append(out, i)
	}
	return out
}
