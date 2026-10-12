package agent

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

func TestARemovedNodeStandsDown(t *testing.T) {
	e := newEnv(t)
	e.gw = &fakeGateway{}
	e.agent = e.newAgent()
	e.apply(pinned(e.snapshot("www.example.com -> :8080")))
	e.conn.reset()
	sets := e.egr.setCount()

	require.NoError(t, e.agent.StandDown(t.Context(), agentproto.Refusal{Code: agentproto.RefusalRemoved, Message: "node pve1 was forgotten"}))

	require.Equal(t, sets+1, e.egr.setCount())
	got, _ := e.egr.last()
	require.Empty(t, got, "the egress set is emptied")
	require.Equal(t, []string{"StopAll", "PruneInstall "}, e.conn.changes(), "stopped and disabled, then their files and tokens removed")
	_, found, err := e.store.AgentSnapshot()
	require.NoError(t, err)
	require.False(t, found, "the desired state goes, run tokens and all")
	require.Empty(t, e.gw.syncs, "the gateway stays until pco uninstall")
	v := e.agent.View()
	require.True(t, strings.HasPrefix(v.StoodDown, "this node was removed from install abc123 at 2026-10-05T12:00:00Z; "+
		"pco setup (host) or a new join token (appliance) enrols it again"), v.StoodDown)
	require.Contains(t, e.agent.Status(t.Context()).Problems, v.StoodDown)

	t.Run("and serves nothing after a restart", func(t *testing.T) {
		e.agent = e.newAgent()
		e.conn.reset()
		require.NoError(t, e.agent.Boot(t.Context()))
		_, err := e.agent.Reverify(t.Context())
		require.NoError(t, err)

		require.Empty(t, e.conn.changes())
	})
}

func TestAContestedNodeKeepsTheNonSecretPartsOfItsDesiredState(t *testing.T) {
	e := newEnv(t)
	s := pinned(e.snapshot("www.example.com -> :8080"))
	e.apply(s)

	require.NoError(t, e.agent.StandDown(t.Context(), agentproto.Refusal{Code: agentproto.RefusalContested}))

	raw, found, err := e.store.AgentSnapshot()
	require.NoError(t, err)
	require.True(t, found)
	require.NotContains(t, string(raw), sharedToken)
	require.NotContains(t, string(raw), pinnedToken)
	require.Contains(t, string(raw), "www.example.com")
	require.Contains(t, e.agent.View().StoodDown, "contested")

	e.conn.reset()
	_, err = e.agent.Reverify(t.Context())
	require.NoError(t, err)
	require.Empty(t, e.conn.changes(), "a node that stood down serves nothing until a controller serves it again")
	got, _ := e.egr.last()
	require.Empty(t, got)

	e.apply(pinned(e.snapshot("www.example.com -> :8080")))
	require.Empty(t, e.agent.View().StoodDown, "a controller that serves it again takes it back")
	require.Contains(t, e.conn.changes(), "Ensure "+sharedID+" "+sharedToken)
}

func TestARefusalThatIsNotDefinitiveIsNoReasonToStandDown(t *testing.T) {
	e := newEnv(t)
	e.apply(e.snapshot("www.example.com -> :8080"))
	e.conn.reset()

	err := e.agent.StandDown(t.Context(), agentproto.Refusal{Code: agentproto.RefusalOutOfRange, Message: "protocol 2 to 2"})

	require.ErrorContains(t, err, "not definitive")
	require.Empty(t, e.conn.changes())
	require.Empty(t, e.agent.View().StoodDown)
}

func TestAStandDownThatFailsInPartGoesOnAndSaysWhat(t *testing.T) {
	e := newEnv(t)
	e.apply(e.snapshot("www.example.com -> :8080"))
	e.conn.fail["StopAll"] = errBoom

	err := e.agent.StandDown(t.Context(), agentproto.Refusal{Code: agentproto.RefusalNotEnrolled})

	require.ErrorIs(t, err, errBoom)
	require.Contains(t, e.conn.changes(), "PruneInstall ", "the tokens go all the same")
	require.NotEmpty(t, e.agent.View().StoodDown)
	require.Empty(t, e.agent.EgressSet())
	saved, _, err := e.store.AgentEgress()
	require.NoError(t, err)
	require.Equal(t, []netip.AddrPort{}, saved)

	t.Run("a desired state that cannot be forgotten is remembered as it is", func(t *testing.T) {
		e := newEnv(t)
		e.apply(e.snapshot("www.example.com -> :8080"))
		desired := filepath.Join(e.paths.Local, "agent", "desired.json")
		require.NoError(t, os.Remove(desired))
		require.NoError(t, os.MkdirAll(filepath.Join(desired, "held"), 0o700))

		err := e.agent.StandDown(t.Context(), agentproto.Refusal{Code: agentproto.RefusalRemoved})

		require.ErrorContains(t, err, "forgetting the desired state")
		require.NotNil(t, e.agent.desired, "what is on disk is what is remembered")
	})
}
