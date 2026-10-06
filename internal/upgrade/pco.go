package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// pcoFile is the name of the package of pco in a release.
func pcoFile(version, arch string) string { return "pco_" + version + "_" + arch + ".deb" }

// Pco upgrades pco: it fetches checksums.txt and its signature, checks the
// signature, fetches the .deb and checks its sha256, keeps the package of the
// installed version in the directory previous of the work directory, installs
// the new one and holds both packages again. With Rollback it installs the
// kept package instead.
func (u *Upgrader) Pco(ctx context.Context, o Options) (Result, error) {
	installedDeb, err := u.installed(ctx, "pco")
	if err != nil {
		return Result{}, err
	}
	installed := releaseVersion(installedDeb)
	if o.Rollback {
		return u.rollbackPco(ctx, o, installed, installedDeb)
	}
	res := Result{Package: "pco", Installed: installed}
	target, err := u.pcoTarget(ctx, o.Version, installedDeb)
	if err != nil || target == "" {
		return res, err
	}
	res.Target, res.Release = target, target
	if o.Check {
		if o.Keep {
			u.keepPcoNoted(ctx, &res)
		}
		return res, nil
	}
	if !o.Yes {
		return res, ErrNotConfirmed
	}
	r, err := u.release(ctx, target)
	if err != nil {
		return res, err
	}
	res.Fingerprint = r.fingerprint
	deb, err := u.fetchFile(ctx, r, pcoFile(target, u.arch), MaxPackage)
	if err != nil {
		return res, err
	}
	defer func() { _ = os.Remove(deb) }()
	u.keepPcoNoted(ctx, &res)
	return res, u.install(ctx, deb, false)
}

// keepPcoNoted keeps the package of the installed version, or notes why it
// cannot.
func (u *Upgrader) keepPcoNoted(ctx context.Context, res *Result) {
	kept, err := u.keepPco(ctx, res.Installed)
	if err != nil {
		res.Notes = append(res.Notes, fmt.Sprintf("the package of pco %s cannot be kept, so --rollback cannot go back to it: %v", res.Installed, err))
		return
	}
	res.Kept = kept
}

// pcoTarget is the version to upgrade to: the one asked for, which must be
// newer, or the latest release when it is newer; empty when nothing is.
func (u *Upgrader) pcoTarget(ctx context.Context, version, installedDeb string) (string, error) {
	if version != "" {
		v, err := cleanRelease(version)
		if err != nil {
			return "", err
		}
		if compareVersions(debVersion(v), installedDeb) <= 0 {
			return "", fmt.Errorf("pco %s is installed, and %s is %w: --rollback goes back to the package the last upgrade kept",
				releaseVersion(installedDeb), v, ErrNotNewer)
		}
		return v, nil
	}
	latest, err := u.fetch.Latest(ctx)
	if err != nil {
		return "", err
	}
	if compareVersions(debVersion(latest), installedDeb) <= 0 {
		return "", nil
	}
	return latest, nil
}

// keepPco keeps the package of the installed version: the one kept already,
// or the one of its release, checked as any other.
func (u *Upgrader) keepPco(ctx context.Context, installed string) (string, error) {
	name := pcoFile(installed, u.arch)
	if path, ok := u.alreadyKept("pco", name); ok {
		return path, nil
	}
	if _, err := cleanRelease(installed); err != nil {
		return "", err
	}
	r, err := u.release(ctx, installed)
	if err != nil {
		return "", err
	}
	deb, err := u.fetchFile(ctx, r, name, MaxPackage)
	if err != nil {
		return "", err
	}
	sum, err := r.sumOf(name)
	if err != nil {
		return "", err
	}
	kept, err := u.keep("pco", deb, name, sum)
	if err != nil {
		_ = os.Remove(deb)
	}
	return kept, err
}

// rollbackPco installs the kept package of pco, unless the store holds an
// object the kept pco could not read.
func (u *Upgrader) rollbackPco(ctx context.Context, o Options, installed, installedDeb string) (Result, error) {
	res := Result{Package: "pco", Installed: installed}
	kept, version, err := u.keptPackage("pco")
	if err != nil {
		return res, err
	}
	if compareVersions(debVersion(version), installedDeb) == 0 {
		return res, fmt.Errorf("%w: pco %s is installed, and it is the package kept", ErrNothingKept, installed)
	}
	res.Target, res.Kept = version, kept
	if err := u.checkSchema(ctx, kept, version); err != nil {
		return res, err
	}
	if o.Check {
		return res, nil
	}
	if !o.Yes {
		return res, ErrNotConfirmed
	}
	return res, u.install(ctx, kept, true)
}

// checkSchema refuses a rollback to a pco that could not read the store: one
// that knows a lower schema version than an object in it was written with.
// The kept package is unpacked into the work directory and its pco asked.
func (u *Upgrader) checkSchema(ctx context.Context, deb, version string) error {
	if u.store == nil {
		return errors.New("a rollback of pco needs the store to check, and none was given")
	}
	// The unpacked pco takes about three times its package; four leave a
	// margin.
	info, err := os.Stat(deb)
	if err != nil {
		return err
	}
	if err := roomFor(u.free, u.workDir, "to unpack "+filepath.Base(deb), 4*uint64(info.Size())); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(u.workDir, extractDir)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if _, err := u.run.Run(ctx, "dpkg-deb", "-x", deb, dir); err != nil {
		return fmt.Errorf("unpacking %s: %w", filepath.Base(deb), err)
	}
	known, err := u.schemaOf(ctx, filepath.Join(dir, "usr", "bin", "pco"), version)
	if err != nil {
		return err
	}
	newest, path, err := store.NewestSchema(*u.store)
	if err != nil {
		return fmt.Errorf("reading the schema versions of the store: %w", err)
	}
	if newest > known {
		return fmt.Errorf("the store holds %s with schema version %d, and pco %s reads %d at most: "+
			"after a rollback the daemon could not read its store; the snapshot taken before the upgrade goes back with the store",
			path, newest, version, known)
	}
	return nil
}

// schemaOf asks the pco at bin for the schema version it reads. A pco from
// before version --json reads schema version 1, the only one there was then;
// it must run all the same.
func (u *Upgrader) schemaOf(ctx context.Context, bin, version string) (int, error) {
	out, err := u.run.Run(ctx, bin, "version", "--json")
	if err != nil {
		if _, err := u.run.Run(ctx, bin, "version"); err != nil {
			return 0, fmt.Errorf("the kept pco %s does not run: %w", version, err)
		}
		return 1, nil
	}
	var v struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if json.Unmarshal([]byte(out), &v) != nil || v.SchemaVersion < 1 {
		return 0, fmt.Errorf("the kept pco %s names no schema version it reads", version)
	}
	return v.SchemaVersion, nil
}
