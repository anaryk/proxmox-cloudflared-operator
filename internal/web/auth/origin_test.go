package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/wire"
)

func TestAllowedHosts(t *testing.T) {
	cases := []struct {
		name   string
		listen string
		names  []string
		want   []string
	}{
		{"the default port", "192.0.2.10:8643", []string{"pve1", "pve1.example.lan"}, []string{
			"192.0.2.10:8643", "pve1.example.lan:8643", "pve1:8643",
		}},
		{"another listen port", "192.0.2.10:9443", []string{"pve1"}, []string{
			"192.0.2.10:8643", "192.0.2.10:9443", "pve1:8643", "pve1:9443",
		}},
		{"an unspecified listen address", "0.0.0.0:8643", []string{"PVE1.Example.LAN", " pco.example.com "}, []string{
			"pco.example.com:8643", "pve1.example.lan:8643",
		}},
		{"IPv6", "[2001:db8::10]:8643", []string{"2001:db8::11", "[2001:db8::12]"}, []string{
			"[2001:db8::10]:8643", "[2001:db8::11]:8643", "[2001:db8::12]:8643",
		}},
		{"a name given with its port", "127.0.0.1:8643", []string{"pco.example.com:9443", ""}, []string{
			"127.0.0.1:8643", "pco.example.com:9443",
		}},
		{"port 443, which browsers leave out", "192.0.2.10:443", []string{"pve1", "2001:db8::11"}, []string{
			"192.0.2.10", "192.0.2.10:443", "192.0.2.10:8643",
			"[2001:db8::11]", "[2001:db8::11]:443", "[2001:db8::11]:8643",
			"pve1", "pve1:443", "pve1:8643",
		}},
		{"a name given with port 443", "127.0.0.1:8643", []string{"pco.example.com:443", "[2001:db8::12]:443"}, []string{
			"127.0.0.1:8643", "[2001:db8::12]", "[2001:db8::12]:443", "pco.example.com", "pco.example.com:443",
		}},
		{"the same name twice", "127.0.0.1:8643", []string{"pve1", "pve1"}, []string{"127.0.0.1:8643", "pve1:8643"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, AllowedHosts(c.listen, c.names...))
		})
	}
}

const (
	testHost   = "pve1.example.lan:8643"
	testOrigin = "https://pve1.example.lan:8643"
)

func TestAdmit(t *testing.T) {
	a := New(staticPVE{}, Config{Hosts: func() []string { return []string{testHost, "192.0.2.10:8643"} }})
	type req struct {
		method      string
		host        string
		contentType string
		origin      []string
		fetchSite   string
	}
	good := req{http.MethodPost, testHost, "application/json", []string{testOrigin}, "same-origin"}
	with := func(f func(*req)) req { r := good; f(&r); return r }
	cases := []struct {
		name   string
		req    req
		status int
		code   string
	}{
		{"a POST of the page", good, 0, ""},
		{"JSON with a charset", with(func(r *req) { r.contentType = "application/json; charset=utf-8" }), 0, ""},
		{"no Sec-Fetch-Site, as from an older browser", with(func(r *req) { r.fetchSite = "" }), 0, ""},
		{"a name in another case", with(func(r *req) { r.host, r.origin = "PVE1.example.lan:8643", []string{"https://pve1.example.lan:8643"} }), 0, ""},
		{"by address", with(func(r *req) { r.host, r.origin = "192.0.2.10:8643", []string{"https://192.0.2.10:8643"} }), 0, ""},
		{"a GET from Proxmox VE's page", req{http.MethodGet, testHost, "", nil, "same-site"}, 0, ""},
		{"a GET from a bookmark", req{http.MethodGet, testHost, "", nil, "none"}, 0, ""},
		{"a HEAD from another site", req{http.MethodHead, testHost, "", []string{"https://evil.example"}, "cross-site"}, 0, ""},

		{"a form post", with(func(r *req) { r.contentType = "application/x-www-form-urlencoded" }), 415, wire.CodeUnsupportedMediaType},
		{"plain text", with(func(r *req) { r.contentType = "text/plain" }), 415, wire.CodeUnsupportedMediaType},
		{"no content type", with(func(r *req) { r.contentType = "" }), 415, wire.CodeUnsupportedMediaType},
		{"a DELETE without a content type", with(func(r *req) { r.method, r.contentType = http.MethodDelete, "" }), 415, wire.CodeUnsupportedMediaType},
		{"415 before the host is looked at", with(func(r *req) { r.contentType, r.host = "text/plain", "evil.example" }), 415, wire.CodeUnsupportedMediaType},
		{"a JSON look-alike", with(func(r *req) { r.contentType = "application/jsonx" }), 415, wire.CodeUnsupportedMediaType},

		{"an unknown host", with(func(r *req) { r.host, r.origin = "evil.example:8643", []string{"https://evil.example:8643"} }), 403, wire.CodeForbidden},
		{"an unknown host on a GET", req{http.MethodGet, "rebound.example:8643", "", nil, ""}, 403, wire.CodeForbidden},
		{"a known name on another port", req{http.MethodGet, "pve1.example.lan:8006", "", nil, ""}, 403, wire.CodeForbidden},
		{"no Origin", with(func(r *req) { r.origin = nil }), 403, wire.CodeForbidden},
		{"a foreign Origin", with(func(r *req) { r.origin = []string{"https://evil.example"} }), 403, wire.CodeForbidden},
		{"an Origin of plain HTTP", with(func(r *req) { r.origin = []string{"http://pve1.example.lan:8643"} }), 403, wire.CodeForbidden},
		{"the Origin of Proxmox VE", with(func(r *req) { r.origin = []string{"https://pve1.example.lan:8006"} }), 403, wire.CodeForbidden},
		{"the Origin with more after the port", with(func(r *req) { r.origin = []string{testOrigin + ".evil"} }), 403, wire.CodeForbidden},
		{"the Origin with a path", with(func(r *req) { r.origin = []string{testOrigin + "/"} }), 403, wire.CodeForbidden},
		{"the null Origin", with(func(r *req) { r.origin = []string{"null"} }), 403, wire.CodeForbidden},
		{"two Origins", with(func(r *req) { r.origin = []string{testOrigin, testOrigin} }), 403, wire.CodeForbidden},
		{"same-site", with(func(r *req) { r.fetchSite = "same-site" }), 403, wire.CodeForbidden},
		{"cross-site", with(func(r *req) { r.fetchSite = "cross-site" }), 403, wire.CodeForbidden},
		{"none on a POST", with(func(r *req) { r.fetchSite = "none" }), 403, wire.CodeForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.req.method, "/api/session/ticket", nil)
			r.Host = c.req.host
			if c.req.contentType != "" {
				r.Header.Set("Content-Type", c.req.contentType)
			}
			for _, o := range c.req.origin {
				r.Header.Add("Origin", o)
			}
			if c.req.fetchSite != "" {
				r.Header.Set("Sec-Fetch-Site", c.req.fetchSite)
			}
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = r
			ok := a.admit(ctx)
			if c.status == 0 {
				require.True(t, ok)
				require.False(t, ctx.IsAborted())
				return
			}
			require.False(t, ok)
			require.True(t, ctx.IsAborted())
			require.Equal(t, c.status, rec.Code)
			var body wire.Error
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Equal(t, c.code, body.Code)
			require.NotEmpty(t, body.Error)
			require.NotContains(t, rec.Body.String(), "evil", "the answer does not repeat what the request claimed")
		})
	}
}

func TestAdmitOnPort443(t *testing.T) {
	a := New(staticPVE{}, Config{Hosts: func() []string { return AllowedHosts("192.0.2.10:443", "pve1.example.lan") }})
	admit := func(host, origin string) int {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/session/ticket", nil)
		ctx.Request.Host = host
		ctx.Request.Header.Set("Content-Type", "application/json")
		ctx.Request.Header.Set("Origin", origin)
		ctx.Request.Header.Set("Sec-Fetch-Site", "same-origin")
		if !a.admit(ctx) {
			return rec.Code
		}
		return 0
	}
	require.Zero(t, admit("pve1.example.lan", "https://pve1.example.lan"), "the Host and Origin a browser sends for port 443")
	require.Zero(t, admit("192.0.2.10", "https://192.0.2.10"))
	require.Zero(t, admit("pve1.example.lan:443", "https://pve1.example.lan:443"))
	require.Equal(t, http.StatusForbidden, admit("pve1.example.lan", "https://pve1.example.lan:443"))
	require.Equal(t, http.StatusForbidden, admit("pve1.example.lan", "https://pve1.example.lan.evil"))

	a = New(staticPVE{}, Config{Hosts: func() []string { return AllowedHosts("192.0.2.10:8643", "pve1.example.lan") }})
	require.Equal(t, http.StatusForbidden, admit("pve1.example.lan", "https://pve1.example.lan"), "only when pco listens on 443")
}

func TestNoHostIsAllowedWithoutAList(t *testing.T) {
	a := New(staticPVE{}, Config{})
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/session", nil)
	ctx.Request.Host = testHost
	require.False(t, a.admit(ctx))
	require.Equal(t, http.StatusForbidden, rec.Code)
}
