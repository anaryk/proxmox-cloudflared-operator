package web

import "github.com/gin-gonic/gin"

// contentSecurityPolicy lets the page load only its own files, run no inline
// script or style, and turn no string into HTML (spec-ui 9.2).
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"font-src 'self'; connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'self'; " +
	"frame-ancestors 'none'; object-src 'none'; require-trusted-types-for 'script'; trusted-types 'none'"

const (
	cacheNever      = "no-store"
	cacheRevalidate = "no-cache"
	// The name of a file under /assets/ carries the hash of its content.
	cacheForever = "public, max-age=31536000, immutable"
)

// securityHeaders sets the headers every answer carries, errors and 404s
// included. Nothing may be cached unless a handler says otherwise, which only
// the assets do.
//
// HSTS is sent only when asked for: it holds for the host name, not the port,
// so a browser would use HTTPS for every port of the node's name for a year,
// and its plain-HTTP services would stop working.
func securityHeaders(hsts bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), payment=(), clipboard-read=()")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", cacheNever)
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		c.Next()
	}
}
