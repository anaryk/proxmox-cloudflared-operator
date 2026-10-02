package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

// logged makes the engine of e log into a buffer, from level on, and returns
// a function that reads the lines written so far and forgets them.
func logged(e *env, level zerolog.Level) func() []map[string]any {
	e.t.Helper()
	var buf testutil.SyncBuffer
	e.log = zerolog.New(&buf).Level(level)
	e.eng = e.newEngine()
	read := 0
	return func() []map[string]any {
		all := buf.String()
		fresh := all[read:]
		read = len(all)
		var out []map[string]any
		for line := range strings.Lines(fresh) {
			var m map[string]any
			require.NoError(e.t, json.Unmarshal([]byte(line), &m), line)
			out = append(out, m)
		}
		return out
	}
}

func withMessage(lines []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["message"] == msg {
			out = append(out, l)
		}
	}
	return out
}

func TestAnEventIsLoggedOnceWithItsFields(t *testing.T) {
	e := newEnv(t)
	lines := logged(e, zerolog.InfoLevel)
	e.enforce()

	e.cycle()

	got := withMessage(lines(), "qemu/101: active")
	require.Len(t, got, 1)
	require.Equal(t, map[string]any{
		"level": "info", "message": "qemu/101: active", "event": "route", "seq": got[0]["seq"],
		"subject": "www.example.com", "route": "www.example.com", "guest": "qemu/101", "account": testAccount,
	}, got[0])

	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Empty(t, withMessage(lines(), "qemu/101: active"), "no change, no line")
}

func TestTheLevelOfAnEventIsTheLevelOfItsLine(t *testing.T) {
	e := newEnv(t)
	lines := logged(e, zerolog.InfoLevel)
	e.enforce()
	e.cycle()
	lines()
	e.res.setUnreachable("www.example.com", "connection refused")

	e.clock.advance(10 * time.Second)
	e.cycle()

	got := withMessage(lines(), "qemu/101: unreachable (connection refused)")
	require.Len(t, got, 1)
	require.Equal(t, "warn", got[0]["level"])
}

func TestAHoldThatStartsAndEndsIsLogged(t *testing.T) {
	e := newEnv(t)
	lines := logged(e, zerolog.InfoLevel)
	e.cycle()
	lines()

	e.inv.set(incomplete("node pve2 did not answer", guest(101, "web-1", "www.example.com -> :8080")))
	e.clock.advance(10 * time.Second)
	e.cycle()
	got := lines()
	holds := withMessage(got, "the cycle holds: "+problemIncomplete)
	require.Len(t, holds, 1)
	require.Equal(t, "warn", holds[0]["level"])
	require.Equal(t, "hold", holds[0]["event"])

	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Empty(t, withMessage(lines(), "the cycle holds: "+problemIncomplete), "a hold that goes on is no new event")

	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080")))
	e.clock.advance(10 * time.Second)
	e.cycle()
	ended := withMessage(lines(), "the cycle no longer holds")
	require.Len(t, ended, 1)
	require.Equal(t, "info", ended[0]["level"])
}

func TestACycleThatChangedSomethingLogsTheCounts(t *testing.T) {
	e := newEnv(t)
	lines := logged(e, zerolog.InfoLevel)
	e.enforce()

	e.cycle()

	done := withMessage(lines(), "cycle changed something")
	require.Len(t, done, 1)
	require.Equal(t, "info", done[0]["level"])
	require.Equal(t, map[string]any{
		"level": "info", "message": "cycle changed something", "mode": "enforce",
		"applied": float64(3), "held": float64(0), "routes": float64(1), "problems": float64(0),
	}, done[0])

	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Empty(t, withMessage(lines(), "cycle changed something"), "an idle cycle says nothing at info level")
}

func TestAProblemEventIsLoggedWithoutASecret(t *testing.T) {
	e := newEnv(t)
	lines := logged(e, zerolog.DebugLevel)
	e.enforce()
	e.conn.ensureErr = func(token string) error { return fmt.Errorf("unit refused %s", token) }

	e.cycle()

	got := withMessage(lines(), "tunnel pco-abc123 in account acc1: starting its connector: unit refused [redacted]")
	require.Len(t, got, 1)
	require.Equal(t, "problem", got[0]["event"])
}
