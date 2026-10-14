package setup

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
)

// infoLines are the lines setup printed as information.
func (e *testEnv) infoLines() []string {
	var out []string
	for _, l := range e.ask.lines {
		if s, ok := strings.CutPrefix(l, "info: "); ok {
			out = append(out, s)
		}
	}
	return out
}

// The appliance's uninstall on the node lists what the install has at
// Cloudflare through its purge, which prints one object a line.
func TestPurgeListsWhatItWouldDelete(t *testing.T) {
	e := newTestEnv(t)
	e.installed(Manifest{})
	e.atCloudflare()

	require.NoError(t, e.s.Purge(t.Context(), true))
	e.done()

	require.Equal(t, []string{
		"DNS record CNAME www.example.com in zone example.com",
		"tunnel pco-" + testInstall + " (" + tunnelID + ") in account " + testAccount,
	}, e.infoLines())
	e.requireNoDelete()
	e.requireStoreKept()
}

func TestPurgeDeletesWithTheDaemonAndTheConnectorsStopped(t *testing.T) {
	e := newTestEnv(t)
	e.installed(Manifest{})
	e.atCloudflare()
	e.script(serviceStopped(), connectorsPruned())

	require.NoError(t, e.s.Purge(t.Context(), false))
	e.done()

	require.Equal(t, []string{"rec-other-install", "rec-by-hand"}, e.recordIDs())
	require.Equal(t, []string{"pco-ba9876543210"}, e.tunnelNames())
	at := func(prefix string) int {
		t.Helper()
		i := slices.IndexFunc(e.events, func(ev string) bool { return strings.HasPrefix(ev, prefix) })
		require.GreaterOrEqual(t, i, 0, "%s happened", prefix)
		return i
	}
	require.IsIncreasing(t, []int{
		at("run systemctl disable --now pco.service"),
		at("cf DeleteRecord " + testZone + " rec-ours"),
		at("run systemctl disable --now -- pco-cloudflared@"),
		at("cf DeleteTunnel " + testAccount + " " + tunnelID),
	})
	e.requireStoreKept()
	e.requireNoSecret()
}

func TestPurgeWithoutCredentialsReachesNothing(t *testing.T) {
	e := newTestEnv(t)
	e.installed(Manifest{})
	e.atCloudflare()
	creds, err := e.st.Credentials()
	require.NoError(t, err)
	for _, c := range creds {
		require.NoError(t, e.st.DeleteCredential(c.ID))
	}

	require.NoError(t, e.s.Purge(t.Context(), true))
	require.Empty(t, e.infoLines(), "nothing printed is nothing to delete")
	require.NoError(t, e.s.Purge(t.Context(), false))
	e.done()

	require.Contains(t, e.ask.text(), "at Cloudflare: nothing pco can reach")
	e.requireNoDelete()
}

// The tunnels pinned to the nodes of a cluster are the install's as much as
// its shared one.
func TestPurgeTakesTheTunnelsPinnedToNodes(t *testing.T) {
	e := newTestEnv(t)
	e.installed(Manifest{})
	e.atCloudflare()
	pinned := e.cf.SeedTunnel(testAccount, "pco-"+testInstall+"-pve2", nil)
	e.cf.SeedTunnel(testAccount, "pco-ba9876543210-pve2", nil)

	require.NoError(t, e.s.Purge(t.Context(), true))
	require.Contains(t, e.infoLines(), "tunnel pco-"+testInstall+"-pve2 ("+pinned.ID+") in account "+testAccount)

	e.script(serviceStopped(), connectorsPruned())
	require.NoError(t, e.s.Purge(t.Context(), false))
	e.done()
	require.Equal(t, []string{"pco-ba9876543210", "pco-ba9876543210-pve2"}, e.tunnelNames())
}

// A connector of another node that stood down only just now, or one whose
// end Cloudflare has not counted yet, keeps a tunnel from being deleted: its
// connections are ended first.
func TestUninstallEndsTheConnectionsOfATunnelBeforeItsDelete(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.atCloudflare()
	e.cf.SetConnectors(testAccount, tunnelID, []cfapi.Connector{{ID: "conn-of-pve2", Connections: 4}})
	e.script(connectorsSeen(), egressSeen(), userRead(), serviceStopped(), connectorsPruned(), egressDeleted(), userRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true}))
	e.done()

	require.Equal(t, []string{"pco-ba9876543210"}, e.tunnelNames())
	clean := slices.Index(e.events, "cf CleanUpConnections "+testAccount+" "+tunnelID)
	del := slices.Index(e.events, "cf DeleteTunnel "+testAccount+" "+tunnelID)
	require.GreaterOrEqual(t, clean, 0)
	require.Greater(t, del, clean)
	e.requireStoreGone()
}

// A connector that connects again after its connections were ended is given
// a few seconds to stand down before the uninstall gives up on the tunnel.
func TestTheDeleteOfATunnelWaitsForConnectorsThatConnectAgain(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.atCloudflare()
	back := &connectingAgain{fake: e.cf, times: 2}
	e.api = func(api cfapi.API) cfapi.API { back.API = api; return back }
	var waited []time.Duration
	e.s.host.sleep = func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil }
	e.script(connectorsSeen(), egressSeen(), userRead(), serviceStopped(), connectorsPruned(), egressDeleted(), userRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true}))
	e.done()

	require.Equal(t, []time.Duration{deleteWait, deleteWait}, waited)
	require.Equal(t, []string{"pco-ba9876543210"}, e.tunnelNames())
	e.requireStoreGone()
}

// connectingAgain is a Cloudflare where a connector of the install's tunnel
// connects again right after its connections are ended, times times.
type connectingAgain struct {
	cfapi.API
	fake  *cffake.Fake
	times int
}

func (c *connectingAgain) CleanUpConnections(ctx context.Context, accountID, tunnelID string) error {
	if err := c.API.CleanUpConnections(ctx, accountID, tunnelID); err != nil {
		return err
	}
	if c.times > 0 {
		c.times--
		c.fake.SetConnectors(accountID, tunnelID, []cfapi.Connector{{ID: "conn-again", Connections: 4}})
	}
	return nil
}
