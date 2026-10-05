package wire_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/wire"
)

var update = flag.Bool("update", false, "write the golden files of the tests")

var t0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func TestGoldens(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"session", wire.Session{
			User:          "alice@pve",
			Method:        "ticket",
			Role:          "admin",
			CSRF:          "Zm9yLXRoZS10ZXN0cy1vbmx5LW5vdC1hLXJlYWwtdG9rZW4",
			IdleExpiresAt: t0.Add(30 * time.Minute),
			ExpiresAt:     t0.Add(12 * time.Hour),
			Profile:       "host",
			Node:          "pve1",
			NodeZone:      "Europe/Prague",
			Version:       "0.3.0",
		}},
		{"unauthenticated", wire.Unauthenticated{
			Code:    wire.CodeUnauthenticated,
			Methods: []string{"ticket", "token"},
			Ticket:  true,
		}},
		{"unauthenticated_appliance", wire.Unauthenticated{
			Code:    wire.CodeUnauthenticated,
			Methods: []string{"password", "token"},
			Realms:  []string{"pam", "pve"},
		}},
		{"error", wire.Error{Error: "the session has ended", Code: wire.CodeUnauthenticated}},
		{"error_forbidden", wire.Error{Error: "pco needs Sys.Audit on /", Code: wire.CodeForbidden, Missing: "Sys.Audit"}},
		{"error_invalid", wire.Error{Error: "the hostname is not valid", Code: wire.CodeInvalid, Field: "hostname"}},
		{"error_rate_limited", wire.Error{Error: "too many sign-ins, try again in a minute", Code: wire.CodeRateLimited, RetryAfter: 42}},
		{"doctor_counts", wire.DoctorCounts{OK: 12, Warn: 2, Fail: 1, At: t0}},
		{"upstream", wire.Upstream{Up: true, Since: t0.Add(-time.Hour)}},
		{"codes", []string{
			wire.CodeUnauthenticated,
			wire.CodeTicketInvalid,
			wire.CodeProxmoxUnreachable,
			wire.CodeDaemonUnreachable,
			wire.CodeRateLimited,
			wire.CodeForbidden,
			wire.CodeInvalid,
			wire.CodeTooLarge,
			wire.CodeUnsupportedMediaType,
			wire.CodeSecondFactor,
			wire.CodeSecondFactorKey,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.value)
			require.NoError(t, err)
			requireGolden(t, c.name+".json", b)
		})
	}
}

// requireGolden checks JSON, indented, against the golden file name.
func requireGolden(t *testing.T, name string, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, json.Indent(&buf, body, "", "  "))
	buf.WriteByte('\n')
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), buf.String(), "the JSON changed; run the test with -update when that is intended")
}
