package web

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// asset is a file of the build as it is answered.
type asset struct {
	body         []byte
	gzipped      []byte // nil for a file that is not text
	contentType  string
	cacheControl string
}

// assets is the build, read and compressed once at start.
type assets struct {
	index asset
	files map[string]asset // by the path of the URL
}

// loadAssets reads the build: index.html and licenses.txt at its top and the
// files under assets/. Nothing else of it is served.
func loadAssets(fsys fs.FS) (*assets, error) {
	a := &assets{files: map[string]asset{}}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		cache := cacheRevalidate
		switch {
		case name == "index.html" || name == "licenses.txt":
		case strings.HasPrefix(name, "assets/"):
			cache = cacheForever
		default:
			return nil
		}
		f, err := readAsset(fsys, name, cache)
		if err != nil {
			return err
		}
		if name == "index.html" {
			a.index = f
		} else {
			a.files["/"+name] = f
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading the interface: %w", err)
	}
	if a.index.body == nil {
		return nil, errors.New("the interface has no index.html")
	}
	return a, nil
}

func readAsset(fsys fs.FS, name, cache string) (asset, error) {
	body, err := fs.ReadFile(fsys, name)
	if err != nil {
		return asset{}, err
	}
	f := asset{body: body, contentType: contentType(name), cacheControl: cache}
	if compressible(name) {
		if f.gzipped, err = gzipped(body); err != nil {
			return asset{}, fmt.Errorf("compressing %s: %w", name, err)
		}
	}
	return f, nil
}

// mount adds the routes of the build: the files, and index.html for every
// page of the interface (spec-ui 3.2).
func (a *assets) mount(r gin.IRouter) {
	get := func(p string, h gin.HandlerFunc) {
		r.GET(p, h)
		r.HEAD(p, h)
	}
	file := func(c *gin.Context) {
		if f, ok := a.files[c.Request.URL.Path]; ok {
			serveAsset(c, f)
			return
		}
		notFound(c)
	}
	page := func(c *gin.Context) { serveAsset(c, a.index) }

	get("/assets/*name", file)
	get("/licenses.txt", file)
	get("/", page)
	for _, p := range []string{"/routes", "/guests", "/edge", "/networks", "/events", "/doctor", "/settings", "/setup", "/signin"} {
		get(p, page)
		get(p+"/*rest", page)
	}
}

func serveAsset(c *gin.Context, f asset) {
	h := c.Writer.Header()
	h.Set("Content-Type", f.contentType)
	h.Set("Cache-Control", f.cacheControl)
	body := f.body
	if f.gzipped != nil {
		h.Set("Vary", "Accept-Encoding")
		if acceptsGzip(strings.Join(c.Request.Header.Values("Accept-Encoding"), ",")) {
			h.Set("Content-Encoding", "gzip")
			body = f.gzipped
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	c.Status(http.StatusOK)
	if c.Request.Method != http.MethodHead {
		_, _ = c.Writer.Write(body)
	}
}

// gzipped compresses b as tightly as compress/gzip can. The gzip header
// carries no time and no name, so the same input gives the same bytes on
// every start.
func gzipped(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func compressible(name string) bool {
	switch path.Ext(name) {
	case ".js", ".css", ".svg", ".json", ".txt":
		return true
	}
	return name == "index.html"
}

// contentType is fixed here rather than taken from the mime types of the
// machine, which differ between hosts: with nosniff, a script answered with
// another type does not run.
func contentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/vnd.microsoft.icon"
	}
	return "application/octet-stream"
}

// acceptsGzip says whether an Accept-Encoding header takes gzip: by its name
// or by *, with a weight above zero. A weight of zero for gzip refuses it
// whatever * says.
func acceptsGzip(header string) bool {
	star := false
	for part := range strings.SplitSeq(header, ",") {
		name, params, _ := strings.Cut(part, ";")
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "gzip", "x-gzip":
			return weighted(params)
		case "*":
			star = weighted(params)
		}
	}
	return star
}

// weighted says whether the parameters of an encoding leave it a weight above
// zero; without a q it has 1.
func weighted(params string) bool {
	for p := range strings.SplitSeq(params, ";") {
		k, v, ok := strings.Cut(p, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
			q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			return err == nil && q > 0
		}
	}
	return true
}
