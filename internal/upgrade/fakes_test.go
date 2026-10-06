package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
)

var update = flag.Bool("update", false, "write the golden files of the tests")

// fakeRunner is a host that runs nothing: do answers every command, and every
// command line is remembered.
type fakeRunner struct {
	mu  sync.Mutex
	ran []string
	do  func(name string, args ...string) (string, error)
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.mu.Lock()
	r.ran = append(r.ran, strings.Join(append([]string{name}, args...), " "))
	r.mu.Unlock()
	return r.do(name, args...)
}

func (r *fakeRunner) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ran...)
}

func sum(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// fakeFetcher is a release host in memory. A release is its files by name,
// checksums.txt among them; a URL of a package of cloudflared is its content.
type fakeFetcher struct {
	t        *testing.T
	dir      string
	latest   string
	releases map[string]map[string]string
	urls     map[string]string
	asked    []string
}

// addRelease adds a release whose checksums.txt lists every file of it, and
// extra lines; its signature is "signed: " and the checksums.
func (f *fakeFetcher) addRelease(version string, files map[string]string, extra ...string) {
	var lines []string
	for name, content := range files {
		lines = append(lines, sum(content)+"  "+name)
	}
	lines = append(lines, extra...)
	slices.Sort(lines)
	sums := strings.Join(lines, "\n") + "\n"
	all := map[string]string{"checksums.txt": sums, "checksums.txt.sig": "signed: " + sums}
	for name, content := range files {
		all[name] = content
	}
	if f.releases == nil {
		f.releases = make(map[string]map[string]string)
	}
	f.releases[version] = all
}

func (f *fakeFetcher) write(content string) (string, error) {
	file, err := os.CreateTemp(f.dir, fetchPrefix+"*")
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	_, err = file.WriteString(content)
	return file.Name(), err
}

func (f *fakeFetcher) Latest(context.Context) (string, error) {
	f.asked = append(f.asked, "latest")
	if f.latest == "" {
		return "", errors.New("no release")
	}
	return f.latest, nil
}

func (f *fakeFetcher) Get(_ context.Context, version, name string, _ int64) (string, error) {
	f.asked = append(f.asked, "get v"+version+"/"+name)
	content, ok := f.releases[version][name]
	if !ok {
		return "", fmt.Errorf("GET v%s/%s: 404 Not Found", version, name)
	}
	return f.write(content)
}

func (f *fakeFetcher) GetURL(_ context.Context, rawURL string, _ int64) (string, error) {
	f.asked = append(f.asked, "get "+rawURL)
	content, ok := f.urls[rawURL]
	if !ok {
		return "", fmt.Errorf("GET %s: 404 Not Found", rawURL)
	}
	return f.write(content)
}

// fakeVerifier calls a signature good when it is "signed: " and the file, as
// the fake fetcher makes them, unless err says otherwise.
type fakeVerifier struct {
	err   error
	asked []string
}

func (v *fakeVerifier) Verify(_ context.Context, keyring, sig, file string) (string, error) {
	v.asked = append(v.asked, "verify with "+keyring)
	if v.err != nil {
		return "", v.err
	}
	s, err := os.ReadFile(sig)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	if string(s) != "signed: "+string(b) {
		return "", ErrSignature
	}
	return fprPrimary, nil
}

// fakeHost answers the commands of an upgrade: dpkg-query with the installed
// versions, apt-get and apt-mark with their errors, dpkg-deb -x with a pco in
// the directory, and that pco with what it says of its version.
type fakeHost struct {
	installed map[string]string
	aptErr    error
	holdErr   error
	// versionJSON is what the unpacked pco answers to version --json; empty
	// for a pco that does not know the flag.
	versionJSON string
	// runs says whether the unpacked pco runs at all.
	runs bool
}

func (h *fakeHost) do(name string, args ...string) (string, error) {
	switch {
	case name == "dpkg-query":
		pkg := args[len(args)-1]
		v, ok := h.installed[pkg]
		if !ok {
			return "", fmt.Errorf("dpkg-query: exit status 1: no packages found matching %s", pkg)
		}
		return "ii " + v, nil
	case name == "apt-get":
		return "", h.aptErr
	case name == "apt-mark":
		return "", h.holdErr
	case name == "dpkg-deb":
		bin := filepath.Join(args[2], "usr", "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			return "", err
		}
		return "", os.WriteFile(filepath.Join(bin, "pco"), []byte("the kept pco"), 0o755)
	case strings.HasSuffix(name, "/usr/bin/pco"):
		if !h.runs {
			return "", errors.New("exec format error")
		}
		if slices.Contains(args, "--json") {
			if h.versionJSON == "" {
				return "", errors.New(`exit status 1: --json has no meaning for "pco version"`)
			}
			return h.versionJSON, nil
		}
		return "pco 1.2.3 (abc, 2026-09-01)\n", nil
	}
	return "", fmt.Errorf("unexpected command %s %v", name, args)
}

var (
	fetchNameRe   = regexp.MustCompile(`\.fetch-[0-9]+`)
	extractNameRe = regexp.MustCompile(`\.extract-[0-9]+`)
)

// transcript is what the host ran, with the work directory as $WORK and the
// random parts of the names of the downloads and of the unpacked package as *.
func transcript(lines []string, work string) string {
	var b strings.Builder
	for _, l := range lines {
		l = strings.ReplaceAll(l, work, "$WORK")
		l = fetchNameRe.ReplaceAllString(l, ".fetch-*")
		l = extractNameRe.ReplaceAllString(l, ".extract-*")
		b.WriteString(l + "\n")
	}
	return b.String()
}

// golden compares got with testdata/<name>.golden, and writes it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), got)
}

// fakeClock is a clock that only moves when the code under test sleeps.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.now = c.now.Add(d)
	return nil
}

// fakeConnectors answers the status of each connector from a script: the
// first status is the one before the restart, and each later one is what a
// poll after it sees; the last stays.
type fakeConnectors struct {
	ids      []string
	statuses map[string][]connector.Status
}

func (c *fakeConnectors) List(context.Context) ([]string, error) { return c.ids, nil }

func (c *fakeConnectors) Status(_ context.Context, id string) (connector.Status, error) {
	s := c.statuses[id]
	if len(s) == 0 {
		return connector.Status{}, fmt.Errorf("no status of %s", id)
	}
	st := s[0]
	if len(s) > 1 {
		c.statuses[id] = s[1:]
	}
	return st, nil
}

// fakeSystemd remembers the restarts.
type fakeSystemd struct {
	connector.Systemd
	restarted []string
	err       error
}

func (s *fakeSystemd) Restart(_ context.Context, unit string) error {
	s.restarted = append(s.restarted, unit)
	return s.err
}
