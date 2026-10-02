package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/daemon"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

func TestTheDaemonCommandFlags(t *testing.T) {
	cmd, _, err := newRootCmd().Find([]string{"daemon"})
	require.NoError(t, err)
	flags := cmd.Flags()

	for name, want := range map[string]string{
		"pve-url":     "https://127.0.0.1:8006",
		"pve-ca-file": "",
		"log-level":   "info",
		"cluster-dir": "",
		"private-dir": "",
		"local-dir":   "",
	} {
		f := flags.Lookup(name)
		require.NotNil(t, f, name)
		require.Equal(t, want, f.DefValue, name)
	}
	require.Equal(t, defaultNode(), flags.Lookup("node").DefValue)

	root := newRootCmd()
	require.Equal(t, "/run/pco/pco.sock", root.PersistentFlags().Lookup("socket").DefValue)
	require.Contains(t, root.PersistentFlags().Lookup("socket").Usage, "named pco")
}

func TestTheNodeIsTheShortHostname(t *testing.T) {
	for host, want := range map[string]string{
		"pve1":             "pve1",
		"pve1.example.com": "pve1",
		"":                 "",
	} {
		require.Equal(t, want, shortHostname(host), host)
	}
}

func TestTheDaemonRefusesAnUnknownLogLevel(t *testing.T) {
	res := newRunner(t, "/nonexistent/pco/pco.sock").run("", "daemon", "--log-level", "chatty")

	require.EqualError(t, res.err, `unknown log level "chatty": want trace, debug, info, warn or error`)
}

func TestTheDaemonSaysWhenTheNodeIsNotSetUp(t *testing.T) {
	base := testutil.ShortDir(t)
	r := newRunner(t, filepath.Join(base, "run", "pco", "pco.sock"))

	res := r.run("", "daemon",
		"--cluster-dir", filepath.Join(base, "nowhere", "cluster"),
		"--private-dir", filepath.Join(base, "nowhere", "private"),
		"--local-dir", filepath.Join(base, "local"),
		"--node", "pve1")

	require.EqualError(t, res.err, "pco is not set up on this node; run pco setup")
}

// The lock of the node is in the local directory: another one with the same
// cluster store would let a second daemon reconcile it.
func TestTheLocalDirectoryNeedsAStoreOfItsOwn(t *testing.T) {
	for name, others := range map[string][]string{
		"alone":                 nil,
		"with the cluster only": {"--cluster-dir", "cluster"},
		"with the private only": {"--private-dir", "private"},
	} {
		t.Run(name, func(t *testing.T) {
			base := testutil.ShortDir(t)
			args := []string{"daemon", "--node", "pve1", "--local-dir", filepath.Join(base, "local")}
			for i := 0; i < len(others); i += 2 {
				args = append(args, others[i], filepath.Join(base, others[i+1]))
			}
			r := newRunner(t, filepath.Join(base, "run", "pco", "pco.sock"))

			res := r.run("", args...)

			require.ErrorContains(t, res.err, "--local-dir is refused without --cluster-dir and --private-dir")
			require.NoDirExists(t, filepath.Join(base, "local"), "nothing was made")
		})
	}
}

// readyNotifier tells a test when the daemon says it is ready.
type readyNotifier struct {
	mu     sync.Mutex
	ready  chan struct{}
	events []string
}

func (n *readyNotifier) Ready() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, "READY=1")
	close(n.ready)
	return nil
}

func (n *readyNotifier) Stopping() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, "STOPPING=1")
	return nil
}

func (n *readyNotifier) sent() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.events...)
}

// noAccounts is a system without the user and the group of the web UI.
type noAccounts struct{}

func (noAccounts) LookupUser(name string) (*user.User, error) {
	return nil, user.UnknownUserError(name)
}

func (noAccounts) LookupGroup(name string) (*user.Group, error) {
	return nil, user.UnknownGroupError(name)
}

var errNoSystemd = errors.New("no systemd in this test")

// noSystemd is a systemd that nothing is asked of in this test.
type noSystemd struct{}

func (noSystemd) EnableNow(context.Context, string) error  { return errNoSystemd }
func (noSystemd) DisableNow(context.Context, string) error { return errNoSystemd }
func (noSystemd) Restart(context.Context, string) error    { return errNoSystemd }

func (noSystemd) IsActive(context.Context, string) (bool, error) {
	return false, errNoSystemd
}

func (noSystemd) ListUnits(context.Context, string) ([]string, error) {
	return nil, errNoSystemd
}

var errNoHost = errors.New("no host in this test")

// noProber sees no network.
type noProber struct{}

func (noProber) Interfaces(context.Context) ([]resolve.HostIface, error) { return nil, errNoHost }

func (noProber) Route(context.Context, netip.Addr) (string, bool, error) {
	return "", false, errNoHost
}

func (noProber) ARP(context.Context, string, netip.Addr) ([]string, error) { return nil, errNoHost }

func (noProber) FDBPorts(context.Context, string, int, string) ([]string, error) {
	return nil, errNoHost
}

func (noProber) Dial(context.Context, netip.AddrPort) error { return errNoHost }

// A signal ends the daemon with a clean stop. The signals are sent to the
// process of the test, which is safe once the daemon has said it is ready: it
// has installed its handler by then. Nothing here reaches outside the test: the
// accounts, systemd and the network of the host are fakes, and Proxmox is a
// server of the test that refuses every request.
func TestASignalStopsTheDaemonCleanly(t *testing.T) {
	proxmox := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"no ticket"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(proxmox.Close)

	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			base := testutil.ShortDir(t)
			paths, err := daemon.StorePaths(filepath.Join(base, "cluster"), filepath.Join(base, "private"), filepath.Join(base, "local"))
			require.NoError(t, err)
			s, err := store.Open(paths)
			require.NoError(t, err)
			require.NoError(t, s.Init())
			require.NoError(t, s.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret("pve-secret-0123456789")}))

			notify := &readyNotifier{ready: make(chan struct{})}
			r := newRunner(t, filepath.Join(base, "run", "pco", "pco.sock"))
			r.env.daemon = daemon.Deps{Notifier: notify, Accounts: noAccounts{}, Systemd: noSystemd{}, Prober: noProber{}}
			cmd := newRootCmdWith(r.env)
			var errOut testutil.SyncBuffer
			cmd.SetErr(&errOut)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetArgs([]string{
				"--socket", r.socket, "daemon",
				"--cluster-dir", paths.Cluster, "--private-dir", paths.Private, "--local-dir", paths.Local,
				"--node", "pve1", "--pve-url", proxmox.URL, "--log-level", "debug",
			})
			// The socket's directory is made by the daemon, in a directory of ours.
			require.NoError(t, os.MkdirAll(filepath.Join(base, "run"), 0o700))
			done := make(chan error, 1)
			go func() { done <- cmd.ExecuteContext(t.Context()) }()

			select {
			case <-notify.ready:
			case err := <-done:
				t.Fatalf("the daemon ended before it was ready: %v\n%s", err, errOut.String())
			}
			require.NoError(t, syscall.Kill(os.Getpid(), sig))

			require.NoError(t, <-done, "a signal is a clean stop")
			require.Equal(t, []string{"READY=1", "STOPPING=1"}, notify.sent())
			logs := errOut.String()
			require.Contains(t, logs, `"message":"pco daemon starting"`)
			require.Contains(t, logs, `"message":"pco daemon stopping"`)
			require.NotContains(t, logs, "pve-secret-0123456789")
			for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
				require.True(t, strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}"), "the log is JSON lines: %q", line)
			}
		})
	}
}
