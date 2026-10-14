package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

// promoteDaemon answers a promotion as the daemon does for root, and keeps
// the bodies it was asked with.
type promoteDaemon struct {
	mu    sync.Mutex
	asked []string
	diffs string // the differences it answers; one of an agent's epoch when empty
	// rotates answers a promotion of a restored install.
	rotates bool
}

func (d *promoteDaemon) bodies() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.asked...)
}

func servePromote(t *testing.T, d *promoteDaemon) string {
	t.Helper()
	socket := filepath.Join(testutil.ShortDir(t), "pco.sock")
	ln, err := net.Listen("unix", socket)
	require.NoError(t, err)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method+" "+r.URL.Path != "POST /v1/promote" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"no such route","code":"no_route"}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.asked = append(d.asked, string(body))
		d.mu.Unlock()
		diffs := d.diffs
		if diffs == "" {
			diffs = `[{"subject":"epoch the agent of pve1 remembers","store":"3.abc","reported":"4.def"}]`
		}
		if strings.Contains(string(body), `"confirm":true`) {
			rotates := ""
			if d.rotates {
				rotates = `,"rotatesSecrets":true`
			}
			_, _ = io.WriteString(w, `{"differences":`+diffs+`,"drawn":true,"generation":5,"nonce":"drawn"`+rotates+`}`)
			return
		}
		_, _ = io.WriteString(w, `{"differences":`+diffs+`,"drawn":false}`)
	}))
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	return socket
}

func TestPromotePrintsTheDifferencesAndAsks(t *testing.T) {
	for _, tt := range []struct {
		name     string
		in       string
		args     []string
		promoted bool
	}{
		{"yes", "y\n", nil, true},
		{"no", "\n", nil, false},
		{"--yes asks nothing", "", []string{"--yes"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := &promoteDaemon{}
			r := newRunner(t, servePromote(t, d))

			res := r.tty().run(tt.in, append([]string{"promote"}, tt.args...)...)

			require.Contains(t, res.out, "The store and what the agents and Cloudflare report differ:\n"+
				"  - epoch the agent of pve1 remembers: the store says 3.abc, reported 4.def\n")
			if !tt.promoted {
				require.ErrorIs(t, res.err, errAborted)
				require.Equal(t, []string{`{}`}, d.bodies(), "the differences only")
				return
			}
			require.NoError(t, res.err)
			require.Equal(t, []string{`{}`, `{"confirm":true,"shown":[{"subject":"epoch the agent of pve1 remembers","store":"3.abc","reported":"4.def"}]}`}, d.bodies(), "with what it was shown")
			require.Contains(t, res.out, "This node is the writer at generation 5. pco is in observe-only mode:\n"+
				"pco plan shows what it would change, and pco apply lets it.\n")
			if len(tt.args) == 0 {
				require.Contains(t, res.errOut, "Draw a new epoch and make this node the writer? [y/N] ")
			}
		})
	}
}

// A daemon with the test hooks on says so in every state; without them the
// file of the hook means nothing and no line says anything.
func TestTheDaemonCarriesTheTestHooksAsAProblem(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"on":            {"PCO_TEST_HOOKS": "1"},
		"anything else": {"PCO_TEST_HOOKS": "yes"},
	} {
		t.Run(name, func(t *testing.T) {
			d := startOverriddenDaemon(t, vars)
			var st engine.State
			require.Eventually(t, func() bool {
				var err error
				st, err = d.client.Status(t.Context())
				return err == nil && !st.FinishedAt.IsZero()
			}, 30*time.Second, 10*time.Millisecond)

			require.Equal(t, vars["PCO_TEST_HOOKS"] == "1", containsLine(st, testHooksLine))
		})
	}
}

func TestPromoteWithoutATerminalWantsYes(t *testing.T) {
	d := &promoteDaemon{}
	r := newRunner(t, servePromote(t, d))

	res := r.run("", "promote")

	require.ErrorIs(t, res.err, errNoTerminal)
	require.Equal(t, []string{`{}`}, d.bodies(), "nothing drawn")
}

// pco promote passes --force through, for a store that agrees with the
// agents.
// The promotion of a restored install says that every tunnel gets a new
// secret once pco may write; any other says nothing of it.
func TestPromoteOfARestoredInstallSaysTheTunnelsGetNewSecrets(t *testing.T) {
	const said = "The install was restored from a bundle: once pco apply lets it write, every tunnel of it gets\n" +
		"a new secret and every connection of it is ended, so that the run tokens of the lost install\n" +
		"stop working.\n"
	d := &promoteDaemon{rotates: true}
	res := newRunner(t, servePromote(t, d)).run("", "promote", "--yes")
	require.NoError(t, res.err)
	require.Contains(t, res.out, said)

	res = newRunner(t, servePromote(t, &promoteDaemon{})).run("", "promote", "--yes")
	require.NoError(t, res.err)
	require.NotContains(t, res.out, "restored from a bundle")
}

func TestPromoteForce(t *testing.T) {
	d := &promoteDaemon{diffs: `[]`}
	r := newRunner(t, servePromote(t, d))

	res := r.run("", "promote", "--yes", "--force")

	require.NoError(t, res.err)
	require.Contains(t, res.out, "The store and what the agents and Cloudflare report agree.")
	require.Equal(t, []string{`{}`, `{"confirm":true,"force":true}`}, d.bodies())
}

// A store made in another cluster is promoted only once the admin typed the
// name of that cluster, or its fingerprint, which is the confirmation; a
// script names it with --cluster.
func TestPromoteOfAnotherClustersStoreAsksForItsName(t *testing.T) {
	const other = `[{"subject":"cluster","store":"old-lab (aaaa)","reported":"lab (bbbb)"}]`
	t.Run("typed", func(t *testing.T) {
		d := &promoteDaemon{diffs: other}
		r := newRunner(t, servePromote(t, d))

		res := r.tty().run("old-lab\n", "promote")

		require.NoError(t, res.err)
		require.Contains(t, res.errOut, "The store was made in another cluster. Type the name of that cluster, or its fingerprint "+
			"when it has none, as shown above, to make this cluster its writer: ")
		require.Equal(t, []string{`{}`, `{"confirm":true,"cluster":"old-lab","shown":[{"subject":"cluster","store":"old-lab (aaaa)","reported":"lab (bbbb)"}]}`}, d.bodies())
	})
	t.Run("nothing typed", func(t *testing.T) {
		d := &promoteDaemon{diffs: other}
		r := newRunner(t, servePromote(t, d))

		res := r.tty().run("\n", "promote")

		require.ErrorIs(t, res.err, errAborted)
		require.Equal(t, []string{`{}`}, d.bodies())
	})
	t.Run("a script", func(t *testing.T) {
		d := &promoteDaemon{diffs: other}
		r := newRunner(t, servePromote(t, d))

		res := r.run("", "promote", "--yes")
		require.EqualError(t, res.err, "the store was made in another cluster: name it with --cluster, as the differences show it")

		res = r.run("", "promote", "--yes", "--cluster", "old-lab")
		require.NoError(t, res.err)
		require.Equal(t, []string{`{}`, `{}`, `{"confirm":true,"cluster":"old-lab","shown":[{"subject":"cluster","store":"old-lab (aaaa)","reported":"lab (bbbb)"}]}`}, d.bodies())
	})
}
