package docscheck

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// envName finds the variables of pco: PCO_ and capital letters, digits and
// underscores. A match that ends in an underscore is a mention of a family,
// such as PCO_E2E_*, and not a name.
var envName = regexp.MustCompile(`PCO_[A-Z0-9_]+`)

// namesIn returns the variables a text names.
func namesIn(text string) []string {
	var out []string
	for _, name := range envName.FindAllString(text, -1) {
		if !strings.HasSuffix(name, "_") {
			out = append(out, name)
		}
	}
	return out
}

// notProduct are the names that are not part of the product, and so not in
// the table of docs/operations.md: the end-to-end suite and the test suites
// that need a tool, the release process, and a constant of the installer
// script. docs/development.md and packaging/RELEASING.md describe them.
var (
	notProductPrefixes = []string{"PCO_E2E_", "PCO_REQUIRE_", "PCO_RELEASE_"}
	notProduct         = map[string]string{
		"PCO_APPLIANCE_DIR": "where the release workflow puts the appliance template",
		"PCO_BIN":           "a constant of scripts/install.sh, assigned there and never read from the environment",
	}
)

// sourcesOfNames are where the product reads a variable: its Go code, the
// installer script, the units and the scripts of the appliance.
func sourcesOfNames(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, dir := range []string{"cmd", "internal", "hack", "test"} {
		err := filepath.WalkDir(repo(dir), func(path string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && (d.Name() == "testdata" || d.Name() == "node_modules"):
				return filepath.SkipDir
			case !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go"):
				files = append(files, path)
			}
			return nil
		})
		require.NoError(t, err)
	}
	files = append(files, repo("scripts/install.sh"))
	for _, dir := range []string{"packaging/systemd", "packaging/scripts", "packaging/appliance"} {
		err := filepath.WalkDir(repo(dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && !strings.HasSuffix(path, "_test.sh") && !strings.HasSuffix(path, ".tar.zst") {
				files = append(files, path)
			}
			return nil
		})
		require.NoError(t, err)
	}
	return files
}

func isNotProduct(name string) bool {
	if _, ok := notProduct[name]; ok {
		return true
	}
	return slices.ContainsFunc(notProductPrefixes, func(p string) bool { return strings.HasPrefix(name, p) })
}

// A variable that the product reads, and that the table of environment
// variables in docs/operations.md does not name, fails here.
func TestEveryVariableHasARow(t *testing.T) {
	documented := make(map[string]bool)
	for _, span := range spans(section(t, page(t, "operations.md"), "Environment variables")) {
		for _, name := range namesIn(span) {
			documented[name] = true
		}
	}
	require.NotEmpty(t, documented, "docs/operations.md has no table of environment variables")

	found := make(map[string][]string)
	for _, file := range sourcesOfNames(t) {
		b, err := os.ReadFile(file)
		require.NoError(t, err)
		rel, err := filepath.Rel(repo(""), file)
		require.NoError(t, err)
		for _, name := range namesIn(string(b)) {
			if !isNotProduct(name) {
				found[name] = append(found[name], filepath.ToSlash(rel))
			}
		}
	}
	require.NotEmpty(t, found)
	for name, files := range found {
		require.True(t, documented[name],
			"%s is read in %s and has no row in the environment variables of docs/operations.md: add one, or if it is a variable "+
				"of the tests or the release add it to notProduct", name, files[0])
	}
}
