package docscheck

import (
	"go/ast"
	"go/token"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/auth"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/gateway"
)

// call is a method and a path of the gateway's table, the path under
// /api/v1.
type call struct{ method, path string }

func (c call) String() string { return c.method + " /api/v1" + c.path }

func tableCalls() []call {
	out := make([]call, 0, len(gateway.Table))
	for _, rule := range gateway.Table {
		out = append(out, call{rule.Method, rule.Path})
	}
	return out
}

var (
	callMethod = regexp.MustCompile(`^(GET|POST|PUT|DELETE) (/\S*)`)
	callPath   = regexp.MustCompile(`^(/\S*)`)
)

// table returns the table of text whose header row begins with header, up to
// the first line that is not a row: a row left below a paragraph is not in
// it, as a reader of the page does not see it there.
func table(t *testing.T, text, header string) string {
	t.Helper()
	var out []string
	for line := range strings.Lines(text) {
		line = strings.TrimRight(line, "\r\n")
		if out == nil && !strings.HasPrefix(line, header) {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		out = append(out, line)
	}
	require.NotEmpty(t, out, "there is no table with the header %q", header)
	return strings.Join(out, "\n")
}

// webCalls reads the calls of the table of "Through the web interface" in
// api.md, whose first cell names a method and a path and then more paths of
// the same method: `GET /api/v1/networks`, `/managed`.
func webCalls(t *testing.T) []call {
	t.Helper()
	var out []call
	for _, row := range firstCells(table(t, section(t, page(t, "api.md"), "Through the web interface"), "| Call | Socket |")) {
		method := ""
		for _, span := range row {
			path := ""
			if m := callMethod.FindStringSubmatch(span); m != nil {
				method, path = m[1], m[2]
			} else if m := callPath.FindStringSubmatch(span); m != nil && method != "" {
				path = "/api/v1" + m[1]
			}
			rest, ok := strings.CutPrefix(path, "/api/v1/")
			if !ok {
				continue
			}
			out = append(out, call{method, "/" + rest})
		}
	}
	return out
}

func TestEveryCallOfTheGatewayIsInTheAPIPage(t *testing.T) {
	documented := webCalls(t)
	for _, c := range tableCalls() {
		require.Contains(t, documented, c, "docs/api.md, Through the web interface, has no row for %s", c)
	}
}

func TestNoCallOfTheAPIPageTheGatewayLost(t *testing.T) {
	table := tableCalls()
	for _, c := range webCalls(t) {
		require.Contains(t, table, c, "docs/api.md lists %s, which the gateway does not pass on", c)
	}
}

// socketRoutes reads the first column of the table of "Routes" in api.md,
// `GET /v1/state`, with the last column, the role the web interface needs.
func socketRoutes(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for line := range strings.Lines(table(t, section(t, page(t, "api.md"), "Routes"), "| Route | Answers |")) {
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) != 4 {
			continue
		}
		route := strings.Trim(strings.TrimSpace(cells[0]), "`")
		if callMethod.MatchString(route) {
			out[route] = strings.TrimSpace(cells[3])
		}
	}
	return out
}

func TestEveryCallOfTheGatewayHasItsSocketRoute(t *testing.T) {
	routes := socketRoutes(t)
	for _, rule := range gateway.Table {
		method := rule.Method
		if rule.Upstream != "" {
			method = rule.Upstream
		}
		route := method + " /v1" + rule.Path
		web, ok := routes[route]
		require.True(t, ok, "docs/api.md, Routes, has no row for %s, which the gateway passes on", route)
		require.True(t, strings.HasPrefix(web, rule.Min.String()),
			"docs/api.md, Routes, says %q for %s, which the gateway passes on for the role %s", web, route, rule.Min)
	}
}

// daemonRoutes reads the routes of the socket from the source of
// internal/api: every call v1.GET("/path", ...) and its kin.
func daemonRoutes(t *testing.T) []string {
	t.Helper()
	files, _ := goFiles(t, "internal/api")
	var out []string
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok || len(c.Args) < 2 {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok || !callMethod.MatchString(sel.Sel.Name+" /") {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "v1" {
				return true
			}
			lit, ok := c.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			path, err := strconv.Unquote(lit.Value)
			require.NoError(t, err)
			out = append(out, sel.Sel.Name+" /v1"+path)
			return true
		})
	}
	require.NotEmpty(t, out, "no route found in internal/api")
	return out
}

func TestEveryRouteOfTheSocketIsInTheAPIPage(t *testing.T) {
	documented := socketRoutes(t)
	served := daemonRoutes(t)
	for _, route := range served {
		_, ok := documented[route]
		require.True(t, ok, "docs/api.md, Routes, has no row for %s, which the daemon serves", route)
	}
	for route := range documented {
		require.Contains(t, served, route, "docs/api.md, Routes, lists %s, which the daemon does not serve", route)
	}
}

func TestNoSocketRouteSaysTheGatewayPassesItOnWhenItDoesNot(t *testing.T) {
	passed := map[string]bool{}
	for _, rule := range gateway.Table {
		method := rule.Method
		if rule.Upstream != "" {
			method = rule.Upstream
		}
		passed[method+" /v1"+rule.Path] = true
	}
	for route, web := range socketRoutes(t) {
		if web == "not passed on" {
			require.False(t, passed[route], "docs/api.md, Routes, says the gateway does not pass on %s, and it does", route)
			continue
		}
		require.True(t, passed[route], "docs/api.md, Routes, says %q for %s, which the gateway does not pass on", web, route)
	}
}

// noAction are the calls an admin may make through the gateway that no page
// of the web interface makes yet, and so have no action in web-ui.md.
var noAction = []call{
	{http.MethodGet, "/networks/candidates"},
	{http.MethodGet, "/networks/:id"},
	{http.MethodPost, "/networks/probe"},
	{http.MethodPost, "/networks"},
	{http.MethodDelete, "/networks/:id"},
	{http.MethodPost, "/managed/approve"},
	{http.MethodPost, "/managed/revoke"},
	{http.MethodPost, "/managed/enable"},
	{http.MethodGet, "/cluster"},
	{http.MethodPost, "/cluster/handoff"},
	{http.MethodPost, "/cluster/join-tokens"},
	{http.MethodPost, "/cluster/unblock"},
	{http.MethodGet, "/nodes"},
	{http.MethodPost, "/nodes/:name/admit"},
	{http.MethodPost, "/nodes/:name/drain"},
	{http.MethodPost, "/nodes/:name/undrain"},
	{http.MethodPost, "/nodes/:name/forget"},
	{http.MethodPut, "/nodes/:name"},
	{http.MethodGet, "/local/agent"},
}

func TestEveryAdminCallHasItsActionInTheWebPage(t *testing.T) {
	web := page(t, "web-ui.md")
	for _, rule := range gateway.Table {
		if rule.Min < auth.RoleAdmin {
			continue
		}
		c := call{rule.Method, rule.Path}
		named := strings.Contains(web, "`"+c.String()+"`")
		if slices.Contains(noAction, c) {
			require.False(t, named, "docs/web-ui.md names %s, which noAction says no page makes: take it off the list", c)
			continue
		}
		require.True(t, named, "docs/web-ui.md has no action that names %s, an admin's call of the gateway", c)
	}
}
