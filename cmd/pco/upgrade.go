package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/upgrade"
)

// tunnelsDir is where the daemon keeps the files of the connectors, below
// the local root of the store.
const tunnelsDir = "tunnels"

// upgradeEnv is what pco upgrade works with on the machine; tests replace it.
type upgradeEnv struct {
	euid     func() int
	run      setup.Runner
	fetcher  func(o upgrade.Overrides, dir string) upgrade.Fetcher
	verifier upgrade.Verifier
	systemd  connector.Systemd
	sleep    func(ctx context.Context, d time.Duration) error
	// volume returns the roots of the store on the mounted state volume;
	// nil is applianceVolume.
	volume   func() (store.Paths, error)
	keyring  string // the release key the package ships
	manifest string // the manifest the package ships
	workDir  string
	arch     string
}

func defaultUpgradeEnv() upgradeEnv {
	run := setup.NewHostRunner()
	return upgradeEnv{
		euid: os.Geteuid,
		run:  run,
		fetcher: func(o upgrade.Overrides, dir string) upgrade.Fetcher {
			return upgrade.NewFetcher(upgrade.FetchConfig{Dir: dir, Base: o.Base})
		},
		verifier: upgrade.NewVerifier(run),
		systemd:  connector.NewSystemctl(),
		sleep:    sleepContext,
		keyring:  upgrade.Keyring,
		manifest: upgrade.ShippedManifest,
		workDir:  upgrade.WorkDir,
		arch:     runtime.GOARCH,
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

const upgradeLong = `Upgrade pco, cloudflared or both (all, the default) in place, inside the appliance.

pco comes from its latest release, or the one --version names. The signature of the
release's checksums.txt is checked with the release key the package ships, and the
package with its line there. cloudflared comes from the manifest of vetted versions the
release lists in that checksums.txt: the newest version it allows, or the one --version
names, never one it denies, checked against the sha256 of the manifest. The connectors
restart on the new cloudflared one after the other, and each has 60 s to be ready again.
With all, pco is upgraded first and cloudflared follows the manifest of the release pco
came from.

The package of the version that is replaced is kept in /var/lib/pco/upgrades/previous,
one per package, and --rollback installs it again. A rollback of pco is refused while the
store holds an object the kept pco cannot read, and one of cloudflared to a version the
manifest denies. pco and cloudflared stay held, so that apt and unattended-upgrades leave
them alone.

pco cannot take a snapshot of its own container. Before an upgrade, take one on the node,
named pco-pre-upgrade-<YYYYMMDD>; a rollback to it is followed by pco appliance recover.

--check says what is installed and what is available, and which versions of cloudflared
are denied; its exit status is 1 when an upgrade is available. pco upgrade runs as root
inside the appliance; a host upgrades pco and cloudflared with apt.`

const upgradeExample = `  # Say what is installed and what the latest release offers; exit status 1 when there is an upgrade
  pco upgrade --check
  # Upgrade pco and then cloudflared without the question, as a script does
  pco upgrade --yes
  # Install the release 1.4.0 of pco, and leave cloudflared as it is
  pco upgrade pco --version 1.4.0
  # Install cloudflared 2026.9.3, if the manifest allows it
  pco upgrade cloudflared --version 2026.9.3
  # Go back to the cloudflared the last upgrade replaced
  pco upgrade cloudflared --rollback`

func (a *app) upgradeCmd() *cobra.Command {
	var o upgrade.Options
	cmd := &cobra.Command{
		Use:       "upgrade [pco|cloudflared|all]",
		Short:     "Upgrade pco and cloudflared in the appliance from signed releases",
		Long:      upgradeLong,
		Example:   upgradeExample,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"pco", "cloudflared", "all"},
		RunE: func(cmd *cobra.Command, args []string) error {
			which := "all"
			if len(args) == 1 {
				which = args[0]
			}
			return a.upgradePackages(cmd, which, o)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&o.Version, "version", "", "the version of pco or of cloudflared to install, instead of the newest")
	flags.BoolVar(&o.Check, "check", false, "only say what is installed and what is available; exit status 1 when an upgrade is")
	flags.BoolVar(&o.Rollback, "rollback", false, "install the package the last upgrade replaced")
	addYesFlag(cmd, &o.Yes)
	return cmd
}

// upgradePlan is what an upgrade of one package is to do.
type upgradePlan struct {
	res      upgrade.Result
	manifest upgrade.Manifest
	release  string // whose manifest cloudflared follows
}

func (a *app) upgradePackages(cmd *cobra.Command, which string, o upgrade.Options) error {
	errOut := &screen{w: cmd.ErrOrStderr()}
	if upgrade.Overridden(a.getenv) {
		errOut.printf("warning: %s\n", upgrade.OverrideLine)
	}
	if err := a.noJSON(cmd); err != nil {
		return err
	}
	overrides, err := upgrade.OverridesFrom(a.getenv)
	if err != nil {
		return err
	}
	if err := checkUpgradeArgs(which, o); err != nil {
		return err
	}
	if err := a.upgradeAllowed(); err != nil {
		return err
	}
	e := a.upgrade
	paths, err := a.upgradeVolume()
	if err != nil {
		return err
	}
	keyring := e.keyring
	if overrides.Keyring != "" {
		keyring = overrides.Keyring
	}
	out := &screen{w: cmd.OutOrStdout()}
	u := upgrade.New(e.run, e.fetcher(overrides, e.workDir), e.verifier, keyring, e.workDir, e.arch, a.now).
		WithStore(paths).
		WithProgress(func(format string, args ...any) { out.printf(format+"\n", args...) })
	unlock, err := u.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	ctx := cmd.Context()
	plans, skipped, err := a.planUpgrade(ctx, u, which, o)
	if err != nil {
		return err
	}
	for _, err := range skipped {
		out.printf("%s\n", err)
	}
	if o.Check {
		return a.printCheck(out, plans)
	}
	return a.applyUpgrade(cmd, u, paths, plans, o)
}

func checkUpgradeArgs(which string, o upgrade.Options) error {
	switch {
	case which != "pco" && which != "cloudflared" && which != "all":
		return fmt.Errorf("unknown package %q: want pco, cloudflared or all", which)
	case o.Version != "" && which == "all":
		return errors.New("--version names a version of pco or of cloudflared: name the package, as in pco upgrade pco --version 1.4.0")
	case o.Version != "" && o.Rollback:
		return errors.New("--rollback installs the package the last upgrade replaced; it takes no --version")
	case o.Check && (o.Rollback || o.Version != ""):
		return errors.New("--check says what the latest release offers; it takes neither --version nor --rollback")
	}
	return nil
}

// upgradeAllowed refuses pco upgrade on a host and for a user other than
// root.
func (a *app) upgradeAllowed() error {
	profile, err := store.DetectProfile(a.profileFile)
	if err != nil {
		return err
	}
	if profile != store.ProfileAppliance {
		return errors.New("pco upgrade runs inside the appliance; a host upgrades with apt: " +
			"the installer installs a newer pco package, and apt-get install --only-upgrade cloudflared a newer cloudflared")
	}
	if a.upgrade.euid() != 0 {
		return errors.New("pco upgrade installs packages: run it as root")
	}
	return nil
}

func (a *app) upgradeVolume() (store.Paths, error) {
	if a.upgrade.volume != nil {
		return a.upgrade.volume()
	}
	return a.applianceVolume()
}

// planUpgrade asks what each package is to do, pco first. A rollback of all
// leaves out a package with nothing kept, and says why in skipped.
func (a *app) planUpgrade(ctx context.Context, u *upgrade.Upgrader, which string, o upgrade.Options) (plans []upgradePlan, skipped []error, err error) {
	ask := upgrade.Options{Version: o.Version, Rollback: o.Rollback, Check: true}
	skip := func(err error) bool {
		if which == "all" && o.Rollback && errors.Is(err, upgrade.ErrNothingKept) {
			skipped = append(skipped, err)
			return true
		}
		return false
	}
	release := ""
	if which != "cloudflared" {
		res, err := u.Pco(ctx, ask)
		if err != nil && !skip(err) {
			return nil, nil, err
		}
		if err == nil {
			plans = append(plans, upgradePlan{res: res})
			release = res.Release
		}
	}
	if which != "pco" {
		var m upgrade.Manifest
		if o.Rollback {
			m, err = upgrade.LoadManifest(a.upgrade.manifest)
		} else {
			m, release, err = u.ReleaseManifest(ctx, release)
		}
		if err != nil {
			return nil, nil, err
		}
		res, err := u.Cloudflared(ctx, ask, m, nil)
		if err != nil && !skip(err) {
			return nil, nil, err
		}
		if err == nil {
			plans = append(plans, upgradePlan{res: res, manifest: m, release: release})
		}
	}
	if len(plans) == 0 && len(skipped) > 0 {
		return nil, nil, errors.Join(skipped...)
	}
	return plans, skipped, nil
}

// printCheck prints what is installed and available; an upgrade that is
// available is exit status 1.
func (a *app) printCheck(out *screen, plans []upgradePlan) error {
	available := false
	for _, p := range plans {
		r := p.res
		available = available || r.Target != ""
		switch {
		case r.Package == "pco" && r.Target != "":
			out.printf("pco: installed %s, available %s\n", r.Installed, r.Target)
		case r.Package == "pco":
			out.printf("pco: installed %s, the newest release\n", r.Installed)
		case r.Target != "":
			out.printf("cloudflared: installed %s, available %s, from the manifest of release v%s of %s\n",
				r.Installed, r.Target, p.release, p.manifest.Updated.Format(time.DateOnly))
		default:
			out.printf("cloudflared: installed %s, the newest the manifest of release v%s of %s allows\n",
				r.Installed, p.release, p.manifest.Updated.Format(time.DateOnly))
		}
		if r.Package != "cloudflared" {
			continue
		}
		if reason, denied := p.manifest.Denied(r.Installed); denied {
			out.printf("cloudflared: the installed %s is denied: %s\n", r.Installed, reason)
		}
		for _, d := range p.manifest.Deny {
			out.printf("cloudflared denied: %s (%s)\n", d.Version, d.Reason)
		}
	}
	if err := out.done(); err != nil {
		return err
	}
	if available {
		return errReported
	}
	return nil
}

func (a *app) applyUpgrade(cmd *cobra.Command, u *upgrade.Upgrader, paths store.Paths, plans []upgradePlan, o upgrade.Options) error {
	ctx := cmd.Context()
	out, errOut := &screen{w: cmd.OutOrStdout()}, &screen{w: cmd.ErrOrStderr()}
	var changes []upgradePlan
	for _, p := range plans {
		if p.res.Target != "" {
			changes = append(changes, p)
		}
	}
	if len(changes) == 0 {
		for _, p := range plans {
			out.printf("%s %s is installed, the newest there is\n", p.res.Package, p.res.Installed)
		}
		return out.done()
	}
	if !o.Rollback {
		remindOfSnapshot(errOut, paths, u.SnapshotName())
	}
	var what []string
	verb := "Upgrade"
	if o.Rollback {
		verb = "Roll back"
	}
	for _, p := range changes {
		what = append(what, fmt.Sprintf("%s from %s to %s", p.res.Package, p.res.Installed, p.res.Target))
		if p.res.Package == "cloudflared" {
			errOut.printf("The connectors restart on the new cloudflared: a tunnel has no connection from this appliance\n"+
				"until its connector is ready again, at most %s.\n", upgrade.ReadyWait)
		}
	}
	if err := errOut.done(); err != nil {
		return err
	}
	ok, err := a.confirm(cmd, o.Yes, verb+" "+strings.Join(what, " and ")+"? [y/N]")
	if err != nil {
		return err
	}
	if !ok {
		return errAborted
	}
	restart := a.connectorRestart(paths, out)
	daemonRan := a.daemonAnswers(ctx)
	for _, p := range changes {
		do := upgrade.Options{Rollback: o.Rollback, Yes: true}
		if !o.Rollback {
			do.Version = p.res.Target
		}
		var res upgrade.Result
		if p.res.Package == "pco" {
			res, err = u.Pco(ctx, do)
		} else {
			res, err = u.Cloudflared(ctx, do, p.manifest, restart)
		}
		for _, note := range res.Notes {
			errOut.printf("warning: %s\n", note)
		}
		if err != nil {
			return err
		}
		printUpgraded(out, res, o.Rollback)
		if p.res.Package != "pco" {
			continue
		}
		if !daemonRan {
			out.println("the daemon did not answer before the upgrade, so it is not waited for")
			continue
		}
		if err := a.waitForDaemon(ctx, res, o.Rollback, a.upgrade.sleep, out); err != nil {
			return err
		}
	}
	out.println("pco and cloudflared are held")
	return out.done()
}

func printUpgraded(out *screen, r upgrade.Result, rollback bool) {
	switch {
	case rollback:
		out.printf("%s %s is installed again from the kept package\n", r.Package, r.Target)
	case r.Fingerprint != "":
		out.printf("%s %s is installed, from release v%s signed by key %s\n", r.Package, r.Target, r.Release, r.Fingerprint)
	default:
		out.printf("%s %s is installed\n", r.Package, r.Target)
	}
	if r.Kept != "" && !rollback {
		out.printf("%s %s is kept as %s for --rollback\n", r.Package, r.Installed, r.Kept)
	}
}

// remindOfSnapshot says what pco cannot do itself before an upgrade.
func remindOfSnapshot(errOut *screen, paths store.Paths, name string) {
	vmid := "<vmid>"
	if st, err := store.OpenExisting(paths); err == nil {
		if inst, found, err := st.Install(); err == nil && found && inst.Appliance != nil {
			vmid = fmt.Sprint(inst.Appliance.VMID)
		}
	}
	errOut.printf("Before an upgrade, take a snapshot of this appliance on the node, as root there:\n"+
		"  pct snapshot %s %s\n"+
		"pco cannot take it: its token has no right to. A rollback to that snapshot is followed by\n"+
		"pco appliance recover in the appliance.\n", vmid, name)
}

// connectorRestart restarts the connectors of the appliance on a new
// cloudflared.
func (a *app) connectorRestart(paths store.Paths, out *screen) func(ctx context.Context) error {
	e := a.upgrade
	return func(ctx context.Context) error {
		conns := connector.NewManager(e.systemd, filepath.Join(paths.Local, tunnelsDir), nil, zerolog.Nop())
		return upgrade.ConnectorRestart{
			Connectors: conns,
			Systemd:    e.systemd,
			Now:        a.now,
			Sleep:      e.sleep,
			Say:        func(format string, args ...any) { out.printf(format+"\n", args...) },
		}.Restart(ctx)
	}
}
