package daemon

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
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

	_, err := d.client.Apply(ctx, false, "")
	require.NoError(t, err)
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
	var cyclesAtReady int64
	w.notify.onReady = func() {
		// The socket must be listening by the time systemd is told so.
		_, answered = apiclient.New(w.cfg.SocketPath).Version(context.Background())
		cyclesAtReady = w.cycles.Load()
	}
	d := w.start()

	require.NoError(t, answered, "the socket answered when READY=1 was sent")
	require.Zero(t, cyclesAtReady, "the engine starts once the socket listens, not before")
	require.Equal(t, []string{"READY=1"}, w.notify.sent())

	require.NoError(t, d.stop(), "a signal ends the daemon with exit code 0")
	require.Equal(t, []string{"READY=1", "STOPPING=1"}, w.notify.sent())
	_, err := os.Lstat(w.cfg.SocketPath)
	require.ErrorIs(t, err, fs.ErrNotExist, "the socket is removed")
}

func TestOneDaemonRunsOnANode(t *testing.T) {
	w := newWorld(t)
	d := w.start()

	// Another socket: only the lock of the node can stop this one. A lock that
	// does not hold lets the second daemon run, and the deadline ends it.
	second := w.cfg
	second.SocketPath = filepath.Join(testutil.ShortDir(t), "pco", "pco.sock")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := Run(ctx, second, w.deps)
	require.ErrorIs(t, err, ErrRunning)
	require.EqualError(t, err, "another pco daemon is running on this node")

	require.NoError(t, d.stop())
	lock, err := lockNode(w.paths.Local)
	require.NoError(t, err, "the lock goes with the daemon")
	lock.release()
}

// The doctor and the diagnosis run inside the daemon, over its socket, with
// the host's facts asked through what the daemon was given.
func TestTheDaemonServesTheDoctorAndTheDiagnosis(t *testing.T) {
	w := newWorld(t)
	var mu sync.Mutex
	var dialed []string
	w.deps.Dial = func(_ context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		dialed = append(dialed, network+" "+addr)
		return nil, errors.New("no network in this test")
	}
	w.deps.Cloudflared = filepath.Join(w.dir, "cloudflared")
	require.NoError(t, os.WriteFile(w.deps.Cloudflared, []byte("#!/bin/sh\necho 'cloudflared version 2026.9.0 (built 2026-09-10-1200 UTC)'\n"), 0o700))
	// The clock of the daemon, which the test moves past the time a doctor
	// run is kept.
	var skew atomic.Int64
	clock := w.deps.Now
	w.deps.Now = func() time.Time { return clock().Add(time.Duration(skew.Load())) }
	d := w.start()
	ctx := t.Context()
	require.NoError(t, d.client.Sync(ctx))
	d.await(func(st engine.State) bool { return len(st.Routes) == 1 })

	findings, err := d.client.Doctor(ctx)

	require.NoError(t, err)
	byCheck := map[string]doctor.Finding{}
	for _, f := range findings {
		byCheck[f.Check] = f
	}
	require.Equal(t, doctor.Finding{Check: "outbound", Level: doctor.LevelFail,
		Detail: "region1.v2.argotunnel.com:7844 cannot be reached over TCP: no network in this test",
		Fix:    "allow outbound TCP and UDP to port 7844"}, byCheck["outbound"])
	require.Equal(t, doctor.LevelOK, byCheck["node lock"].Level, byCheck["node lock"].Detail)
	require.Equal(t, doctor.LevelOK, byCheck["store"].Level, byCheck["store"].Detail)
	require.Equal(t, doctor.Finding{Check: "proxmox", Level: doctor.LevelOK, Detail: "Proxmox VE 9.0"}, byCheck["proxmox"])
	require.Equal(t, doctor.Finding{Check: "cloudflared", Level: doctor.LevelOK, Detail: "cloudflared 2026.9.0"}, byCheck["cloudflared"],
		"the binary the daemon was given")
	require.Equal(t, doctor.LevelWarn, byCheck["mode"].Level)
	mu.Lock()
	require.Equal(t, []string{"tcp region1.v2.argotunnel.com:7844"}, dialed)
	mu.Unlock()

	steps, err := d.client.Diagnose(ctx, "www.example.com")
	require.NoError(t, err)
	require.Equal(t, doctor.Step{Name: "route", Level: doctor.LevelOK, Detail: "qemu/101 (web-1) holds it; state active"}, steps[0])
	require.Equal(t, doctor.Step{Name: "http", Level: doctor.LevelWarn, Detail: "skipped"}, steps[len(steps)-1],
		"observe-only: the tunnel does not exist yet")
	_, err = d.client.Diagnose(ctx, "nope.example.com")
	require.ErrorIs(t, err, engine.ErrNotFound)

	// The lock the doctor looks at is the one this daemon holds.
	require.NoError(t, os.Remove(filepath.Join(w.paths.Local, lockName)))
	skew.Add(int64(6 * time.Second))
	findings, err = d.client.Doctor(ctx)
	require.NoError(t, err)
	i := slices.IndexFunc(findings, func(f doctor.Finding) bool { return f.Check == "node lock" })
	require.GreaterOrEqual(t, i, 0)
	require.Equal(t, doctor.LevelFail, findings[i].Level)
	require.Contains(t, findings[i].Detail, "a second daemon could start")
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

func TestASocketThatCannotBeMadeLeavesTheEngineAlone(t *testing.T) {
	w := newWorld(t)
	// Serve refuses a socket whose directory is not named pco.
	w.cfg.SocketPath = filepath.Join(w.dir, "run", "pco.sock")

	err := Run(t.Context(), w.cfg, w.deps)

	require.ErrorContains(t, err, "pco")
	require.Empty(t, w.pve.authorizations(), "no cycle was started, so none was cut off")
	require.Zero(t, w.cycles.Load())
	require.NoFileExists(t, filepath.Join(w.paths.Local, "events.log"))
	require.NotContains(t, w.notify.sent(), "READY=1")
}

func TestStoppingWhileWaitingForTheClusterFilesystemIsACleanStop(t *testing.T) {
	w := newWorld(t)
	require.NoError(t, os.Remove(w.mount))
	ctx, cancel := context.WithCancel(t.Context())
	w.deps.Sleep = func(ctx context.Context, _ time.Duration) error {
		cancel() // SIGTERM, as far as the daemon can tell
		return ctx.Err()
	}

	err := Run(ctx, w.cfg, w.deps)

	require.NoError(t, err, "exit code 0")
	require.Empty(t, w.notify.sent())
	require.Empty(t, w.pve.authorizations())
}

// slowAPI is a Cloudflare whose token check does not answer before the request
// that asked is over.
type slowAPI struct {
	cfapi.API
	asked chan struct{}
	once  *sync.Once
}

func (a slowAPI) VerifyToken(ctx context.Context) (cfapi.TokenStatus, error) {
	a.once.Do(func() { close(a.asked) })
	<-ctx.Done()
	return cfapi.TokenStatus{}, ctx.Err()
}

const slowToken = "slow-token-0123456789-abcdefgh"

func TestAStopGivesARequestInFlightTimeToFinishAndThenCutsItOffCleanly(t *testing.T) {
	w := newWorld(t)
	asked := make(chan struct{})
	slow := slowAPI{API: w.cf, asked: asked, once: &sync.Once{}}
	w.deps.NewClient = func(c store.Credential) (cfapi.API, error) {
		if c.Token.Reveal() == slowToken {
			return slow, nil
		}
		return w.cf, nil
	}
	w.deps.ShutdownTimeout = 20 * time.Millisecond
	d := w.start()
	added := make(chan error, 1)
	go func() {
		_, err := d.client.AddCredential(t.Context(), "slow", slowToken)
		added <- err
	}()
	<-asked

	require.NoError(t, d.stopWithin(3*time.Second), "running into the limit is a clean stop")

	require.Error(t, <-added, "the request was cut off")
	require.Contains(t, w.logs.String(), "they were cut off")
	require.Contains(t, w.logs.String(), `"level":"warn"`)
}

func TestAStopWaitsThirtySecondsForRequestsUnlessToldOtherwise(t *testing.T) {
	require.Equal(t, 30*time.Second, Deps{}.withDefaults().ShutdownTimeout)
	require.Equal(t, time.Second, Deps{ShutdownTimeout: time.Second}.withDefaults().ShutdownTimeout)
}
