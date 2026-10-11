package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/bundle"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	bundleInstall    = "0123456789ab"
	bundlePassphrase = "correct horse battery"
	bundleToken      = "cf-token-of-the-install"
)

// bundleWorld is a machine for pco export and pco import: a store, root, and
// no terminal.
type bundleWorld struct {
	t     *testing.T
	app   *app
	env   bundleEnv
	st    *store.Store
	paths store.Paths
	dir   string // the bundle and the passphrase file are made here
	stdin io.Reader
}

func newBundleWorld(t *testing.T) *bundleWorld {
	t.Helper()
	dir := t.TempDir()
	paths := store.Paths{
		Cluster: filepath.Join(dir, "cluster"),
		Private: filepath.Join(dir, "private"),
		Local:   filepath.Join(dir, "local"),
	}
	st, err := store.Open(paths)
	require.NoError(t, err)
	require.NoError(t, st.Init())
	return &bundleWorld{
		t: t, app: &app{env: testEnv()}, st: st, paths: paths, dir: dir, stdin: strings.NewReader(""),
		env: bundleEnv{
			euid:  func() int { return 0 },
			rand:  rand.Reader,
			paths: func() (store.Paths, error) { return paths, nil },
		},
	}
}

// withInstall makes the install of an install that was in use.
func (w *bundleWorld) withInstall() *bundleWorld {
	t := w.t
	t.Helper()
	st := w.st
	require.NoError(t, st.SaveInstall(store.Install{ID: bundleInstall, CreatedAt: t0.Add(-72 * time.Hour), Profile: store.ProfileHost}))
	settings := store.DefaultSettings()
	settings.ObserveOnly = false
	settings.GateTag = "published"
	require.NoError(t, st.SaveSettings(settings))
	require.NoError(t, st.SaveManualRoute(model.Route{
		Hostname: "grafana.example.com", ManualID: "grafana", Source: model.SourceManual,
		Target: model.Target{Scheme: model.SchemeHTTP, Addr: netip.MustParseAddr("192.0.2.10"), Port: 3000},
	}))
	require.NoError(t, st.SaveApproval(store.Approval{Owner: "qemu/101", Identity: "qemu/101 on pve1"}))
	require.NoError(t, st.SaveNode(store.NodeEntry{Name: "pve1", Version: "0.2.0", Since: t0.Add(-72 * time.Hour)}))
	ca, err := pki.NewCA(bundleInstall, t0, rand.Reader)
	require.NoError(t, err)
	certPEM, keyPEM, err := ca.PEM()
	require.NoError(t, err)
	require.NoError(t, st.SaveCA(store.CAFiles{CertPEM: string(certPEM), KeyPEM: store.NewSecret(string(keyPEM))}, nil, time.Time{}))
	require.NoError(t, st.SaveCredential(store.Credential{
		ID: "main", Label: "main", Kind: "scoped", Token: store.NewSecret(bundleToken), AddedAt: t0.Add(-70 * time.Hour),
	}))
	require.NoError(t, st.SaveLeader(store.LeaderRecord{
		Writer: planner.Writer{InstallID: bundleInstall, Generation: 7, Nonce: "n0nce"}, Node: "pve1",
	}))
	return w
}

// file is a path in the world's directory.
func (w *bundleWorld) file(name string) string { return filepath.Join(w.dir, name) }

// passphraseFile writes a file that holds text, readable by root only.
func (w *bundleWorld) passphraseFile(text string) string {
	w.t.Helper()
	path := w.file("passphrase")
	require.NoError(w.t, os.WriteFile(path, []byte(text), 0o600))
	return path
}

// terminal makes stdin a terminal that answers each prompt with the next of
// answers, and returns what the prompts were asked.
func (w *bundleWorld) terminal(answers ...string) *[]int {
	asked := &[]int{}
	w.app.stdinTerminal = func(io.Reader) (int, bool) { return 7, true }
	w.app.readPassword = func(fd int) ([]byte, error) {
		*asked = append(*asked, fd)
		require.NotEmpty(w.t, answers, "the command asked for more than the test answered")
		answer := answers[0]
		answers = answers[1:]
		return []byte(answer), nil
	}
	return asked
}

func (w *bundleWorld) run(cmd *cobra.Command, args ...string) result {
	w.t.Helper()
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(w.stdin)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(w.t.Context())
	return result{out: out.String(), errOut: errOut.String(), err: err}
}

func (w *bundleWorld) export(args ...string) result {
	w.t.Helper()
	return w.run(w.app.exportCmdWith(w.env), args...)
}

func (w *bundleWorld) importBundle(args ...string) result {
	w.t.Helper()
	return w.run(w.app.importCmdWith(w.env), args...)
}

// opened reads a bundle file back with the passphrase.
func opened(t *testing.T, path, passphrase string) bundle.Contents {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	c, err := bundle.Open(raw, []byte(passphrase))
	require.NoError(t, err)
	return c
}

func TestExportWritesTheInstallInAnEncryptedBundle(t *testing.T) {
	w := newBundleWorld(t).withInstall()
	out := w.file("pco.bundle")

	res := w.export("--passphrase-file", w.passphraseFile(bundlePassphrase+"\n"), out)

	require.NoError(t, res.err)
	require.Empty(t, res.errOut)
	info, err := os.Stat(out)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the bundle holds the key of the CA")
	c := opened(t, out, bundlePassphrase)
	require.Equal(t, bundleInstall, c.InstallID)
	require.Len(t, c.ManualRoutes, 1)
	require.Len(t, c.Approvals, 1)
	require.Len(t, c.Nodes, 1)
	require.NotNil(t, c.CA)
	require.NotEmpty(t, c.Leader)
	require.Equal(t, t0, c.MadeAt)
	require.Equal(t, cliVersion, c.Build)
	requireGolden(t, "export_done.golden", strings.ReplaceAll(res.out, out, "<bundle>"))
	require.NotContains(t, res.out+res.errOut, bundlePassphrase)
}

func TestExportHoldsNoCredentialWithoutTheFlag(t *testing.T) {
	w := newBundleWorld(t).withInstall()
	out := w.file("pco.bundle")

	res := w.export("--passphrase-file", w.passphraseFile(bundlePassphrase), out)

	require.NoError(t, res.err)
	c := opened(t, out, bundlePassphrase)
	require.Empty(t, c.Credentials)
	require.Contains(t, res.out, "none")
	require.Contains(t, res.out, "--credentials")
	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	require.NotContains(t, string(raw), bundleToken)
}

func TestExportWithCredentialsHoldsTheTokens(t *testing.T) {
	w := newBundleWorld(t).withInstall()
	out := w.file("pco.bundle")

	res := w.export("--credentials", "--passphrase-file", w.passphraseFile(bundlePassphrase), out)

	require.NoError(t, res.err)
	c := opened(t, out, bundlePassphrase)
	require.Len(t, c.Credentials, 1)
	var doc struct{ ID, Token string }
	require.NoError(t, json.Unmarshal(c.Credentials[0], &doc))
	require.Equal(t, "main", doc.ID)
	require.Equal(t, bundleToken, doc.Token, "the token, not the word that stands for it")
	require.Contains(t, res.out, "1")
	require.NotContains(t, res.out+res.errOut, bundleToken)
}

func TestExportRefusesAPassphraseUnderSixteenCharacters(t *testing.T) {
	w := newBundleWorld(t).withInstall()
	out := w.file("pco.bundle")

	res := w.export("--passphrase-file", w.passphraseFile("fifteen chars!!\n"), out)

	require.ErrorIs(t, res.err, bundle.ErrWeakPassphrase)
	require.NoFileExists(t, out)
	require.NotContains(t, res.err.Error()+res.out+res.errOut, "fifteen chars")
}

func TestExportTakesTheFirstLineOfThePassphraseFileOnly(t *testing.T) {
	for name, text := range map[string]string{
		"no newline": bundlePassphrase, "a newline": bundlePassphrase + "\n", "a carriage return too": bundlePassphrase + "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			w := newBundleWorld(t).withInstall()
			out := w.file("pco.bundle")

			require.NoError(t, w.export("--passphrase-file", w.passphraseFile(text), out).err)

			opened(t, out, bundlePassphrase)
		})
	}
	for name, text := range map[string]string{
		"two lines": bundlePassphrase + "\nanother line\n", "nothing": "", "only a newline": "\n",
	} {
		t.Run(name, func(t *testing.T) {
			w := newBundleWorld(t).withInstall()

			res := w.export("--passphrase-file", w.passphraseFile(text), w.file("pco.bundle"))

			require.Error(t, res.err)
			require.NoFileExists(t, w.file("pco.bundle"))
			require.NotContains(t, res.err.Error(), "another line")
		})
	}
}

func TestExportWarnsOfAPassphraseFileOthersCanRead(t *testing.T) {
	w := newBundleWorld(t).withInstall()
	path := w.passphraseFile(bundlePassphrase)
	require.NoError(t, os.Chmod(path, 0o644))

	res := w.export("--passphrase-file", path, w.file("pco.bundle"))

	require.NoError(t, res.err)
	require.Contains(t, res.errOut, "can be read by others")
	require.NotContains(t, res.errOut, bundlePassphrase)
}

func TestExportAsksForThePassphraseTwice(t *testing.T) {
	w := newBundleWorld(t).withInstall()
	asked := w.terminal(bundlePassphrase, bundlePassphrase)
	out := w.file("pco.bundle")

	res := w.export(out)

	require.NoError(t, res.err)
	require.Equal(t, []int{7, 7}, *asked)
	opened(t, out, bundlePassphrase)
	require.Contains(t, res.errOut, "Passphrase")
	require.NotContains(t, res.out+res.errOut, bundlePassphrase)
}

func TestExportRefusesTwoEntriesThatDiffer(t *testing.T) {
	w := newBundleWorld(t).withInstall()
	w.terminal(bundlePassphrase, bundlePassphrase+"x")
	out := w.file("pco.bundle")

	res := w.export(out)

	require.EqualError(t, res.err, "the two entries of the passphrase differ")
	require.NoFileExists(t, out)
}

func TestExportMakesAPassphraseWhenNoneIsTyped(t *testing.T) {
	w := newBundleWorld(t).withInstall()
	asked := w.terminal("")
	out := w.file("pco.bundle")

	res := w.export(out)

	require.NoError(t, res.err)
	require.Len(t, *asked, 1, "an empty answer is no passphrase to confirm")
	var passphrase string
	for _, line := range strings.Split(res.out, "\n") {
		if rest, ok := strings.CutPrefix(line, "Passphrase: "); ok {
			passphrase = rest
		}
	}
	require.Len(t, passphrase, 24)
	opened(t, out, passphrase)
	require.Equal(t, 1, strings.Count(res.out+res.errOut, passphrase), "it is shown once")
	require.Contains(t, res.out, "shown only now")
}

func TestExportDoesNotPrintAPassphraseThatWasGiven(t *testing.T) {
	w := newBundleWorld(t).withInstall()

	res := w.export("--passphrase-file", w.passphraseFile(bundlePassphrase), w.file("pco.bundle"))

	require.NoError(t, res.err)
	require.NotContains(t, res.out, "Passphrase:")
}

func TestExportWithoutATerminalOrAFileHasNoPassphrase(t *testing.T) {
	w := newBundleWorld(t).withInstall()
	out := w.file("pco.bundle")

	res := w.export(out)

	require.EqualError(t, res.err, "there is no passphrase: pass --passphrase-file, or run pco export on a terminal")
	require.NoFileExists(t, out)
}

func TestExportNeedsRoot(t *testing.T) {
	w := newBundleWorld(t).withInstall()
	w.env.euid = func() int { return 1000 }

	res := w.export("--passphrase-file", w.passphraseFile(bundlePassphrase), w.file("pco.bundle"))

	require.EqualError(t, res.err, "pco export reads the secrets of the install: run it as root")
	require.NoFileExists(t, w.file("pco.bundle"))
}

func TestExportDoesNotReplaceAFile(t *testing.T) {
	w := newBundleWorld(t).withInstall()
	out := w.file("pco.bundle")
	require.NoError(t, os.WriteFile(out, []byte("an earlier backup"), 0o600))

	res := w.export("--passphrase-file", w.passphraseFile(bundlePassphrase), out)

	require.ErrorContains(t, res.err, "already exists")
	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, "an earlier backup", string(raw))
}

// whileAsking runs do while the passphrase is being typed, after the checks
// that come before the prompt.
func (w *bundleWorld) whileAsking(do func()) {
	ask := w.app.readPassword
	w.app.readPassword = func(fd int) ([]byte, error) {
		do()
		return ask(fd)
	}
}

func TestExportDoesNotReplaceAFileThatAppearedWhileItRan(t *testing.T) {
	w := newBundleWorld(t).withInstall()
	w.terminal(bundlePassphrase, bundlePassphrase)
	out := w.file("pco.bundle")
	w.whileAsking(func() { require.NoError(t, os.WriteFile(out, []byte("an earlier backup"), 0o600)) })

	res := w.export(out)

	require.ErrorContains(t, res.err, "already exists")
	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, "an earlier backup", string(raw))
	entries, err := os.ReadDir(w.dir)
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, strings.HasSuffix(e.Name(), ".tmp"), "no temporary file is left: %s", e.Name())
	}
}

func TestExportRefusesWhenTheCAChangedWhileItRan(t *testing.T) {
	w := newBundleWorld(t).withInstall()
	w.terminal(bundlePassphrase, bundlePassphrase)
	out := w.file("pco.bundle")
	w.whileAsking(func() {
		old, _, _, err := w.st.CA()
		require.NoError(t, err)
		ca, err := pki.NewCA(bundleInstall, t0, rand.Reader)
		require.NoError(t, err)
		certPEM, keyPEM, err := ca.PEM()
		require.NoError(t, err)
		require.NoError(t, w.st.SaveCA(store.CAFiles{CertPEM: string(certPEM), KeyPEM: store.NewSecret(string(keyPEM))}, &old, t0))
	})

	res := w.export(out)

	require.ErrorContains(t, res.err, "the CA of the install changed")
	require.ErrorContains(t, res.err, "run pco export again")
	require.NoFileExists(t, out)

	w.terminal(bundlePassphrase, bundlePassphrase)
	again := w.export(out)
	require.NoError(t, again.err, "the export after the rotation carries both CAs")
	c := opened(t, out, bundlePassphrase)
	require.NotNil(t, c.PreviousCA)
	require.Contains(t, again.out, "with the previous CA of a rotation")
}

func TestExportReadsThePassphraseFromAPipeWithoutAWarning(t *testing.T) {
	w := newBundleWorld(t).withInstall()
	fifo := w.file("fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o666))
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err == nil {
			_, _ = f.WriteString(bundlePassphrase + "\n")
			_ = f.Close()
		}
	}()
	out := w.file("pco.bundle")

	res := w.export("--passphrase-file", fifo, out)

	require.NoError(t, res.err)
	require.Empty(t, res.errOut, "a pipe has no mode to warn of")
	opened(t, out, bundlePassphrase)
}

func TestExportToADirectoryThatIsNotThereFailsBeforeAskingForAPassphrase(t *testing.T) {
	w := newBundleWorld(t).withInstall()
	asked := w.terminal()

	res := w.export(filepath.Join(w.dir, "missing", "pco.bundle"))

	require.ErrorIs(t, res.err, os.ErrNotExist)
	require.Empty(t, *asked)
}

func TestExportOfAStoreWithoutAnInstall(t *testing.T) {
	w := newBundleWorld(t)

	res := w.export("--passphrase-file", w.passphraseFile(bundlePassphrase), w.file("pco.bundle"))

	require.EqualError(t, res.err, "there is no install to export: pco is not set up on this node")
	require.NoFileExists(t, w.file("pco.bundle"))
}

func TestExportOfAMachineWhereNothingWasSetUp(t *testing.T) {
	w := newBundleWorld(t)
	w.env.paths = func() (store.Paths, error) {
		return store.Paths{Cluster: w.paths.Cluster, Private: w.paths.Private, Local: filepath.Join(w.dir, "never-made")}, nil
	}

	res := w.export("--passphrase-file", w.passphraseFile(bundlePassphrase), w.file("pco.bundle"))

	require.EqualError(t, res.err, "there is no install to export: pco is not set up on this node")
}

func TestExportTakesOneFileAndPrintsNoJSON(t *testing.T) {
	w := newBundleWorld(t).withInstall()

	require.Error(t, w.export("--passphrase-file", w.passphraseFile(bundlePassphrase)).err, "no file")
	require.Error(t, w.export("--passphrase-file", w.passphraseFile(bundlePassphrase), w.file("a"), w.file("b")).err, "two files")

	w.app.json = true
	res := w.export("--passphrase-file", w.passphraseFile(bundlePassphrase), w.file("pco.bundle"))
	require.ErrorContains(t, res.err, "--json has no meaning")
}

func TestTheHelpOfExportSaysWhatTheBundleHolds(t *testing.T) {
	w := newBundleWorld(t)

	cmd := w.app.exportCmdWith(w.env)

	require.NotEmpty(t, cmd.Example)
	for _, want := range []string{
		"AES-256-GCM", "Argon2id", "16 characters", "--passphrase-file", "--credentials",
		"key of the CA", "impersonate", "never taken from an argument", "while the daemon runs",
		"previous CA", "networks", "service addresses", "claims", "tombstones", "run again",
	} {
		require.Contains(t, strings.Join(strings.Fields(cmd.Long), " "), want)
	}
}
