package applianceinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// takenBack fails unless the node holds nothing the run made: what the admin
// had stays as it was.
func (e *testEnv) takenBack() {
	e.t.Helper()
	require.Empty(e.t, e.node.cts, "the container")
	require.Empty(e.t, e.node.pools, "the pool")
	require.Equal(e.t, []string{"root@pam"}, e.node.userIDs(), "the user")
	require.Nil(e.t, e.node.roles["PCO"], "the role")
	require.Equal(e.t, []string{"admin-only"}, e.node.tags, "the registered tags")
	require.Empty(e.t, e.node.leaked, "a token removed with its user leaves its secret")
	require.Empty(e.t, entries(e.t, e.journals), "the journal")
	require.Empty(e.t, entries(e.t, e.runDir), "the run's directory")
}

func TestAFailureAfterTheContainerTakesBackEverything(t *testing.T) {
	for _, tt := range []struct {
		name   string
		failAt string
	}{
		{"the token", "pveum acl modify / --tokens pco@pve!vm100 --roles PCO"},
		{"the start", "pct start 100"},
		{"the push", "pct push 100 "},
		{"the init", "pct exec 100 --keep-env 0 -- pco appliance init"},
		{"the protection", "pct set 100 --protection 1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.node.on(tt.failAt, func(context.Context, []string) (string, error) {
				return "", errors.New("it broke")
			})

			err := e.in.Install(t.Context(), e.options())

			require.ErrorContains(t, err, "it broke; what the run made is taken back")
			e.takenBack()
			require.Contains(t, e.node.volumes, "local:vztmpl/pco-appliance_1.2.3_amd64.tar.zst", "kept, as --keep-template says")
		})
	}
}

func TestATakenBackRunRemovesTheTemplateItDownloadedUnlessKept(t *testing.T) {
	e := newEnv(t)
	e.node.on("pct start 100", func(context.Context, []string) (string, error) { return "", errors.New("it broke") })
	o := e.options()
	o.KeepTemplate = false

	err := e.in.Install(t.Context(), o)

	require.Error(t, err)
	e.takenBack()
	require.Empty(t, e.node.volumes)
}

func TestADownloadThatFailsIsReadFromItsTask(t *testing.T) {
	e := newEnv(t)
	e.node.urls[ReleaseBase(testVersion)+"/pco-appliance_1.2.3_amd64.tar.zst"] = []byte("another file")

	err := e.in.Install(t.Context(), e.options())

	require.ErrorContains(t, err, `downloading pco-appliance_1.2.3_amd64.tar.zst: the task ended with "checksum mismatch`)
	e.nothingMade()
}

func TestTheVersionOfTheTemplateMustBeTheInstallers(t *testing.T) {
	e := newEnv(t)
	e.node.pcoVersion = "1.2.2"

	err := e.in.Install(t.Context(), e.options())

	require.ErrorContains(t, err, "lxc/100 holds pco 1.2.2 (none, unknown), not pco 1.2.3 as this installer: use the template of version 1.2.3")
	require.Equal(t, 0, e.node.count("pct push"), "nothing is pushed into it")
	e.takenBack()
}

func TestABootThatDidNotGoWellIsRefused(t *testing.T) {
	e := newEnv(t)
	e.node.on("pct start 100", func(_ context.Context, args []string) (string, error) {
		ct := e.node.cts[100]
		ct.running, ct.failed = true, []string{"pco.service", "systemd-networkd.service"}
		return "", nil
	})

	err := e.in.Install(t.Context(), e.options())

	require.ErrorContains(t, err, "lxc/100 booted with failed units: systemd-networkd.service")
	e.takenBack()
}

func TestASignalTakesTheRunBack(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			e := newEnv(t)
			// The signal comes while the installer waits for the container to boot.
			e.node.on("pct exec 100 --keep-env 0 -- timeout 90", func(ctx context.Context, _ []string) (string, error) {
				e.sigs.ch <- sig
				<-ctx.Done()
				return "", ctx.Err()
			})

			err := e.in.Install(t.Context(), e.options())

			require.ErrorContains(t, err, fmt.Sprintf("interrupted by %v during step start; what the run made is taken back", sig))
			e.takenBack()
		})
	}
}

// A run killed outright leaves its journal; --resume finishes it, with the
// token made anew, as no journal holds its secret, and the directory under
// /run made anew too.
func TestResumeFinishesARunKilledAfterTheToken(t *testing.T) {
	e := newEnv(t)
	e.node.killAt = "pveum acl modify / --tokens pco@pve!vm100"
	o := e.options()
	o.CloudflareToken = cfToken

	require.PanicsWithValue(t, errKilled, func() { _ = e.in.Install(t.Context(), o) })

	path := e.journal()
	require.Empty(t, entries(t, e.runDir), "no directory under /run yet")
	var j journal
	require.NoError(t, json.Unmarshal([]byte(mustRead(t, path)), &j))
	require.Equal(t, []string{"preflight", "template", "pool", "container", "access"}, j.Done)
	require.True(t, j.CloudflareToken)
	require.True(t, j.Manifest.CreatedToken)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// Without the Cloudflare token it had, the run does not go on.
	err = e.in.Install(t.Context(), Options{Resume: path, Yes: true})
	require.ErrorContains(t, err, "was given a Cloudflare token, which no journal holds: pass it again with --cf-token-file")

	err = e.in.Install(t.Context(), Options{Resume: path, Yes: true, CloudflareToken: cfToken})

	require.NoError(t, err, e.ask.text())
	require.Equal(t, 1, e.node.count("pct create"))
	require.Equal(t, 2, e.node.count("pveum user token add pco@pve vm100"))
	require.Equal(t, 1, e.node.count("pveum user token remove pco@pve vm100"))
	b := e.bootstrap(0)
	require.Equal(t, "install", b["mode"])
	require.Equal(t, pveSecret+"2", b["pveToken"].(map[string]any)["secret"], "the token made anew")
	require.Equal(t, cfToken, b["cloudflareToken"])
	require.Equal(t, "1", e.node.cts[100].cfg["protection"])
	require.Empty(t, entries(t, e.journals))
	require.Empty(t, entries(t, e.runDir))
}

// A run killed after init finished finds the install there: it is repaired,
// never taken back.
func TestResumeAfterInitRepairsTheInstall(t *testing.T) {
	e := newEnv(t)
	e.node.killAt = "pct exec 100 --keep-env 0 -- pco appliance init"
	require.PanicsWithValue(t, errKilled, func() { _ = e.in.Install(t.Context(), e.options()) })
	path := e.journal()

	require.NoError(t, e.in.Install(t.Context(), Options{Resume: path, Yes: true}), e.ask.text())

	require.Len(t, e.node.inits, 2)
	require.Equal(t, "install", e.bootstrap(0)["mode"])
	require.Equal(t, "repair", e.bootstrap(1)["mode"])
	require.Equal(t, pveSecret+"2", e.bootstrap(1)["pveToken"].(map[string]any)["secret"])
	require.Equal(t, "1", e.node.cts[100].cfg["protection"])
	require.Equal(t, []string{"vm100"}, e.node.tokenNames("pco@pve"))
	require.Empty(t, entries(t, e.journals))
}

// lockedCreate makes pct create of lxc/100 make the container and leave it
// locked, as one cut short does, with the run's description in its
// configuration.
func (e *testEnv) lockedCreate(then func(ctx context.Context)) {
	e.node.on("pct create 100 ", func(ctx context.Context, args []string) (string, error) {
		out, err := e.node.create(100, args)
		require.NoError(e.t, err)
		e.node.cts[100].cfg["lock"] = "create"
		then(ctx)
		return out, errors.New("interrupted")
	})
}

// A run killed inside pct create, after the configuration was written: the
// container carries the run's mark, and --resume, on another day, goes on
// with it once it is unlocked.
func TestResumeFinishesARunKilledInsidePctCreate(t *testing.T) {
	e := newEnv(t)
	e.lockedCreate(func(context.Context) {})
	e.node.killAt = "pct create 100"
	require.PanicsWithValue(t, errKilled, func() { _ = e.in.Install(t.Context(), e.options()) })
	path := e.journal()
	e.in.now = func() time.Time { return t0.Add(48 * time.Hour) }

	require.NoError(t, e.in.Install(t.Context(), Options{Resume: path, Yes: true}), e.ask.text())

	require.Equal(t, 1, e.node.count("pct create"))
	require.Less(t, indexOf(t, e.node.ran, "pct unlock 100"), indexOf(t, e.node.ran, "pct start 100"))
	require.Contains(t, e.ask.text(), "lxc/100 was left locked by a pct create that was cut short: unlocked")
	ct := e.node.cts[100]
	require.Empty(t, ct.cfg["lock"])
	require.Equal(t, "pco appliance vm100, installed 2026-10-01 by pco appliance install\n", ct.cfg["description"])
	require.Equal(t, "1", ct.cfg["protection"])
	require.Len(t, e.node.inits, 1)
	require.Empty(t, entries(t, e.journals))
}

// Ctrl-C reaches pct create too, which can leave the container locked: the
// run that takes itself back unlocks the container its mark proves its own.
func TestATakenBackRunUnlocksTheContainerItMade(t *testing.T) {
	e := newEnv(t)
	e.lockedCreate(func(ctx context.Context) {
		e.sigs.ch <- syscall.SIGINT
		<-ctx.Done()
	})

	err := e.in.Install(t.Context(), e.options())

	require.ErrorContains(t, err, "interrupted by interrupt during step container; what the run made is taken back")
	require.Less(t, indexOf(t, e.node.ran, "pct unlock 100"), indexOf(t, e.node.ran, "pct destroy 100 --purge 1"))
	e.takenBack()
}

// A run killed outright leaves its directory under /run, the bootstrap with
// the secrets in it maybe: the next run of any kind removes it, as the lock
// says no other run is live.
func TestAStaleBootstrapIsRemovedByTheNextRun(t *testing.T) {
	for name, run := range map[string]func(e *testEnv) error{
		"install":   func(e *testEnv) error { return e.in.Install(e.t.Context(), e.options()) },
		"repair":    func(e *testEnv) error { return e.in.Repair(e.t.Context(), 100, Options{Yes: true}) },
		"uninstall": func(e *testEnv) error { return e.in.Uninstall(e.t.Context(), 100, uninstallOptions()) },
		"grant-network": func(e *testEnv) error {
			return e.in.GrantNetworkCommand(e.t.Context(), NetworkOptions{VMID: 100, Bridge: "vmbr1", Yes: true})
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			if name != "install" {
				e = installed(t)
			}
			stale := filepath.Join(e.runDir, "pco-appliance-install-20260930T235959-beef")
			require.NoError(t, os.Mkdir(stale, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(stale, "bootstrap.json"), []byte(`{"pveToken":{"secret":"left"}}`), 0o600))
			other := filepath.Join(e.runDir, "pco")
			require.NoError(t, os.Mkdir(other, 0o755))

			require.NoError(t, run(e), e.ask.text())

			require.NoDirExists(t, stale)
			require.DirExists(t, other)
			require.Contains(t, e.ask.text(), "removed "+stale+", which a run that was cut short left")
		})
	}
}

// A run that could not take everything back keeps its journal; resuming it
// takes back the rest and never goes on with the install.
func TestResumeTakesBackWhatAFailedRunLeft(t *testing.T) {
	e := newEnv(t)
	e.node.on("pct start 100", func(context.Context, []string) (string, error) { return "", errors.New("it broke") })
	e.node.on("pveum user token remove pco@pve vm100", func(context.Context, []string) (string, error) {
		return "", errors.New("the cluster filesystem is busy")
	})

	err := e.in.Install(t.Context(), e.options())

	require.ErrorContains(t, err, "it broke; taking back what the run made failed")
	require.ErrorContains(t, err, "takes back the rest")
	require.Empty(t, e.node.cts)
	require.Equal(t, []string{"vm100"}, e.node.tokenNames("pco@pve"))
	path := e.journal()

	require.NoError(t, e.in.Install(t.Context(), Options{Resume: path, Yes: true}), e.ask.text())

	e.takenBack()
	require.Equal(t, 1, e.node.count("pct create"), "the install does not go on")
}

func TestResumeTakesBackACutShortGrant(t *testing.T) {
	e := installed(t)
	e.node.killAt = "pveum acl modify /sdn/zones/localnetwork/vmbr1"
	require.PanicsWithValue(t, errKilled, func() {
		_ = e.in.GrantNetworkCommand(t.Context(), NetworkOptions{VMID: 100, Bridge: "vmbr1", Yes: true})
	})
	require.NotEmpty(t, e.node.pcoLines())

	require.NoError(t, e.in.Install(t.Context(), Options{Resume: e.journal(), Yes: true}), e.ask.text())

	require.Empty(t, e.node.pcoLines())
	require.Nil(t, e.node.roles["PCOSDN"])
	require.Empty(t, entries(t, e.journals))
}

func TestResumeTakesBackARunItCannotFinish(t *testing.T) {
	e := newEnv(t)
	e.node.killAt = "pct start 100"
	require.PanicsWithValue(t, errKilled, func() { _ = e.in.Install(t.Context(), e.options()) })
	path := e.journal()
	e.node.pcoVersion = "0.9.0"
	e.node.cts[100].version = "0.9.0"

	err := e.in.Install(t.Context(), Options{Resume: path, Yes: true})

	require.ErrorContains(t, err, "not pco 1.2.3 as this installer")
	e.takenBack()
}

// killedAtTheDownload leaves the journal of a run killed while the template
// was being fetched, before the step was done.
func (e *testEnv) killedAtTheDownload(o Options) string {
	e.t.Helper()
	e.node.on("pvesh create /nodes/pve1/storage/local/download-url", func(context.Context, []string) (string, error) {
		panic(errKilled)
	})
	require.PanicsWithValue(e.t, errKilled, func() { _ = e.in.Install(e.t.Context(), o) })
	return e.journal()
}

// move puts a file where the old path no longer leads to it, as the removal of
// install.sh's temporary directory does.
func move(t *testing.T, from string) string {
	t.Helper()
	to := filepath.Join(t.TempDir(), filepath.Base(from))
	require.NoError(t, os.WriteFile(to, []byte(mustRead(t, from)), 0o644))
	require.NoError(t, os.Remove(from))
	return to
}

// The journal names the checksums.txt of the run that was killed, which the
// script that started it removed with its temporary directory: the resume is
// given the one it has now, and finishes the run instead of taking it back.
func TestResumeTakesTheChecksumsItIsGiven(t *testing.T) {
	e := newEnv(t)
	path := e.killedAtTheDownload(e.options())
	moved := move(t, e.checksums)

	require.NoError(t, e.in.Install(t.Context(), Options{Resume: path, Yes: true, ChecksumsFile: moved}), e.ask.text())

	require.Equal(t, 1, e.node.count("pct create"))
	require.Equal(t, 2, e.node.count("pvesh create"), "the download that was cut short, and the one of the resume")
	require.Equal(t, "1", e.node.cts[100].cfg["protection"])
	require.Empty(t, entries(t, e.journals))
}

// --checksums as typed, checksums.txt, is a path of the directory the install
// ran in: the journal keeps it by its absolute path, as it does --api-ca, so
// that a resume from another directory finds the file.
func TestTheJournalKeepsTheChecksumsByTheirAbsolutePath(t *testing.T) {
	e := newEnv(t)
	t.Chdir(filepath.Dir(e.checksums))
	o := e.options()
	o.ChecksumsFile = "checksums.txt"
	path := e.killedAtTheDownload(o)

	j, err := readJournal(path)
	require.NoError(t, err)
	require.Equal(t, e.checksums, j.Options.ChecksumsFile)
	t.Chdir(t.TempDir())

	require.NoError(t, e.in.Install(t.Context(), Options{Resume: path, Yes: true}), e.ask.text())
	require.Equal(t, 1, e.node.count("pct create"))
	require.Empty(t, entries(t, e.journals))
}

func TestResumeChecksTheTemplateAgainstTheChecksumsItIsGiven(t *testing.T) {
	e := newEnv(t)
	path := e.killedAtTheDownload(e.options())
	other := filepath.Join(t.TempDir(), "checksums.txt")
	require.NoError(t, os.WriteFile(other, []byte(strings.Repeat("1", 64)+"  pco-appliance_1.2.3_amd64.tar.zst\n"), 0o644))

	err := e.in.Install(t.Context(), Options{Resume: path, Yes: true, ChecksumsFile: other})

	require.ErrorContains(t, err, "expected '"+strings.Repeat("1", 64)+"'")
}

func TestResumeTakesTheReleaseBaseItIsGiven(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.ReleaseBase = "https://mirror.example.test/gone"
	path := e.killedAtTheDownload(o)

	require.NoError(t, e.in.Install(t.Context(), Options{Resume: path, Yes: true, ReleaseBase: ReleaseBase(testVersion)}), e.ask.text())

	require.Contains(t, strings.Join(e.node.ran, "\n"), "--url "+ReleaseBase(testVersion)+"/pco-appliance_1.2.3_amd64.tar.zst")
	require.Equal(t, 1, e.node.count("pct create"))
	require.Empty(t, entries(t, e.journals))
}

func TestResumeTakesTheTemplateItIsGiven(t *testing.T) {
	e := newEnv(t)
	path := e.killedAtTheDownload(e.options())
	file := filepath.Join(t.TempDir(), "pco-appliance_1.2.3_amd64.tar.zst")
	require.NoError(t, os.WriteFile(file, e.node.urls[ReleaseBase(testVersion)+"/pco-appliance_1.2.3_amd64.tar.zst"], 0o644))

	require.NoError(t, e.in.Install(t.Context(), Options{Resume: path, Yes: true, Template: file}), e.ask.text())

	require.Equal(t, 1, e.node.count("pvesh create"), "only the download that was cut short")
	require.Contains(t, e.node.ran[slicesIndexPrefix(e.node.ran, "pct create")], "pct create 100 "+file+" ")
	require.Empty(t, entries(t, e.journals))
}

// Without a template step done, a run needs the checksums.txt that names the
// template. When the file the journal names is gone and none is given, the
// resume says what to pass and leaves the run as it is, to be resumed again.
func TestResumeRefusesWhenTheChecksumsAreGoneAndNoneIsGiven(t *testing.T) {
	e := newEnv(t)
	path := e.killedAtTheDownload(e.options())
	moved := move(t, e.checksums)
	ran := len(e.node.ran)

	err := e.in.Install(t.Context(), Options{Resume: path, Yes: true})

	require.ErrorContains(t, err, "pass the checksums.txt of the release with --checksums")
	require.ErrorContains(t, err, e.checksums)
	require.NotContains(t, err.Error(), "taken back")
	require.FileExists(t, path)
	require.Empty(t, e.node.ran[ran:], "nothing was run")

	require.NoError(t, e.in.Install(t.Context(), Options{Resume: path, Yes: true, ChecksumsFile: moved}), e.ask.text())
	require.Empty(t, entries(t, e.journals))
}

func TestResumeRefusesAChecksumsFileItCannotRead(t *testing.T) {
	e := newEnv(t)
	path := e.killedAtTheDownload(e.options())
	missing := filepath.Join(t.TempDir(), "checksums.txt")

	err := e.in.Install(t.Context(), Options{Resume: path, Yes: true, ChecksumsFile: missing})

	require.ErrorContains(t, err, "--checksums")
	require.ErrorContains(t, err, missing)
	require.FileExists(t, path)
}

func TestResumeRefusesATemplateThatIsNotAbsolute(t *testing.T) {
	e := newEnv(t)
	path := e.killedAtTheDownload(e.options())

	err := e.in.Install(t.Context(), Options{Resume: path, Yes: true, Template: "pco-appliance_1.2.3_amd64.tar.zst"})

	require.ErrorContains(t, err, "--template pco-appliance_1.2.3_amd64.tar.zst: give the file by its absolute path")
	require.FileExists(t, path)
}

// Once the template step is done the checksums.txt is not read again, so its
// being gone is no reason to refuse.
func TestResumeAfterTheTemplateNeedsNoChecksums(t *testing.T) {
	e := newEnv(t)
	e.node.killAt = "pct start 100"
	require.PanicsWithValue(t, errKilled, func() { _ = e.in.Install(t.Context(), e.options()) })
	path := e.journal()
	move(t, e.checksums)

	require.NoError(t, e.in.Install(t.Context(), Options{Resume: path, Yes: true}), e.ask.text())

	require.Equal(t, 1, e.node.count("pct create"))
	require.Empty(t, entries(t, e.journals))
}

// The journal is written before every create, and never holds a secret.
func TestTheJournalHoldsNoSecret(t *testing.T) {
	e := newEnv(t)
	checked := 0
	e.node.before = func(string) {
		for _, n := range entries(t, e.journals) {
			if strings.HasSuffix(n, ".json") {
				text := mustRead(t, filepath.Join(e.journals, n))
				require.NotContains(t, text, pveSecret)
				require.NotContains(t, text, cfToken)
				checked++
			}
		}
	}
	o := e.options()
	o.CloudflareToken = cfToken

	e.install(o)

	require.Greater(t, checked, 20, "the journal was there from the first create on")
	require.Empty(t, entries(t, e.journals))
}

func TestASecondInstallerIsRefused(t *testing.T) {
	e := newEnv(t)
	other := New(e.node, e.ask, e.in.now, e.in.rand, e.journals)
	other.h = e.in.h
	e.node.on("pveversion", func(context.Context, []string) (string, error) {
		err := other.Install(t.Context(), e.options())
		require.ErrorContains(t, err, "another pco appliance installer runs on this node (it holds "+filepath.Join(e.journals, "lock")+"); wait for it to finish")
		return "pve-manager/9.2.21/0123456789abcdef (running kernel: 7.0.14-20-pve)\n", nil
	})

	e.install(e.options())

	require.Empty(t, entries(t, e.journals), "the lock goes with the run")
}

// The end-to-end suite points the container at its relay: the variable is
// forwarded to init, and only to the commands of pco that reach Cloudflare.
func TestTheCloudflareAPIOfTestsIsForwardedToInit(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.CloudflareAPI = "http://127.0.0.1:8787/client/v4"

	e.install(o)

	require.Contains(t, e.node.ran, "pct exec 100 --keep-env 0 -- env PCO_CLOUDFLARE_API_URL=http://127.0.0.1:8787/client/v4 pco appliance init --bootstrap /var/lib/pco/bootstrap.json")
	require.Contains(t, e.node.ran, "pct exec 100 --keep-env 0 -- pco version")
	require.Equal(t, 1, e.node.count("pct exec 100 --keep-env 0 -- env"))
	require.Equal(t, "PCO_CLOUDFLARE_API_URL=http://127.0.0.1:8787/client/v4", e.node.inits[0].Env)

	e = newEnv(t)
	e.install(e.options())
	require.Equal(t, 0, e.node.count("pct exec 100 --keep-env 0 -- env"))
	require.Empty(t, e.node.inits[0].Env)
}

func TestATemplateOnTheStorageIsTakenWhenItsChecksumIsTheReleases(t *testing.T) {
	e := newEnv(t)
	file := filepath.Join(t.TempDir(), "pco-appliance_1.2.3_amd64.tar.zst")
	require.NoError(t, os.WriteFile(file, e.node.urls[ReleaseBase(testVersion)+"/pco-appliance_1.2.3_amd64.tar.zst"], 0o644))
	e.node.volumes["local:vztmpl/pco-appliance_1.2.3_amd64.tar.zst"] = file

	e.install(e.options())

	require.Equal(t, 0, e.node.count("pvesh create"))
	require.Contains(t, e.node.ran, "pvesm path local:vztmpl/pco-appliance_1.2.3_amd64.tar.zst")
	require.Empty(t, e.bootstrapAppliance()["template"], "not the installer's to remove")

	e = newEnv(t)
	require.NoError(t, os.WriteFile(file, []byte("something else"), 0o644))
	e.node.volumes["local:vztmpl/pco-appliance_1.2.3_amd64.tar.zst"] = file
	err := e.in.Install(t.Context(), e.options())
	require.ErrorContains(t, err, "remove it with pvesm free local:vztmpl/pco-appliance_1.2.3_amd64.tar.zst")
}

func TestATemplateGivenAsAFile(t *testing.T) {
	e := newEnv(t)
	file := filepath.Join(t.TempDir(), "pco-appliance_1.2.3_amd64.tar.zst")
	require.NoError(t, os.WriteFile(file, e.node.urls[ReleaseBase(testVersion)+"/pco-appliance_1.2.3_amd64.tar.zst"], 0o644))
	o := e.options()
	o.Template = file

	e.install(o)

	require.Equal(t, 0, e.node.count("pvesh create"))
	require.Equal(t, 0, e.node.count("pvesh get /nodes/pve1/storage/local/content"))
	require.Contains(t, e.node.ran[slicesIndexPrefix(e.node.ran, "pct create")], "pct create 100 "+file+" --unprivileged 1")

	require.NoError(t, os.WriteFile(file, []byte("tampered"), 0o644))
	e = newEnv(t)
	o.ChecksumsFile = e.checksums
	err := e.in.Install(t.Context(), o)
	require.ErrorContains(t, err, "as --checksums says")
	e.nothingMade()
}

// bootstrapAppliance is the appliance's part of the manifest the first init
// was given.
func (e *testEnv) bootstrapAppliance() map[string]any {
	e.t.Helper()
	return e.bootstrap(0)["manifest"].(map[string]any)["appliance"].(map[string]any)
}

func slicesIndexPrefix(lines []string, prefix string) int {
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}
