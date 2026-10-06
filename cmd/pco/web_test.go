package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/pvefake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/ui"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/wire"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

func TestWebConfig(t *testing.T) {
	unit := map[string]string{"CREDENTIALS_DIRECTORY": "/run/credentials/pco-web.service"}
	setup := map[string]string{
		"CREDENTIALS_DIRECTORY": "/run/credentials/pco-web.service",
		"PCO_WEB_LISTEN":        "192.0.2.10:8643",
		"PCO_WEB_HOSTS":         " pve1.example.com,pco.example.com,, ",
		"PCO_WEB_HSTS":          "1",
	}
	type want struct {
		listen, cert, key string
		hosts             []string
		hsts              bool
	}
	cases := []struct {
		name  string
		env   map[string]string
		flags webFlags
		want  want
	}{
		{"the unit without an environment file", unit, webFlags{}, want{
			listen: "127.0.0.1:8643",
			cert:   "/run/credentials/pco-web.service/tls.crt",
			key:    "/run/credentials/pco-web.service/tls.key",
		}},
		{"the environment setup writes", setup, webFlags{}, want{
			listen: "192.0.2.10:8643",
			cert:   "/run/credentials/pco-web.service/tls.crt",
			key:    "/run/credentials/pco-web.service/tls.key",
			hosts:  []string{"pve1.example.com", "pco.example.com"},
			hsts:   true,
		}},
		{"the flags win", setup, webFlags{listen: "127.0.0.1:9443", cert: "/tmp/c.pem", key: "/tmp/k.pem", hosts: "lab.example.com"}, want{
			listen: "127.0.0.1:9443",
			cert:   "/tmp/c.pem",
			key:    "/tmp/k.pem",
			hosts:  []string{"lab.example.com"},
			hsts:   true,
		}},
		{"HSTS off", map[string]string{"PCO_WEB_HSTS": "0"}, webFlags{cert: "/c", key: "/k"}, want{
			listen: "127.0.0.1:8643", cert: "/c", key: "/k",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := webApp(c.env).webConfig(c.flags)
			require.NoError(t, err)
			require.Equal(t, c.want, want{cfg.Listen, cfg.CertFile, cfg.KeyFile, cfg.Hosts, cfg.HSTS})
			require.NotNil(t, cfg.Assets)
			require.NotNil(t, cfg.Now)
		})
	}
}

func TestWebConfigRefuses(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		flags webFlags
		want  string
	}{
		{"no certificate", nil, webFlags{}, "--cert and --key"},
		{"a certificate without its key", map[string]string{"CREDENTIALS_DIRECTORY": "/run/c"}, webFlags{cert: "/c"}, "--cert and --key go together"},
		{"a key without its certificate", nil, webFlags{key: "/k"}, "--cert and --key go together"},
		{"HSTS of another word", map[string]string{"PCO_WEB_HSTS": "yes"}, webFlags{cert: "/c", key: "/k"}, `PCO_WEB_HSTS is "yes": want 1 or 0`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := webApp(c.env).webConfig(c.flags)
			require.ErrorContains(t, err, c.want)
		})
	}
}

func TestWebStopsWithoutItsCertificate(t *testing.T) {
	dir := t.TempDir()
	pin := writePin(t, dir)
	r := newRunner(t, filepath.Join(dir, "pco", "api.sock"))
	res := r.run("", "web", "--listen", "127.0.0.1:0", "--pin", pin,
		"--cert", filepath.Join(dir, "tls.crt"), "--key", filepath.Join(dir, "tls.key"))
	require.ErrorContains(t, res.err, filepath.Join(dir, "tls.crt"))

	res = r.run("", "web", "--json")
	require.ErrorContains(t, res.err, "--json has no meaning")
}

func TestWebRefusesAnUnknownLogLevel(t *testing.T) {
	r := newRunner(t, "/nonexistent/pco/pco.sock")
	for _, level := range []string{"chatty", "none", ""} {
		res := r.run("", "web", "--log-level", level, "--cert", "/c", "--key", "/k")
		require.EqualError(t, res.err, `unknown log level "`+level+`": want trace, debug, info, warn or error`)
	}
}

func TestTheLogLevelOfWeb(t *testing.T) {
	for name, want := range map[string]zerolog.Level{
		"trace": zerolog.TraceLevel,
		"debug": zerolog.DebugLevel,
		"info":  zerolog.InfoLevel,
		"warn":  zerolog.WarnLevel,
		"error": zerolog.ErrorLevel,
	} {
		got, err := logLevel(name)
		require.NoError(t, err, name)
		require.Equal(t, want, got, name)
	}
}

// The level reaches the log of the server: a build without the interface warns
// at start, and an error level leaves the warning out.
func TestWebLogsAtTheLevelGiven(t *testing.T) {
	if ui.Built {
		t.Skip("this build has the interface, so there is nothing to warn about")
	}
	dir := t.TempDir()
	r := newRunner(t, filepath.Join(dir, "pco", "api.sock"))
	args := []string{"web", "--listen", "127.0.0.1:0", "--pin", writePin(t, dir),
		"--cert", filepath.Join(dir, "tls.crt"), "--key", filepath.Join(dir, "tls.key")}

	res := r.run("", args...)
	require.Error(t, res.err)
	require.Contains(t, res.errOut, "has no web interface")
	require.Contains(t, res.errOut, `"level":"warn"`)

	res = r.run("", append(args, "--log-level", "error")...)
	require.Error(t, res.err)
	require.Empty(t, res.errOut)

	res = r.run("", append(args, "--log-level", "info")...)
	require.Contains(t, res.errOut, "has no web interface")
}

func TestWebStopsWithoutThePin(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(t, filepath.Join(dir, "pco", "api.sock"))
	cert := []string{"--cert", filepath.Join(dir, "tls.crt"), "--key", filepath.Join(dir, "tls.key")}

	res := r.run("", append([]string{"web", "--pin", filepath.Join(dir, "missing.crt")}, cert...)...)
	require.ErrorContains(t, res.err, "reading the certificate of pveproxy")

	notPEM := filepath.Join(dir, "not.crt")
	require.NoError(t, os.WriteFile(notPEM, []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"), 0o600))
	res = r.run("", append([]string{"web", "--pin", notPEM}, cert...)...)
	require.ErrorContains(t, res.err, "holds no certificate")

	res = r.run("", append([]string{"web"}, cert...)...)
	require.ErrorContains(t, res.err, "--pin")
}

func TestWebPinFile(t *testing.T) {
	file, err := webApp(map[string]string{"CREDENTIALS_DIRECTORY": "/run/credentials/pco-web.service"}).webPinFile(webFlags{})
	require.NoError(t, err)
	require.Equal(t, "/run/credentials/pco-web.service/pveproxy.crt", file, "never tls.crt")

	file, err = webApp(map[string]string{"CREDENTIALS_DIRECTORY": "/run/c"}).webPinFile(webFlags{pin: "/tmp/fakepve/pveproxy.crt"})
	require.NoError(t, err)
	require.Equal(t, "/tmp/fakepve/pveproxy.crt", file)

	_, err = webApp(nil).webPinFile(webFlags{})
	require.ErrorContains(t, err, "--pin")
}

func TestNodeNames(t *testing.T) {
	hosts := []byte("127.0.0.1 localhost.localdomain localhost\n" +
		"# 192.0.2.9 pve1.old.lan pve1\n" +
		"192.0.2.10 pve1.example.lan pve1 # the node\n" +
		"::1 ip6-localhost\n")
	cases := []struct {
		hostname, short, fqdn string
		hosts                 []byte
	}{
		{"pve1", "pve1", "pve1.example.lan", hosts},
		{"pve1.example.lan", "pve1", "pve1.example.lan", nil},
		{"pve2", "pve2", "", hosts},
		{"pve1", "pve1", "", nil},
		{"pve1", "pve1", "", []byte("192.0.2.10 pve1\n")},
	}
	for _, c := range cases {
		short, fqdn := nodeNames(c.hostname, c.hosts)
		require.Equal(t, c.short, short, c.hostname)
		require.Equal(t, c.fqdn, fqdn, c.hostname)
	}
}

func TestNodeZone(t *testing.T) {
	require.Equal(t, "Europe/Prague", nodeZone("", "/usr/share/zoneinfo/Europe/Prague", nil))
	require.Equal(t, "America/Argentina/Salta", nodeZone("", "../usr/share/zoneinfo/America/Argentina/Salta", nil))
	require.Equal(t, "Asia/Tokyo", nodeZone(":Asia/Tokyo", "/usr/share/zoneinfo/Europe/Prague", nil), "TZ wins, as it does for the process")
	require.Equal(t, "UTC", nodeZone("", "", os.ErrNotExist), "no /etc/localtime is UTC")
	require.Empty(t, nodeZone("", "/etc/zone-copy", nil))
	require.Empty(t, nodeZone("", "", os.ErrInvalid))
}

// A user signed in to pco web reaches the daemon on --socket: a change and a
// read of the page go through to it, and one without the session does not.
func TestWebForwardsTheCallsOfASession(t *testing.T) {
	const token = "alice@pve!pco=0b5c3e7e-1d2f-4a6b-9c8d-7e6f5a4b3c2d"
	dir := t.TempDir()
	pve, err := pvefake.New("pve1", pvefake.Users{Users: []pvefake.User{{
		ID:         "alice@pve",
		Privileges: map[string][]string{"/": {"Sys.Audit", "Sys.Modify"}},
		Tokens:     []pvefake.Token{{ID: "pco", Secret: "0b5c3e7e-1d2f-4a6b-9c8d-7e6f5a4b3c2d"}},
	}}})
	require.NoError(t, err)
	pvePair, pinPEM, err := pvefake.SelfSigned(time.Now(), "127.0.0.1")
	require.NoError(t, err)
	proxmox := httptest.NewUnstartedServer(pve)
	proxmox.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pvePair}}
	proxmox.StartTLS()
	t.Cleanup(proxmox.Close)
	pin := filepath.Join(dir, "pveproxy.crt")
	require.NoError(t, os.WriteFile(pin, pinPEM, 0o644))
	certFile, keyFile, webCert := writeWebPair(t, dir)

	daemon := &fakeEngine{state: engine.State{Node: "pve1", Digest: "d1"}}
	socket := serveFake(t, daemon)
	listen := freeAddr(t)

	ctx, cancel := context.WithCancel(t.Context())
	var log testutil.SyncBuffer
	cmd := newRootCmdWith(testEnv())
	cmd.SetOut(&log)
	cmd.SetErr(&log)
	cmd.SetArgs([]string{"--socket", socket, "web", "--listen", listen, "--cert", certFile, "--key", keyFile,
		"--pin", pin, "--pve-url", proxmox.URL + "/api2/json"})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	defer func() {
		cancel()
		require.NoError(t, <-done, log.String())
	}()

	pool := x509.NewCertPool()
	pool.AddCert(webCert)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	origin := "https://" + listen
	call := func(method, path, csrf, body string) (int, string) {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, origin+path, rd)
		require.NoError(t, err)
		req.Header.Set("Origin", origin)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if csrf != "" {
			req.Header.Set("Pco-Csrf", csrf)
		}
		res, err := client.Do(req)
		require.NoError(t, err)
		defer func() { _ = res.Body.Close() }()
		data, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return res.StatusCode, string(data)
	}

	require.Eventually(t, func() bool {
		res, err := client.Get(origin + "/api/session")
		if err == nil {
			_ = res.Body.Close()
		}
		return err == nil
	}, 10*time.Second, 20*time.Millisecond, "pco web does not answer: %s", log.String())

	status, _ := call(http.MethodPost, "/api/v1/sync", "", "{}")
	require.Equal(t, http.StatusUnauthorized, status)
	require.Empty(t, daemon.called(), "a call without a session reaches no daemon")

	status, body := call(http.MethodPost, "/api/session/token", "", `{"token":"`+token+`"}`)
	require.Equal(t, http.StatusOK, status, body)
	var session wire.Session
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	require.Equal(t, "admin", session.Role)

	status, body = call(http.MethodPost, "/api/v1/sync", session.CSRF, "{}")
	require.Less(t, status, 300, body)
	require.Equal(t, []string{"sync"}, daemon.called())

	status, body = call(http.MethodGet, "/api/v1/state", "", "")
	require.Equal(t, http.StatusOK, status, body)
	var st engine.State
	require.NoError(t, json.Unmarshal([]byte(body), &st))
	require.Equal(t, "pve1", st.Node)
}

// applianceEnv is the environment of pco web in an appliance whose net0 has
// the address net0, with vars as the unit's environment.
func applianceEnv(t *testing.T, net0 string, vars map[string]string) env {
	t.Helper()
	dir := t.TempDir()
	e := testEnv()
	e.profileFile = filepath.Join(dir, "profile")
	require.NoError(t, os.WriteFile(e.profileFile, []byte("appliance\n"), 0o644))
	e.net0File = filepath.Join(dir, "net0")
	if net0 != "" {
		require.NoError(t, os.WriteFile(e.net0File, []byte(net0+"\n"), 0o644))
	}
	e.getenv = func(name string) string { return vars[name] }
	return e
}

func TestTheApplianceListensOnNet0Only(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := writeWebPair(t, dir)
	for _, tt := range []struct {
		name, net0, listen, want string
	}{
		{"every address", "127.0.0.1", "0.0.0.0:8643", "pco-web must listen on net0's address only (127.0.0.1), not 0.0.0.0:8643"},
		{"every IPv6 address", "127.0.0.1", "[::]:8643", "pco-web must listen on net0's address only (127.0.0.1), not [::]:8643"},
		{"a leg's address", "127.0.0.1", "127.0.0.2:8643", "pco-web must listen on net0's address only (127.0.0.1), not 127.0.0.2:8643"},
		{"net0 unknown", "", "127.0.0.1:8643", "pco-web must listen on net0's address only, and its address is not known"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newRootCmdWith(applianceEnv(t, tt.net0, nil))
			var out testutil.SyncBuffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{"--socket", filepath.Join(dir, "pco", "pco.sock"), "web", "--listen", tt.listen,
				"--cert", certFile, "--key", keyFile, "--pve-api", filepath.Join(dir, "missing.json")})
			require.ErrorContains(t, cmd.ExecuteContext(t.Context()), tt.want)
		})
	}

	a := &app{env: applianceEnv(t, "192.0.2.30", nil)}
	cfg := web.Config{Listen: web.DefaultListen}
	require.NoError(t, a.applianceListen(webFlags{}, &cfg))
	require.Equal(t, "192.0.2.30:8643", cfg.Listen, "without an address given, net0's")
}

func TestTheSignInOfTheApplianceRefusesWhatItCannotRead(t *testing.T) {
	dir := t.TempDir()
	_, caPEM, err := pvefake.SelfSigned(time.Now(), "pve1")
	require.NoError(t, err)
	apiFile := filepath.Join(dir, "pve-api.json")
	require.NoError(t, os.WriteFile(apiFile, webcert.API{URL: "https://192.0.2.10:8006/api2/json", ServerName: "pve1", CA: string(caPEM)}.Encode(), 0o644))

	a := &app{env: applianceEnv(t, "192.0.2.30", map[string]string{"PCO_WEB_ALLOW_ROOT": "yes"})}
	_, err = a.applianceAuth(webFlags{pveAPI: apiFile}, web.Config{})
	require.EqualError(t, err, `PCO_WEB_ALLOW_ROOT is "yes": want 1 or 0`)

	a = &app{env: applianceEnv(t, "192.0.2.30", nil)}
	_, err = a.applianceAuth(webFlags{}, web.Config{})
	require.ErrorContains(t, err, "no API of the node to sign users in at: give --pve-api")
	_, err = a.applianceAuth(webFlags{pveAPI: filepath.Join(dir, "missing.json")}, web.Config{})
	require.ErrorContains(t, err, "reading the API of the node")
}

// In the appliance a user signs in with the user and password of Proxmox VE,
// at the node's API as the daemon wrote it: verified under its server name
// against its CA, not pinned.
func TestTheApplianceSignsInWithAPassword(t *testing.T) {
	const password = "pw-dora-Zebra-7731"
	dir := t.TempDir()
	pve, err := pvefake.New("pve1", pvefake.Users{Users: []pvefake.User{
		{ID: "dora@pve", Password: password, Privileges: map[string][]string{"/": {"Sys.Audit", "Sys.Modify"}}},
		{ID: "root@pam", Password: password, Privileges: map[string][]string{"/": {"Sys.Audit", "Sys.Modify"}}},
	}})
	require.NoError(t, err)
	pvePair, caPEM, err := pvefake.SelfSigned(time.Now(), "pve1.example.lan", "127.0.0.1")
	require.NoError(t, err)
	proxmox := httptest.NewUnstartedServer(pve)
	proxmox.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pvePair}}
	proxmox.StartTLS()
	t.Cleanup(proxmox.Close)
	apiFile := filepath.Join(dir, "pve-api.json")
	require.NoError(t, os.WriteFile(apiFile, webcert.API{
		URL: proxmox.URL + "/api2/json", ServerName: "pve1.example.lan", CA: string(caPEM), Node: "pve1",
	}.Encode(), 0o644))
	certFile, keyFile, webCert := writeWebPair(t, dir)
	socket := serveFake(t, &fakeEngine{state: engine.State{Node: "pve1", Digest: "d1"}})
	listen := freeAddr(t)

	ctx, cancel := context.WithCancel(t.Context())
	var log testutil.SyncBuffer
	cmd := newRootCmdWith(applianceEnv(t, "127.0.0.1", nil))
	cmd.SetOut(&log)
	cmd.SetErr(&log)
	cmd.SetArgs([]string{"--socket", socket, "web", "--listen", listen, "--cert", certFile, "--key", keyFile, "--pve-api", apiFile})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	defer func() {
		cancel()
		require.NoError(t, <-done, log.String())
	}()

	pool := x509.NewCertPool()
	pool.AddCert(webCert)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	origin := "https://" + listen
	require.Eventually(t, func() bool {
		res, err := client.Get(origin + "/api/session")
		if err == nil {
			_ = res.Body.Close()
		}
		return err == nil
	}, 10*time.Second, 20*time.Millisecond, "pco web does not answer: %s", log.String())
	post := func(body string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/api/session/password", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		require.NoError(t, err)
		defer func() { _ = res.Body.Close() }()
		data, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return res.StatusCode, string(data)
	}

	res, err := client.Get(origin + "/api/session")
	require.NoError(t, err)
	var u wire.Unauthenticated
	require.NoError(t, json.NewDecoder(res.Body).Decode(&u))
	_ = res.Body.Close()
	require.Equal(t, []string{"password", "token"}, u.Methods)
	require.Equal(t, []string{"pam", "pve"}, u.Realms)

	status, body := post(`{"user":"dora","realm":"pve","password":"` + password + `"}`)
	require.Equal(t, http.StatusOK, status, body)
	var session wire.Session
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	require.Equal(t, "dora@pve", session.User)
	require.Equal(t, "password", session.Method)
	require.Equal(t, "admin", session.Role)
	require.Equal(t, "appliance", session.Profile)
	require.Equal(t, "pve1", session.Node)

	status, body = post(`{"user":"root","realm":"pam","password":"` + password + `"}`)
	require.Equal(t, http.StatusForbidden, status, body)
	require.NotContains(t, log.String(), password)
}

// writeWebPair writes a certificate of pco web for 127.0.0.1 and its key,
// and returns their files and the certificate.
func writeWebPair(t *testing.T, dir string) (string, string, *x509.Certificate) {
	t.Helper()
	pair, certPEM, err := pvefake.SelfSigned(time.Now(), "127.0.0.1")
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	require.NoError(t, err)
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(certFile, certPEM, 0o644))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))
	return certFile, keyFile, pair.Leaf
}

// freeAddr is an address on loopback whose port nothing listened on a moment
// ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

// writePin writes a certificate as pveproxy's and returns its file.
func writePin(t *testing.T, dir string) string {
	t.Helper()
	_, certPEM, err := pvefake.SelfSigned(time.Now(), "127.0.0.1")
	require.NoError(t, err)
	file := filepath.Join(dir, "pveproxy.crt")
	require.NoError(t, os.WriteFile(file, certPEM, 0o644))
	return file
}

func webApp(vars map[string]string) *app {
	e := testEnv()
	e.getenv = func(name string) string { return vars[name] }
	return &app{env: e}
}
