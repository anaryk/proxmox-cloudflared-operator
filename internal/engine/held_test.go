package engine

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func tunnelView(st State, id string) (TunnelView, bool) {
	i := slices.IndexFunc(st.Tunnels, func(t TunnelView) bool { return t.ID == id })
	if i < 0 {
		return TunnelView{}, false
	}
	return st.Tunnels[i], true
}

func connectorOf(st State, id string) bool {
	return slices.ContainsFunc(st.Connectors, func(c connector.Status) bool { return c.TunnelID == id })
}

// Item 4: the tunnel of a frozen account is in the state, marked as held,
// with its connector.
func TestTheTunnelOfAFrozenAccountIsShownAsHeld(t *testing.T) {
	e, _, tun := servingThrough(t)
	e.settings(func(s *store.Settings) { s.ZonePins = map[string]string{"example.com": "cred9"} })

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	v, ok := tunnelView(st, tun.ID)
	require.True(t, ok)
	require.True(t, v.Exists)
	require.Equal(t, "account frozen: zone example.com is pinned to credential cred9, which does not see it", v.Held)
	require.True(t, connectorOf(st, tun.ID))
}

func TestATunnelWithoutAZoneIsShownAsHeld(t *testing.T) {
	e, view, tun := servingThrough(t)
	view.hide(testZone, true)
	require.NoError(t, os.Remove(e.memoryFile()))
	e.restart()

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	v, ok := tunnelView(st, tun.ID)
	require.True(t, ok)
	require.Equal(t, "serves no zone pco sees", v.Held)
	require.True(t, connectorOf(st, tun.ID))
}

func TestATunnelNoCredentialSeesIsShownAsHeld(t *testing.T) {
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

	v, ok := tunnelView(st, tun.ID)
	require.True(t, ok)
	require.Equal(t, "not visible through any credential", v.Held)
	require.Equal(t, testAccount, v.AccountID)
	require.True(t, v.Unknown, "nothing could be asked about it")
	require.True(t, connectorOf(st, tun.ID))
}

func TestHeldTunnelsAreShownInObserveModeToo(t *testing.T) {
	e, _, tun := servingThrough(t)
	e.settings(func(s *store.Settings) {
		s.ZonePins = map[string]string{"example.com": "cred9"}
		s.ObserveOnly = true
	})
	ensures := len(e.conn.ensures())

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	v, ok := tunnelView(st, tun.ID)
	require.True(t, ok)
	require.NotEmpty(t, v.Held)
	require.Len(t, e.conn.ensures(), ensures, "observing starts nothing")
}

// Item 3: the routes of an account frozen by a zone two credentials see show
// frozen, although the planner gives them no zone.
func TestTheRoutesOfATwoCredentialFreezeShowFrozen(t *testing.T) {
	e, _, _ := servingThrough(t)
	e.addSecondCredential("second-token", nil)
	require.NoError(t, os.Remove(e.memoryFile()))
	e.restart()

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	r := route(st, "www.example.com")
	require.Equal(t, RouteFrozen, r.State)
	require.Equal(t, "account frozen: zone example.com is visible through credentials cred1 and cred2 and none of them served it before", r.Reason)
}

// Pin 4: a route that lost its hostname to another owner is served by nobody
// either way; in a frozen account it still says so.
func TestARouteInConflictStaysInConflictInAFrozenAccount(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), guest(102, "web-2", "www.example.com -> :8080")))
	e.cycle()
	e.settings(func(s *store.Settings) { s.ZonePins = map[string]string{"example.com": "cred9"} })

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	states := map[string]planner.RouteState{}
	for _, r := range st.Routes {
		if r.Hostname == "www.example.com" {
			states[r.Owner] = r.State
		}
	}
	require.Equal(t, map[string]planner.RouteState{"qemu/101": RouteFrozen, "qemu/102": planner.StateConflict}, states)
}

// Item 6: a memory of another install is not this one's.
func TestAMemoryOfAnotherInstallIsSetAside(t *testing.T) {
	e := published(t)
	m, err := e.store.EngineMemory()
	require.NoError(t, err)
	require.Equal(t, testInstall, m.InstallID)
	m.InstallID = "other1"
	m.Tunnels = append(m.Tunnels, store.SeenTunnel{ID: "00000000-0000-4000-8000-0000000000aa", Name: "pco-other1", AccountID: "acc7", CredentialID: "cred7"})
	require.NoError(t, e.store.SaveEngineMemory(m))

	e.restart()
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.True(t, hasProblem(st, "the engine memory on this node is of install other1, not abc123; it is set aside"))
	keep, _ := e.conn.lastPrune()
	require.NotContains(t, keep, "00000000-0000-4000-8000-0000000000aa", "nothing of another install is kept")
	m, err = e.store.EngineMemory()
	require.NoError(t, err)
	require.Equal(t, testInstall, m.InstallID, "replaced by this install's own")
}

// Item 7: a memory that cannot be saved before the Cloudflare part holds it.
func TestAMemoryThatCannotBeSavedHoldsCloudflare(t *testing.T) {
	e := published(t)
	// A directory where the file should be: reading was done, writing fails.
	require.NoError(t, os.Remove(e.memoryFile()))
	require.NoError(t, os.Mkdir(e.memoryFile(), 0o700))
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com api.example.com -> :8080")))
	writes := e.writes()

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.True(t, hasProblem(st, "saving what the engine remembers"))
	require.Equal(t, writes, e.writes())

	require.NoError(t, os.Remove(e.memoryFile()))
	e.clock.advance(20 * time.Second)
	st = e.cycle()
	require.False(t, hasProblem(st, "remembers"))
	require.Equal(t, []string{"api.example.com", "www.example.com"}, e.recordNames())
}

// Item 7: the accounts its tunnels were seen in count for a credential that
// no longer lists them.
func TestRemovingACredentialWhoseTunnelWasSeenInAnAccountItNoLongerListsIsRefused(t *testing.T) {
	e := newEnv(t)
	other := cffake.New()
	other.AddAccount("acc2", "Other")
	seeded := other.SeedTunnel("acc2", tunnelName, nil)
	view := newZoneView(other)
	view.hideAccount("acc2")
	e.addSecondCredential("other-token", view)
	require.NoError(t, e.store.SaveEngineMemory(store.EngineMemory{
		InstallID: testInstall,
		Tunnels:   []store.SeenTunnel{{ID: seeded.ID, Name: tunnelName, AccountID: "acc2", CredentialID: "cred2"}},
	}))

	err := e.eng.RemoveCredential(t.Context(), "cred2")

	require.ErrorIs(t, err, ErrRefused)
	require.Contains(t, err.Error(), "tunnel pco-abc123 in account acc2")
}
