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

// listNodes reads the cluster members, sorted by name. An offline node keeps
// the interfaces the previous snapshot knew, which is the best that can be
// said about it. When the list cannot be read it returns the previous nodes
// and false; the snapshot is then incomplete and every node counts as online,
// so that the calls that follow decide.
func (i *Inventory) listNodes(ctx context.Context, r *run) ([]Node, bool) {
	cluster, err := i.src.ClusterNodes(ctx)
	if err != nil {
		i.log.Debug().Err(err).Msg("listing cluster nodes failed")
		r.fail("cluster nodes not listed: %v", err)
		return cloneNodes(i.last.Nodes), false
	}
	nodes := make([]Node, len(cluster))
	for j, c := range cluster {
		nodes[j] = Node{Name: c.Name, Addr: c.Addr, Online: c.Online, Local: c.Local}
		if !c.Online {
			nodes[j].Ifaces = i.knownIfaces(c.Name)
		}
	}
	slices.SortFunc(nodes, func(a, b Node) int { return strings.Compare(a.Name, b.Name) })
	return nodes, true
}

// offlineNames returns the names of the nodes that are not online.
func offlineNames(nodes []Node) map[string]bool {
	offline := map[string]bool{}
	for _, n := range nodes {
		if !n.Online {
			offline[n.Name] = true
		}
	}
	return offline
}

// refreshNodeIfaces reads the interfaces of the online nodes into nodes. A
// failure keeps what the previous snapshot knew and makes this one
// incomplete.
func (i *Inventory) refreshNodeIfaces(ctx context.Context, r *run, nodes []Node) {
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
		return
	}
	for _, j := range due {
		if err := results[j].err; err != nil {
			i.log.Debug().Str("node", nodes[j].Name).Err(err).Msg("reading node network failed")
			r.fail("node %s: network not refreshed: %v", nodes[j].Name, err)
			nodes[j].Ifaces = i.knownIfaces(nodes[j].Name)
			continue
		}
		nodes[j].Ifaces = cloneIfaces(results[j].ifaces)
	}
}

// knownIfaces returns a copy of the interfaces the previous snapshot had for
// a node.
func (i *Inventory) knownIfaces(name string) []pve.NodeIface {
	for _, n := range i.last.Nodes {
		if n.Name == name {
			return cloneIfaces(n.Ifaces)
		}
	}
	return nil
}

// cloneNodes copies nodes down to the slices they hold.
func cloneNodes(in []Node) []Node {
	out := slices.Clone(in)
	for j := range out {
		out[j].Ifaces = cloneIfaces(out[j].Ifaces)
	}
	return out
}

func cloneIfaces(in []pve.NodeIface) []pve.NodeIface {
	out := slices.Clone(in)
	for j := range out {
		out[j].Addrs = slices.Clone(out[j].Addrs)
		out[j].Ports = slices.Clone(out[j].Ports)
	}
	return out
}
