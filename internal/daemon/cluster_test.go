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

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
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

	// The observe-only window began with this tenure's first look, and is
	// kept in the store for the next controller.
	rec, found, err := first.store.ObserveSince()
	require.NoError(t, err)
	require.True(t, found, "a new install observes only")
	require.False(t, rec.Since.IsZero())

	// A holder that loses the lease closes the endpoint: the lock is gone,
	// and the node may not take it again.
	self = entryIn(t, first.store, testNode)
	no := false
	self.ControllerEligible = &no
	require.NoError(t, first.store.SaveNode(self))
	require.NoError(t, os.Remove(first.lockDir))
	require.Eventually(t, func() bool { return !listens(endpoint) }, 20*time.Second, 50*time.Millisecond)

	require.NoError(t, r2.stop())
	require.NoError(t, r1.stop())
}

// Every host node observes the forwarding tables of its bridges for as long
// as its agent runs, and says so in its status: the controller sees both
// nodes watching and the cluster covered, which attested proofs and shared
// placement need.
func TestEveryNodeOfTheClusterWatchesItsForwardingTables(t *testing.T) {
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
		if len(st.Nodes) != 2 || st.Cluster == nil || !st.Cluster.Coverage.Covered {
			return false
		}
		for _, n := range st.Nodes {
			if n.State != store.NodeActive || !n.WatchRunning || !n.Covered {
				return false
			}
		}
		return true
	})
	require.Empty(t, st.Cluster.Coverage.Uncovered)
	require.EqualValues(t, 1, first.tables.runs.Load(), "one observer, started with the agent")
	require.EqualValues(t, 1, second.tables.runs.Load())

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
	require.Zero(t, w.tables.runs.Load(), "the forwarding tables vouch for nobody")
	require.NoError(t, r.stop())
}

// The Proxmox client falls back on the other nodes the members file lists
// with an address, and follows the file as nodes come and go.
func TestTheProxmoxFallbacksFollowTheMembersFile(t *testing.T) {
	w := newWorld(t)
	m, err := pve.NewMulti(pve.Config{
		BaseURL: w.cfg.PVEURL, TokenID: pveTokenID, Secret: pveSecret, CAFile: w.cfg.PVECAFile,
	}, nil)
	require.NoError(t, err)
	member := func(name, ip string) members.Member {
		var addr netip.Addr
		if ip != "" {
			addr = netip.MustParseAddr(ip)
		}
		return members.Member{Name: name, Online: true, IP: addr}
	}

	followMembers(m, testNode, members.Members{Nodes: []members.Member{member(testNode, "10.0.0.1"), member(otherNode, "10.0.0.2")}}, nil)
	require.Equal(t, []pve.Endpoint{{Address: "10.0.0.2", ServerName: otherNode}}, m.Endpoints())

	followMembers(m, testNode, members.Members{Nodes: []members.Member{
		member(testNode, "10.0.0.1"), member(otherNode, "10.0.0.2"), member("pve3", "10.0.0.3"), member("pve4", ""),
	}}, nil)
	require.Equal(t, []string{otherNode, "pve3"}, fallbackNames(m), "a node without an address is none")

	followMembers(m, testNode, members.Members{Nodes: []members.Member{member(testNode, "10.0.0.1")}}, nil)
	require.Empty(t, m.Endpoints())
}

// The start of the observe-only window is kept in the store: a later
// process, or another controller, goes by the same day; it ends with
// observe-only mode; and a start that cannot be read closes the window.
func TestTheObserveOnlyWindowOutlivesTheProcess(t *testing.T) {
	w := newWorld(t)
	log := zerolog.Nop()
	began := t0.Add(time.Hour)

	var first observeWindow
	first.note(w.store, true, true, began, log)
	since, on := first.window()
	require.True(t, on)
	require.Equal(t, began, since)

	var later observeWindow
	later.note(w.store, true, false, began.Add(5*time.Hour), log)
	since, on = later.window()
	require.True(t, on)
	require.Equal(t, began, since, "the day began with the first process")

	later.note(w.store, false, true, began.Add(6*time.Hour), log)
	_, on = later.window()
	require.False(t, on)
	_, found, err := w.store.ObserveSince()
	require.NoError(t, err)
	require.False(t, found)

	require.NoError(t, os.WriteFile(filepath.Join(w.paths.Cluster, "meta", "observe.json"), []byte("{not json"), 0o600))
	later.note(w.store, true, true, began.Add(7*time.Hour), log)
	_, on = later.window()
	require.False(t, on, "a start that cannot be read takes no certificate")
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
