package web

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// The headers of spec-ui 9.2, which every answer carries.
var everyAnswer = http.Header{
	"Content-Security-Policy": {"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
		"font-src 'self'; connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'self'; " +
		"frame-ancestors 'none'; object-src 'none'; require-trusted-types-for 'script'; trusted-types 'none'"},
	"X-Content-Type-Options":       {"nosniff"},
	"Referrer-Policy":              {"no-referrer"},
	"Cross-Origin-Opener-Policy":   {"same-origin"},
	"Cross-Origin-Resource-Policy": {"same-origin"},
	"Permissions-Policy":           {"camera=(), microphone=(), geolocation=(), usb=(), payment=(), clipboard-read=()"},
	"X-Frame-Options":              {"DENY"},
}

func with(extra http.Header) http.Header {
	h := everyAnswer.Clone()
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// stubMounter stands for the session and the gateway: a route of the API, and
// one that panics.
type stubMounter struct{}

func (stubMounter) Mount(r gin.IRouter) {
	r.GET("/api/stub", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	r.GET("/api/peer", func(c *gin.Context) { c.String(http.StatusOK, "%s", c.ClientIP()) })
	r.POST("/api/panic", func(*gin.Context) { panic("ticket PVE:alice@pve:SECRET") })
}

func TestHeaders(t *testing.T) {
	cases := []struct {
		name, path, acceptEncoding string
		status                     int
		want                       http.Header
	}{
		{"the page", "/", "gzip", http.StatusOK, with(http.Header{
			"Cache-Control":    {"no-cache"},
			"Content-Type":     {"text/html; charset=utf-8"},
			"Content-Encoding": {"gzip"},
			"Vary":             {"Accept-Encoding"},
		})},
		{"an asset", "/assets/x.js", "gzip", http.StatusOK, with(http.Header{
			"Cache-Control":    {"public, max-age=31536000, immutable"},
			"Content-Type":     {"text/javascript; charset=utf-8"},
			"Content-Encoding": {"gzip"},
			"Vary":             {"Accept-Encoding"},
		})},
		{"an asset to a client without gzip", "/assets/x.js", "", http.StatusOK, with(http.Header{
			"Cache-Control": {"public, max-age=31536000, immutable"},
			"Content-Type":  {"text/javascript; charset=utf-8"},
			"Vary":          {"Accept-Encoding"},
		})},
		{"a mounted route", "/api/stub", "gzip", http.StatusOK, with(http.Header{
			"Cache-Control": {"no-store"},
			"Content-Type":  {"application/json; charset=utf-8"},
		})},
		{"not found", "/nope", "gzip", http.StatusNotFound, with(http.Header{
			"Cache-Control": {"no-store"},
			"Content-Type":  {"text/plain; charset=utf-8"},
		})},
		{"an asset not found", "/assets/nope.js", "gzip", http.StatusNotFound, with(http.Header{
			"Cache-Control": {"no-store"},
			"Content-Type":  {"text/plain; charset=utf-8"},
		})},
	}
	s := newTestServer(t, Config{Assets: testAssets()}, stubMounter{})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := get(s, c.path, c.acceptEncoding)
			require.Equal(t, c.status, rec.Code)
			require.Equal(t, c.want, headersOf(t, rec))
		})
	}

	t.Run("a method not allowed", func(t *testing.T) {
		rec := do(s, request(http.MethodOptions, "/"))
		require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
		require.Equal(t, with(http.Header{
			"Allow":         {"GET, HEAD"},
			"Cache-Control": {"no-store"},
			"Content-Type":  {"text/plain; charset=utf-8"},
		}), headersOf(t, rec))
	})

	t.Run("a handler that panicked", func(t *testing.T) {
		rec := do(s, request(http.MethodPost, "/api/panic"))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.Equal(t, with(http.Header{
			"Cache-Control": {"no-store"},
			"Content-Type":  {"text/plain; charset=utf-8"},
		}), headersOf(t, rec))
	})
}

// headersOf returns the headers of an answer but its length, which, where it
// is set, is checked against the body: of a file, it is whatever the
// compressor makes of it.
func headersOf(t *testing.T, rec *httptest.ResponseRecorder) http.Header {
	t.Helper()
	h := rec.Header().Clone()
	if n := h.Get("Content-Length"); n != "" {
		require.Equal(t, strconv.Itoa(rec.Body.Len()), n)
	}
	h.Del("Content-Length")
	return h
}

func TestHSTSOnlyWhenAskedFor(t *testing.T) {
	paths := []string{"/", "/assets/x.js", "/api/stub", "/nope"}

	off := newTestServer(t, Config{Assets: testAssets()}, stubMounter{})
	for _, path := range paths {
		require.Empty(t, get(off, path, "").Header().Values("Strict-Transport-Security"), path)
	}

	on := newTestServer(t, Config{Assets: testAssets(), HSTS: true}, stubMounter{})
	for _, path := range paths {
		require.Equal(t, []string{"max-age=31536000"}, get(on, path, "").Header().Values("Strict-Transport-Security"), path)
	}
}
