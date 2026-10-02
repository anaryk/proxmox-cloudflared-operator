package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// The reviewed sequence: www is served by qemu/101 on :8080, the writer
// identity becomes unusable, the admin resolves to qemu/102 and a cycle runs.
// Cloudflare still has :8080; the state must not show the tunnel as verified.
func TestATunnelACycleDidNotCheckIsShownAsHeld(t *testing.T) {
	e := contested(t)
	tun := e.tunnels()[0]
	require.NoError(t, os.Remove(filepath.Join(e.paths.Cluster, "meta", "leader.json")))
	require.NoError(t, e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102"))

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules(), "Cloudflare still has :8080")
	require.Equal(t, &planner.IngressRule{Hostname: "www.example.com", Service: "http://10.0.0.11:9090"}, wwwRoute(st, "qemu/102").Rule,
		"the plan has :9090")
	require.Equal(t, []TunnelView{{
		TunnelState: reconcile.TunnelState{AccountID: testAccount, CredentialID: testCred, Name: tunnelName, ID: tun.ID, Version: 1, Exists: true},
		Held:        "not checked in the last cycle: no writer identity; run pco setup",
	}}, st.Tunnels)

	require.NoError(t, e.store.SaveWriter(testWriter))
	e.clock.advance(20 * time.Second)
	st = e.cycle()
	require.Empty(t, st.Tunnels[0].Held, "a cycle that checks takes the mark away")
	require.True(t, st.Tunnels[0].Verified)
}

// After a restart under a hold the state has no tunnel it found; the one the
// plan wants is shown as held, not as missing.
func TestATunnelNotCheckedSinceARestartIsShownAsHeld(t *testing.T) {
	e := contested(t)
	require.NoError(t, os.Remove(filepath.Join(e.paths.Cluster, "meta", "leader.json")))

	e.restart()
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Equal(t, []TunnelView{{
		TunnelState: reconcile.TunnelState{AccountID: testAccount, CredentialID: testCred, Name: tunnelName, Unknown: true},
		Held:        "not checked in the last cycle: no writer identity; run pco setup",
	}}, st.Tunnels)
	require.Equal(t, testAccount, wwwRoute(st, "qemu/101").Account)
}

// A cycle that holds before it gets to plan marks what it carries all the
// same, with the reason it held.
func TestACycleThatHoldsEarlyMarksTheTunnelsItCarries(t *testing.T) {
	e := contested(t)
	e.inv.set(incomplete("cluster status: no quorum", webOne, webTwo))

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Len(t, st.Tunnels, 1)
	require.Equal(t, "not checked in the last cycle: cluster status: no quorum", st.Tunnels[0].Held)
	require.False(t, st.Tunnels[0].Verified)
}
