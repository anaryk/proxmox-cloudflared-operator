package ui_test

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/ui"
)

// make test runs these without the webui tag; the build-ui job of CI runs them
// again with it, on the interface the ui job built.

func TestWithoutTheInterface(t *testing.T) {
	if ui.Built {
		t.Skip("built with the webui tag")
	}
	assets := ui.Assets()
	require.Equal(t, []string{"index.html"}, files(t, assets))

	page, err := fs.ReadFile(assets, "index.html")
	require.NoError(t, err)
	require.Contains(t, string(page), "This build of pco has no web interface. Build it with make build UI=1.")
	// The page is served under the same Content-Security-Policy as the interface.
	require.NotRegexp(t, `(?i)<script|<style|\sstyle=|\son[a-z]+=`, string(page))
}

func TestWithTheInterface(t *testing.T) {
	if !ui.Built {
		t.Skip("built without the webui tag")
	}
	assets := ui.Assets()
	page, err := fs.ReadFile(assets, "index.html")
	require.NoError(t, err)
	require.NotContains(t, string(page), "no web interface")

	notices, err := fs.ReadFile(assets, "licenses.txt")
	require.NoError(t, err)
	require.Contains(t, string(notices), "Name: react")

	refs := regexp.MustCompile(`(?:src|href)="/(assets/[^"]+)"`).FindAllStringSubmatch(string(page), -1)
	require.NotEmpty(t, refs, "index.html loads nothing from /assets/")
	for _, ref := range refs {
		_, err := fs.Stat(assets, ref[1])
		require.NoError(t, err, "index.html names /%s", ref[1])
	}
	for _, name := range files(t, assets) {
		if name == "index.html" || name == "licenses.txt" {
			continue
		}
		require.True(t, strings.HasPrefix(name, "assets/"), "%s is outside assets/", name)
	}
}

func files(t *testing.T, fsys fs.FS) []string {
	t.Helper()
	var names []string
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			names = append(names, name)
		}
		return nil
	})
	require.NoError(t, err)
	return names
}
