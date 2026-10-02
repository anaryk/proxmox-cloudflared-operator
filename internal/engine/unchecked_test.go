package engine

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
		Unchecked:   true,
	}}, st.Tunnels)
	require.Equal(t, problemNoWriter, st.Hold)

	require.NoError(t, e.store.SaveWriter(testWriter))
	e.clock.advance(20 * time.Second)
	st = e.cycle()
	require.Empty(t, st.Tunnels[0].Held, "a cycle that checks takes the mark away")
	require.False(t, st.Tunnels[0].Unchecked)
	require.True(t, st.Tunnels[0].Verified)
	require.Empty(t, st.Hold)
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
		Unchecked:   true,
	}}, st.Tunnels)
	require.Equal(t, testAccount, wwwRoute(st, "qemu/101").Account)
}

// A cycle that holds before it gets to plan marks what it carries all the
// same, with the reason it held: that the inventory is incomplete, not what
// the inventory said first.
func TestACycleThatHoldsEarlyMarksTheTunnelsItCarries(t *testing.T) {
	e := contested(t)
	e.inv.set(incomplete("cluster status: no quorum", webOne, webTwo))

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Len(t, st.Tunnels, 1)
	require.Equal(t, "not checked in the last cycle: "+problemIncomplete, st.Tunnels[0].Held)
	require.True(t, st.Tunnels[0].Unchecked)
	require.False(t, st.Tunnels[0].Verified)
	require.Equal(t, problemIncomplete, st.Hold)
}

// A fresh process whose first cycle holds before it plans shows no tunnel,
// but says why it did not check Cloudflare.
func TestAHoldBeforeAnyTunnelIsKnownIsNamed(t *testing.T) {
	e := contested(t)
	e.inv.set(incomplete("cluster status: no quorum", webOne, webTwo))

	e.restart()
	st := e.cycle()

	require.Empty(t, st.Tunnels)
	require.Equal(t, problemIncomplete, st.Hold)
}

// The reproduced sequence: node pve2 is offline, which the inventory notes
// while it stays complete, and then another writer takes over during the
// tunnel run. The tunnel is held for the writer, not for the note.
func TestAHoldIsNamedByItsOwnReasonNotByTheFirstProblem(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	var armed atomic.Bool
	e.useAPI(testToken, hookedAPI{API: e.cf, before: func(method string) {
		if method == "FindTunnel" && armed.CompareAndSwap(true, false) {
			require.NoError(t, e.store.SaveWriter(takeover))
		}
	}})
	e.cycle()
	snap := snapshot(guest(101, "web-1", "www.example.com -> :8080"))
	snap.Problems = []string{"node pve2 is offline; using cached data for its guests"}
	e.inv.set(snap)
	armed.Store(true)

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Equal(t, "stale", st.WriterVerdict)
	require.Contains(t, st.Problems, "node pve2 is offline; using cached data for its guests")
	require.Len(t, st.Tunnels, 1)
	require.Equal(t, "not checked in the last cycle: the tunnel run found a stale writer", st.Tunnels[0].Held)
	require.True(t, st.Tunnels[0].Unchecked)
	require.Equal(t, "the tunnel run found a stale writer", st.Hold)
}

// A cycle has checked Cloudflare only when its DNS run looked at the records
// as the writer: one that listed them and then found another writer, and one
// that could not start, did not.
func TestACycleChecksOnlyWhenItsDNSRunLookedAsTheWriter(t *testing.T) {
	t.Run("another writer takes over after the records were listed", func(t *testing.T) {
		e := newEnv(t)
		e.enforce()
		var once sync.Once
		e.useAPI(testToken, hookedAPI{API: e.cf, before: func(method string) {
			if method == "Records" {
				once.Do(func() { require.NoError(t, e.store.SaveWriter(takeover)) })
			}
		}})

		st := e.cycle()

		require.Equal(t, "stale", st.WriterVerdict)
		require.Empty(t, e.records(), "nothing was written as the stale writer")
		require.Len(t, st.Tunnels, 1)
		require.True(t, st.Tunnels[0].Unchecked)
		require.False(t, st.Tunnels[0].Verified)
		require.Equal(t, "not checked in the last cycle: the DNS run found a stale writer", st.Tunnels[0].Held)
		require.Equal(t, "the DNS run found a stale writer", st.Hold)
	})
	t.Run("the DNS run cannot read the writer identity", func(t *testing.T) {
		e := newEnv(t)
		e.enforce()
		path := filepath.Join(e.paths.Cluster, "meta", "leader.json")
		var once sync.Once
		e.conn.onEnsure = func() { once.Do(func() { require.NoError(t, os.WriteFile(path, []byte("{"), 0o600)) }) }

		st := e.cycle()

		require.Equal(t, "unknown", st.WriterVerdict)
		require.Zero(t, dnsCalls(e.cf.Calls()), "the DNS run did not look")
		require.Len(t, st.Tunnels, 1)
		require.True(t, st.Tunnels[0].Unchecked)
		require.True(t, strings.HasPrefix(st.Hold, "the DNS run did not look at the records: dns: reading the writer identity: "), st.Hold)
		require.True(t, strings.HasSuffix(st.Hold, "leader.json: invalid JSON at offset 1"), st.Hold)
		require.Equal(t, "not checked in the last cycle: "+st.Hold, st.Tunnels[0].Held)
	})
}
