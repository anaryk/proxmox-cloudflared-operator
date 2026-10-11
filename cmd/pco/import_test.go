package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/bundle"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The bundle of an install that was in use, with its credentials or without,
// is sealed once for all the tests: sealing costs what it should cost an
// attacker.
var sealedBundles [2]struct {
	once sync.Once
	raw  []byte
}

// bundleOf writes the bundle file an export of an install that was in use
// left, and returns its path.
func bundleOf(t *testing.T, credentials bool) string {
	t.Helper()
	made := &sealedBundles[0]
	if credentials {
		made = &sealedBundles[1]
	}
	made.once.Do(func() {
		from := newBundleWorld(t).withInstall()
		c, err := bundle.Collect(t.Context(), from.st, credentials, t0, cliVersion)
		require.NoError(t, err)
		made.raw, err = bundle.Seal(c, []byte(bundlePassphrase), from.env.rand)
		require.NoError(t, err)
	})
	path := filepath.Join(t.TempDir(), "pco.bundle")
	require.NoError(t, os.WriteFile(path, made.raw, 0o600))
	return path
}

func TestImportRestoresTheInstallIntoAStoreThatHasNone(t *testing.T) {
	file := bundleOf(t, true)
	w := newBundleWorld(t)

	res := w.importBundle("--passphrase-file", w.passphraseFile(bundlePassphrase+"\n"), file)

	require.NoError(t, res.err)
	require.Empty(t, res.errOut)
	requireGolden(t, "import_done.golden", strings.ReplaceAll(res.out, file, "<bundle>"))
	inst, found, err := w.st.Install()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, bundleInstall, inst.ID)
	routes, err := w.st.ManualRoutes()
	require.NoError(t, err)
	require.Len(t, routes, 1)
	creds, err := w.st.Credentials()
	require.NoError(t, err)
	require.Len(t, creds, 1)
	require.Equal(t, bundleToken, creds[0].Token.Reveal())
	settings, err := w.st.Settings()
	require.NoError(t, err)
	require.True(t, settings.ObserveOnly, "the restored install publishes nothing until pco apply")
	require.Equal(t, "published", settings.GateTag)
	leader, _, found, err := w.st.Leader()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, 7, leader.Generation)
	require.NotContains(t, res.out+res.errOut, bundlePassphrase)
	require.NotContains(t, res.out+res.errOut, bundleToken)
}

func TestImportLeavesTheBundleAsItIs(t *testing.T) {
	file := bundleOf(t, false)
	before, err := os.ReadFile(file)
	require.NoError(t, err)
	w := newBundleWorld(t)

	require.NoError(t, w.importBundle("--passphrase-file", w.passphraseFile(bundlePassphrase), file).err)

	after, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, before, after)
	creds, err := w.st.Credentials()
	require.NoError(t, err)
	require.Empty(t, creds, "a bundle made without the credentials restores none")
}

func TestImportRefusesAStoreThatHoldsAnInstall(t *testing.T) {
	file := bundleOf(t, true)
	w := newBundleWorld(t)
	require.NoError(t, w.st.SaveInstall(store.Install{ID: "ba9876543210", CreatedAt: t0, Profile: store.ProfileHost}))

	res := w.importBundle("--passphrase-file", w.passphraseFile(bundlePassphrase), file)

	require.ErrorIs(t, res.err, bundle.ErrInstalled)
	require.ErrorContains(t, res.err, "ba9876543210")
	require.Empty(t, res.out)
	inst, _, err := w.st.Install()
	require.NoError(t, err)
	require.Equal(t, "ba9876543210", inst.ID)
	routes, err := w.st.ManualRoutes()
	require.NoError(t, err)
	require.Empty(t, routes)
}

func TestImportRefusesAnInstalledStoreBeforeAskingForAPassphrase(t *testing.T) {
	file := bundleOf(t, false)
	w := newBundleWorld(t).withInstall()
	asked := w.terminal()

	res := w.importBundle(file)

	require.ErrorIs(t, res.err, bundle.ErrInstalled)
	require.Empty(t, *asked, "no passphrase was asked for")
}

func TestImportWithAWrongPassphraseWritesNothing(t *testing.T) {
	file := bundleOf(t, true)
	w := newBundleWorld(t)

	res := w.importBundle("--passphrase-file", w.passphraseFile("correct horse batterz"), file)

	require.ErrorIs(t, res.err, bundle.ErrWrongPassphrase)
	require.NotContains(t, res.err.Error(), "batterz")
	_, found, err := w.st.Install()
	require.NoError(t, err)
	require.False(t, found)
	require.Empty(t, res.out)
}

func TestImportOfAFileThatIsNoBundleOrIsCutShort(t *testing.T) {
	file := bundleOf(t, false)
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	w := newBundleWorld(t)
	pass := w.passphraseFile(bundlePassphrase)
	cut := w.file("cut.bundle")
	require.NoError(t, os.WriteFile(cut, raw[:30], 0o600))
	text := w.file("text")
	require.NoError(t, os.WriteFile(text, []byte(strings.Repeat("a note about the backup\n", 10)), 0o600))

	require.ErrorIs(t, w.importBundle("--passphrase-file", pass, cut).err, bundle.ErrTruncated)
	require.ErrorIs(t, w.importBundle("--passphrase-file", pass, text).err, bundle.ErrNotBundle)
	require.ErrorIs(t, w.importBundle("--passphrase-file", pass, w.file("missing")).err, os.ErrNotExist)
}

func TestImportRefusesAFileNoBundleCouldBeTheSizeOf(t *testing.T) {
	w := newBundleWorld(t)
	big := w.file("big")
	f, err := os.Create(big)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(maxBundleSize+1))
	require.NoError(t, f.Close())

	res := w.importBundle("--passphrase-file", w.passphraseFile(bundlePassphrase), big)

	require.ErrorContains(t, res.err, "too large to be a pco bundle")
}

func TestImportAsksForThePassphraseOnce(t *testing.T) {
	file := bundleOf(t, false)
	w := newBundleWorld(t)
	asked := w.terminal(bundlePassphrase)

	res := w.importBundle(file)

	require.NoError(t, res.err)
	require.Equal(t, []int{7}, *asked)
	require.Contains(t, res.errOut, "Passphrase of the bundle")
	require.NotContains(t, res.out+res.errOut, bundlePassphrase)
}

func TestImportWithoutATerminalOrAFileHasNoPassphrase(t *testing.T) {
	file := bundleOf(t, false)
	w := newBundleWorld(t)

	res := w.importBundle(file)

	require.EqualError(t, res.err, "there is no passphrase: pass --passphrase-file, or run pco import on a terminal")
}

func TestImportNeedsRoot(t *testing.T) {
	w := newBundleWorld(t)
	w.env.euid = func() int { return 1000 }

	res := w.importBundle("--passphrase-file", w.passphraseFile(bundlePassphrase), w.file("pco.bundle"))

	require.EqualError(t, res.err, "pco import writes the store of the node: run it as root")
}

func TestImportRefusesWhileTheDaemonRuns(t *testing.T) {
	file := bundleOf(t, false)
	w := newBundleWorld(t)
	lock, err := os.OpenFile(w.paths.NodeLock(), os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Close() })
	require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))

	res := w.importBundle("--passphrase-file", w.passphraseFile(bundlePassphrase), file)

	require.ErrorContains(t, res.err, "a pco daemon runs on this node")
	require.ErrorContains(t, res.err, "systemctl stop pco")
	_, found, err := w.st.Install()
	require.NoError(t, err)
	require.False(t, found)
}

func TestImportIsForTheHost(t *testing.T) {
	file := bundleOf(t, false)
	w := newBundleWorld(t)
	profile := w.file("profile")
	require.NoError(t, os.WriteFile(profile, []byte("appliance\n"), 0o644))
	w.app.profileFile = profile

	res := w.importBundle("--passphrase-file", w.passphraseFile(bundlePassphrase), file)

	require.ErrorContains(t, res.err, "pco import restores the install of a host")
	_, found, err := w.st.Install()
	require.NoError(t, err)
	require.False(t, found)
}

func TestImportMakesTheRootsOfAStoreThatWasNeverUsed(t *testing.T) {
	file := bundleOf(t, false)
	w := newBundleWorld(t)
	fresh := filepath.Join(w.dir, "fresh")
	require.NoError(t, os.Mkdir(fresh, 0o700))
	w.env.paths = func() (store.Paths, error) {
		return store.Paths{Cluster: filepath.Join(fresh, "cluster"), Private: filepath.Join(fresh, "private"), Local: filepath.Join(fresh, "local")}, nil
	}

	res := w.importBundle("--passphrase-file", w.passphraseFile(bundlePassphrase), file)

	require.NoError(t, res.err)
	require.DirExists(t, filepath.Join(fresh, "cluster"))
	require.DirExists(t, filepath.Join(fresh, "private"))
}

func TestImportTakesOneFileAndPrintsNoJSON(t *testing.T) {
	w := newBundleWorld(t)
	pass := w.passphraseFile(bundlePassphrase)

	require.Error(t, w.importBundle("--passphrase-file", pass).err, "no file")
	w.app.json = true
	require.ErrorContains(t, w.importBundle("--passphrase-file", pass, w.file("x")).err, "--json has no meaning")
}

func TestTheHelpOfImportSaysWhereItRunsAndWhatComesNext(t *testing.T) {
	w := newBundleWorld(t)

	cmd := w.app.importCmdWith(w.env)

	require.NotEmpty(t, cmd.Example)
	long := strings.Join(strings.Fields(cmd.Long), " ")
	for _, want := range []string{"pco promote", "observe-only", "holds an install", "as root", "pco setup", "--passphrase-file"} {
		require.Contains(t, long, want)
	}
}
