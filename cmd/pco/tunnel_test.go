package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

const rotatedID = "00000000-0000-4000-8000-000000000001"

// rotationDaemon answers the state and a rotation, as the daemon does for
// root, and keeps the bodies of the rotations it was asked for.
type rotationDaemon struct {
	mu     sync.Mutex
	asked  []string
	status int    // of the answer to a rotation; 200 when zero
	answer string // its body
}

func (d *rotationDaemon) rotations() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.asked...)
}

func serveRotation(t *testing.T, st engine.State, d *rotationDaemon) string {
	t.Helper()
	socket := filepath.Join(testutil.ShortDir(t), "pco.sock")
	ln, err := net.Listen("unix", socket)
	require.NoError(t, err)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/state":
			_, _ = io.WriteString(w, mustJSON(t, st))
		case "POST /v1/tunnels/rotate":
			body, _ := io.ReadAll(r.Body)
			d.mu.Lock()
			d.asked = append(d.asked, string(body))
			status, answer := d.status, d.answer
			d.mu.Unlock()
			if status == 0 {
				status, answer = http.StatusOK, `{"tunnel":"pco-abc123","tunnelId":"`+rotatedID+`","accountId":"acc1"}`
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, answer)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"no such route","code":"no_route"}`)
		}
	}))
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	return socket
}

func rotationState() engine.State {
	return engine.State{
		At: t0, Mode: "enforce", Complete: true, WriterVerdict: "ok",
		Tunnels: []engine.TunnelView{{TunnelState: reconcile.TunnelState{
			AccountID: "acc1", CredentialID: "cred1", Name: "pco-abc123", ID: rotatedID, Version: 3, Exists: true, Verified: true,
		}}},
		RogueConnectors: []engine.RogueConnector{{
			Tunnel: "pco-abc123", TunnelID: rotatedID, Account: "acc1", ID: "attacker-elsewhere", OriginIP: "198.51.100.7", Version: "2026.8.0", Since: t0,
		}},
	}
}

func TestTunnelRotateAsksAndSaysWhatRestarts(t *testing.T) {
	for _, tt := range []struct {
		name    string
		in      string
		args    []string
		rotated bool
	}{
		{"yes", "y\n", nil, true},
		{"no", "\n", nil, false},
		{"--yes asks nothing", "", []string{"--yes"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := &rotationDaemon{}
			r := newRunner(t, serveRotation(t, rotationState(), d))

			res := r.tty().run(tt.in, append([]string{"tunnel", "rotate"}, tt.args...)...)

			require.Contains(t, res.out, "This gives tunnel pco-abc123 ("+rotatedID+") in account acc1 a new secret at Cloudflare and ends\n"+
				"the connections of every connector of it. The connector pco runs on this node restarts with\n"+
				"the new token; any other connector loses its session and cannot connect again.\n")
			require.Contains(t, res.out, "Cloudflare lists connectors on it that pco does not run on this node:\n"+
				"  - attacker-elsewhere from 198.51.100.7 (cloudflared 2026.8.0)\n")
			if !tt.rotated {
				require.ErrorIs(t, res.err, errAborted)
				require.Empty(t, d.rotations())
				return
			}
			require.NoError(t, res.err)
			require.Equal(t, []string{`{"account":"acc1"}`}, d.rotations(), "the account the admin was shown")
			require.Contains(t, res.out, "The secret of tunnel pco-abc123 in account acc1 was rotated; its connector on this node "+
				"restarts with the new token.\nFollow it with pco status.\n")
			if len(tt.args) == 0 {
				require.Contains(t, res.errOut, "Rotate the secret of tunnel pco-abc123? [y/N] ")
			}
		})
	}
}

func TestTunnelRotateWithoutATerminalWantsYes(t *testing.T) {
	d := &rotationDaemon{}
	r := newRunner(t, serveRotation(t, rotationState(), d))

	res := r.run("", "tunnel", "rotate")

	require.ErrorIs(t, res.err, errNoTerminal)
	require.Empty(t, d.rotations())
}

func TestTunnelRotateNamesTheAccount(t *testing.T) {
	st := rotationState()
	st.Tunnels = append(st.Tunnels, engine.TunnelView{TunnelState: reconcile.TunnelState{
		AccountID: "acc2", CredentialID: "cred1", Name: "pco-abc123", ID: "00000000-0000-4000-8000-000000000002", Exists: true, Verified: true,
	}})
	d := &rotationDaemon{}
	r := newRunner(t, serveRotation(t, st, d))

	res := r.run("", "tunnel", "rotate", "--yes")
	require.EqualError(t, res.err, "invalid request: the install has tunnels in accounts acc1 and acc2; name one with --account",
		"what the daemon would answer")

	res = r.run("", "tunnel", "rotate", "--account", "acc9", "--yes")
	require.EqualError(t, res.err, "not found: no tunnel of this install is known in account acc9; pco status lists the tunnels")

	res = r.run("", "tunnel", "rotate", "--account", "acc2", "--yes")
	require.NoError(t, res.err)
	require.Contains(t, res.out, "This gives tunnel pco-abc123 (00000000-0000-4000-8000-000000000002) in account acc2")
	require.NotContains(t, res.out, "Cloudflare lists connectors", "none on this tunnel")
	require.Equal(t, []string{`{"account":"acc2"}`}, d.rotations())
}

func TestTunnelRotateOfATunnelLeftAsItIs(t *testing.T) {
	st := rotationState()
	st.Tunnels[0].Held = "account frozen: zone example.com is no longer listed by credential cred1"
	st.Tunnels[0].LeftAsIs = true
	d := &rotationDaemon{}
	r := newRunner(t, serveRotation(t, st, d))

	res := r.run("", "tunnel", "rotate", "--yes")

	require.EqualError(t, res.err, "refused: tunnel pco-abc123 in account acc1 is left as it is: "+
		"account frozen: zone example.com is no longer listed by credential cred1; nothing was changed")
	require.Empty(t, d.rotations())
}

// A tunnel the last cycle did not check, as when a forged sentinel holds it,
// can be rotated: that is the remedy.
func TestTunnelRotateOfATunnelTheLastCycleDidNotCheck(t *testing.T) {
	st := rotationState()
	st.Tunnels[0].Held, st.Tunnels[0].Unchecked = "not checked in the last cycle: the tunnel run found a foreign writer", true
	d := &rotationDaemon{}
	r := newRunner(t, serveRotation(t, st, d))

	res := r.run("", "tunnel", "rotate", "--yes")

	require.NoError(t, res.err)
	require.Equal(t, []string{`{"account":"acc1"}`}, d.rotations())
}

func TestTunnelRotateIsRootsAlone(t *testing.T) {
	d := &rotationDaemon{status: http.StatusForbidden, answer: `{"error":"only root may rotate the secret of a tunnel","code":"forbidden"}`}
	socket := serveRotation(t, rotationState(), d)
	r := newRunner(t, socket)

	res := r.run("", "tunnel", "rotate", "--yes")

	require.EqualError(t, res.err, "permission denied on "+socket+": run as root")
}

func TestTunnelRotateSaysWhatTheDaemonRefused(t *testing.T) {
	d := &rotationDaemon{status: http.StatusConflict, answer: `{"error":"pco is in observe-only mode and changes nothing at Cloudflare; pco apply ends it","code":"refused"}`}
	r := newRunner(t, serveRotation(t, rotationState(), d))

	res := r.run("", "tunnel", "rotate", "--yes")

	require.EqualError(t, res.err, "pco is in observe-only mode and changes nothing at Cloudflare; pco apply ends it")
	require.ErrorIs(t, res.err, engine.ErrRefused)
}
