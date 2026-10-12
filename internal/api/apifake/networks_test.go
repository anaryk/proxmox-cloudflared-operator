package apifake

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func TestTheNetworksOfTheScenarioAreAttachedAndDetached(t *testing.T) {
	e, _ := load(t, "populated")
	d := serve(t, e)
	ctx := context.Background()

	nets, err := d.Networks(ctx)
	require.NoError(t, err)
	require.Equal(t, e.State().Networks, nets)
	n, err := d.Network(ctx, "vmbr2")
	require.NoError(t, err)
	require.Equal(t, []string{"zabbix.example.com"}, n.Served)
	candidates, err := d.CandidateNetworks(ctx)
	require.NoError(t, err)
	require.Len(t, candidates, len(nets))

	res, _, err := d.ProbeNetwork(ctx, engine.ProbeRequest{Bridge: "vmbr2"})
	require.NoError(t, err)
	require.True(t, res.Free)
	require.Equal(t, netip.MustParseAddr("10.50.0.254"), res.Address)
	_, _, err = d.ProbeNetwork(ctx, engine.ProbeRequest{Bridge: "vmbr7"})
	require.ErrorIs(t, err, engine.ErrInvalid)

	v, err := d.Attach(ctx, engine.AttachRequest{Bridge: "vmbr7", Prefix: netip.MustParsePrefix("10.77.0.0/24"), Address: netip.MustParseAddr("10.77.0.250")})
	require.NoError(t, err)
	require.Equal(t, 3, v.Mark)
	_, err = d.Attach(ctx, engine.AttachRequest{Bridge: "vmbr7", Prefix: netip.MustParsePrefix("10.77.0.0/24"), Address: netip.MustParseAddr("10.77.0.250")})
	require.ErrorIs(t, err, engine.ErrRefused)
	require.Len(t, e.State().Networks, 3)

	require.ErrorIs(t, d.Detach(ctx, "vmbr2", 3, nil), engine.ErrRefused, "a route goes through it")
	require.ErrorIs(t, d.Detach(ctx, "vmbr2", 3, []string{"old.example.com"}), engine.ErrRefused, "not the route it serves")
	require.ErrorIs(t, d.Detach(ctx, "vmbr2", 2, []string{"zabbix.example.com"}), engine.ErrRefused, "a stale revision")
	require.NoError(t, d.Detach(ctx, "vmbr2", 3, []string{"zabbix.example.com"}))
	require.NoError(t, d.Detach(ctx, "vmbr7", 1, nil))
	require.ErrorIs(t, d.Detach(ctx, "vmbr7", 1, nil), engine.ErrNotFound)
	require.Len(t, e.State().Networks, 1)
}
