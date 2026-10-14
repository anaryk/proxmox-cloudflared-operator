package main

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

// clusterState is a three-node cluster as the controller shows it: this
// node holds the lease, pve2 is active and pve3 down, with an upgrade not
// finished and a rotation waiting on the admin.
func clusterState() engine.State {
	st := healthyState()
	one, two := 1, 2
	st.Lease = &engine.LeaseView{Holder: "pve1", Address: netip.MustParseAddr("10.9.0.1"), Generation: 3, Nonce: "abc", AcquiredAt: t0.Add(-time.Hour), RefreshAge: 4 * time.Second, Valid: true}
	st.Cluster = &engine.ClusterView{
		Controller: "pve1", Generation: 3, Members: []string{"pve1", "pve2", "pve3"}, Quorate: true, QuorumDevice: true, Overrides: []string{"two_node"},
		Coverage:   engine.CoverageView{Covered: false, Since: t0.Add(-time.Minute), Uncovered: []string{"pve3"}},
		Upgrade:    &engine.UpgradeView{From: "0.9.0", Since: t0.Add(-24 * time.Hour), Older: []string{"pve3"}},
		CARotation: &engine.CARotationView{Node: "pve4", Also: []string{"pve5"}, StartedAt: t0.Add(-10 * time.Minute), Done: []string{"credentials"}, Next: "proxmox"},
		Blocks:     []engine.BlockView{{Address: netip.MustParseAddr("10.0.0.7"), Guest: "qemu/102", MAC: "bc:24:11:00:00:02", Node: "pve3", Port: "tap103i0", Since: t0}},
	}
	zero := 0
	st.Nodes = []engine.NodeView{
		{Name: "pve1", State: "active", StateSince: t0.Add(-time.Hour), Kind: "host", Index: &zero, AgentAddress: netip.MustParseAddr("10.9.0.1"), Controller: true,
			Build: "1.0.0", Protocol: "1-1", StatusAge: time.Second, WatchRunning: true, Covered: true, ReplicaReady: true, Verified: 3, CertExpires: t0.Add(60 * 24 * time.Hour)},
		{Name: "pve2", State: "draining", Drained: "admin", StateSince: t0.Add(-time.Minute), Kind: "host", Index: &one, AgentAddress: netip.MustParseAddr("10.9.0.2"),
			Build: "1.0.0", Protocol: "1-1", StatusAge: 3 * time.Second, WatchRunning: true, Covered: true, Verified: 2, Failing: 1, Pinned: 1, CertExpires: t0.Add(60 * 24 * time.Hour), ClockSkew: 200 * time.Millisecond},
		{Name: "pve3", State: "down", StateSince: t0.Add(-5 * time.Minute), Kind: "host", Index: &two, AgentAddress: netip.MustParseAddr("10.9.0.3"), StatusAge: 5 * time.Minute},
		{Name: "app4", State: "contested", StateSince: t0.Add(-2 * time.Minute), Kind: "appliance", VMID: 9004,
			Contested: []engine.JoinUseView{{At: t0.Add(-3 * time.Minute), Source: netip.MustParseAddr("10.9.0.40"), VMID: 9004, MACs: []string{"bc:24:11:00:00:40"}}, {At: t0.Add(-2 * time.Minute), Source: netip.MustParseAddr("10.9.0.41"), VMID: 9005}}},
	}
	return st
}

func TestClusterStatusGolden(t *testing.T) {
	r, _ := daemonWith(t, clusterState())

	res := r.run("", "cluster", "status")

	requireGolden(t, "cluster_status.golden", res.out)
	require.ErrorIs(t, res.err, errReported, "a node is down and contested")

	res = r.run("", "cluster", "status", "--json")
	require.NoError(t, res.err)
	require.Contains(t, res.out, `"controller": "pve1"`)
	require.Contains(t, res.out, `"nodes": [`)
}

// A controller whose cycles hold says since when, how old what it shows is,
// and that its lease expired.
func TestClusterStatusOfACycleThatHeld(t *testing.T) {
	st := clusterState()
	st.Cluster.HeldSince, st.Cluster.JudgedAt, st.Cluster.LeaseExpired = t0.Add(-time.Minute), t0.Add(-2*time.Minute), true
	r, _ := daemonWith(t, st)

	res := r.run("", "cluster", "status")

	require.Contains(t, res.out, "Controller:  pve1, its lease expired")
	require.Regexp(t, `Held:        the cycles hold since 2026-\S+; coverage and blocks as judged at 2026-\S+\n`, res.out)
}

func TestClusterStatusOfANodeAlone(t *testing.T) {
	r, _ := daemonWith(t, healthyState())

	res := r.run("", "cluster", "status")

	require.NoError(t, res.err)
	requireGolden(t, "cluster_status_alone.golden", res.out)
}

func TestClusterHandoffAsksAndNamesTheNode(t *testing.T) {
	r, e := daemonWith(t, clusterState())

	res := r.tty().run("y\n", "cluster", "handoff", "--to", "pve2")
	require.NoError(t, res.err)
	require.Contains(t, res.errOut, "Hand the lease off to node pve2? [y/N] ")
	require.Equal(t, "The lease is handed off to node pve2, which alone may take it for 30 seconds.\n", res.out)
	require.Equal(t, []string{"handoff pve2"}, e.called())

	res = r.run("", "cluster", "handoff", "--yes")
	require.NoError(t, res.err)
	require.Contains(t, res.out, "whichever node comes first takes it")

	res = r.tty().run("\n", "cluster", "handoff")
	require.ErrorIs(t, res.err, errAborted)
	require.Equal(t, []string{"handoff pve2", "handoff "}, e.called())
}

func TestClusterJoinTokenGoesToAFileOrATerminal(t *testing.T) {
	r, e := daemonWith(t, clusterState())
	file := filepath.Join(testutil.ShortDir(t), "pve3.join")

	res := r.run("", "cluster", "join-token", "--node", "pve3", "--file", file)
	require.NoError(t, res.err)
	require.Equal(t, "The join token for node pve3 is in "+file+" (mode 0600); it is good once, until 2026-10-01T14:15:00+02:00.\n"+
		"On the node: pco appliance install --kind agent --join-file pve3.join\n", res.out)
	info, err := os.Stat(file)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, "pco1.c2VjcmV0LXRva2Vu\n", string(data))
	require.NotContains(t, res.out, "pco1.", "the token is not shown when it went to a file")

	res = r.run("", "cluster", "join-token", "--node", "pve3", "--file", file)
	require.EqualError(t, res.err, file+" exists already; name a new file")

	res = r.run("", "cluster", "join-token", "--node", "pve3")
	require.EqualError(t, res.err, "the output is no terminal: write the token to a file with --file, so that it is shown nowhere")

	r.env.stderrTerminal = func(io.Writer) bool { return true }
	res = r.run("", "cluster", "join-token", "--node", "pve3")
	require.NoError(t, res.err)
	require.Contains(t, res.out, "pco1.c2VjcmV0LXRva2Vu")
	require.Contains(t, res.out, "pco appliance install --kind agent --join -")

	res = r.run("", "cluster", "join-token")
	require.EqualError(t, res.err, "name the node the agent joins as, with --node")
	require.Equal(t, []string{"join-token pve3", "join-token pve3"}, e.called())
}

func TestClusterFinishUpgradeAsksFirst(t *testing.T) {
	r, e := daemonWith(t, clusterState())

	res := r.run("", "cluster", "finish-upgrade")
	require.ErrorIs(t, res.err, errNoTerminal)

	res = r.run("", "cluster", "finish-upgrade", "--yes")
	require.NoError(t, res.err)
	require.Equal(t, "The upgrade from 0.9.0 is finished; the store is at schema 1.\nA copy of the store from before is in /var/lib/pco/backup/1-20261001T120000Z.\n", res.out)
	require.Equal(t, []string{"finish-upgrade"}, e.called())

	e.clusterErr = fmt.Errorf("%w: 1 node runs a build that does not read store schema 1: pve3 (an earlier release, without a store schema), which is down", engine.ErrRefused)
	res = r.run("", "cluster", "finish-upgrade", "--yes")
	require.ErrorIs(t, res.err, engine.ErrRefused)
	require.ErrorContains(t, res.err, "pve3")
}

func TestClusterUnblock(t *testing.T) {
	r, e := daemonWith(t, clusterState())

	res := r.run("", "cluster", "unblock", "10.0.0.7")
	require.NoError(t, res.err)
	require.Equal(t, "The block of 10.0.0.7 is ended; the next cycle lets the connectors reach it again.\n", res.out)
	require.Equal(t, []string{"unblock 10.0.0.7"}, e.called())

	res = r.run("", "cluster", "unblock", "nope")
	require.EqualError(t, res.err, `"nope" is no IP address`)

	e.clusterErr = fmt.Errorf("%w: 10.0.0.8 is not blocked", engine.ErrNotFound)
	res = r.run("", "cluster", "unblock", "10.0.0.8")
	require.True(t, errors.Is(res.err, engine.ErrNotFound))
}
