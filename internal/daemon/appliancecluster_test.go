package daemon

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
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

// controllerForJoins starts a controller container whose agent endpoint
// listens on 127.0.0.1, with the agent container 9301 of node pve2 in pool
// pco, and returns a join token for pve2.
func controllerForJoins(t *testing.T) (*applianceWorld, *running, string) {
	t.Helper()
	a := newApplianceWorld(t)
	inst, _, err := a.store.Install()
	require.NoError(t, err)
	inst.ID = joinInstall
	inst.Appliance.Kind = store.ApplianceController
	require.NoError(t, a.store.SaveInstall(inst))
	require.NoError(t, a.store.SaveWriter(planner.Writer{InstallID: joinInstall, Generation: 1, Nonce: "n1", Incarnation: incarnation}))
	require.NoError(t, a.store.SaveNode(store.NodeEntry{Name: testNode, Since: t0, State: store.NodeActive, StateSince: t0}))
	a.eth0.set(netip.MustParseAddr("127.0.0.1"))
	a.deps.Cluster = ClusterDeps{Port: 0, Every: 20 * time.Millisecond}
	a.pve.answer("/api2/json/cluster/resources", []map[string]any{
		{"type": "lxc", "vmid": applianceVMID, "name": "pco", "node": testNode, "status": "running", "template": 0, "pool": "pco", "uptime": 3600},
		{"type": "lxc", "vmid": agentVMID, "name": "pco", "node": "pve2", "status": "running", "template": 0, "pool": "pco", "uptime": 3600},
	})
	a.pve.answer("/api2/json/cluster/ha/resources", []map[string]any{})
	a.pve.answer("/api2/json/nodes/pve2/lxc/9301/config", map[string]any{
		"net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:93:01,ip=dhcp,type=veth", "digest": "d",
	})
	a.pve.answer("/api2/json/nodes/pve2/lxc/9301/interfaces", []map[string]any{
		{"name": "eth0", "hwaddr": agentMAC, "inet": "127.0.0.1/8"},
	})

	d := a.start()
	var token string
	require.Eventually(t, func() bool {
		v, err := d.client.JoinToken(t.Context(), "pve2")
		token = v.Token
		return err == nil
	}, 10*time.Second, 20*time.Millisecond)
	return a, d, token
}

// joinerOf is the agent of container 9301 on pve2, with token in its file.
func joinerOf(t *testing.T, token string) agent.Joiner {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "join.token")
	require.NoError(t, os.WriteFile(file, []byte(token), 0o600))
	return agent.Joiner{
		TokenFile: file, Owner: os.Getuid(), Files: agent.TLSFiles{Dir: filepath.Join(dir, "tls")}, Dir: dir,
		InstallID: joinInstall, Node: "pve2", VMID: agentVMID, MACs: []string{agentMAC}, Build: "1.2.3", Log: zerolog.Nop(),
	}
}

func entryOf(t *testing.T, st *store.Store, node string) store.NodeEntry {
	t.Helper()
	nodes, err := st.Nodes()
	require.NoError(t, err)
	for _, n := range nodes {
		if n.Name == node {
			return n
		}
	}
	t.Fatalf("node %s is not in the registry", node)
	return store.NodeEntry{}
}

// A controller container makes the install's CA, serves the agent endpoint
// on net0's address, makes a join token that names it, and enrols the agent
// container of another node that joins with the token, once: a second join
// for another key holds the node contested.
func TestAControllerContainerEnrolsAnAgentThatJoins(t *testing.T) {
	a, d, token := controllerForJoins(t)
	parsed, err := pki.ParseJoinToken(token)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(parsed.Endpoints[0], "127.0.0.1:"))
	cur, _, _, err := a.store.CA()
	require.NoError(t, err)
	ca, err := pki.ParseCA([]byte(cur.CertPEM), []byte(cur.KeyPEM.Reveal()))
	require.NoError(t, err)
	require.Equal(t, ca.SHA256(), parsed.CASHA256)

	j := joinerOf(t, token)
	creds, err := j.Join(context.Background())
	require.NoError(t, err)
	joined := entryOf(t, a.store, "pve2")
	require.Equal(t, store.NodeJoining, joined.State)
	require.Equal(t, pki.Fingerprint(creds.Cert), joined.CertSHA256)
	require.NoFileExists(t, j.TokenFile)

	require.NoError(t, os.WriteFile(j.TokenFile, []byte(token), 0o600))
	_, err = j.Join(context.Background())
	var refused *agent.JoinRefusedError
	require.ErrorAs(t, err, &refused, "a token is good once")
	require.Equal(t, store.NodeContested, entryOf(t, a.store, "pve2").State)
	require.NoError(t, d.stop())
}

// An agent whose join was taken but whose answer was lost keeps its token
// and the key of the join, and sends the join again: while its node is
// joining, it gets the certificate issued for it.
func TestAJoinWhoseAnswerWasLostIsAnsweredAgain(t *testing.T) {
	a, d, token := controllerForJoins(t)
	j := joinerOf(t, token)
	key, err := pki.NewKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	keyFile := filepath.Join(j.Dir, "join.key")
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0o600))

	first, err := j.Join(context.Background())
	require.NoError(t, err)
	require.True(t, key.PublicKey.Equal(first.Cert.PublicKey), "the join is for the key kept before it was sent")

	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0o600))
	require.NoError(t, os.WriteFile(j.TokenFile, []byte(token), 0o600))
	again, err := j.Join(context.Background())
	require.NoError(t, err)
	require.Equal(t, first.Cert.Raw, again.Cert.Raw)
	require.Equal(t, store.NodeJoining, entryOf(t, a.store, "pve2").State)
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

// The container that joins may reach the join before its join, and while
// its node is joining, so that a join whose answer was lost is answered
// again; once the node is admitted it is no longer taken to join.
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
	require.Equal(t, []netip.Addr{agentAddr}, src.Join, "a node that is joining may send its join again")
	require.Equal(t, []netip.Addr{agentAddr}, src.Nodes["pve2"])

	admitted := jt.agentEntry()
	admitted.State, admitted.StateSince = store.NodeActive, jt.now
	require.NoError(t, jt.st.SaveNode(admitted))
	src, err = c.sources(t.Context())
	require.NoError(t, err)
	require.Empty(t, src.Join, "an admitted node joins no more")
	require.Equal(t, []netip.Addr{agentAddr}, src.Nodes["pve2"])
}

// A used token keeps the join open while its node has no entry or is
// joining, and for a removed entry older than its use; an admitted,
// contested or later removed node closes it.
func TestAUsedJoinTokenStaysOpenWhileItsNodeJoins(t *testing.T) {
	use := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	tok := store.JoinRecord{Node: "pve2", Uses: []store.JoinUse{{At: use}}}
	for _, tc := range []struct {
		name  string
		entry *store.NodeEntry
		open  bool
	}{
		{"no entry yet", nil, true},
		{"joining", &store.NodeEntry{State: store.NodeJoining}, true},
		{"admitted", &store.NodeEntry{State: store.NodeActive}, false},
		{"draining", &store.NodeEntry{State: store.NodeDraining}, false},
		{"contested", &store.NodeEntry{State: store.NodeContested}, false},
		{"removed before the use", &store.NodeEntry{State: store.NodeRemoved, StateSince: use.Add(-time.Hour)}, true},
		{"removed after the use", &store.NodeEntry{State: store.NodeRemoved, StateSince: use.Add(time.Hour)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := map[string]store.NodeEntry{}
			if tc.entry != nil {
				entries["pve2"] = *tc.entry
			}
			require.Equal(t, tc.open, joinOpen(tok, entries))
		})
	}
	require.True(t, joinOpen(store.JoinRecord{Node: "pve2"}, map[string]store.NodeEntry{"pve2": {State: store.NodeActive}}), "an unused token is open")
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
