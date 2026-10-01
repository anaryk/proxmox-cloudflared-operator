package connector

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeSystemctl stands in for /usr/bin/systemctl. It appends its arguments to
// a log next to itself and answers by command and unit name.
const fakeSystemctl = `#!/bin/sh
printf '%s\n' "$*" >> "$0.log"
for last; do :; done
case "$1" in
is-active)
	case "$last" in
	up.service) echo active ;;
	down.service) echo inactive; exit 3 ;;
	starting.service) echo activating; exit 3 ;;
	failed.service) echo failed; exit 3 ;;
	block.service) echo started > "$0.ready"; exec sleep 30 ;;
	*) echo "  unit exploded  " >&2; exit 1 ;;
	esac ;;
list-units)
	printf '%s\n' 'pco-cloudflared@one.service loaded active running pco cloudflared connector' '' 'pco-cloudflared@two.service loaded inactive dead pco cloudflared connector'
	;;
*)
	if [ "$last" = bad.service ]; then
		echo "  Failed to act on bad.service  " >&2
		exit 1
	fi
	;;
esac
`

func newFakeSystemctl(t *testing.T) (ctl systemctl, calls func() []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "systemctl")
	require.NoError(t, os.WriteFile(path, []byte(fakeSystemctl), 0o755))
	return systemctl{bin: path}, func() []string {
		b, err := os.ReadFile(path + ".log")
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		require.NoError(t, err)
		return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
}

func TestNewSystemctlRunsTheSystemBinary(t *testing.T) {
	require.Equal(t, systemctl{bin: "/usr/bin/systemctl"}, NewSystemctl())
}

func TestSystemctlArguments(t *testing.T) {
	ctl, calls := newFakeSystemctl(t)
	ctx := t.Context()

	require.NoError(t, ctl.EnableNow(ctx, "up.service"))
	require.NoError(t, ctl.DisableNow(ctx, "up.service"))
	require.NoError(t, ctl.Restart(ctx, "up.service"))
	_, err := ctl.IsActive(ctx, "up.service")
	require.NoError(t, err)
	_, err = ctl.ListUnits(ctx, "pco-cloudflared@*.service")
	require.NoError(t, err)

	require.Equal(t, []string{
		"enable --now --no-block -- up.service",
		"disable --now --no-block -- up.service",
		"restart --no-block -- up.service",
		"is-active -- up.service",
		"list-units --all --plain --no-legend -- pco-cloudflared@*.service",
	}, calls())
}

func TestSystemctlIsActive(t *testing.T) {
	tests := []struct {
		unit string
		want bool
	}{
		{"up.service", true},
		{"down.service", false},
		{"failed.service", false},
		{"starting.service", true},
	}
	for _, tc := range tests {
		t.Run(tc.unit, func(t *testing.T) {
			ctl, _ := newFakeSystemctl(t)

			got, err := ctl.IsActive(t.Context(), tc.unit)

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestSystemctlIsActiveFailsOnOtherExitCodes(t *testing.T) {
	ctl, _ := newFakeSystemctl(t)

	got, err := ctl.IsActive(t.Context(), "unknown.service")

	require.False(t, got)
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, 1, exit.ExitCode())
	require.ErrorContains(t, err, "systemctl is-active -- unknown.service")
	require.ErrorContains(t, err, ": unit exploded", "stderr is part of the error, trimmed")
	require.False(t, strings.HasSuffix(err.Error(), " "))
}

func TestSystemctlFailuresCarryStderr(t *testing.T) {
	ctl, _ := newFakeSystemctl(t)
	ctx := t.Context()

	for name, err := range map[string]error{
		"enable":  ctl.EnableNow(ctx, "bad.service"),
		"disable": ctl.DisableNow(ctx, "bad.service"),
		"restart": ctl.Restart(ctx, "bad.service"),
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, err, "systemctl "+name)
			require.ErrorContains(t, err, ": Failed to act on bad.service")
			require.False(t, strings.HasSuffix(err.Error(), " "))
		})
	}
}

func TestSystemctlListUnitsReturnsTheFirstColumn(t *testing.T) {
	ctl, _ := newFakeSystemctl(t)

	got, err := ctl.ListUnits(t.Context(), "pco-cloudflared@*.service")

	require.NoError(t, err)
	require.Equal(t, []string{"pco-cloudflared@one.service", "pco-cloudflared@two.service"}, got)
}

func TestSystemctlWithoutABinaryFails(t *testing.T) {
	ctl := systemctl{bin: filepath.Join(t.TempDir(), "missing")}

	active, err := ctl.IsActive(t.Context(), "up.service")

	require.Error(t, err)
	require.False(t, active, "a binary that cannot run does not say the unit is stopped")
}

func TestSystemctlStopsWhenTheContextIsCancelled(t *testing.T) {
	ctl, calls := newFakeSystemctl(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	active, err := ctl.IsActive(ctx, "down.service")

	require.Error(t, err)
	require.False(t, active)
	require.Empty(t, calls())
}

func TestSystemctlStopsWhenTheContextIsCancelledWhileItRuns(t *testing.T) {
	ctl, _ := newFakeSystemctl(t)
	ready := ctl.bin + ".ready" // a fifo, so that the test knows when the script runs
	require.NoError(t, syscall.Mkfifo(ready, 0o600))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	started := make(chan struct{})
	go func() {
		defer close(started)
		if f, err := os.Open(ready); err == nil { // blocks until the script opens it
			_, _ = io.Copy(io.Discard, f)
			_ = f.Close()
		}
	}()
	type result struct {
		active bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		active, err := ctl.IsActive(ctx, "block.service")
		done <- result{active, err}
	}()

	select {
	case <-started:
		cancel()
	case r := <-done:
		t.Fatalf("the script returned before it was cancelled: %+v", r)
	}
	select {
	case r := <-done:
		require.Error(t, r.err)
		require.False(t, r.active)
	case <-time.After(10 * time.Second):
		t.Fatal("IsActive did not return after its context was cancelled")
	}
}

// unitFile is the parsed content of a systemd unit: values by section and key,
// in the order they appear.
type unitFile map[string]map[string][]string

func parseUnitFile(t *testing.T, path string) unitFile {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	u := unitFile{}
	section := ""
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = line[1 : len(line)-1]
			u[section] = map[string][]string{}
		default:
			key, value, ok := strings.Cut(line, "=")
			require.True(t, ok, "line %q is no assignment", line)
			require.NotEmpty(t, section, "assignment before a section")
			u[section][key] = append(u[section][key], value)
		}
	}
	return u
}

func TestUnitFileMatchesWhatTheManagerWrites(t *testing.T) {
	u := parseUnitFile(t, filepath.Join("..", "..", "packaging", "systemd", "pco-cloudflared@.service"))

	require.Equal(t, []string{"yes"}, u["Service"]["DynamicUser"])
	require.Equal(t, []string{"notify"}, u["Service"]["Type"])
	require.Equal(t, []string{"token:/var/lib/pco/tunnels/%i.token"}, u["Service"]["LoadCredential"])
	require.Equal(t, []string{"/var/lib/pco/tunnels/%i.env"}, u["Service"]["EnvironmentFile"])
	require.Len(t, u["Service"]["ExecStart"], 1)
	require.Contains(t, u["Service"]["ExecStart"][0], "--token-file %d/token")
	require.Contains(t, u["Service"]["ExecStart"][0], "--metrics ${"+metricsKey+"}")
	require.Equal(t, []string{"multi-user.target"}, u["Install"]["WantedBy"])

	// The unit reads the files under the names the manager gives them.
	require.True(t, strings.HasSuffix(u["Service"]["LoadCredential"][0], "/"+tokenFile("%i")))
	require.True(t, strings.HasSuffix(u["Service"]["EnvironmentFile"][0], "/"+envFile("%i")))
}
