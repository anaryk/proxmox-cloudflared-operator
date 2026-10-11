package pve

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

func TestNodeFirewallOptionsReadTheBackend(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/nodes/pve1/firewall/options": {http.StatusOK, `{"data":{"enable":1,"nftables":1,"log_level_in":"nolog"}}`},
		"/nodes/pve2/firewall/options": {http.StatusOK, `{"data":{"enable":"0"}}`},
		"/nodes/pve3/firewall/options": {http.StatusOK, `{"data":{"nftables":"maybe"}}`},
	})
	ctx := context.Background()

	got, err := c.NodeFirewallOptions(ctx, "pve1")
	require.NoError(t, err)
	require.Equal(t, NodeFirewallOptions{Enabled: true, NFTables: true}, got)
	require.Equal(t, "/api2/json/nodes/pve1/firewall/options", rec.requests()[0].uri)

	got, err = c.NodeFirewallOptions(ctx, "pve2")
	require.NoError(t, err)
	require.Equal(t, NodeFirewallOptions{}, got, "an absent nftables is the iptables backend")

	_, err = c.NodeFirewallOptions(ctx, "pve3")
	require.Error(t, err)
	_, err = c.NodeFirewallOptions(ctx, "pve 1")
	require.ErrorContains(t, err, "invalid node name")
}

func TestClusterFirewallOptions(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{"/cluster/firewall/options": {http.StatusOK, `{"data":{"enable":1,"policy_in":"DROP"}}`}})
	got, err := c.ClusterFirewallOptions(context.Background())
	require.NoError(t, err)
	require.Equal(t, ClusterFirewallOptions{Enabled: true}, got)

	c, _ = newTestClient(t, map[string]reply{"/cluster/firewall/options": {http.StatusOK, `{"data":{}}`}})
	got, err = c.ClusterFirewallOptions(context.Background())
	require.NoError(t, err)
	require.Equal(t, ClusterFirewallOptions{}, got, "a datacenter firewall never configured is off")
}

func TestGuestFirewallOptions(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/nodes/pve1/qemu/101/firewall/options": {http.StatusOK, `{"data":{"enable":1,"dhcp":1}}`},
		"/nodes/pve1/lxc/200/firewall/options":  {http.StatusOK, `{"data":{}}`},
	})
	ctx := context.Background()

	got, err := c.GuestFirewallOptions(ctx, "pve1", model.GuestRef{Kind: model.KindQEMU, VMID: 101})
	require.NoError(t, err)
	require.Equal(t, GuestFirewallOptions{Enabled: true}, got)
	require.Equal(t, "/api2/json/nodes/pve1/qemu/101/firewall/options", rec.requests()[0].uri)

	got, err = c.GuestFirewallOptions(ctx, "pve1", model.GuestRef{Kind: model.KindLXC, VMID: 200})
	require.NoError(t, err)
	require.Equal(t, GuestFirewallOptions{}, got)

	_, err = c.GuestFirewallOptions(ctx, "pve1", model.GuestRef{Kind: "other", VMID: 1})
	require.ErrorContains(t, err, "invalid guest kind")
}

func TestNodeServiceState(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/nodes/pve1/services/pve-firewall/state": {http.StatusOK, `{"data":{"name":"pve-firewall","state":"running","service":"pve-firewall"}}`},
		"/nodes/pve2/services/pve-firewall/state": {http.StatusOK, `{"data":{"state":"stopped"}}`},
		"/nodes/pve3/services/pve-firewall/state": {http.StatusOK, `{"data":{"name":"pve-firewall"}}`},
	})
	ctx := context.Background()

	state, err := c.NodeServiceState(ctx, "pve1", "pve-firewall")
	require.NoError(t, err)
	require.Equal(t, "running", state)
	require.Equal(t, "/api2/json/nodes/pve1/services/pve-firewall/state", rec.requests()[0].uri)

	state, err = c.NodeServiceState(ctx, "pve2", "pve-firewall")
	require.NoError(t, err)
	require.Equal(t, "stopped", state)

	_, err = c.NodeServiceState(ctx, "pve3", "pve-firewall")
	require.ErrorContains(t, err, "no state")
	_, err = c.NodeServiceState(ctx, "pve1", "pve firewall")
	require.ErrorContains(t, err, "invalid service name")
}
