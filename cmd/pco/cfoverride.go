package main

import (
	"errors"
	"net"
	"net/netip"
	"net/url"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

const cloudflareURLVar = "PCO_CLOUDFLARE_API_URL"

// errOverride does not quote the value, which may carry a secret.
var errOverride = errors.New(cloudflareURLVar + " must be an http or https URL of a loopback address or localhost, " +
	"without credentials, query or fragment, such as http://127.0.0.1:8787/client/v4; it is for tests only")

// cloudflareOverride returns the base URL of the Cloudflare API that
// PCO_CLOUDFLARE_API_URL points the daemon, setup and uninstall at, or "" when
// it is not set. It is there to run them against a fake on this machine, and
// nothing that leaves it is accepted.
func (a *app) cloudflareOverride() (string, error) {
	raw := a.getenv(cloudflareURLVar)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errOverride
	}
	host := u.Hostname()
	if addr, err := netip.ParseAddr(host); err == nil && addr.Unmap().IsLoopback() {
		return u.String(), nil
	}
	if !isLocalhost(host) {
		return "", errOverride
	}
	if u.Scheme == "http" {
		// The client sends plain http only to an address, never to a name
		// that the resolver could send elsewhere.
		port := u.Port()
		u.Host = "127.0.0.1"
		if port != "" {
			u.Host = net.JoinHostPort(u.Host, port)
		}
	}
	return u.String(), nil
}

// isLocalhost compares in ASCII only: strings.EqualFold also takes letters
// such as the long s for the s of the name.
func isLocalhost(host string) bool {
	const name = "localhost"
	if len(host) != len(name) {
		return false
	}
	for i := range len(name) {
		if host[i]|0x20 != name[i] {
			return false
		}
	}
	return true
}

// overrideLine says that the Cloudflare API is overridden: a warning of the
// daemon at its start and a problem of every cycle, and a warning of setup and
// uninstall.
func overrideLine(base string) string {
	return "the Cloudflare API is overridden to " + base + " (" + cloudflareURLVar + "); this is for tests only"
}

// cloudflareClients builds the clients of setup and uninstall: for the
// Cloudflare API, or for base when it is not empty.
func cloudflareClients(base string) func(token string) (cfapi.API, error) {
	return func(token string) (cfapi.API, error) {
		return cfapi.New(cfapi.Options{BaseURL: base, Token: token})
	}
}
