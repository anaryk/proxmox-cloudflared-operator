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

// pveTimeout is the time a request to the Proxmox API has in all: with
// fallbacks on the other nodes each endpoint tried gets a share of it, about
// five seconds with three nodes.
const pveTimeout = 15 * time.Second

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
	return c
}

// wire gives the engine the agents of the other nodes, the members file and
// the maintenance mode of the nodes, and has the agent of this node tell its
// events and changes to its client too.
func (c *clusterPart) wire(ed *engine.Deps, client *pve.Client, multi *pve.Multi) {
	c.multiPVE = multi
	ed.Fallbacks = func() []string { return fallbackNames(multi) }
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

// proxmoxClient is the client of the Proxmox API, which falls back on the
// other nodes of a cluster, as the members file lists them, verified under
// the cluster's CA, so that an outage of the API on this node does not hold
// the controller. The fallbacks follow the members file: the cluster part
// sets them again as it changes.
func proxmoxClient(cfg Config, deps Deps, token store.PVEToken, hl *hostLease) (*pve.Client, *pve.Multi, error) {
	pc := pve.Config{
		BaseURL:     cfg.PVEURL,
		TokenID:     token.TokenID,
		Secret:      token.Secret.Reveal(),
		CAFile:      cfg.PVECAFile,
		NodeCertDir: deps.PVECertDir,
		Timeout:     pveTimeout,
	}
	if pc.CAFile == "" {
		pc.CAFile = filepath.Join(hl.d.PVEDir, filepath.Base(members.RootCAFile))
	}
	m, err := pve.NewMulti(pc, nil)
	if err != nil {
		return nil, nil, err
	}
	followMembers(m, cfg.Node, readMembers(hl), nil)
	return m.Client, m, nil
}

// readMembers reads the members file now.
func readMembers(hl *hostLease) members.Members {
	m, err := members.Read(filepath.Join(hl.d.PVEDir, filepath.Base(members.MembersFile)), time.Now())
	if err != nil {
		return members.Members{}
	}
	return m
}

// followMembers falls back on every other node the members file lists with
// an address, known by its node's name.
func followMembers(m *pve.Multi, node string, mem members.Members, log *zerolog.Logger) {
	var eps []pve.Endpoint
	for _, n := range mem.Nodes {
		if n.Name != node && n.IP.IsValid() {
			eps = append(eps, pve.Endpoint{Address: n.IP.String(), ServerName: n.Name})
		}
	}
	if err := m.SetEndpoints(eps); err != nil && log != nil {
		log.Warn().Err(err).Msg("the other nodes' Proxmox API cannot be fallen back on")
	}
}

// fallbackNames are the nodes whose Proxmox API is fallen back on.
func fallbackNames(m *pve.Multi) []string {
	var out []string
	for _, ep := range m.Endpoints() {
		out = append(out, ep.ServerName)
	}
	return out
}
