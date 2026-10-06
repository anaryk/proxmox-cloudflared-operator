package upgrade

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"time"
)

const (
	// ManifestName is the manifest's file in a release, and in the package
	// below /usr/share/pco.
	ManifestName = "cloudflared-versions.json"
	// ShippedManifest is the manifest the installed package of pco carries.
	ShippedManifest = "/usr/share/pco/" + ManifestName

	manifestSchema = 1
)

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Manifest is cloudflared-versions.json: the versions of cloudflared a release
// of pco vetted, with the packages of each architecture and their sha256,
// and the versions it denies with the reason.
type Manifest struct {
	Updated  time.Time // "updated": the day the list was last changed
	Versions []Entry
	Deny     []Denial
}

// Entry is a version of cloudflared the manifest allows.
type Entry struct {
	Version string
	AMD64   Package
	ARM64   Package
}

// Package is the .deb of a version for one architecture.
type Package struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Denial is a version the manifest denies, and why.
type Denial struct {
	Version string `json:"version"`
	Reason  string `json:"reason"`
}

// Package returns the package of the entry for arch.
func (e Entry) Package(arch string) (Package, bool) {
	switch arch {
	case "amd64":
		return e.AMD64, true
	case "arm64":
		return e.ARM64, true
	}
	return Package{}, false
}

// Newest is the highest version the manifest allows for arch.
func (m Manifest) Newest(arch string) (Entry, bool) {
	var newest Entry
	found := false
	for _, e := range m.Versions {
		if _, ok := e.Package(arch); !ok {
			continue
		}
		if !found || compareVersions(e.Version, newest.Version) > 0 {
			newest, found = e, true
		}
	}
	return newest, found
}

// Allowed returns the entry of version when the manifest allows it.
func (m Manifest) Allowed(version string) (Entry, bool) {
	for _, e := range m.Versions {
		if e.Version == version {
			return e, true
		}
	}
	return Entry{}, false
}

// Denied says whether the manifest denies version, and why.
func (m Manifest) Denied(version string) (reason string, denied bool) {
	for _, d := range m.Deny {
		if d.Version == version {
			return d.Reason, true
		}
	}
	return "", false
}

// LoadManifest reads the manifest at path.
func LoadManifest(path string) (Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("reading the manifest of cloudflared: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, MaxSmallFile+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(b) > MaxSmallFile {
		return Manifest{}, fmt.Errorf("%s is larger than %d bytes", path, MaxSmallFile)
	}
	return ParseManifest(b)
}

// manifestFile is the JSON of the manifest.
type manifestFile struct {
	SchemaVersion int     `json:"schemaVersion"`
	Updated       *string `json:"updated"`
	Versions      []struct {
		Version string   `json:"version"`
		AMD64   *Package `json:"amd64"`
		ARM64   *Package `json:"arm64"`
	} `json:"versions"`
	Deny []Denial `json:"deny"`
}

// ParseManifest reads a manifest and checks it as the tests of the
// repository do: a date, both architectures for every version, an https URL
// and a sha256 for every package, no version twice and none both allowed and
// denied, a reason for every denial.
func ParseManifest(b []byte) (Manifest, error) {
	var f manifestFile
	if err := json.Unmarshal(b, &f); err != nil {
		return Manifest{}, errors.New("the manifest of cloudflared is not JSON")
	}
	switch {
	case f.SchemaVersion > manifestSchema:
		return Manifest{}, fmt.Errorf("the manifest of cloudflared has schema version %d, this pco reads %d: it was written for a newer pco",
			f.SchemaVersion, manifestSchema)
	case f.SchemaVersion != manifestSchema:
		return Manifest{}, fmt.Errorf("the manifest of cloudflared has schema version %d, this pco reads %d", f.SchemaVersion, manifestSchema)
	case f.Updated == nil:
		return Manifest{}, errors.New(`the manifest of cloudflared has no "updated"`)
	}
	updated, err := time.Parse(time.DateOnly, *f.Updated)
	if err != nil {
		return Manifest{}, fmt.Errorf(`the manifest of cloudflared: "updated" is %q, not a date YYYY-MM-DD`, shorten(*f.Updated))
	}
	m := Manifest{Updated: updated, Deny: f.Deny}
	if len(f.Versions) == 0 {
		return Manifest{}, errors.New("the manifest of cloudflared allows no version")
	}
	seen := make(map[string]bool)
	for _, v := range f.Versions {
		if err := checkCloudflared(v.Version); err != nil {
			return Manifest{}, fmt.Errorf("the manifest of cloudflared: %w", err)
		}
		if seen[v.Version] {
			return Manifest{}, fmt.Errorf("the manifest of cloudflared allows %s twice", v.Version)
		}
		seen[v.Version] = true
		e := Entry{Version: v.Version}
		for _, p := range []struct {
			arch string
			pkg  *Package
			into *Package
		}{{"amd64", v.AMD64, &e.AMD64}, {"arm64", v.ARM64, &e.ARM64}} {
			if err := checkPackage(v.Version, p.arch, p.pkg); err != nil {
				return Manifest{}, err
			}
			*p.into = *p.pkg
		}
		m.Versions = append(m.Versions, e)
	}
	denied := make(map[string]bool)
	for _, d := range f.Deny {
		if err := checkCloudflared(d.Version); err != nil {
			return Manifest{}, fmt.Errorf("the manifest of cloudflared: %w", err)
		}
		switch {
		case d.Reason == "":
			return Manifest{}, fmt.Errorf("the denial of cloudflared %s gives no reason", d.Version)
		case seen[d.Version]:
			return Manifest{}, fmt.Errorf("the manifest of cloudflared allows and denies %s", d.Version)
		case denied[d.Version]:
			return Manifest{}, fmt.Errorf("the manifest of cloudflared denies %s twice", d.Version)
		}
		denied[d.Version] = true
	}
	return m, nil
}

func checkPackage(version, arch string, p *Package) error {
	if p == nil {
		return fmt.Errorf("the manifest of cloudflared names no package of %s for %s", version, arch)
	}
	if u, err := url.Parse(p.URL); err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("the package of cloudflared %s for %s is not at an https URL", version, arch)
	}
	if !sha256Re.MatchString(p.SHA256) {
		return fmt.Errorf("the package of cloudflared %s for %s has no sha256", version, arch)
	}
	return nil
}
