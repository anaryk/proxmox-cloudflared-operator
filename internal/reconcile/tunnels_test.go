package reconcile

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

const (
	testInstall = "abc"
	testTunnel  = "pco-abc"
)

var (
	t0 = time.Date(2026, time.March, 4, 5, 6, 7, 0, time.UTC)

	// ours is the writer every test runs as unless it says otherwise.
	ours = writerAt(5, "n5")

	app      = planner.IngressRule{Hostname: "app.example.com", Service: "http://10.0.0.5:8080"}
	web      = planner.IngressRule{Hostname: "web.example.com", Service: "https://10.0.0.6:443", OriginServerName: "web.example.com"}
	catchAll = planner.IngressRule{Service: "http_status:404"}
)

func writerAt(generation int, nonce string) planner.Writer {
	return planner.Writer{InstallID: testInstall, Generation: generation, Nonce: nonce}
}

func sentinelOf(w planner.Writer) planner.IngressRule {
	return planner.IngressRule{Hostname: planner.SentinelHostname(w), Service: "http_status:404"}
}

// rulesOf returns rules followed by the sentinel of w and the catch-all, as
// the planner ends the rules of every tunnel.
func rulesOf(w planner.Writer, rules ...planner.IngressRule) []planner.IngressRule {
	return append(slices.Clone(rules), sentinelOf(w), catchAll)
}

func planFor(account, credential string, rules ...planner.IngressRule) planner.TunnelPlan {
	return planner.TunnelPlan{AccountID: account, CredentialID: credential, Name: testTunnel, Rules: rulesOf(ours, rules...)}
}

func newFake(accounts ...string) *cffake.Fake {
	f := cffake.New()
	for _, a := range accounts {
		f.AddAccount(a, a)
	}
	return f
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// answer is one answer of the writer callback.
type answer struct {
	us, stored planner.Writer
	err        error
}

// scripted returns a writer callback that gives the answers in turn and the
// last one again once they run out, and the number of times it was called.
func scripted(answers ...answer) (func() (planner.Writer, planner.Writer, error), *int) {
	calls := 0
	return func() (planner.Writer, planner.Writer, error) {
		a := answers[min(calls, len(answers)-1)]
		calls++
		return a.us, a.stored, a.err
	}, &calls
}

// writerOf returns a writer callback that always answers us and stored.
func writerOf(us, stored planner.Writer) func() (planner.Writer, planner.Writer, error) {
	writer, _ := scripted(answer{us: us, stored: stored})
	return writer
}

func newReconciler(clients Clients, c *clock) *TunnelReconciler {
	return NewTunnelReconciler(clients, writerOf(ours, ours), c.now, zerolog.Nop())
}

func reconcilerWith(clients Clients, writer func() (planner.Writer, planner.Writer, error)) *TunnelReconciler {
	return NewTunnelReconciler(clients, writer, (&clock{t0}).now, zerolog.Nop())
}

// callsTo returns the calls of the fake that go to one of the methods.
func callsTo(f *cffake.Fake, methods ...string) []string {
	var out []string
	for _, call := range f.Calls() {
		if slices.Contains(methods, strings.Fields(call)[0]) {
			out = append(out, call)
		}
	}
	return out
}

// requireNoCallsFor fails when a call of the fake names the account.
func requireNoCallsFor(t *testing.T, f *cffake.Fake, account string) {
	t.Helper()
	for _, call := range f.Calls() {
		require.NotContains(t, strings.Fields(call), account, "no call may reach %s", account)
	}
}

// tunnelIn returns the one tunnel of an account.
func tunnelIn(t *testing.T, f *cffake.Fake, account string) cfapi.Tunnel {
	t.Helper()
	tunnels := f.TunnelsIn(account)
	require.Len(t, tunnels, 1, "tunnels in %s", account)
	return tunnels[0]
}

// configIn reads the configuration of the one tunnel of an account. The read
// is in the call log of the fake, so a test looks at the log first.
func configIn(t *testing.T, f *cffake.Fake, account string) cfapi.TunnelConfig {
	t.Helper()
	cfg, err := f.TunnelConfig(context.Background(), account, tunnelIn(t, f, account).ID)
	require.NoError(t, err)
	return cfg
}

// withoutDetail clears the details of actions, for tests that look at the
// rest.
func withoutDetail(actions []Action) []Action {
	out := slices.Clone(actions)
	for i := range out {
		out[i].Detail = ""
	}
	return out
}

func action(kind ActionKind, credential, held string) Action {
	return Action{Kind: kind, Credential: credential, Target: testTunnel, Applied: held == "", Held: held}
}

// unknownIn is the state of the tunnel of an account reached through cred1
// that the run knows nothing about.
func unknownIn(account string) TunnelState {
	return TunnelState{AccountID: account, CredentialID: "cred1", Name: testTunnel, Unknown: true}
}

func statusError(code int) error {
	return &cfapi.Error{Status: code, Message: http.StatusText(code)}
}

// spy wraps an API to change what the fake cannot: a lookup that misses a
// tunnel that is there, failing reads of the configuration, and something
// that happens right after a configuration write.
type spy struct {
	cfapi.API

	findMisses int     // the first lookups answer "not found"
	configErrs []error // what the next reads of the configuration answer, in turn; nil passes the read on
	afterPut   func()  // runs after each configuration write that succeeded
	puts       int     // configuration writes made through the spy
}

func (s *spy) FindTunnel(ctx context.Context, accountID, name string) (cfapi.Tunnel, bool, error) {
	if s.findMisses > 0 {
		s.findMisses--
		return cfapi.Tunnel{}, false, nil
	}
	return s.API.FindTunnel(ctx, accountID, name)
}

func (s *spy) TunnelConfig(ctx context.Context, accountID, tunnelID string) (cfapi.TunnelConfig, error) {
	if len(s.configErrs) > 0 {
		err := s.configErrs[0]
		s.configErrs = s.configErrs[1:]
		if err != nil {
			return cfapi.TunnelConfig{}, err
		}
	}
	return s.API.TunnelConfig(ctx, accountID, tunnelID)
}

func (s *spy) PutTunnelConfig(ctx context.Context, accountID, tunnelID string, rules []planner.IngressRule) (int, error) {
	s.puts++
	v, err := s.API.PutTunnelConfig(ctx, accountID, tunnelID, rules)
	if err == nil && s.afterPut != nil {
		s.afterPut()
	}
	return v, err
}

func TestJudgeWriter(t *testing.T) {
	other := func(generation int, nonce string) planner.Writer {
		return planner.Writer{InstallID: "xyz", Generation: generation, Nonce: nonce}
	}
	cases := []struct {
		name     string
		remote   planner.Writer
		remoteOK bool
		stored   planner.Writer
		want     WriterVerdict
	}{
		{"no remote sentinel", planner.Writer{}, false, ours, WriterProceed},
		{"remote generation lower", writerAt(4, "n4"), true, ours, WriterProceed},
		{"remote generation zero", writerAt(0, "n0"), true, ours, WriterProceed},
		{"equal generation, our nonce", writerAt(5, "n5"), true, ours, WriterProceed},
		{"equal generation, other nonce", writerAt(5, "zz"), true, ours, WriterForeign},
		{"remote higher, stored above it", writerAt(7, "n7"), true, writerAt(8, "n8"), WriterStale},
		{"remote higher, stored is its writer", writerAt(7, "n7"), true, writerAt(7, "n7"), WriterStale},
		{"remote higher, stored at its generation with another nonce", writerAt(7, "n7"), true, writerAt(7, "zz"), WriterForeign},
		{"remote higher, stored below it", writerAt(7, "n7"), true, writerAt(6, "n6"), WriterForeign},
		{"remote higher, stored is us", writerAt(7, "n7"), true, ours, WriterForeign},
		{"another install, higher generation", other(9, "n9"), true, writerAt(7, "n7"), WriterProceed},
		{"another install, equal generation and other nonce", other(5, "zz"), true, ours, WriterProceed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, JudgeWriter(ours, tc.remote, tc.remoteOK, tc.stored))
		})
	}
}

func TestJudgeConfig(t *testing.T) {
	newer, twin := writerAt(7, "n7"), writerAt(5, "zz")
	otherInstall := planner.IngressRule{Hostname: "g9.n9.pco-xyz.invalid", Service: "http_status:404"}
	cases := []struct {
		name   string
		rules  []planner.IngressRule
		stored planner.Writer
		want   WriterVerdict
		by     planner.Writer
	}{
		{"no sentinel", []planner.IngressRule{app, catchAll}, ours, WriterProceed, planner.Writer{}},
		{"our sentinel", rulesOf(ours, app), ours, WriterProceed, planner.Writer{}},
		{"sentinel of another install", []planner.IngressRule{app, otherInstall, catchAll}, ours, WriterProceed, planner.Writer{}},
		{"stale, then foreign", []planner.IngressRule{app, sentinelOf(newer), sentinelOf(twin), catchAll}, newer, WriterForeign, twin},
		{"foreign, then stale", []planner.IngressRule{app, sentinelOf(twin), sentinelOf(newer), catchAll}, newer, WriterForeign, twin},
		{"older, then stale", []planner.IngressRule{app, sentinelOf(writerAt(3, "n3")), sentinelOf(newer), catchAll}, newer, WriterStale, newer},
		{"two foreign: the first counts", []planner.IngressRule{sentinelOf(twin), sentinelOf(writerAt(5, "yy")), catchAll}, ours, WriterForeign, twin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, by := judgeConfig(ours, tc.stored, tc.rules)
			require.Equal(t, tc.want, v)
			require.Equal(t, tc.by, by)
		})
	}
}

func TestEqualIngress(t *testing.T) {
	insecure := app
	insecure.NoTLSVerify = true
	renamed := web
	renamed.OriginServerName = "origin.example.com"
	moved := app
	moved.Service = "http://10.0.0.7:8080"
	blocking := catchAll
	blocking.Service = "http_status:503"

	cases := []struct {
		name string
		a, b []planner.IngressRule
		want bool
	}{
		{"both nil", nil, nil, true},
		{"nil and empty", nil, []planner.IngressRule{}, true},
		{"same rules", rulesOf(ours, app, web), rulesOf(ours, app, web), true},
		{"order differs", rulesOf(ours, app, web), rulesOf(ours, web, app), false},
		{
			"empty options equal absent ones",
			[]planner.IngressRule{{Hostname: app.Hostname, Service: app.Service, OriginServerName: "", HTTPHostHeader: "", MatchSNIToHost: false, NoTLSVerify: false}, catchAll},
			[]planner.IngressRule{{Hostname: app.Hostname, Service: app.Service}, catchAll},
			true,
		},
		{"option set on one side", rulesOf(ours, insecure), rulesOf(ours, app), false},
		{"option value differs", rulesOf(ours, renamed), rulesOf(ours, web), false},
		{"service differs", rulesOf(ours, moved), rulesOf(ours, app), false},
		{"sentinel differs", rulesOf(writerAt(4, "n4"), app), rulesOf(ours, app), false},
		{"extra rule", rulesOf(ours, app, web), rulesOf(ours, app), false},
		{"catch-all missing", []planner.IngressRule{app, sentinelOf(ours)}, rulesOf(ours, app), false},
		{"catch-all answers otherwise", []planner.IngressRule{app, sentinelOf(ours), blocking}, rulesOf(ours, app), false},
		{"catch-all not last", []planner.IngressRule{catchAll, app}, []planner.IngressRule{app, catchAll}, false},
		{"empty and catch-all only", nil, []planner.IngressRule{catchAll}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, EqualIngress(tc.a, tc.b))
			require.Equal(t, tc.want, EqualIngress(tc.b, tc.a), "reversed")
		})
	}
}

func TestTunnelCreateAndPut(t *testing.T) {
	f := newFake("acct1")
	writer, asked := scripted(answer{us: ours, stored: ours})

	res := reconcilerWith(Clients{"cred1": f}, writer).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, WriterProceed, res.Verdict)
	id := tunnelIn(t, f, "acct1").ID
	require.Equal(t, []string{
		"FindTunnel acct1 pco-abc",
		"CreateTunnel acct1 pco-abc",
		"TunnelConfig acct1 " + id,
		"PutTunnelConfig acct1 " + id,
		"TunnelConfig acct1 " + id,
	}, f.Calls())
	require.Equal(t, []Action{action(CreateTunnel, "cred1", ""), action(PutConfig, "cred1", "")}, withoutDetail(res.Actions))
	require.Equal(t, []TunnelState{
		{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: id, Version: 1, Exists: true, Verified: true},
	}, res.Tunnels)
	require.Equal(t, 2, *asked, "once for the run and once before the configuration is read")
	require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct1").Ingress)
}

func TestTunnelNoOpWhenEqual(t *testing.T) {
	f := newFake("acct1")
	tun := f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Empty(t, res.Problems)
	require.Empty(t, res.Actions)
	require.Equal(t, []string{"FindTunnel acct1 pco-abc", "TunnelConfig acct1 " + tun.ID}, f.Calls())
	require.Equal(t, []TunnelState{
		{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: tun.ID, Version: 1, Exists: true, Verified: true},
	}, res.Tunnels)
}

func TestTunnelForeignConfigRewritten(t *testing.T) {
	f := newFake("acct1")
	tun := f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))
	f.SetForeign("acct1", tun.ID, true)

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, []Action{action(PutConfig, "cred1", "")}, withoutDetail(res.Actions))
	require.Contains(t, res.Actions[0].Detail, "settings pco does not manage")
	require.Len(t, callsTo(f, "PutTunnelConfig"), 1)
	require.Equal(t, 2, res.Tunnels[0].Version)
	require.True(t, res.Tunnels[0].Verified)
	cfg := configIn(t, f, "acct1")
	require.False(t, cfg.Foreign)
	require.Equal(t, rulesOf(ours, app), cfg.Ingress)
}

func TestTunnelExternalEditOverwritten(t *testing.T) {
	f := newFake("acct1")
	edited := planner.IngressRule{Hostname: "added.example.com", Service: "http://10.9.9.9:80"}
	f.SeedTunnel("acct1", testTunnel, rulesOf(writerAt(3, "n3"), app, edited))

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, WriterProceed, res.Verdict)
	require.Equal(t, []Action{action(PutConfig, "cred1", "")}, withoutDetail(res.Actions))
	require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct1").Ingress)
}

func TestTunnelOtherInstallSentinelIgnored(t *testing.T) {
	f := newFake("acct1")
	otherInstall := planner.IngressRule{Hostname: "g9.n9.pco-xyz.invalid", Service: "http_status:404"}
	f.SeedTunnel("acct1", testTunnel, []planner.IngressRule{app, otherInstall, catchAll})

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, WriterProceed, res.Verdict)
	require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct1").Ingress)
}

func TestTunnelStaleWriterStops(t *testing.T) {
	newer := writerAt(7, "n7")
	cases := []struct {
		name    string
		mode    Mode
		answers []answer
		calls   int  // calls that reach Cloudflare, all of them for acct1
		held    bool // whether the write of acct1 is reported as held
	}{
		{"newer writer stored from the start", Enforce, []answer{{us: ours, stored: newer}}, 0, false},
		{"takeover before the configuration read", Enforce, []answer{{us: ours, stored: ours}, {us: ours, stored: newer}}, 1, false},
		{"takeover seen when judging", Enforce, []answer{{us: ours, stored: ours}, {us: ours, stored: ours}, {us: ours, stored: newer}}, 2, true},
		{"takeover seen when judging in observe mode", Observe, []answer{{us: ours, stored: ours}, {us: ours, stored: newer}}, 2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake("acct1", "acct2")
			f.SeedTunnel("acct1", testTunnel, rulesOf(newer, app))
			writer, _ := scripted(tc.answers...)

			res := reconcilerWith(Clients{"cred1": f}, writer).Run(context.Background(), []planner.TunnelPlan{
				planFor("acct1", "cred1", app),
				planFor("acct2", "cred1", web),
			}, nil, tc.mode)

			require.Equal(t, WriterStale, res.Verdict)
			require.Empty(t, callsTo(f, "PutTunnelConfig", "CreateTunnel"))
			require.Len(t, f.Calls(), tc.calls, "calls: %v", f.Calls())
			requireNoCallsFor(t, f, "acct2")
			require.Len(t, res.Problems, 1)
			require.Contains(t, res.Problems[0], "leader.json names generation 7 nonce n7")
			require.Contains(t, res.Problems[0], "this writer is stale and stops")
			if tc.held {
				require.Contains(t, res.Problems[0], "pco-abc in account acct1")
				require.Equal(t, []Action{action(PutConfig, "cred1", "stale writer")}, withoutDetail(res.Actions))
			} else {
				require.Empty(t, res.Actions)
			}
			require.Len(t, res.Tunnels, 2)
			require.Equal(t, unknownIn("acct2"), res.Tunnels[1])
			require.Equal(t, rulesOf(newer, app), configIn(t, f, "acct1").Ingress)
		})
	}
}

func TestTunnelStoredWriterMovedOn(t *testing.T) {
	remotes := []struct {
		name  string
		rules []planner.IngressRule
	}{
		{"our sentinel", rulesOf(ours, web)},
		{"lower sentinel", rulesOf(writerAt(3, "n3"), web)},
		{"no sentinel", []planner.IngressRule{web, catchAll}},
		{"no configuration", nil},
	}
	stored := []struct {
		name   string
		writer planner.Writer
	}{
		{"takeover", writerAt(6, "n6")},
		{"duplicate install", writerAt(5, "zz")},
	}
	timings := []struct {
		name    string
		answers func(stored planner.Writer) []answer
		calls   int
	}{
		{"from the start", func(s planner.Writer) []answer {
			return []answer{{us: ours, stored: s}}
		}, 0},
		{"during the run", func(s planner.Writer) []answer {
			return []answer{{us: ours, stored: ours}, {us: ours, stored: s}}
		}, 1},
	}
	for _, remote := range remotes {
		for _, st := range stored {
			for _, timing := range timings {
				t.Run(remote.name+"/"+st.name+"/"+timing.name, func(t *testing.T) {
					f := newFake("acct1", "acct2")
					f.SeedTunnel("acct1", testTunnel, remote.rules)
					writer, _ := scripted(timing.answers(st.writer)...)

					res := reconcilerWith(Clients{"cred1": f}, writer).Run(context.Background(), []planner.TunnelPlan{
						planFor("acct1", "cred1", app),
						planFor("acct2", "cred1", app),
					}, nil, Enforce)

					require.Equal(t, WriterStale, res.Verdict)
					require.Empty(t, callsTo(f, "CreateTunnel", "PutTunnelConfig"))
					require.Len(t, f.Calls(), timing.calls, "calls: %v", f.Calls())
					requireNoCallsFor(t, f, "acct2")
					require.Len(t, res.Problems, 1)
					require.Contains(t, res.Problems[0], "this writer is stale and stops")
					require.Len(t, res.Tunnels, 2)
					require.Equal(t, unknownIn("acct2"), res.Tunnels[1])
				})
			}
		}
	}
}

func TestTunnelForeignWriterStops(t *testing.T) {
	f := newFake("acct1", "acct2")
	twin := writerAt(5, "zz")
	f.SeedTunnel("acct1", testTunnel, rulesOf(twin, app))
	writer, asked := scripted(answer{us: ours, stored: ours})

	res := reconcilerWith(Clients{"cred1": f}, writer).Run(context.Background(), []planner.TunnelPlan{
		planFor("acct1", "cred1", app),
		planFor("acct2", "cred1", web),
	}, nil, Enforce)

	require.Equal(t, WriterForeign, res.Verdict)
	require.Empty(t, callsTo(f, "PutTunnelConfig", "CreateTunnel"))
	requireNoCallsFor(t, f, "acct2")
	require.Equal(t, 3, *asked, "the verdict is checked against a fresh read")
	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "pco-abc in account acct1")
	require.Contains(t, res.Problems[0], "another installation")
	require.Equal(t, []Action{action(PutConfig, "cred1", "foreign writer")}, withoutDetail(res.Actions))
	require.Equal(t, unknownIn("acct2"), res.Tunnels[1])
	require.Equal(t, rulesOf(twin, app), configIn(t, f, "acct1").Ingress)
}

func TestTunnelObserveMode(t *testing.T) {
	f := newFake("acct1", "acct2")
	drifted := rulesOf(writerAt(3, "n3"), app)
	tun := f.SeedTunnel("acct2", testTunnel, drifted)
	writer, asked := scripted(answer{us: ours, stored: ours})

	res := reconcilerWith(Clients{"cred1": f}, writer).Run(context.Background(), []planner.TunnelPlan{
		planFor("acct1", "cred1", app),
		planFor("acct2", "cred1", app),
	}, nil, Observe)

	require.Empty(t, res.Problems)
	require.Equal(t, []string{
		"FindTunnel acct1 pco-abc",
		"FindTunnel acct2 pco-abc",
		"TunnelConfig acct2 " + tun.ID,
	}, f.Calls())
	require.Equal(t, []Action{
		action(CreateTunnel, "cred1", "observe mode"),
		action(PutConfig, "cred1", "observe mode"),
		action(PutConfig, "cred1", "observe mode"),
	}, withoutDetail(res.Actions))
	require.Contains(t, res.Actions[0].Detail, "acct1")
	require.Contains(t, res.Actions[2].Detail, "acct2")
	require.Equal(t, []TunnelState{
		{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel},
		{AccountID: "acct2", CredentialID: "cred1", Name: testTunnel, ID: tun.ID, Version: 1, Exists: true},
	}, res.Tunnels)
	require.Equal(t, 1, *asked, "the writer is read once, as nothing is written")
	require.Empty(t, f.TunnelsIn("acct1"))
	require.Equal(t, drifted, configIn(t, f, "acct2").Ingress)
}

func TestTunnelPutRateLimited(t *testing.T) {
	ctx := context.Background()
	f := newFake("acct1")
	c := &clock{t0}
	r := newReconciler(Clients{"cred1": f}, c)
	plans := []planner.TunnelPlan{planFor("acct1", "cred1", app)}

	res := r.Run(ctx, plans, nil, Enforce)
	require.Empty(t, res.Problems)
	id := tunnelIn(t, f, "acct1").ID
	_, err := f.PutTunnelConfig(ctx, "acct1", id, rulesOf(ours, web))
	require.NoError(t, err)

	c.t = t0.Add(10 * time.Second)
	res = r.Run(ctx, plans, nil, Enforce)
	require.Empty(t, res.Problems)
	require.Equal(t, []Action{action(PutConfig, "cred1", "rate limit: next write in 5s")}, withoutDetail(res.Actions))
	require.Len(t, callsTo(f, "PutTunnelConfig"), 2, "the first run's write and the edit")
	require.Equal(t, 2, res.Tunnels[0].Version)
	require.False(t, res.Tunnels[0].Verified)

	c.t = t0.Add(15 * time.Second)
	res = r.Run(ctx, plans, nil, Enforce)
	require.Empty(t, res.Problems)
	require.Equal(t, []Action{action(PutConfig, "cred1", "")}, withoutDetail(res.Actions))
	require.Equal(t, 3, res.Tunnels[0].Version)
	require.True(t, res.Tunnels[0].Verified)
	require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct1").Ingress)
}

func TestTunnelRateLimitClockStepsBack(t *testing.T) {
	ctx := context.Background()
	f := newFake("acct1")
	c := &clock{t0}
	r := newReconciler(Clients{"cred1": f}, c)
	plans := []planner.TunnelPlan{planFor("acct1", "cred1", app)}

	res := r.Run(ctx, plans, nil, Enforce)
	require.Empty(t, res.Problems)
	_, err := f.PutTunnelConfig(ctx, "acct1", tunnelIn(t, f, "acct1").ID, rulesOf(ours, web))
	require.NoError(t, err)

	c.t = t0.Add(-time.Hour)
	res = r.Run(ctx, plans, nil, Enforce)
	require.Equal(t, []Action{action(PutConfig, "cred1", "rate limit: next write in 15s")}, withoutDetail(res.Actions))

	c.t = t0.Add(-time.Hour + 15*time.Second)
	res = r.Run(ctx, plans, nil, Enforce)
	require.Empty(t, res.Problems)
	require.Equal(t, []Action{action(PutConfig, "cred1", "")}, withoutDetail(res.Actions))
	require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct1").Ingress)
}

func TestTunnelRateLimitIsPerTunnel(t *testing.T) {
	ctx := context.Background()
	f := newFake("acct1", "acct2")
	c := &clock{t0}
	r := newReconciler(Clients{"cred1": f}, c)

	res := r.Run(ctx, []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)
	require.Empty(t, res.Problems)

	c.t = t0.Add(time.Second)
	res = r.Run(ctx, []planner.TunnelPlan{planFor("acct1", "cred1", app), planFor("acct2", "cred1", app)}, nil, Enforce)
	require.Empty(t, res.Problems)
	require.Equal(t, []Action{action(CreateTunnel, "cred1", ""), action(PutConfig, "cred1", "")}, withoutDetail(res.Actions))
	require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct2").Ingress)
}

func TestTunnelUnplannedKnownEmptied(t *testing.T) {
	f := newFake("acct1", "acct2", "acct3")
	tun := f.SeedTunnel("acct2", testTunnel, rulesOf(ours, app, web))
	known := map[string]string{"acct3": "cred1", "acct2": "cred1", "acct1": "cred1"}

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct3", "cred1", web)}, known, Enforce)

	require.Empty(t, res.Problems)
	require.Empty(t, f.TunnelsIn("acct1"), "a known account without a tunnel gets none")
	created := tunnelIn(t, f, "acct3")
	require.Equal(t, []TunnelState{
		{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel},
		{AccountID: "acct2", CredentialID: "cred1", Name: testTunnel, ID: tun.ID, Version: 2, Exists: true, Verified: true},
		{AccountID: "acct3", CredentialID: "cred1", Name: testTunnel, ID: created.ID, Version: 1, Exists: true, Verified: true},
	}, res.Tunnels)
	require.Equal(t, []Action{
		action(PutConfig, "cred1", ""),
		action(CreateTunnel, "cred1", ""),
		action(PutConfig, "cred1", ""),
	}, withoutDetail(res.Actions))
	require.Equal(t, []planner.IngressRule{sentinelOf(ours), catchAll}, configIn(t, f, "acct2").Ingress)
	require.Equal(t, rulesOf(ours, web), configIn(t, f, "acct3").Ingress)
}

func TestTunnelFailureOnOneAccount(t *testing.T) {
	rateLimited := &cfapi.Error{Status: http.StatusTooManyRequests, Message: "Too many requests", RetryAfter: time.Minute}
	// drifted gives acct1 a tunnel that the run would write to.
	drifted := func(f *cffake.Fake) cfapi.Tunnel { return f.SeedTunnel("acct1", testTunnel, rulesOf(ours, web)) }
	cases := []struct {
		name string
		// setup returns the client of the failing account and the writes
		// it may see: at most the one that fails.
		setup func(f *cffake.Fake) (cfapi.API, []string)
	}{
		{"lookup denied", func(f *cffake.Fake) (cfapi.API, []string) {
			f.Deny("tunnel.read")
			return f, nil
		}},
		{"configuration read denied", func(f *cffake.Fake) (cfapi.API, []string) {
			drifted(f)
			return &spy{API: f, configErrs: []error{statusError(http.StatusForbidden)}}, nil
		}},
		{"configuration not found", func(f *cffake.Fake) (cfapi.API, []string) {
			drifted(f)
			return &spy{API: f, configErrs: []error{statusError(http.StatusNotFound)}}, nil
		}},
		{"lookup rate limited", func(f *cffake.Fake) (cfapi.API, []string) {
			f.FailNext("tunnel.read", 1, rateLimited)
			return f, nil
		}},
		{"configuration read rate limited", func(f *cffake.Fake) (cfapi.API, []string) {
			drifted(f)
			return &spy{API: f, configErrs: []error{rateLimited}}, nil
		}},
		{"unexpected answer", func(f *cffake.Fake) (cfapi.API, []string) {
			f.SeedTunnel("acct1", testTunnel, nil)
			f.SeedTunnel("acct1", testTunnel, nil)
			return f, nil
		}},
		{"lookup cancelled", func(f *cffake.Fake) (cfapi.API, []string) {
			f.FailNext("tunnel.read", 1, context.Canceled)
			return f, nil
		}},
		{"configuration read timed out", func(f *cffake.Fake) (cfapi.API, []string) {
			drifted(f)
			return &spy{API: f, configErrs: []error{context.DeadlineExceeded}}, nil
		}},
		{"transport failure", func(f *cffake.Fake) (cfapi.API, []string) {
			f.FailNext("tunnel.read", 1, errors.New("connection reset by peer"))
			return f, nil
		}},
		{"create denied", func(f *cffake.Fake) (cfapi.API, []string) {
			f.Deny("tunnel.write")
			return f, []string{"CreateTunnel acct1 pco-abc"}
		}},
		{"write denied", func(f *cffake.Fake) (cfapi.API, []string) {
			tun := drifted(f)
			f.Deny("tunnel.write")
			return f, []string{"PutTunnelConfig acct1 " + tun.ID}
		}},
		{"write rate limited", func(f *cffake.Fake) (cfapi.API, []string) {
			tun := drifted(f)
			f.FailNext("tunnel.write", 1, rateLimited)
			return f, []string{"PutTunnelConfig acct1 " + tun.ID}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failing := newFake("acct1")
			healthy := newFake("acct2")
			api, attempts := tc.setup(failing)

			res := newReconciler(Clients{"cred1": api, "cred2": healthy}, &clock{t0}).Run(context.Background(), []planner.TunnelPlan{
				planFor("acct1", "cred1", app),
				planFor("acct2", "cred2", app),
			}, nil, Enforce)

			require.Equal(t, attempts, callsTo(failing, "CreateTunnel", "PutTunnelConfig"), "nothing is written after the error")
			require.Len(t, res.Problems, 1, "problems: %v", res.Problems)
			require.Contains(t, res.Problems[0], "pco-abc in account acct1")
			require.Equal(t, WriterProceed, res.Verdict)
			require.Len(t, res.Tunnels, 2)
			require.False(t, res.Tunnels[0].Verified)
			require.Equal(t, TunnelState{
				AccountID: "acct2", CredentialID: "cred2", Name: testTunnel,
				ID: tunnelIn(t, healthy, "acct2").ID, Version: 1, Exists: true, Verified: true,
			}, res.Tunnels[1])
			require.Equal(t, rulesOf(ours, app), configIn(t, healthy, "acct2").Ingress)
		})
	}
}

func TestTunnelLookupFailureIsUnknown(t *testing.T) {
	f := newFake("acct1")
	f.Deny("tunnel.read")

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(context.Background(), nil, map[string]string{"acct1": "cred1"}, Enforce)

	require.Len(t, res.Problems, 1)
	require.Equal(t, []TunnelState{unknownIn("acct1")}, res.Tunnels)
	require.Empty(t, res.Actions)
}

func TestTunnelEveryAccountListed(t *testing.T) {
	f := newFake("acct1", "acct2")
	writer, _ := scripted(answer{err: errors.New("leader.json: no such file")})

	res := reconcilerWith(Clients{"cred1": f}, writer).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct2", "cred1", app)}, map[string]string{"acct1": "cred1"}, Enforce)

	require.Empty(t, f.Calls())
	require.Equal(t, []TunnelState{
		{AccountID: "acct1", CredentialID: "cred1", Unknown: true}, // without a writer identity the name is not known
		unknownIn("acct2"),
	}, res.Tunnels)
}

func TestTunnelVerifyAfterPutMismatch(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, f *cffake.Fake, id string)
	}{
		{"rules replaced", func(t *testing.T, f *cffake.Fake, id string) {
			_, err := f.PutTunnelConfig(context.Background(), "acct1", id, rulesOf(ours, web))
			require.NoError(t, err)
		}},
		{"unmanaged settings added", func(_ *testing.T, f *cffake.Fake, id string) {
			f.SetForeign("acct1", id, true)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake("acct1")
			tun := f.SeedTunnel("acct1", testTunnel, rulesOf(writerAt(3, "n3"), app))
			s := &spy{API: f}
			s.afterPut = func() { tc.change(t, f, tun.ID) }

			res := newReconciler(Clients{"cred1": s}, &clock{t0}).
				Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

			require.Equal(t, []string{"config changed under us on pco-abc in account acct1"}, res.Problems)
			require.Equal(t, 1, s.puts, "no second write in the same run")
			require.Equal(t, []Action{action(PutConfig, "cred1", "")}, withoutDetail(res.Actions))
			require.Equal(t, []TunnelState{
				{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: tun.ID, Version: 2, Exists: true},
			}, res.Tunnels, "the version of our write, not verified")
		})
	}
}

func TestTunnelVerifyReadFails(t *testing.T) {
	f := newFake("acct1")
	tun := f.SeedTunnel("acct1", testTunnel, rulesOf(writerAt(3, "n3"), app))
	s := &spy{API: f}
	s.afterPut = func() { s.configErrs = []error{errors.New("connection reset by peer")} }

	res := newReconciler(Clients{"cred1": s}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "pco-abc in account acct1: reading the configuration back")
	require.Equal(t, []Action{action(PutConfig, "cred1", "")}, withoutDetail(res.Actions))
	require.Equal(t, []TunnelState{
		{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: tun.ID, Version: 2, Exists: true},
	}, res.Tunnels, "the version of our write, not verified")
}

func TestTunnelConfigNotFoundAfterCreate(t *testing.T) {
	cases := []struct {
		name   string
		seeded bool // the tunnel is there, but the first lookup misses it
		writes bool
	}{
		{"created in this run", false, true},
		{"found after a conflicting create", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake("acct1")
			s := &spy{API: f, configErrs: []error{statusError(http.StatusNotFound)}}
			if tc.seeded {
				f.SeedTunnel("acct1", testTunnel, nil)
				s.findMisses = 1
			}

			res := newReconciler(Clients{"cred1": s}, &clock{t0}).
				Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

			if !tc.writes {
				require.Len(t, res.Problems, 1)
				require.Contains(t, res.Problems[0], "pco-abc in account acct1: reading the configuration")
				require.Empty(t, callsTo(f, "PutTunnelConfig"))
				return
			}
			require.Empty(t, res.Problems)
			require.Len(t, callsTo(f, "PutTunnelConfig"), 1)
			require.True(t, res.Tunnels[0].Verified)
			require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct1").Ingress)
		})
	}
}

func TestTunnelCreateConflictFindsTunnel(t *testing.T) {
	f := newFake("acct1")
	tun := f.SeedTunnel("acct1", testTunnel, nil)
	s := &spy{API: f, findMisses: 1}

	res := newReconciler(Clients{"cred1": s}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, []string{
		"CreateTunnel acct1 pco-abc",
		"FindTunnel acct1 pco-abc",
		"TunnelConfig acct1 " + tun.ID,
		"PutTunnelConfig acct1 " + tun.ID,
		"TunnelConfig acct1 " + tun.ID,
	}, f.Calls())
	require.Equal(t, []Action{
		action(CreateTunnel, "cred1", "tunnel already exists"),
		action(PutConfig, "cred1", ""),
	}, withoutDetail(res.Actions))
	require.Equal(t, tun.ID, res.Tunnels[0].ID)
	require.True(t, res.Tunnels[0].Verified)
	require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct1").Ingress)
}

func TestTunnelCreateConflictUnresolved(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *cffake.Fake) *spy
		want  string
	}{
		{"lookup fails", func(f *cffake.Fake) *spy {
			f.Deny("tunnel.read")
			return &spy{API: f, findMisses: 1}
		}, "finding the tunnel after its name was taken"},
		{"still not found", func(f *cffake.Fake) *spy {
			return &spy{API: f, findMisses: 2}
		}, "no tunnel of that name is found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake("acct1")
			f.SeedTunnel("acct1", testTunnel, nil)
			healthy := newFake("acct2")

			res := newReconciler(Clients{"cred1": tc.setup(f), "cred2": healthy}, &clock{t0}).Run(context.Background(), []planner.TunnelPlan{
				planFor("acct1", "cred1", app),
				planFor("acct2", "cred2", app),
			}, nil, Enforce)

			require.Len(t, res.Problems, 1)
			require.Contains(t, res.Problems[0], "pco-abc in account acct1")
			require.Contains(t, res.Problems[0], tc.want)
			require.Len(t, f.TunnelsIn("acct1"), 1)
			require.Empty(t, callsTo(f, "PutTunnelConfig"))
			require.Equal(t, []Action{
				action(CreateTunnel, "cred1", "tunnel already exists"),
				action(CreateTunnel, "cred2", ""),
				action(PutConfig, "cred2", ""),
			}, withoutDetail(res.Actions))
			require.Equal(t, []TunnelState{
				unknownIn("acct1"),
				{AccountID: "acct2", CredentialID: "cred2", Name: testTunnel, ID: tunnelIn(t, healthy, "acct2").ID, Version: 1, Exists: true, Verified: true},
			}, res.Tunnels)
		})
	}
}

func TestTunnelWriterGuards(t *testing.T) {
	older := writerAt(3, "n3")
	cases := []struct {
		name     string
		answers  []answer
		remote   planner.Writer // sentinel of the configuration of acct1
		calls    bool           // whether any call reaches Cloudflare
		problems int
		verdict  WriterVerdict
	}{
		{
			name:    "identity unreadable",
			answers: []answer{{err: errors.New("leader.json: no such file")}},
			remote:  older, problems: 1,
		},
		{
			name:    "identity invalid",
			answers: []answer{{us: planner.Writer{InstallID: "ABC", Generation: 5, Nonce: "n5"}, stored: ours}},
			remote:  older, problems: 1,
		},
		{
			name:    "identity unreadable before the configuration read",
			answers: []answer{{us: ours, stored: ours}, {err: errors.New("lease lost")}},
			remote:  older, calls: true, problems: 1,
		},
		{
			name:    "identity changed during the run",
			answers: []answer{{us: ours, stored: ours}, {us: writerAt(6, "n6"), stored: writerAt(6, "n6")}},
			remote:  older, calls: true, problems: 1, verdict: WriterStale,
		},
		{
			name:    "identity unreadable when judging",
			answers: []answer{{us: ours, stored: ours}, {us: ours, stored: ours}, {err: errors.New("lease lost")}},
			remote:  writerAt(5, "zz"), calls: true, problems: 2, verdict: WriterForeign,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake("acct1", "acct2")
			f.SeedTunnel("acct1", testTunnel, rulesOf(tc.remote, app))
			writer, _ := scripted(tc.answers...)

			res := reconcilerWith(Clients{"cred1": f}, writer).Run(context.Background(), []planner.TunnelPlan{
				planFor("acct1", "cred1", app),
				planFor("acct2", "cred1", app),
			}, nil, Enforce)

			require.Len(t, res.Problems, tc.problems, "problems: %v", res.Problems)
			require.Equal(t, tc.verdict, res.Verdict)
			require.Empty(t, callsTo(f, "PutTunnelConfig", "CreateTunnel"))
			require.Equal(t, tc.calls, len(f.Calls()) > 0, "calls: %v", f.Calls())
			requireNoCallsFor(t, f, "acct2")
			require.Len(t, res.Tunnels, 2)
			require.Equal(t, unknownIn("acct2"), res.Tunnels[1])
		})
	}
}

func TestTunnelPlanShape(t *testing.T) {
	withRules := func(rules ...planner.IngressRule) func(*planner.TunnelPlan) {
		return func(p *planner.TunnelPlan) { p.Rules = rules }
	}
	optioned := catchAll
	optioned.NoTLSVerify = true
	cases := []struct {
		name   string
		change func(*planner.TunnelPlan)
	}{
		{"tunnel of another install", func(p *planner.TunnelPlan) { p.Name = "pco-xyz" }},
		{"no rules", withRules()},
		{"no catch-all", withRules(app, sentinelOf(ours))},
		{"catch-all answers otherwise", withRules(app, sentinelOf(ours), planner.IngressRule{Service: "http_status:503"})},
		{"catch-all with origin options", withRules(app, sentinelOf(ours), optioned)},
		{"no sentinel", withRules(app, catchAll)},
		{"sentinel of another writer", withRules(rulesOf(writerAt(4, "n4"), app)...)},
		{"sentinel not before the catch-all", withRules(sentinelOf(ours), app, catchAll)},
		{"sentinel answers otherwise", withRules(app, planner.IngressRule{Hostname: planner.SentinelHostname(ours), Service: "http_status:503"}, catchAll)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake("acct1", "acct2")
			p := planFor("acct1", "cred1", app)
			tc.change(&p)

			res := newReconciler(Clients{"cred1": f}, &clock{t0}).
				Run(context.Background(), []planner.TunnelPlan{p, planFor("acct2", "cred1", app)}, nil, Enforce)

			requireNoCallsFor(t, f, "acct1")
			require.Len(t, res.Problems, 1)
			require.Contains(t, res.Problems[0], "in account acct1")
			require.True(t, res.Tunnels[0].Unknown)
			require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct2").Ingress)
		})
	}
}

func TestTunnelMissingClient(t *testing.T) {
	f := newFake("acct2")

	res := newReconciler(Clients{"cred2": f}, &clock{t0}).Run(context.Background(), []planner.TunnelPlan{
		planFor("acct1", "cred1", app),
		planFor("acct2", "cred2", app),
	}, nil, Enforce)

	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "cred1")
	require.Equal(t, unknownIn("acct1"), res.Tunnels[0])
	require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct2").Ingress)
}

func TestTunnelRunCancelled(t *testing.T) {
	f := newFake("acct1", "acct2")
	tun := f.SeedTunnel("acct1", testTunnel, rulesOf(writerAt(3, "n3"), app))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &spy{API: f, afterPut: cancel}

	res := newReconciler(Clients{"cred1": s}, &clock{t0}).Run(ctx, []planner.TunnelPlan{
		planFor("acct1", "cred1", app),
		planFor("acct2", "cred1", app),
	}, nil, Enforce)

	requireNoCallsFor(t, f, "acct2")
	require.Len(t, res.Problems, 2, "problems: %v", res.Problems)
	require.Contains(t, res.Problems[0], "reading the configuration back")
	require.Equal(t, "run stopped: context canceled", res.Problems[1])
	require.Equal(t, []TunnelState{
		{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: tun.ID, Version: 2, Exists: true},
		unknownIn("acct2"),
	}, res.Tunnels)
}
