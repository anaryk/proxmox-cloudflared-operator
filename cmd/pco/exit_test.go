package main

import (
	"bytes"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

// The exit status says how a command fared: 0 all is well, 1 it ran and has
// findings or was refused, 2 it could not ask the daemon.
func TestExitCodes(t *testing.T) {
	for _, tt := range []struct {
		name   string
		runner func(t *testing.T) *runner
		args   []string
		code   int
		stderr string
	}{
		{"a status without problems", func(t *testing.T) *runner { r, _ := daemonWith(t, healthyState()); return r },
			[]string{"status"}, 0, ""},
		{"a status with problems", func(t *testing.T) *runner { r, _ := daemonWith(t, problemState()); return r },
			[]string{"status"}, 1, ""},
		{"a failed doctor check", func(t *testing.T) *runner {
			r, e := daemonWith(t, healthyState())
			e.findings = []doctor.Finding{{Check: "store", Level: doctor.LevelFail, Detail: "not mounted", Fix: "mount it"}}
			return r
		}, []string{"doctor"}, 1, ""},
		{"a failed diagnosis step", func(t *testing.T) *runner {
			r, e := daemonWith(t, healthyState())
			e.steps = []doctor.Step{{Name: "route", Level: doctor.LevelFail, Detail: "nobody holds it"}}
			return r
		}, []string{"diagnose", "www.example.com"}, 1, ""},
		{"a refused request", func(t *testing.T) *runner {
			r, e := daemonWith(t, healthyState())
			e.applyErr = fmt.Errorf("%w: no", engine.ErrRefused)
			return r
		}, []string{"apply"}, 1, "pco: refused: no\n"},
		{"no daemon", func(t *testing.T) *runner {
			return newRunner(t, filepath.Join(testutil.ShortDir(t), "pco", "pco.sock"))
		}, []string{"status"}, 2, "is it running?"},
		{"a socket that refuses the peer", func(t *testing.T) *runner {
			return newRunner(t, serveRaw(t, map[string]rawReply{"GET /v1/state": {http.StatusForbidden, `{"error":"not allowed","code":"forbidden"}`}}))
		}, []string{"status"}, 2, "run as root"},
		{"a daemon that gave up waiting for a cycle", func(t *testing.T) *runner {
			return newRunner(t, serveRaw(t, map[string]rawReply{"POST /v1/apply": {http.StatusServiceUnavailable,
				`{"error":"a cycle is running and took longer than 45s; try again","code":"unavailable"}`}}))
		}, []string{"apply"}, 2, "pco: a cycle is running and took longer than 45s; try again\n"},
		{"an answer that cannot be read", func(t *testing.T) *runner {
			return newRunner(t, serveRaw(t, map[string]rawReply{"GET /v1/state": {http.StatusOK, `{"routes":`}}))
		}, []string{"routes"}, 2, "decoding"},
		{"a status that cannot be read", func(t *testing.T) *runner {
			return newRunner(t, serveRaw(t, map[string]rawReply{"GET /v1/state": {http.StatusOK, `{"at":"yesterday"}`}}))
		}, []string{"status"}, 2, "decoding the state"},
		{"a doctor that cannot be read", func(t *testing.T) *runner {
			return newRunner(t, serveRaw(t, map[string]rawReply{"GET /v1/doctor": {http.StatusOK, `{}`}}))
		}, []string{"doctor"}, 2, "decoding"},
		{"a daemon of another version", func(t *testing.T) *runner { return newRunner(t, serveRaw(t, nil)) },
			[]string{"status"}, 2, "different versions"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := tt.runner(t).run("", tt.args...)

			var stderr bytes.Buffer
			require.Equal(t, tt.code, exitCode(res.err, &stderr), "%v", res.err)
			require.Contains(t, stderr.String(), tt.stderr)
		})
	}
}

func TestTheHelpSaysWhatTheExitStatusMeans(t *testing.T) {
	r, _ := daemonWith(t, healthyState())

	res := r.run("", "--help")

	require.NoError(t, res.err)
	require.Contains(t, res.out, "Exit status:\n")
	for _, line := range []string{
		"  0  all is well",
		"  1  the command ran and found something to look at",
		"  2  it could not ask the daemon",
	} {
		require.True(t, strings.Contains(res.out, line), "%q in %s", line, res.out)
	}
}
