package inventory

import (
	"context"
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// NodeReader reads what belongs to one node: its network and the
// configuration and addresses of its guests.
type NodeReader interface {
	GuestConfig(ctx context.Context, node string, ref model.GuestRef) (pve.GuestConfig, error)
	AgentInterfaces(ctx context.Context, node string, vmid int) ([]pve.GuestIface, error)
	LXCInterfaces(ctx context.Context, node string, vmid int) ([]pve.GuestIface, error)
	NodeNetwork(ctx context.Context, node string) ([]pve.NodeIface, error)
}

// OnNode is src, but what belongs to node itself, the node the daemon runs
// on, is read with local when the API gives no answer for it
// (pve.Unreachable). Every node's pveproxy hands a read of node's own to
// node's pveproxy, so while that one is stopped, no other node can answer
// it either. What the API answered, a refusal or a guest that is not there
// among it, stands. What neither could read says why for both.
func OnNode(src Source, node string, local NodeReader) Source {
	return onNode{Source: src, node: node, local: local}
}

type onNode struct {
	Source
	node  string
	local NodeReader
}

func (o onNode) GuestConfig(ctx context.Context, node string, ref model.GuestRef) (pve.GuestConfig, error) {
	return readOn(ctx, o, node, func(r NodeReader) (pve.GuestConfig, error) { return r.GuestConfig(ctx, node, ref) })
}

func (o onNode) AgentInterfaces(ctx context.Context, node string, vmid int) ([]pve.GuestIface, error) {
	return readOn(ctx, o, node, func(r NodeReader) ([]pve.GuestIface, error) { return r.AgentInterfaces(ctx, node, vmid) })
}

func (o onNode) LXCInterfaces(ctx context.Context, node string, vmid int) ([]pve.GuestIface, error) {
	return readOn(ctx, o, node, func(r NodeReader) ([]pve.GuestIface, error) { return r.LXCInterfaces(ctx, node, vmid) })
}

func (o onNode) NodeNetwork(ctx context.Context, node string) ([]pve.NodeIface, error) {
	return readOn(ctx, o, node, func(r NodeReader) ([]pve.NodeIface, error) { return r.NodeNetwork(ctx, node) })
}

func readOn[T any](ctx context.Context, o onNode, node string, read func(NodeReader) (T, error)) (T, error) {
	out, err := read(o.Source)
	if err == nil || node != o.node || !pve.Unreachable(err) || ctx.Err() != nil {
		return out, err
	}
	out, localErr := read(o.local)
	if localErr != nil {
		return out, fmt.Errorf("%w; read on this node instead: %w", err, localErr)
	}
	return out, nil
}
