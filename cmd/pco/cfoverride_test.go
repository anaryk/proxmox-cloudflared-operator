package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/daemon"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

const refusedOverride = "PCO_CLOUDFLARE_API_URL must be an http or https URL of a loopback address or localhost, " +
	"without credentials, query or fragment, such as http://127.0.0.1:8787/client/v4; it is for tests only"

func withOverride(e env, value string) env {
	e.getenv = func(name string) string {
		if name == "PCO_CLOUDFLARE_API_URL" {
			return value
		}
		return ""
	}
	return e
}

func TestTheCloudflareOverrideTakesOnlyThisMachine(t *testing.T) {
	for _, tt := range []struct{ value, want string }{
		{"", ""},
		{"http://127.0.0.1:8787/client/v4", "http://127.0.0.1:8787/client/v4"},
		{"http://127.9.8.7:8787/client/v4", "http://127.9.8.7:8787/client/v4"},
		{"http://[::1]:8787/client/v4", "http://[::1]:8787/client/v4"},
		{"https://127.0.0.1:8443/client/v4", "https://127.0.0.1:8443/client/v4"},
		{"http://localhost:8787/client/v4", "http://127.0.0.1:8787/client/v4"},
		{"http://LocalHost/client/v4", "http://127.0.0.1/client/v4"},
		{"https://localhost:8443/client/v4", "https://localhost:8443/client/v4"},
		{"http://[::ffff:127.0.0.1]:8787/client/v4", "http://[::ffff:127.0.0.1]:8787/client/v4"},
		{"http://[::1%25lo]:8787/client/v4", "http://[::1%25lo]:8787/client/v4"},
	} {
		t.Run(tt.value, func(t *testing.T) {
			a := &app{env: withOverride(testEnv(), tt.value)}
			got, err := a.cloudflareOverride()
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}

	for _, value := range []string{
		"https://api.cloudflare.com/client/v4",
		"http://192.168.4.209:8787/client/v4",
		"http://10.0.0.5/client/v4",
		"http://localhost.example.com/client/v4",
		"http://127.0.0.1.nip.io/client/v4",
		"ftp://127.0.0.1/client/v4",
		"127.0.0.1:8787",
		"/client/v4",
		"http:127.0.0.1",
		"http://user:secret@127.0.0.1:8787/client/v4",
		"http://127.0.0.1:8787/client/v4?a=b",
		"http://127.0.0.1:8787/client/v4#top",
		"http://127.0.0.1:8787/client/\x00v4",
		"http://0.0.0.0:8787/client/v4",
		"http://[::]:8787/client/v4",
		"http://127.1:8787/client/v4",
		"http://localhost.:8787/client/v4",
		"https://localhost.:8443/client/v4",
		"http://[::ffff:10.0.0.1]:8787/client/v4",
		// The long s folds onto s for strings.EqualFold.
		"http://localhoſt:8787/client/v4",
		"https://localhoſt:8443/client/v4",
	} {
		t.Run(value, func(t *testing.T) {
			a := &app{env: withOverride(testEnv(), value)}
			_, err := a.cloudflareOverride()
			require.EqualError(t, err, refusedOverride)
		})
	}
}

func TestTheClientsOfSetupTalkToTheOverride(t *testing.T) {
	fake := cffake.New()
	srv := httptest.NewServer(cffake.Handler(fake, cffake.WithToken("tok-0123456789")))
	t.Cleanup(srv.Close)

	api, err := cloudflareClients(srv.URL + "/client/v4")("tok-0123456789")
	require.NoError(t, err)
	status, err := api.VerifyToken(t.Context())
	require.NoError(t, err)
	require.Equal(t, "active", status.Status)
	require.Contains(t, fake.Calls(), "VerifyToken")
}

func TestSetupAndUninstallRefuseAnOverrideOfAnotherMachine(t *testing.T) {
	r := newRunner(t, "/nonexistent/pco/pco.sock")
	r.env = withOverride(r.env, "https://cf.example.com/client/v4")
	for _, args := range [][]string{{"setup", "--yes"}, {"uninstall", "--yes", "--keep-cloudflare"}} {
		res := r.runReader(unreadable{t}, args...)
		require.EqualError(t, res.err, refusedOverride, args)
	}
}

func TestTheDaemonRefusesAnOverrideOfAnotherMachine(t *testing.T) {
	r := newRunner(t, "/nonexistent/pco/pco.sock")
	r.env = withOverride(r.env, "http://192.168.4.209:8787/client/v4")

	res := r.run("", "daemon", "--node", "pve1")

	require.EqualError(t, res.err, refusedOverride)
}

// meAsWeb is a system on which the user and the group of the web UI are the
// ones the test runs as, so that the socket of the daemon answers the test.
type meAsWeb struct{ me *user.User }

func (m meAsWeb) LookupUser(name string) (*user.User, error) {
	if name != "pco-web" {
		return nil, user.UnknownUserError(name)
	}
	return m.me, nil
}

func (meAsWeb) WebInstalled() bool { return true }

func (m meAsWeb) LookupGroup(name string) (*user.Group, error) {
	if name != "pco-web" {
		return nil, user.UnknownGroupError(name)
	}
	return &user.Group{Gid: m.me.Gid, Name: name}, nil
}

// A daemon whose Cloudflare is overridden says so at its start and in every
// state, and talks to the override. Proxmox is a server of the test that
// refuses every request; the rest of the host is faked as in the test of the
// signals.
func TestTheDaemonWarnsOfTheOverrideAndCarriesItAsAProblem(t *testing.T) {
	proxmox := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"no ticket"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(proxmox.Close)
	fake := cffake.New()
	cf := httptest.NewServer(cffake.Handler(fake))
	t.Cleanup(cf.Close)
	override := cf.URL + "/client/v4"
	line := "the Cloudflare API is overridden to " + override + " (PCO_CLOUDFLARE_API_URL); this is for tests only"

	base := testutil.ShortDir(t)
	paths, err := daemon.StorePaths(store.ProfileHost, filepath.Join(base, "cluster"), filepath.Join(base, "private"), filepath.Join(base, "local"))
	require.NoError(t, err)
	s, err := store.Open(paths)
	require.NoError(t, err)
	require.NoError(t, s.Init())
	require.NoError(t, s.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret("pve-secret-0123456789")}))
	require.NoError(t, s.SaveInstall(store.Install{ID: "0123456789ab", CreatedAt: t0}))
	require.NoError(t, s.SaveCredential(store.Credential{ID: "cred1", Label: "main", Kind: "scoped", Token: store.NewSecret("tok-0123456789"), AddedAt: t0}))
	me, err := user.Current()
	require.NoError(t, err)

	notify := &readyNotifier{ready: make(chan struct{})}
	r := newRunner(t, filepath.Join(base, "run", "pco", "pco.sock"))
	r.env = withOverride(r.env, override)
	r.env.daemon = daemon.Deps{Notifier: notify, Accounts: meAsWeb{me}, Systemd: noSystemd{}, Prober: noProber{}}
	cmd := newRootCmdWith(r.env)
	var errOut testutil.SyncBuffer
	cmd.SetErr(&errOut)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--socket", r.socket, "daemon",
		"--cluster-dir", paths.Cluster, "--private-dir", paths.Private, "--local-dir", paths.Local,
		"--node", "pve1", "--pve-url", proxmox.URL,
	})
	require.NoError(t, os.MkdirAll(filepath.Join(base, "run"), 0o700))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-notify.ready:
	case err := <-done:
		t.Fatalf("the daemon ended before it was ready: %v\n%s", err, errOut.String())
	}

	require.Contains(t, errOut.String(), `"level":"warn"`)
	require.Contains(t, errOut.String(), line)
	client := apiclient.New(r.socket)
	require.Eventually(t, func() bool {
		st, err := client.Status(ctx)
		return err == nil && !st.FinishedAt.IsZero() && containsLine(st, line)
	}, 10*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		for _, c := range fake.Calls() {
			if c == "VerifyToken" {
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond, "the daemon checks the token of the credential at the override")
}

func containsLine(st engine.State, line string) bool {
	for _, p := range st.Problems {
		if p == line {
			return true
		}
	}
	return false
}
