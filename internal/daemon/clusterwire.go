package daemon

import (
	"cmp"
	"context"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/clusterapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	// pveTimeout is the time a request to the Proxmox API has on one node;
	// with fallbacks on the other nodes of the cluster each gets a share of
	// a request's time, so the request gets this much for every endpoint.
	pveTimeout = 10 * time.Second
	// pveShare is the least time an endpoint gets of a request.
	pveShare = 5 * time.Second
)

// newClusterPart makes the cluster mode of the daemon, before the engine,
// which it is wired into: the remote fleet, the events of the agent of this
// node for the controller, and the first start of this release, read before
// any cycle.
func newClusterPart(cfg Config, deps Deps, st *store.Store, hl *hostLease, log zerolog.Logger) *clusterPart {
	now := deps.Now
	c := &clusterPart{
		node: cfg.Node, st: st, paths: cfg.Paths, keeper: hl.keeper,
		quorum: hl.members,
		first:  newFirstStart(st, cfg.Node, cfg.Version, now, hl.Cluster, log),
		events: agent.NewEventLog(now),
		port:   cmp.Or(deps.Cluster.Port, uint16(clusterapi.Port)),
		every:  cmp.Or(deps.Cluster.Every, clusterEvery),
		now:    now, log: log,
	}
	c.remote = newRemoteFleet(now, func() agentproto.Epoch {
		w := hl.keeper.State().Writer
		return agentproto.Epoch{InstallID: w.InstallID, Generation: w.Generation, Nonce: w.Nonce}
	})
	c.multi = c.clustered()
	return c
}

// wire gives the engine the agents of the other nodes, the members file and
// the maintenance mode of the nodes, and has the agent of this node tell its
// events and changes to its client too.
func (c *clusterPart) wire(ed *engine.Deps, client *pve.Client) {
	ed.Remote = c.remote
	ed.Members = c.quorum
	ed.Maintenance = func(ctx context.Context) (map[string]pve.Maintenance, error) {
		st, err := client.HAStatus(ctx)
		return st.Maintenance, err
	}
	ed.Fleet.Also(c.events.Add, c.trigger)
}

// attach gives the cluster mode the engine it serves for.
func (c *clusterPart) attach(eng *engine.Engine) {
	c.eng = eng
	c.remote.attach(eng, func(ctx context.Context) agentproto.Status {
		if a := eng.Agent(); a != nil {
			return a.Status(ctx)
		}
		return agentproto.Status{}
	})
}

// trigger has the client send a status soon.
func (c *clusterPart) trigger() {
	c.mu.Lock()
	cl := c.client
	c.mu.Unlock()
	if cl != nil {
		cl.Trigger()
	}
}

// members is the last read of the members file.
func (h *hostLease) members() members.Members {
	m, _ := h.quorum.Last()
	return m
}

// proxmoxClient is the client of the Proxmox API: on a node of a cluster
// with the other nodes, as the members file lists them, as fallbacks under
// the cluster's CA, so that an outage of the API on this node does not hold
// the controller.
func proxmoxClient(cfg Config, deps Deps, token store.PVEToken, hl *hostLease) (*pve.Client, error) {
	pc := pve.Config{
		BaseURL:     cfg.PVEURL,
		TokenID:     token.TokenID,
		Secret:      token.Secret.Reveal(),
		CAFile:      cfg.PVECAFile,
		NodeCertDir: deps.PVECertDir,
	}
	others := otherEndpoints(cfg.Node, hl)
	if len(others) == 0 {
		return pve.New(pc)
	}
	if pc.CAFile == "" {
		pc.CAFile = filepath.Join(hl.d.PVEDir, filepath.Base(members.RootCAFile))
	}
	pc.Timeout = max(pveTimeout, pveShare*time.Duration(len(others)+1))
	m, err := pve.NewMulti(pc, others)
	if err != nil {
		return nil, err
	}
	return m.Client, nil
}

// otherEndpoints are the API endpoints of the other nodes the members file
// lists with an address, each known by its node's name.
func otherEndpoints(node string, hl *hostLease) []pve.Endpoint {
	m, err := members.Read(filepath.Join(hl.d.PVEDir, filepath.Base(members.MembersFile)), time.Now())
	if err != nil {
		return nil
	}
	var out []pve.Endpoint
	for _, n := range m.Nodes {
		if n.Name != node && n.IP.IsValid() {
			out = append(out, pve.Endpoint{Address: n.IP.String(), ServerName: n.Name})
		}
	}
	return out
}
