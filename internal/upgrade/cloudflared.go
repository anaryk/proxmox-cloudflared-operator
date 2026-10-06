package upgrade

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// cloudflaredFile is the name of a kept package of cloudflared: those of
// Cloudflare are all called cloudflared-linux-<arch>.deb.
func cloudflaredFile(version, arch string) string {
	return "cloudflared_" + version + "_" + arch + ".deb"
}

// Cloudflared upgrades cloudflared to the version the manifest allows: the
// newest, or the one asked for. The package is checked against the sha256 of
// the manifest, the one of the installed version is kept, the new one is
// installed the way pco is, and restart then restarts the connectors and
// waits for them. With Rollback it installs the kept package instead, unless
// the manifest denies its version.
func (u *Upgrader) Cloudflared(ctx context.Context, o Options, m Manifest, restart func(ctx context.Context) error) (Result, error) {
	installed, err := u.installed(ctx, "cloudflared")
	if err != nil {
		return Result{}, err
	}
	if o.Rollback {
		return u.rollbackCloudflared(ctx, o, installed, m, restart)
	}
	res := Result{Package: "cloudflared", Installed: installed}
	entry, err := u.cloudflaredTarget(o.Version, installed, m)
	if err != nil || entry.Version == "" {
		return res, err
	}
	res.Target = entry.Version
	if o.Check {
		return res, nil
	}
	if !o.Yes {
		return res, ErrNotConfirmed
	}
	deb, err := u.fetchPackage(ctx, entry)
	if err != nil {
		return res, err
	}
	defer func() { _ = os.Remove(deb) }()
	if kept, err := u.keepCloudflared(ctx, installed, m); err != nil {
		res.Notes = append(res.Notes, fmt.Sprintf("the package of cloudflared %s could not be kept, so --rollback cannot go back to it: %v", installed, err))
	} else {
		res.Kept = kept
	}
	if err := u.install(ctx, deb, false); err != nil {
		return res, err
	}
	return res, u.restarted(ctx, restart, entry.Version, installed)
}

// cloudflaredTarget is the entry to upgrade to: the version asked for, which
// the manifest must allow and must be newer, or the newest it allows when that
// is newer; an empty entry when nothing is.
func (u *Upgrader) cloudflaredTarget(version, installed string, m Manifest) (Entry, error) {
	if version == "" {
		newest, ok := m.Newest(u.arch)
		if !ok {
			return Entry{}, fmt.Errorf("the manifest allows no cloudflared for %s", u.arch)
		}
		if compareVersions(newest.Version, installed) <= 0 {
			return Entry{}, nil
		}
		return newest, nil
	}
	if err := checkCloudflared(version); err != nil {
		return Entry{}, err
	}
	if reason, denied := m.Denied(version); denied {
		return Entry{}, fmt.Errorf("cloudflared %s is denied: %s", version, reason)
	}
	e, ok := m.Allowed(version)
	if !ok {
		var allowed []string
		for _, e := range m.Versions {
			allowed = append(allowed, e.Version)
		}
		return Entry{}, fmt.Errorf("cloudflared %s is not among the versions the manifest allows: %s", version, strings.Join(allowed, ", "))
	}
	if compareVersions(version, installed) <= 0 {
		return Entry{}, fmt.Errorf("cloudflared %s is installed, and %s is %w: --rollback goes back to the package the last upgrade kept",
			installed, version, ErrNotNewer)
	}
	return e, nil
}

// fetchPackage downloads the package of an entry for this architecture and
// checks it against the sha256 the manifest gives.
func (u *Upgrader) fetchPackage(ctx context.Context, e Entry) (string, error) {
	pkg, ok := e.Package(u.arch)
	if !ok {
		return "", fmt.Errorf("the manifest names no package of cloudflared %s for %s", e.Version, u.arch)
	}
	file, err := u.fetch.GetURL(ctx, pkg.URL, MaxPackage)
	if err != nil {
		return "", fmt.Errorf("downloading cloudflared %s: %w", e.Version, err)
	}
	return u.checked(file, cloudflaredFile(e.Version, u.arch), pkg.SHA256, "the manifest")
}

// keepCloudflared keeps the package of the installed version: the one kept
// already, or the one the manifest names for it.
func (u *Upgrader) keepCloudflared(ctx context.Context, installed string, m Manifest) (string, error) {
	name := cloudflaredFile(installed, u.arch)
	if kept, ok := u.alreadyKept(name); ok {
		return u.keep("cloudflared", kept, name)
	}
	e, ok := m.Allowed(installed)
	if !ok {
		return "", fmt.Errorf("the manifest does not allow cloudflared %s, so its package is not fetched", installed)
	}
	deb, err := u.fetchPackage(ctx, e)
	if err != nil {
		return "", err
	}
	kept, err := u.keep("cloudflared", deb, name)
	if err != nil {
		_ = os.Remove(deb)
	}
	return kept, err
}

func (u *Upgrader) rollbackCloudflared(ctx context.Context, o Options, installed string, m Manifest, restart func(ctx context.Context) error) (Result, error) {
	res := Result{Package: "cloudflared", Installed: installed}
	kept, version, err := u.keptPackage("cloudflared")
	if err != nil {
		return res, err
	}
	if compareVersions(version, installed) == 0 {
		return res, fmt.Errorf("%w: cloudflared %s is installed, and it is the package kept", ErrNothingKept, installed)
	}
	if reason, denied := m.Denied(version); denied {
		return res, fmt.Errorf("the kept cloudflared %s is denied: %s; it is not installed again", version, reason)
	}
	res.Target, res.Kept = version, kept
	if o.Check {
		return res, nil
	}
	if !o.Yes {
		return res, ErrNotConfirmed
	}
	if err := u.install(ctx, kept, true); err != nil {
		return res, err
	}
	return res, u.restarted(ctx, restart, version, installed)
}

// restarted restarts the connectors on the cloudflared just installed.
func (u *Upgrader) restarted(ctx context.Context, restart func(ctx context.Context) error, now, before string) error {
	if restart == nil {
		return nil
	}
	if err := restart(ctx); err != nil {
		return fmt.Errorf("cloudflared %s is installed, but %w; pco upgrade cloudflared --rollback goes back to %s", now, err, before)
	}
	return nil
}
