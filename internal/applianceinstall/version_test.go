package applianceinstall

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A release build carries its tag, with the v; the files of its release do
// not, and the installer looks for them by the version without it.
func TestTheInstallerNamesTheFilesOfItsReleaseWithoutTheV(t *testing.T) {
	require.Equal(t, "0.1.0-rc.6", releaseVersion("v0.1.0-rc.6"))
	require.Equal(t, "1.2.3", releaseVersion("1.2.3"))
	require.Equal(t, "pco-appliance_0.1.0-rc.6_amd64.tar.zst", templateName(releaseVersion("v0.1.0-rc.6"), "amd64"))
	require.Equal(t, "https://github.com/anaryk/proxmox-cloudflared-operator/releases/download/v0.1.0-rc.6",
		ReleaseBase(releaseVersion("v0.1.0-rc.6")))
}
