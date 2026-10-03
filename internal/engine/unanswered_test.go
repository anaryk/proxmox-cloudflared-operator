package engine

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
)

// credentialEvents are the events about credentials of the engine, oldest first.
func credentialEvents(e *env) []Event {
	var out []Event
	for _, ev := range e.eng.Events(time.Time{}) {
		if ev.Kind == kindCredential {
			out = append(out, ev)
		}
	}
	return out
}

func TestACloudflareFailureOnARecheckKeepsTheLastReport(t *testing.T) {
	tests := []struct {
		name string
		op   string
		err  error
	}{
		{"zones unavailable", "zones", &cfapi.Error{Status: http.StatusServiceUnavailable, Message: "unavailable"}},
		{"verification rate limited", "verify", &cfapi.Error{Status: http.StatusTooManyRequests, Message: "slow down", RetryAfter: time.Minute}},
		{"records failing", "dns.read", &cfapi.Error{Status: http.StatusInternalServerError, Message: "internal error"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.cycle()
			e.eng.recheck(t.Context())
			good := credentialView(e.cycle(), testCred)
			require.True(t, good.Report.Usable)
			e.clock.advance(recheckEvery)
			e.cf.FailNext(tt.op, 1, tt.err)
			checked := verifies(e.cf)

			e.eng.recheck(t.Context())
			st := e.cycle()

			require.Equal(t, checked+1, verifies(e.cf))
			v := credentialView(st, testCred)
			require.True(t, v.Checked)
			require.Equal(t, good.Report, v.Report, "the report of the check before stays")
			require.False(t, hasProblem(st, "credential cred1"), "%v", st.Problems)
			events := credentialEvents(e)
			require.Len(t, events, 1)
			require.Equal(t, levelWarn, events[0].Level)
			require.Equal(t, testCred, events[0].Subject)
			require.Contains(t, events[0].Message, `"main"`)
			require.Contains(t, events[0].Message, "Cloudflare did not answer")
			require.NotContains(t, events[0].Message, "grant")

			e.clock.advance(recheckFailedEvery - time.Minute)
			e.eng.recheck(t.Context())
			require.Equal(t, checked+1, verifies(e.cf), "not before the short wait")
			e.clock.advance(time.Minute)
			e.eng.recheck(t.Context())
			require.Equal(t, checked+2, verifies(e.cf), "checked again after the short wait, not after a day")
			require.Equal(t, e.clock.now(), credentialView(e.cycle(), testCred).Report.CheckedAt)
			require.Len(t, credentialEvents(e), 1, "an answer ends the warnings")
		})
	}
}

func TestACredentialNeverCheckedStaysUncheckedWhenCloudflareDoesNotAnswer(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	e.cf.FailNext("zones", 1, &cfapi.Error{Status: http.StatusServiceUnavailable})

	e.eng.recheck(t.Context())
	st := e.cycle()

	require.False(t, credentialView(st, testCred).Checked)
	require.False(t, hasProblem(st, "credential cred1"), "%v", st.Problems)
	e.clock.advance(recheckFailedEvery)
	e.eng.recheck(t.Context())
	require.True(t, credentialView(e.cycle(), testCred).Report.Usable)
}

func TestARefusalOnARecheckIsAProblemThatNamesWhatToGrant(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	e.cf.FailNext("dns.read", 1, &cfapi.Error{Status: http.StatusForbidden, Message: "denied"})

	e.eng.recheck(t.Context())
	st := e.cycle()

	require.True(t, hasProblem(st, "credential cred1 (main): its last check found that the token cannot be used: "+
		"dns.read: token can read the DNS of no zone it lists; grant Zone > DNS > Edit on the zones to manage"), "%v", st.Problems)
	require.False(t, credentialView(st, testCred).Report.Usable)
	require.Empty(t, credentialEvents(e))
}

func TestARefusalBesideAMissingAnswerIsStillAProblem(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	e.cf.FailNext("dns.read", 1, &cfapi.Error{Status: http.StatusServiceUnavailable, Message: "unavailable"})
	e.cf.FailNext("tunnel.read", 1, &cfapi.Error{Status: http.StatusForbidden, Message: "denied"})

	e.eng.recheck(t.Context())
	st := e.cycle()

	require.True(t, hasProblem(st, "tunnel.read on Main: grant Account > Cloudflare Tunnel > Read on Main"), "%v", st.Problems)
	require.True(t, hasProblem(st, "dns.read on example.com: Cloudflare did not answer"), "%v", st.Problems)
}

func TestACheckByTheAdminThatCloudflareDoesNotAnswerChangesNoReport(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	_, err := e.eng.CheckCredential(t.Context(), testCred, false)
	require.NoError(t, err)
	good := credentialView(e.cycle(), testCred)
	require.True(t, good.Report.Usable)
	e.clock.advance(time.Hour)
	e.cf.FailNext("zones", 1, &cfapi.Error{Status: http.StatusServiceUnavailable, Message: "unavailable"})

	view, err := e.eng.CheckCredential(t.Context(), testCred, false)

	require.NoError(t, err)
	require.True(t, view.Checked)
	require.True(t, view.Report.Unanswered(), "the admin sees what Cloudflare answered: %v", view.Report.Checks)
	st := e.cycle()
	require.Equal(t, good.Report, credentialView(st, testCred).Report)
	require.False(t, hasProblem(st, "credential cred1"), "%v", st.Problems)

	checked := verifies(e.cf)
	e.clock.advance(recheckFailedEvery)
	e.eng.recheck(t.Context())
	require.Equal(t, checked+1, verifies(e.cf), "checked again soon, not in a day")
}

func TestAddingATokenCloudflareCannotCheckSaysSoAndStoresNothing(t *testing.T) {
	e := newEnv(t)
	other := cffake.New()
	other.FailNext("verify", 1, &cfapi.Error{Status: http.StatusTooManyRequests, Message: "slow down"})
	e.useAPI("other-token-0123456789", other)

	view, err := e.eng.AddCredential(t.Context(), "second", "other-token-0123456789")

	require.ErrorIs(t, err, ErrInvalid)
	require.Contains(t, err.Error(), "could not be checked")
	require.Contains(t, err.Error(), "Cloudflare did not answer")
	require.NotContains(t, err.Error(), "cannot be used")
	require.True(t, view.Report.Unanswered())
	creds, err := e.store.Credentials()
	require.NoError(t, err)
	require.Len(t, creds, 1)
}
