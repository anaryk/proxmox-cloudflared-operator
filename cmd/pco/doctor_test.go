package main

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// failingSteps is a diagnosis that stops at the connector.
func failingSteps() []doctor.Step {
	return []doctor.Step{
		{Name: "route", Level: doctor.LevelOK, Detail: "qemu/101 (web-1) holds it; state active"},
		{Name: "zone", Level: doctor.LevelOK, Detail: "zone example.com is active"},
		{Name: "dns", Level: doctor.LevelOK, Detail: "its record points at the tunnel"},
		{Name: "ingress", Level: doctor.LevelOK, Detail: "tunnel pco-abc123 sends it to http://10.0.0.11:8080 (configuration version 3)"},
		{Name: "connector", Level: doctor.LevelFail, Detail: "the connector of tunnel pco-abc123 is not connected to Cloudflare"},
		{Name: "identity", Level: doctor.LevelWarn, Detail: "skipped"},
		{Name: "tcp", Level: doctor.LevelWarn, Detail: "skipped"},
		{Name: "http", Level: doctor.LevelWarn, Detail: "skipped"},
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
		{Check: "credential cred1", Level: doctor.LevelWarn, Detail: "not checked since the daemon started", Fix: "pco credential check cred1"},
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
