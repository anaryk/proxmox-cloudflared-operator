package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/pvefake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/ui"
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
