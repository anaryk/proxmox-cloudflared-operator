package upgrade

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"
)

const (
	// Repository is where pco is released.
	Repository = "anaryk/proxmox-cloudflared-operator"

	githubAPI = "https://api.github.com"
	githubWeb = "https://github.com"

	// MaxSmallFile caps checksums.txt, its signature, the manifest and the
	// answer of GitHub about the latest release.
	MaxSmallFile = 1 << 20
	// MaxPackage caps a .deb, as the installer does.
	MaxPackage = 200 << 20

	fetchTimeout = 5 * time.Minute
	maxRedirects = 10

	// fetchPrefix starts the name of a file being downloaded into the work
	// directory.
	fetchPrefix = ".fetch-"
)

// fileNameRe is a file of a release: a plain name, never a path.
var fileNameRe = regexp.MustCompile(`^[A-Za-z0-9_+~-][A-Za-z0-9._+~-]*$`)

// Fetcher downloads release files with net/http: TLS 1.2 or later, redirects
// followed only to https, a size cap per file, a 5 minute deadline.
type Fetcher interface {
	// Latest is the version of the latest release of pco, without its v.
	Latest(ctx context.Context) (version string, err error)
	// Get downloads a file of the release of version and returns where it
	// put it; the caller removes it.
	Get(ctx context.Context, version, name string, max int64) (path string, err error)
	// GetURL downloads a package the manifest of cloudflared names.
	GetURL(ctx context.Context, rawURL string, max int64) (path string, err error)
}

// FetchConfig is what NewFetcher works with.
type FetchConfig struct {
	Dir     string         // where files are downloaded to
	Base    string         // the overridden release host, see Overrides; empty for GitHub
	RootCAs *x509.CertPool // the roots TLS trusts; nil for those of the system
	Timeout time.Duration  // for one file; 5 minutes when zero
}

// NewFetcher returns the Fetcher of pco upgrade.
func NewFetcher(c FetchConfig) Fetcher {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: c.RootCAs}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = fetchTimeout
	}
	return &httpFetcher{
		dir:     c.Dir,
		base:    c.Base,
		timeout: timeout,
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: httpsOnly,
		},
	}
}

// httpsOnly follows a redirect only to https: a release host that sends a
// download elsewhere sends it to TLS.
func httpsOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("more than %d redirects", maxRedirects)
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("redirected to %s, which is not https", req.URL.Redacted())
	}
	return nil
}

type httpFetcher struct {
	client  *http.Client
	dir     string
	base    string
	timeout time.Duration
}

func (f *httpFetcher) latestURL() string {
	if f.base != "" {
		return f.base + "/repos/" + Repository + "/releases/latest"
	}
	return githubAPI + "/repos/" + Repository + "/releases/latest"
}

func (f *httpFetcher) releaseURL(version, name string) string {
	host := githubWeb
	if f.base != "" {
		host = f.base
	}
	return host + "/" + Repository + "/releases/download/v" + version + "/" + name
}

// packageURL is the URL of a package of cloudflared, at the release host of
// the override when there is one.
func (f *httpFetcher) packageURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("%q is not an https URL", shorten(raw))
	}
	if f.base != "" {
		return f.base + u.EscapedPath(), nil
	}
	return u.String(), nil
}

func (f *httpFetcher) Latest(ctx context.Context) (string, error) {
	path, err := f.download(ctx, f.latestURL(), MaxSmallFile)
	if err != nil {
		return "", fmt.Errorf("asking for the latest release of %s: %w", Repository, err)
	}
	defer func() { _ = os.Remove(path) }()
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var release struct {
		Tag string `json:"tag_name"`
	}
	if err := json.Unmarshal(b, &release); err != nil {
		return "", fmt.Errorf("the latest release of %s: the answer is not the JSON of a release", Repository)
	}
	version, err := cleanRelease(release.Tag)
	if err != nil {
		return "", fmt.Errorf("the latest release of %s: %w", Repository, err)
	}
	return version, nil
}

func (f *httpFetcher) Get(ctx context.Context, version, name string, max int64) (string, error) {
	if _, err := cleanRelease(version); err != nil {
		return "", err
	}
	if !fileNameRe.MatchString(name) {
		return "", fmt.Errorf("%q is no file name of a release", shorten(name))
	}
	return f.download(ctx, f.releaseURL(version, name), max)
}

func (f *httpFetcher) GetURL(ctx context.Context, rawURL string, max int64) (string, error) {
	u, err := f.packageURL(rawURL)
	if err != nil {
		return "", err
	}
	return f.download(ctx, u, max)
}

// download writes what u answers to a new file of the directory, of at most
// max bytes, and returns its path. A failed download leaves nothing.
func (f *httpFetcher) download(ctx context.Context, u string, max int64) (path string, err error) {
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	if resp.ContentLength > max {
		return "", fmt.Errorf("%s is larger than %d bytes", u, max)
	}
	file, err := os.CreateTemp(f.dir, fetchPrefix+"*")
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := file.Close(); err == nil && cerr != nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(file.Name())
		}
	}()
	n, err := io.Copy(file, io.LimitReader(resp.Body, max+1))
	switch {
	case err != nil:
		return "", fmt.Errorf("GET %s: %w", u, err)
	case n > max:
		return "", fmt.Errorf("%s is larger than %d bytes", u, max)
	}
	return file.Name(), nil
}
