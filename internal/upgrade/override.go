package upgrade

import (
	"errors"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	// BaseVar points every download at a release host on this machine.
	BaseVar = "PCO_UPGRADE_BASE"
	// KeyringVar names the keyring a release is checked with instead of the
	// one the package ships.
	KeyringVar = "PCO_UPGRADE_KEYRING"

	// OverrideLine is what pco upgrade prints first while either variable is
	// set, and what a daemon with either in its environment adds to the
	// problems of every state.
	OverrideLine = "the release host or key is overridden (" + BaseVar + ", " + KeyringVar + "); this is for tests only"
)

// The errors do not quote the values.
var (
	errBase = errors.New(BaseVar + " must be an http or https URL of a loopback address, such as " +
		"http://127.0.0.1:8788, without credentials, query or fragment; it is for tests only")
	errKeyring = errors.New(KeyringVar + " must be the absolute path of a keyring file; it is for tests only")
)

// Overrides are for tests only. Base is the release host on this machine that
// takes every request in place of GitHub, at the same path: the latest
// release at <base>/repos/<repository>/releases/latest, the files of a release
// at <base>/<repository>/releases/download/v<version>/<name>, and a package of
// cloudflared at <base> and the path of its URL in the manifest. Keyring is
// the keyring a release is checked with. Neither leaves the machine: Base is
// an address of loopback, never a name the resolver could send elsewhere.
type Overrides struct {
	Base    string
	Keyring string
}

// Set says whether anything is overridden.
func (o Overrides) Set() bool { return o.Base != "" || o.Keyring != "" }

// OverridesFromEnv reads the overrides from the environment of the process.
func OverridesFromEnv() (Overrides, error) { return OverridesFrom(os.Getenv) }

// OverridesFrom reads the overrides through getenv.
func OverridesFrom(getenv func(string) string) (Overrides, error) {
	var o Overrides
	if raw := getenv(BaseVar); raw != "" {
		base, err := loopbackBase(raw)
		if err != nil {
			return Overrides{}, err
		}
		o.Base = base
	}
	if raw := getenv(KeyringVar); raw != "" {
		if !filepath.IsAbs(raw) {
			return Overrides{}, errKeyring
		}
		o.Keyring = filepath.Clean(raw)
	}
	return o, nil
}

// Overridden says whether either variable is set at all, valid or not.
func Overridden(getenv func(string) string) bool {
	return getenv(BaseVar) != "" || getenv(KeyringVar) != ""
}

func loopbackBase(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errBase
	}
	addr, err := netip.ParseAddr(u.Hostname())
	if err != nil || !addr.Unmap().IsLoopback() || addr.Is4In6() {
		return "", errBase
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}
