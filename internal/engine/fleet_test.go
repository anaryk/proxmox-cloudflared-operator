package engine

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// publishable is a snapshot of this node as a controller of the test writer
// sends it.
func publishable(seq uint64) agentproto.Snapshot {
	return agentproto.Snapshot{
		Protocol: agentproto.ProtocolMax,
		Epoch:    agentproto.Epoch{InstallID: testInstall, Generation: testWriter.Generation, Nonce: testWriter.Nonce},
		Seq:      seq,
		Node:     testNode,
		State:    agentproto.Active,
		Settings: agentproto.Settings{PollInterval: 10 * time.Second, IdentityMinimum: "port", ReverifyInterval: time.Hour},
		Deny:     agentproto.DenyList{Denied: []netip.Addr{nodeAddr}},
	}
}

func TestTheLocalFleetPublishesSynchronously(t *testing.T) {
	e := newEnv(t)
	f := e.eng.fleet

	require.NoError(t, f.Publish(t.Context(), testNode, publishable(3)))

	reports := f.Reports(t.Context(), 0, nil)
	require.Len(t, reports, 1)
	r := reports[testNode]
	require.True(t, r.Fresh)
	require.Equal(t, uint64(3), r.Status.SeqApplied, "applied before Publish returned")
	require.Equal(t, testNode, f.Local())
}

func TestTheLocalFleetHasNoAgentOfAnotherNode(t *testing.T) {
	e := newEnv(t)
	s := publishable(1)
	s.Node = "pve2"

	require.Error(t, e.eng.fleet.Publish(t.Context(), "pve2", s))
}

func TestTheLocalFleetRefusesWhatItsAgentRefuses(t *testing.T) {
	e := newEnv(t)
	f := e.eng.fleet
	require.NoError(t, f.Publish(t.Context(), testNode, publishable(3)))

	require.ErrorIs(t, f.Publish(t.Context(), testNode, publishable(2)), agentproto.ErrSeqBack)
}

func TestAFleetServesOneEngine(t *testing.T) {
	e := newEnv(t)

	_, err := New(Deps{
		Store: e.store, Inventory: e.inv, Connectors: e.conn, NewClient: e.newClient, Node: testNode, Log: zerolog.Nop(),
		Fleet: e.eng.fleet,
	})

	require.Error(t, err)
}

// Before an install is set up there is no agent; the first cycle that reads
// one makes it, of that install.
func TestTheAgentIsMadeOnceTheInstallIsKnown(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, os.Remove(filepath.Join(e.paths.Cluster, "meta", "install.json")))
	e.eng = e.newEngine()

	require.Nil(t, e.eng.Agent())
	require.Empty(t, e.eng.fleet.Reports(t.Context(), 0, nil))
	require.ErrorIs(t, e.eng.fleet.Publish(t.Context(), testNode, publishable(1)), errNoAgent)

	require.NoError(t, e.store.SaveInstall(store.Install{ID: testInstall, CreatedAt: t0}))
	e.cycle()

	require.NotNil(t, e.eng.Agent())
	require.Equal(t, testNode, e.eng.Agent().View().Node)
}

// The watch may start before the agent is made: the agent made later is
// told that it runs.
func TestTheWatchStartedBeforeTheAgentIsKept(t *testing.T) {
	e := newEnv(t)
	f := NewLocalFleet(agent.Deps{Resolver: e.res, Connectors: e.conn, Egress: e.egr})
	require.NoError(t, f.attach(e.eng))
	f.watch(true)
	require.Nil(t, f.current())

	a := f.made()

	require.NotNil(t, a)
	require.True(t, f.watching)
}
