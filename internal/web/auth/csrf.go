package auth

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/wire"
)

// secret is 256 random bits as 43 characters of unpadded base64url: a
// session id or a CSRF token.
func secret(rand io.Reader) (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand, b); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// csrfOK compares the request's Pco-Csrf with the session's in constant time.
func csrfOK(c *gin.Context, s Session) bool {
	got := c.Request.Header.Values(headerCSRF)
	return len(got) == 1 && subtle.ConstantTimeCompare([]byte(got[0]), []byte(s.CSRF)) == 1
}

// Require admits a request of a session with at least the role min. Every
// request needs a known Host; one that is not GET or HEAD also JSON, the
// page's own Origin, no Sec-Fetch-Site but same-origin, and the session's
// Pco-Csrf, all of them checked before Proxmox VE is asked about the session.
func (a *Auth) Require(min Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !a.admit(c) {
			return
		}
		s, ok := a.session(c)
		if !ok {
			return
		}
		if mutating(c.Request) && !csrfOK(c, s) {
			refuse(c, http.StatusForbidden, wire.Error{Error: "the request does not carry the token of this session", Code: wire.CodeForbidden})
			return
		}
		if s, ok = a.check(c, s); !ok {
			return
		}
		if s.Principal.Role < min {
			refuse(c, http.StatusForbidden, wire.Error{Error: "your role cannot do this: it needs Sys.Modify on /", Code: wire.CodeForbidden, Missing: "Sys.Modify"})
			return
		}
		s = a.seen(c, s)
		c.Set(sessionKey, &s)
		c.Next()
	}
}

const sessionKey = "pco.session"

// SessionOf is the session Require admitted the request with; nil on a route
// without it.
func SessionOf(c *gin.Context) *Session {
	v, ok := c.Get(sessionKey)
	if !ok {
		return nil
	}
	s, _ := v.(*Session)
	return s
}
