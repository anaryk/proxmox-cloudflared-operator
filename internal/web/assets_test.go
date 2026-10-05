package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

const indexPage = `<!doctype html><html><head><title>pco</title>` +
	`<script type="module" src="/assets/x.js"></script></head><body><div id="root"></div></body></html>`

// testAssets is a build as Vite makes it: index.html and licenses.txt at the
// top, everything else under assets/ with a hash in its name.
func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html":         {Data: []byte(indexPage)},
		"licenses.txt":       {Data: []byte(strings.Repeat("Name: react\nLicense: MIT\n\n", 20))},
		"assets/x.js":        {Data: []byte(strings.Repeat("export const a = () => 1;\n", 50))},
		"assets/x.css":       {Data: []byte(strings.Repeat(".a { color: #ea7317; }\n", 50))},
		"assets/icons.svg":   {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0h1v1z"/></svg>`)},
		"assets/data.json":   {Data: []byte(`{"a":[1,2,3],"b":"text"}`)},
		"assets/font.woff2":  {Data: []byte{0x77, 0x4f, 0x46, 0x32, 0x00, 0x01, 0x00, 0x00, 0x13, 0x37}},
		"assets/chunk/_m.js": {Data: []byte("export default 2;\n")},
		"favicon.ico":        {Data: []byte{0, 0, 1, 0}},
	}
}

func TestEveryTextAssetIsTheSameWithAndWithoutGzip(t *testing.T) {
	assets := testAssets()
	s := newTestServer(t, Config{Assets: assets})
	for _, name := range []string{"assets/x.js", "assets/x.css", "assets/icons.svg", "assets/data.json", "assets/chunk/_m.js", "licenses.txt"} {
		t.Run(name, func(t *testing.T) {
			plain := get(s, "/"+name, "")
			require.Equal(t, http.StatusOK, plain.Code)
			require.Empty(t, plain.Header().Get("Content-Encoding"))
			require.Equal(t, "Accept-Encoding", plain.Header().Get("Vary"))
			require.Equal(t, assets[name].Data, plain.Body.Bytes())

			zipped := get(s, "/"+name, "br, gzip, deflate")
			require.Equal(t, http.StatusOK, zipped.Code)
			require.Equal(t, "gzip", zipped.Header().Get("Content-Encoding"))
			require.Equal(t, "Accept-Encoding", zipped.Header().Get("Vary"))
			require.Equal(t, plain.Header().Get("Content-Type"), zipped.Header().Get("Content-Type"))
			require.Equal(t, assets[name].Data, gunzip(t, zipped.Body.Bytes()))
		})
	}

	t.Run("index.html", func(t *testing.T) {
		plain := get(s, "/", "")
		zipped := get(s, "/", "gzip")
		require.Equal(t, "gzip", zipped.Header().Get("Content-Encoding"))
		require.Equal(t, []byte(indexPage), plain.Body.Bytes())
		require.Equal(t, []byte(indexPage), gunzip(t, zipped.Body.Bytes()))
	})

	t.Run("a font is never gzipped", func(t *testing.T) {
		rec := get(s, "/assets/font.woff2", "gzip")
		require.Equal(t, http.StatusOK, rec.Code)
		require.Empty(t, rec.Header().Get("Content-Encoding"))
		require.Empty(t, rec.Header().Values("Vary"))
		require.Equal(t, "font/woff2", rec.Header().Get("Content-Type"))
		require.Equal(t, assets["assets/font.woff2"].Data, rec.Body.Bytes())
	})
}

func TestGzipIsTheSameEveryTime(t *testing.T) {
	in := []byte(strings.Repeat("export const a = () => 1;\n", 50))
	first, err := gzipped(in)
	require.NoError(t, err)
	second, err := gzipped(in)
	require.NoError(t, err)
	require.Equal(t, first, second)

	// And the same in two processes: nothing of the time or the file goes in.
	a := get(newTestServer(t, Config{Assets: testAssets()}), "/assets/x.js", "gzip")
	b := get(newTestServer(t, Config{Assets: testAssets()}), "/assets/x.js", "gzip")
	require.Equal(t, a.Body.Bytes(), b.Body.Bytes())
	require.Equal(t, first, a.Body.Bytes())
}

func TestGzipIsAsTightAsItGets(t *testing.T) {
	out, err := gzipped([]byte(strings.Repeat("export const a = () => 1;\n", 50)))
	require.NoError(t, err)
	// The extra flags of the header: 2 for the best compression, 4 for the
	// fastest, 0 for anything between.
	require.Equal(t, byte(2), out[8])
}

func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"gzip", true},
		{"GZip", true},
		{"x-gzip", true},
		{"gzip, deflate, br, zstd", true},
		{"deflate, gzip;q=0.5", true},
		{"gzip;q=0", false},
		{"gzip; q=0.000", false},
		{"identity", false},
		{"br", false},
		{"*", true},
		{"*;q=0", false},
		{"gzip;q=0, *", false},
		{"br, *;q=0.1", true},
		{"gzip;q=nonsense", false},
	}
	for _, c := range cases {
		t.Run(c.header, func(t *testing.T) {
			require.Equal(t, c.want, acceptsGzip(c.header))
		})
	}
}

func TestPagesOfTheInterfaceAnswerIndexHTML(t *testing.T) {
	s := newTestServer(t, Config{Assets: testAssets()})
	for _, path := range []string{
		"/",
		"/routes",
		"/routes/",
		"/routes/app.example.com",
		"/routes/plan",
		"/routes/manual/new",
		"/guests",
		"/guests/qemu/101",
		"/guests/claims",
		"/networks",
		"/edge",
		"/edge/credentials",
		"/edge/zones/example.com",
		"/edge/tunnels/0123456789abcdef0123456789abcdef",
		"/events",
		"/doctor",
		"/settings",
		"/setup",
		"/signin",
	} {
		t.Run(path, func(t *testing.T) {
			rec := get(s, path, "")
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, indexPage, rec.Body.String())
			require.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
			require.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
		})
	}
}

func TestEverythingElseIsNotFound(t *testing.T) {
	s := newTestServer(t, Config{Assets: testAssets()})
	for _, path := range []string{
		"/index.html",
		"/routesx",
		"/signing",
		"/edges",
		"/favicon.ico",
		"/assets",
		"/assets/",
		"/assets/missing.js",
		"/assets/../index.html",
		"/api/v1/state",
		"/api/session",
		"/nope",
	} {
		t.Run(path, func(t *testing.T) {
			rec := get(s, path, "gzip")
			require.Equal(t, http.StatusNotFound, rec.Code)
			require.Equal(t, "not found\n", rec.Body.String())
			require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
			require.Empty(t, rec.Header().Get("Content-Encoding"))
		})
	}
}

func TestHeadAndOtherMethods(t *testing.T) {
	s := newTestServer(t, Config{Assets: testAssets()})

	head := do(s, request(http.MethodHead, "/assets/x.js"))
	require.Equal(t, http.StatusOK, head.Code)
	require.Empty(t, head.Body.Bytes())
	require.Equal(t, "public, max-age=31536000, immutable", head.Header().Get("Cache-Control"))
	require.Equal(t, strconv.Itoa(len(testAssets()["assets/x.js"].Data)), head.Header().Get("Content-Length"))

	// It says as long as a GET would, with gzip too.
	zippedHead := request(http.MethodHead, "/assets/x.js")
	zippedHead.Header.Set("Accept-Encoding", "gzip")
	zipped := get(s, "/assets/x.js", "gzip")
	require.NotEmpty(t, zipped.Body.Bytes())
	require.Equal(t, strconv.Itoa(zipped.Body.Len()), do(s, zippedHead).Header().Get("Content-Length"))

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		for _, path := range []string{"/", "/routes/plan", "/assets/x.js", "/licenses.txt"} {
			rec := do(s, request(method, path))
			require.Equal(t, http.StatusMethodNotAllowed, rec.Code, "%s %s", method, path)
			require.Equal(t, "GET, HEAD", rec.Header().Get("Allow"), "%s %s", method, path)
			require.Empty(t, rec.Header().Values("Access-Control-Allow-Origin"))
		}
	}
}

func TestAssetsAreCachedAndThePageIsNot(t *testing.T) {
	s := newTestServer(t, Config{Assets: testAssets()})
	cases := map[string]string{
		"/assets/x.js":       "public, max-age=31536000, immutable",
		"/assets/font.woff2": "public, max-age=31536000, immutable",
		"/licenses.txt":      "no-cache",
		"/":                  "no-cache",
		"/settings":          "no-cache",
	}
	for path, want := range cases {
		require.Equal(t, want, get(s, path, "gzip").Header().Get("Cache-Control"), path)
	}
	require.Equal(t, "text/plain; charset=utf-8", get(s, "/licenses.txt", "").Header().Get("Content-Type"))
}

func TestContentTypes(t *testing.T) {
	cases := map[string]string{
		"index.html":          "text/html; charset=utf-8",
		"assets/x.js":         "text/javascript; charset=utf-8",
		"assets/x.mjs":        "text/javascript; charset=utf-8",
		"assets/x.css":        "text/css; charset=utf-8",
		"assets/x.svg":        "image/svg+xml",
		"assets/x.json":       "application/json",
		"licenses.txt":        "text/plain; charset=utf-8",
		"assets/x.woff2":      "font/woff2",
		"assets/x.woff":       "font/woff",
		"assets/x.png":        "image/png",
		"assets/x.webp":       "image/webp",
		"assets/x.ico":        "image/vnd.microsoft.icon",
		"assets/x.unknown":    "application/octet-stream",
		"assets/no-extension": "application/octet-stream",
	}
	for name, want := range cases {
		require.Equal(t, want, contentType(name), name)
	}
}

func TestNewRefusesABuildWithoutItsPage(t *testing.T) {
	_, err := New(Config{Assets: fstest.MapFS{"assets/x.js": {Data: []byte("1")}}})
	require.ErrorContains(t, err, "index.html")

	_, err = New(Config{})
	require.Error(t, err)
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	require.NoError(t, zr.Close())
	return out
}
