package upgrade

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func cfURL(version, arch string) string {
	return "https://github.com/cloudflare/cloudflared/releases/download/" + version + "/cloudflared-linux-" + arch + ".deb"
}

// manifestJSON is a manifest that allows versions, each with the packages of
// both architectures, and denies 2026.9.0.
func manifestJSON(versions ...string) string {
	var entries []string
	for _, v := range versions {
		entries = append(entries, `{"version":"`+v+`",`+
			`"amd64":{"url":"`+cfURL(v, "amd64")+`","sha256":"`+shaA+`"},`+
			`"arm64":{"url":"`+cfURL(v, "arm64")+`","sha256":"`+shaB+`"}}`)
	}
	return `{"schemaVersion":1,"updated":"2026-10-05","versions":[` + strings.Join(entries, ",") +
		`],"deny":[{"version":"2026.9.0","reason":"cloudflare/cloudflared#1737"}]}`
}

func TestTheManifestOfTheRepositoryIsRead(t *testing.T) {
	m, err := LoadManifest(filepath.Join("..", "..", "packaging", "cloudflared-versions.json"))

	require.NoError(t, err)
	require.False(t, m.Updated.IsZero())
	newest, ok := m.Newest("amd64")
	require.True(t, ok)
	require.NotEmpty(t, newest.Version)
}

func TestAManifestIsReadWithItsDate(t *testing.T) {
	m, err := ParseManifest([]byte(manifestJSON("2026.9.3", "2026.10.1", "2026.8.2")))

	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), m.Updated)
	newest, ok := m.Newest("arm64")
	require.True(t, ok)
	require.Equal(t, "2026.10.1", newest.Version, "by the order of versions, not of the list")
	pkg, ok := newest.Package("arm64")
	require.True(t, ok)
	require.Equal(t, Package{URL: cfURL("2026.10.1", "arm64"), SHA256: shaB}, pkg)
	_, ok = newest.Package("riscv64")
	require.False(t, ok)
	_, ok = m.Newest("riscv64")
	require.False(t, ok)

	reason, denied := m.Denied("2026.9.0")
	require.True(t, denied)
	require.Equal(t, "cloudflare/cloudflared#1737", reason)
	_, denied = m.Denied("2026.9.3")
	require.False(t, denied)

	e, ok := m.Allowed("2026.9.3")
	require.True(t, ok)
	require.Equal(t, "2026.9.3", e.Version)
	_, ok = m.Allowed("2026.9.0")
	require.False(t, ok)
}

func TestAManifestThatBreaksARuleIsRefused(t *testing.T) {
	good := manifestJSON("2026.9.3")
	change := func(f func(m map[string]any)) string {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(good), &m))
		f(m)
		b, err := json.Marshal(m)
		require.NoError(t, err)
		return string(b)
	}
	entry := func(m map[string]any) map[string]any { return m["versions"].([]any)[0].(map[string]any) }
	for _, tt := range []struct {
		name, manifest, want string
	}{
		{"not JSON", "{", "the manifest of cloudflared is not JSON"},
		{"of a newer pco", change(func(m map[string]any) { m["schemaVersion"] = 2 }),
			"the manifest of cloudflared has schema version 2, this pco reads 1: it was written for a newer pco"},
		{"without a schema", change(func(m map[string]any) { delete(m, "schemaVersion") }),
			"the manifest of cloudflared has schema version 0, this pco reads 1"},
		{"without its date", change(func(m map[string]any) { delete(m, "updated") }),
			`the manifest of cloudflared has no "updated"`},
		{"with a date that is none", change(func(m map[string]any) { m["updated"] = "2026-13-01" }),
			`the manifest of cloudflared: "updated" is "2026-13-01", not a date YYYY-MM-DD`},
		{"with a time for a date", change(func(m map[string]any) { m["updated"] = "2026-10-05T00:00:00Z" }),
			`not a date YYYY-MM-DD`},
		{"allowing nothing", change(func(m map[string]any) { m["versions"] = []any{} }),
			"the manifest of cloudflared allows no version"},
		{"with a version that is none", change(func(m map[string]any) { entry(m)["version"] = "latest" }),
			`"latest" is no version of cloudflared`},
		{"without a package", change(func(m map[string]any) { delete(entry(m), "arm64") }),
			"the manifest of cloudflared names no package of 2026.9.3 for arm64"},
		{"with a package of http", change(func(m map[string]any) {
			entry(m)["amd64"].(map[string]any)["url"] = "http://github.com/x.deb"
		}), "the package of cloudflared 2026.9.3 for amd64 is not at an https URL"},
		{"without a sha256", change(func(m map[string]any) { entry(m)["amd64"].(map[string]any)["sha256"] = "abc" }),
			"the package of cloudflared 2026.9.3 for amd64 has no sha256"},
		{"allowing a version twice", manifestJSON("2026.9.3", "2026.9.3"), "allows 2026.9.3 twice"},
		{"allowing what it denies", manifestJSON("2026.9.0"), "allows and denies 2026.9.0"},
		{"denying without a reason", change(func(m map[string]any) {
			m["deny"] = []any{map[string]any{"version": "2026.9.1", "reason": ""}}
		}), "the denial of cloudflared 2026.9.1 gives no reason"},
		{"denying what is no version", change(func(m map[string]any) {
			m["deny"] = []any{map[string]any{"version": "x", "reason": "r"}}
		}), `"x" is no version of cloudflared`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(tt.manifest))
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestAManifestOverItsCapIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cloudflared-versions.json")
	require.NoError(t, os.WriteFile(path, []byte(manifestJSON("2026.9.3")+strings.Repeat(" ", MaxSmallFile)), 0o600))

	_, err := LoadManifest(path)

	require.ErrorContains(t, err, "is larger than 1048576 bytes")
}
