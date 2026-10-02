package engine

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// verifies counts the token checks a fake has answered.
func verifies(f *cffake.Fake) int {
	n := 0
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, "VerifyToken") {
			n++
		}
	}
	return n
}

func credentialView(st State, id string) CredentialView {
	i := slices.IndexFunc(st.Credentials, func(c CredentialView) bool { return c.ID == id })
	if i < 0 {
		return CredentialView{}
	}
	return st.Credentials[i]
}

func TestAStartChecksEveryCredentialOnce(t *testing.T) {
	e := newEnv(t)
	second := cffake.New()
	second.AddAccount("acc2", "Other")
	second.AddZone("zone2", "example.org", "acc2")
	e.addSecondCredential("second-token", second)
	e.cycle()

	e.eng.recheck(t.Context())

	require.Equal(t, 1, verifies(e.cf))
	require.Equal(t, 1, verifies(second))
	for _, c := range e.cf.Calls() {
		require.NotRegexp(t, `^(Create|Update|Delete|Put)`, c, "a recheck is a shallow check")
	}
	st := e.cycle()
	require.True(t, credentialView(st, testCred).Checked)
	require.True(t, credentialView(st, testCred).Report.Usable)
	require.True(t, credentialView(st, "cred2").Checked)

	e.eng.recheck(t.Context())
	require.Equal(t, 1, verifies(e.cf), "checked once")
	require.Equal(t, 1, verifies(second))
}

func TestACredentialIsCheckedAgainAfterADay(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	e.eng.recheck(t.Context())
	require.Equal(t, 1, verifies(e.cf))

	e.clock.advance(recheckEvery - time.Minute)
	e.cycle()
	e.eng.recheck(t.Context())
	require.Equal(t, 1, verifies(e.cf), "not before a day")

	e.clock.advance(time.Minute)
	e.eng.recheck(t.Context())
	require.Equal(t, 2, verifies(e.cf))
}

func TestACredentialAddedOrCheckedByTheAdminIsNotCheckedAgainAtOnce(t *testing.T) {
	e := newEnv(t)
	_, err := e.eng.CheckCredential(t.Context(), testCred, false)
	require.NoError(t, err)
	e.cycle()

	e.eng.recheck(t.Context())

	require.Equal(t, 1, verifies(e.cf))
}

func TestTheLastReportOfACredentialSurvivesARestart(t *testing.T) {
	e := newEnv(t)
	e.cf.SetTokenStatus("active", new(t0.Add(20*24*time.Hour)))
	e.cycle()
	e.eng.recheck(t.Context())
	memory, err := os.ReadFile(filepath.Join(e.paths.Local, "meta", "engine-memory.json"))
	require.NoError(t, err)
	require.Contains(t, string(memory), `"credentialId": "cred1"`)
	require.NotContains(t, string(memory), testToken)

	e.eng = e.newEngine()
	st := e.cycle()

	v := credentialView(st, testCred)
	require.True(t, v.Checked, "the report of the earlier process is shown")
	require.True(t, v.Report.Usable)
	require.Equal(t, t0.Add(20*24*time.Hour), *v.Report.Token.ExpiresOn)
	require.Equal(t, 1, verifies(e.cf))

	e.eng.recheck(t.Context())
	require.Equal(t, 2, verifies(e.cf), "a start checks it all the same")
}

func TestARefusedTokenOnARecheckIsAProblemNamingTheCredential(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	e.cf.FailNext("verify", 1, &cfapi.Error{Status: http.StatusUnauthorized, Codes: []int{1000}, Message: "Invalid API Token"})

	e.eng.recheck(t.Context())
	st := e.cycle()

	require.True(t, hasProblem(st, "credential cred1 (main): its last check found that the token cannot be used: token: "), "%v", st.Problems)
	require.False(t, credentialView(st, testCred).Report.Usable)

	// A failure may have been Cloudflare's: it is checked again soon.
	e.clock.advance(recheckFailedEvery)
	e.eng.recheck(t.Context())
	st = e.cycle()
	require.False(t, hasProblem(st, "credential cred1"), "%v", st.Problems)
}

func TestNoRecheckWhileACycleHoldsForTheStore(t *testing.T) {
	e := newEnv(t)
	e.settings(func(*store.Settings) {})
	e.cycle()
	settings := filepath.Join(e.paths.Cluster, "meta", "settings.json")
	good, err := os.ReadFile(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(settings, []byte("{"), 0o600))
	st := e.cycle()
	require.True(t, hasProblem(st, "reading the settings"))

	e.eng.recheck(t.Context())
	require.Zero(t, verifies(e.cf))

	require.NoError(t, os.WriteFile(settings, good, 0o600))
	e.cycle()
	e.eng.recheck(t.Context())
	require.Equal(t, 1, verifies(e.cf))
}

func TestATokenAboutToExpireIsAWarningEvent(t *testing.T) {
	e := newEnv(t)
	expires := t0.Add(ExpiryWarning - 24*time.Hour)
	e.cf.SetTokenStatus("active", &expires)
	e.cycle()

	e.eng.recheck(t.Context())

	var found []Event
	for _, ev := range e.eng.Events(time.Time{}) {
		if ev.Kind == kindCredential {
			found = append(found, ev)
		}
	}
	require.Len(t, found, 1)
	require.Equal(t, levelWarn, found[0].Level)
	require.Equal(t, testCred, found[0].Subject)
	require.Contains(t, found[0].Message, "expires at 2026-10-30T12:00:00Z")
}

// verifying tells a test about every token check it answers.
type verifying struct {
	cfapi.API
	verified chan struct{}
}

func (v verifying) VerifyToken(ctx context.Context) (cfapi.TokenStatus, error) {
	st, err := v.API.VerifyToken(ctx)
	v.verified <- struct{}{}
	return st, err
}

func TestRunChecksTheCredentialsBetweenCycles(t *testing.T) {
	e := newEnv(t)
	api := verifying{API: e.cf, verified: make(chan struct{})}
	e.useAPI(testToken, api)
	timer := &fakeTimer{waits: make(chan time.Duration, 8), fire: make(chan time.Time)}
	e.eng.after = timer.after
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- e.eng.Run(ctx) }()

	<-api.verified
	<-timer.waits
	cancel()

	require.NoError(t, <-done)
	require.Equal(t, 1, verifies(e.cf))
}
