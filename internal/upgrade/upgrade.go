// Package upgrade replaces pco and cloudflared in place, inside the appliance:
// from releases whose checksums.txt is signed by a key of the keyring the
// package ships, with the package of the version before kept for a rollback,
// and the two packages held, so that nothing else upgrades them.
package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	// Keyring is the keyring of the release key the package ships.
	Keyring = "/usr/share/pco/release-key.gpg"
	// WorkDir is where pco upgrade works, on the state volume of the
	// appliance; the package of the version before an upgrade is kept in
	// its directory previous.
	WorkDir = "/var/lib/pco/upgrades"

	// SnapshotPrefix starts the name of the snapshot an admin takes of the
	// appliance before an upgrade; the date of the day follows, as YYYYMMDD.
	SnapshotPrefix = "pco-pre-upgrade-"

	previousDir = "previous"
	sumExt      = ".sha256"
	lockName    = ".lock"
	extractDir  = ".extract-"
)

var (
	// ErrNotConfirmed is the error of a change that was not confirmed.
	ErrNotConfirmed = errors.New("the change was not confirmed")
	// ErrNothingKept is the error of a rollback without a kept package.
	ErrNothingKept = errors.New("no package is kept to roll back to")
	// ErrNotNewer is the error of a --version that is not newer than the
	// installed one.
	ErrNotNewer = errors.New("not newer than the installed version")
)

// Options are the choices of the admin.
type Options struct {
	Version  string // empty: latest
	Check    bool   // only say what would change
	Rollback bool   // install the kept package
	Yes      bool   // the change is confirmed; without it nothing is changed
	// Keep keeps the package of the installed version already with Check,
	// so that the question can say whether a rollback will be there.
	Keep bool
}

// Result says what changed, or with Check what would change.
type Result struct {
	Package     string
	Installed   string // before, as the release names it
	Target      string // what is or would be installed; empty when nothing changes
	Release     string // the release that vouches for Target; empty for a rollback
	Fingerprint string // the key that signed that release
	Kept        string // the package kept for a rollback
	Notes       []string
}

// Upgrader replaces the packages.
type Upgrader struct {
	run     setup.Runner
	fetch   Fetcher
	verify  Verifier
	keyring string
	workDir string
	arch    string
	now     func() time.Time

	store    *store.Paths
	say      func(format string, args ...any)
	free     func(dir string) (uint64, error)
	releases map[string]release
}

// New returns an Upgrader that works in workDir on a machine of arch; f
// downloads into workDir as well.
func New(run setup.Runner, f Fetcher, v Verifier, keyring, workDir, arch string, now func() time.Time) *Upgrader {
	return &Upgrader{
		run: run, fetch: f, verify: v, keyring: keyring, workDir: workDir, arch: arch, now: now,
		say:      func(string, ...any) {},
		free:     freeSpace,
		releases: make(map[string]release),
	}
}

// SnapshotName is the name of the snapshot to take before an upgrade today.
// pco cannot take it: its token has no right to snapshot its own container.
func (u *Upgrader) SnapshotName() string { return SnapshotPrefix + u.now().Format("20060102") }

// WithStore gives the store a rollback of pco checks: the kept pco must read
// every object in it.
func (u *Upgrader) WithStore(p store.Paths) *Upgrader {
	u.store = &p
	return u
}

// WithProgress prints a line for each step.
func (u *Upgrader) WithProgress(say func(format string, args ...any)) *Upgrader {
	u.say = say
	return u
}

// Lock takes the work directory for this run and removes what an earlier run
// left in it; another pco upgrade that runs is an error. The returned func
// lets it go.
func (u *Upgrader) Lock() (func(), error) {
	if err := os.MkdirAll(filepath.Join(u.workDir, previousDir), 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", u.workDir, err)
	}
	path := filepath.Join(u.workDir, lockName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another pco upgrade runs (it holds %s)", path)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	entries, err := os.ReadDir(u.workDir)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, fetchPrefix) || strings.HasPrefix(name, extractDir) || strings.HasSuffix(name, ".deb") {
			_ = os.RemoveAll(filepath.Join(u.workDir, name))
		}
	}
	return func() { _ = f.Close() }, nil
}

// installed returns the version of an installed package as dpkg names it. A
// package the appliance keeps is held, which dpkg says as its want; its
// status must be installed, with no error flag.
func (u *Upgrader) installed(ctx context.Context, pkg string) (string, error) {
	out, err := u.run.Run(ctx, "dpkg-query", "--show",
		"--showformat=${db:Status-Want} ${db:Status-Status} ${db:Status-Eflag} ${Version}", pkg)
	if err != nil {
		return "", fmt.Errorf("asking dpkg for the version of %s: %w", pkg, err)
	}
	fields := strings.Fields(out)
	if len(fields) != 4 {
		return "", fmt.Errorf("dpkg says %q of %s, not its state and version", shorten(strings.TrimSpace(out)), pkg)
	}
	want, status, eflag, version := fields[0], fields[1], fields[2], fields[3]
	switch {
	case eflag != "ok":
		return "", fmt.Errorf("%s needs to be reinstalled, dpkg says: apt-get install --reinstall %s", pkg, pkg)
	case status == "config-files":
		return "", fmt.Errorf("%s is removed; only its configuration files are left", pkg)
	case status != "installed":
		return "", fmt.Errorf("%s is %s: a dpkg run did not finish, and dpkg --configure -a finishes it", pkg, unfinished(status))
	case want != "install" && want != "hold":
		return "", fmt.Errorf("%s is installed but selected for %s: apt-mark hold %s selects it again", pkg, shorten(want), pkg)
	}
	if err := checkPackageVersion(version); err != nil {
		return "", fmt.Errorf("dpkg names the version of %s: %w", pkg, err)
	}
	return version, nil
}

// unfinished says what a status of dpkg other than installed means.
func unfinished(status string) string {
	switch status {
	case "unpacked":
		return "unpacked but not configured"
	case "triggers-awaited", "triggers-pending":
		return "waiting for triggers"
	}
	return shorten(status)
}

// install installs a .deb over the held package, allowing a lower version
// only for a rollback, and holds pco and cloudflared again afterwards,
// whether the install worked or not.
func (u *Upgrader) install(ctx context.Context, deb string, downgrade bool) error {
	args := []string{"install", "-y", "--allow-change-held-packages"}
	if downgrade {
		args = append(args, "--allow-downgrades")
	}
	// dpkg would ask about a changed configuration file on a stdin that is
	// /dev/null, and fail; the admin's version stays, a new default is taken.
	args = append(args, "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", deb)
	u.say("installing %s", filepath.Base(deb))
	_, err := u.run.Run(ctx, "apt-get", args...)
	_, holdErr := u.run.Run(ctx, "apt-mark", "hold", "pco", "cloudflared")
	switch {
	case err != nil && holdErr != nil:
		return fmt.Errorf("installing %s: %w; holding pco and cloudflared again failed too: %w", filepath.Base(deb), err, holdErr)
	case err != nil:
		return fmt.Errorf("installing %s: %w (pco and cloudflared are held again)", filepath.Base(deb), err)
	case holdErr != nil:
		return fmt.Errorf("%s is installed, but holding pco and cloudflared again failed: %w; run apt-mark hold pco cloudflared",
			filepath.Base(deb), holdErr)
	}
	return nil
}

// keep moves a package of a verified version into the directory of the kept
// packages, under name and with its sha256 beside it, and removes the other
// kept packages of pkg: one is kept per package.
func (u *Upgrader) keep(pkg, path, name, sum string) (string, error) {
	dir := filepath.Join(u.workDir, previousDir)
	kept := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := atomicfile.Write(kept+sumExt, []byte(sum+"  "+name+"\n"), atomicfile.Options{Mode: 0o600}); err != nil {
		return "", err
	}
	if err := os.Rename(path, kept); err != nil {
		return "", err
	}
	u.dropOthers(pkg, name)
	return kept, nil
}

// dropOthers removes the kept packages of pkg but the one of that name.
func (u *Upgrader) dropOthers(pkg, name string) {
	dir := filepath.Join(u.workDir, previousDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if other := e.Name(); other != name && other != name+sumExt && strings.HasPrefix(other, pkg+"_") {
			_ = os.Remove(filepath.Join(dir, other))
		}
	}
}

// alreadyKept returns the kept package of that name when it is there and
// matches the sha256 beside it, and drops the other kept packages of pkg.
func (u *Upgrader) alreadyKept(pkg, name string) (string, bool) {
	path := filepath.Join(u.workDir, previousDir, name)
	if checkKept(path) != nil {
		return "", false
	}
	u.dropOthers(pkg, name)
	return path, true
}

// checkKept compares a kept package with the sha256 kept beside it, so that
// a rollback installs what an upgrade checked, whatever happened to the
// directory since.
func checkKept(path string) error {
	b, err := os.ReadFile(path + sumExt)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the kept package %s has no checksum beside it; it is not installed", path)
	}
	if err != nil {
		return err
	}
	m := checksumLineRe.FindStringSubmatch(strings.TrimSpace(string(b)))
	if m == nil || m[2] != filepath.Base(path) || checkSum(path, m[2], m[1], "") != nil {
		return fmt.Errorf("the kept package %s does not match the checksum kept beside it; it is not installed", path)
	}
	return nil
}

// keptPackage returns the kept package of pkg for this architecture and its
// version.
func (u *Upgrader) keptPackage(pkg string) (path, version string, err error) {
	dir := filepath.Join(u.workDir, previousDir)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	suffix := "_" + u.arch + ".deb"
	for _, e := range entries {
		name := e.Name()
		middle, ok := strings.CutSuffix(strings.TrimPrefix(name, pkg+"_"), suffix)
		if !ok || !strings.HasPrefix(name, pkg+"_") || !e.Type().IsRegular() {
			continue
		}
		if path != "" {
			return "", "", fmt.Errorf("%s holds more than one package of %s", dir, pkg)
		}
		path, version = filepath.Join(dir, name), middle
	}
	if path == "" {
		return "", "", fmt.Errorf("%w: %s holds no package of %s", ErrNothingKept, dir, pkg)
	}
	if err := checkKept(path); err != nil {
		return "", "", err
	}
	return path, version, nil
}
