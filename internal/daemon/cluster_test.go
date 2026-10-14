package daemon

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	otherNode      = "pve2"
	clusterInstall = "0123456789ab"
)

// twoNodes is a cluster of two nodes, each with a daemon of its own, which
// share the cluster filesystem: the store's cluster and private roots, the
// members file and the lock of the lease. testNode may be the controller;
// otherNode may not, so that the test knows which one takes the lease.
func twoNodes(t *testing.T) (first, second *world) {
	t.Helper()
	first, second = newWorld(t), newWorld(t)
	second.paths.Cluster, second.paths.Private = first.paths.Cluster, first.paths.Private
	second.cfg.Paths = second.paths
	second.cfg.Node = otherNode
	s, err := store.Open(second.paths)
	require.NoError(t, err)
	second.store = s
	second.deps.Lease.PVEDir = first.deps.Lease.PVEDir
	second.deps.Lease.FS = first.deps.Lease.FS
	second.deps.Lease.BootID = func() (string, error) { return "boot-2", nil }
	second.lockDir, second.leaseFS = first.lockDir, first.leaseFS

	members := `{"nodename": "` + testNode + `", "version": 3,
"cluster": {"name": "lab", "version": 1, "nodes": 2, "quorate": 1},
"nodelist": {"` + testNode + `": {"id": 1, "online": 1, "ip": "127.0.0.1"},
"` + otherNode + `": {"id": 2, "online": 1, "ip": "127.0.0.1"}}}`
	require.NoError(t, os.WriteFile(filepath.Join(first.deps.Lease.PVEDir, ".members"), []byte(members), 0o600))

	port := freePort(t)
	for _, w := range []*world{first, second} {
		w.deps.Cluster = ClusterDeps{Port: port, Every: 50 * time.Millisecond}
	}
	// The install's CA is made of the install id, which an install of
	// this release has in its full form.
	require.NoError(t, first.store.SaveInstall(store.Install{ID: clusterInstall, CreatedAt: t0}))
	require.NoError(t, first.store.SaveWriter(planner.Writer{InstallID: clusterInstall, Generation: 1, Nonce: "n1"}))
	loop := netip.MustParseAddr("127.0.0.1")
	no := false
	require.NoError(t, first.store.SaveNode(store.NodeEntry{Name: testNode, Since: t0, AgentAddress: loop, Kind: "host"}))
	require.NoError(t, first.store.SaveNode(store.NodeEntry{
		Name: otherNode, Since: t0, AgentAddress: loop, Kind: "host", State: store.NodeJoining, StateSince: t0,
		Protocol: store.ProtocolRange{Min: 1, Max: 1}, ControllerEligible: &no,
	}))
	return first, second
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return uint16(port)
}

// adminWrite posts an admin action to the daemon's socket and returns the
// status of the answer and its body.
func adminWrite(t *testing.T, socket string) (int, string) {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://pco/v1/segments/acknowledge",
		strings.NewReader(`{"bridge":"vmbr0","vlan":0}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var b strings.Builder
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	b.Write(buf[:n])
	return resp.StatusCode, b.String()
}

// Two daemons of a cluster: the one that takes the lease is the controller,
// numbers itself and listens for the agents; the other's agent finds it
// where leader.json says, reports to it and is admitted; the controller's
// state shows both nodes; and an admin write on the node that is not the
// controller is refused and names the controller.
func TestTheControllerServesTheAgentOfTheOtherNode(t *testing.T) {
	first, second := twoNodes(t)
	r1 := first.start()
	r1.await(func(st engine.State) bool { return st.Lease != nil && st.Lease.Valid })
	r2 := second.start()
	t.Cleanup(func() {
		if t.Failed() {
			t.Log("the controller's log:\n" + first.logs.String())
			t.Log("the other node's log:\n" + second.logs.String())
		}
	})

	st := r1.await(func(st engine.State) bool {
		for _, n := range st.Nodes {
			if n.Name == otherNode && n.State == store.NodeActive {
				return true
			}
		}
		return false
	})
	require.Len(t, st.Nodes, 2)
	self := entryIn(t, first.store, testNode)
	require.Equal(t, store.NodeActive, self.State, "the first start numbered the node of the earlier release")
	require.NotNil(t, self.Index)
	require.Equal(t, 0, *self.Index)
	other := entryIn(t, first.store, otherNode)
	require.NotNil(t, other.Index)
	require.Equal(t, 1, *other.Index)

	_, found, err := first.store.Enrolment(otherNode)
	require.NoError(t, err)
	require.True(t, found, "the other node enrolled the certificate it issued itself")

	require.Eventually(t, func() bool {
		code, _ := adminWrite(t, second.cfg.SocketPath)
		return code == http.StatusConflict
	}, 10*time.Second, 20*time.Millisecond)
	code, body := adminWrite(t, second.cfg.SocketPath)
	require.Equal(t, http.StatusConflict, code)
	require.Contains(t, body, "this node is not the controller; run it on "+testNode)
	segs, err := first.store.Segments()
	require.NoError(t, err)
	require.Empty(t, segs)

	// A registry that cannot be read serves nobody: the endpoint closes,
	// and opens again once it can be read.
	endpoint := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), first.deps.Cluster.Port).String()
	damaged := filepath.Join(first.paths.Cluster, "nodes", "pve9.json")
	require.NoError(t, os.WriteFile(damaged, []byte("{not json"), 0o600))
	require.Eventually(t, func() bool { return !listens(endpoint) }, 10*time.Second, 20*time.Millisecond)
	require.NoError(t, os.Remove(damaged))
	require.Eventually(t, func() bool { return listens(endpoint) }, 10*time.Second, 20*time.Millisecond)

	require.NoError(t, r2.stop())
	require.NoError(t, r1.stop())
}

// A node that stands alone opens no agent endpoint and runs no client: it
// runs as before.
func TestASingleNodeOpensNoAgentEndpoint(t *testing.T) {
	w := newWorld(t)
	port := freePort(t)
	w.deps.Cluster = ClusterDeps{Port: port, Every: 20 * time.Millisecond}
	r := w.start()
	r.await(func(st engine.State) bool { return st.Lease != nil && st.Lease.Valid })
	time.Sleep(200 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port).String(), time.Second)
	if err == nil {
		_ = conn.Close()
	}
	require.Error(t, err, "nothing listens")
	require.NoDirExists(t, filepath.Join(w.paths.Local, "agent", "tls"), "no certificate is issued")
	require.NoError(t, r.stop())
}

// listens says whether something takes a connection at addr.
func listens(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func entryIn(t *testing.T, st *store.Store, name string) store.NodeEntry {
	t.Helper()
	nodes, err := st.Nodes()
	require.NoError(t, err)
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no entry of %s", name)
	return store.NodeEntry{}
}
