package agent

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const pkiInstall = "0123456789ab"

func saveLeader(t *testing.T, st *store.Store, node, caSHA string) {
	t.Helper()
	require.NoError(t, st.SaveLeader(store.LeaderRecord{
		Writer: planner.Writer{InstallID: pkiInstall, Generation: 1, Nonce: "n1"},
		Node:   node, AcquiredAt: t0, CASHA256: caSHA,
	}))
}

func saveAgentAddress(t *testing.T, st *store.Store, node string, addr netip.Addr) {
	t.Helper()
	require.NoError(t, st.SaveNode(store.NodeEntry{Name: node, Version: "dev", Since: t0, Kind: string(pki.KindHost), AgentAddress: addr}))
}

func TestTheHostAgentFollowsTheNodeLeaderJSONNames(t *testing.T) {
	e := newEnv(t)
	d := HostDiscovery{Node: testNode, InstallID: pkiInstall, Store: e.store}

	_, err := d.Endpoint(t.Context())
	require.ErrorIs(t, err, ErrNoController)

	sha := strings.Repeat("ab", 32)
	saveLeader(t, e.store, "pve2", sha)
	_, err = d.Endpoint(t.Context())
	require.ErrorContains(t, err, "no agent address")

	saveAgentAddress(t, e.store, "pve2", netip.MustParseAddr("10.0.0.2"))
	ep, err := d.Endpoint(t.Context())
	require.NoError(t, err)
	require.Equal(t, Endpoint{
		Node: "pve2", Addrs: []string{"10.0.0.2:8644"}, ServerName: pki.NodeServerName(pkiInstall, "pve2"), CASHA256: sha,
	}, ep)

	t.Run("this node holds the lease", func(t *testing.T) {
		saveLeader(t, e.store, testNode, sha)
		ep, err := d.Endpoint(t.Context())
		require.NoError(t, err)
		require.True(t, ep.Self)
		require.Empty(t, ep.Addrs)
	})
	t.Run("released", func(t *testing.T) {
		require.NoError(t, e.store.SaveLeader(store.LeaderRecord{
			Writer: planner.Writer{InstallID: pkiInstall, Generation: 1, Nonce: "n1"},
			Node:   "pve2", AcquiredAt: t0, ReleasedAt: t0.Add(1),
		}))
		_, err := d.Endpoint(t.Context())
		require.ErrorIs(t, err, ErrNoController)
	})
	t.Run("another install", func(t *testing.T) {
		require.NoError(t, e.store.SaveLeader(store.LeaderRecord{
			Writer: planner.Writer{InstallID: "fedcba987654", Generation: 1, Nonce: "n1"}, Node: "pve2",
		}))
		_, err := d.Endpoint(t.Context())
		require.ErrorContains(t, err, "install")
	})
}

// A host agent reads its own entry where it reads the controller's: a node
// forgotten while its agent cannot reach the controller, or is kept from it,
// learns it there.
func TestTheHostAgentLearnsItsRemovalFromTheRegistry(t *testing.T) {
	e := newEnv(t)
	d := HostDiscovery{Node: testNode, InstallID: pkiInstall, Store: e.store}
	saveLeader(t, e.store, "pve2", strings.Repeat("ab", 32))
	saveAgentAddress(t, e.store, "pve2", netip.MustParseAddr("10.0.0.2"))
	require.NoError(t, e.store.SaveNode(store.NodeEntry{Name: testNode, Since: t0, Kind: string(pki.KindHost), State: store.NodeRemoved}))

	_, err := d.Endpoint(t.Context())

	var refused *refusedError
	require.ErrorAs(t, err, &refused)
	require.True(t, refused.r.Definitive())
	require.Equal(t, agentproto.RefusalRemoved, refused.r.Code)
	require.Contains(t, refused.r.Message, "the registry")

	t.Run("also while this node holds the lease", func(t *testing.T) {
		saveLeader(t, e.store, testNode, strings.Repeat("ab", 32))
		_, err := d.Endpoint(t.Context())
		require.ErrorAs(t, err, &refused)
	})
	t.Run("not once it is enrolled again", func(t *testing.T) {
		require.NoError(t, e.store.SaveNode(store.NodeEntry{Name: testNode, Since: t0, Kind: string(pki.KindHost), State: store.NodeJoining}))
		ep, err := d.Endpoint(t.Context())
		require.NoError(t, err)
		require.True(t, ep.Self)
	})
}

func TestTheApplianceAgentTriesItsEndpointsThenTheAPI(t *testing.T) {
	d := ApplianceDiscovery{
		InstallID: pkiInstall,
		Endpoints: func() []string { return []string{"10.0.0.5:8644", "", "10.0.0.5:8644", "10.0.0.6:8644"} },
		API:       func(context.Context) ([]string, error) { return []string{"10.0.0.7:8644", "10.0.0.6:8644"}, nil },
	}
	ep, err := d.Endpoint(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"10.0.0.5:8644", "10.0.0.6:8644", "10.0.0.7:8644"}, ep.Addrs)
	require.Equal(t, pki.ControllerServerName(pkiInstall), ep.ServerName)
	require.False(t, ep.Self)

	t.Run("nothing known", func(t *testing.T) {
		d := ApplianceDiscovery{InstallID: pkiInstall, API: func(context.Context) ([]string, error) { return nil, errors.New("the API is away") }}
		_, err := d.Endpoint(t.Context())
		require.ErrorIs(t, err, ErrNoController)
		require.ErrorContains(t, err, "the API is away")
	})
}
