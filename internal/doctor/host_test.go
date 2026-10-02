package doctor

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// fakeSystemd answers IsActive and remembers the deadline it was given.
type fakeSystemd struct {
	active   bool
	deadline time.Time
	unit     string
}

var errNotHere = errors.New("not in this test")

func (f *fakeSystemd) EnableNow(context.Context, string) error             { return errNotHere }
func (f *fakeSystemd) DisableNow(context.Context, string) error            { return errNotHere }
func (f *fakeSystemd) Restart(context.Context, string) error               { return errNotHere }
func (f *fakeSystemd) ListUnits(context.Context, string) ([]string, error) { return nil, errNotHere }

func (f *fakeSystemd) IsActive(ctx context.Context, unit string) (bool, error) {
	f.deadline, _ = ctx.Deadline()
	f.unit = unit
	return f.active, nil
}

type fakeProxmox struct {
	deadline time.Time
	err      error
}

func (f *fakeProxmox) Version(ctx context.Context) (pve.Version, error) {
	f.deadline, _ = ctx.Deadline()
	return pve.Version{Release: "9.0", Major: 9}, f.err
}

func hostEnv(t *testing.T) (*HostEnv, *fakeSystemd, *fakeProxmox) {
	t.Helper()
	sd, px := &fakeSystemd{active: true}, &fakeProxmox{}
	return &HostEnv{
		Systemd:    sd,
		Proxmox:    px,
		Interval:   func() time.Duration { return 10 * time.Second },
		Clock:      func() time.Time { return now },
		StoreCheck: func() error { return nil },
		LockCheck:  func() error { return errors.New("the lock file was replaced") },
		Binary:     filepath.Join(t.TempDir(), "missing"),
	}, sd, px
}

// fakeBinary writes an executable that prints out and exits with code.
func fakeBinary(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloudflared")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700))
	return path
}

func TestTheHostTellsTheVersionOfCloudflared(t *testing.T) {
	env, _, _ := hostEnv(t)
	env.Binary = fakeBinary(t, `[ "$1" = "--version" ] || exit 3
echo "cloudflared version 2026.9.0 (built 2026-09-10-1200 UTC)"
echo "second line"`)

	v, err := env.CloudflaredVersion(t.Context())

	require.NoError(t, err)
	require.Equal(t, "cloudflared version 2026.9.0 (built 2026-09-10-1200 UTC)", v)
}

func TestACloudflaredThatDoesNotRun(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		env, _, _ := hostEnv(t)

		_, err := env.CloudflaredVersion(t.Context())

		require.Error(t, err)
	})
	t.Run("failing", func(t *testing.T) {
		env, _, _ := hostEnv(t)
		env.Binary = fakeBinary(t, "exit 1")

		_, err := env.CloudflaredVersion(t.Context())

		require.Error(t, err)
	})
}

func TestTheHostAsksWithADeadline(t *testing.T) {
	env, sd, px := hostEnv(t)

	active, err := env.UnitActive(t.Context(), "pco-cloudflared@x.service")
	require.NoError(t, err)
	require.True(t, active)
	require.Equal(t, "pco-cloudflared@x.service", sd.unit)
	release, err := env.PVEVersion(t.Context())
	require.NoError(t, err)
	require.Equal(t, "9.0", release)

	for _, d := range []time.Time{sd.deadline, px.deadline} {
		require.False(t, d.IsZero())
		require.LessOrEqual(t, time.Until(d), hostTimeout)
	}
	require.Equal(t, 5*time.Second, hostTimeout)
}

func TestTheHostDials(t *testing.T) {
	env, _, _ := hostEnv(t)
	srv := httptest.NewServer(nil)
	addr := srv.Listener.Addr().String()

	require.NoError(t, env.CanDial(t.Context(), "tcp", addr))
	srv.Close()
	require.Error(t, env.CanDial(t.Context(), "tcp", addr))
}

func TestTheHostPassesOnWhatTheDaemonKnows(t *testing.T) {
	env, _, px := hostEnv(t)
	px.err = errors.New("proxmox api: HTTP 401")

	_, err := env.PVEVersion(t.Context())
	require.EqualError(t, err, "proxmox api: HTTP 401")
	require.NoError(t, env.Store(t.Context()))
	require.EqualError(t, env.NodeLock(t.Context()), "the lock file was replaced")
	require.Equal(t, 10*time.Second, env.PollInterval())
	require.Equal(t, now, env.Now())
}
