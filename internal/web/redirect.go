package web

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/gin-gonic/gin"
)

// controllerPort is where the web interface of the controller's node
// listens, the same port on every node.
const controllerPort = "8643"

// RedirectToController answers a page asked for with GET or HEAD with 303
// to https://<addr>:8643<path> while the daemon says this node is not the
// controller and one is known. Only an address of another node is used
// (Redirectable). The calls of the page under /api are left alone: the
// daemon answers them for its own node, and the session of a browser is
// bound to the host name it signed in under, so a call sent elsewhere would
// carry nothing; a request with another method is served here, where the
// API refuses what it cannot answer. Without a known controller nothing is
// redirected, and the page is served here.
func RedirectToController(role func() (controller bool, addr string, ok bool)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if role == nil || (c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead) {
			return
		}
		path := c.Request.URL.EscapedPath()
		if path == "/api" || strings.HasPrefix(path, "/api/") || !strings.HasPrefix(path, "/") {
			return
		}
		controller, addr, ok := role()
		if controller || !ok {
			return
		}
		ip, err := netip.ParseAddr(addr)
		if err != nil || !Redirectable(ip) {
			return
		}
		target := "https://" + net.JoinHostPort(ip.String(), controllerPort) + path
		if q := c.Request.URL.RawQuery; q != "" {
			target += "?" + q
		}
		c.Header("Cache-Control", cacheNever)
		c.Redirect(http.StatusSeeOther, target)
		c.Abort()
	}
}

// Redirectable reports whether addr is one a browser may be sent to: an
// address of another node, never a loopback, unspecified, multicast or zoned
// one.
func Redirectable(addr netip.Addr) bool {
	return addr.IsValid() && addr.Zone() == "" && !addr.IsLoopback() && !addr.IsUnspecified() && !addr.IsMulticast()
}
