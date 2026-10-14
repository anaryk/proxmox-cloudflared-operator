package web

import (
	"net/http"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// roleOf is what the daemon says about the node, as the page's process
// reads it.
func roleOf(controller bool, addr string, ok bool) func() (bool, string, bool) {
	return func() (bool, string, bool) { return controller, addr, ok }
}

func redirectingServer(t *testing.T, role func() (bool, string, bool)) *Server {
	t.Helper()
	return newTestServer(t, Config{Assets: testAssets(), Log: zerolog.Nop(), Role: role}, stubMounter{})
}

func TestEveryPageGoesToTheController(t *testing.T) {
	s := redirectingServer(t, roleOf(false, "10.9.0.2", true))
	for _, tt := range []struct{ path, location string }{
		{"/", "https://10.9.0.2:8643/"},
		{"/routes", "https://10.9.0.2:8643/routes"},
		{"/routes/app.example.com?owner=qemu%2F101&tab=detail", "https://10.9.0.2:8643/routes/app.example.com?owner=qemu%2F101&tab=detail"},
		{"/routes/a%2Fb%20c", "https://10.9.0.2:8643/routes/a%2Fb%20c"},
		{"/signin?next=%2Fsettings", "https://10.9.0.2:8643/signin?next=%2Fsettings"},
		{"/assets/x.js", "https://10.9.0.2:8643/assets/x.js"},
		{"/nope", "https://10.9.0.2:8643/nope"},
	} {
		rec := get(s, tt.path, "gzip")
		require.Equal(t, http.StatusSeeOther, rec.Code, tt.path)
		require.Equal(t, tt.location, rec.Header().Get("Location"), tt.path)
		require.Empty(t, rec.Header().Values("Set-Cookie"), tt.path)
		require.Equal(t, "no-store", rec.Header().Get("Cache-Control"), tt.path)
		require.Empty(t, rec.Header().Get("Content-Encoding"), tt.path)
	}
	rec := do(s, request(http.MethodHead, "/routes"))
	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "https://10.9.0.2:8643/routes", rec.Header().Get("Location"))
}

func TestAnIPv6ControllerIsBracketed(t *testing.T) {
	s := redirectingServer(t, roleOf(false, "fd00::2", true))
	rec := get(s, "/doctor", "")
	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "https://[fd00::2]:8643/doctor", rec.Header().Get("Location"))
}

func TestOnlyAGetOrAHeadIsRedirected(t *testing.T) {
	s := redirectingServer(t, roleOf(false, "10.9.0.2", true))
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodPatch} {
		rec := do(s, request(method, "/routes"))
		require.NotEqual(t, http.StatusSeeOther, rec.Code, method)
		require.Empty(t, rec.Header().Get("Location"), method)
	}
}

func TestTheCallsOfThePageAreNotRedirected(t *testing.T) {
	s := redirectingServer(t, roleOf(false, "10.9.0.2", true))
	rec := get(s, "/api/stub", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = get(s, "/api/session", "")
	require.NotEqual(t, http.StatusSeeOther, rec.Code)
	rec = get(s, "/api", "")
	require.NotEqual(t, http.StatusSeeOther, rec.Code)
}

func TestWithoutAKnownControllerThePageIsServedHere(t *testing.T) {
	for _, tt := range []struct {
		name string
		role func() (bool, string, bool)
	}{
		{"no controller is known", roleOf(false, "", false)},
		{"the daemon names one it cannot reach", roleOf(false, "10.9.0.2", false)},
		{"this node is the controller", roleOf(true, "", false)},
		{"nothing is known", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := redirectingServer(t, tt.role)
			for _, path := range []string{"/", "/signin", "/assets/x.js", "/routes/app.example.com?owner=qemu%2F101"} {
				rec := get(s, path, "")
				require.Equal(t, http.StatusOK, rec.Code, path)
				require.Empty(t, rec.Header().Get("Location"), path)
			}
			require.Equal(t, http.StatusNotFound, get(s, "/nope", "").Code)
		})
	}
}

func TestOnlyAnAddressOfAnotherNodeIsRedirectedTo(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1", "::1", // loopback
		"0.0.0.0", "::", // unspecified
		"224.0.0.1", "ff02::1", // multicast
		"fe80::1%eth0", "10.9.0.2%eth0", // zoned
		"10.9.0.2:8643", "node2.example.com", "https://10.9.0.2", "", // not an address
	} {
		s := redirectingServer(t, roleOf(false, addr, true))
		rec := get(s, "/routes", "")
		require.Equal(t, http.StatusOK, rec.Code, "%q is served here", addr)
		require.Empty(t, rec.Header().Get("Location"), addr)
	}
	for _, addr := range []string{"10.9.0.2", "192.0.2.10", "fd00::2", "2001:db8::10"} {
		s := redirectingServer(t, roleOf(false, addr, true))
		require.Equal(t, http.StatusSeeOther, get(s, "/routes", "").Code, addr)
	}
}

func TestTheRoleIsAskedOnEveryRequest(t *testing.T) {
	controller := true
	s := redirectingServer(t, func() (bool, string, bool) { return controller, "10.9.0.2", true })
	require.Equal(t, http.StatusOK, get(s, "/", "").Code)
	controller = false
	require.Equal(t, http.StatusSeeOther, get(s, "/", "").Code)
}
