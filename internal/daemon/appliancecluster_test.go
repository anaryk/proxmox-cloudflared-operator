package daemon

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// A controller container makes the install's CA, serves the agent endpoint
// on net0's address, makes a join token that names it, and enrols the agent
// container of another node that joins with the token, once.
func TestAControllerContainerEnrolsAnAgentThatJoins(t *testing.T) {
	a := newApplianceWorld(t)
	const install = "0123456789ab"
	inst, _, err := a.store.Install()
	require.NoError(t, err)
	inst.ID = install
	inst.Appliance.Kind = store.ApplianceController
	require.NoError(t, a.store.SaveInstall(inst))
	require.NoError(t, a.store.SaveWriter(planner.Writer{InstallID: install, Generation: 1, Nonce: "n1", Incarnation: incarnation}))
	require.NoError(t, a.store.SaveNode(store.NodeEntry{Name: testNode, Since: t0, State: store.NodeActive, StateSince: t0}))
	a.eth0.set(netip.MustParseAddr("127.0.0.1"))
	a.deps.Cluster = ClusterDeps{Port: 0, Every: 20 * time.Millisecond}
	a.pve.answer("/api2/json/cluster/resources", []map[string]any{
		{"type": "lxc", "vmid": applianceVMID, "name": "pco", "node": testNode, "status": "running", "template": 0, "pool": "pco", "uptime": 3600},
		{"type": "lxc", "vmid": 9301, "name": "pco", "node": "pve2", "status": "running", "template": 0, "pool": "pco", "uptime": 3600},
	})
	a.pve.answer("/api2/json/cluster/ha/resources", []map[string]any{})
	a.pve.answer("/api2/json/nodes/pve2/lxc/9301/config", map[string]any{
		"net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:93:01,ip=dhcp,type=veth", "digest": "d",
	})
	a.pve.answer("/api2/json/nodes/pve2/lxc/9301/interfaces", []map[string]any{
		{"name": "eth0", "hwaddr": "bc:24:11:00:93:01", "inet": "127.0.0.1/8"},
	})

	d := a.start()
	var token string
	require.Eventually(t, func() bool {
		v, err := d.client.JoinToken(t.Context(), "pve2")
		token = v.Token
		return err == nil
	}, 10*time.Second, 20*time.Millisecond)
	parsed, err := pki.ParseJoinToken(token)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(parsed.Endpoints[0], "127.0.0.1:"))
	cur, _, _, err := a.store.CA()
	require.NoError(t, err)
	ca, err := pki.ParseCA([]byte(cur.CertPEM), []byte(cur.KeyPEM.Reveal()))
	require.NoError(t, err)
	require.Equal(t, ca.SHA256(), parsed.CASHA256)

	dir := t.TempDir()
	file := filepath.Join(dir, "join.token")
	require.NoError(t, os.WriteFile(file, []byte(token), 0o600))
	j := agent.Joiner{
		TokenFile: file, Owner: os.Getuid(), Files: agent.TLSFiles{Dir: filepath.Join(dir, "tls")}, Dir: dir,
		InstallID: install, Node: "pve2", VMID: 9301, MACs: []string{"bc:24:11:00:93:01"}, Build: "1.2.3", Log: zerolog.Nop(),
	}
	creds, err := j.Join(context.Background())
	require.NoError(t, err)

	nodes, err := a.store.Nodes()
	require.NoError(t, err)
	var joined store.NodeEntry
	for _, n := range nodes {
		if n.Name == "pve2" {
			joined = n
		}
	}
	require.Equal(t, store.NodeJoining, joined.State)
	require.Equal(t, pki.Fingerprint(creds.Cert), joined.CertSHA256)
	require.NoFileExists(t, file)

	require.NoError(t, os.WriteFile(file, []byte(token), 0o600))
	_, err = j.Join(context.Background())
	var refused *agent.JoinRefusedError
	require.ErrorAs(t, err, &refused, "a token is good once")
	require.NoError(t, d.stop())
}

// A controller container that is not proven, as a copy, takes no join: its
// endpoint is not opened, and one that is open closes.
func TestAnUnprovenControllerServesNoAgent(t *testing.T) {
	stopped := false
	c := &applianceCluster{
		proven: func() bool { return false },
		addrs: func(context.Context) ([]netip.Addr, error) {
			t.Fatal("an unproven controller looks for no address to listen on")
			return nil, nil
		},
		log:    zerolog.Nop(),
		remote: newRemoteFleet(time.Now, func() agentproto.Epoch { return agentproto.Epoch{} }),
	}
	c.tick(t.Context())
	require.Nil(t, c.listening)

	c.listening = start(t.Context(), func(ctx context.Context) {
		<-ctx.Done()
		stopped = true
	})
	c.tick(t.Context())
	require.Nil(t, c.listening)
	require.True(t, stopped)
}

// joinMidway is a Proxmox whose first listing of the guests runs a whole
// join, as one that lands while the endpoint reads who may connect.
type joinMidway struct {
	*fakeJoinPVE
	join func()
}

func (p *joinMidway) Resources(ctx context.Context) ([]pve.Resource, error) {
	if join := p.join; join != nil {
		p.join = nil
		join()
	}
	return p.fakeJoinPVE.Resources(ctx)
}

func clusterOf(jt *joinTest, px joinProxmox) *applianceCluster {
	jt.j.px = px
	return &applianceCluster{st: jt.st, joins: jt.j, install: joinInstall, vmid: 9250, node: "pve1", now: func() time.Time { return jt.now }, log: zerolog.Nop()}
}

// The container that joins may connect before its join, and as the node it
// joined as after it, no longer to join.
func TestTheAgentEndpointTakesAJoiningContainerThroughItsJoin(t *testing.T) {
	jt := newJoinTest(t)
	c := clusterOf(jt, jt.px)

	src, err := c.sources(t.Context())
	require.NoError(t, err)
	require.Equal(t, []netip.Addr{agentAddr}, src.Join)
	require.Empty(t, src.Nodes)

	_, err = jt.join(agentAddr)
	require.NoError(t, err)
	src, err = c.sources(t.Context())
	require.NoError(t, err)
	require.Empty(t, src.Join, "a used token is closed once its node is registered")
	require.Equal(t, []netip.Addr{agentAddr}, src.Nodes["pve2"])
}

// A join that lands between the reads of the registry and of the join
// tokens leaves its container in one of the two lists, never in neither,
// where the allowlist would close its connections before any answer.
func TestAJoinWhileTheSourcesAreReadLeavesTheContainerReachable(t *testing.T) {
	jt := newJoinTest(t)
	px := &joinMidway{fakeJoinPVE: jt.px}
	px.join = func() {
		_, err := jt.join(agentAddr)
		require.NoError(t, err)
	}
	c := clusterOf(jt, px)

	src, err := c.sources(t.Context())
	require.NoError(t, err)
	require.Nil(t, px.join, "the join ran while the sources were read")
	held := slices.Contains(src.Join, agentAddr) || slices.Contains(src.Nodes["pve2"], agentAddr)
	require.True(t, held, "the container that joined is in neither list: %+v", src)
}
