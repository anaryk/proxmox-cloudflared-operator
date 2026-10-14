package main

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func TestNodeListGolden(t *testing.T) {
	r, _ := daemonWith(t, clusterState())

	res := r.run("", "node", "list")

	requireGolden(t, "node_list.golden", res.out)
	require.ErrorIs(t, res.err, errReported)

	res = r.run("", "node", "list", "--json")
	require.NoError(t, res.err)
	require.Contains(t, res.out, `"name": "pve1"`)

	r, _ = daemonWith(t, healthyState())
	res = r.run("", "node", "list")
	require.NoError(t, res.err)
	require.Equal(t, "This node alone runs the install.\n", res.out)
}

func TestNodeAdmitDrainUndrainAndSet(t *testing.T) {
	r, e := daemonWith(t, clusterState())
	for _, tt := range []struct {
		args []string
		call string
		out  string
	}{
		{[]string{"node", "admit", "app4", "--fingerprint", "9f86d081", "--renew"}, "admit app4 fingerprint=9f86d081 renew=true",
			"Node app4 is admitted; it becomes active once every route it fails moved. Follow it with pco node list.\n"},
		{[]string{"node", "admit", "pve2"}, "admit pve2 fingerprint= renew=false",
			"Node pve2 is admitted; it becomes active once every route it fails moved. Follow it with pco node list.\n"},
		{[]string{"node", "drain", "pve2"}, "drain pve2 force=false",
			"Node pve2 is draining: it leaves the shared tunnel's replicas; its pinned tunnel keeps the routes of its own guests.\n"},
		{[]string{"node", "drain", "pve1", "--force"}, "drain pve1 force=true",
			"Node pve1 is draining: it leaves the shared tunnel's replicas; its pinned tunnel keeps the routes of its own guests.\n"},
		{[]string{"node", "undrain", "pve2"}, "undrain pve2", "Node pve2 is no longer drained; it is active.\n"},
		{[]string{"node", "set", "pve3", "--controller-eligible=false"}, "set pve3 controllerEligible=false", "Node pve3 may not hold the lease from now on.\n"},
		{[]string{"node", "set", "pve3", "--controller-eligible=true"}, "set pve3 controllerEligible=true", "Node pve3 may hold the lease from now on.\n"},
	} {
		t.Run(tt.call, func(t *testing.T) {
			res := r.run("", tt.args...)
			require.NoError(t, res.err)
			require.Equal(t, tt.out, res.out)
			require.Contains(t, e.called(), tt.call)
		})
	}

	res := r.run("", "node", "set", "pve3")
	require.EqualError(t, res.err, "nothing to set: give --controller-eligible=true or false")
	res = r.run("", "node", "set", "pve3", "--controller-eligible=maybe")
	require.EqualError(t, res.err, `--controller-eligible takes true or false, not "maybe"`)

	e.clusterErr = fmt.Errorf("%w: node app4 is contested", engine.ErrRefused)
	res = r.run("", "node", "admit", "app4", "--fingerprint", "9f86d081")
	require.ErrorIs(t, res.err, engine.ErrRefused)
}

func TestNodeForgetAsksAndSaysWhatIsLeft(t *testing.T) {
	r, e := daemonWith(t, clusterState())

	res := r.run("", "node", "forget", "pve3")
	require.ErrorIs(t, res.err, errNoTerminal)
	require.Empty(t, e.called())

	res = r.tty().run("y\n", "node", "forget", "pve3")
	require.NoError(t, res.err)
	require.Contains(t, res.errOut, "Forget node pve3? Its agent is refused and the shared tunnel's secret is rotated. [y/N] ")
	requireGolden(t, "node_forget.golden", res.out)

	res = r.tty().run("y\n", "node", "forget", "pve3", "--compromised")
	require.NoError(t, res.err)
	require.Contains(t, res.errOut, "Forget node pve3 as compromised?")
	requireGolden(t, "node_forget_compromised.golden", res.out)
	require.Equal(t, []string{"forget pve3 compromised=false", "forget pve3 compromised=true"}, e.called())

	e.clusterErr = fmt.Errorf("%w: run pvecm delnode pve3 first: a node still in the cluster would receive every new secret", engine.ErrRefused)
	res = r.run("", "node", "forget", "pve3", "--compromised", "--yes")
	require.ErrorIs(t, res.err, engine.ErrRefused)
	require.ErrorContains(t, res.err, "pvecm delnode pve3")
}
