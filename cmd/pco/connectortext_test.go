package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// The connector column of a tunnel pinned to another node says where it
// runs, as no connector of it runs here; this node's own tunnels show what
// the last cycle found of theirs.
func TestTheConnectorColumnOfATunnelPinnedElsewhereNamesItsNode(t *testing.T) {
	conns := []connector.Status{{TunnelID: "shared", Active: true, Ready: true, Connections: 4}}
	shared := engine.TunnelView{TunnelState: reconcile.TunnelState{Name: "pco-abc123", ID: "shared"}}
	ownPinned := engine.TunnelView{TunnelState: reconcile.TunnelState{Name: "pco-abc123-pve1", ID: "pin1", Kind: engine.TunnelPinned, Node: "pve1"}}
	elsewhere := engine.TunnelView{TunnelState: reconcile.TunnelState{Name: "pco-abc123-pve2", ID: "pin2", Kind: engine.TunnelPinned, Node: "pve2"}}

	require.Equal(t, "active, ready, 4 connections", connectorText(conns, shared, "pve1"))
	require.Equal(t, "none", connectorText(conns, ownPinned, "pve1"))
	require.Equal(t, "on pve2 (pco cluster status)", connectorText(conns, elsewhere, "pve1"))
}
