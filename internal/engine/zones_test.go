package engine

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// servingThrough is an engine that serves www.example.com in enforce mode,
// with the first credential reaching Cloudflare through view.
func servingThrough(t *testing.T) (*env, zoneView, cfapi.Tunnel) {
	t.Helper()
	e := newEnv(t)
	view := newZoneView(e.cf)
	e.useAPI(testToken, view)
	e.enforce()
	st := e.cycle()
	require.Empty(t, st.Problems)
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
	return e, view, e.tunnels()[0]
}

// requireUntouched checks that Cloudflare kept the tunnel and the record as
// they were and that the connector of the tunnel is kept by the last prune.
func requireUntouched(t *testing.T, e *env, writes []string, tun cfapi.Tunnel) {
	t.Helper()
	require.Equal(t, writes, e.writes(), "nothing is written")
	require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules())
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
	keep, pruned := e.conn.lastPrune()
	require.True(t, pruned)
	require.Contains(t, keep, tun.ID, "the connector is kept")
}

func noDNSIn(t *testing.T, calls []string, zoneID string) {
	t.Helper()
	for _, c := range calls {
		require.False(t, strings.Contains(c, "Record") && strings.Contains(c, zoneID), "DNS does nothing in the zone: %s", c)
	}
}

// Reproduced (a): a listing that succeeds without the zone.
func TestAZoneThatLeavesItsListingFreezesItsAccount(t *testing.T) {
	e, view, tun := servingThrough(t)
	writes := e.writes()

	view.hide(testZone, true)
	for range 3 {
		e.clock.advance(zoneRefreshEvery)
		n := len(e.cf.Calls())
		st := e.cycle()

		require.Contains(t, st.Problems, "zone example.com is no longer listed by credential cred1; account acc1 is left as it is "+
			"until the zone is listed again or pco apply --confirm-deletes confirms it is gone")
		require.Equal(t, planner.StateActive, route(st, "www.example.com").State, "its rules are still planned")
		noDNSIn(t, e.callsSince(n), testZone)
		requireUntouched(t, e, writes, tun)
	}

	view.hide(testZone, false)
	e.clock.advance(zoneRefreshEvery)
	st := e.cycle()
	require.Empty(t, st.Problems, "listed again, the zone is no longer in doubt")
}

func TestAConfirmationAcceptsAZoneThatLeftItsListing(t *testing.T) {
	e := newEnv(t)
	e.cf.AddZone("zone2", "example.net", testAccount)
	view := newZoneView(e.cf)
	e.useAPI(testToken, view)
	e.enforce()
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com www.example.net -> :8080")))
	e.cycle()
	both := withSentinel(hostRule("www.example.com"), hostRule("www.example.net"))
	require.Equal(t, both, e.rules())

	view.hide("zone2", true)
	e.clock.advance(zoneRefreshEvery)
	st := e.cycle()
	require.True(t, hasProblem(st, "zone example.net is no longer listed by credential cred1"))
	require.Equal(t, both, e.rules(), "the whole account is left as it is")

	e.apply(true)
	e.clock.advance(20 * time.Second)
	st = e.cycle()

	require.False(t, hasProblem(st, "no longer listed"))
	require.Equal(t, planner.StateNoZone, route(st, "www.example.net").State)
	require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules(),
		"confirmed gone, the zone no longer holds its account back")
}

// A node with no memory of the zone, after a restart: the account has none.
func TestAnAccountWithoutAZoneKeepsItsTunnel(t *testing.T) {
	e, view, tun := servingThrough(t)
	writes := e.writes()
	view.hide(testZone, true)

	require.NoError(t, os.Remove(e.memoryFile()))
	e.restart()
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Contains(t, st.Problems, "tunnel pco-abc123 in account acc1 serves no zone pco sees; the tunnel and its connector are left as they are")
	requireUntouched(t, e, writes, tun)
}

// Reproduced (b).
func TestAPinToACredentialThatDoesNotSeeTheZoneFreezesItsAccount(t *testing.T) {
	e, _, tun := servingThrough(t)
	writes := e.writes()
	e.settings(func(s *store.Settings) { s.ZonePins = map[string]string{"example.com": "cred9"} })

	e.clock.advance(20 * time.Second)
	n := len(e.cf.Calls())
	st := e.cycle()

	require.Contains(t, st.Problems, "zone example.com is pinned to credential cred9, which does not see it; account acc1 is left as it is until the pin is fixed")
	noDNSIn(t, e.callsSince(n), testZone)
	requireUntouched(t, e, writes, tun)
}

// Reproduced (c).
func TestARemovedCredentialLeavesTheConnectorsOfItsTunnels(t *testing.T) {
	e := newEnv(t)
	other := cffake.New()
	other.AddAccount("acc2", "Other")
	other.AddZone("zone2", "example.org", "acc2")
	e.addSecondCredential("other-token", other)
	e.enforce()
	e.cycle()
	tun := e.tunnels()[0]

	require.NoError(t, e.store.DeleteCredential(testCred))
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Contains(t, st.Problems, "tunnel pco-abc123 in account acc1 is not visible through any credential; its connector is kept")
	keep, pruned := e.conn.lastPrune()
	require.True(t, pruned)
	require.Contains(t, keep, tun.ID)
}

func TestATunnelDeletedAtCloudflareIsPrunedOnceEverythingAnswered(t *testing.T) {
	e, _, tun := servingThrough(t)
	e.settings(func(s *store.Settings) { s.ZonePins = map[string]string{"example.com": "cred9"} })
	e.clock.advance(20 * time.Second)
	e.cycle()
	keep, _ := e.conn.lastPrune()
	require.Equal(t, []string{tun.ID}, keep)

	require.NoError(t, e.cf.DeleteTunnel(t.Context(), testAccount, tun.ID))
	e.clock.advance(20 * time.Second)
	e.cycle()

	keep, _ = e.conn.lastPrune()
	require.Empty(t, keep, "a tunnel Cloudflare no longer has loses its connector")
}

func TestNoPruneUnlessEveryAccountAndTunnelAnswered(t *testing.T) {
	t.Run("account listing failed", func(t *testing.T) {
		e, _, _ := servingThrough(t)
		prunes := len(e.conn.prunes())
		e.cf.FailNext("accounts", 1, errors.New("connection reset"))

		e.clock.advance(20 * time.Second)
		st := e.cycle()

		require.Len(t, e.conn.prunes(), prunes)
		require.Contains(t, st.Problems, "connectors are not pruned in this cycle: listing the accounts of credential cred1 failed: connection reset")
	})
	t.Run("tunnel lookup failed", func(t *testing.T) {
		e, _, _ := servingThrough(t)
		e.settings(func(s *store.Settings) { s.ZonePins = map[string]string{"example.com": "cred9"} })
		e.clock.advance(20 * time.Second)
		e.cycle()
		prunes := len(e.conn.prunes())
		e.cf.FailNext("tunnel.read", 1, errors.New("connection reset"))

		e.clock.advance(20 * time.Second)
		st := e.cycle()

		require.Len(t, e.conn.prunes(), prunes)
		require.Contains(t, st.Problems, "connectors are not pruned in this cycle: looking up the tunnel of account acc1 failed: connection reset")
	})
}

// Reproduced (d), and A6.
func TestASecondCredentialForAServedZone(t *testing.T) {
	e, _, tun := servingThrough(t)
	writes := e.writes()
	// The same Cloudflare account, reached with another token.
	e.addSecondCredential("second-token", nil)

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Contains(t, st.Problems, "zone example.com is visible through credentials cred1 and cred2; pin it with zonePins (cred1 serves it until then)")
	require.Equal(t, planner.StateActive, route(st, "www.example.com").State)
	requireUntouched(t, e, writes, tun)

	// A restart remembers who served the zone.
	e.restart()
	e.clock.advance(20 * time.Second)
	st = e.cycle()

	require.Contains(t, st.Problems, "zone example.com is visible through credentials cred1 and cred2; pin it with zonePins (cred1 serves it until then)")
	require.Equal(t, planner.StateActive, route(st, "www.example.com").State)
	requireUntouched(t, e, writes, tun)

	// Without that memory, as on a node that never served it, the account
	// is frozen.
	require.NoError(t, os.Remove(e.memoryFile()))
	e.restart()
	e.clock.advance(20 * time.Second)
	st = e.cycle()

	require.Contains(t, st.Problems, "zone example.com is visible through credentials cred1 and cred2 and none of them served it before; "+
		"pin it with zonePins; account acc1 is left as it is until then")
	requireUntouched(t, e, writes, tun)

	e.settings(func(s *store.Settings) { s.ZonePins = map[string]string{"example.com": "cred2"} })
	e.clock.advance(20 * time.Second)
	st = e.cycle()

	require.Empty(t, st.Problems)
	require.Equal(t, planner.StateActive, route(st, "www.example.com").State)
	requireUntouched(t, e, writes, tun)
}

// A zone in doubt freezes its whole account: the plan of the account's other
// zones would PUT the tunnel without the rules of the zone in doubt.
func TestAZoneInDoubtFreezesTheOtherZonesOfItsAccount(t *testing.T) {
	e := newEnv(t)
	e.cf.AddZone("zone2", "example.net", testAccount)
	e.enforce()
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com www.example.net -> :8080")))
	e.cycle()
	both := withSentinel(hostRule("www.example.com"), hostRule("www.example.net"))
	require.Equal(t, both, e.rules())
	// A second token sees example.net only.
	second := newZoneView(e.cf)
	second.hide(testZone, true)
	e.addSecondCredential("second-token", second)

	require.NoError(t, os.Remove(e.memoryFile()))
	e.restart()
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Contains(t, st.Problems, "zone example.net is visible through credentials cred1 and cred2 and none of them served it before; "+
		"pin it with zonePins; account acc1 is left as it is until then")
	require.Equal(t, both, e.rules(), "the rules of example.com stay, and those of example.net with them")
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
	require.Len(t, e.cf.RecordsIn("zone2"), 1)
}

func TestTheSameTokenTwiceIsRefused(t *testing.T) {
	e := newEnv(t)

	_, err := e.eng.AddCredential(t.Context(), "again", testToken)

	require.ErrorIs(t, err, ErrInvalid)
	require.Contains(t, err.Error(), `credential "main" has this token already`)
	require.Empty(t, e.cf.Calls(), "refused before anything is asked")
	creds, err := e.store.Credentials()
	require.NoError(t, err)
	require.Len(t, creds, 1)
}

func TestAZoneThatIsNotActiveIsLeftOut(t *testing.T) {
	e := newEnv(t)
	view := newZoneView(e.cf)
	view.setStatus(testZone, "pending")
	e.useAPI(testToken, view)
	e.enforce()

	st := e.cycle()

	require.Equal(t, planner.StateNoZone, route(st, "www.example.com").State)
	require.Empty(t, e.writes())
	noDNSIn(t, e.cf.Calls(), testZone)
}
