package doctor

import (
	"context"
	"errors"
	"net"
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
	// Not the deadline under test: a busy machine may take its time.
	env.Timeout = time.Minute
	env.Binary = fakeBinary(t, `[ "$1" = "--version" ] || exit 3
echo "cloudflared version 2026.9.0 (built 2026-09-10-1200 UTC)"
echo "second line"`)

	v, err := env.CloudflaredVersion(t.Context())

	require.NoError(t, err)
	require.Equal(t, "cloudflared version 2026.9.0 (built 2026-09-10-1200 UTC)", v)
}

// cloudflared --version gets the deadline of the host, which reads as a
// timeout, and a child that keeps its output open does not hold the call.
func TestCloudflaredIsAskedWithinADeadline(t *testing.T) {
	t.Run("a cloudflared that does not answer", func(t *testing.T) {
		env, _, _ := hostEnv(t)
		env.Timeout = 100 * time.Millisecond
		env.Binary = fakeBinary(t, "exec sleep 5")

		_, err := env.CloudflaredVersion(t.Context())

		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
	t.Run("a child that keeps the output open", func(t *testing.T) {
		// The script gets five seconds to start and end, however busy the
		// machine; its child would hold the output for twenty.
		env, _, _ := hostEnv(t)
		env.Timeout = 5 * time.Second
		env.Binary = fakeBinary(t, `sleep 20 &
echo "cloudflared version 2026.9.0"`)
		start := time.Now()

		v, err := env.CloudflaredVersion(t.Context())

		require.NoError(t, err)
		require.Equal(t, "cloudflared version 2026.9.0", v)
		require.Less(t, time.Since(start), 15*time.Second, "the call did not wait for the child")
	})
	t.Run("an answer that does not end", func(t *testing.T) {
		env, _, _ := hostEnv(t)
		env.Binary = fakeBinary(t, `i=0
while [ $i -lt 100 ]; do printf '%0100d' 0; i=$((i+1)); done
echo`)

		v, err := env.CloudflaredVersion(t.Context())

		require.NoError(t, err)
		require.Len(t, v, maxVersionOutput)
	})
}

// A store or a lock that does not answer, as on a stalled cluster filesystem,
// does not hold the doctor.
func TestTheChecksOfTheDaemonHaveADeadline(t *testing.T) {
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	env, _, _ := hostEnv(t)
	env.Timeout = 50 * time.Millisecond
	env.StoreCheck = func() error { <-stuck; return nil }
	env.LockCheck = func() error { <-stuck; return nil }

	require.ErrorIs(t, env.Store(t.Context()), context.DeadlineExceeded)
	require.ErrorIs(t, env.NodeLock(t.Context()), context.DeadlineExceeded)
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

	var deadline time.Time
	env.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		deadline, _ = ctx.Deadline()
		return nil, errors.New("dialed " + network + " " + addr)
	}
	require.EqualError(t, env.CanDial(t.Context(), "tcp", "region1.v2.argotunnel.com:7844"), "dialed tcp region1.v2.argotunnel.com:7844")
	require.False(t, deadline.IsZero())
	require.LessOrEqual(t, time.Until(deadline), hostTimeout)
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
