package daemon

import (
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
)

// On a node whose controller runs elsewhere, the agent drives the gateway,
// and what it gives it is kept as the cache a boot restores the namespace
// from, as the engine keeps it where it runs.
func TestTheAgentKeepsTheCacheOfTheGatewayItDrives(t *testing.T) {
	b := &fakeBackend{}
	local := t.TempDir()
	g := newGatewayPart(b, local, func() time.Time { return t0 }, zerolog.Nop())
	d := agentDeps(nil, nil, nil, g, nil)
	require.NotNil(t, d.Runner, "the agent writes guests' cards with qm and pct")

	_, err := d.Gateway.Sync(t.Context(), testCache.Legs, testCache.Services, nil, nil)

	require.NoError(t, err)
	cached, found, err := gateway.ReadCache(local)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, testCache.Legs, cached.Legs)
	require.Equal(t, testCache.Services, cached.Services)
	require.Equal(t, testCache.Legs, g.wantedNow().Legs, "the keeper brings back what the agent gave")
}
