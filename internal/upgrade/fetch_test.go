package upgrade

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// releaseHost is a release host of a test: it serves files by path and
// remembers what was asked.
type releaseHost struct {
	mu    sync.Mutex
	files map[string]string
	asked []string
}

func (h *releaseHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.asked = append(h.asked, r.URL.Path)
	body, ok := h.files[r.URL.Path]
	h.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write([]byte(body))
}

func (h *releaseHost) paths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.asked...)
}

func fetcherFor(t *testing.T, base string) (Fetcher, string) {
	t.Helper()
	dir := t.TempDir()
	return NewFetcher(FetchConfig{Dir: dir, Base: base}), dir
}

func TestTheFetcherAsksTheOverrideAtGitHubsPaths(t *testing.T) {
	host := &releaseHost{files: map[string]string{
		"/repos/anaryk/proxmox-cloudflared-operator/releases/latest":                     `{"tag_name":"v1.2.4","name":"pco 1.2.4"}`,
		"/anaryk/proxmox-cloudflared-operator/releases/download/v1.2.4/checksums.txt":    "sums\n",
		"/cloudflare/cloudflared/releases/download/2026.9.3/cloudflared-linux-amd64.deb": "cloudflared package",
	}}
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	f, dir := fetcherFor(t, srv.URL)

	latest, err := f.Latest(t.Context())
	require.NoError(t, err)
	require.Equal(t, "1.2.4", latest)

	path, err := f.Get(t.Context(), "1.2.4", "checksums.txt", 1024)
	require.NoError(t, err)
	require.Equal(t, dir, filepath.Dir(path))
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "sums\n", string(b))

	path, err = f.GetURL(t.Context(), "https://github.com/cloudflare/cloudflared/releases/download/2026.9.3/cloudflared-linux-amd64.deb", 1024)
	require.NoError(t, err)
	b, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "cloudflared package", string(b))
}

func TestTheFetcherRefusesWhatIsNoRelease(t *testing.T) {
	host := &releaseHost{files: map[string]string{
		"/repos/anaryk/proxmox-cloudflared-operator/releases/latest": `{"tag_name":"nightly"}`,
	}}
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	f, _ := fetcherFor(t, srv.URL)

	_, err := f.Latest(t.Context())
	require.ErrorContains(t, err, `the latest release of anaryk/proxmox-cloudflared-operator: "nightly" is no version of pco`)

	for _, name := range []string{"../checksums.txt", "a/b", ".hidden", ""} {
		_, err := f.Get(t.Context(), "1.2.4", name, 1024)
		require.ErrorContains(t, err, "is no file name of a release", name)
	}
	_, err = f.Get(t.Context(), "1.2.4/../..", "checksums.txt", 1024)
	require.ErrorContains(t, err, "is no version of pco")
	_, err = f.GetURL(t.Context(), "http://github.com/cloudflare/cloudflared/releases/download/2026.9.3/cloudflared-linux-amd64.deb", 1024)
	require.ErrorContains(t, err, "is not an https URL")
	require.Equal(t, []string{"/repos/anaryk/proxmox-cloudflared-operator/releases/latest"}, host.paths(),
		"nothing that is refused is asked for")
}

func TestAFileOverItsCapIsRefused(t *testing.T) {
	big := strings.Repeat("x", 2048)
	host := &releaseHost{files: map[string]string{
		"/anaryk/proxmox-cloudflared-operator/releases/download/v1.2.4/pco_1.2.4_amd64.deb": big,
	}}
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	f, dir := fetcherFor(t, srv.URL)

	_, err := f.Get(t.Context(), "1.2.4", "pco_1.2.4_amd64.deb", 1024)
	require.ErrorContains(t, err, "is larger than 1024 bytes")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "what was read of it is removed")

	path, err := f.Get(t.Context(), "1.2.4", "pco_1.2.4_amd64.deb", 2048)
	require.NoError(t, err, "a file of exactly the cap is taken")
	require.FileExists(t, path)
}

// A server that sends no length is cut off at the cap all the same.
func TestAFileWithoutALengthIsCutAtItsCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for range 4 {
			_, _ = w.Write([]byte(strings.Repeat("y", 512)))
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	f, _ := fetcherFor(t, srv.URL)

	_, err := f.Get(t.Context(), "1.2.4", "pco_1.2.4_amd64.deb", 1000)
	require.ErrorContains(t, err, "is larger than 1000 bytes")
}

func TestAnAnswerOtherThan200IsAnError(t *testing.T) {
	srv := httptest.NewServer(&releaseHost{})
	t.Cleanup(srv.Close)
	f, dir := fetcherFor(t, srv.URL)

	_, err := f.Get(t.Context(), "1.2.4", "checksums.txt", 1024)
	require.ErrorContains(t, err, "/anaryk/proxmox-cloudflared-operator/releases/download/v1.2.4/checksums.txt: 404 Not Found")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestARedirectToHTTPIsRefused(t *testing.T) {
	host := &releaseHost{files: map[string]string{"/elsewhere": "package"}}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".deb") {
			http.Redirect(w, r, srv.URL+"/elsewhere", http.StatusFound)
			return
		}
		host.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f, _ := fetcherFor(t, srv.URL)

	_, err := f.Get(t.Context(), "1.2.4", "pco_1.2.4_amd64.deb", 1024)

	require.ErrorContains(t, err, "redirected to "+srv.URL+"/elsewhere, which is not https")
	require.Empty(t, host.paths(), "the redirect is not followed")
}

func tlsFetcher(t *testing.T, srv *httptest.Server) Fetcher {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return NewFetcher(FetchConfig{Dir: t.TempDir(), Base: srv.URL, RootCAs: pool})
}

func TestARedirectToHTTPSIsFollowed(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			_, _ = w.Write([]byte("package"))
			return
		}
		http.Redirect(w, r, srv.URL+"/elsewhere", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	path, err := tlsFetcher(t, srv).Get(t.Context(), "1.2.4", "pco_1.2.4_amd64.deb", 1024)

	require.NoError(t, err)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "package", string(b))
}

func TestTLSBelow12IsRefused(t *testing.T) {
	for _, tt := range []struct {
		max uint16
		ok  bool
	}{
		{tls.VersionTLS11, false},
		{tls.VersionTLS12, true},
		{tls.VersionTLS13, true},
	} {
		t.Run(tls.VersionName(tt.max), func(t *testing.T) {
			srv := httptest.NewUnstartedServer(&releaseHost{files: map[string]string{
				"/anaryk/proxmox-cloudflared-operator/releases/download/v1.2.4/checksums.txt": "sums\n",
			}})
			srv.TLS = &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tt.max} //nolint:gosec // the server of the test offers what the client must refuse
			srv.Config.ErrorLog = log.New(io.Discard, "", 0)
			srv.StartTLS()
			t.Cleanup(srv.Close)

			_, err := tlsFetcher(t, srv).Get(t.Context(), "1.2.4", "checksums.txt", 1024)

			if tt.ok {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "protocol version")
			}
		})
	}
}

// A download that takes longer than its deadline is given up.
func TestADownloadHasADeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	f := NewFetcher(FetchConfig{Dir: t.TempDir(), Base: srv.URL, Timeout: time.Millisecond})

	_, err := f.Get(t.Context(), "1.2.4", "checksums.txt", 1024)

	require.ErrorContains(t, err, "context deadline exceeded")
}

func TestTheFetcherOfGitHubAsksGitHub(t *testing.T) {
	f := NewFetcher(FetchConfig{Dir: t.TempDir()}).(*httpFetcher)
	require.Equal(t, "https://api.github.com/repos/anaryk/proxmox-cloudflared-operator/releases/latest", f.latestURL())
	require.Equal(t, "https://github.com/anaryk/proxmox-cloudflared-operator/releases/download/v1.2.4/checksums.txt", f.releaseURL("1.2.4", "checksums.txt"))
	u, err := f.packageURL("https://github.com/cloudflare/cloudflared/releases/download/2026.9.3/cloudflared-linux-amd64.deb")
	require.NoError(t, err)
	require.Equal(t, "https://github.com/cloudflare/cloudflared/releases/download/2026.9.3/cloudflared-linux-amd64.deb", u)
	require.Equal(t, uint16(tls.VersionTLS12), f.client.Transport.(*http.Transport).TLSClientConfig.MinVersion)
	require.Equal(t, 5*time.Minute, f.timeout)
}
