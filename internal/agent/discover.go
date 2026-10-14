package agent

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/clusterapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// ErrNoController is a discovery that found no controller to follow.
var ErrNoController = errors.New("no controller to follow")

// Endpoint is where the controller answers and how the agent knows it.
type Endpoint struct {
	// Self says that this node holds the lease: its controller drives the
	// agent in process, and nothing is dialled.
	Self bool
	// Node is the node the controller runs on, where it is known.
	Node string
	// Addrs are the controller's addresses (host:port), tried in order.
	Addrs []string
	// ServerName is the name its certificate must carry, and CASHA256 the
	// fingerprint of the CA it is under; empty when the agent's own CA
	// stands.
	ServerName string
	CASHA256   string
}

func (e Endpoint) same(o Endpoint) bool {
	return e.Self == o.Self && e.Node == o.Node && slices.Equal(e.Addrs, o.Addrs) &&
		e.ServerName == o.ServerName && e.CASHA256 == o.CASHA256
}

// Discoverer finds the controller. The client asks it again every two
// seconds and follows what changes.
type Discoverer interface {
	Endpoint(ctx context.Context) (Endpoint, error)
}

// LeaderSource is what a host agent reads to find the controller.
// *store.Store is one.
type LeaderSource interface {
	Leader() (store.LeaderRecord, int64, bool, error)
	Nodes() ([]store.NodeEntry, error)
}

// HostDiscovery finds the controller of a host install: the node that
// leader.json names, at the agent address the registry holds for it, known
// by the name of that node's server certificate and the CA leader.json
// names. leader.json is in the cluster filesystem, so it is read on every
// node without the network.
type HostDiscovery struct {
	Node      string // this node
	InstallID string
	Store     LeaderSource
	Port      uint16 // zero is the agent endpoint's port
}

// Endpoint reads leader.json and the registry. An entry of this node the
// registry holds removed is a definitive refusal, as the controller would
// give it: a forgotten node whose agent the controller no longer takes a
// connection from learns it this way, without the network.
func (d HostDiscovery) Endpoint(context.Context) (Endpoint, error) {
	nodes, nodesErr := d.Store.Nodes()
	if nodesErr == nil {
		i := slices.IndexFunc(nodes, func(n store.NodeEntry) bool { return n.Name == d.Node })
		if i >= 0 && nodes[i].State == store.NodeRemoved {
			return Endpoint{}, &refusedError{r: agentproto.Refusal{Code: agentproto.RefusalRemoved,
				Message: fmt.Sprintf("the registry holds node %s removed from install %s", d.Node, d.InstallID)}}
		}
	}
	rec, _, found, err := d.Store.Leader()
	switch {
	case err != nil:
		return Endpoint{}, fmt.Errorf("reading who holds the lease: %w", err)
	case !found || rec.Node == "":
		return Endpoint{}, fmt.Errorf("%w: no node holds the lease", ErrNoController)
	case rec.InstallID != d.InstallID:
		return Endpoint{}, fmt.Errorf("leader.json is of install %s, this node of %s", rec.InstallID, d.InstallID)
	case !rec.ReleasedAt.IsZero() && !rec.ReleasedAt.Before(rec.AcquiredAt):
		return Endpoint{}, fmt.Errorf("%w: node %s released the lease", ErrNoController, rec.Node)
	case rec.Node == d.Node:
		return Endpoint{Self: true, Node: rec.Node, CASHA256: rec.CASHA256}, nil
	}
	if nodesErr != nil {
		return Endpoint{}, fmt.Errorf("reading the registry: %w", nodesErr)
	}
	i := slices.IndexFunc(nodes, func(n store.NodeEntry) bool { return n.Name == rec.Node })
	if i < 0 || !nodes[i].AgentAddress.IsValid() {
		return Endpoint{}, fmt.Errorf("node %s holds the lease and has no agent address in the registry", rec.Node)
	}
	port := d.Port
	if port == 0 {
		port = clusterapi.Port
	}
	return Endpoint{
		Node:       rec.Node,
		Addrs:      []string{netip.AddrPortFrom(nodes[i].AgentAddress, port).String()},
		ServerName: pki.NodeServerName(d.InstallID, rec.Node),
		CASHA256:   rec.CASHA256,
	}, nil
}

// ApplianceDiscovery finds the controller of an appliance install: the
// endpoints of the join token and of the last desired state, and failing
// those the current addresses of the controller's container from the
// Proxmox API. It is known by the controller's fixed name, so an address
// that changed does not matter.
type ApplianceDiscovery struct {
	InstallID string
	// Endpoints are the endpoints of the join token and of the last
	// desired state, in that order.
	Endpoints func() []string
	// API reads the current addresses of the controller's container; nil
	// where the agent cannot ask.
	API func(ctx context.Context) ([]string, error)
}

// Endpoint lists every address the controller may be found at.
func (d ApplianceDiscovery) Endpoint(ctx context.Context) (Endpoint, error) {
	var addrs []string
	if d.Endpoints != nil {
		addrs = append(addrs, d.Endpoints()...)
	}
	var apiErr error
	if d.API != nil {
		more, err := d.API(ctx)
		addrs, apiErr = append(addrs, more...), err
	}
	addrs = slices.DeleteFunc(dedupe(addrs), func(a string) bool { return a == "" })
	if len(addrs) == 0 {
		if apiErr != nil {
			return Endpoint{}, fmt.Errorf("%w: reading the controller's addresses: %w", ErrNoController, apiErr)
		}
		return Endpoint{}, fmt.Errorf("%w: no address of the controller is known", ErrNoController)
	}
	return Endpoint{Addrs: addrs, ServerName: pki.ControllerServerName(d.InstallID)}, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
