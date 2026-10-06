package setup

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
