package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

// failingSteps is a diagnosis that stops at the connector.
func failingSteps() []doctor.Step {
	return []doctor.Step{
		{Name: "route", Level: doctor.LevelOK, Detail: "qemu/101 (web-1) holds it; state active"},
		{Name: "zone", Level: doctor.LevelOK, Detail: "zone example.com is active"},
		{Name: "dns", Level: doctor.LevelOK, Detail: "its record points at the tunnel"},
		{Name: "ingress", Level: doctor.LevelOK, Detail: "tunnel pco-abc123 sends it to http://10.0.0.11:8080 (configuration version 3)"},
		{Name: "connector", Level: doctor.LevelFail, Detail: "the connector of tunnel pco-abc123 is not connected to Cloudflare"},
		{Name: "identity", Level: doctor.LevelWarn, Detail: "skipped", Skipped: true},
		{Name: "tcp", Level: doctor.LevelWarn, Detail: "skipped", Skipped: true},
		{Name: "http", Level: doctor.LevelWarn, Detail: "skipped", Skipped: true},
	}
}

func passingSteps() []doctor.Step {
	steps := failingSteps()[:4]
	return append(steps,
		doctor.Step{Name: "connector", Level: doctor.LevelOK, Detail: "active, ready, 4 connections"},
		doctor.Step{Name: "identity", Level: doctor.LevelOK, Detail: "10.0.0.11 is the address of qemu/101, verified at identity level port (static)"},
		doctor.Step{Name: "tcp", Level: doctor.LevelOK, Detail: "10.0.0.11:8080 answers"},
		doctor.Step{Name: "http", Level: doctor.LevelWarn, Detail: "the origin answered 502 Bad Gateway"},
	)
}

func TestDiagnoseGolden(t *testing.T) {
	e := &fakeEngine{state: healthyState(), steps: failingSteps()}
	r := newRunner(t, serveFake(t, e))

	res := r.run("", "diagnose", "www.example.com")

	require.ErrorIs(t, res.err, errReported, "a step that fails is an exit status of 1")
	require.Empty(t, res.errOut)
	requireGolden(t, "diagnose.golden", res.out)
}

func TestADiagnosisWithoutAFailureExitsWithZero(t *testing.T) {
	e := &fakeEngine{state: healthyState(), steps: passingSteps()}
	r := newRunner(t, serveFake(t, e))

	res := r.run("", "diagnose", "www.example.com")

	require.NoError(t, res.err)
	require.Contains(t, res.out, "! http       the origin answered 502 Bad Gateway\n")
}

func TestDiagnoseOfAHostnameWithoutARoute(t *testing.T) {
	e := &fakeEngine{state: healthyState(), diagnoseErr: fmt.Errorf("%w: the last cycle has no route for nope.example.com", engine.ErrNotFound)}
	r := newRunner(t, serveFake(t, e))

	res := r.run("", "diagnose", "nope.example.com")

	require.EqualError(t, res.err, "not found: the last cycle has no route for nope.example.com")
	require.Empty(t, res.out)
}

func TestDiagnoseNeedsOneHostname(t *testing.T) {
	r, _ := daemonWith(t, healthyState())

	require.Error(t, r.run("", "diagnose").err)
	require.Error(t, r.run("", "diagnose", "a.example.com", "b.example.com").err)
}

func TestDiagnoseJSONIsWhatTheDaemonSent(t *testing.T) {
	const raw = `[{"level":"fail","name":"route","detail":"x","futureField":1}]`
	r := newRunner(t, serveRaw(t, map[string]rawReply{"GET /v1/diagnose": {200, raw}}))

	res := r.run("", "--json", "diagnose", "www.example.com")

	require.ErrorIs(t, res.err, errReported, "the exit status still says that a step failed")
	require.Equal(t, indented(t, raw), res.out)
}

func someFindings() []doctor.Finding {
	return []doctor.Finding{
		{Check: "cloudflared", Level: doctor.LevelFail, Detail: "cloudflared does not run: exec: no such file", Fix: "install cloudflared from the package repository of Cloudflare"},
		{Check: "credential cred1", Level: doctor.LevelWarn, Detail: "not checked yet", Fix: "pco credential check cred1"},
		{Check: "mode", Level: doctor.LevelOK, Detail: "enforce: changes are applied"},
		{Check: "store", Level: doctor.LevelOK, Detail: "the store is mounted and set up"},
	}
}

func TestDoctorGolden(t *testing.T) {
	e := &fakeEngine{state: healthyState(), findings: someFindings()}
	r := newRunner(t, serveFake(t, e))

	res := r.run("", "doctor")

	require.ErrorIs(t, res.err, errReported, "a failed check is an exit status of 1")
	require.Empty(t, res.errOut)
	requireGolden(t, "doctor.golden", res.out)
}

func TestADoctorWithWarningsOnlyExitsWithZero(t *testing.T) {
	e := &fakeEngine{state: healthyState(), findings: someFindings()[1:]}
	r := newRunner(t, serveFake(t, e))

	res := r.run("", "doctor")

	require.NoError(t, res.err)
	require.Contains(t, res.out, "1 warning, no failure.\n")
}

func TestDoctorJSONIsWhatTheDaemonSent(t *testing.T) {
	const raw = `[{"level":"ok","check":"mode","detail":"enforce"}]`
	r := newRunner(t, serveRaw(t, map[string]rawReply{"GET /v1/doctor": {200, raw}}))

	res := r.run("", "--json", "doctor")

	require.NoError(t, res.err)
	require.Equal(t, indented(t, raw), res.out)
}

// fakeNode is a node whose daemon is not running, as the doctor reads it: it
// answers from its fields and counts the questions.
type fakeNode struct {
	version   string
	stopped   map[string]bool // units that do not run
	enabled   map[string]bool // units that start at boot
	storeErr  error
	egress    engine.EgressView
	egressErr error
	questions int
}

func healthyNode() *fakeNode {
	return &fakeNode{
		version: "cloudflared version 2026.9.0 (built 2026-09-10-1200 UTC)",
		enabled: map[string]bool{"pco.service": true, "pco-egress.service": true},
		egress:  engine.EgressView{State: engine.EgressOn},
	}
}

func (n *fakeNode) CloudflaredVersion(context.Context) (string, error) {
	n.questions++
	return n.version, nil
}

func (n *fakeNode) UnitActive(_ context.Context, unit string) (bool, error) {
	n.questions++
	return !n.stopped[unit], nil
}

func (n *fakeNode) UnitEnabled(_ context.Context, unit string) (bool, error) {
	n.questions++
	return n.enabled[unit], nil
}

func (n *fakeNode) Store(context.Context) error {
	n.questions++
	return n.storeErr
}

func (n *fakeNode) Egress(context.Context) (engine.EgressView, error) {
	n.questions++
	return n.egress, n.egressErr
}

func (n *fakeNode) Now() time.Time { return t0 }

// doctorWithout runs pco doctor as euid with the daemon on socket, on node.
func doctorWithout(t *testing.T, socket string, euid int, node doctor.LocalEnv) result {
	t.Helper()
	return doctorOn(t, &app{env: testEnv(), socket: socket}, euid, node)
}

func doctorOn(t *testing.T, a *app, euid int, node doctor.LocalEnv) result {
	t.Helper()
	cmd := a.doctorCmdWith(doctorEnv{euid: func() int { return euid }, node: node})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(unreadable{t})
	cmd.SetArgs([]string{})
	err := cmd.ExecuteContext(t.Context())
	return result{out: out.String(), errOut: errOut.String(), err: err}
}

func noDaemon(t *testing.T) string { return filepath.Join(testutil.ShortDir(t), "pco", "pco.sock") }

func TestDoctorWithoutTheDaemonMakesTheChecksThatNeedNone(t *testing.T) {
	node := healthyNode()
	node.stopped = map[string]bool{"pco.service": true}

	res := doctorWithout(t, noDaemon(t), 0, node)

	require.ErrorIs(t, res.err, errReported, "the unit of the daemon does not run: a failed check is an exit status of 1")
	require.Empty(t, res.errOut)
	requireGolden(t, "doctor_down.golden", res.out)
}

func TestDoctorWithoutTheDaemonExitsByTheWorstFinding(t *testing.T) {
	for _, tt := range []struct {
		name   string
		node   func(n *fakeNode)
		failed bool
		summed string
	}{
		{"a daemon that runs and does not answer", func(*fakeNode) {}, false, "1 warning, no failure.\n"},
		{"a daemon that runs, with a table that is gone", func(n *fakeNode) { n.egress = engine.EgressView{State: engine.EgressNotLoaded} }, true,
			"1 failure, 1 warning.\n"},
		{"a store that is not set up", func(n *fakeNode) { n.storeErr = errors.New("pco is not set up on this node; run pco setup") }, true,
			"1 failure, 1 warning.\n"},
		{"a daemon that does not run", func(n *fakeNode) { n.stopped = map[string]bool{"pco.service": true} }, true, "1 failure, 1 warning.\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := healthyNode()
			tt.node(node)

			res := doctorWithout(t, noDaemon(t), 0, node)

			if tt.failed {
				require.ErrorIs(t, res.err, errReported)
			} else {
				require.NoError(t, res.err)
			}
			require.True(t, strings.HasSuffix(res.out, tt.summed), res.out)
		})
	}
}

func TestDoctorWithoutTheDaemonSaysWhatItDidNotCheck(t *testing.T) {
	res := doctorWithout(t, noDaemon(t), 0, healthyNode())

	require.NoError(t, res.err)
	require.Contains(t, res.out, "the checks that need the daemon were not made: the mode, the last cycle")
}

func TestDoctorWithoutTheDaemonPrintsTheFindingsAsJSON(t *testing.T) {
	stopped := func() *fakeNode {
		node := healthyNode()
		node.stopped = map[string]bool{"pco.service": true}
		return node
	}
	a := &app{env: testEnv(), socket: noDaemon(t), json: true}

	res := doctorOn(t, a, 0, stopped())

	require.ErrorIs(t, res.err, errReported, "the exit status still says that a check failed")
	want, err := json.Marshal(doctor.RunLocal(t.Context(), stopped()))
	require.NoError(t, err)
	require.Equal(t, indented(t, string(want)), res.out)
}

func TestDoctorWithoutTheDaemonAsAnotherUserCannotAsk(t *testing.T) {
	node := healthyNode()
	socket := noDaemon(t)

	res := doctorWithout(t, socket, 1000, node)

	require.EqualError(t, res.err, "cannot reach the pco daemon at "+socket+": is it running?")
	require.Empty(t, res.out)
	require.Zero(t, node.questions, "the node is read as root")
}

func TestDoctorLeavesTheNodeAloneWhenTheDaemonAnswers(t *testing.T) {
	node := healthyNode()
	socket := serveFake(t, &fakeEngine{state: healthyState(), findings: someFindings()[1:]})

	res := doctorWithout(t, socket, 0, node)

	require.NoError(t, res.err)
	require.Contains(t, res.out, "credential cred1")
	require.Zero(t, node.questions, "the daemon makes the checks")
}

func TestDoctorLeavesTheNodeAloneWhenTheSocketRefusesOrTheDaemonIsUnsure(t *testing.T) {
	for _, tt := range []struct {
		name  string
		reply rawReply
		says  string
	}{
		{"a socket that refuses the peer", rawReply{http.StatusForbidden, `{"error":"not allowed","code":"forbidden"}`}, "run as root"},
		{"a daemon that gave up", rawReply{http.StatusServiceUnavailable, `{"error":"a cycle took too long","code":"unavailable"}`}, "a cycle took too long"},
		{"a daemon of another version", rawReply{http.StatusNotFound, `{"error":"no such route","code":"no_route"}`}, "different versions"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := healthyNode()
			socket := serveRaw(t, map[string]rawReply{"GET /v1/doctor": tt.reply, "GET /v1/version": {http.StatusNotFound, `{}`}})

			res := doctorWithout(t, socket, 0, node)

			require.ErrorContains(t, res.err, tt.says)
			require.Zero(t, node.questions)
		})
	}
}

func TestTheHelpOfDoctorSaysWhatItDoesWithoutTheDaemon(t *testing.T) {
	r := newRunner(t, "/nonexistent/pco/pco.sock")

	res := r.run("", "doctor", "--help")

	require.NoError(t, res.err)
	help := strings.Join(strings.Fields(res.out), " ")
	require.Contains(t, help, "When the daemon is not running, root still gets the checks that need no daemon: the units of pco, the store, cloudflared and the egress table.")
	require.Contains(t, help, "The rest is said not to have been made")
}

// The egress filter as the node has it, read without the daemon, is what pco
// egress show finds.
func TestTheEgressTableOfANodeWithoutTheDaemon(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, r *egressRig)
		state string
		err   error
	}{
		{"a table that is not loaded", func(*testing.T, *egressRig) {}, engine.EgressNotLoaded, nil},
		{"a table as pco loads it", func(t *testing.T, r *egressRig) {
			r.nft.listErr, r.nft.live = nil, liveTable(t)
		}, engine.EgressOn, nil},
		{"a table that is dormant", func(t *testing.T, r *egressRig) {
			r.nft.listErr = nil
			r.nft.live = strings.Replace(liveTable(t), `"name": "pco_egress", "handle": 3}`, `"name": "pco_egress", "handle": 3, "flags": "dormant"}`, 1)
		}, engine.EgressChanged, nil},
		{"a listing that cannot be read", func(_ *testing.T, r *egressRig) {
			r.nft.listErr, r.nft.live = nil, "garbage"
		}, engine.EgressChanged, nil},
		{"a filter that is switched off", func(t *testing.T, r *egressRig) {
			require.NoError(t, r.overrides().SwitchOff(t0))
		}, engine.EgressOff, nil},
		{"no connector user", func(_ *testing.T, r *egressRig) {
			r.env.uid = func() (uint32, error) { return 0, egress.ErrNoConnectorUser }
		}, "", egress.ErrNoConnectorUser},
		{"a listing that fails", func(_ *testing.T, r *egressRig) {
			r.nft.listErr = errors.New("nft: Operation not permitted")
		}, "", errors.New("Operation not permitted")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newEgressRig(t)
			tt.setup(t, r)

			view, err := localNode{egress: r.env}.Egress(t.Context())

			if tt.err != nil {
				require.ErrorContains(t, err, tt.err.Error())
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.state, view.State)
			if tt.state == engine.EgressOff {
				require.True(t, view.Since.Equal(t0), "%s", view.Since)
			}
		})
	}
}

// The doctor looks at the node and changes nothing on it: a node that was
// never set up stays without a store, and the roots of one that was are left
// as they are.
func TestTheDoctorChecksTheStoreWithoutMakingIt(t *testing.T) {
	base := t.TempDir()
	p := store.Paths{
		Cluster: filepath.Join(base, "cluster"),
		Private: filepath.Join(base, "private"),
		Local:   filepath.Join(base, "var", "lib", "pco"),
	}

	err := storeReadyAt(p)

	require.ErrorIs(t, err, store.ErrNoRoot)
	entries, err := os.ReadDir(base)
	require.NoError(t, err)
	require.Empty(t, entries, "nothing was made")

	st, err := store.Open(p)
	require.NoError(t, err)
	require.NoError(t, st.Init())
	require.ErrorContains(t, storeReadyAt(p), "run pco setup")
	require.NoError(t, st.SaveInstall(store.Install{ID: "0123456789ab", CreatedAt: t0}))
	old := time.Now().Add(-time.Hour)
	leftover := filepath.Join(p.Local, "bindings", ".a.json.1.tmp")
	require.NoError(t, os.MkdirAll(filepath.Dir(leftover), 0o700))
	require.NoError(t, os.WriteFile(leftover, []byte("x"), 0o600))
	require.NoError(t, os.Chtimes(leftover, old, old))

	require.NoError(t, storeReadyAt(p))
	_, err = os.Stat(leftover)
	require.NoError(t, err, "a leftover of a crashed write is the daemon's to remove")
}
