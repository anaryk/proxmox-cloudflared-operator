package engine

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
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
	e.cycle()
	e.eng.recheck(t.Context())
	e.restart()
	api := verifying{API: e.cf, verified: make(chan struct{})}
	e.useAPI(testToken, api)
	timer := &fakeTimer{waits: make(chan time.Duration, 8), fire: make(chan time.Time)}
	e.eng.after = timer.after
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- e.eng.Run(ctx) }()

	select {
	case <-api.verified:
	case <-time.After(10 * time.Second):
		t.Fatal("the credential was not checked")
	}
	<-timer.waits
	cancel()

	require.NoError(t, <-done)
	require.Equal(t, 2, verifies(e.cf))
}

// startRun starts the loop of the daemon and returns the state of its first
// cycle, and stop, which ends the loop and waits for it.
func (e *env) startRun() (State, func()) {
	e.t.Helper()
	timer := &fakeTimer{waits: make(chan time.Duration, 8), fire: make(chan time.Time)}
	e.eng.after = timer.after
	ctx, cancel := context.WithCancel(e.t.Context())
	done := make(chan error)
	go func() { done <- e.eng.Run(ctx) }()
	<-timer.waits
	return e.eng.State(), func() {
		cancel()
		require.NoError(e.t, <-done)
	}
}

// checksBeforeFirstCycle returns what tells, once the first cycle started,
// how many token checks the fake had answered by then.
func (e *env) checksBeforeFirstCycle() func() int {
	n := -1
	e.inv.hook(func() {
		if n < 0 {
			n = verifies(e.cf)
		}
	})
	return func() int { return n }
}

// Reproduced on a node: the daemon started with a credential setup had
// checked and kept no report of, its first cycles tried the records of seven
// zones the token may not read, and remembered them as served once.
func TestAStartChecksACredentialNeverCheckedBeforeTheFirstCycle(t *testing.T) {
	e := sevenUnreadable(t)
	before := e.checksBeforeFirstCycle()

	st, stop := e.startRun()
	stop()

	require.Equal(t, 1, before(), "checked before the first cycle")
	require.Empty(t, st.Problems)
	for range 3 {
		e.clock.advance(20 * time.Second)
		require.Empty(t, e.cycle().Problems)
	}
	m, err := e.store.EngineMemory()
	require.NoError(t, err)
	require.Equal(t, []string{"example.com"}, zoneNames(m.EverServed))
	require.Equal(t, 1, verifies(e.cf))
}

// pco setup keeps the report of the check it made before it stored the token.
func TestAStartWithTheReportOfSetupChecksNothingFirst(t *testing.T) {
	e := sevenUnreadable(t)
	report := credentials.NewChecker(testInstall, e.clock.now, func() string { return "s1" }).Run(t.Context(), e.cf, true)
	require.True(t, report.Usable)
	require.NoError(t, e.store.SaveEngineMemory(store.EngineMemory{
		InstallID: testInstall,
		Reports:   []store.CheckedCredential{{CredentialID: testCred, Report: report}},
	}))
	n := verifies(e.cf)
	before := e.checksBeforeFirstCycle()

	st, stop := e.startRun()
	stop()

	require.Equal(t, n, before())
	require.Empty(t, st.Problems)
}

// stalling holds the first token check it is asked for until the time of the
// check is up, as a Cloudflare that does not answer does.
type stalling struct {
	cfapi.API
	stalled *atomic.Bool
}

func (s stalling) VerifyToken(ctx context.Context) (cfapi.TokenStatus, error) {
	if s.stalled.CompareAndSwap(false, true) {
		<-ctx.Done()
		return cfapi.TokenStatus{}, ctx.Err()
	}
	return s.API.VerifyToken(ctx)
}

func TestAFirstCheckThatDoesNotEndInTimeLeavesTheCycleAsBefore(t *testing.T) {
	e := sevenUnreadable(t)
	e.useAPI(testToken, stalling{API: e.cf, stalled: new(atomic.Bool)})
	e.eng.timeout = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		if d == firstCheckTimeout {
			d = time.Millisecond
		}
		return context.WithTimeout(ctx, d)
	}

	st, stop := e.startRun()
	require.True(t, hasProblem(st, "zone other2.org: listing the records"), "planned as before a check: %v", st.Problems)
	require.Eventually(t, func() bool {
		e.eng.repMu.Lock()
		defer e.eng.repMu.Unlock()
		_, ok := e.eng.reports[testCred]
		return ok
	}, 10*time.Second, 10*time.Millisecond, "checked beside the cycles at once")
	stop()

	e.clock.advance(20 * time.Second)
	require.Empty(t, e.cycle().Problems)
	m, err := e.store.EngineMemory()
	require.NoError(t, err)
	require.Equal(t, []string{"example.com"}, zoneNames(m.EverServed))
}

func TestAStartSaysItSetAsideTheMemoryOfAnotherInstall(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.store.SaveEngineMemory(store.EngineMemory{InstallID: "other1"}))

	st, stop := e.startRun()
	stop()

	require.True(t, hasProblem(st, "the engine memory on this node is of install other1, not abc123; it is set aside"), "%v", st.Problems)
}
