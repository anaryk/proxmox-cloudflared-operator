package daemon

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const tunnelID = "00000000-0000-4000-8000-000000000001"

func TestTheDaemonRunsACycleOverItsSocket(t *testing.T) {
	w := newWorld(t)
	d := w.start()
	ctx := t.Context()

	version, err := d.client.Version(ctx)
	require.NoError(t, err)
	require.Equal(t, "1.2.3", version)

	require.NoError(t, d.client.Sync(ctx))
	st := d.await(func(st engine.State) bool { return len(st.Routes) == 1 })

	require.Equal(t, "observe", st.Mode, "a fresh install only observes")
	require.Equal(t, store.ProfileHost, st.Profile)
	require.True(t, st.Complete)
	require.Empty(t, st.Problems)
	require.Equal(t, "www.example.com", st.Routes[0].Hostname)
	require.Equal(t, "qemu/101", st.Routes[0].Owner)
	require.Equal(t, planner.StateActive, st.Routes[0].State)
	require.Equal(t, "http://"+guestAddress+":8080", st.Routes[0].Service)
	require.Len(t, st.Credentials, 1)
	require.Equal(t, testCred, st.Credentials[0].ID)
	for _, a := range st.Actions {
		require.False(t, a.Applied, "%s %s", a.Kind, a.Target)
	}
	require.Empty(t, w.sysd.units(), "observe-only mode starts no connector")

	for _, auth := range w.pve.authorizations() {
		require.Equal(t, "PVEAPIToken="+pveTokenID+"="+pveSecret, auth, "the token of the store is the one sent to Proxmox")
	}
	require.NotEmpty(t, w.pve.authorizations())
}

func TestApplyThroughTheSocketPublishesTheRoute(t *testing.T) {
	w := newWorld(t)
	d := w.start()
	ctx := t.Context()

	require.NoError(t, d.client.Apply(ctx, false))
	st := d.await(func(st engine.State) bool {
		return st.Mode == "enforce" && len(st.Tunnels) == 1 && st.Tunnels[0].Verified && len(st.Connectors) == 1
	})

	require.Equal(t, tunnelID, st.Tunnels[0].ID)
	tunnel, found, err := w.cf.FindTunnel(ctx, "acc1", planner.TunnelName(testInstall))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, tunnelID, tunnel.ID)
	require.Equal(t, []string{"pco-cloudflared@" + tunnelID + ".service"}, w.sysd.units(), "the connector was started through systemd")
	files, err := os.ReadDir(filepath.Join(w.paths.Local, tunnelsDir))
	require.NoError(t, err)
	require.NotEmpty(t, files, "the token and env files of the connector are in the local root")

	d.await(func(st engine.State) bool {
		for _, a := range st.Actions {
			if a.Kind == "create-record" && a.Applied {
				return true
			}
		}
		return false
	})
	records, err := w.cf.Records(ctx, "zone1", cfapi.RecordFilter{})
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "www.example.com", records[0].Name)
	require.Equal(t, tunnelID+".cfargotunnel.com", records[0].Content)

	require.NoError(t, d.stop())
	logs := w.logs.String()
	require.NotContains(t, logs, pveSecret)
	require.NotContains(t, logs, cfToken)
}

func TestReadyComesWhenTheSocketAnswersAndStoppingWhenTheStopBegins(t *testing.T) {
	w := newWorld(t)
	var answered error
	w.notify.onReady = func() {
		// The socket must be listening by the time systemd is told so.
		_, answered = apiclient.New(w.cfg.SocketPath).Version(context.Background())
	}
	d := w.start()

	require.NoError(t, answered, "the socket answered when READY=1 was sent")
	require.Equal(t, []string{"READY=1"}, w.notify.sent())

	require.NoError(t, d.stop(), "a signal ends the daemon with exit code 0")
	require.Equal(t, []string{"READY=1", "STOPPING=1"}, w.notify.sent())
	_, err := os.Lstat(w.cfg.SocketPath)
	require.ErrorIs(t, err, fs.ErrNotExist, "the socket is removed")
}

func TestOneDaemonRunsOnANode(t *testing.T) {
	w := newWorld(t)
	d := w.start()

	// Another socket: only the lock of the node can stop this one.
	second := w.cfg
	second.SocketPath = filepath.Join(shortDir(t), "pco", "pco.sock")
	err := Run(t.Context(), second, w.deps)
	require.ErrorIs(t, err, ErrRunning)
	require.EqualError(t, err, "another pco daemon is running on this node")

	require.NoError(t, d.stop())
	release, err := lockNode(w.paths.Local)
	require.NoError(t, err, "the lock goes with the daemon")
	release()
}

func TestTheDaemonWontStartWithoutTheProxmoxToken(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(w *world)
		want  string
	}{
		{
			name: "no token",
			setup: func(w *world) {
				require.NoError(t, os.Remove(filepath.Join(w.paths.Private, "meta", "pve-token.json")))
			},
			want: "no Proxmox API token found; run pco setup",
		},
		{
			name: "not set up",
			setup: func(w *world) {
				w.cfg.Paths.Cluster = filepath.Join(w.dir, "nowhere", "cluster")
				w.cfg.Paths.Private = filepath.Join(w.dir, "nowhere", "private")
			},
			want: "pco is not set up on this node; run pco setup",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			tt.setup(w)

			err := Run(t.Context(), w.cfg, w.deps)

			require.EqualError(t, err, tt.want)
			require.Empty(t, w.notify.sent())
		})
	}
}

func TestTheDaemonWaitsForTheClusterFilesystemToReadTheToken(t *testing.T) {
	w := newWorld(t)
	require.NoError(t, os.Remove(w.mount))
	var slept []time.Duration
	w.deps.Sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		if len(slept) == 3 {
			require.NoError(t, os.WriteFile(w.mount, nil, 0o600))
		}
		return nil
	}

	d := w.start()

	require.Equal(t, []time.Duration{10 * time.Second, 10 * time.Second, 10 * time.Second}, slept)
	require.NoError(t, d.client.Sync(t.Context()))
	d.await(func(st engine.State) bool { return len(st.Routes) == 1 })
}

func TestTheDaemonGivesUpWaitingForTheClusterFilesystemAfterTwoMinutes(t *testing.T) {
	w := newWorld(t)
	require.NoError(t, os.Remove(w.mount))
	var slept time.Duration
	w.deps.Sleep = func(_ context.Context, d time.Duration) error {
		slept += d
		return nil
	}

	err := Run(t.Context(), w.cfg, w.deps)

	require.ErrorContains(t, err, "the cluster filesystem is still not mounted after 2m0s")
	require.Equal(t, 2*time.Minute, slept)
	require.Empty(t, w.notify.sent())
}

func TestTheDaemonKeepsRunningWithoutTheClusterFilesystem(t *testing.T) {
	w := newWorld(t)
	d := w.start()
	ctx := t.Context()
	require.NoError(t, d.client.Sync(ctx))
	d.await(func(st engine.State) bool { return len(st.Routes) == 1 })

	require.NoError(t, os.Remove(w.mount))
	require.NoError(t, d.client.Sync(ctx))
	// A cycle already under way when the mount went may have its inventory
	// and still fail on the store; the one after it has nothing to go on.
	d.await(func(st engine.State) bool {
		return containsProblem(st, "cluster filesystem is not mounted") && !st.Complete
	})
	version, err := d.client.Version(ctx)
	require.NoError(t, err, "the socket still answers")
	require.Equal(t, "1.2.3", version)

	require.NoError(t, os.WriteFile(w.mount, nil, 0o600))
	require.NoError(t, d.client.Sync(ctx))
	d.await(func(st engine.State) bool { return st.Complete && len(st.Problems) == 0 })
}

func TestSettingsReadAtStartAreFlaggedWhenTheyChange(t *testing.T) {
	const warning = "restart pco for them to take effect"
	w := newWorld(t)
	d := w.start()
	ctx := t.Context()
	d.await(func(st engine.State) bool { return len(st.Routes) == 1 })
	require.NotContains(t, w.logs.String(), warning)

	s, err := w.store.Settings()
	require.NoError(t, err)
	s.GateTag = "publish"
	s.PollInterval = store.Duration(30 * time.Second) // read by every cycle: no warning of its own
	require.NoError(t, w.store.SaveSettings(s))
	require.NoError(t, d.client.Sync(ctx))
	require.Eventually(t, func() bool { return strings.Contains(w.logs.String(), warning) }, 10*time.Second, 5*time.Millisecond)
	require.Contains(t, w.logs.String(), `"settings":["gateTag"]`)

	// Cycles that follow say nothing more about the same change. The second
	// one started after the warning was logged.
	at := d.state().At
	for range 2 {
		require.NoError(t, d.client.Sync(ctx))
		at = d.await(func(st engine.State) bool { return st.At.After(at) }).At
	}
	require.Equal(t, 1, strings.Count(w.logs.String(), warning))
}

func TestTheDaemonLogsTheProfileAtStart(t *testing.T) {
	w := newWorld(t)
	require.NoError(t, w.store.SaveInstall(store.Install{ID: testInstall, CreatedAt: t0, Profile: store.ProfileAppliance}))
	d := w.start()
	require.NoError(t, d.stop())

	require.Contains(t, w.logs.String(), `"profile":"appliance"`)
	require.Contains(t, w.logs.String(), `"message":"pco daemon starting"`)
}

func containsProblem(st engine.State, text string) bool {
	for _, p := range st.Problems {
		if strings.Contains(p, text) {
			return true
		}
	}
	return false
}
