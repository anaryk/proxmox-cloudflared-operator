package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// silentCredential is a credential whose check got no answer to its zone
// listing.
func silentCredential(id, label string) engine.CredentialView {
	v := usableCredential(id, label, 365*24*time.Hour)
	v.Report.Usable = false
	v.Report.Checks = []credentials.Check{
		{Capability: credentials.CapToken, OK: true},
		{Capability: credentials.CapAccounts, OK: true},
		{Capability: credentials.CapZones, Detail: "cloudflare api: HTTP 503: unavailable", Unanswered: true},
	}
	return v
}

func TestCredentialCheckSaysCloudflareDidNotAnswerAndNeverToGrantAnything(t *testing.T) {
	r, e := daemonWith(t, credentialsState())
	e.checkView = silentCredential("a1b2c3d4", "main")

	res := r.run("", "credential", "check", "a1b2c3d4")

	require.NoError(t, res.err)
	require.Contains(t, res.out, "  ? zones\n      Cloudflare did not answer (cloudflare api: HTTP 503: unavailable)\n")
	require.Contains(t, res.out, "Usable:    not known, Cloudflare did not answer\n")
	require.NotContains(t, res.out, "grant")
	require.NotContains(t, res.out, "✗")
}

func TestACheckThatFailedForGoodStillSaysNo(t *testing.T) {
	r, e := daemonWith(t, credentialsState())
	e.checkView = failingCredential("c9d0e1f2", "readonly")

	res := r.run("", "credential", "check", "c9d0e1f2")

	require.NoError(t, res.err)
	require.Contains(t, res.out, "  ✗ dns.read on example.com\n      grant Zone > DNS > Read on example.com\n")
	require.Contains(t, res.out, "Usable:    no\n")
}

func TestAUnansweredCredentialIsUnknownInTheListAndNotAProblem(t *testing.T) {
	st := credentialsState()
	st.Credentials = []engine.CredentialView{silentCredential("a1b2c3d4", "main")}
	r, _ := daemonWith(t, st)

	res := r.run("", "credential", "list")

	require.NoError(t, res.err)
	require.Contains(t, res.out, "unknown")
	require.NotContains(t, res.out, "problem")
	require.Contains(t, res.out, "zones: Cloudflare did not answer (cloudflare api: HTTP 503: unavailable)")
	require.NotContains(t, res.out, "grant")
}
