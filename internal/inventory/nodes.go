package inventory

import (
	"context"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// nodeResult is the outcome of one node network fetch.
type nodeResult struct {
	ifaces []pve.NodeIface
	err    error
}

// refreshNodes lists the cluster nodes and the interfaces of those online. A
// failure keeps what the previous snapshot knew.
func (i *Inventory) refreshNodes(ctx context.Context, r *run) []Node {
	cluster, err := i.src.ClusterNodes(ctx)
	if err != nil {
		i.log.Debug().Err(err).Msg("listing cluster nodes failed")
		r.fail("cluster nodes not listed: %v", err)
		return slices.Clone(i.last.Nodes)
	}
	nodes := make([]Node, len(cluster))
	for j, c := range cluster {
		nodes[j] = Node{Name: c.Name, Addr: c.Addr, Online: c.Online, Local: c.Local}
	}
	slices.SortFunc(nodes, func(a, b Node) int { return strings.Compare(a.Name, b.Name) })

	var due []int
	for j, n := range nodes {
		if n.Online {
			due = append(due, j)
		}
	}
	results := make([]nodeResult, len(nodes))
	i.forEach(ctx, len(due), func(k int) {
		ifaces, err := i.src.NodeNetwork(ctx, nodes[due[k]].Name)
		results[due[k]] = nodeResult{ifaces: ifaces, err: err}
	})
	if ctx.Err() != nil {
		return nil
	}
	for _, j := range due {
		if err := results[j].err; err != nil {
			i.log.Debug().Str("node", nodes[j].Name).Err(err).Msg("reading node network failed")
			r.fail("node %s: network not refreshed: %v", nodes[j].Name, err)
			nodes[j].Ifaces = i.knownIfaces(nodes[j].Name)
			continue
		}
		nodes[j].Ifaces = slices.Clone(results[j].ifaces)
	}
	return nodes
}

// knownIfaces returns the interfaces the previous snapshot had for a node.
func (i *Inventory) knownIfaces(name string) []pve.NodeIface {
	for _, n := range i.last.Nodes {
		if n.Name == name {
			return slices.Clone(n.Ifaces)
		}
	}
	return nil
}
