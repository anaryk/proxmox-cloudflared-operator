package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	checksumsName = "checksums.txt"
	signatureName = checksumsName + ".sig"
)

// checksumLineRe is a line of checksums.txt as sha256sum writes it.
var checksumLineRe = regexp.MustCompile(`^([0-9a-f]{64})  (.+)$`)

// release is a release whose checksums.txt was found signed by a key of the
// keyring: what it lists is what the release vouches for.
type release struct {
	version     string
	fingerprint string
	sums        map[string][]string
}

// sumOf returns the checksum of the one line that names name, read the way
// the installer reads it: line by line, never handed to sha256sum -c, which
// would check whatever files the lines name.
func (r release) sumOf(name string) (string, error) {
	switch sums := r.sums[name]; len(sums) {
	case 1:
		return sums[0], nil
	case 0:
		return "", fmt.Errorf("checksums.txt of release v%s has no line for %s", r.version, name)
	default:
		return "", fmt.Errorf("checksums.txt of release v%s has %d lines for %s, expected one", r.version, len(sums), name)
	}
}

func parseChecksums(b []byte) map[string][]string {
	sums := make(map[string][]string)
	for line := range strings.Lines(string(b)) {
		if m := checksumLineRe.FindStringSubmatch(strings.TrimRight(line, "\n")); m != nil {
			sums[m[2]] = append(sums[m[2]], m[1])
		}
	}
	return sums
}

// release fetches checksums.txt of a release and its signature, and checks
// the signature with the keyring before anything of the release is used.
// A release is fetched once per run.
func (u *Upgrader) release(ctx context.Context, version string) (release, error) {
	if r, ok := u.releases[version]; ok {
		return r, nil
	}
	sums, err := u.fetch.Get(ctx, version, checksumsName, MaxSmallFile)
	if err != nil {
		return release{}, fmt.Errorf("downloading checksums.txt of release v%s: %w", version, err)
	}
	defer func() { _ = os.Remove(sums) }()
	sig, err := u.fetch.Get(ctx, version, signatureName, MaxSmallFile)
	if err != nil {
		return release{}, fmt.Errorf("downloading checksums.txt.sig of release v%s: %w", version, err)
	}
	defer func() { _ = os.Remove(sig) }()
	fpr, err := u.verify.Verify(ctx, u.keyring, sig, sums)
	if err != nil {
		return release{}, fmt.Errorf("release v%s: %w", version, err)
	}
	b, err := os.ReadFile(sums)
	if err != nil {
		return release{}, err
	}
	r := release{version: version, fingerprint: fpr, sums: parseChecksums(b)}
	u.releases[version] = r
	u.say("release v%s: checksums.txt is signed by key %s", version, fpr)
	return r, nil
}

// fetchFile downloads a file of a verified release, checks it against the
// release's checksum and returns it under its own name in the work
// directory, where apt-get takes a package by its name.
func (u *Upgrader) fetchFile(ctx context.Context, r release, name string, max int64) (string, error) {
	want, err := r.sumOf(name)
	if err != nil {
		return "", err
	}
	path, err := u.fetch.Get(ctx, r.version, name, max)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", name, err)
	}
	return u.checked(path, name, want, "checksums.txt of release v"+r.version)
}

// checked moves a download whose sha256 is want to name in the work
// directory, and removes one whose sha256 is not.
func (u *Upgrader) checked(path, name, want, source string) (string, error) {
	if err := checkSum(path, name, want, source); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	final := filepath.Join(u.workDir, name)
	if err := os.Rename(path, final); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return final, nil
}

// checkSum compares the sha256 of the file at path with want, which source
// gives for name.
func checkSum(path, name, want, source string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("reading %s: %w", name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("the sha256 of %s is %s, but %s says %s", name, got, source, want)
	}
	return nil
}

// ReleaseManifest returns the manifest of cloudflared that a release lists in
// its signed checksums.txt, with the version of that release: of version, or
// of the latest release when version is empty.
func (u *Upgrader) ReleaseManifest(ctx context.Context, version string) (Manifest, string, error) {
	if version == "" {
		latest, err := u.fetch.Latest(ctx)
		if err != nil {
			return Manifest{}, "", err
		}
		version = latest
	}
	r, err := u.release(ctx, version)
	if err != nil {
		return Manifest{}, "", err
	}
	if _, err := r.sumOf(ManifestName); err != nil {
		return Manifest{}, "", fmt.Errorf("release v%s lists no %s in its checksums.txt, so it names no vetted cloudflared", version, ManifestName)
	}
	path, err := u.fetchFile(ctx, r, ManifestName, MaxSmallFile)
	if err != nil {
		return Manifest{}, "", err
	}
	defer func() { _ = os.Remove(path) }()
	m, err := LoadManifest(path)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("release v%s: %w", version, err)
	}
	return m, version, nil
}
