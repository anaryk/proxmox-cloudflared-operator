package setup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// --yes alone would remove the credentials and leave what the install has at
// Cloudflare with nothing on the node to remove it.
func TestYesAloneRefusesToOrphanCloudflareObjects(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.atCloudflare()

	err := e.uninstall(UninstallOptions{Yes: true})

	require.ErrorContains(t, err, "--purge-cloudflare")
	require.ErrorContains(t, err, "--keep-cloudflare")
	require.ErrorContains(t, err, "nothing on this node can remove them later")
	require.Empty(t, e.run.ran, "refused before anything is looked at or stopped")
	require.Empty(t, e.cf.Calls())
	require.Len(t, e.tunnelNames(), 2)
	e.requireStoreKept()
}

func TestYesWithAFlagForCloudflareGoesOn(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options UninstallOptions
		purged  bool
	}{
		{"--keep-cloudflare", UninstallOptions{Yes: true, KeepCloudflare: true}, false},
		{"--purge-cloudflare", UninstallOptions{Yes: true, PurgeCloudflare: true}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installed(Manifest{InstalledCloudflared: true})
			e.atCloudflare()
			e.script(connectorsSeen(), noEgressSeen(), serviceStopped(), connectorsPruned())

			require.NoError(t, e.uninstall(tt.options))
			e.done()

			if tt.purged {
				require.Equal(t, []string{"pco-ba9876543210"}, e.tunnelNames())
			} else {
				require.Empty(t, e.cf.Calls(), "Cloudflare is not looked at")
				require.Len(t, e.tunnelNames(), 2)
				e.requireShown("nothing on this node can remove it later")
			}
			// cloudflared stays unless asked for.
			e.requireShown("cloudflared: kept")
			e.requireStoreGone()
		})
	}
}

func TestYesAloneIsEnoughWithoutAnInstallOrACredential(t *testing.T) {
	for _, tt := range []struct {
		name string
		take func(e *testEnv)
	}{
		{"no credential", func(e *testEnv) { require.NoError(t, e.st.DeleteCredential("c0ffee00")) }},
		{"no install", func(e *testEnv) {
			require.NoError(t, os.Remove(filepath.Join(e.paths.Cluster, "meta", "install.json")))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installed(Manifest{})
			e.atCloudflare()
			tt.take(e)
			e.script(connectorsSeen(), noEgressSeen(), serviceStopped(), connectorsPruned())

			require.NoError(t, e.uninstall(UninstallOptions{Yes: true}))
			e.done()

			require.Empty(t, e.cf.Calls())
			e.requireStoreGone()
		})
	}
}
