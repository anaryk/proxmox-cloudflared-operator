package main

import (
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

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
	r := newRunner(t, filepath.Join(dir, "pco", "api.sock"))
	res := r.run("", "web", "--listen", "127.0.0.1:0",
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
	args := []string{"web", "--listen", "127.0.0.1:0", "--cert", filepath.Join(dir, "tls.crt"), "--key", filepath.Join(dir, "tls.key")}

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

func webApp(vars map[string]string) *app {
	e := testEnv()
	e.getenv = func(name string) string { return vars[name] }
	return &app{env: e}
}
