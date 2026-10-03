package credentials

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

func TestAFailureCloudflareDidNotAnswerIsMarked(t *testing.T) {
	ops := []struct {
		name  string
		op    string
		check func(*testing.T, Report) Check
	}{
		{"verify", "verify", func(t *testing.T, r Report) Check { return find(t, r, CapToken, scope{}) }},
		{"accounts", "accounts", func(t *testing.T, r Report) Check { return find(t, r, CapAccounts, scope{}) }},
		{"zones", "zones", func(t *testing.T, r Report) Check { return find(t, r, CapZones, scope{}) }},
		{"dns read", "dns.read", func(t *testing.T, r Report) Check { return find(t, r, CapDNSRead, exampleCom) }},
		{"dns write", "dns.write", func(t *testing.T, r Report) Check { return find(t, r, CapDNSWrite, exampleCom) }},
		{"tunnel read", "tunnel.read", func(t *testing.T, r Report) Check { return find(t, r, CapTunnelRead, acme) }},
		{"tunnel write", "tunnel.write", func(t *testing.T, r Report) Check { return find(t, r, CapTunnelWrite, acme) }},
	}
	errs := []error{
		&cfapi.Error{Status: 503, Message: "unavailable"},
		&cfapi.Error{Status: 500, Message: "internal error"},
		&cfapi.Error{Status: 429, Message: "rate limited", RetryAfter: 5 * time.Second},
		context.DeadlineExceeded,
	}
	for _, tc := range ops {
		for _, injected := range errs {
			t.Run(tc.name+"/"+injected.Error(), func(t *testing.T) {
				f := newFake()
				f.FailNext(tc.op, 1, injected)

				got := newChecker().Run(t.Context(), f, true)

				require.True(t, tc.check(t, got).Unanswered)
				require.False(t, got.Usable)
				require.True(t, got.Unanswered(), "%v", got.Checks)
			})
		}
	}
}

func TestARefusalIsNotMarkedUnanswered(t *testing.T) {
	for _, op := range []string{"accounts", "zones", "dns.read", "dns.write", "tunnel.read", "tunnel.write"} {
		t.Run(op, func(t *testing.T) {
			f := newFake()
			f.FailNext(op, 1, &cfapi.Error{Status: 403, Message: "denied"})

			got := newChecker().Run(t.Context(), f, true)

			require.NotEmpty(t, failures(got))
			for _, c := range failures(got) {
				require.False(t, c.Unanswered, "%v", c)
			}
			require.False(t, got.Unanswered())
		})
	}
}

func TestAnUnrefusedTokenWithoutAnAnswerIsMarked(t *testing.T) {
	f := newFake()
	f.FailNext("verify", 1, &cfapi.Error{Status: 429, Message: "rate limited"})

	got := newChecker().Run(t.Context(), f, false)

	require.Equal(t, []Check{{Capability: CapToken, Detail: (&cfapi.Error{Status: 429, Message: "rate limited"}).Error(), Unanswered: true}}, got.Checks)
}

func TestAnUnansweredProbeThatMayExistIsMarked(t *testing.T) {
	f := newFake()
	spy := &spyAPI{API: f, lostCreate: &cfapi.Error{Status: 503, Message: "unavailable"}}

	got := newChecker().Run(t.Context(), spy, true)

	require.True(t, find(t, got, CapDNSWrite, exampleCom).Unanswered)
	require.True(t, find(t, got, CapTunnelWrite, acme).Unanswered)
}

func TestAProbeLeftBehindByAnUnansweredDeleteIsMarked(t *testing.T) {
	f := newFake()
	spy := &spyAPI{API: f, deleteErr: &cfapi.Error{Status: 503, Message: "unavailable"}}

	got := newChecker().Run(t.Context(), spy, true)

	require.True(t, find(t, got, CapDNSWrite, exampleCom).Unanswered)
	require.Contains(t, find(t, got, CapDNSWrite, exampleCom).Detail, "probe record left behind")
}

func TestReportUnansweredNeedsEveryFailureUnanswered(t *testing.T) {
	answered := Check{Capability: CapDNSRead, Scope: "example.com", Detail: "grant Zone > DNS > Read on example.com"}
	silent := Check{Capability: CapZones, Detail: "cloudflare api: HTTP 503: unavailable", Unanswered: true}
	pass := Check{Capability: CapToken, OK: true}
	tests := []struct {
		name   string
		usable bool
		checks []Check
		want   bool
	}{
		{"one unanswered check", false, []Check{pass, silent}, true},
		{"an unanswered check and a refusal", false, []Check{pass, silent, answered}, false},
		{"a refusal", false, []Check{pass, answered}, false},
		{"nothing failed and nothing usable", false, []Check{pass}, false},
		{"nothing at all", false, nil, false},
		{"a usable report", true, []Check{pass}, false},
		{"a usable report with an unanswered note", true, []Check{pass, silent}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Report{Usable: tt.usable, Checks: tt.checks}.Unanswered())
		})
	}
}

func TestReasonSaysWhatToGrantOrThatCloudflareDidNotAnswer(t *testing.T) {
	tests := []struct {
		name  string
		check Check
		want  string
	}{
		{"a pass", Check{Capability: CapZones, OK: true, Detail: "ignored"}, ""},
		{"a refusal", Check{Capability: CapDNSRead, Detail: "grant Zone > DNS > Read on example.com"}, "grant Zone > DNS > Read on example.com"},
		{"no answer", Check{Capability: CapZones, Detail: "cloudflare api: HTTP 503: unavailable", Unanswered: true},
			"Cloudflare did not answer (cloudflare api: HTTP 503: unavailable)"},
		{"no answer and no detail", Check{Capability: CapZones, Unanswered: true}, "Cloudflare did not answer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.check.Reason())
		})
	}
}

func TestTheUnansweredMarkIsInTheJSON(t *testing.T) {
	raw, err := json.Marshal(Check{Capability: CapZones, Detail: "x", Unanswered: true})
	require.NoError(t, err)
	require.JSONEq(t, `{"capability":"zones","ok":false,"detail":"x","unanswered":true}`, string(raw))

	var back Check
	require.NoError(t, json.Unmarshal(raw, &back))
	require.True(t, back.Unanswered)
}

func TestACheckCutShortIsUnanswered(t *testing.T) {
	f := newFake()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got := newChecker().Run(ctx, f, false)

	require.False(t, got.Usable)
	for _, c := range failures(got) {
		require.True(t, c.Unanswered, "%v", c)
	}
}
