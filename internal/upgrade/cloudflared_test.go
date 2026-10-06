package upgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// cloudflaredRig is the rig with the packages of the manifest of release
// 1.2.4 on the release host: 2026.9.3 and 2026.10.1.
func cloudflaredRig(t *testing.T) (*rig, Manifest) {
	t.Helper()
	r := newRig(t)
	m, err := ParseManifest([]byte(manifestJSON("2026.9.3", "2026.10.1")))
	require.NoError(t, err)
	for i, e := range m.Versions {
		content := "cloudflared " + e.Version
		r.fetch.urls[e.AMD64.URL] = content
		m.Versions[i].AMD64.SHA256 = sum(content)
	}
	return r, m
}

// restarts counts the calls of a restart.
type restarts struct {
	calls int
	err   error
}

func (s *restarts) restart(context.Context) error {
	s.calls++
	return s.err
}

func TestAnUpgradeOfCloudflaredInstallsTheNewestItAllowsAndRestarts(t *testing.T) {
	r, m := cloudflaredRig(t)
	s := &restarts{}

	res, err := r.u.Cloudflared(t.Context(), Options{Yes: true}, m, s.restart)

	require.NoError(t, err)
	require.Equal(t, Result{
		Package: "cloudflared", Installed: "2026.9.3", Target: "2026.10.1",
		Kept: r.previous("cloudflared_2026.9.3_amd64.deb"),
	}, res)
	golden(t, "cloudflared_upgrade", r.transcript())
	require.Equal(t, []string{"get " + cfURL("2026.10.1", "amd64"), "get " + cfURL("2026.9.3", "amd64")}, r.fetch.asked)
	require.Equal(t, 1, s.calls)
	kept, err := os.ReadFile(res.Kept)
	require.NoError(t, err)
	require.Equal(t, "cloudflared 2026.9.3", string(kept))
	r.noDownloadsLeft()
}

func TestADeniedCloudflaredIsRefused(t *testing.T) {
	r, m := cloudflaredRig(t)
	s := &restarts{}

	_, err := r.u.Cloudflared(t.Context(), Options{Version: "2026.9.0", Yes: true}, m, s.restart)

	require.EqualError(t, err, "cloudflared 2026.9.0 is denied: cloudflare/cloudflared#1737")
	require.Empty(t, r.fetch.asked)
	require.Equal(t, "dpkg-query --show --showformat=${db:Status-Abbrev}${Version} cloudflared\n", r.transcript())
	require.Zero(t, s.calls)
}

func TestACloudflaredTheManifestDoesNotNameIsRefused(t *testing.T) {
	r, m := cloudflaredRig(t)

	_, err := r.u.Cloudflared(t.Context(), Options{Version: "2026.11.0", Yes: true}, m, nil)
	require.EqualError(t, err, "cloudflared 2026.11.0 is not among the versions the manifest allows: 2026.9.3, 2026.10.1")

	_, err = r.u.Cloudflared(t.Context(), Options{Version: "latest", Yes: true}, m, nil)
	require.EqualError(t, err, `"latest" is no version of cloudflared: want one such as 2026.9.3`)
	require.Empty(t, r.fetch.asked)
}

func TestACloudflaredNotNewerIsRefused(t *testing.T) {
	r, m := cloudflaredRig(t)
	r.host.installed["cloudflared"] = "2026.10.1"

	_, err := r.u.Cloudflared(t.Context(), Options{Version: "2026.9.3", Yes: true}, m, nil)

	require.ErrorIs(t, err, ErrNotNewer)
	require.EqualError(t, err, "cloudflared 2026.10.1 is installed, and 2026.9.3 is not newer than the installed version: "+
		"--rollback goes back to the package the last upgrade kept")
}

func TestAVersionOfCloudflaredAskedForIsInstalled(t *testing.T) {
	r, m := cloudflaredRig(t)
	r.host.installed["cloudflared"] = "2026.8.2"

	res, err := r.u.Cloudflared(t.Context(), Options{Version: "2026.9.3", Yes: true}, m, nil)

	require.NoError(t, err)
	require.Equal(t, "2026.9.3", res.Target)
	require.Equal(t, []string{"the package of cloudflared 2026.8.2 could not be kept, so --rollback cannot go back to it: " +
		"the manifest does not allow cloudflared 2026.8.2, so its package is not fetched"}, res.Notes)
}

func TestTheNewestCloudflaredInstalledIsNothingToDo(t *testing.T) {
	r, m := cloudflaredRig(t)
	r.host.installed["cloudflared"] = "2026.10.1"
	s := &restarts{}

	res, err := r.u.Cloudflared(t.Context(), Options{Yes: true}, m, s.restart)

	require.NoError(t, err)
	require.Equal(t, Result{Package: "cloudflared", Installed: "2026.10.1"}, res)
	require.Empty(t, r.fetch.asked)
	require.Zero(t, s.calls)
}

func TestAPackageOfCloudflaredThatDoesNotMatchTheManifestIsNotInstalled(t *testing.T) {
	r, m := cloudflaredRig(t)
	r.fetch.urls[cfURL("2026.10.1", "amd64")] = "changed on the way"
	s := &restarts{}

	_, err := r.u.Cloudflared(t.Context(), Options{Yes: true}, m, s.restart)

	require.EqualError(t, err, "the sha256 of cloudflared_2026.10.1_amd64.deb is "+sum("changed on the way")+
		", but the manifest says "+sum("cloudflared 2026.10.1"))
	require.NotContains(t, r.transcript(), "apt-get")
	require.Zero(t, s.calls)
	r.noDownloadsLeft()
}

func TestAFailedInstallOfCloudflaredRestartsNothing(t *testing.T) {
	r, m := cloudflaredRig(t)
	r.host.aptErr = errors.New("apt-get: exit status 100")
	s := &restarts{}

	_, err := r.u.Cloudflared(t.Context(), Options{Yes: true}, m, s.restart)

	require.EqualError(t, err, "installing cloudflared_2026.10.1_amd64.deb: apt-get: exit status 100 (pco and cloudflared are held again)")
	require.Contains(t, r.transcript(), "apt-mark hold pco cloudflared\n")
	require.Zero(t, s.calls)
}

func TestAConnectorThatIsNotReadyAfterTheUpgradeIsAnError(t *testing.T) {
	r, m := cloudflaredRig(t)
	s := &restarts{err: errors.New("the connector of tunnel t1 is not ready 1m0s after its restart")}

	_, err := r.u.Cloudflared(t.Context(), Options{Yes: true}, m, s.restart)

	require.EqualError(t, err, "cloudflared 2026.10.1 is installed, but the connector of tunnel t1 is not ready 1m0s after its restart; "+
		"pco upgrade cloudflared --rollback goes back to 2026.9.3")
}

func TestACheckOfCloudflaredChangesNothing(t *testing.T) {
	r, m := cloudflaredRig(t)
	s := &restarts{}

	res, err := r.u.Cloudflared(t.Context(), Options{Check: true}, m, s.restart)
	require.NoError(t, err)
	require.Equal(t, Result{Package: "cloudflared", Installed: "2026.9.3", Target: "2026.10.1"}, res)

	_, err = r.u.Cloudflared(t.Context(), Options{}, m, s.restart)
	require.ErrorIs(t, err, ErrNotConfirmed)
	require.Empty(t, r.fetch.asked)
	require.Zero(t, s.calls)
}

func TestARollbackOfCloudflaredInstallsTheKeptPackage(t *testing.T) {
	r, m := cloudflaredRig(t)
	r.host.installed["cloudflared"] = "2026.10.1"
	r.keepFile("cloudflared_2026.9.3_amd64.deb", "cloudflared 2026.9.3")
	s := &restarts{}

	res, err := r.u.Cloudflared(t.Context(), Options{Rollback: true, Yes: true}, m, s.restart)

	require.NoError(t, err)
	require.Equal(t, "2026.9.3", res.Target)
	golden(t, "cloudflared_rollback", r.transcript())
	require.Equal(t, 1, s.calls)
	require.Empty(t, r.fetch.asked)
}

func TestARollbackToADeniedCloudflaredIsRefused(t *testing.T) {
	r, m := cloudflaredRig(t)
	r.keepFile("cloudflared_2026.9.0_amd64.deb", "cloudflared 2026.9.0")
	s := &restarts{}

	_, err := r.u.Cloudflared(t.Context(), Options{Rollback: true, Yes: true}, m, s.restart)

	require.EqualError(t, err, "the kept cloudflared 2026.9.0 is denied: cloudflare/cloudflared#1737; it is not installed again")
	require.NotContains(t, r.transcript(), "apt-get")
	require.Zero(t, s.calls)
}

func TestTheManifestOfAReleaseIsCheckedAsItsPackagesAre(t *testing.T) {
	r := newRig(t)

	m, version, err := r.u.ReleaseManifest(t.Context(), "")
	require.NoError(t, err)
	require.Equal(t, "1.2.4", version)
	newest, ok := m.Newest("amd64")
	require.True(t, ok)
	require.Equal(t, "2026.10.1", newest.Version)
	require.Equal(t, []string{"latest", "get v1.2.4/checksums.txt", "get v1.2.4/checksums.txt.sig", "get v1.2.4/" + ManifestName}, r.fetch.asked)
	r.noDownloadsLeft()

	r.fetch.releases["1.2.4"][ManifestName] = manifestJSON("2026.11.0")
	r.u.releases = map[string]release{}
	_, _, err = r.u.ReleaseManifest(t.Context(), "1.2.4")
	require.ErrorContains(t, err, "the sha256 of cloudflared-versions.json is ")
	r.noDownloadsLeft()
}

func TestAReleaseWithoutAManifestNamesNoCloudflared(t *testing.T) {
	r := newRig(t)

	_, _, err := r.u.ReleaseManifest(t.Context(), "1.2.3")

	require.EqualError(t, err, "release v1.2.3 lists no cloudflared-versions.json in its checksums.txt, so it names no vetted cloudflared")
	require.NotContains(t, r.fetch.asked, "get v1.2.3/"+ManifestName)
}

func TestAManifestOfABadSignatureIsNotRead(t *testing.T) {
	r := newRig(t)
	r.verify.err = fmt.Errorf("%w (sqv: exit status 1)", ErrSignature)

	_, _, err := r.u.ReleaseManifest(t.Context(), "1.2.4")

	require.ErrorIs(t, err, ErrSignature)
	require.NotContains(t, r.fetch.asked, "get v1.2.4/"+ManifestName)
}
