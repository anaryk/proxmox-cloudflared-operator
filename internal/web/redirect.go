package web

import (
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// controllerPort is where the web interface of the controller's node
// listens, the same port on every node.
const controllerPort = "8643"

// RedirectToController answers every page with 307 to https://<addr>:8643<path>
// while the daemon says this node is not the controller and one is known.
// The calls of the page under /api are left alone: the daemon answers them
// for its own node, and the session of a browser is bound to the host name
// it signed in under, so a call sent elsewhere would carry nothing. Without
// a known controller nothing is redirected, and the page is served here.
func RedirectToController(role func() (controller bool, addr string, ok bool)) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if role == nil || path == "/api" || strings.HasPrefix(path, "/api/") || !strings.HasPrefix(path, "/") {
			return
		}
		controller, addr, ok := role()
		if controller || !ok || addr == "" {
			return
		}
		target := "https://" + withPort(addr) + path
		if q := c.Request.URL.RawQuery; q != "" {
			target += "?" + q
		}
		c.Header("Cache-Control", cacheNever)
		c.Redirect(http.StatusTemporaryRedirect, target)
		c.Abort()
	}
}

// withPort is addr with the port of the web interface, brackets around an
// IPv6 address; an addr that names a port already is kept.
func withPort(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, controllerPort)
}
