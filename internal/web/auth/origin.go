package auth

import (
	"mime"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/wire"
)

// The headers between the page and the web process.
const (
	headerCSRF       = "Pco-Csrf"
	headerBackground = "Pco-Background"
)

// StreamPath is where the gateway serves the stream, which is no activity of
// the user: an open tab nobody looks at idles out.
const StreamPath = "/api/v1/stream"

// DefaultPort is the port of pco web; a name is accepted with it and with the
// port it listens on.
const DefaultPort = "8643"

// AllowedHosts are the Host headers the interface answers to: each name, and
// the address listen is on unless it is 0.0.0.0 or ::, with port 8643 and
// with the port of listen. A name that comes with a port keeps only that one.
func AllowedHosts(listen string, names ...string) []string {
	ports := []string{DefaultPort}
	names = slices.Clone(names)
	host, port, err := net.SplitHostPort(listen)
	if err == nil {
		if port != DefaultPort {
			ports = append(ports, port)
		}
		if addr, err := netip.ParseAddr(host); err != nil || !addr.IsUnspecified() {
			names = append(names, host)
		}
	}
	var out []string
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		if h, p, err := net.SplitHostPort(n); err == nil {
			out = append(out, net.JoinHostPort(h, p))
			continue
		}
		n = strings.TrimSuffix(strings.TrimPrefix(n, "["), "]")
		for _, p := range ports {
			out = append(out, net.JoinHostPort(n, p))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func mutating(r *http.Request) bool {
	return r.Method != http.MethodGet && r.Method != http.MethodHead
}

// admit checks what a request must carry before its session is looked at.
// A request that changes something must be JSON: a form of another site can
// send only a simple content type, and is refused here before anything else.
// Then the Host must be one of the names pco web answers to, which also
// stops a DNS rebinding; and the request must come from pco's own page: its
// Origin is https:// and the Host, and Sec-Fetch-Site, which browsers send,
// says same-origin. A GET or HEAD is a navigation too, same-site from the
// page of Proxmox VE and none from a bookmark, so only its Host is checked.
func (a *Auth) admit(c *gin.Context) bool {
	r := c.Request
	if mutating(r) {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			refuse(c, http.StatusUnsupportedMediaType, wire.Error{Error: "the request is not JSON", Code: wire.CodeUnsupportedMediaType})
			return false
		}
	}
	if !a.knownHost(r.Host) {
		refuse(c, http.StatusForbidden, wire.Error{
			Error: "pco does not answer to the name in the address bar; open it under the node's name or add the name to PCO_WEB_HOSTS",
			Code:  wire.CodeForbidden,
		})
		return false
	}
	if !mutating(r) {
		return true
	}
	origin := r.Header.Values("Origin")
	site := r.Header.Get("Sec-Fetch-Site")
	if len(origin) != 1 || !strings.EqualFold(origin[0], "https://"+r.Host) || (site != "" && site != "same-origin") {
		refuse(c, http.StatusForbidden, wire.Error{Error: "the request does not come from pco's own page", Code: wire.CodeForbidden})
		return false
	}
	return true
}

func (a *Auth) knownHost(host string) bool {
	if a.cfg.Hosts == nil || host == "" {
		return false
	}
	return slices.Contains(a.cfg.Hosts(), strings.ToLower(host))
}
