package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	// cloudflaredBin is the binary the units of the connectors run.
	cloudflaredBin = "/usr/bin/cloudflared"

	keyURL  = "https://pkg.cloudflare.com/cloudflare-main.gpg"
	repoURL = "https://pkg.cloudflare.com/cloudflared"
)

// sourcesContent is the deb822 apt source of cloudflared, signed by the key
// at keyring.
func sourcesContent(keyring string) []byte {
	return []byte("Types: deb\n" +
		"URIs: " + repoURL + "\n" +
		"Suites: any\n" +
		"Components: main\n" +
		"Signed-By: " + keyring + "\n")
}

// ensureCloudflared keeps a cloudflared that runs, or installs it from
// Cloudflare's apt repository when the operator agrees.
func (r *run) ensureCloudflared(ctx context.Context) error {
	if out, err := r.run.Run(ctx, cloudflaredBin, "--version"); err == nil {
		version, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
		r.ask.Info("cloudflared: %s, kept", version)
		return nil
	}
	install, err := r.choose(r.o.InstallCloudflared, true, "cloudflared is not installed. Install it from Cloudflare's apt repository?")
	if err != nil {
		return err
	}
	if !install {
		r.ask.Warn("cloudflared: not installed; the tunnels cannot run until %s is there", cloudflaredBin)
		return nil
	}
	if err := r.ensureKeyring(ctx); err != nil {
		return err
	}
	if err := r.ensureSources(); err != nil {
		return err
	}
	if _, err := r.run.Run(ctx, "apt-get", "update"); err != nil {
		return fmt.Errorf("updating the package lists: %w", err)
	}
	if _, err := r.run.Run(ctx, "apt-get", "install", "-y", "cloudflared"); err != nil {
		return fmt.Errorf("installing cloudflared: %w", err)
	}
	if err := r.record(func(m *Manifest) { m.InstalledCloudflared = true }); err != nil {
		return err
	}
	r.ask.Info("cloudflared: installed from %s", repoURL)
	return nil
}

// ensureKeyring downloads the key of the repository, unless it is there. It
// is written next to its place and moved there once it is complete.
func (r *run) ensureKeyring(ctx context.Context) (err error) {
	path := r.host.keyring
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating a file in %s: %w", dir, err)
	}
	name := tmp.Name()
	_ = tmp.Close()
	defer func() {
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	if _, err := r.run.Run(ctx, "curl", "--disable", "--fail", "--silent", "--show-error", "--location",
		"--proto", "=https", "--tlsv1.2", "--max-time", "60", "--max-filesize", "1048576",
		"--output", name, keyURL); err != nil {
		return fmt.Errorf("downloading the key of the cloudflared repository: %w", err)
	}
	if info, err := os.Stat(name); err != nil || info.Size() == 0 {
		return fmt.Errorf("the key downloaded from %s is empty", keyURL)
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return r.record(func(m *Manifest) { m.AddedKeyring = true })
}

// ensureSources writes the apt source of cloudflared. A file of that name
// with other content is the admin's and is left as it is.
func (r *run) ensureSources() error {
	path, want := r.host.sources, sourcesContent(r.host.keyring)
	have, err := os.ReadFile(path)
	switch {
	case err == nil && bytes.Equal(have, want):
		return nil
	case err == nil:
		r.ask.Warn("%s is not the apt source setup writes; it is left as it is", path)
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if err := writeFileAtomic(path, want, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return r.record(func(m *Manifest) { m.AddedAptSource = true })
}

// removeCloudflared removes what setup installed of cloudflared.
func (u *uninstall) removeCloudflared(ctx context.Context) {
	m := u.manifest
	if m.InstalledCloudflared {
		if _, err := u.run.Run(ctx, "apt-get", "remove", "-y", "cloudflared"); err != nil {
			u.fail("removing the cloudflared package: %v", err)
		} else {
			u.ask.Info("cloudflared: removed the package")
		}
	}
	if m.AddedAptSource {
		have, err := os.ReadFile(u.host.sources)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			u.fail("reading %s: %v", u.host.sources, err)
		case !bytes.Equal(have, sourcesContent(u.host.keyring)):
			u.ask.Warn("%s was changed since setup wrote it; it is left as it is", u.host.sources)
		default:
			u.removeFile(u.host.sources)
		}
	}
	if m.AddedKeyring {
		u.removeFile(u.host.keyring)
	}
}

func (u *uninstall) removeFile(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		u.fail("removing %s: %v", path, err)
		return
	}
	u.ask.Info("removed %s", path)
}
