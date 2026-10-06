package upgrade

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	debOld = "the package of pco 1.2.3"
	debNew = "the package of pco 1.2.4"
)

// rig is an appliance with pco 1.2.3 and cloudflared 2026.9.3 installed, and a
// release host with the releases 1.2.3 and 1.2.4.
type rig struct {
	t      *testing.T
	work   string
	fetch  *fakeFetcher
	verify *fakeVerifier
	host   *fakeHost
	run    *fakeRunner
	paths  store.Paths
	said   []string
	u      *Upgrader
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, work: t.TempDir()}
	r.fetch = &fakeFetcher{t: t, dir: r.work, latest: "1.2.4", urls: map[string]string{}}
	r.fetch.addRelease("1.2.3", map[string]string{"pco_1.2.3_amd64.deb": debOld, "pco_1.2.3_arm64.deb": "arm64"})
	r.fetch.addRelease("1.2.4", map[string]string{
		"pco_1.2.4_amd64.deb": debNew, "pco_1.2.4_arm64.deb": "arm64",
		ManifestName: manifestJSON("2026.9.3", "2026.10.1"),
	})
	r.verify = &fakeVerifier{}
	r.host = &fakeHost{
		installed:   map[string]string{"pco": "1.2.3", "cloudflared": "2026.9.3"},
		runs:        true,
		versionJSON: `{"version":"v1.2.3","commit":"abc","date":"2026-09-01","schemaVersion":1}`,
	}
	r.run = &fakeRunner{do: r.host.do}
	base := t.TempDir()
	r.paths = store.Paths{Cluster: filepath.Join(base, "cluster"), Private: filepath.Join(base, "private"), Local: base}
	r.writeObject(filepath.Join(r.paths.Cluster, "routes", "a.json"), 1)
	clock := &fakeClock{now: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)}
	r.u = New(r.run, r.fetch, r.verify, "/usr/share/pco/release-key.gpg", r.work, "amd64", clock.Now).
		WithStore(r.paths).
		WithProgress(func(format string, args ...any) { r.said = append(r.said, fmt.Sprintf(format, args...)) })
	return r
}

func (r *rig) writeObject(path string, schema int) {
	r.t.Helper()
	require.NoError(r.t, os.MkdirAll(filepath.Dir(path), 0o700))
	content := fmt.Sprintf(`{"schemaVersion":%d,"rev":1,"id":"%s","data":{}}`, schema, filepath.Base(path))
	require.NoError(r.t, os.WriteFile(path, []byte(content), 0o600))
}

func (r *rig) previous(name string) string { return filepath.Join(r.work, "previous", name) }

func (r *rig) keepFile(name, content string) {
	r.t.Helper()
	require.NoError(r.t, os.MkdirAll(filepath.Join(r.work, "previous"), 0o700))
	require.NoError(r.t, os.WriteFile(r.previous(name), []byte(content), 0o600))
}

func (r *rig) transcript() string { return transcript(r.run.lines(), r.work) }

// noDownloadsLeft says that every file downloaded was removed or kept.
func (r *rig) noDownloadsLeft() {
	r.t.Helper()
	entries, err := os.ReadDir(r.work)
	require.NoError(r.t, err)
	for _, e := range entries {
		require.Equal(r.t, "previous", e.Name(), "nothing but the kept packages is left in the work directory")
	}
}

func TestAnUpgradeOfPcoInstallsTheSignedPackageAndKeepsTheOldOne(t *testing.T) {
	r := newRig(t)

	res, err := r.u.Pco(t.Context(), Options{Yes: true})

	require.NoError(t, err)
	require.Equal(t, Result{
		Package: "pco", Installed: "1.2.3", Target: "1.2.4", Release: "1.2.4",
		Fingerprint: fprPrimary, Kept: r.previous("pco_1.2.3_amd64.deb"),
	}, res)
	golden(t, "pco_upgrade", r.transcript())
	require.Equal(t, []string{
		"latest",
		"get v1.2.4/checksums.txt", "get v1.2.4/checksums.txt.sig", "get v1.2.4/pco_1.2.4_amd64.deb",
		"get v1.2.3/checksums.txt", "get v1.2.3/checksums.txt.sig", "get v1.2.3/pco_1.2.3_amd64.deb",
	}, r.fetch.asked)
	require.Equal(t, []string{"verify with /usr/share/pco/release-key.gpg", "verify with /usr/share/pco/release-key.gpg"}, r.verify.asked)
	kept, err := os.ReadFile(res.Kept)
	require.NoError(t, err)
	require.Equal(t, debOld, string(kept))
	r.noDownloadsLeft()
	require.Contains(t, r.said, "release v1.2.4: checksums.txt is signed by key "+fprPrimary)
}

func TestABadSignatureStopsBeforeThePackageIsDownloaded(t *testing.T) {
	r := newRig(t)
	r.verify.err = ErrSignature

	_, err := r.u.Pco(t.Context(), Options{Yes: true})

	require.ErrorIs(t, err, ErrSignature)
	require.Equal(t, []string{"latest", "get v1.2.4/checksums.txt", "get v1.2.4/checksums.txt.sig"}, r.fetch.asked)
	require.Equal(t, queryPco, r.transcript(), "apt-get is not run")
	r.noDownloadsLeft()
}

func TestChecksumsSignedForAnotherFileStopTheUpgrade(t *testing.T) {
	r := newRig(t)
	rel := r.fetch.releases["1.2.4"]
	rel["checksums.txt.sig"] = "signed: something else"

	_, err := r.u.Pco(t.Context(), Options{Yes: true})

	require.ErrorIs(t, err, ErrSignature)
	require.NotContains(t, r.fetch.asked, "get v1.2.4/pco_1.2.4_amd64.deb")
}

func TestAPackageThatDoesNotMatchItsChecksumIsNotInstalled(t *testing.T) {
	r := newRig(t)
	r.fetch.releases["1.2.4"]["pco_1.2.4_amd64.deb"] = "a package changed on the way"

	_, err := r.u.Pco(t.Context(), Options{Yes: true})

	require.ErrorContains(t, err, "the sha256 of pco_1.2.4_amd64.deb is "+sum("a package changed on the way")+
		", but checksums.txt of release v1.2.4 says "+sum(debNew))
	require.Equal(t, queryPco, r.transcript(), "apt-get is not run")
	r.noDownloadsLeft()
}

func TestAReleaseWithoutThePackageIsRefused(t *testing.T) {
	r := newRig(t)
	r.fetch.addRelease("1.2.4", map[string]string{"pco_1.2.4_arm64.deb": "arm64"})

	_, err := r.u.Pco(t.Context(), Options{Yes: true})

	require.EqualError(t, err, "checksums.txt of release v1.2.4 has no line for pco_1.2.4_amd64.deb")
}

func TestAReleaseThatListsThePackageTwiceIsRefused(t *testing.T) {
	r := newRig(t)
	r.fetch.addRelease("1.2.4", map[string]string{"pco_1.2.4_amd64.deb": debNew}, sum("other")+"  pco_1.2.4_amd64.deb")

	_, err := r.u.Pco(t.Context(), Options{Yes: true})

	require.EqualError(t, err, "checksums.txt of release v1.2.4 has 2 lines for pco_1.2.4_amd64.deb, expected one")
}

func TestAPackageKeptAlreadyIsKeptAndTheOlderOneGoes(t *testing.T) {
	r := newRig(t)
	r.keepFile("pco_1.2.3_amd64.deb", debOld)
	r.keepFile("pco_1.2.1_amd64.deb", "older")
	r.keepFile("cloudflared_2026.8.2_amd64.deb", "cloudflared")

	res, err := r.u.Pco(t.Context(), Options{Yes: true})

	require.NoError(t, err)
	require.Equal(t, r.previous("pco_1.2.3_amd64.deb"), res.Kept)
	require.NotContains(t, r.fetch.asked, "get v1.2.3/checksums.txt", "the release of the kept version is not fetched again")
	require.NoFileExists(t, r.previous("pco_1.2.1_amd64.deb"))
	require.FileExists(t, r.previous("cloudflared_2026.8.2_amd64.deb"), "the kept package of the other package stays")
}

// A version without a release to fetch its package from is upgraded all the
// same; the admin is told that there is no going back but the snapshot.
func TestAnUpgradeGoesOnWhenTheOldPackageCannotBeKept(t *testing.T) {
	r := newRig(t)
	delete(r.fetch.releases, "1.2.3")

	res, err := r.u.Pco(t.Context(), Options{Yes: true})

	require.NoError(t, err)
	require.Empty(t, res.Kept)
	require.Equal(t, []string{"the package of pco 1.2.3 could not be kept, so --rollback cannot go back to it: " +
		"downloading checksums.txt of release v1.2.3: GET v1.2.3/checksums.txt: 404 Not Found"}, res.Notes)
	golden(t, "pco_upgrade", r.transcript())
	r.noDownloadsLeft()
}

func TestTheHoldIsSetAgainAfterAFailedInstall(t *testing.T) {
	r := newRig(t)
	r.host.aptErr = errors.New("apt-get: exit status 100: E: Sub-process /usr/bin/dpkg returned an error code (1)")

	_, err := r.u.Pco(t.Context(), Options{Yes: true})

	require.EqualError(t, err, "installing pco_1.2.4_amd64.deb: apt-get: exit status 100: E: Sub-process /usr/bin/dpkg returned an error code (1) "+
		"(pco and cloudflared are held again)")
	golden(t, "pco_upgrade_failed", r.transcript())
	r.noDownloadsLeft()
}

func TestAHoldThatCannotBeSetIsAnError(t *testing.T) {
	r := newRig(t)
	r.host.holdErr = errors.New("apt-mark: exit status 100")

	_, err := r.u.Pco(t.Context(), Options{Yes: true})

	require.ErrorContains(t, err, "is installed, but holding pco and cloudflared again failed: apt-mark: exit status 100; run apt-mark hold pco cloudflared")
}

func TestAVersionBelowOrAtTheInstalledOneIsRefused(t *testing.T) {
	for _, v := range []string{"1.2.2", "1.2.3", "v1.2.3", "1.2.3-rc.1"} {
		t.Run(v, func(t *testing.T) {
			r := newRig(t)

			_, err := r.u.Pco(t.Context(), Options{Version: v, Yes: true})

			require.ErrorIs(t, err, ErrNotNewer)
			require.ErrorContains(t, err, "pco 1.2.3 is installed, and ")
			require.ErrorContains(t, err, "--rollback goes back to the package the last upgrade kept")
			require.Empty(t, r.fetch.asked)
			require.Equal(t, queryPco, r.transcript())
		})
	}
}

func TestAVersionAboveTheInstalledOneIsTakenAsItIs(t *testing.T) {
	r := newRig(t)
	r.fetch.latest = "1.3.0"

	res, err := r.u.Pco(t.Context(), Options{Version: "v1.2.4", Yes: true})

	require.NoError(t, err)
	require.Equal(t, "1.2.4", res.Target)
	require.NotContains(t, r.fetch.asked, "latest")
}

// The installed 1.2.4~rc.1 is the release 1.2.4-rc.1, which 1.2.4 follows.
func TestAPreReleaseIsFollowedByItsRelease(t *testing.T) {
	r := newRig(t)
	r.host.installed["pco"] = "1.2.4~rc.1"

	res, err := r.u.Pco(t.Context(), Options{Check: true})

	require.NoError(t, err)
	require.Equal(t, Result{Package: "pco", Installed: "1.2.4-rc.1", Target: "1.2.4", Release: "1.2.4"}, res)
}

// The installed 1.2.4~rc.1 is the release 1.2.4-rc.1 itself, whatever dpkg
// would make of the name of the release.
func TestAPreReleaseIsNotUpgradedToItself(t *testing.T) {
	r := newRig(t)
	r.host.installed["pco"] = "1.2.4~rc.1"
	r.fetch.latest = "1.2.4-rc.1"

	res, err := r.u.Pco(t.Context(), Options{Check: true})

	require.NoError(t, err)
	require.Empty(t, res.Target)
}

func TestTheNewestReleaseInstalledIsNothingToDo(t *testing.T) {
	r := newRig(t)
	r.host.installed["pco"] = "1.2.4"

	res, err := r.u.Pco(t.Context(), Options{Yes: true})

	require.NoError(t, err)
	require.Equal(t, Result{Package: "pco", Installed: "1.2.4"}, res)
	require.Equal(t, []string{"latest"}, r.fetch.asked)
}

func TestACheckChangesNothing(t *testing.T) {
	r := newRig(t)

	res, err := r.u.Pco(t.Context(), Options{Check: true})

	require.NoError(t, err)
	require.Equal(t, Result{Package: "pco", Installed: "1.2.3", Target: "1.2.4", Release: "1.2.4"}, res)
	require.Equal(t, []string{"latest"}, r.fetch.asked)
	require.Equal(t, queryPco, r.transcript())
}

func TestAnUpgradeThatWasNotConfirmedChangesNothing(t *testing.T) {
	r := newRig(t)

	res, err := r.u.Pco(t.Context(), Options{})

	require.ErrorIs(t, err, ErrNotConfirmed)
	require.Equal(t, "1.2.4", res.Target)
	require.Equal(t, []string{"latest"}, r.fetch.asked)
}

func TestAPackageThatIsNotInstalledIsNoUpgrade(t *testing.T) {
	r := newRig(t)
	delete(r.host.installed, "pco")

	_, err := r.u.Pco(t.Context(), Options{Yes: true})

	require.ErrorContains(t, err, "asking dpkg for the version of pco: dpkg-query: exit status 1: no packages found matching pco")
}

// What dpkg-query says of a package, as want, status, error flag and
// version: on the appliance pco and cloudflared are held.
func TestThePackageIsTakenInTheStatesDpkgCallsInstalled(t *testing.T) {
	for _, tt := range []struct{ out, refusal string }{
		{"hold installed ok 1.2.3", ""},
		{"install installed ok 1.2.3", ""},
		{"hold installed ok 1.2.3\n", ""},
		{"deinstall installed ok 1.2.3", "pco is installed but selected for deinstall: apt-mark hold pco selects it again"},
		{"hold installed reinstreq 1.2.3", "pco needs to be reinstalled, dpkg says: apt-get install --reinstall pco"},
		{"hold unpacked ok 1.2.3", "pco is unpacked but not configured: a dpkg run did not finish, and dpkg --configure -a finishes it"},
		{"hold half-configured ok 1.2.3", "pco is half-configured: a dpkg run did not finish, and dpkg --configure -a finishes it"},
		{"hold half-installed ok 1.2.3", "pco is half-installed: a dpkg run did not finish, and dpkg --configure -a finishes it"},
		{"hold triggers-awaited ok 1.2.3", "pco is waiting for triggers: a dpkg run did not finish, and dpkg --configure -a finishes it"},
		{"hold triggers-pending ok 1.2.3", "pco is waiting for triggers: a dpkg run did not finish, and dpkg --configure -a finishes it"},
		{"deinstall config-files ok 1.2.3", "pco is removed; only its configuration files are left"},
		{"unknown not-installed ok ", "dpkg says \"unknown not-installed ok\" of pco, not its state and version"},
		{"hold installed ok", "dpkg says \"hold installed ok\" of pco, not its state and version"},
		{"", "dpkg says \"\" of pco, not its state and version"},
	} {
		t.Run(tt.out, func(t *testing.T) {
			r := newRig(t)
			r.run.do = func(name string, args ...string) (string, error) {
				if name == "dpkg-query" {
					return tt.out, nil
				}
				return r.host.do(name, args...)
			}

			res, err := r.u.Pco(t.Context(), Options{Check: true})

			if tt.refusal == "" {
				require.NoError(t, err)
				require.Equal(t, "1.2.3", res.Installed)
				return
			}
			require.EqualError(t, err, tt.refusal)
		})
	}
}

func TestARollbackInstallsTheKeptPackageAndAllowsTheDowngrade(t *testing.T) {
	r := newRig(t)
	r.host.installed["pco"] = "1.2.4"
	r.keepFile("pco_1.2.3_amd64.deb", debOld)

	res, err := r.u.Pco(t.Context(), Options{Rollback: true, Yes: true})

	require.NoError(t, err)
	require.Equal(t, Result{Package: "pco", Installed: "1.2.4", Target: "1.2.3", Kept: r.previous("pco_1.2.3_amd64.deb")}, res)
	golden(t, "pco_rollback", r.transcript())
	require.Empty(t, r.fetch.asked, "a rollback downloads nothing")
	r.noDownloadsLeft()
}

func TestARollbackIsRefusedWhenTheKeptPcoCannotReadTheStore(t *testing.T) {
	r := newRig(t)
	r.host.installed["pco"] = "1.2.4"
	r.keepFile("pco_1.2.3_amd64.deb", debOld)
	newer := filepath.Join(r.paths.Private, "credentials", "c.json")
	r.writeObject(newer, 2)

	_, err := r.u.Pco(t.Context(), Options{Rollback: true, Yes: true})

	require.EqualError(t, err, "the store holds "+newer+" with schema version 2, and pco 1.2.3 reads 1 at most: "+
		"after a rollback the daemon could not read its store; the snapshot taken before the upgrade goes back with the store")
	require.NotContains(t, r.transcript(), "apt-get")
	r.noDownloadsLeft()
}

func TestAKeptPcoThatReadsANewerSchemaMayRollBack(t *testing.T) {
	r := newRig(t)
	r.host.installed["pco"] = "1.2.4"
	r.host.versionJSON = `{"schemaVersion":2}`
	r.keepFile("pco_1.2.3_amd64.deb", debOld)
	r.writeObject(filepath.Join(r.paths.Private, "credentials", "c.json"), 2)

	_, err := r.u.Pco(t.Context(), Options{Rollback: true, Yes: true})

	require.NoError(t, err)
}

// A pco from before version --json reads schema version 1.
func TestAKeptPcoThatCannotSayItsSchemaReadsTheFirst(t *testing.T) {
	r := newRig(t)
	r.host.installed["pco"] = "1.2.4"
	r.host.versionJSON = ""
	r.keepFile("pco_1.2.3_amd64.deb", debOld)

	_, err := r.u.Pco(t.Context(), Options{Rollback: true, Yes: true})
	require.NoError(t, err)
	golden(t, "pco_rollback_before_json", r.transcript())

	r.writeObject(filepath.Join(r.paths.Cluster, "routes", "b.json"), 2)
	_, err = r.u.Pco(t.Context(), Options{Rollback: true, Yes: true})
	require.ErrorContains(t, err, "with schema version 2, and pco 1.2.3 reads 1 at most")
}

func TestAKeptPcoThatDoesNotRunIsNotRolledBackTo(t *testing.T) {
	r := newRig(t)
	r.host.installed["pco"] = "1.2.4"
	r.host.runs = false
	r.keepFile("pco_1.2.3_amd64.deb", debOld)

	_, err := r.u.Pco(t.Context(), Options{Rollback: true, Yes: true})

	require.EqualError(t, err, "the kept pco 1.2.3 does not run: exec format error")
}

func TestAKeptPcoThatNamesNoSchemaIsNotRolledBackTo(t *testing.T) {
	r := newRig(t)
	r.host.installed["pco"] = "1.2.4"
	r.host.versionJSON = `{"version":"v1.2.3"}`
	r.keepFile("pco_1.2.3_amd64.deb", debOld)

	_, err := r.u.Pco(t.Context(), Options{Rollback: true, Yes: true})

	require.EqualError(t, err, "the kept pco 1.2.3 names no schema version it reads")
}

func TestARollbackNeedsAKeptPackageOtherThanTheInstalledOne(t *testing.T) {
	r := newRig(t)
	_, err := r.u.Pco(t.Context(), Options{Rollback: true, Yes: true})
	require.ErrorIs(t, err, ErrNothingKept)
	require.ErrorContains(t, err, "holds no package of pco")

	r.keepFile("pco_1.2.3_amd64.deb", debOld)
	_, err = r.u.Pco(t.Context(), Options{Rollback: true, Yes: true})
	require.ErrorIs(t, err, ErrNothingKept)
	require.ErrorContains(t, err, "pco 1.2.3 is installed, and it is the package kept")

	r.keepFile("pco_1.2.3_arm64.deb", "arm64")
	r.host.installed["pco"] = "1.2.4"
	_, err = r.u.Pco(t.Context(), Options{Rollback: true, Yes: true})
	require.NoError(t, err, "the package of another architecture is not this one's")
	require.NotContains(t, r.transcript(), "--allow-downgrades "+r.previous("pco_1.2.3_arm64.deb"))
}

func TestARollbackNeedsTheStoreToCheck(t *testing.T) {
	r := newRig(t)
	r.host.installed["pco"] = "1.2.4"
	r.keepFile("pco_1.2.3_amd64.deb", debOld)
	u := New(r.run, r.fetch, r.verify, "/k.gpg", r.work, "amd64", time.Now)

	_, err := u.Pco(t.Context(), Options{Rollback: true, Yes: true})

	require.EqualError(t, err, "a rollback of pco needs the store to check, and none was given")
}

func TestARollbackChecksBeforeItAsks(t *testing.T) {
	r := newRig(t)
	r.host.installed["pco"] = "1.2.4"
	r.keepFile("pco_1.2.3_amd64.deb", debOld)

	res, err := r.u.Pco(t.Context(), Options{Rollback: true, Check: true})
	require.NoError(t, err)
	require.Equal(t, "1.2.3", res.Target)
	require.NotContains(t, r.transcript(), "apt-get")

	_, err = r.u.Pco(t.Context(), Options{Rollback: true})
	require.ErrorIs(t, err, ErrNotConfirmed)
	require.NotContains(t, r.transcript(), "apt-get")
}

func TestTheSnapshotIsNamedForTheDay(t *testing.T) {
	require.Equal(t, "pco-pre-upgrade-20261006", newRig(t).u.SnapshotName())
}

func TestTheWorkDirectoryIsOneRunsAtATime(t *testing.T) {
	r := newRig(t)
	require.NoError(t, os.WriteFile(filepath.Join(r.work, ".fetch-123"), []byte("left by a killed run"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(r.work, ".extract-456", "usr"), 0o700))

	unlock, err := r.u.Lock()
	require.NoError(t, err)
	_, err = r.u.Lock()
	require.EqualError(t, err, "another pco upgrade runs (it holds "+filepath.Join(r.work, ".lock")+")")
	entries, err := os.ReadDir(r.work)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	require.Equal(t, []string{".lock", "previous"}, names, "what a run left is gone")

	unlock()
	unlock, err = r.u.Lock()
	require.NoError(t, err)
	unlock()
}
