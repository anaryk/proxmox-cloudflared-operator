package apifake

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func TestANodeAloneHasNoClusterToActOn(t *testing.T) {
	e, _ := load(t, "populated")
	d := serve(t, e)
	ctx := context.Background()

	status, err := d.ClusterStatus(ctx)
	require.NoError(t, err)
	require.Nil(t, status.Lease)
	require.Empty(t, status.Nodes)
	require.ErrorIs(t, d.Handoff(ctx, "pve2"), engine.ErrRefused)
	_, err = d.JoinToken(ctx, "pve2")
	require.ErrorIs(t, err, engine.ErrRefused)
	_, err = d.AdmitNode(ctx, "pve2", "", false)
	require.ErrorIs(t, err, engine.ErrRefused)
	_, err = d.ForgetNode(ctx, "pve2", false)
	require.ErrorIs(t, err, engine.ErrRefused)
	require.ErrorIs(t, d.Unblock(ctx, netip.MustParseAddr("10.0.0.60")), engine.ErrNotFound)
}

func TestTheClusterScenarioKeepsWhatAnAdminDoesToItsNodes(t *testing.T) {
	e, _ := load(t, "cluster")
	d := serve(t, e)
	ctx := context.Background()
	nodeOf := func(name string) engine.NodeView {
		t.Helper()
		for _, n := range e.State().Nodes {
			if n.Name == name {
				return n
			}
		}
		t.Fatalf("no node %s", name)
		return engine.NodeView{}
	}

	status, err := d.ClusterStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, "pve1", status.Lease.Holder)
	require.Equal(t, []string{"pve1", "pve2", "pve3"}, status.Cluster.Members)
	require.Len(t, status.Nodes, 3)

	// An appliance agent is admitted with the fingerprint of its certificate.
	_, err = d.AdmitNode(ctx, "pve3", "", false)
	require.ErrorIs(t, err, engine.ErrInvalid)
	_, err = d.AdmitNode(ctx, "pve3", "0123456789abcdef", false)
	require.ErrorIs(t, err, engine.ErrRefused)
	require.Contains(t, err.Error(), "is not that of the certificate enrolled for node pve3")
	_, err = d.AdmitNode(ctx, "pve3", "zz", false)
	require.ErrorIs(t, err, engine.ErrInvalid)
	admitted, err := d.AdmitNode(ctx, "pve3", strings.ToUpper(EnrolledFingerprint("pve3")[:12]), false)
	require.NoError(t, err)
	require.Equal(t, "active", admitted.State)
	require.Empty(t, admitted.JoinUses)
	require.Equal(t, "active", nodeOf("pve3").State)
	_, err = d.AdmitNode(ctx, "pve3", "", false)
	require.ErrorIs(t, err, engine.ErrRefused, "admitted already")
	_, err = d.AdmitNode(ctx, "pve9", "", false)
	require.ErrorIs(t, err, engine.ErrNotFound)

	// A drain leaves the shared tunnel's replicas; the last one needs force.
	drained, err := d.DrainNode(ctx, "pve2", false)
	require.NoError(t, err)
	require.Equal(t, "draining", drained.State)
	require.Equal(t, "admin", drained.Drained)
	_, err = d.DrainNode(ctx, "pve3", false)
	require.NoError(t, err)
	_, err = d.DrainNode(ctx, "pve1", false)
	require.ErrorIs(t, err, engine.ErrRefused, "the only active replica")
	_, err = d.DrainNode(ctx, "pve1", true)
	require.NoError(t, err)
	undrained, err := d.UndrainNode(ctx, "pve2")
	require.NoError(t, err)
	require.Equal(t, "active", undrained.State)
	require.Empty(t, undrained.Drained)
	_, err = d.UndrainNode(ctx, "pve2")
	require.ErrorIs(t, err, engine.ErrRefused, "not drained")

	// A forget as compromised waits for pvecm delnode; a plain one removes
	// the node.
	_, err = d.ForgetNode(ctx, "pve3", true)
	require.ErrorIs(t, err, engine.ErrRefused)
	require.Contains(t, err.Error(), "run pvecm delnode pve3 first")
	_, err = d.ForgetNode(ctx, "pve1", false)
	require.ErrorIs(t, err, engine.ErrRefused, "this node")
	res, err := d.ForgetNode(ctx, "pve3", false)
	require.NoError(t, err)
	require.Equal(t, "pve3", res.Node)
	require.NotEmpty(t, res.Done)
	require.Equal(t, "removed", nodeOf("pve3").State)
	require.Equal(t, []string{"pve1", "pve2"}, e.State().Cluster.Members)
	_, err = d.ForgetNode(ctx, "pve3", false)
	require.ErrorIs(t, err, engine.ErrRefused, "removed already")

	// A join token for a node of the install is refused; a new node gets one.
	_, err = d.JoinToken(ctx, "pve2")
	require.ErrorIs(t, err, engine.ErrRefused)
	token, err := d.JoinToken(ctx, "pve4")
	require.NoError(t, err)
	require.Equal(t, "pve4", token.Node)
	require.True(t, strings.HasPrefix(token.Token, "pco1."))

	// A hand-off names the node the lease goes to.
	require.ErrorIs(t, d.Handoff(ctx, "pve1"), engine.ErrRefused, "the controller")
	require.ErrorIs(t, d.Handoff(ctx, "pve3"), engine.ErrRefused, "removed")
	hand, err := e.Handoff(ctx, "pve2")
	require.NoError(t, err)
	require.Equal(t, "pve1", hand.From)
	require.Equal(t, "pve2", e.State().Lease.Handoff)

	// A block ends once; the problem line goes with it.
	addr := netip.MustParseAddr("10.0.0.60")
	require.NoError(t, d.Unblock(ctx, addr))
	require.Empty(t, e.State().Cluster.Blocks)
	for _, p := range e.State().Problems {
		require.NotContains(t, p, "10.0.0.60 is blocked")
	}
	require.ErrorIs(t, d.Unblock(ctx, addr), engine.ErrNotFound)

	agent, err := d.AgentStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, 7, agent.Epoch.Generation)
	require.Equal(t, EnrolledFingerprint("pve1"), agent.CertFingerprint)

	methods := map[string]bool{}
	for _, c := range e.Calls() {
		methods[c.Method] = true
	}
	for _, m := range []string{"AdmitNode", "DrainNode", "UndrainNode", "ForgetNode", "NewJoinToken", "Handoff", "Unblock", "AgentView"} {
		require.True(t, methods[m], "%s is recorded", m)
	}
}
